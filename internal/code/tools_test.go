package code

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// eventRec is one event emitted through the CodeTools onEvent sink.
type eventRec struct {
	typ  string
	data any
}

// newCodeToolsWithEvents returns an engine and a CodeTools bundle wired to a
// slice that records every emitted code/* event (the composition root wires the
// same sink to the session log in cmd/pa, D3).
func newCodeToolsWithEvents(t *testing.T) (*engine, *CodeTools, *[]eventRec) {
	t.Helper()
	e := NewEngine(nil)
	t.Cleanup(func() { e.Close() })
	recs := &[]eventRec{}
	ct := NewCodeTools(e, func(typ string, data any) {
		*recs = append(*recs, eventRec{typ: typ, data: data})
	})
	// These seam tests exercise command execution itself. Full access is
	// explicit because the production default is workspace-write and must
	// fail closed when no enforcing backend is installed on the host.
	ct.DefaultMode = SandboxFullAccess
	return e, ct, recs
}

// decodeEvent unmarshals a captured event payload into T (the session payloads
// are plain JSON-serializable data).
func decodeEvent[T any](t *testing.T, ev eventRec) T {
	t.Helper()
	raw, err := json.Marshal(ev.data)
	if err != nil {
		t.Fatalf("marshal %s event data: %v", ev.typ, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s event data %s: %v", ev.typ, raw, err)
	}
	return out
}

// eventTypes returns the emitted event types in order.
func eventTypes(recs []eventRec) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.typ)
	}
	return out
}

// TestCodeRunToolSchema verifies the D7 shape the registry compiles and sends
// to the model (dispatch-m6e-2 §3): additionalProperties false, lang restricted
// to the ["sh"] enum, code required, timeout as a numeric-seconds number, and
// cwd optional.
func TestCodeRunToolSchema(t *testing.T) {
	_, ct, _ := newCodeToolsWithEvents(t)
	sch := ct.Run().Schema()
	if sch["type"] != "object" {
		t.Fatalf("schema type = %v, want object", sch["type"])
	}
	if sch["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v, want false", sch["additionalProperties"])
	}
	req, _ := sch["required"].([]string)
	if !containsStr(req, "lang") || !containsStr(req, "code") {
		t.Fatalf("required = %v, want lang and code", req)
	}
	props, _ := sch["properties"].(map[string]any)
	lang, _ := props["lang"].(map[string]any)
	enum, _ := lang["enum"].([]string)
	if len(enum) != 1 || enum[0] != "sh" {
		t.Fatalf("lang enum = %v, want [sh]", enum)
	}
	timeout, _ := props["timeout"].(map[string]any)
	if timeout["type"] != "number" {
		t.Fatalf("timeout type = %v, want number (numeric seconds)", timeout["type"])
	}
	if _, ok := props["cwd"]; !ok {
		t.Fatal("cwd property missing")
	}
}

// TestCodeRunCancellationClassification prevents the catalog from silently
// claiming cancellation unless the tool retains its explicit opt-in.
func TestCodeRunCancellationClassification(t *testing.T) {
	_, ct, _ := newCodeToolsWithEvents(t)
	tool := ct.Run()
	classified, ok := any(tool).(interface{ CancellationAware() bool })
	if !ok || !classified.CancellationAware() {
		t.Fatal("run_code must explicitly classify cooperative cancellation")
	}
}

// TestCodeRunToolExecutesAndEmits covers the happy path through the real local
// engine: a script that exits 0 and prints runs in the sandbox cwd, and
// code/run lands through the event sink with the outcome markers.
func TestCodeRunToolExecutesAndEmits(t *testing.T) {
	_, ct, recs := newCodeToolsWithEvents(t)
	out, err := ct.Run().Execute(context.Background(), json.RawMessage(
		`{"lang":"sh","code":"echo hello","cwd":`+jsonString(testCwd(t))+`}`))
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("run_code output = %q, want it to carry hello", out)
	}
	if strings.Contains(out, "[exit code:") {
		t.Fatalf("run_code output = %q, want no exit-code marker", out)
	}
	if got := eventTypes(*recs); len(got) != 1 || got[0] != "code/run" {
		t.Fatalf("emitted types = %v, want [code/run]", got)
	}
	d := decodeEvent[struct {
		Lang      string `json:"lang"`
		ExitCode  int    `json:"exitCode"`
		TimedOut  bool   `json:"timedOut"`
		Truncated bool   `json:"truncated"`
	}](t, (*recs)[0])
	if d.Lang != "sh" || d.ExitCode != 0 || d.TimedOut || d.Truncated {
		t.Fatalf("code/run payload = %+v, want lang sh / exit 0 / no markers", d)
	}
}

