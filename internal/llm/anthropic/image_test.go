package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// TestStreamUserImageBlock verifies the M8-3b anthropic wire contract
// (dispatch-m8-3b §4.2/§7): a user message with an image block serializes a
// {"type":"image","source":{"type":"base64","media_type",...,"data":<base64>}}
// block in content order, the image bytes read from the ImageRef path at
// request time, with the surrounding text block preserved.
func TestStreamUserImageBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(path, []byte("pngbytes"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(sseEventLine("message_stop", `{"type":"message_stop"}`))))
	})
	p.supportsImages = true

	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.Text("what is this?"),
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Bytes: 8, Path: path}},
		}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	drain(t, reader)

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v, want 2 blocks (text + image)", msgs[0])
	}
	tx, _ := content[0].(map[string]any)
	if tx["type"] != "text" || tx["text"] != "what is this?" {
		t.Fatalf("text block = %v", tx)
	}
	img, _ := content[1].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("image block type = %v", img["type"])
	}
	src, _ := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("source = %v, want {type:base64 media_type:image/png}", src)
	}
	if src["data"] != base64.StdEncoding.EncodeToString([]byte("pngbytes")) {
		t.Fatalf("data = %v, want the base64 image bytes", src["data"])
	}
}

// TestStreamImageFailsClosedWhenNotSupported verifies the M8-3b fail-closed
// contract (dispatch-m8-3b §3): with SupportsImages=false (the default) an
// image request errors before any network call or file read.
func TestStreamImageFailsClosedWhenNotSupported(t *testing.T) {
	p := New(Config{APIKey: "k"}) // supportsImages defaults false
	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Path: "does-not-exist.png"}},
		}}},
	})
	if err == nil {
		t.Fatal("image with SupportsImages=false must fail closed")
	}
	if !strings.Contains(err.Error(), "anthropic: model does not support image input (model_input_modalities=text)") {
		t.Fatalf("err = %q, want the fail-closed image error", err)
	}
}

// TestStreamImageReadFailureFailsClosed verifies a read failure on an image
// path is an error (dispatch-m8-3b §3: 读图失败 fail-closed).
func TestStreamImageReadFailureFailsClosed(t *testing.T) {
	p := New(Config{APIKey: "k"})
	p.supportsImages = true
	_, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Path: filepath.Join(t.TempDir(), "missing.png")}},
		}}},
	})
	if err == nil {
		t.Fatal("read failure must fail closed")
	}
	if !strings.Contains(err.Error(), "read image") {
		t.Fatalf("err = %q, want a read-image error", err)
	}
}

// TestStreamImageOffloadedOverBudget verifies offload runs before
// serialization: an over-budget image is replaced by the OffloadedImageText
// placeholder, so the wire carries two text blocks (the leading text plus the
// placeholder) and never reads the dangling image path.
func TestStreamImageOffloadedOverBudget(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseEvents(sseEventLine("message_stop", `{"type":"message_stop"}`))))
	})
	p.supportsImages = true
	p.maxRequestImageBytes = 5 // the image Bytes (8) exceed the budget

	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.Text("desc"),
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Bytes: 8, Path: filepath.Join(t.TempDir(), "dangling.png")}},
		}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	drain(t, reader)

	msgs, _ := gotBody["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v, want 2 text blocks (desc + placeholder)", msgs[0])
	}
	if content[0].(map[string]any)["text"] != "desc" ||
		content[1].(map[string]any)["text"] != llm.OffloadedImageText {
		t.Fatalf("offloaded blocks = %v", content)
	}
}
