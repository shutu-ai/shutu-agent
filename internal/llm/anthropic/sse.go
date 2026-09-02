package anthropic

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jabing/shutu-agent/internal/llm"
)

// sseEvent is one parsed SSE event: the event name plus the joined data field.
type sseEvent struct {
	event string
	data  string
}

// sseDecoder parses an Anthropic SSE stream (dispatch-m8-2b §2.2: event: +
// data: lines, unlike OpenAI's bare data: JSON). Comment lines are skipped; a
// blank line terminates the current event.
type sseDecoder struct {
	r *bufio.Reader
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{r: bufio.NewReader(r)}
}

// Next returns the next event, or io.EOF at end of input.
func (d *sseDecoder) Next() (sseEvent, error) {
	var event string
	var data []string
	for {
		line, err := d.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if event != "" || len(data) > 0 {
					return sseEvent{event: event, data: strings.Join(data, "\n")}, nil
				}
				return sseEvent{}, io.EOF
			}
			return sseEvent{}, err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			if event != "" || len(data) > 0 {
				return sseEvent{event: event, data: strings.Join(data, "\n")}, nil
			}
		case strings.HasPrefix(line, ":"):
			// comment line
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// id:/retry: fields are not part of this protocol and are ignored.
		}
	}
}

// streamReader translates Anthropic SSE events into llm.StreamEvents
// (dispatch-m8-2b §2.2). Tool-call arguments are accumulated by wire block
// index from input_json_delta fragments; thinking_delta becomes a reasoning
// delta (parallel to text); message_stop emits the finish with the accumulated
// tool calls and reasoning. The response body is closed once the stream is
// terminal (finish or error), releasing the connection.
type streamReader struct {
	dec             *sseDecoder
	resp            *http.Response
	done            bool
	credentialLease llm.CredentialLease

	stopReason string
	reasoning  strings.Builder // accumulated thinking deltas (M8)
	toolCalls  []llm.ToolCall  // in first-seen wire block order
	toolIndex  map[int]int     // wire block index -> position in toolCalls
	usage      llm.TokenUsage
	sawContent bool
	parseErr   error
}

func newStreamReader(resp *http.Response) *streamReader {
	return &streamReader{
		dec:       newSSEDecoder(resp.Body),
		resp:      resp,
		toolIndex: map[int]int{},
	}
}

// Next returns the next StreamEvent, or io.EOF after message_stop.
func (r *streamReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	for {
		ev, err := r.dec.Next()
		if err != nil {
			// EOF before message_stop: the stream was cut short
			// (dispatch-m8-2b §2.2 流终止).
			if errors.Is(err, io.EOF) {
				r.close()
				return llm.StreamEvent{}, llm.NewFailureError("anthropic: stream ended without message_stop", "STREAM_CLOSED", err)
			}
			r.close()
			return llm.StreamEvent{}, llm.NewFailureError("anthropic: stream read failed: "+err.Error(), "STREAM_CLOSED", err)
		}
		switch ev.event {
		case "message_start":
			r.onMessageStart(ev.data)
		case "content_block_start":
			r.onContentBlockStart(ev.data)
		case "content_block_delta":
			if out, ok := r.onContentBlockDelta(ev.data); ok {
				return out, nil
			}
		case "content_block_stop":
			// Ignored (the accumulated block is only read at the finish).
		case "message_delta":
			r.onMessageDelta(ev.data)
		case "message_stop":
			if r.parseErr != nil {
				err := r.parseErr
				r.close()
				return llm.StreamEvent{}, llm.NewFailureError("anthropic: malformed SSE payload: "+err.Error(), "MALFORMED_RESPONSE", err)
			}
			r.done = true
			r.close()
			if !r.sawContent && (r.stopReason == "" || mapStopReason(r.stopReason) == "stop") {
				failure := llm.Failure{Message: "anthropic: completed stream contained no content", Code: "EMPTY_RESPONSE"}
				return llm.StreamEvent{Kind: llm.StreamFinish, FinishReason: "stop", Failure: &failure, Usage: r.usage}, nil
			}
			return llm.StreamEvent{
				Kind:         llm.StreamFinish,
				FinishReason: mapStopReason(r.stopReason),
				ToolCalls:    r.toolCalls,
				Reasoning:    r.reasoning.String(),
				Usage:        r.usage,
			}, nil
		case "error":
			r.close()
			return llm.StreamEvent{}, llm.NewFailureError("anthropic: provider error: "+eventErrorMessage(ev.data), "SERVER", nil)
		default:
			// Unknown event type: ignore.
		}
		if r.parseErr != nil {
			err := r.parseErr
			r.close()
			return llm.StreamEvent{}, llm.NewFailureError("anthropic: malformed SSE payload: "+err.Error(), "MALFORMED_RESPONSE", err)
		}
	}
}

