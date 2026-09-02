package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/code"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

type richCodeTool struct{}
type richFailCodeTool struct{}

func (richFailCodeTool) Name() string        { return "rich_fail_code_tool" }
func (richFailCodeTool) Description() string { return "returns a failed rich result" }
func (richFailCodeTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (richFailCodeTool) OutputSchema() map[string]any                 { return map[string]any{"type": "string"} }
func (richFailCodeTool) Execute(context.Context, any) (string, error) { return "failed", nil }
func (richFailCodeTool) ExecuteResult(context.Context, any) (tools.ToolResult, error) {
	return tools.ToolResult{
		Output:                    "blocked",
		Content:                   []llm.ContentBlock{llm.Text("blocked by policy")},
		AdditionalContextMessages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("failure context")}}},
		IsError:                   true,
		Error:                     &tools.ErrorInfo{Name: "PolicyError", Code: tools.CodeToolDenied},
	}, nil
}

func (richCodeTool) Name() string        { return "rich_code_tool" }
func (richCodeTool) Description() string { return "returns ordered rich content" }
func (richCodeTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (richCodeTool) OutputSchema() map[string]any                 { return map[string]any{"type": "object"} }
func (richCodeTool) Execute(context.Context, any) (string, error) { return "plain", nil }
func (richCodeTool) ExecuteResult(context.Context, any) (tools.ToolResult, error) {
	return tools.ToolResult{
		Value:  map[string]any{"answer": "value"},
		Output: "plain",
		Content: []llm.ContentBlock{
			llm.Text("before"),
			{Kind: llm.BlockImage, Image: llm.ImageRef{ID: "attachment-1", MediaType: "image/png", Bytes: 3}},
			llm.Text("after"),
		},
		AdditionalContextMessages: []llm.Message{{
			Role: llm.RoleUser, SourceKind: "plugin", SourcePlugin: "rich-code-test",
			Content: []llm.ContentBlock{llm.Text("nested deferred context")},
		}},
	}, nil
}

// makeCodeApp builds a minimal app for code wiring tests: only the fields
// registerCode touches (cfg.Code, reg, log) are set.
func makeCodeApp(codeEnabled bool) *app {
	return &app{
		cfg: config.Config{
			Code: config.CodeConfig{Enabled: config.Bool(codeEnabled)},
		},
		reg:        tools.New(),
		log:        session.New(),
		basePolicy: tools.Policy{Enabled: []string{"get_time", "run_code"}},
	}
}

// codePolicy whitelists run_code so the registry Execute gate can run it (in
// production config.applyDefaults + PolicyFromConfig do this).
func codePolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"run_code"},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// TestRegisterCodeDisabledRegistersNothing verifies the D10 gate: with
// code.enabled=false the composition root creates no Engine and registers no
// run_code tool (dispatch-m6e-2 §4).
func TestRegisterCodeDisabledRegistersNothing(t *testing.T) {
	a := makeCodeApp(false)
	if err := a.registerCode(); err != nil {
		t.Fatalf("registerCode: %v", err)
	}
	if a.code != nil {
		t.Fatal("code engine must be nil when code.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		if spec.Name == "run_code" {
			t.Fatalf("run_code registered while code disabled")
		}
	}
}

func TestRegisterCodeUnavailableDowngradesWithoutStoppingAgent(t *testing.T) {
	oldProbe := probeTypeScriptRuntimeStatus
	probeTypeScriptRuntimeStatus = func() (bool, string) {
		return false, "permission model unavailable"
	}
	t.Cleanup(func() { probeTypeScriptRuntimeStatus = oldProbe })

	a := makeCodeApp(true)
	a.reg.SetPolicy(codePolicy())
	if err := a.registerCode(); err != nil {
		t.Fatalf("unavailable optional code capability must not stop startup: %v", err)
	}
	if a.code != nil {
		t.Fatal("unverified code runtime must not be constructed")
	}
	if a.codeUnavailableReason != "permission model unavailable" {
		t.Fatalf("unavailable reason = %q", a.codeUnavailableReason)
	}
	if containsString(a.basePolicy.Enabled, "run_code") {
		t.Fatalf("base policy retained unavailable run_code: %#v", a.basePolicy.Enabled)
	}
	if containsString(a.reg.Policy().Enabled, "run_code") {
		t.Fatalf("registry policy retained unavailable run_code: %#v", a.reg.Policy().Enabled)
	}
	if _, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unavailable run_code was executable")
	}
}

