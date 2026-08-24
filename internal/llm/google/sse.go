package google

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

// sseDecoder decodes an SSE byte stream into event data payloads (same
// protocol as the deepseek/anthropic providers): each event's "data:" field
// content, with multiple data lines joined by "\n"; a blank line terminates
// the current event. Google streams the JSON payload directly in data: lines.
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
			// other SSE fields are ignored
		}
	}
}

// wireChunk is one SSE data payload of a streamGenerateContent response.
type wireChunk struct {
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []part `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

// streamReader translates Gemini SSE payloads into llm.StreamEvents: text
// parts → StreamTextDelta, thought:true parts → StreamReasoningDelta, and
// functionCall parts → accumulated llm.ToolCalls (in first-seen order).
type streamReader struct {
	dec  *sseDecoder
	resp *http.Response
	done bool

	finishReason string
	reasoning    strings.Builder
	toolCalls    []llm.ToolCall
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
				// Gemini carries the finish reason on the final candidate chunk
				// (never a separate completed event); a clean EOF therefore
				// always finishes the stream.
				return r.finish("stop"), nil
			}
			return llm.StreamEvent{}, err
		}
		if strings.TrimSpace(payload) == "" {
			continue
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return llm.StreamEvent{}, fmt.Errorf("google: malformed SSE payload: %w", err)
		}
		if chunk.UsageMetadata != nil {
			r.usage = llm.TokenUsage{
				InputTokens:       chunk.UsageMetadata.PromptTokenCount,
				OutputTokens:      chunk.UsageMetadata.CandidatesTokenCount,
				TotalTokens:       chunk.UsageMetadata.TotalTokenCount,
				CachedInputTokens: chunk.UsageMetadata.CachedContentTokenCount,
				ReasoningTokens:   chunk.UsageMetadata.ThoughtsTokenCount,
			}
		}
		for _, cand := range chunk.Candidates {
			if cand.FinishReason != "" {
				r.finishReason = mapFinishReason(cand.FinishReason)
			}
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					args, _ := json.Marshal(p.FunctionCall.Args)
					r.toolCalls = append(r.toolCalls, llm.ToolCall{
						ID:        p.FunctionCall.Name + "-call",
						Name:      p.FunctionCall.Name,
						Arguments: string(args),
					})
					continue
				}
				if p.Text == "" {
					continue
				}
				if p.Thought {
					r.reasoning.WriteString(p.Text)
					return llm.StreamEvent{Kind: llm.StreamReasoningDelta, Text: p.Text}, nil
				}
				return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: p.Text}, nil
			}
		}
		// A payload with no streamable delta (empty/only finish) loops.
	}
}

// finish emits the terminal StreamFinish event, resolving the finish reason
// to "stop" when the stream never reported one.
func (r *streamReader) finish(fallback string) llm.StreamEvent {
	r.done = true
	reason := r.finishReason
	if reason == "" {
		reason = fallback
	}
	return llm.StreamEvent{
		Kind:         llm.StreamFinish,
		FinishReason: reason,
		ToolCalls:    r.toolCalls,
		Reasoning:    r.reasoning.String(),
		Usage:        r.usage,
	}
}

// mapFinishReason translates Gemini FinishReason to the provider-neutral
// vocabulary (pi-ai mapStopReason 范式): STOP → stop, MAX_TOKENS → length,
// everything else → error.
func mapFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		return "error"
	}
}

// newStreamReader wraps a 2xx response body in the SSE stream reader.
func newStreamReader(resp *http.Response) *streamReader {
	return &streamReader{dec: newSSEDecoder(resp.Body), resp: resp}
}
