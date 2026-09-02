package session

import (
	"encoding/json"
	"fmt"
	"math"
)

// validateCommandLifecycle mirrors the command registry contract. Command
// lifecycle rows are standalone audit records: a run reserves a unique ID,
// and exactly one done row may settle it. A successful done may point at an
// earlier domain event for adapter rendering, but never at another command
// lifecycle row. Open runs remain valid because a crash can leave the handler
// between run and done; recovery decides how to surface that pending command.
func validateCommandLifecycle(events []Event) error {
	runs := make(map[string]Event)
	dones := make(map[string]Event)
	bySeq := make(map[uint64]Event, len(events))
	for _, event := range events {
		bySeq[event.Seq] = event
		switch event.Type {
		case EventCommandRun:
			var data commandRunData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return fmt.Errorf("session: command/run at seq %d is malformed: %w", event.Seq, err)
			}
			if data.CommandID == "" || data.Name == "" {
				return fmt.Errorf("session: command/run at seq %d requires commandId and name", event.Seq)
			}
			if _, exists := runs[data.CommandID]; exists {
				return fmt.Errorf("session: command/run repeats command id %q", data.CommandID)
			}
			runs[data.CommandID] = event
		case EventCommandDone:
			var data commandDoneData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return fmt.Errorf("session: command/done at seq %d is malformed: %w", event.Seq, err)
			}
			if data.CommandID == "" {
				return fmt.Errorf("session: command/done at seq %d requires commandId", event.Seq)
			}
			if data.Kind != "success" && data.Kind != "error" {
				return fmt.Errorf("session: command/done at seq %d has invalid kind %q", event.Seq, data.Kind)
			}
			if _, exists := runs[data.CommandID]; !exists {
				return fmt.Errorf("session: command/done at seq %d has no matching command/run for %q", event.Seq, data.CommandID)
			}
			if _, exists := dones[data.CommandID]; exists {
				return fmt.Errorf("session: command/done repeats command id %q", data.CommandID)
			}
			if data.SourceEventSeq != nil {
				if data.Kind != "success" {
					return fmt.Errorf("session: command/done at seq %d may cite a source only on success", event.Seq)
				}
				sourceSeq := *data.SourceEventSeq
				if sourceSeq >= event.Seq {
					return fmt.Errorf("session: command/done at seq %d source seq %d is not earlier", event.Seq, sourceSeq)
				}
				source, exists := bySeq[sourceSeq]
				if !exists {
					return fmt.Errorf("session: command/done at seq %d source seq %d is missing", event.Seq, sourceSeq)
				}
				if source.Type == EventCommandRun || source.Type == EventCommandDone {
					return fmt.Errorf("session: command/done at seq %d cannot cite %s at seq %d", event.Seq, source.Type, sourceSeq)
				}
			}
			dones[data.CommandID] = event
		}
	}
	return nil
}