func TestRegisterCodeUnavailableRemovesRunCodeFromACPAndSDKCatalogs(t *testing.T) {
	oldProbe := probeTypeScriptRuntimeStatus
	probeTypeScriptRuntimeStatus = func() (bool, string) {
		return false, "permission model unavailable"
	}
	t.Cleanup(func() { probeTypeScriptRuntimeStatus = oldProbe })

	a := makeCodeApp(true)
	if err := a.registerCode(); err != nil {
		t.Fatalf("unavailable optional code capability must not stop startup: %v", err)
	}

	acpCatalog, err := (&acpFactory{app: a}).ToolCatalog(context.Background())
	if err != nil {
		t.Fatalf("ACP catalog: %v", err)
	}
	for _, entry := range acpCatalog.Tools {
		if entry.Name == "run_code" {
			t.Fatal("ACP catalog exposed unavailable run_code")
		}
	}

	sdkCatalog, err := newSDKServer(a, strings.NewReader(""), &strings.Builder{}).toolCatalog()
	if err != nil {
		t.Fatalf("SDK catalog: %v", err)
	}
	for _, entry := range sdkCatalog.Tools {
		if entry.Name == "run_code" {
			t.Fatal("SDK catalog exposed unavailable run_code")
		}
	}
}

// TestCodeUnavailableFailClosedAcrossEntries is the A3.2 release contract:
// a host without a proven Node permission model must not turn configured
// preference into availability. The same unavailable runtime is hidden from
// Native and Web projections, absent from ACP/SDK catalogs, removed from the
// execution whitelist, and returns the stable registry denial if addressed.
func TestCodeUnavailableFailClosedAcrossEntries(t *testing.T) {
	oldProbe := probeTypeScriptRuntimeStatus
	probeTypeScriptRuntimeStatus = func() (bool, string) {
		return false, "permission model unavailable"
	}
	t.Cleanup(func() { probeTypeScriptRuntimeStatus = oldProbe })

	a := makeCodeApp(true)
	if err := a.registerCode(); err != nil {
		t.Fatal(err)
	}
	if a.code != nil || a.codeUnavailableReason != "permission model unavailable" {
		t.Fatalf("unavailable runtime state = engine:%v reason:%q", a.code, a.codeUnavailableReason)
	}
	if containsString(a.reg.Policy().Enabled, "run_code") {
		t.Fatal("unavailable run_code remained whitelisted")
	}
	for _, spec := range toolSpecsForMode(config.ModeStandard, a.reg.VisibleSpecs()) {
		if spec.Name == "run_code" {
			t.Fatal("Native standard projection exposed unavailable run_code")
		}
	}

	cfg := a.webConfig()
	if cfg["code_available"] != false || cfg["code_enabled"] != false {
		t.Fatalf("Web capability projection = %#v, want code unavailable", cfg)
	}
	for _, name := range cfg["tools_enabled"].([]string) {
		if name == "run_code" {
			t.Fatal("Web tool inventory exposed unavailable run_code")
		}
	}

	acpCatalog, err := (&acpFactory{app: a}).ToolCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range acpCatalog.Tools {
		if entry.Name == "run_code" {
			t.Fatal("ACP catalog exposed unavailable run_code")
		}
	}

	sdkCatalog, err := newSDKServer(a, strings.NewReader(""), &strings.Builder{}).toolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range sdkCatalog.Tools {
		if entry.Name == "run_code" {
			t.Fatal("SDK catalog exposed unavailable run_code")
		}
	}

	if _, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(`{}`)); tools.ErrorInfoOf(err).Code != tools.CodeUnknownTool {
		t.Fatalf("unavailable run_code denial = %+v, want %s", err, tools.CodeUnknownTool)
	}
}

