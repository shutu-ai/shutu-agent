package deepseek

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// writeTestImage writes a small fake image file and returns its path. The
// bytes are arbitrary (M8-3 decodes no image content — 宽高不解析).
func writeTestImage(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "pic.png")
	data := []byte("fake-png-bytes-0123456789")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	return path
}

// imageMsg builds a user message carrying one image block.
func imageMsg(text string, ref llm.ImageRef) llm.Message {
	content := []llm.ContentBlock{}
	if text != "" {
		content = append(content, llm.Text(text))
	}
	content = append(content, llm.ContentBlock{Kind: llm.BlockImage, Image: ref})
	return llm.Message{Role: llm.RoleUser, Content: content}
}

// TestStreamImageRequestPartsArray verifies the M8-3b wire contract
// (dispatch-m8-3b §4.1/§7): a request with an image serializes the message
// content as a parts array — a leading text part, then an image_url part whose
// url is a data URL with the data:image/png;base64, prefix carrying the image
// file bytes read at request time.
func TestStreamImageRequestPartsArray(t *testing.T) {
	dir := t.TempDir()
	path := writeTestImage(t, dir)
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	c.supportsImages = true

	reader, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{imageMsg("what is this?", llm.ImageRef{MediaType: "image/png", Bytes: 24, Path: path})},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := drainReader(reader); err != nil {
		t.Fatalf("drain: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	content, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content must be a parts array, got %T", msgs[0].(map[string]any)["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content parts = %v, want 2 (text + image_url)", content)
	}
	text, _ := content[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is this?" {
		t.Fatalf("text part = %v", text)
	}
	img, _ := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part type = %v", img["type"])
	}
	iu, _ := img["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("fake-png-bytes-0123456789"))
	if url != wantURL {
		t.Fatalf("data URL = %q, want %q", url, wantURL)
	}
}

// TestStreamNoImageContentStaysString verifies the regression: a text-only
// request keeps the single-string wire content (dispatch-m8-3b §4.1: 无图=string
// 兼容，wire 与既有测试不变).
func TestStreamNoImageContentStaysString(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	reader, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hi")}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := drainReader(reader); err != nil {
		t.Fatalf("drain: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["content"] != "hi" {
		t.Fatalf("content = %v (%T), want the string \"hi\"", first["content"], first["content"])
	}
}

// TestStreamImageFailsClosedWhenNotSupported verifies the M8-3b fail-closed
// contract (dispatch-m8-3b §3): with SupportsImages=false (the default,
// model_input_modalities=text) an image request errors before any network call
// or file read — never silently dropped.
func TestStreamImageFailsClosedWhenNotSupported(t *testing.T) {
	c := New(Config{APIKey: "k"}) // supportsImages defaults false
	_, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{imageMsg("", llm.ImageRef{MediaType: "image/png", Path: "does-not-exist.png"})},
	})
	if err == nil {
		t.Fatal("image with SupportsImages=false must fail closed")
	}
	if !strings.Contains(err.Error(), "deepseek-official: model does not support image input (model_input_modalities=text)") {
		t.Fatalf("err = %q, want the fail-closed image error", err)
	}
}

// TestStreamImageReadFailureFailsClosed verifies a read failure on an image
// path is an error (dispatch-m8-3b §3: 读图失败 fail-closed, 不静默丢图).
func TestStreamImageReadFailureFailsClosed(t *testing.T) {
	c := New(Config{APIKey: "k"})
	c.supportsImages = true
	_, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{imageMsg("", llm.ImageRef{MediaType: "image/png", Path: filepath.Join(t.TempDir(), "missing.png")})},
	})
	if err == nil {
		t.Fatal("read failure must fail closed")
	}
	if !strings.Contains(err.Error(), "read image") {
		t.Fatalf("err = %q, want a read-image error", err)
	}
}

// TestStreamImageOffloadedOverBudget verifies offload runs before
// serialization: an image whose bytes exceed the request budget is replaced by
// the OffloadedImageText placeholder, so the wire carries the text string (no
// image part, no file read — the Path is deliberately dangling).
func TestStreamImageOffloadedOverBudget(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]")))
	})
	c.supportsImages = true
	c.maxRequestImageBytes = 10 // the image Bytes (24) exceed the budget

	reader, err := c.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{imageMsg("desc", llm.ImageRef{MediaType: "image/png", Bytes: 24, Path: filepath.Join(t.TempDir(), "dangling.png")})},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := drainReader(reader); err != nil {
		t.Fatalf("drain: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	// The image block became a text block in place; Text() concatenates the
	// text blocks directly (no separator), so the wire carries the placeholder
	// glued to the leading text.
	if first["content"] != "desc"+llm.OffloadedImageText {
		t.Fatalf("content = %v, want the offloaded text string %q", first["content"], "desc"+llm.OffloadedImageText)
	}
}

// drainReader consumes a stream reader to EOF.
func drainReader(reader llm.StreamReader) error {
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