// validateCanonicalLifecycle adds the relation checks used by the DSH
// session invariant when a log uses explicit turn/step coordinates. Legacy
// Shutu rows intentionally remain accepted by ValidateLifecycle; canonical
// rows, however, must not merely have balanced brackets — their coordinates,
// step numbering and tool-result pairing must also agree.
func validateCanonicalLifecycle(events []Event) error {
	canonical := false
	for _, event := range events {
		if event.Type != EventTurnStart && event.Type != EventStepStart {
			continue
		}
		var data map[string]any
		if json.Unmarshal(event.Data, &data) == nil {
			if _, ok := data["turn"]; ok {
				canonical = true
				break
			}
		}
	}
	if !canonical {
		return nil
	}

	openTurn, openStep := 0, 0
	nextTurn, nextStep := 1, 1
	pending := make(map[string]struct{})
	for _, event := range events {
		data, err := lifecycleObject(event.Data)
		if err != nil {
			return fmt.Errorf("session: %s data is not an object: %w", event.Type, err)
		}
		coord := func(key string) (int, error) {
			value, ok := data[key].(float64)
			if !ok || value != float64(int(value)) || int(value) <= 0 {
				return 0, fmt.Errorf("session: %s requires positive %s", event.Type, key)
			}
			return int(value), nil
		}
		requireOpen := func() error {
			turn, err := coord("turn")
			if err != nil {
				return err
			}
			step, err := coord("step")
			if err != nil {
				return err
			}
			if openTurn != turn || openStep != step {
				return fmt.Errorf("session: %s names turn %d/step %d but open is %d/%d", event.Type, turn, step, openTurn, openStep)
			}
			return nil
		}

		switch event.Type {
		case EventTurnStart:
			turn, err := coord("turn")
			if err != nil {
				return err
			}
			if openTurn != 0 || turn != nextTurn {
				return fmt.Errorf("session: turn/start expected turn %d, got %d", nextTurn, turn)
			}
			openTurn, openStep, nextStep = turn, 0, 1
			pending = make(map[string]struct{})
		case EventTurnEnd:
			turn, err := coord("turn")
			if err != nil {
				return err
			}
			if openTurn != turn || openStep != 0 {
				return fmt.Errorf("session: turn/end does not close turn %d/step %d", turn, openStep)
			}
			openTurn, nextTurn, pending = 0, nextTurn+1, make(map[string]struct{})
		case EventStepStart:
			turn, err := coord("turn")
			if err != nil {
				return err
			}
			step, err := coord("step")
			if err != nil {
				return err
			}
			if openTurn != turn || openStep != 0 || step != nextStep {
				return fmt.Errorf("session: step/start expected turn %d step %d, got turn %d step %d", openTurn, nextStep, turn, step)
			}
			openStep = step
		case EventStepEnd:
			if err := requireOpen(); err != nil {
				return err
			}
			openStep = 0
			nextStep++
			pending = make(map[string]struct{})
		case EventAssistantChunk, EventAssistantReasoning, EventAssistantMessage, EventToolCall:
			if err := requireOpen(); err != nil {
				return err
			}
			if event.Type == EventToolCall {
				callID, ok := data["callId"].(string)
				if !ok || callID == "" {
					return fmt.Errorf("session: tool/call requires callId")
				}
				pending[callID] = struct{}{}
			}
		case EventToolResult:
			if err := requireOpen(); err != nil {
				return err
			}
			callID := toolResultCallID(data)
			if callID == "" {
				return fmt.Errorf("session: tool/result requires message.source.callId")
			}
			if _, exists := pending[callID]; !exists && toolResultErrorCode(data) != "TOOL_NOT_STARTED" {
				return fmt.Errorf("session: tool/result for %s has no prior tool/call", callID)
			}
			delete(pending, callID)
		case EventRequestHeader, EventRequestContext, EventTodoWrite, EventLLMRetry, EventLLMRetryStarted:
			// These are non-surface lifecycle records. Canonical request/retry
			// rows are still turn-scoped when their coordinates are present.
			if _, hasTurn := data["turn"]; hasTurn {
				turn, err := coord("turn")
				if err != nil {
					return err
				}
				if openTurn != turn {
					return fmt.Errorf("session: %s names closed turn %d (open %d)", event.Type, turn, openTurn)
				}
			}
		}
	}
	return nil
}

func lifecycleObject(raw []byte) (map[string]any, error) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		if err == nil {
			err = fmt.Errorf("null payload")
		}
		return nil, err
	}
	return data, nil
}

func toolResultCallID(data map[string]any) string {
	if callID, _ := data["callId"].(string); callID != "" {
		return callID
	}
	message, _ := data["message"].(map[string]any)
	source, _ := message["source"].(map[string]any)
	callID, _ := source["callId"].(string)
	return callID
}

func toolResultErrorCode(data map[string]any) string {
	errorData, _ := data["error"].(map[string]any)
	code, _ := errorData["code"].(string)
	return code
}