// TestRegisterCodeEnabledRegistersAndValidates verifies the enabled path: the
// TypeScript runtime is created, run_code is registered, D7 rejects bad
// arguments at the Execute gate, a valid run flows through and lands code/run
// in the session log (D3) without deriving into history (log-only), and a
// non-zero exit is returned to the model.
func TestRegisterCodeEnabledRegistersAndValidates(t *testing.T) {
	a := makeCodeApp(true)
	a.cfg.Workspace.DefaultDir = t.TempDir()
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatalf("register get_time: %v", err)
	}
	pol := codePolicy()
	pol.Enabled = []string{"get_time", "run_code"}
	a.reg.SetPolicy(pol)
	if err := a.registerCode(); err != nil {
		t.Fatalf("registerCode: %v", err)
	}
	defer a.code.Close()
	if a.code == nil {
		t.Fatal("code engine must be created when code.enabled=true")
	}
	found := false
	for _, s := range a.reg.Specs() {
		if s.Name == "run_code" {
			found = true
		}
	}
	if !found {
		t.Fatal("run_code not registered when code.enabled=true")
	}
	for _, spec := range a.reg.Specs() {
		if spec.Name != "run_code" {
			continue
		}
		// PTC's transport itself is an object-valued tool. The text shown in
		// res.Output is only its presentation projection.
		if spec.Parameters["type"] != "object" {
			t.Fatalf("run_code parameters = %#v, want object", spec.Parameters)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct{ name, args string }{
		{"run_code", `{}`},                                       // missing required code/description
		{"run_code", `{"code":"x"}`},                             // description required
		{"run_code", `{"description":"x"}`},                      // code required
		{"run_code", `{"code":"x","description":"x","extra":1}`}, // additional properties rejected
		{"run_code", `{"code":123,"description":"x"}`},           // code must be a string
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid run works and lands the code/run event (D3).
	good := fmt.Sprintf(`{"code":%s,"description":"call time and print a marker"}`, jsonString("const now = await tools.get_time({}); console.log('hi'); return now"))
	prepared, err := a.reg.Prepare(context.Background(), "outer-1", "run_code", json.RawMessage(good))
	if err != nil {
		t.Fatalf("prepare run_code via registry: %v", err)
	}
	res, err := a.reg.ExecutePrepared(context.Background(), prepared)
	if err != nil {
		t.Fatalf("run_code via registry: %v", err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Fatalf("run_code output = %q, want it to carry hi", res.Output)
	}
	value, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("run_code value = %#v, want DSH object envelope", res.Value)
	}
	if _, ok := value["logs"].([]any); !ok {
		t.Fatalf("run_code logs = %#v, want JSON array", value["logs"])
	}
	if value["result"] == nil {
		t.Fatalf("run_code value = %#v, want returned result", value)
	}
	if !hasEvent(a.log, session.EventCodeRun) {
		t.Fatal("code/run event missing from the session log after run_code")
	}
	if !hasEvent(a.log, session.EventCodeDispatchStart) || !hasEvent(a.log, session.EventCodeDispatch) {
		t.Fatalf("nested code dispatch events missing: %+v", a.log.Events())
	}
	var dispatchSeen bool
	for _, event := range a.log.Events() {
		if event.Type == session.EventCodeDispatch && strings.Contains(string(event.Data), `"subCallId":"outer-1:code:1"`) {
			dispatchSeen = true
		}
	}
	if !dispatchSeen {
		t.Fatalf("nested dispatch did not preserve parent call id: %+v", a.log.Events())
	}
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("code/run events must not derive into messages: %+v", msgs)
	}

	// A nested tool failure is catchable inside the TypeScript program, matching
	// DSH's ToolCallError promise rejection contract.
	fail := `{"code":"try { await tools.missing({}); return 'unexpected' } catch (error) { return { name: error.name, message: error.message } }","description":"catch a nested tool failure"}`
	res2, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(fail))
	if err != nil {
		t.Fatalf("nested tool failure must be catchable: %v", err)
	}
	if !strings.Contains(res2.Output, "ToolCallError") || !strings.Contains(res2.Output, "unknown tool") {
		t.Fatalf("run_code output = %q, want caught ToolCallError", res2.Output)
	}
}

