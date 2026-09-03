package deepseek

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

var errStreamIdleTimeout = errors.New("deepseek: stream idle timeout")

type idleRead struct {
	n   int
	err error
}

// idleBody makes a silent response interval cancellable even when the
// underlying net/http body is blocked in Read. The small result buffer keeps
// the read goroutine from leaking when the watchdog wins the race.
type idleBody struct {
	body    io.ReadCloser
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	once    sync.Once
}

func newIdleBody(body io.ReadCloser, ctx context.Context, cancel context.CancelFunc, timeout time.Duration) io.ReadCloser {
	if body == nil || timeout <= 0 {
		return body
	}
	return &idleBody{body: body, ctx: ctx, cancel: cancel, timeout: timeout}
}

func (b *idleBody) Read(p []byte) (int, error) {
	result := make(chan idleRead, 1)
	go func() {
		n, err := b.body.Read(p)
		result <- idleRead{n: n, err: err}
	}()
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case read := <-result:
		return read.n, read.err
	case <-b.ctx.Done():
		_ = b.Close()
		return 0, b.ctx.Err()
	case <-timer.C:
		b.cancel()
		_ = b.body.Close()
		return 0, errStreamIdleTimeout
	}
}

func (b *idleBody) Close() error {
	var err error
	b.once.Do(func() {
		b.cancel()
		err = b.body.Close()
	})
	return err
}

// sseDecoder decodes an SSE byte stream into event data payloads. It yields
// each event's "data:" field content (multiple data lines joined with "\n"
// per the SSE spec); comment lines are skipped. A blank line terminates the
// current event; a blank line with no accumulated data (heartbeat) is skipped.
type sseDecoder struct {
	r       *bufio.Reader
	started bool
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{r: bufio.NewReader(r)}
}

// Next returns the next event payload, or io.EOF at end of input.
func (d *sseDecoder) Next() (string, error) {
	var data []string
	for {
		line, err := d.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// eventsource-parser dispatches only on a blank-line event
				// terminator. An unterminated tail is a truncated stream, not
				// a valid payload that may be flushed at EOF.
				return "", io.EOF
			}
			return "", err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if !d.started {
			line = strings.TrimPrefix(line, "\ufeff")
			d.started = true
		}
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
			// other SSE fields (event:, id:, retry:) are not part of this
			// protocol and are ignored.
		}
	}
}

