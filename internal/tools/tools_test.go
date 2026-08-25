package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeTool records whether it was executed, so tests can prove schema
// validation happens before dispatch (D7).
type fakeTool struct {
	name     string
	schema   map[string]any
	executed bool
}

type structuredFakeTool struct{}

type parsedClassifierTool struct{}

func (parsedClassifierTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }

func (parsedClassifierTool) Name() string        { return "parsed_classifier" }
func (parsedClassifierTool) Description() string { return "classifier test tool" }
func (parsedClassifierTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (parsedClassifierTool) Execute(context.Context, any) (string, error) {
	return "ok", nil
}
func (parsedClassifierTool) ConcurrencySafe(args any) bool {
	values, ok := args.(map[string]any)
	return ok && values["parallel"] == true
}

type panickingClassifierTool struct{}

func (panickingClassifierTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }

func (panickingClassifierTool) Name() string        { return "panic_classifier" }
func (panickingClassifierTool) Description() string { return "panic classifier test tool" }
func (panickingClassifierTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (panickingClassifierTool) Execute(context.Context, any) (string, error) {
	return "ok", nil
}
func (panickingClassifierTool) ConcurrencySafe(any) bool { panic("classifier failed") }

func (structuredFakeTool) Name() string        { return "structured" }
func (structuredFakeTool) Description() string { return "structured fake tool" }
func (structuredFakeTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (structuredFakeTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (structuredFakeTool) Execute(context.Context, any) (string, error) { return "unused", nil }
func (structuredFakeTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{Output: "structured", IsError: true}, nil
}

func TestExecuteUsesStructuredToolResult(t *testing.T) {
	r := New()
	if err := r.Register(structuredFakeTool{}); err != nil {
		t.Fatalf("register structured tool: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"structured"}})
	res, err := r.Execute(context.Background(), "structured", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute structured tool: %v", err)
	}
	if res.Output != "structured" || !res.IsError {
		t.Fatalf("structured result = %+v", res)
	}
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake tool" }
func (f *fakeTool) Schema() map[string]any {
	if f.schema != nil {
		return f.schema
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (f *fakeTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (f *fakeTool) Execute(ctx context.Context, args any) (string, error) {
	f.executed = true
	return "ok", nil
}

func TestRegisterDuplicate(t *testing.T) {
	r := New()
	if err := r.Register(&fakeTool{name: "x"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(&fakeTool{name: "x"}); err == nil {
		t.Fatal("duplicate register should fail")
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	r := New()
	if _, err := r.Execute(context.Background(), "nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown tool should fail")
	}
}

func TestExecuteValidatesArgumentsBeforeDispatch(t *testing.T) {
	ft := &fakeTool{
		name: "needs_path",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
	r := New()
	r.Register(ft)
	// M3 whitelist gate: the fake must be whitelisted before it can run.
	r.SetPolicy(Policy{Enabled: []string{"needs_path"}})

	// Missing required field: must be rejected, tool must not run.
	if _, err := r.Execute(context.Background(), "needs_path", json.RawMessage(`{}`)); err == nil {
		t.Fatal("invalid args should be rejected")
	}
	if ft.executed {
		t.Fatal("tool executed despite invalid arguments (D7 violated)")
	}

	// Valid arguments: tool runs.
	res, err := r.Execute(context.Background(), "needs_path", json.RawMessage(`{"path":"/a"}`))
	if err != nil {
		t.Fatalf("valid args: %v", err)
	}
	if res.Output != "ok" || !ft.executed {
		t.Fatalf("out=%q executed=%v", res.Output, ft.executed)
	}
}

func TestExecuteMalformedJSON(t *testing.T) {
	r := New()
	r.Register(&fakeTool{name: "x"})
	r.SetPolicy(Policy{Enabled: []string{"x"}})
	if _, err := r.Execute(context.Background(), "x", json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed JSON should be rejected")
	}
}

func TestConcurrencyClassifierReceivesParsedArguments(t *testing.T) {
	r := New()
	if err := r.Register(parsedClassifierTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !r.IsConcurrencySafe("parsed_classifier", map[string]any{"parallel": true}) {
		t.Fatal("parsed classifier should allow the call")
	}
	if r.IsConcurrencySafe("parsed_classifier", map[string]any{"parallel": false}) {
		t.Fatal("parsed classifier should reject the call")
	}
}

func TestConcurrencyClassifierPanicsFailClosed(t *testing.T) {
	r := New()
	if err := r.Register(panickingClassifierTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if r.IsConcurrencySafe("panic_classifier", map[string]any{}) {
		t.Fatal("classifier panic must fail closed to exclusive execution")
	}
}

func TestExecutionErrorsExposeDshCodes(t *testing.T) {
	r := New()
	if _, err := r.Execute(context.Background(), "missing", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != "UNKNOWN_TOOL" {
		t.Fatalf("unknown tool error = %+v", ErrorInfoOf(err))
	}
	tool := &fakeTool{name: "needs_name", schema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []string{"name"},
	}}
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"needs_name"}})
	if _, err := r.Execute(context.Background(), "needs_name", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != "INVALID_ARGS" {
		t.Fatalf("invalid args error = %+v", ErrorInfoOf(err))
	}
}

func TestSpecsSorted(t *testing.T) {
	r := New()
	r.Register(&fakeTool{name: "zebra"})
	r.Register(&fakeTool{name: "alpha"})
	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("specs len = %d, want 2", len(specs))
	}
	if specs[0].Name != "alpha" || specs[1].Name != "zebra" {
		t.Fatalf("specs not sorted: %+v", specs)
	}
}

func TestGetTime(t *testing.T) {
	r := New()
	r.Register(GetTime{})
	res, err := r.Execute(context.Background(), "get_time", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_time: %v", err)
	}
	if res.Output == "" {
		t.Fatal("get_time returned empty")
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello agent"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	r := New()
	r.Register(ReadFile{})
	args, _ := json.Marshal(map[string]string{"file_path": path})
	res, err := r.Execute(context.Background(), "read", args)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Output != "1\thello agent" {
		t.Fatalf("read out = %q, want numbered output", res.Output)
	}
}

func TestReadFileWindowAndRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New()
	r.Register(NewReadFile(root))
	res, err := r.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"note.txt","offset":2,"limit":1}`))
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if res.Output != "2\ttwo" {
		t.Fatalf("read window = %q", res.Output)
	}
	if res, err := r.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"../outside.txt"}`)); err != nil || !res.IsError {
		t.Fatalf("read must reject a path outside the workspace root: result=%+v err=%v", res, err)
	}
}

func TestReadFileMissing(t *testing.T) {
	r := New()
	r.Register(ReadFile{})
	if res, err := r.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"/definitely/not/here"}`)); err != nil || !res.IsError {
		t.Fatalf("missing file should fail: result=%+v err=%v", res, err)
	}
}

// TestExecuteGate verifies the M6d-2 pre-execution gate hook
// (dispatch-m6d-2 §4): when installed, Execute runs the gate after D7
// validation and before the tool — a non-nil gate return is the verdict
// (denial/failure) and the tool never runs, a nil return lets the tool run.
// With no gate installed the behavior is unchanged.
func TestExecuteGate(t *testing.T) {
	ft := &fakeTool{name: "gated"}
	r := New()
	r.Register(ft)
	r.SetPolicy(Policy{Enabled: []string{"gated"}})

	var gateCalls []string
	denied := true
	r.AddPreExecuteHook(func(ctx context.Context, exec Execution) (PreToolDecision, error) {
		gateCalls = append(gateCalls, exec.Name)
		if denied && exec.Name == "gated" {
			return PreToolDecision{}, fmt.Errorf("denied: user said no")
		}
		return PreToolDecision{Kind: "allow"}, nil
	})

	if _, err := r.Execute(context.Background(), "gated", json.RawMessage(`{}`)); err == nil {
		t.Fatal("gated tool must be denied when the gate returns an error")
	} else if err.Error() != "denied: user said no" {
		t.Fatalf("gate verdict = %v, want the gate error verbatim", err)
	}
	if ft.executed {
		t.Fatal("gated tool executed despite the gate denying it")
	}
	if len(gateCalls) != 1 || gateCalls[0] != "gated" {
		t.Fatalf("gate calls = %v, want [gated]", gateCalls)
	}

	// The same installed pre-hook now allows the next execution.
	denied = false
	res, err := r.Execute(context.Background(), "gated", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("gated tool after nil gate return: %v", err)
	}
	if res.Output != "ok" || !ft.executed {
		t.Fatalf("out=%q executed=%v, want ok/true", res.Output, ft.executed)
	}

	// No gate installed: execution is unchanged.
	r2 := New()
	ft2 := &fakeTool{name: "x"}
	r2.Register(ft2)
	r2.SetPolicy(Policy{Enabled: []string{"x"}})
	if _, err := r2.Execute(context.Background(), "x", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("ungated registry: %v", err)
	}
	if !ft2.executed {
		t.Fatal("tool must run when no gate is installed")
	}
}

// TestExecuteGateRunsAfterValidation verifies the gate sits after D7: an
// invalid-arguments call is rejected by the schema before the gate ever sees it
// (so a malformed request never triggers a human approval prompt).
func TestExecuteGateRunsAfterValidation(t *testing.T) {
	ft := &fakeTool{
		name: "needs_path",
		schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"path": map[string]any{"type": "string"}},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
	r := New()
	r.Register(ft)
	r.SetPolicy(Policy{Enabled: []string{"needs_path"}})
	called := false
	r.AddPreExecuteHook(func(ctx context.Context, exec Execution) (PreToolDecision, error) {
		called = true
		return PreToolDecision{Kind: "allow"}, nil
	})
	if _, err := r.Execute(context.Background(), "needs_path", json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing required arg must be rejected (D7)")
	}
	if called {
		t.Fatal("gate ran before D7 validation; a malformed call must not prompt for approval")
	}
}
