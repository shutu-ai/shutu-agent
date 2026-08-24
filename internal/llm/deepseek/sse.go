package deepseek

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
// each event's "data:" field content (multiple data lines joined with "\n"
// per the SSE spec); comment lines are skipped. A blank line terminates the
// current event; a blank line with no accumulated data (heartbeat) is skipped.
type sseDecoder struct {
	r *bufio.Reader
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
		CompletionDetails *struct {
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
	dec  *sseDecoder
	resp *http.Response
	done bool

	finishReason string
	reasoning    strings.Builder // accumulated reasoning_content deltas (M8)
	toolCalls    []llm.ToolCall  // in first-seen wire order
	toolIndex    map[int]int     // wire index -> position in toolCalls
	usage        llm.TokenUsage
}

func (r *streamReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	for {
		payload, err := r.dec.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return llm.StreamEvent{}, fmt.Errorf("deepseek: SSE stream ended without [DONE]")
			}
			return llm.StreamEvent{}, err
		}
		if payload == "[DONE]" {
			r.done = true
			if r.finishReason == "" {
				r.finishReason = "stop"
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
			return llm.StreamEvent{}, fmt.Errorf("deepseek: malformed SSE payload: %w", err)
		}
		if chunk.Usage != nil {
			r.usage = llm.TokenUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
			if chunk.Usage.PromptDetails != nil {
				r.usage.CachedInputTokens = chunk.Usage.PromptDetails.CachedTokens
			}
			if chunk.Usage.CompletionDetails != nil {
				r.usage.ReasoningTokens = chunk.Usage.CompletionDetails.ReasoningTokens
			}
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				r.finishReason = *choice.FinishReason
			}
			if choice.Delta.Content != "" {
				return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: choice.Delta.Content}, nil
			}
			// DeepSeek streams the assistant's reasoning in a parallel
			// reasoning_content delta (M8): accumulate it and surface it as a
			// reasoning event, parallel to the content-delta branch.
			if choice.Delta.ReasoningContent != "" {
				r.reasoning.WriteString(choice.Delta.ReasoningContent)
				return llm.StreamEvent{Kind: llm.StreamReasoningDelta, Text: choice.Delta.ReasoningContent}, nil
			}
			for _, tc := range choice.Delta.ToolCalls {
				r.accumulateToolCall(tc)
			}
		}
		// A payload with only finish_reason or empty content carries no
		// streamable delta; loop to the next SSE event.
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
