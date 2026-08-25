// Black-box tests for the kb consumer tools (design.md §8 Consumer / D2/D9,
// dispatch-m4b §1): they are not registered by default (D10), once registered
// and whitelisted they execute against any KB provider, D7 rejects bad
// arguments at the Execute gate, kb_add→kb_search round-trips with source, and
// kb_read returns the full entry.
package kb_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/kb"
	"github.com/jabing/shutu-agent/internal/tools"
)

// TestKBToolsNotRegisteredByDefault proves the three kb tools are not
// available out of the box: with the default registry they are neither
// registered nor advertised, so Execute rejects them as unknown (dispatch-m4b:
// 默认关闭, D10 — mirrors TestRunCommandNotRegisteredByDefault).
func TestKBToolsNotRegisteredByDefault(t *testing.T) {
	r := tools.New() // default policy: read-only whitelist, no kb tools registered
	for _, name := range []string{"kb_search", "kb_read", "kb_add"} {
		if _, err := r.Execute(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%s must not execute by default", name)
		}
	}
	for _, spec := range r.Specs() {
		if strings.HasPrefix(spec.Name, "kb_") {
			t.Fatalf("kb tool %q must not be advertised to the model by default", spec.Name)
		}
	}
}

// TestKBToolsRejectedWhenNotWhitelisted proves the whitelist gate applies even
// to registered kb tools: registered but not whitelisted ⇒ refused (未启用 ⇒ 拒
// 绝执行, design.md §5).
func TestKBToolsRejectedWhenNotWhitelisted(t *testing.T) {
	k := kb.NewMemProvider()
	r := tools.New()
	if err := r.Register(kb.NewSearchTool(k, 5)); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(tools.Policy{Enabled: []string{"get_time"}}) // kb_search not whitelisted
	if _, err := r.Execute(context.Background(), "kb_search", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("kb_search must be refused when not whitelisted")
	}
}

// newKBToolRegistry registers the three kb tools against an in-memory provider
// and whitelists them.
func newKBToolRegistry(t *testing.T) (*tools.Registry, kb.KB) {
	t.Helper()
	k := kb.NewMemProvider()
	r := tools.New()
	for _, tool := range []tools.Tool{kb.NewSearchTool(k, 5), kb.NewReadTool(k), kb.NewAddTool(k, nil)} {
		if err := r.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	r.SetPolicy(tools.Policy{
		Enabled: []string{"kb_search", "kb_read", "kb_add"},
		Timeout: time.Hour,
	})
	return r, k
}

// TestKBAddThenSearchAndRead round-trips a manual write: kb_add → assigned id,
// kb_search retrieves it with a manual source, kb_read returns the full entry.
func TestKBAddThenSearchAndRead(t *testing.T) {
	r, _ := newKBToolRegistry(t)
	ctx := context.Background()

	res, err := r.Execute(ctx, "kb_add", json.RawMessage(`{"title":"最喜欢的编程语言","body":"markerx 偏好 Go 语言，用于个人项目","type":"preference","tags":["语言"]}`))
	if err != nil {
		t.Fatalf("kb_add: %v", err)
	}
	if !strings.Contains(res.Output, "added knowledge entry") {
		t.Fatalf("kb_add output = %q", res.Output)
	}

	// kb_search finds it and the result carries the manual source.
	searchRes, err := r.Execute(ctx, "kb_search", json.RawMessage(`{"query":"markerx 编程"}`))
	if err != nil {
		t.Fatalf("kb_search: %v", err)
	}
	if !strings.Contains(searchRes.Output, "最喜欢的编程语言") || !strings.Contains(searchRes.Output, "source=manual:") {
		t.Fatalf("kb_search output = %q, want title + manual source", searchRes.Output)
	}

	// The search result exposes the id; kb_read returns the full entry.
	id := searchEntryID(t, searchRes.Output)
	readRes, err := r.Execute(ctx, "kb_read", json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("kb_read: %v", err)
	}
	if !strings.Contains(readRes.Output, "# 最喜欢的编程语言") || !strings.Contains(readRes.Output, "偏好 Go 语言") {
		t.Fatalf("kb_read output = %q, want full entry", readRes.Output)
	}
	if !strings.Contains(readRes.Output, "type=preference") || !strings.Contains(readRes.Output, "version=1") {
		t.Fatalf("kb_read output = %q, want metadata", readRes.Output)
	}
}

// searchEntryID extracts the entry id from a formatted kb_search result.
func searchEntryID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "id: ") {
			return strings.TrimPrefix(line, "id: ")
		}
	}
	t.Fatalf("no id line in kb_search output: %q", output)
	return ""
}

