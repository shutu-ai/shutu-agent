package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// fakeTool records whether it was executed, so tests can prove schema
// validation happens before dispatch (D7).
type fakeTool struct {
	name     string
	schema   map[string]any
	executed bool
}

type argumentCaptureTool struct{ seen any }

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

type panickingExecuteTool struct{}
type leakingErrorTool struct{}
type postDecisionTool struct{}
type partialFailureTool struct{}
type cancellationFailureTool struct{ entered chan struct{} }

type renderedOutputTool struct{}

type finalizerTool struct{ calls int }

func (postDecisionTool) Name() string        { return "post_decision" }
func (postDecisionTool) Description() string { return "post decision test tool" }
func (postDecisionTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (postDecisionTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (postDecisionTool) Execute(context.Context, any) (string, error) { return "original", nil }
func (postDecisionTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{
		Value: "original", Output: "original",
		AdditionalContextMessages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("tool-context")}}},
	}, nil
}

func (renderedOutputTool) Name() string        { return "rendered_output" }
func (renderedOutputTool) Description() string { return "renderer contract test tool" }
func (renderedOutputTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (renderedOutputTool) OutputSchema() map[string]any                 { return map[string]any{"type": "object"} }
func (renderedOutputTool) Execute(context.Context, any) (string, error) { return "unused", nil }
func (renderedOutputTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{Value: map[string]any{"state": "body"}, ValueSet: true, Output: `{"state":"body"}`}, nil
}
func (renderedOutputTool) RenderOutput(_ any, value any) ([]llm.ContentBlock, error) {
	state, _ := value.(map[string]any)["state"].(string)
	return []llm.ContentBlock{llm.Text("rendered:" + state)}, nil
}
func (renderedOutputTool) PresentationMetadata(_ any, value any) any {
	state, _ := value.(map[string]any)["state"].(string)
	return map[string]any{"state": state}
}

func (t *finalizerTool) Name() string        { return "finalizer" }
func (t *finalizerTool) Description() string { return "finalizer contract test tool" }
func (*finalizerTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (*finalizerTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (*finalizerTool) Execute(context.Context, any) (string, error) {
	return "", errors.New("body failure")
}
func (t *finalizerTool) FinalizeOutput(_ any, result ToolResult) ([]llm.ContentBlock, error) {
	t.calls++
	if result.IsError {
		return []llm.ContentBlock{llm.Text("finalized failure")}, nil
	}
	return []llm.ContentBlock{llm.Text(result.Output)}, nil
}

func (panickingExecuteTool) Name() string        { return "panic_execute" }
func (panickingExecuteTool) Description() string { return "panic execution test tool" }
func (panickingExecuteTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (panickingExecuteTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (panickingExecuteTool) Execute(context.Context, any) (string, error) {
	panic("tool body failed")
}

func (leakingErrorTool) Name() string        { return "leaking_error" }
func (leakingErrorTool) Description() string { return "diagnostic redaction test tool" }
func (leakingErrorTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (leakingErrorTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (leakingErrorTool) Execute(context.Context, any) (string, error) {
	return "", fmt.Errorf("provider rejected authorization: Bearer super-secret")
}

type contextAwareTool struct{}

func (contextAwareTool) Name() string        { return "context_aware" }
func (contextAwareTool) Description() string { return "context propagation test tool" }
func (contextAwareTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (contextAwareTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (contextAwareTool) Execute(ctx context.Context, _ any) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		return "missing deadline", nil
	}
	return "deadline", nil
}

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
	return ToolResult{Output: "structured", Meta: map[string]any{"provider": "test"}, IsError: true}, nil
}

func (partialFailureTool) Name() string        { return "partial_failure" }
func (partialFailureTool) Description() string { return "partial structured failure test tool" }
func (partialFailureTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (partialFailureTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (partialFailureTool) Execute(context.Context, any) (string, error) { return "unused", nil }
func (partialFailureTool) ExecuteResult(context.Context, any) (ToolResult, error) {
	return ToolResult{
		Output:  "partial",
		Content: []llm.ContentBlock{llm.Text("partial diagnostic")},
		Meta:    map[string]any{"phase": "before-error"},
		AdditionalContextMessages: []llm.Message{{
			Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("deferred after failure")},
		}},
	}, errors.New("backend failed after producing context")
}

func (t cancellationFailureTool) Name() string { return "cancellation_failure" }
func (t cancellationFailureTool) Description() string {
	return "cancellation failure contract test tool"
}
func (t cancellationFailureTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t cancellationFailureTool) OutputSchema() map[string]any {
	return map[string]any{"type": "string"}
}
func (t cancellationFailureTool) Execute(ctx context.Context, _ any) (string, error) {
	close(t.entered)
	<-ctx.Done()
	return "", &ExecutionError{
		Info:    ErrorInfo{Name: "ToolFailure", Code: "TOOL_FAILURE"},
		Message: "tool failed",
	}
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
	if meta, ok := res.Meta.(map[string]any); !ok || meta["provider"] != "test" {
		t.Fatalf("structured result metadata = %#v", res.Meta)
	}
}

func TestExecuteErrorPreservesValidatedPartialRichResult(t *testing.T) {
	r := New()
	if err := r.Register(partialFailureTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"partial_failure"}})
	result, err := r.Execute(context.Background(), "partial_failure", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute partial failure: %v", err)
	}
	if !result.IsError || result.Error == nil || result.Error.Code != CodeToolExecutionError {
		t.Fatalf("partial failure classification = %+v", result)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "partial diagnostic" {
		t.Fatalf("partial failure content = %+v", result.Content)
	}
	if len(result.AdditionalContextMessages) != 1 || result.AdditionalContextMessages[0].Content[0].Text != "deferred after failure" {
		t.Fatalf("partial failure contexts = %+v", result.AdditionalContextMessages)
	}
	if meta, ok := result.Meta.(map[string]any); !ok || meta["phase"] != "before-error" {
		t.Fatalf("partial failure metadata = %#v", result.Meta)
	}
}

func TestOutputRendererProjectsCanonicalValueAndRefreshesReplacement(t *testing.T) {
	r := New()
	if err := r.Register(renderedOutputTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"rendered_output"}})
	result, err := r.Execute(context.Background(), "rendered_output", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute rendered tool: %v", err)
	}
	if result.Output != `{"state":"body"}` || len(result.Content) != 1 || result.Content[0].Text != "rendered:body" {
		t.Fatalf("rendered result = %+v", result)
	}
	meta, ok := result.Meta.(map[string]any)
	if !ok || meta["state"] != "body" {
		t.Fatalf("rendered metadata = %#v", result.Meta)
	}

	r.AddPostExecuteDecisionHook(func(context.Context, Execution, ToolResult) (PostExecuteDecision, error) {
		return PostExecuteDecision{Kind: "accept", Value: map[string]any{"state": "replacement"}, ValueSet: true}, nil
	})
	result, err = r.Execute(context.Background(), "rendered_output", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute replacement: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "rendered:replacement" {
		t.Fatalf("replacement content = %+v", result.Content)
	}
	meta, ok = result.Meta.(map[string]any)
	if !ok || meta["state"] != "replacement" {
		t.Fatalf("replacement metadata = %#v", result.Meta)
	}
}

func TestOutputFinalizerRunsOnceForNormalizedFailure(t *testing.T) {
	r := New()
	tool := &finalizerTool{}
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"finalizer"}})
	result, err := r.Execute(context.Background(), "finalizer", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute finalizer tool: %v", err)
	}
	if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "finalized failure" {
		t.Fatalf("finalized result = %+v", result)
	}
	if tool.calls != 1 {
		t.Fatalf("finalizer calls = %d, want 1", tool.calls)
	}
}

func TestToolDiagnosticsRedactCredentialShapedErrors(t *testing.T) {
	r := New()
	if err := r.Register(leakingErrorTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"leaking_error"}})
	result, err := r.Execute(context.Background(), "leaking_error", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute leaking tool: %v", err)
	}
	if strings.Contains(result.Output, "super-secret") || strings.Contains(result.Output, "Bearer super-secret") {
		t.Fatalf("tool result leaked credential: %q", result.Output)
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

func (argumentCaptureTool) Name() string        { return "argument_capture" }
func (argumentCaptureTool) Description() string { return "argument snapshot test tool" }
func (argumentCaptureTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (argumentCaptureTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (t *argumentCaptureTool) Execute(_ context.Context, args any) (string, error) {
	t.seen = args
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

func TestRegisterOwnedRemovesToolOnDispose(t *testing.T) {
	r := New()
	dispose, err := r.RegisterOwned(&fakeTool{name: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Specs()) != 1 {
		t.Fatalf("specs before dispose = %v", r.Specs())
	}
	if err := dispose(); err != nil {
		t.Fatal(err)
	}
	if len(r.Specs()) != 0 {
		t.Fatalf("specs after dispose = %v", r.Specs())
	}
	if err := dispose(); err != nil {
		t.Fatalf("second dispose: %v", err)
	}
}

func TestPreparedExecutionRejectsStaleRegistration(t *testing.T) {
	r := New()
	first := &fakeTool{name: "reloadable"}
	if err := r.RegisterWithInfo(first, RegistrationInfo{Owner: "plugin-a", Plugin: "demo", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"reloadable"}})
	prepared, err := r.Prepare(context.Background(), "call-1", "reloadable", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	info, ok := r.Registration("reloadable")
	if !ok || info.Plugin != "demo" || info.Generation == 0 {
		t.Fatalf("registration = %+v, ok=%v", info, ok)
	}
	if err := r.Unregister("reloadable"); err != nil {
		t.Fatal(err)
	}
	second := &fakeTool{name: "reloadable"}
	if err := r.RegisterWithInfo(second, RegistrationInfo{Owner: "plugin-b", Plugin: "demo", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecutePrepared(context.Background(), prepared); ErrorInfoOf(err).Code != "STALE_TOOL_GENERATION" {
		t.Fatalf("stale execution error = %v (%+v)", err, ErrorInfoOf(err))
	}
	if first.executed || second.executed {
		t.Fatal("stale prepared execution must not invoke either tool")
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	r := New()
	if _, err := r.Execute(context.Background(), "nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown tool should fail")
	}
}

func TestExecuteConvertsToolPanicToClassifiedResult(t *testing.T) {
	r := New()
	if err := r.Register(panickingExecuteTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"panic_execute"}})
	result, err := r.Execute(context.Background(), "panic_execute", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("panic should be a tool result, got error: %v", err)
	}
	if !result.IsError || result.Error == nil || result.Error.Code != "TOOL_PANIC" {
		t.Fatalf("panic result = %+v", result)
	}
}

func TestExecutePassesPerCallDeadlineToTool(t *testing.T) {
	r := New()
	if err := r.Register(contextAwareTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"context_aware"}, Timeout: time.Second})
	result, err := r.Execute(context.Background(), "context_aware", json.RawMessage(`{}`))
	if err != nil || result.Output != "deadline" {
		t.Fatalf("context result = %+v, err=%v", result, err)
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
	r.Register(&fakeTool{name: "disabled"})
	if _, err := r.Execute(context.Background(), "disabled", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != CodeToolDenied {
		t.Fatalf("disabled tool error = %+v, want %s", ErrorInfoOf(err), CodeToolDenied)
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Execute(ctx, "needs_name", json.RawMessage(`{"name":"x"}`)); ErrorInfoOf(err).Code != CodeAbortedBeforeDispatch {
		t.Fatalf("pre-dispatch cancellation error = %+v, want %s", ErrorInfoOf(err), CodeAbortedBeforeDispatch)
	}
}

func TestResultHookObservesPreparationFailuresExactlyOnce(t *testing.T) {
	r := New()
	tool := &fakeTool{name: "hook_failure_target", schema: map[string]any{
		"type": "object", "required": []string{"name"},
	}}
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	seen := make([]ToolResult, 0, 4)
	r.AddResultHook(func(_ Execution, result ToolResult) {
		seen = append(seen, result)
	})

	if _, err := r.Execute(context.Background(), "missing", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != CodeUnknownTool {
		t.Fatalf("unknown tool error = %+v", ErrorInfoOf(err))
	}
	r.SetPolicy(Policy{Enabled: nil})
	if _, err := r.Execute(context.Background(), "hook_failure_target", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != CodeToolDenied {
		t.Fatalf("denied tool error = %+v", ErrorInfoOf(err))
	}
	r.SetPolicy(Policy{Enabled: []string{"hook_failure_target"}})
	if _, err := r.Execute(context.Background(), "hook_failure_target", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != CodeInvalidArgs {
		t.Fatalf("invalid args error = %+v", ErrorInfoOf(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Execute(ctx, "hook_failure_target", json.RawMessage(`{"name":"cancelled"}`)); ErrorInfoOf(err).Code != CodeAbortedBeforeDispatch {
		t.Fatalf("pre-cancelled error = %+v", ErrorInfoOf(err))
	}
	if len(seen) != 4 {
		t.Fatalf("result hook calls = %d, want exactly 4", len(seen))
	}
	for index, result := range seen {
		if !result.IsError || len(result.Content) != 1 || result.Content[0].Kind != llm.BlockText {
			t.Fatalf("failure result %d = %+v, want canonical error content", index, result)
		}
	}
}

func TestPreCancelledCallSkipsPolicySchemaAndHooks(t *testing.T) {
	r := New()
	tool := &fakeTool{
		name: "pre_cancelled_pipeline",
		schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
	}
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Deliberately leave the tool disabled and pass invalid arguments. DSH
	// materializes first, then a pre-aborted call skips policy/schema/hooks.
	r.SetPolicy(Policy{Enabled: nil})
	hookCalls := 0
	r.AddPreExecuteHook(func(context.Context, Execution) (PreToolDecision, error) {
		hookCalls++
		return PreToolDecision{Kind: "deny", Reason: "should not observe cancelled call"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Execute(ctx, "pre_cancelled_pipeline", json.RawMessage(`{}`))
	if ErrorInfoOf(err).Code != CodeAbortedBeforeDispatch {
		t.Fatalf("pre-cancelled pipeline error = %+v, want %s", ErrorInfoOf(err), CodeAbortedBeforeDispatch)
	}
	if hookCalls != 0 {
		t.Fatalf("pre-hook calls = %d, want 0", hookCalls)
	}
	if tool.executed {
		t.Fatal("pre-cancelled call dispatched tool body")
	}
}

func TestPrepareSnapshotsArgumentsBeforeDispatch(t *testing.T) {
	r := New()
	tool := &argumentCaptureTool{}
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"argument_capture"}})

	args := map[string]any{"nested": map[string]any{"value": "before"}}
	prepared, err := r.Prepare(context.Background(), "call-snapshot", "argument_capture", args)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	args["nested"].(map[string]any)["value"] = "after"
	if _, err := r.ExecutePrepared(context.Background(), prepared); err != nil {
		t.Fatalf("execute prepared: %v", err)
	}
	seen, ok := tool.seen.(map[string]any)
	if !ok || seen["nested"].(map[string]any)["value"] != "before" {
		t.Fatalf("tool saw mutated arguments: %#v", tool.seen)
	}
}

func TestPreCancelledArgumentMaterializationFailureWins(t *testing.T) {
	r := New()
	if err := r.Register(&fakeTool{name: "materialization_failure"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"materialization_failure"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Execute(ctx, "materialization_failure", map[string]any{"bad": func() {}})
	if ErrorInfoOf(err).Code != CodeInvalidArgs {
		t.Fatalf("pre-cancelled materialization error = %+v, want %s", ErrorInfoOf(err), CodeInvalidArgs)
	}
}

func TestExecutePreparedRechecksCancellationBeforeDispatch(t *testing.T) {
	r := New()
	tool := &fakeTool{name: "prepared_cancel"}
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"prepared_cancel"}})

	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := r.Prepare(ctx, "call-prepared-cancel", "prepared_cancel", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cancel()
	if _, err := r.ExecutePrepared(ctx, prepared); ErrorInfoOf(err).Code != CodeAbortedBeforeDispatch {
		t.Fatalf("prepared cancellation error = %+v, want %s", ErrorInfoOf(err), CodeAbortedBeforeDispatch)
	}
	if tool.executed {
		t.Fatal("prepared call dispatched after cancellation")
	}
}

func TestLateCancellationPreservesContextsAndClassifiesBodyStart(t *testing.T) {
	r := New()
	if err := r.Register(postDecisionTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"post_decision"}})
	entered := make(chan struct{})
	release := make(chan struct{})
	r.AddExecuteHook(func(ctx context.Context, execution Execution, next func(context.Context) (ToolResult, error)) (ToolResult, error) {
		result, err := next(ctx)
		close(entered)
		<-release
		return result, err
	})

	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := r.Prepare(ctx, "call-late-cancel", "post_decision", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	done := make(chan ToolResult, 1)
	go func() {
		result, err := r.ExecutePrepared(ctx, prepared)
		if err != nil {
			t.Errorf("execute prepared: %v", err)
		}
		done <- result
	}()
	<-entered
	cancel()
	close(release)
	result := <-done
	if !result.IsError || result.Error == nil || result.Error.Code != CodeAborted {
		t.Fatalf("late cancellation result = %+v, want %s", result, CodeAborted)
	}
	if len(result.AdditionalContextMessages) != 1 || result.AdditionalContextMessages[0].Text() != "tool-context" {
		t.Fatalf("late cancellation contexts = %+v, want deferred tool context", result.AdditionalContextMessages)
	}
}

func TestWrapperShortCircuitCancellationIsBeforeDispatch(t *testing.T) {
	r := New()
	tool := &fakeTool{name: "short_circuit_cancel"}
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"short_circuit_cancel"}})
	entered := make(chan struct{})
	release := make(chan struct{})
	r.AddExecuteHook(func(context.Context, Execution, func(context.Context) (ToolResult, error)) (ToolResult, error) {
		close(entered)
		<-release
		return ToolResult{Output: "wrapper success"}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := r.Prepare(ctx, "call-short-circuit", "short_circuit_cancel", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	done := make(chan ToolResult, 1)
	go func() {
		result, err := r.ExecutePrepared(ctx, prepared)
		if err != nil {
			t.Errorf("execute prepared: %v", err)
		}
		done <- result
	}()
	<-entered
	cancel()
	close(release)
	result := <-done
	if !result.IsError || result.Error == nil || result.Error.Code != CodeAbortedBeforeDispatch {
		t.Fatalf("short-circuit cancellation result = %+v, want %s", result, CodeAbortedBeforeDispatch)
	}
	if tool.executed {
		t.Fatal("wrapper short-circuit still dispatched tool body")
	}
}

func TestCancellationDoesNotEraseWrapperFailure(t *testing.T) {
	r := New()
	tool := &fakeTool{name: "wrapper_failure_cancel"}
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"wrapper_failure_cancel"}})
	entered := make(chan struct{})
	release := make(chan struct{})
	r.AddExecuteHook(func(context.Context, Execution, func(context.Context) (ToolResult, error)) (ToolResult, error) {
		close(entered)
		<-release
		return ToolResult{}, errors.New("wrapper failed")
	})

	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := r.Prepare(ctx, "call-wrapper-failure", "wrapper_failure_cancel", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	done := make(chan ToolResult, 1)
	go func() {
		result, err := r.ExecutePrepared(ctx, prepared)
		if err != nil {
			t.Errorf("execute prepared: %v", err)
		}
		done <- result
	}()
	<-entered
	cancel()
	close(release)
	result := <-done
	if !result.IsError || result.Error == nil || result.Error.Code != CodeToolExecutionError || result.Output != "Error: wrapper failed" {
		t.Fatalf("wrapper failure after cancellation = %+v, want specific execution failure", result)
	}
	if tool.executed {
		t.Fatal("wrapper failure path dispatched tool body")
	}
}

func TestCancellationPreservesToolOwnedFailure(t *testing.T) {
	entered := make(chan struct{})
	r := New()
	if err := r.Register(cancellationFailureTool{entered: entered}); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetPolicy(Policy{Enabled: []string{"cancellation_failure"}})
	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := r.Prepare(ctx, "call-tool-failure", "cancellation_failure", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	done := make(chan ToolResult, 1)
	go func() {
		result, err := r.ExecutePrepared(ctx, prepared)
		if err != nil {
			t.Errorf("execute prepared: %v", err)
		}
		done <- result
	}()
	<-entered
	cancel()
	result := <-done
	if !result.IsError || result.Error == nil || result.Error.Code != "TOOL_FAILURE" || result.Output != "Error: tool failed" {
		t.Fatalf("tool-owned failure after cancellation = %+v, want specific tool failure", result)
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

func TestCatalogUnifiesVisibilitySchemasAndOwnership(t *testing.T) {
	r := New()
	if err := r.RegisterWithInfo(&fakeTool{name: "zebra"}, RegistrationInfo{Owner: "owner-z", Plugin: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithInfo(&fakeTool{name: "alpha"}, RegistrationInfo{Owner: "owner-a", Plugin: "demo"}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"alpha"}})

	catalog := r.Catalog()
	if len(catalog) != 2 || catalog[0].Name != "alpha" || catalog[1].Name != "zebra" {
		t.Fatalf("catalog = %+v, want stable name order", catalog)
	}
	if !catalog[0].Visible || catalog[1].Visible {
		t.Fatalf("catalog visibility = %+v, want only alpha visible", catalog)
	}
	if catalog[0].Registration.Owner != "owner-a" || catalog[0].Registration.Generation == 0 {
		t.Fatalf("catalog registration = %+v, want generated ownership", catalog[0].Registration)
	}
	if catalog[0].Provenance != "plugin" {
		t.Fatalf("catalog provenance = %q, want plugin", catalog[0].Provenance)
	}
	if catalog[0].Parameters == nil || catalog[0].OutputSchema == nil {
		t.Fatalf("catalog schemas = %+v, want input and output schemas", catalog[0])
	}
	visible := r.VisibleSpecs()
	if len(visible) != 1 || visible[0].Name != "alpha" {
		t.Fatalf("visible specs = %+v, want catalog-visible alpha", visible)
	}

	// Catalog schemas are detached snapshots; callers cannot mutate the
	// registry's generated definition by editing a transport response.
	catalog[0].Parameters["type"] = "array"
	if got := r.Catalog()[0].Parameters["type"]; got != "object" {
		t.Fatalf("registry schema mutated through catalog snapshot: %v", got)
	}
}

func TestCatalogJSONIsStableMachineReadableSnapshot(t *testing.T) {
	r := New()
	if err := r.RegisterWithInfo(&fakeTool{name: "zebra"}, RegistrationInfo{Owner: "owner-z"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithInfo(&fakeTool{name: "alpha"}, RegistrationInfo{Owner: "owner-a"}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"alpha"}})
	first, err := r.CatalogJSON()
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.CatalogJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("catalog JSON is not deterministic:\n%s\n---\n%s", first, second)
	}
	var snapshot CatalogSnapshot
	if err := json.Unmarshal(first, &snapshot); err != nil {
		t.Fatalf("decode catalog JSON: %v", err)
	}
	if snapshot.SchemaVersion != 1 || len(snapshot.Tools) != 2 || snapshot.Tools[0].Name != "alpha" || !snapshot.Tools[0].Visible || snapshot.Tools[1].Visible {
		t.Fatalf("catalog snapshot = %+v", snapshot)
	}
}

func TestCatalogManifestIsVersionedAndTamperEvident(t *testing.T) {
	r := New()
	if err := r.RegisterWithInfo(&fakeTool{name: "zebra"}, RegistrationInfo{Owner: "owner-z", Plugin: "demo", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithInfo(&fakeTool{name: "alpha"}, RegistrationInfo{Owner: "owner-a", Plugin: "demo", Generation: 9}); err != nil {
		t.Fatal(err)
	}
	manifest, err := r.CatalogManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.Revision != 9 || len(manifest.Tools) != 2 || manifest.Digest == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	repeat, err := r.CatalogManifest()
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Digest != manifest.Digest || repeat.Revision != manifest.Revision {
		t.Fatalf("manifest is not deterministic: %+v vs %+v", repeat, manifest)
	}
	if err := ValidateCatalogManifest(manifest); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	tampered := manifest
	tampered.Tools[0].Description = "changed"
	if err := ValidateCatalogManifest(tampered); err == nil {
		t.Fatal("tampered tool payload accepted")
	}
	badRevision := manifest
	badRevision.Revision++
	if err := ValidateCatalogManifest(badRevision); err == nil {
		t.Fatal("revision drift accepted")
	}
	badDigest := manifest
	badDigest.Digest = strings.Repeat("0", 64)
	if err := ValidateCatalogManifest(badDigest); err == nil {
		t.Fatal("digest drift accepted")
	}

	// Replacing a definition advances registration generation, and the next
	// release manifest makes that reload observable without comparing tools.
	if err := r.Unregister("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithInfo(&fakeTool{name: "alpha"}, RegistrationInfo{Owner: "owner-a2", Plugin: "demo", Generation: 11}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := r.CatalogManifest()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision != 11 || reloaded.Digest == manifest.Digest {
		t.Fatalf("reload manifest = %+v, want advanced revision and digest", reloaded)
	}
}

func TestCatalogCarriesGeneratedExecutionContract(t *testing.T) {
	r := New()
	if err := r.RegisterWithInfo(&cancellableTool{fakeTool: &fakeTool{name: "cancellable"}}, RegistrationInfo{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Profile: "standard", Enabled: []string{"cancellable"}, Timeout: 1500 * time.Millisecond})

	entry := r.Catalog()[0]
	if entry.Provenance != "builtin" || entry.Registration.Owner != "builtin" || entry.Registration.Plugin != "builtin" {
		t.Fatalf("catalog ownership/provenance = %+v/%q, want builtin", entry.Registration, entry.Provenance)
	}
	if entry.Profile != "standard" || entry.TimeoutMS != 1500 || !entry.Cancellable {
		t.Fatalf("catalog runtime contract = %+v", entry)
	}
	if entry.Policy.Execution != "allowed" || entry.Policy.Concurrency != "exclusive" {
		t.Fatalf("catalog policy = %+v", entry.Policy)
	}
	if len(entry.ErrorCodes) == 0 || len(entry.Events) != 2 {
		t.Fatalf("catalog contract = errors:%v events:%v", entry.ErrorCodes, entry.Events)
	}

	valid := r.VisibleSpecs()
	if err := r.ValidateProjection("standard", valid); err != nil {
		t.Fatalf("canonical projection rejected: %v", err)
	}
	alien := append(valid, llm.ToolSchema{Name: "not-registered"})
	if err := r.ValidateProjection("standard", alien); err == nil {
		t.Fatal("projection outside canonical catalog was accepted")
	}
	if err := r.ValidateProjection("minimal", valid); err == nil {
		t.Fatal("wrong-profile projection was accepted")
	}
}

type cancellableTool struct {
	*fakeTool
}

func (cancellableTool) CancellationAware() bool { return true }

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

func TestPreExecuteHookPanicIsClassified(t *testing.T) {
	ft := &fakeTool{name: "pre_hook_panic"}
	r := New()
	if err := r.Register(ft); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"pre_hook_panic"}})
	r.AddPreExecuteHook(func(context.Context, Execution) (PreToolDecision, error) {
		panic("approval extension failed")
	})

	if _, err := r.Execute(context.Background(), "pre_hook_panic", json.RawMessage(`{}`)); ErrorInfoOf(err).Code != CodeToolPanic {
		t.Fatalf("pre-hook panic error = %v (%+v), want TOOL_PANIC", err, ErrorInfoOf(err))
	}
	if ft.executed {
		t.Fatal("tool must not execute after a pre-hook panic")
	}
}

func TestPostExecuteHookPanicBecomesToolResult(t *testing.T) {
	r := New()
	if err := r.Register(&fakeTool{name: "post_hook_panic"}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"post_hook_panic"}})
	r.AddPostExecuteHook(func(context.Context, Execution, ToolResult) (ToolResult, error) {
		panic("post observer failed")
	})

	result, err := r.Execute(context.Background(), "post_hook_panic", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("post-hook panic escaped as error: %v", err)
	}
	if !result.IsError || result.Error == nil || result.Error.Code != CodeToolPanic {
		t.Fatalf("post-hook panic result = %+v, want TOOL_PANIC error result", result)
	}
}

func TestPostExecuteAroundHooksComposeInRegistrationOrder(t *testing.T) {
	r := New()
	if err := r.Register(&fakeTool{name: "post_around"}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"post_around"}})
	var order []string
	r.AddPostExecuteAroundHook(func(ctx context.Context, exec Execution, result ToolResult, next func(context.Context, Execution, ToolResult) (ToolResult, error)) (ToolResult, error) {
		order = append(order, "outer-before")
		result.Output += ":one"
		result, err := next(ctx, exec, result)
		order = append(order, "outer-after")
		return result, err
	})
	r.AddPostExecuteAroundHook(func(ctx context.Context, exec Execution, result ToolResult, next func(context.Context, Execution, ToolResult) (ToolResult, error)) (ToolResult, error) {
		order = append(order, "inner-before")
		result.Output += ":two"
		result, err := next(ctx, exec, result)
		order = append(order, "inner-after")
		return result, err
	})
	result, err := r.Execute(context.Background(), "post_around", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("around execution: %v", err)
	}
	if result.Output != "ok:one:two" {
		t.Fatalf("around result = %q, want ordered mutations", result.Output)
	}
	if got, want := strings.Join(order, ","), "outer-before,inner-before,inner-after,outer-after"; got != want {
		t.Fatalf("around order = %q, want %q", got, want)
	}
}

func TestPostExecuteDecisionAcceptAndBlockContract(t *testing.T) {
	r := New()
	if err := r.Register(postDecisionTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"post_decision"}})
	r.AddPostExecuteDecisionHook(func(context.Context, Execution, ToolResult) (PostExecuteDecision, error) {
		return PostExecuteDecision{
			Kind: "accept", Value: "replacement",
			AdditionalContexts: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("policy-context")}}},
		}, nil
	})
	accepted, err := r.Execute(context.Background(), "post_decision", json.RawMessage(`{}`))
	if err != nil || accepted.IsError || accepted.Value != "replacement" || accepted.Output != "replacement" {
		t.Fatalf("accepted decision = %+v err=%v", accepted, err)
	}
	if len(accepted.AdditionalContextMessages) != 2 || accepted.AdditionalContextMessages[0].Content[0].Text != "tool-context" || accepted.AdditionalContextMessages[1].Content[0].Text != "policy-context" {
		t.Fatalf("accepted contexts = %+v, want tool context before policy context", accepted.AdditionalContextMessages)
	}

	blockedRegistry := New()
	if err := blockedRegistry.Register(postDecisionTool{}); err != nil {
		t.Fatal(err)
	}
	blockedRegistry.SetPolicy(Policy{Enabled: []string{"post_decision"}})
	blockedRegistry.AddPostExecuteDecisionHook(func(context.Context, Execution, ToolResult) (PostExecuteDecision, error) {
		return PostExecuteDecision{
			Kind: "block", Feedback: []llm.ContentBlock{llm.Text("correct the unsafe output")},
			AdditionalContexts: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("block-context")}}},
		}, nil
	})
	blocked, err := blockedRegistry.Execute(context.Background(), "post_decision", json.RawMessage(`{}`))
	if err != nil || !blocked.IsError || blocked.Error == nil || blocked.Error.Code != CodeToolDenied || blocked.Output != "correct the unsafe output" {
		t.Fatalf("blocked decision = %+v err=%v", blocked, err)
	}
	if len(blocked.AdditionalContextMessages) != 1 || blocked.AdditionalContextMessages[0].Content[0].Text != "block-context" {
		t.Fatalf("blocked contexts = %+v, want decision-only context", blocked.AdditionalContextMessages)
	}
}