func TestCodeModeNestedDispatchPreservesRichContentOrder(t *testing.T) {
	a := makeCodeApp(true)
	a.cfg.Workspace.DefaultDir = t.TempDir()
	if err := a.reg.Register(richCodeTool{}); err != nil {
		t.Fatal(err)
	}
	policy := codePolicy()
	policy.Enabled = []string{"run_code", "rich_code_tool"}
	a.reg.SetPolicy(policy)
	a.basePolicy = policy
	if err := a.registerCode(); err != nil {
		t.Fatal(err)
	}
	defer a.code.Close()
	result, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(`{"code":"const value = await tools.rich_code_tool({}); return value","description":"preserve rich nested output"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "value") {
		t.Fatalf("output = %q", result.Output)
	}
	if len(result.AdditionalContextMessages) != 1 || result.AdditionalContextMessages[0].Text() != "nested deferred context" {
		t.Fatalf("nested additional contexts = %+v, want one rich context", result.AdditionalContextMessages)
	}
	failing, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(`{"code":"await tools.rich_code_tool({}); throw new Error('outer failure')","description":"retain nested context on failure"}`))
	if err != nil {
		t.Fatalf("failed outer program must settle as a structured tool result: %v", err)
	}
	if !failing.IsError || len(failing.AdditionalContextMessages) != 1 || failing.AdditionalContextMessages[0].Text() != "nested deferred context" {
		t.Fatalf("failed outer result = %+v, want error plus deferred context", failing)
	}
	var dispatch map[string]any
	for _, event := range a.log.Events() {
		if event.Type == session.EventCodeDispatch {
			if err := json.Unmarshal(event.Data, &dispatch); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	content, ok := dispatch["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("dispatch content = %#v, want three ordered blocks", dispatch["content"])
	}
	if content[0].(map[string]any)["text"] != "before" || content[1].(map[string]any)["type"] != "image" || content[2].(map[string]any)["text"] != "after" {
		t.Fatalf("dispatch content order = %#v", content)
	}
}

func TestCodeModeNestedFailedDispatchPreservesDeferredContexts(t *testing.T) {
	a := makeCodeApp(true)
	if err := a.reg.Register(richFailCodeTool{}); err != nil {
		t.Fatal(err)
	}
	a.basePolicy = tools.Policy{Enabled: []string{"rich_fail_code_tool"}}
	value, err := a.executeCodeBinding(context.Background(), code.ProgramBindingRequest{
		CallID: "outer:code:1", Name: "rich_fail_code_tool", Args: map[string]any{},
	})
	if err == nil {
		t.Fatal("failed nested dispatch unexpectedly succeeded")
	}
	rich, ok := value.(code.ProgramBindingResult)
	if !ok || len(rich.AdditionalContextMessages) != 1 || rich.AdditionalContextMessages[0].Text() != "failure context" {
		t.Fatalf("failed nested rich result = %#v err=%v, want deferred context", value, err)
	}
}

func TestCodeContentBlocksPreservesUnknownRichWireMetadata(t *testing.T) {
	raw := json.RawMessage(`{"type":"audio","data":"AQ==","mimeType":"audio/wav","metadata":{"durationMs":120}}`)
	blocks := codeContentBlocks([]llm.ContentBlock{{Kind: llm.ContentBlockKind("audio"), Raw: raw}})
	if len(blocks) != 1 {
		t.Fatalf("projected blocks = %#v, want one block", blocks)
	}
	if got := blocks[0]; got["type"] != "audio" || got["data"] != "AQ==" || got["mimeType"] != "audio/wav" {
		t.Fatalf("projected rich block = %#v, want original audio metadata", got)
	}
	metadata, ok := blocks[0]["metadata"].(map[string]any)
	if !ok || metadata["durationMs"] != float64(120) {
		t.Fatalf("projected rich metadata = %#v, want nested duration", blocks[0]["metadata"])
	}
}

// TestRegisterCodePolicyDeadlineBoundsSandboxRun verifies code.timeout is the
// outer per-tool deadline for run_code (mirrors run_command): a sandbox run
// that would outlive the config bound is cut at the Execute gate even when the
// model requests a longer per-call timeout, and the cut surfaces as a normal
// sandbox timeout result (the model sees the "[timed out]" marker, not an
// error).
func TestRegisterCodePolicyDeadlineBoundsSandboxRun(t *testing.T) {
	a := makeCodeApp(true)
	a.cfg.Workspace.DefaultDir = t.TempDir()
	pol := codePolicy()
	pol.CodeRun.Timeout = 200 * time.Millisecond
	a.reg.SetPolicy(pol)
	if err := a.registerCode(); err != nil {
		t.Fatalf("registerCode: %v", err)
	}
	defer a.code.Close()
	args := `{"code":"await new Promise(() => {})","description":"wait forever"}`
	res, err := a.reg.Execute(context.Background(), "run_code", json.RawMessage(args))
	if err != nil {
		t.Fatalf("run_code after the policy deadline must be a normal timeout result, not an error: %v", err)
	}
	if !res.IsError || res.Error == nil || res.Error.Code != "TOOL_TIMEOUT" {
		t.Fatalf("run_code result = %+v, want structured timeout (the code.timeout bound cut the run)", res)
	}
}

// jsonString returns s as a JSON string literal (for embedding paths/code in
// tool argument JSON).
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
