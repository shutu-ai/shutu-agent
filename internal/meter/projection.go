package meter

import (
	"encoding/json"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// UsageProjection is the durable, cumulative provider-usage view. The four
// fields are disjoint; reasoning tokens are already part of output and are
// intentionally not accumulated a second time.
type UsageProjection struct {
	UncachedInputTokens int `json:"uncachedInputTokens"`
	OutputTokens        int `json:"outputTokens"`
	CacheReadTokens     int `json:"cacheReadTokens"`
	CacheWriteTokens    int `json:"cacheWriteTokens"`
}

// CompletionProjection is the replayable output ledger. Provider usage is the
// authoritative billing sample when present; this ledger is the deterministic
// fallback for surfaces that have no provider accounting (tool-only turns,
// streamed responses, and restored legacy logs). It intentionally counts only
// committed assistant/tool-result boundaries, never transient chunks.
type CompletionProjection struct {
	AssistantTokens  int   `json:"assistantTokens"`
	ReasoningTokens  int   `json:"reasoningTokens"`
	ToolCallTokens   int   `json:"toolCallTokens"`
	ToolResultTokens int   `json:"toolResultTokens"`
	AttachmentBytes  int64 `json:"attachmentBytes"`
}

// CompletionProjection returns the output-side ledger for a durable log.
func (m *Meter) CompletionProjection(log *session.Log) CompletionProjection {
	if log == nil {
		return CompletionProjection{}
	}
	return ProjectCompletion(log.Events())
}

// ProjectCompletion folds committed assistant messages and tool results. A
// message that carries both text and reasoning is priced block-by-block using
// the same estimator as context pressure, while image bytes remain an
// observable attachment metric rather than being silently converted to text.
func ProjectCompletion(events []session.Event) CompletionProjection {
	var result CompletionProjection
	for _, event := range events {
		if event.Type != session.EventAssistantMessage && event.Type != session.EventToolResult {
			continue
		}
		message, ok := session.DeriveEventMessage(event)
		if !ok {
			continue
		}
		if event.Type == session.EventAssistantMessage {
			result.AssistantTokens += EstimateMessage(message)
			for _, block := range message.Content {
				if block.Kind == llm.BlockReasoning {
					result.ReasoningTokens += ceilChars(block.Text) + 4
				}
				if block.Kind == llm.BlockImage {
					result.AttachmentBytes += block.Image.Bytes
				}
			}
			for _, call := range message.ToolCalls {
				result.ToolCallTokens += ceilChars(call.Name) + ceilChars(call.Arguments) + 4
			}
		} else {
			result.ToolResultTokens += EstimateMessage(message)
			for _, block := range message.Content {
				if block.Kind == llm.BlockImage {
					result.AttachmentBytes += block.Image.Bytes
				}
			}
		}
	}
	return result
}

// ContextPressureProjection is a last-sample prompt occupancy view. Pointer
// fields preserve the reference distinction between “not sampled yet” and a
// real zero value.
type ContextPressureProjection struct {
	PressureTokens  *int `json:"pressureTokens,omitempty"`
	ProjectedTokens *int `json:"projectedTokens,omitempty"`
	ContextWindow   *int `json:"contextWindow,omitempty"`
}

// ContextBreakdownProjection separates the heuristic composition of the next
// request. These figures are intentionally descriptive and do not replace the
// provider-anchored pressure sample.
type ContextBreakdownProjection struct {
	SystemTokens  int `json:"systemTokens"`
	ToolsTokens   int `json:"toolsTokens"`
	MessageTokens int `json:"messageTokens"`
}

// UsageProjection returns the cumulative provider usage represented by the
// session's durable usage samples. A final assistant/message replaces an
// earlier usage sample from the same turn/step rather than double-counting a
// streaming usage chunk.
func (m *Meter) UsageProjection(log *session.Log) UsageProjection {
	if log == nil {
		return UsageProjection{}
	}
	return ProjectUsage(log.Events())
}

// ProjectUsage folds provider usage samples from a detached event stream.
func ProjectUsage(events []session.Event) UsageProjection {
	type stepKey struct{ turn, step int }
	samples := make(map[stepKey]UsageProjection)
	for _, event := range events {
		turn, step, usage, ok := usageSample(event)
		if !ok {
			continue
		}
		samples[stepKey{turn: turn, step: step}] = usageBuckets(usage)
	}
	var total UsageProjection
	for _, sample := range samples {
		total.UncachedInputTokens += sample.UncachedInputTokens
		total.OutputTokens += sample.OutputTokens
		total.CacheReadTokens += sample.CacheReadTokens
		total.CacheWriteTokens += sample.CacheWriteTokens
	}
	return total
}

// ContextPressureProjection returns the newest provider prompt sample plus a
// surface-delta projection for the next request. It is intentionally a pure
// replay helper so callers can use it after restart without Meter callback
// state.
func (m *Meter) ContextPressureProjection(log *session.Log) ContextPressureProjection {
	if log == nil {
		return ContextPressureProjection{}
	}
	return ProjectContextPressure(log.Events())
}

// ProjectContextPressure folds request/context, usage, and surface events in
// replay order. Provider pressure excludes output tokens; projected pressure
// adds the heuristic surface movement since the usage sample.
func ProjectContextPressure(events []session.Event) ContextPressureProjection {
	var result ContextPressureProjection
	var sampledSurface int
	var haveSample bool
	for index, event := range events {
		if event.Type == "request/context" {
			var data struct {
				ContextWindow *int `json:"contextWindow"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				if data.ContextWindow == nil {
					result.ContextWindow = nil
				} else {
					value := *data.ContextWindow
					result.ContextWindow = &value
				}
			}
		}
		if _, _, usage, ok := usageSample(event); ok {
			pressure := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
			if usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
				pressure += usage.CachedInputTokens
			}
			result.PressureTokens = intPtr(pressure)
			// An assistant/message's own surface is appended after its usage
			// sample; chunk samples normally have no surface node, but the
			// before-event rule is correct for both forms.
			sampledSurface = surfaceTokensThrough(events[:index])
			haveSample = true
		}
	}
	if haveSample {
		current := surfaceTokensThrough(events)
		projected := *result.PressureTokens + current - sampledSurface
		if projected < 0 {
			projected = 0
		}
		result.ProjectedTokens = intPtr(projected)
	}
	return result
}

// ContextBreakdownProjection returns the newest request envelope split from
// the current folded conversation surface.
func (m *Meter) ContextBreakdownProjection(log *session.Log) ContextBreakdownProjection {
	if log == nil {
		return ContextBreakdownProjection{}
	}
	return ProjectContextBreakdown(log.Events())
}

// ProjectContextBreakdown folds request/header last-wins envelope pricing and
// the same replacement-aware message surface used by Measure.
func ProjectContextBreakdown(events []session.Event) ContextBreakdownProjection {
	var result ContextBreakdownProjection
	for _, event := range events {
		if event.Type != session.EventRequestHeader {
			continue
		}
		request := requestHeaderAt(event)
		if request == nil {
			continue
		}
		result.SystemTokens = estimateSystemTokens(request)
		result.ToolsTokens = estimateToolsTokens(request)
	}
	result.MessageTokens = surfaceTokensThrough(events)
	return result
}

func usageSample(event session.Event) (turn, step int, usage llm.TokenUsage, ok bool) {
	switch event.Type {
	case session.EventAssistantMessage:
		var data struct {
			Turn  int             `json:"turn"`
			Step  int             `json:"step"`
			Usage *llm.TokenUsage `json:"usage"`
		}
		if json.Unmarshal(event.Data, &data) != nil || data.Usage == nil {
			return 0, 0, llm.TokenUsage{}, false
		}
		return data.Turn, data.Step, *data.Usage, true
	case session.EventAssistantChunk:
		var data struct {
			Turn  int `json:"turn"`
			Step  int `json:"step"`
			Chunk struct {
				Type  string          `json:"type"`
				Usage *llm.TokenUsage `json:"usage"`
			} `json:"chunk"`
		}
		if json.Unmarshal(event.Data, &data) != nil || data.Chunk.Type != "usage" || data.Chunk.Usage == nil {
			return 0, 0, llm.TokenUsage{}, false
		}
		return data.Turn, data.Step, *data.Chunk.Usage, true
	default:
		return 0, 0, llm.TokenUsage{}, false
	}
}

func usageBuckets(usage llm.TokenUsage) UsageProjection {
	cacheRead := usage.CacheReadTokens
	if cacheRead == 0 && usage.CacheWriteTokens == 0 {
		cacheRead = usage.CachedInputTokens
	}
	return UsageProjection{
		UncachedInputTokens: usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheReadTokens:     cacheRead,
		CacheWriteTokens:    usage.CacheWriteTokens,
	}
}

func estimateSystemTokens(request *llm.ChatRequest) int {
	if request == nil {
		return 0
	}
	var system string
	for _, message := range request.Messages {
		if message.Role != llm.RoleSystem {
			continue
		}
		if system != "" {
			system += "\n"
		}
		system += message.Text()
	}
	if system == "" {
		return 0
	}
	return ceilChars(system) + 4
}

func estimateToolsTokens(request *llm.ChatRequest) int {
	if request == nil || len(request.Tools) == 0 {
		return 0
	}
	defs := make([]map[string]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		defs = append(defs, map[string]any{
			"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters,
		})
	}
	encoded, _ := json.Marshal(defs)
	return ceilChars(string(encoded)) + 4
}

func intPtr(value int) *int { return &value }