// wireChunk is one SSE data payload of a chat.completion.chunk stream.
type wireChunk struct {
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptDetails    *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
		CompletionDetails     *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// streamReader translates SSE payloads into llm.StreamEvents, accumulating
// tool-call argument fragments by their wire index and the reasoning text by a
// parallel builder.
type streamReader struct {
	dec             *sseDecoder
	resp            *http.Response
	ctx             context.Context
	provider        string
	done            bool
	credentialLease llm.CredentialLease

	finishReason string
	reasoning    strings.Builder // accumulated reasoning_content deltas (M8)
	toolCalls    []llm.ToolCall  // in first-seen wire order
	toolIndex    map[int]int     // wire index -> position in toolCalls
	usage        llm.TokenUsage
	sawContent   bool
	pending      []llm.StreamEvent
}

func (r *streamReader) label() string {
	if r.provider != "" {
		return r.provider
	}
	return "deepseek"
}

func (r *streamReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	if len(r.pending) > 0 {
		event := r.pending[0]
		r.pending = r.pending[1:]
		return event, nil
	}
	for {
		payload, err := r.dec.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.close()
				return llm.StreamEvent{}, llm.NewFailureError(r.label()+": SSE stream ended without [DONE]", "STREAM_CLOSED", err)
			}
			r.close()
			if errors.Is(err, errStreamIdleTimeout) {
				return llm.StreamEvent{}, llm.NewFailureError(r.label()+": SSE stream idle timeout", "TIMEOUT", err)
			}
			if r.ctx != nil && r.ctx.Err() != nil {
				return llm.StreamEvent{}, llm.NewFailureError(r.label()+": SSE stream aborted: "+r.ctx.Err().Error(), "ABORTED", r.ctx.Err())
			}
			return llm.StreamEvent{}, llm.NewFailureError(r.label()+": SSE stream read failed: "+err.Error(), "STREAM_CLOSED", err)
		}
		if payload == "[DONE]" {
			r.done = true
			r.close()
			if r.finishReason == "" {
				r.finishReason = "stop"
			}
			if !r.sawContent && (r.finishReason == "" || r.finishReason == "stop") {
				failure := llm.Failure{Message: r.label() + ": completed stream contained no content", Code: "EMPTY_RESPONSE"}
				return llm.StreamEvent{
					Kind: llm.StreamFinish, FinishReason: r.finishReason,
					ToolCalls: r.toolCalls, Reasoning: r.reasoning.String(), Usage: r.usage,
					Failure: &failure,
				}, nil
			}
			return llm.StreamEvent{
				Kind:         llm.StreamFinish,
				FinishReason: r.finishReason,
				ToolCalls:    r.toolCalls,
				Reasoning:    r.reasoning.String(), // accumulated reasoning deltas (M8)
				Usage:        r.usage,
			}, nil
		}

		var chunk wireChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			r.close()
			return llm.StreamEvent{}, llm.NewFailureError(r.label()+": malformed SSE payload: "+err.Error(), "MALFORMED_RESPONSE", err)
		}
		if chunk.Usage != nil {
			r.usage = llm.TokenUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
			cached := chunk.Usage.PromptCacheHitTokens
			if chunk.Usage.PromptDetails != nil && chunk.Usage.PromptDetails.CachedTokens > 0 {
				cached = chunk.Usage.PromptDetails.CachedTokens
			}
			if cached > 0 {
				// DeepSeek's prompt_tokens includes cache hits. Convert it
				// into the reference's disjoint uncached/cache-read buckets.
				r.usage.InputTokens -= cached
				if r.usage.InputTokens < 0 {
					r.usage.InputTokens = 0
				}
				r.usage.CacheReadTokens = cached
			}
			if chunk.Usage.CompletionDetails != nil {
				r.usage.ReasoningTokens = chunk.Usage.CompletionDetails.ReasoningTokens
			}
		}
		// A single SSE payload can contain multiple logical deltas. Preserve
		// all of them; returning on the first one used to silently drop the
		// remainder (notably reasoning+text in one chunk).
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				r.finishReason = *choice.FinishReason
			}
		}
		var events []llm.StreamEvent
		for _, choice := range chunk.Choices {
			// DeepSeek streams the assistant's reasoning in a parallel
			// reasoning_content delta (M8): accumulate it and surface it as a
			// reasoning event, parallel to the content-delta branch.
			if choice.Delta.ReasoningContent != "" {
				r.sawContent = true
				r.reasoning.WriteString(choice.Delta.ReasoningContent)
				events = append(events, llm.StreamEvent{Kind: llm.StreamReasoningDelta, Text: choice.Delta.ReasoningContent})
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				r.sawContent = true
				events = append(events, llm.StreamEvent{Kind: llm.StreamTextDelta, Text: choice.Delta.Content})
			}
		}
		for _, choice := range chunk.Choices {
			for _, tc := range choice.Delta.ToolCalls {
				r.accumulateToolCall(tc)
				r.sawContent = true
			}
		}
		if len(events) > 0 {
			r.pending = events
			event := r.pending[0]
			r.pending = r.pending[1:]
			return event, nil
		}
		// A payload with only finish_reason or empty content carries no
		// streamable delta; loop to the next SSE event.
	}
}

func (r *streamReader) close() {
	if r.resp != nil && r.resp.Body != nil {
		_ = r.resp.Body.Close()
	}
	if r.credentialLease != nil {
		r.credentialLease.Release()
		r.credentialLease = nil
	}
}

func (r *streamReader) accumulateToolCall(tc wireToolCall) {
	pos, ok := r.toolIndex[tc.Index]
	if !ok {
		pos = len(r.toolCalls)
		r.toolIndex[tc.Index] = pos
		r.toolCalls = append(r.toolCalls, llm.ToolCall{})
	}
	call := &r.toolCalls[pos]
	if tc.ID != "" {
		call.ID = tc.ID
	}
	if tc.Function.Name != "" {
		call.Name = tc.Function.Name
	}
	call.Arguments += tc.Function.Arguments
}