// TestCodeRunToolNonZeroExit verifies a non-zero exit is a normal outcome
// returned to the model (never an error, never a panic): the exit-code marker
// plus the stderr, and the code/run event carrying the exit code.
func TestCodeRunToolNonZeroExit(t *testing.T) {
	_, ct, recs := newCodeToolsWithEvents(t)
	out, err := ct.Run().Execute(context.Background(), json.RawMessage(
		`{"lang":"sh","code":`+jsonString(failCommand())+`,"cwd":`+jsonString(testCwd(t))+`}`))
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}
	if !strings.Contains(out, "[exit code: 3]") {
		t.Fatalf("run_code output = %q, want [exit code: 3]", out)
	}
	if !strings.Contains(out, "oops") {
		t.Fatalf("run_code output = %q, want it to carry stderr oops", out)
	}
	d := decodeEvent[struct {
		ExitCode int  `json:"exitCode"`
		TimedOut bool `json:"timedOut"`
	}](t, (*recs)[0])
	if d.ExitCode != 3 || d.TimedOut {
		t.Fatalf("code/run payload = %+v, want exit 3 / not timed out", d)
	}
}

// TestCodeRunToolTimeout verifies a sandbox timeout is a normal outcome
// returned to the model: the timeout marker in the output and the code/run
// event's TimedOut marker (dispatch-m6e-2 §3: 超时返回给模型，非 panic).
func TestCodeRunToolTimeout(t *testing.T) {
	_, ct, recs := newCodeToolsWithEvents(t)
	out, err := ct.Run().Execute(context.Background(), json.RawMessage(
		`{"lang":"sh","code":`+jsonString(longSleep())+`,"timeout":0.3,"cwd":`+jsonString(testCwd(t))+`}`))
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}
	if !strings.Contains(out, "[timed out") {
		t.Fatalf("run_code output = %q, want a timeout marker", out)
	}
	d := decodeEvent[struct {
		ExitCode int  `json:"exitCode"`
		TimedOut bool `json:"timedOut"`
	}](t, (*recs)[0])
	if !d.TimedOut {
		t.Fatalf("code/run payload = %+v, want timedOut", d)
	}
}

// TestCodeRunToolTruncated verifies the output-quota path: with a small
// DefaultMaxOutput the per-stream cap truncates, the truncation marker appears
// in the output, and the code/run event's Truncated marker is set.
func TestCodeRunToolTruncated(t *testing.T) {
	_, ct, recs := newCodeToolsWithEvents(t)
	ct.DefaultMaxOutput = 1024
	out, err := ct.Run().Execute(context.Background(), json.RawMessage(
		`{"lang":"sh","code":`+jsonString(bigOutput(70000))+`,"cwd":`+jsonString(testCwd(t))+`}`))
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}
	if !strings.Contains(out, "[output truncated") {
		t.Fatalf("run_code output = %q, want a truncation marker", out)
	}
	d := decodeEvent[struct {
		Truncated bool `json:"truncated"`
	}](t, (*recs)[0])
	if !d.Truncated {
		t.Fatalf("code/run payload = %+v, want truncated", d)
	}
}

// TestCodeRunToolDefaults verifies the config-derived knobs: when the model
// omits cwd, DefaultCwd is used as the sandbox working directory.
func TestCodeRunToolDefaults(t *testing.T) {
	_, ct, _ := newCodeToolsWithEvents(t)
	staticCwd := testCwd(t)
	ct.DefaultCwd = staticCwd
	dynamicCwd := testCwd(t)
	called := 0
	ct.DefaultCwdFunc = func() string {
		called++
		return dynamicCwd
	}
	out, err := ct.Run().Execute(context.Background(), json.RawMessage(`{"lang":"sh","code":`+jsonString(printCwdCommand())+`}`))
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}
	if !samePath(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "[stdout]")), dynamicCwd) {
		t.Fatalf("run_code cwd = %q, want %q (DefaultCwdFunc applied)", strings.TrimSpace(out), dynamicCwd)
	}
	if called != 1 {
		t.Fatalf("DefaultCwdFunc calls = %d, want 1", called)
	}
}

// TestCodeRunToolRejectsBadArgs verifies the tool's own argument checks (the
// registry enforces the same via D7): an unsupported lang and an empty code
// are errors, and no code/run event may be emitted on a failed run.
func TestCodeRunToolRejectsBadArgs(t *testing.T) {
	_, ct, recs := newCodeToolsWithEvents(t)
	if _, err := ct.Run().Execute(context.Background(), json.RawMessage(`{"lang":"python","code":"print(1)"}`)); err == nil {
		t.Fatal("unsupported lang must error")
	}
	if _, err := ct.Run().Execute(context.Background(), json.RawMessage(`{"lang":"sh","code":"   "}`)); err == nil {
		t.Fatal("empty code must error")
	}
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed run, got %v", eventTypes(*recs))
	}
}

// containsStr reports whether s is in list.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// jsonString returns s as a JSON string literal (for embedding paths/code in
// tool argument JSON).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
