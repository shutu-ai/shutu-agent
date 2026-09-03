package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// bigOutputTool returns a fixed oversized payload.
type bigOutputTool struct {
	text string
}

type richBigOutputTool struct{ text string }

type contextHandleTool struct{}

type invalidContentTool struct{}

func (invalidContentTool) Name() string        { return "invalid_content" }
func (invalidContentTool) Description() string { return "returns an invalid content tag" }
func (invalidContentTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (invalidContentTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (invalidContentTool) Execute(context.Context, any) (string, error) { return "unused", nil }
func (invalidContentTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{
		Value: "ok", Output: "ok", Content: []llm.ContentBlock{{Kind: "future-block"}},
		AdditionalContextMessages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("context survives validation")}}},
	}, nil
}

func (contextHandleTool) Name() string        { return "context_handle" }
func (contextHandleTool) Description() string { return "returns a context handle" }
func (contextHandleTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (contextHandleTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (contextHandleTool) Execute(context.Context, any) (string, error) { return "ok", nil }
func (contextHandleTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{Value: "ok", Output: "ok", AdditionalContexts: []string{"ctx-1", "ctx-2"}}, nil
}

func (richBigOutputTool) Name() string        { return "rich_big_out" }
func (richBigOutputTool) Description() string { return "returns rich content and text" }
func (richBigOutputTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (richBigOutputTool) OutputSchema() map[string]any                   { return map[string]any{"type": "string"} }
func (b richBigOutputTool) Execute(context.Context, any) (string, error) { return b.text, nil }
func (b richBigOutputTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{Output: b.text, Content: []llm.ContentBlock{{Kind: llm.BlockImage, Image: llm.ImageRef{ID: "att-1", MediaType: "image/png"}}}}, nil
}

func (bigOutputTool) Name() string        { return "big_out" }
func (bigOutputTool) Description() string { return "returns a lot of text" }
func (bigOutputTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (bigOutputTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (b bigOutputTool) Execute(ctx context.Context, args any) (string, error) {
	return b.text, nil
}

// TestExecuteOutputTruncationSpills covers 截断 + spill 文件生成与定位符: an
// oversized tool result is truncated to the model-facing cap, the full text is
// spilled under data/spill/<session>-<seq>.txt, and the locator is embedded in
// the returned text (which the loop logs into tool/result, D3).
func TestExecuteOutputTruncationSpills(t *testing.T) {
	spillDir := t.TempDir()
	big := strings.Repeat("0123456789", 20_000) // 200KB

	r := New()
	r.Register(bigOutputTool{text: big})
	seq := uint64(42)
	r.SetPolicy(Policy{
		Enabled:     []string{"big_out"},
		Timeout:     time.Hour,
		OutputLimit: 1024,
		SpillDir:    spillDir,
	})
	r.SetOwner(Owner{SessionID: "s-test", NextSeq: func() uint64 { return seq }})

	res, err := r.Execute(context.Background(), "big_out", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.SpillPath == "" {
		t.Fatal("expected a spill path for oversized output")
	}
	if !strings.Contains(res.Output, res.SpillPath) {
		t.Fatalf("truncated output must carry the locator; got head: %.80q", res.Output)
	}
	if !strings.Contains(res.Output, "output truncated") {
		t.Fatalf("truncated output must say it is truncated: %.80q", res.Output)
	}
	if len(res.Output) > 1024 {
		t.Fatalf("model-facing output = %d bytes, exceeds cap 1024", len(res.Output))
	}
	if res.SpillBytes != len(big) {
		t.Fatalf("spill bytes = %d, want %d", res.SpillBytes, len(big))
	}

	// The spill file is data/spill/<session>-<seq>.txt and holds the full text.
	wantName := "s-test-42.txt"
	spillPath := filepath.Join(spillDir, wantName)
	if res.SpillPath != spillPath {
		t.Fatalf("spill path = %q, want %q", res.SpillPath, spillPath)
	}
	got, err := os.ReadFile(spillPath)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if string(got) != big {
		t.Fatal("spill file does not contain the full output")
	}
}

func TestExecuteOutputUnderLimitNoSpill(t *testing.T) {
	r := New()
	r.Register(bigOutputTool{text: "small"})
	r.SetPolicy(Policy{
		Enabled:     []string{"big_out"},
		Timeout:     time.Hour,
		OutputLimit: 1024,
		SpillDir:    t.TempDir(),
	})
	res, err := r.Execute(context.Background(), "big_out", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Output != "small" || res.SpillPath != "" {
		t.Fatalf("res = %+v, want inline output with no spill", res)
	}
}

func TestSpillSkipsRichContent(t *testing.T) {
	big := strings.Repeat("x", 4096)
	r := New()
	if err := r.Register(richBigOutputTool{text: big}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"rich_big_out"}, OutputLimit: 64, SpillDir: t.TempDir()})
	res, err := r.Execute(context.Background(), "rich_big_out", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.SpillPath != "" || res.Output != big || len(res.Content) != 1 {
		t.Fatalf("rich result was rewritten by text spill policy: %+v", res)
	}
}

func TestOutputNormalizationPreservesAdditionalContexts(t *testing.T) {
	r := New()
	if err := r.Register(contextHandleTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"context_handle"}, OutputLimit: 64})
	res, err := r.Execute(context.Background(), "context_handle", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.AdditionalContexts, ","), "ctx-1,ctx-2"; got != want {
		t.Fatalf("additional contexts = %q, want %q", got, want)
	}
}

func TestInvalidRichContentFailsClosed(t *testing.T) {
	r := New()
	if err := r.Register(invalidContentTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"invalid_content"}})
	res, err := r.Execute(context.Background(), "invalid_content", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Error == nil || res.Error.Code != CodeInvalidToolOutput {
		t.Fatalf("invalid rich result = %+v, want INVALID_TOOL_OUTPUT", res)
	}
	if !strings.Contains(res.Output, "unsupported block kind") {
		t.Fatalf("invalid rich result output = %q", res.Output)
	}
	if len(res.AdditionalContextMessages) != 1 || res.AdditionalContextMessages[0].Text() != "context survives validation" {
		t.Fatalf("invalid rich result dropped additional context: %+v", res.AdditionalContextMessages)
	}
}

// TestSpillKeepsInlineOnWriteFailure is the best-effort degradation: a spill
// write failure must never turn a successful tool call into an error, so the
// inline result is kept (mirrors dsh-spill-policy).
func TestSpillKeepsInlineOnWriteFailure(t *testing.T) {
	spillDir := t.TempDir()
	// Occupy the spill directory path with a file so MkdirAll fails.
	blocker := filepath.Join(spillDir, "spill")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	big := strings.Repeat("x", 4096)
	r := New()
	r.Register(bigOutputTool{text: big})
	r.SetPolicy(Policy{
		Enabled:     []string{"big_out"},
		Timeout:     time.Hour,
		OutputLimit: 64,
		SpillDir:    blocker,
	})
	res, err := r.Execute(context.Background(), "big_out", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("spill failure must not fail the tool call: %v", err)
	}
	if res.Output != big || res.SpillPath != "" {
		t.Fatalf("res = %+v, want inline full output", res)
	}
}

// TestTruncateUTF8NeverSplitsRune verifies the byte cap backs off to a rune
// boundary (multibyte content must not be corrupted).
func TestTruncateUTF8NeverSplitsRune(t *testing.T) {
	// "你" is 3 UTF-8 bytes; capping mid-rune must back off to a boundary.
	s := "你好世界"
	got := truncateUTF8(s, 4)
	if !strings.HasPrefix(s, got) {
		t.Fatalf("truncated %q is not a prefix of %q", got, s)
	}
	if len(got) > 4 {
		t.Fatalf("truncated length %d > 4", len(got))
	}
	// A valid rune must be kept whole: byte 4 would split 你 (3) then take 1
	// byte of 好, which is invalid, so we must stop at 3 bytes.
	if got != "你" {
		t.Fatalf("truncated = %q, want %q", got, "你")
	}
}

func TestTruncateResultRetainsHeadAndTail(t *testing.T) {
	res := truncateResult("HEAD-"+strings.Repeat("x", 100)+"-TAIL", "spill.txt", 160)
	if !strings.HasPrefix(res.Output, "HEAD-") || !strings.Contains(res.Output, "-TAIL") {
		t.Fatalf("head/tail preview lost an endpoint: %q", res.Output)
	}
	if len(res.Output) > 160 {
		t.Fatalf("preview exceeds cap: %d", len(res.Output))
	}
}