// validateCanonicalRetryLifecycle mirrors the retry package's durable
// invariant at the session boundary. It activates only for canonical rows
// carrying coordinates; old Shutu retry rows remain readable.
func validateCanonicalRetryLifecycle(events []Event) error {
	canonical := false
	for _, event := range events {
		if event.Type != EventLLMRetry && event.Type != EventLLMRetryStarted {
			continue
		}
		data, err := lifecycleObject(event.Data)
		if err != nil {
			return fmt.Errorf("session: %s data is not an object: %w", event.Type, err)
		}
		if _, ok := data["turn"]; ok {
			canonical = true
			break
		}
	}
	if !canonical {
		return nil
	}

	type retryRecord struct {
		id, provider, policy string
		turn, step, retry    int
	}
	openTurn, openStep := 0, 0
	scheduled := make([]retryRecord, 0)
	started := make(map[string]struct{})
	lastByChain := make(map[string]retryRecord)
	requestProvider := make(map[string]string)
	for _, event := range events {
		data, err := lifecycleObject(event.Data)
		if err != nil {
			return fmt.Errorf("session: %s data is not an object: %w", event.Type, err)
		}
		coord := func(key string) (int, error) {
			value, ok := data[key].(float64)
			if !ok || value != math.Trunc(value) || value <= 0 || value > float64(maxInt()) {
				return 0, fmt.Errorf("session: %s requires positive integer %s", event.Type, key)
			}
			return int(value), nil
		}
		switch event.Type {
		case EventTurnStart:
			if turn, ok := positiveInt(data["turn"]); ok {
				openTurn, openStep = turn, 0
			}
		case EventTurnEnd:
			openTurn, openStep = 0, 0
		case EventStepStart:
			turn, turnOK := positiveInt(data["turn"])
			step, stepOK := positiveInt(data["step"])
			if turnOK && stepOK {
				openTurn, openStep = turn, step
			}
		case EventStepEnd:
			openStep = 0
		case EventRequestHeader:
			turn, turnErr := coord("turn")
			step, stepErr := coord("step")
			if turnErr == nil && stepErr == nil {
				provider := nonEmptyString(data["provider"])
				// Reference request headers keep the effective route under
				// header.config. Go-generated headers also expose it at the top
				// level; accept both so replay validates imported canonical logs
				// without weakening provider association.
				if provider == "" {
					if header, ok := data["header"].(map[string]any); ok {
						if config, ok := header["config"].(map[string]any); ok {
							provider = nonEmptyString(config["provider"])
						}
					}
				}
				requestProvider[fmt.Sprintf("%d/%d", turn, step)] = provider
			}
		case EventLLMRetry:
			turn, err := coord("turn")
			if err != nil {
				return err
			}
			step, err := coord("step")
			if err != nil {
				return err
			}
			if openTurn != turn || openStep != step {
				return fmt.Errorf("session: llm/retry must be inside open turn/step %d/%d, open is %d/%d", turn, step, openTurn, openStep)
			}
			id, _ := data["retryId"].(string)
			provider, _ := data["provider"].(string)
			policy, _ := data["policyKey"].(string)
			mode, _ := data["mode"].(string)
			if id == "" || provider == "" || policy == "" {
				return fmt.Errorf("session: llm/retry requires retryId, provider and policyKey")
			}
			retry, ok := positiveInt(data["retry"])
			if !ok {
				return fmt.Errorf("session: llm/retry retry must be a positive integer")
			}
			delay, ok := nonNegativeInt64(data["delayMs"])
			if !ok || delay > 2147483647 {
				return fmt.Errorf("session: llm/retry delayMs must be an integer in 0..2147483647")
			}
			failure, ok := data["failure"].(map[string]any)
			if !ok || nonEmptyString(failure["message"]) == "" || nonEmptyString(failure["code"]) == "" {
				return fmt.Errorf("session: llm/retry failure requires non-empty message and code")
			}
			if status, present := failure["status"]; present && !wireStatus(status) {
				return fmt.Errorf("session: llm/retry failure status must be an integer in 100..599")
			}
			if retryAfter, present := failure["providerRetryAfterMs"]; present && !wirePositiveNumber(retryAfter) {
				return fmt.Errorf("session: llm/retry failure providerRetryAfterMs must be positive")
			}
			if requestID, present := failure["requestId"]; present && !wireNonEmptyString(requestID) {
				return fmt.Errorf("session: llm/retry failure requestId must be non-empty")
			}
			if mode != "normal" && mode != "always" {
				return fmt.Errorf("session: llm/retry mode must be normal or always")
			}
			if mode == "normal" {
				max, ok := positiveInt(data["maxRetries"])
				if !ok || retry > max {
					return fmt.Errorf("session: llm/retry %d exceeds maxRetries", retry)
				}
			} else if _, present := data["maxRetries"]; present {
				return fmt.Errorf("session: llm/retry always mode must omit maxRetries")
			}
			key := fmt.Sprintf("%d/%d/%s/%s", turn, step, provider, policy)
			if previous, exists := lastByChain[key]; exists {
				if retry != previous.retry+1 || id != previous.id {
					return fmt.Errorf("session: llm/retry chain %s is not sequential or changed retryId", key)
				}
			} else {
				for otherKey, other := range lastByChain {
					if otherKey != key && other.id == id {
						return fmt.Errorf("session: llm/retry retryId %q is already owned by another chain", id)
					}
				}
			}
			if routed := requestProvider[fmt.Sprintf("%d/%d", turn, step)]; routed != "" && routed != provider {
				return fmt.Errorf("session: llm/retry provider %q does not match request provider %q", provider, routed)
			}
			record := retryRecord{id: id, turn: turn, step: step, provider: provider, policy: policy, retry: retry}
			lastByChain[key] = record
			scheduled = append(scheduled, record)
		case EventLLMRetryStarted:
			turn, err := coord("turn")
			if err != nil {
				return err
			}
			step, err := coord("step")
			if err != nil {
				return err
			}
			id, _ := data["retryId"].(string)
			retry, ok := positiveInt(data["retry"])
			if id == "" || !ok {
				return fmt.Errorf("session: llm/retry-started requires retryId and positive retry")
			}
			key := fmt.Sprintf("%s/%d", id, retry)
			if _, exists := started[key]; exists {
				return fmt.Errorf("session: llm/retry-started repeats %s", key)
			}
			var match *retryRecord
			for i := range scheduled {
				if scheduled[i].id == id && scheduled[i].retry == retry {
					match = &scheduled[i]
				}
			}
			if match == nil || match.turn != turn || match.step != step {
				return fmt.Errorf("session: llm/retry-started has no matching scheduled retry")
			}
			started[key] = struct{}{}
		}
	}
	return nil
}

func positiveInt(value any) (int, bool) {
	n, ok := value.(float64)
	return int(n), ok && n == math.Trunc(n) && n > 0 && n <= float64(maxInt())
}

func nonNegativeInt64(value any) (int64, bool) {
	n, ok := value.(float64)
	return int64(n), ok && n == math.Trunc(n) && n >= 0 && n <= float64(math.MaxInt64)
}

func nonEmptyString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