// TestKBAddRejectsBadArgs proves D7: malformed model arguments are rejected at
// the Execute gate by the compiled JSON Schema, and the tool never runs.
func TestKBAddRejectsBadArgs(t *testing.T) {
	k := kb.NewMemProvider()
	r := tools.New()
	added := kb.NewAddTool(k, nil)
	if err := r.Register(added); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(tools.Policy{Enabled: []string{"kb_add"}, Timeout: time.Hour})

	cases := []struct {
		name string
		args string
	}{
		{"missing title", `{"body":"b","type":"fact"}`},
		{"empty body", `{"title":"t","body":"","type":"fact"}`},
		{"bad type", `{"title":"t","body":"b","type":"recipe"}`},
		{"unknown field", `{"title":"t","body":"b","type":"fact","scope":"x"}`},
		{"non-object", `["t","b"]`},
	}
	for _, c := range cases {
		if _, err := r.Execute(context.Background(), "kb_add", json.RawMessage(c.args)); err == nil {
			t.Errorf("%s: expected schema rejection, got nil", c.name)
		}
	}
	if st, _ := k.Stats(context.Background()); st.EntryCount != 0 {
		t.Errorf("bad args must not reach the provider; entry count = %d, want 0", st.EntryCount)
	}
}

// TestKBSearchSchemaValidation proves kb_search arguments are schema-checked
// too (missing query, out-of-range limit).
func TestKBSearchSchemaValidation(t *testing.T) {
	r, _ := newKBToolRegistry(t)
	if _, err := r.Execute(context.Background(), "kb_search", json.RawMessage(`{}`)); err == nil {
		t.Fatal("kb_search without query must be rejected")
	}
	if _, err := r.Execute(context.Background(), "kb_search", json.RawMessage(`{"query":"x","limit":0}`)); err == nil {
		t.Fatal("kb_search limit 0 must be rejected (minimum 1)")
	}
	if _, err := r.Execute(context.Background(), "kb_search", json.RawMessage(`{"query":"x","limit":1000}`)); err == nil {
		t.Fatal("kb_search limit 1000 must be rejected (maximum capped)")
	}
}

// TestKBReadNotFound proves kb_read on an unknown id fails (the model never
// mistakes a stale id for a live entry).
func TestKBReadNotFound(t *testing.T) {
	r, _ := newKBToolRegistry(t)
	if res, err := r.Execute(context.Background(), "kb_read", json.RawMessage(`{"id":"kb-nope"}`)); err != nil || !res.IsError {
		t.Fatalf("kb_read on an unknown id must return a structured error: result=%+v err=%v", res, err)
	}
}

// TestKBAddOnAddedFires proves the onAdded hook receives the assigned entry
// after a successful write — the wiring point for the kb/add session event
// (dispatch-m4b §3, D3).
func TestKBAddOnAddedFires(t *testing.T) {
	k := kb.NewMemProvider()
	r := tools.New()
	var got kb.Entry
	added := kb.NewAddTool(k, func(e kb.Entry) { got = e })
	if err := r.Register(added); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(tools.Policy{Enabled: []string{"kb_add"}, Timeout: time.Hour})
	if _, err := r.Execute(context.Background(), "kb_add", json.RawMessage(`{"title":"t","body":"b","type":"lesson"}`)); err != nil {
		t.Fatalf("kb_add: %v", err)
	}
	if got.ID == "" || got.Version != 1 {
		t.Fatalf("onAdded entry = %+v, want assigned id + version 1", got)
	}
	if !strings.HasPrefix(got.Source, "manual:") {
		t.Fatalf("onAdded source = %q, want manual: prefix", got.Source)
	}
}

// TestCatalogTextNonEmpty is a sanity check that the catalog section text is
// meaningful when injected (dispatch-m4b §2).
func TestCatalogTextNonEmpty(t *testing.T) {
	text := kb.CatalogText()
	if !strings.Contains(text, "kb_search") || !strings.Contains(text, "kb_read") || !strings.Contains(text, "kb_add") {
		t.Fatalf("catalog text should mention the tools: %q", text)
	}
}
