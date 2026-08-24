package openairesponses

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jabing/shutu-agent/internal/llm"
)

// sseDecoder decodes an SSE byte stream into event data payloads. It yields
// each event's "data:" field content (multiple data lines joined with "\n");
// comment lines are skipped. OpenAI Responses streams one JSON event per data
// block; the "event:" field is informational and ignored.
type sseDecoder struct {
	r *bufio.Reader
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{r: bufio.NewReader(r)}
}

func (d *sseDecoder) Next() (string, error) {
	var data []string
	for {
		line, err := d.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(data) > 0 {
					return strings.Join(data, "\n"), nil
				}
				return "", io.EOF
			}
			return "", err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
		case strings.HasPrefix(line, ":"):
			// comment line
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// other SSE fields (event:, id:) are ignored
		}
	}
}

// wireEvent is the common envelope of every Responses SSE event: the "type"
// discriminator plus the per-type fields we consume. Unknown fields are left
// untouched by json.
type wireEvent struct {
	Type     string         `json:"type"`
	Delta    string         `json:"delta"`
	Item     *wireEventItem `json:"item"`
	Response *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
			InputDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

// wireEventItem is the item shape within output_item.added / delta / done
// events (the fields we consume; others are ignored).
type wireEventItem struct {
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// streamReader translates the Responses SSE event stream into llm.StreamEvents,
// accumulating function-call argument deltas per item id and the reasoning text
// by a parallel builder.
type streamReader struct {
	dec  *sseDecoder
	resp *http.Response
	done bool

	finishReason string
	reasoning    strings.Builder
	// function call accumulation, keyed by the wire function_call item id; the
	// llm.ToolCall.id is the Responses call_id (echoed by function_call_output).
	funcCalls map[string]*funcAccum
	order     []string
	usage     llm.TokenUsage
}

type funcAccum struct {
	callID    string
	name      string
	arguments strings.Builder
}

func (r *streamReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	for {
		payload, err := r.dec.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(r.order) > 0 {
					return r.finish("tool_calls"), nil
				}
				return llm.StreamEvent{}, fmt.Errorf("openairesponses: SSE stream ended without response.completed")
			}
			return llm.StreamEvent{}, err
		}
		if strings.TrimSpace(payload) == "" {
			continue
		}
		var ev wireEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return llm.StreamEvent{}, fmt.Errorf("openairesponses: malformed SSE payload: %w", err)
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: ev.Delta}, nil
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if ev.Delta != "" {
				r.reasoning.WriteString(ev.Delta)
				return llm.StreamEvent{Kind: llm.StreamReasoningDelta, Text: ev.Delta}, nil
			}
		case "response.function_call_arguments.delta":
			r.accumDelta(ev.Item, ev.Delta)
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				r.closeCall(*ev.Item)
			}
		case "response.completed":
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					r.usage = llm.TokenUsage{
						InputTokens:  ev.Response.Usage.InputTokens,
						OutputTokens: ev.Response.Usage.OutputTokens,
						TotalTokens:  ev.Response.Usage.TotalTokens,
					}
					if ev.Response.Usage.InputDetails != nil {
						r.usage.CachedInputTokens = ev.Response.Usage.InputDetails.CachedTokens
					}
					if ev.Response.Usage.OutputDetails != nil {
						r.usage.ReasoningTokens = ev.Response.Usage.OutputDetails.ReasoningTokens
					}
				}
				switch ev.Response.Status {
				case "completed":
					r.finishReason = "stop"
				case "incomplete":
					r.finishReason = "length"
				default:
					r.finishReason = "error"
				}
			}
			return r.finish("stop"), nil
		case "response.failed":
			return r.finish("error"), nil
		}
		// Other events (response.created, response.output_item.added,
		// response.content_part.added, ...) carry no streamable delta.
	}
}

// accumDelta feeds a function_call item's argument delta into the accumulator
// (creating it on first sight, from the output_item.added item or this delta's
// own item — either may carry the call id/name first).
func (r *streamReader) accumDelta(item *wireEventItem, delta string) {
	if item == nil {
		return
	}
	acc, ok := r.funcCalls[item.ID]
	if !ok {
		acc = &funcAccum{callID: item.ID, name: item.Name}
		r.funcCalls[item.ID] = acc
		r.order = append(r.order, item.ID)
	}
	if item.CallID != "" {
		acc.callID = item.CallID
	}
	if item.Name != "" {
		acc.name = item.Name
	}
	acc.arguments.WriteString(delta)
}

// closeCall finalizes a function_call item from output_item.done, which carries
// the complete arguments string.
func (r *streamReader) closeCall(item wireEventItem) {
	acc, ok := r.funcCalls[item.ID]
	if !ok {
		acc = &funcAccum{callID: item.ID, name: item.Name}
		r.funcCalls[item.ID] = acc
		r.order = append(r.order, item.ID)
	}
	if item.CallID != "" {
		acc.callID = item.CallID
	}
	if item.Name != "" {
		acc.name = item.Name
	}
	if item.Arguments != "" && acc.arguments.Len() == 0 {
		acc.arguments.WriteString(item.Arguments)
	}
}

// finish emits the terminal StreamFinish event with the accumulated tool calls
// in first-seen wire order.
func (r *streamReader) finish(fallback string) llm.StreamEvent {
	r.done = true
	reason := r.finishReason
	if reason == "" {
		reason = fallback
	}
	calls := make([]llm.ToolCall, 0, len(r.order))
	for _, id := range r.order {
		acc := r.funcCalls[id]
		if acc == nil {
			continue
		}
		calls = append(calls, llm.ToolCall{ID: acc.callID, Name: acc.name, Arguments: acc.arguments.String()})
	}
	return llm.StreamEvent{
		Kind:         llm.StreamFinish,
		FinishReason: reason,
		ToolCalls:    calls,
		Reasoning:    r.reasoning.String(),
		Usage:        r.usage,
	}
}

// newStreamReader wraps a 2xx response body in the SSE stream reader.
func newStreamReader(resp *http.Response) *streamReader {
	return &streamReader{
		dec:       newSSEDecoder(resp.Body),
		resp:      resp,
		funcCalls: map[string]*funcAccum{},
	}
}
