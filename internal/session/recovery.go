package session

import (
	"encoding/json"
	"fmt"
)

// InterruptedTurnClosers returns deterministic synthetic events for an open
// tail. It preserves all committed events, answers assistant tool calls that
// have no durable result, and then closes the step and turn. The caller owns
// whether the returned events are only projected (Inspect) or appended (Load).
//
// A tool call present in assistant/message but absent from tool/call is marked
// TOOL_NOT_STARTED. A tool/call without a result is marked
// TOOL_OUTCOME_UNKNOWN and carries sourceEventSeqs so projections can retain
// the execution-intent checkpoint.
func InterruptedTurnClosers(events []Event) ([]Event, error) {
	if len(events) == 0 {
		return nil, nil
	}

	turn, step := 0, 0
	turnNumber, stepNumber := 1, 1
	anchored := false
	type pendingTool struct {
		step    int
		name    string
		started uint64
	}
	pending := make(map[string]pendingTool)
	order := make([]string, 0)
	addPending := func(callID string, tool pendingTool) {
		if callID == "" {
			return
		}
		if _, exists := pending[callID]; !exists {
			order = append(order, callID)
		}
		pending[callID] = tool
	}
	clearPending := func() {
		pending = make(map[string]pendingTool)
		order = order[:0]
	}

	for _, event := range events {
		switch event.Type {
		case EventTurnStart:
			anchored = true
			if turn != 0 {
				return nil, fmt.Errorf("session: turn/start while turn %d is open", turnNumber)
			}
			turn = 1
			step = 0
			clearPending()
			turnNumber = positiveLifecycleNumber(event.Data, "turn", turnNumber)
		case EventTurnEnd:
			anchored = true
			if turn == 0 || step != 0 {
				return nil, fmt.Errorf("session: turn/end without a closed step")
			}
			turn = 0
			clearPending()
		case EventStepStart:
			anchored = true
			if turn == 0 || step != 0 {
				return nil, fmt.Errorf("session: step/start outside a turn or with step open")
			}
			step = 1
			stepNumber = positiveLifecycleNumber(event.Data, "step", stepNumber)
		case EventStepEnd:
			anchored = true
			if turn == 0 || step == 0 {
				return nil, fmt.Errorf("session: step/end without step/start")
			}
			step = 0
			clearPending()
		case EventAssistantMessage:
			if turn == 0 || step == 0 {
				continue
			}
			var data struct {
				Turn      int `json:"turn"`
				Step      int `json:"step"`
				ToolCalls []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"toolCalls"`
				Message *struct {
					Content []struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			callStep := stepNumber
			if data.Step > 0 {
				callStep = data.Step
			}
			for _, call := range data.ToolCalls {
				addPending(call.ID, pendingTool{step: callStep, name: call.Name})
			}
			if data.Message != nil {
				for _, block := range data.Message.Content {
					if block.Type == "tool-call" {
						addPending(block.ID, pendingTool{step: callStep, name: block.Name})
					}
				}
			}
		case EventToolCall:
			var data struct {
				CallID string `json:"callId"`
				Name   string `json:"name"`
				Step   int    `json:"step"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				if call, exists := pending[data.CallID]; exists {
					call.started = event.Seq
					if data.Name != "" {
						call.name = data.Name
					}
					if data.Step > 0 {
						call.step = data.Step
					}
					pending[data.CallID] = call
				}
			}
		case EventToolResult:
			var data struct {
				CallID  string `json:"callId"`
				Message *struct {
					Source struct {
						CallID string `json:"callId"`
					} `json:"source"`
				} `json:"message"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				callID := data.CallID
				if callID == "" && data.Message != nil {
					callID = data.Message.Source.CallID
				}
				delete(pending, callID)
			}
		}
	}

	if !anchored || turn == 0 {
		return nil, nil
	}
	last := events[len(events)-1]
	seq := last.Seq + 1
	when := last.At
	closers := make([]Event, 0, len(order)+2)
	for _, callID := range order {
		call, exists := pending[callID]
		if !exists {
			continue
		}
		if call.step <= 0 {
			call.step = stepNumber
		}
		code := "TOOL_NOT_STARTED"
		text := "The tool call was interrupted before the Harness recorded it as started. Retry it if it is still needed."
		if call.started != 0 {
			code = "TOOL_OUTCOME_UNKNOWN"
			text = "The tool call was interrupted after it was recorded, but no result was durably recorded. Its outcome is unknown. Decide whether to retry from the tool semantics: retry only if the operation is read-only or idempotent; if it may have side effects, first verify external state or ask the user. Do not retry blindly."
		}
		payload := NewToolErrorResultAtCodeWithSource(turnNumber, call.step, callID, call.name, text, nil, code, call.started)
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("session: marshal interrupted tool result: %w", err)
		}
		closers = append(closers, Event{Seq: seq, Type: EventToolResult, At: when, Version: EventVersion, Data: raw})
		seq++
	}
	if step != 0 {
		raw, err := json.Marshal(NewStepEndAt(turnNumber, stepNumber, "interrupted", "recovered after crash"))
		if err != nil {
			return nil, fmt.Errorf("session: marshal interrupted step end: %w", err)
		}
		closers = append(closers, Event{Seq: seq, Type: EventStepEnd, At: when, Version: EventVersion, Data: raw})
		seq++
	}
	raw, err := json.Marshal(NewTurnEndAt(turnNumber, "interrupted", "recovered after crash"))
	if err != nil {
		return nil, fmt.Errorf("session: marshal interrupted turn end: %w", err)
	}
	closers = append(closers, Event{Seq: seq, Type: EventTurnEnd, At: when, Version: EventVersion, Data: raw})
	return closers, nil
}

func positiveLifecycleNumber(raw json.RawMessage, key string, fallback int) int {
	var data map[string]any
	if json.Unmarshal(raw, &data) == nil {
		if value, ok := data[key].(float64); ok && int(value) > 0 {
			return int(value)
		}
	}
	return fallback
}
