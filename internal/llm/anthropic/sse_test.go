package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

// TestStreamToolUseAndReasoning verifies the SSE → StreamEvent mapping
// (dispatch-m8-2b §2.2 / §4.2): a sequence of content_block_start tool_use →
// input_json_delta (multi-segment) → thinking_delta → text_delta → tool_use
// stop → message_delta stop_reason=tool_use → message_stop produces
// StreamReasoningDelta / StreamTextDelta events and a StreamFinish whose tool
// call arguments are fully joined and whose Reasoning is fully accumulated.
func TestStreamToolUseAndReasoning(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(
			sseEventLine("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`),
			sseEventLine("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me"}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" think"}}`),
			sseEventLine("content_block_stop", `{"type":"content_block_stop","index":0}`),
			sseEventLine("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"The time is "}}`),
			sseEventLine("content_block_stop", `{"type":"content_block_stop","index":1}`),
			sseEventLine("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_time","input":{}}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":"}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"UTC\"}"}}`),
			sseEventLine("content_block_stop", `{"type":"content_block_stop","index":2}`),
			sseEventLine("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`),
			sseEventLine("message_stop", `{"type":"message_stop"}`),
		)))
	})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var reasoning, text strings.Builder
	var finish llm.StreamEvent
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		switch ev.Kind {
		case llm.StreamReasoningDelta:
			reasoning.WriteString(ev.Text)
		case llm.StreamTextDelta:
			text.WriteString(ev.Text)
		case llm.StreamFinish:
			finish = ev
		}
	}

	if reasoning.String() != "Let me think" {
		t.Errorf("reasoning deltas = %q, want %q", reasoning.String(), "Let me think")
	}
	if text.String() != "The time is " {
		t.Errorf("text deltas = %q, want %q", text.String(), "The time is ")
	}
	if finish.Reasoning != "Let me think" {
		t.Errorf("finish.Reasoning = %q, want the accumulated reasoning", finish.Reasoning)
	}
	if finish.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls (stop_reason tool_use)", finish.FinishReason)
	}
	if len(finish.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", finish.ToolCalls)
	}
	call := finish.ToolCalls[0]
	if call.ID != "toolu_01" || call.Name != "get_time" {
		t.Errorf("tool call = %+v", call)
	}
	if call.Arguments != `{"tz":"UTC"}` {
		t.Errorf("tool call arguments = %q, want the joined partial_json", call.Arguments)
	}
}

// TestStreamTextOnly verifies a plain text stream (no tool use, no reasoning)
// maps end_turn → stop (dispatch-m8-2b §2.2).
func TestStreamTextOnly(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(
			sseEventLine("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`),
			sseEventLine("content_block_stop", `{"type":"content_block_stop","index":0}`),
			sseEventLine("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`),
			sseEventLine("message_stop", `{"type":"message_stop"}`),
		)))
	})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text strings.Builder
	var finish llm.StreamEvent
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev.Kind == llm.StreamTextDelta {
			text.WriteString(ev.Text)
		}
		if ev.Kind == llm.StreamFinish {
			finish = ev
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("text = %q, want %q", text.String(), "Hello world")
	}
	if finish.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop (end_turn)", finish.FinishReason)
	}
	if len(finish.ToolCalls) != 0 || finish.Reasoning != "" {
		t.Errorf("finish = %+v, want no tool calls and empty reasoning", finish)
	}
}

// TestStreamMissingMessageStop verifies the stream-termination rule
// (dispatch-m8-2b §2.2 流终止 / §4.3): EOF before message_stop is an error.
func TestStreamMissingMessageStop(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(
			sseEventLine("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`),
			sseEventLine("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		)))
	})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if _, err := reader.Next(); err != nil { // the text delta is delivered
		t.Fatalf("first next: %v", err)
	}
	_, err = reader.Next() // EOF without message_stop
	if err == nil {
		t.Fatal("expected truncated-stream error")
	}
	if !strings.Contains(err.Error(), "message_stop") {
		t.Errorf("err = %q, want a message_stop mention", err)
	}
}

// TestStreamErrorEvent verifies an SSE error event surfaces the server message
// wrapped as an anthropic provider error (dispatch-m8-2b §2.2).
func TestStreamErrorEvent(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(
			sseEventLine("error", `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`),
		)))
	})

	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, err = reader.Next()
	if err == nil {
		t.Fatal("expected the SSE error event to surface")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("err = %q, want the error-event message", err)
	}
	if !strings.Contains(err.Error(), "anthropic: provider error") {
		t.Errorf("err = %q, want the provider-error wrapper", err)
	}
}

func TestStreamMalformedPayloadIsTypedFailure(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(
			sseEventLine("content_block_delta", `{"type":"content_block_delta"`),
		)))
	})
	reader, err := p.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, err = reader.Next()
	if failure, ok := llm.FailureFacts(err); !ok || failure.Code != "MALFORMED_RESPONSE" {
		t.Fatalf("malformed stream error = %v (typed=%v), want MALFORMED_RESPONSE", err, ok)
	}
}

// TestMapStopReason pins the stop_reason vocabulary mapping (dispatch-m8-2b
// §2.2): end_turn→stop, tool_use→tool_calls, max_tokens→max-tokens, others
// verbatim.
func TestMapStopReason(t *testing.T) {
	for _, tc := range []struct{ wire, want string }{
		{"end_turn", "stop"},
		{"tool_use", "tool_calls"},
		{"max_tokens", "max-tokens"},
		{"stop_sequence", "stop_sequence"},
		{"", ""},
	} {
		if got := mapStopReason(tc.wire); got != tc.want {
			t.Errorf("mapStopReason(%q) = %q, want %q", tc.wire, got, tc.want)
		}
	}
}

// TestSSEDecoder verifies the event:/data: parser in isolation: multi-line
// data is joined, comments are skipped, and trailing data without a blank line
// is still delivered.
func TestSSEDecoder(t *testing.T) {
	stream := "event: message_start\ndata: {\"a\":\ndata: \"b\"}\n\n" +
		": a comment line\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n"
	dec := newSSEDecoder(strings.NewReader(stream))

	ev, err := dec.Next()
	if err != nil {
		t.Fatalf("next 1: %v", err)
	}
	if ev.event != "message_start" || ev.data != "{\"a\":\n\"b\"}" {
		t.Errorf("event 1 = %+v", ev)
	}

	ev, err = dec.Next()
	if err != nil {
		t.Fatalf("next 2: %v", err)
	}
	if ev.event != "message_stop" || ev.data != `{"type":"message_stop"}` {
		t.Errorf("event 2 = %+v", ev)
	}

	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("next 3 err = %v, want io.EOF", err)
	}
}