func TestPostExecuteDecisionRejectsAmbiguousOrFailedReplacement(t *testing.T) {
	for name, decision := range map[string]PostExecuteDecision{
		"both-value-and-content": {Kind: "accept", Value: "value", Content: []llm.ContentBlock{llm.Text("content")}},
	} {
		t.Run(name, func(t *testing.T) {
			r := New()
			if err := r.Register(postDecisionTool{}); err != nil {
				t.Fatal(err)
			}
			r.SetPolicy(Policy{Enabled: []string{"post_decision"}})
			r.AddPostExecuteDecisionHook(func(context.Context, Execution, ToolResult) (PostExecuteDecision, error) { return decision, nil })
			result, err := r.Execute(context.Background(), "post_decision", json.RawMessage(`{}`))
			if err != nil || !result.IsError || result.Error == nil || result.Error.Code != CodeInvalidToolOutput {
				t.Fatalf("ambiguous decision = %+v err=%v", result, err)
			}
		})
	}

	r := New()
	if err := r.Register(structuredFakeTool{}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"structured"}})
	r.AddPostExecuteDecisionHook(func(context.Context, Execution, ToolResult) (PostExecuteDecision, error) {
		return PostExecuteDecision{Kind: "accept", Value: "must-not-replace-failure"}, nil
	})
	result, err := r.Execute(context.Background(), "structured", json.RawMessage(`{}`))
	if err != nil || !result.IsError || result.Value != nil {
		t.Fatalf("failed result replacement = %+v err=%v", result, err)
	}
}

func TestResultHookPanicDoesNotChangeToolResult(t *testing.T) {
	r := New()
	if err := r.Register(&fakeTool{name: "result_hook_panic"}); err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(Policy{Enabled: []string{"result_hook_panic"}})
	r.AddResultHook(func(Execution, ToolResult) {
		panic("telemetry observer failed")
	})

	result, err := r.Execute(context.Background(), "result_hook_panic", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("result-hook panic escaped as error: %v", err)
	}
	if result.IsError || result.Output != "ok" {
		t.Fatalf("result-hook panic changed result = %+v", result)
	}
}