func (r *streamReader) onMessageStart(data string) {
	var start struct {
		Message struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &start); err != nil {
		r.parseErr = err
		return
	}
	r.usage.InputTokens = start.Message.Usage.InputTokens
	r.usage.CacheWriteTokens = start.Message.Usage.CacheCreationInputTokens
	r.usage.CacheReadTokens = start.Message.Usage.CacheReadInputTokens
}

// close releases the response body. Safe to call multiple times.
func (r *streamReader) close() {
	if r.resp != nil && r.resp.Body != nil {
		r.resp.Body.Close()
	}
	if r.credentialLease != nil {
		r.credentialLease.Release()
		r.credentialLease = nil
	}
}

// onContentBlockStart records a tool_use block's id/name at its wire index;
// other block types are ignored (dispatch-m8-2b §2.2).
func (r *streamReader) onContentBlockStart(data string) {
	var start struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(data), &start); err != nil {
		r.parseErr = err
		return
	}
	if start.ContentBlock.Type != "tool_use" {
		return
	}
	r.sawContent = true
	r.toolIndex[start.Index] = len(r.toolCalls)
	r.toolCalls = append(r.toolCalls, llm.ToolCall{ID: start.ContentBlock.ID, Name: start.ContentBlock.Name})
}

// onContentBlockDelta maps a delta to a StreamEvent (dispatch-m8-2b §2.2):
//   - text_delta → StreamTextDelta;
//   - thinking_delta → StreamReasoningDelta (accumulated and surfaced);
//   - input_json_delta → appended to the block's tool-call arguments (no event);
//   - signature_delta → ignored (thinking signatures are out of scope M8-2b).
func (r *streamReader) onContentBlockDelta(data string) (llm.StreamEvent, bool) {
	var delta struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(data), &delta); err != nil {
		r.parseErr = err
		return llm.StreamEvent{}, false
	}
	switch delta.Delta.Type {
	case "text_delta":
		if delta.Delta.Text == "" {
			return llm.StreamEvent{}, false
		}
		r.sawContent = true
		return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: delta.Delta.Text}, true
	case "thinking_delta":
		if delta.Delta.Thinking == "" {
			return llm.StreamEvent{}, false
		}
		r.sawContent = true
		r.reasoning.WriteString(delta.Delta.Thinking)
		return llm.StreamEvent{Kind: llm.StreamReasoningDelta, Text: delta.Delta.Thinking}, true
	case "input_json_delta":
		r.accumulateToolArguments(delta.Index, delta.Delta.PartialJSON)
		return llm.StreamEvent{}, false
	case "signature_delta":
		return llm.StreamEvent{}, false
	}
	return llm.StreamEvent{}, false
}

// accumulateToolArguments appends an input_json_delta fragment to the tool
// call at the same wire index (dispatch-m8-2b §2.2 工具调用累积, mirroring the
// deepseek accumulateToolCall index association). A fragment without a
// preceding tool_use start creates a placeholder so it is never dropped.
func (r *streamReader) accumulateToolArguments(index int, partial string) {
	pos, ok := r.toolIndex[index]
	if !ok {
		pos = len(r.toolCalls)
		r.toolIndex[index] = pos
		r.toolCalls = append(r.toolCalls, llm.ToolCall{})
	}
	r.toolCalls[pos].Arguments += partial
}

// onMessageDelta records the stop_reason for the finish event
// (dispatch-m8-2b §2.2).
func (r *streamReader) onMessageDelta(data string) {
	var md struct {
		Delta struct {
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &md); err != nil {
		r.parseErr = err
		return
	}
	if md.Delta.StopReason != nil {
		r.stopReason = *md.Delta.StopReason
	}
	if md.Usage.OutputTokens > 0 {
		r.usage.OutputTokens = md.Usage.OutputTokens
	}
	r.usage.TotalTokens = r.usage.InputTokens + r.usage.OutputTokens
}

// mapStopReason maps the Anthropic stop_reason vocabulary to the
// provider-neutral finish reason (dispatch-m8-2b §2.2): end_turn→stop,
// tool_use→tool_calls, max_tokens→max-tokens, anything else verbatim.
func mapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "max-tokens"
	}
	return reason
}

// eventErrorMessage extracts the server message from an SSE error event's data
// ({"type":"error","error":{...}}, dispatch-m8-2b §2.2); the raw data is used
// when it cannot be parsed.
func eventErrorMessage(data string) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(data)
}
