// tools.go — the M6e-2 Consumer half of the code-sandbox seam (design.md §8
// Consumer / D2, dispatch-m6e-2 §3): run_code is registered into the
// tools.Registry by the composition root (cmd/pa) when code.enabled, and
// auto-whitelisted by config.applyDefaults the same way the job_*/subagent_*/
// skill_*/schedule_*/plan_*/spill_*/interact_* tools are. It implements the
// tools.Tool method set structurally (Go structural typing), so this package
// never imports the tools package — the seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates model-generated
// arguments against the compiled JSON Schema before this code runs. The
// production TypeScript contract requires code + description; the legacy
// Engine path retains its shell-specific schema for seam tests.
//
// D3 event logging follows the M5a-2 tool-layer decision (ADR 决策 M6e /
// dispatch-m6e-2 §3): run_code emits code/run on a completed sandbox
// execution — zero or non-zero exit, with or without a timeout/truncation
// marker — through the injected onEvent sink (the composition root wires it to
// the session log), inside a tool Execute on the serial main-loop path (D5). A
// run that failed to happen at all (invalid request, closed engine, cancelled
// context, start failure) returns an error and logs nothing — the loop
// surfaces it as tool/error.
//
// The tool is the sandboxed sibling of M3's run_command (ADR 决策 M6e):
// run_command stays available; run_code adds the controlled-sandbox semantics
// (a hard-kill timeout, per-stream output quotas, and an isolated sandbox cwd)
// on top. A non-zero exit code and a timeout are normal sandbox outcomes
// returned to the model, never a panic (dispatch-m6e-2 §3: 超时/非零退出码返回
// 给模型，非 panic).
package code

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
)

// ToolRunName is the code-sandbox tool (whitelisted when code.enabled; see
// config.codeToolNames).
const ToolRunName = "run_code"

// CodeTools bundles the shared state of the run_code tool: the Engine service,
// the event sink, and the config-derived sandbox policy knobs the composition
// root supplies (code.timeout / code.max_output / code.sandbox_dir). Keeping
// the knobs as fields — set by the wiring after NewCodeTools — keeps the
// constructor's signature the seam contract and the tool package decoupled
// from config (D2).
type CodeTools struct {
	e          Engine
	runtime    ProgramRuntime
	binding    ProgramBinding
	onEvent    func(typ string, data any)
	onEventErr func(typ string, data any) error

	// DefaultTimeout is the sandbox deadline applied when the model omits the
	// per-call timeout (code.timeout; 0 ⇒ the provider default 30s). Set by the
	// composition root.
	DefaultTimeout time.Duration
	// DefaultMaxOutput is the per-stream output cap of a sandbox run
	// (code.max_output; 0 ⇒ the provider default 64KiB). The model cannot
	// override it. Set by the composition root.
	DefaultMaxOutput int
	// DefaultComputeMS and DefaultMaxWallMS are the independent TypeScript
	// worker budgets. They are separate from DefaultTimeout, which remains the
	// legacy shell-provider setting.
	DefaultComputeMS int
	DefaultMaxWallMS int
	// MaxOldGenerationSizeMB is forwarded to the subprocess runtime as a heap
	// ceiling when non-zero.
	MaxOldGenerationSizeMB int
	// DefaultCwd is the sandbox working directory used when the model omits
	// cwd when no resolver is installed (code.sandbox_dir; empty ⇒ the provider
	// default <project>/.sandbox). Set by the composition root.
	DefaultCwd string
	// DefaultCwdFunc resolves the default execution directory at call time. The
	// composition root uses this to bind run_code to the active session workspace
	// while preserving an explicit cwd override from the model.
	DefaultCwdFunc        func() string
	DefaultCwdContextFunc func(context.Context) string
	// DefaultMode is the per-call sandbox authority used by the legacy shell
	// consumer. The composition root sets this to workspace-write; providers
	// without an enforcing backend reject it rather than running unconfined.
	DefaultMode             SandboxMode
	RequireStrongSandbox    bool
	RequireNetworkIsolation bool
	// MaxParallelSubCalls and IsConcurrencySafe are forwarded to the
	// TypeScript runtime's per-program dispatch gate. The host normally fills
	// the classifier from tools.Registry; nil preserves standalone behavior.
	MaxParallelSubCalls int
	IsConcurrencySafe   func(name string, args any) bool
}

// NewCodeTools returns the run_code tool bundle bound to an Engine. onEvent,
// when non-nil, receives the code/* event payloads; the composition root wires
// it to the session log (D3).
func NewCodeTools(e Engine, onEvent func(typ string, data any)) *CodeTools {
	return &CodeTools{e: e, onEvent: onEvent}
}

// NewCodeToolsWithRuntime returns a DSH-compatible TypeScript Code Mode tool
// bundle. The host installs the registry bridge through SetBinding.
func NewCodeToolsWithRuntime(runtime ProgramRuntime, onEvent func(typ string, data any)) *CodeTools {
	return &CodeTools{runtime: runtime, onEvent: onEvent, MaxParallelSubCalls: defaultMaxParallelSubCalls}
}

// SetBinding installs the host-side dispatch bridge used by tools.<name>() in
// a TypeScript program.
func (t *CodeTools) SetBinding(binding ProgramBinding) { t.binding = binding }

// SetErrorSink lets a composition root enforce durable event failures for
// legacy calls that do not carry a runtime context. Runtime Emit callbacks
// already return errors and take precedence.
func (t *CodeTools) SetErrorSink(sink func(typ string, data any) error) { t.onEventErr = sink }

// Run returns the run_code tool.
func (t *CodeTools) Run() CodeRunTool { return CodeRunTool{t: t} }

// emit forwards one code/* event payload to the injected sink (D3).
func (t *CodeTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *CodeTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	if t.onEventErr != nil {
		return t.onEventErr(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// CodeRunTool executes TypeScript Code Mode through ProgramRuntime in the
// production wiring. The legacy Engine constructor remains available for
// isolated shell-seam tests; it is not used by cmd/pa's PTC path.
type CodeRunTool struct {
	t *CodeTools
}

func (CodeRunTool) Name() string { return ToolRunName }

// CancellationAware is explicit: both runtime and local-command execution
// derive their deadlines from the registry context.
func (CodeRunTool) CancellationAware() bool { return true }

func (t CodeRunTool) Description() string {
	if t.t.runtime != nil {
		return "Execute a TypeScript program against the available tools. The code is the body of an async function; top-level await and return work. Call host tools as await tools.name(args). Only printed or returned values become program output."
	}
	return "run a shell script in a controlled local sandbox (separate child process, hard-kill timeout, per-stream output quota, isolated sandbox cwd, no network credentials); returns the exit code and output, marking timeouts and truncation — a non-zero exit or a timeout is a normal outcome"
}

func (t CodeRunTool) Schema() map[string]any {
	if t.t.runtime != nil {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "the body of an async TypeScript function; top-level await and return are supported",
				},
				"description": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "a concise description of what the program does",
				},
			},
			"required":             []string{"code", "description"},
			"additionalProperties": false,
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"lang": map[string]any{
				"type":        "string",
				"enum":        []string{"sh"},
				"description": "language of code; v1 supports only sh",
			},
			"code": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the shell script to execute in the sandbox (a single command line, no interactive shell)",
			},
			"timeout": map[string]any{
				"type":        "number",
				"minimum":     0,
				"description": "sandbox timeout in seconds (optional; default code.timeout)",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "sandbox working directory (optional; default code.sandbox_dir or <project>/.sandbox)",
			},
		},
		"required":             []string{"lang", "code"},
		"additionalProperties": false,
	}
}

func (t CodeRunTool) OutputSchema() map[string]any {
	if t.t.runtime == nil {
		return map[string]any{"type": "string"}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"logs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "result": map[string]any{}},
		"required":             []string{"logs"},
		"additionalProperties": false,
	}
}

func (t CodeRunTool) Execute(ctx context.Context, args any) (string, error) {
	if t.t.runtime != nil {
		return t.executeProgram(ctx, args)
	}
	var a struct {
		Lang    string  `json:"lang"`
		Code    string  `json:"code"`
		Timeout float64 `json:"timeout"`
		Cwd     string  `json:"cwd"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("run_code: %w", err)
	}
	lang := a.Lang
	if lang == "" {
		lang = langSh
	}
	if lang != langSh {
		return "", fmt.Errorf("run_code: unsupported lang %q (only %q)", a.Lang, langSh)
	}
	if strings.TrimSpace(a.Code) == "" {
		return "", fmt.Errorf("run_code: empty code")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("run_code: cancelled: %w", err)
	}
	req := RunRequest{
		Lang:                    lang,
		Code:                    a.Code,
		Mode:                    t.t.DefaultMode,
		MaxOutput:               t.t.DefaultMaxOutput,
		Cwd:                     a.Cwd,
		RequireStrongIsolation:  t.t.RequireStrongSandbox,
		RequireNetworkIsolation: t.t.RequireNetworkIsolation,
	}
	if a.Timeout > 0 {
		req.Timeout = time.Duration(a.Timeout * float64(time.Second))
	} else {
		req.Timeout = t.t.DefaultTimeout
	}
	if req.Cwd == "" {
		req.Cwd = t.t.DefaultCwd
		if t.t.DefaultCwdContextFunc != nil {
			req.Cwd = t.t.DefaultCwdContextFunc(ctx)
		} else if t.t.DefaultCwdFunc != nil {
			req.Cwd = t.t.DefaultCwdFunc()
		}
	}
	res, err := t.t.e.Run(ctx, req)
	if err != nil {
		return "", fmt.Errorf("run_code: %w", err)
	}
	// code/run is a log-only fact (D3) carrying the language and the outcome
	// markers; the full stdout/stderr live in the tool/result the loop logs.
	if err := t.t.emitContext(ctx, session.EventCodeRun, session.NewCodeRun(lang, res.ExitCode, res.TimedOut, res.Truncated)); err != nil {
		return "", fmt.Errorf("run_code: persist event: %w", err)
	}
	return formatResult(res), nil
}

func (t CodeRunTool) executeProgramResult(ctx context.Context, args any) (ProgramResult, error) {
	var a struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return ProgramResult{}, fmt.Errorf("run_code: %w", err)
	}
	if strings.TrimSpace(a.Code) == "" {
		return ProgramResult{}, fmt.Errorf("run_code: empty TypeScript program")
	}
	if strings.TrimSpace(a.Description) == "" {
		return ProgramResult{}, fmt.Errorf("run_code: description is required")
	}
	if t.t.binding == nil {
		return ProgramResult{}, fmt.Errorf("run_code: TypeScript tool binding is not configured")
	}
	cwd := t.t.DefaultCwd
	if t.t.DefaultCwdContextFunc != nil {
		cwd = t.t.DefaultCwdContextFunc(ctx)
	} else if t.t.DefaultCwdFunc != nil {
		cwd = t.t.DefaultCwdFunc()
	}
	parentCallID := agenttools.CallIDFromContext(ctx)
	if strings.TrimSpace(parentCallID) == "" {
		// Standalone consumers may not install a registry call context, but the
		// durable nested-dispatch contract still requires a non-empty root.
		parentCallID = fmt.Sprintf("run_code:%d", time.Now().UnixNano())
	}
	// Binding calls may overlap. Keep deferred contexts in submission order,
	// matching the reference scheduler's post-result FIFO rather than the
	// nondeterministic order in which host callbacks happen to settle.
	var additionalContextsMu sync.Mutex
	additionalContextBuckets := make(map[int][]llm.Message)
	nextBindingOrder := 0
	nestedConcludesTurn := false
	binding := func(bindingCtx context.Context, request ProgramBindingRequest) (any, error) {
		additionalContextsMu.Lock()
		bindingOrder := nextBindingOrder
		nextBindingOrder++
		additionalContextsMu.Unlock()
		// Keep nested dispatch visible in the trajectory while its canonical
		// value remains private to the current TypeScript program.
		if err := t.t.emitContext(bindingCtx, session.EventCodeDispatchStart, session.NewCodeDispatchStart(
			parentCallID, parentCallID, request.CallID, request.Name, request.Args,
		)); err != nil {
			return nil, fmt.Errorf("code dispatch start: %w", err)
		}
		value, bindingErr := t.t.binding(bindingCtx, request)
		content := []map[string]any{{"type": "text", "text": formatProgramBindingValue(value)}}
		var dispatchMeta any
		var additionalContexts []string
		var additionalContextMessages []llm.Message
		var concludesTurn bool
		switch rich := value.(type) {
		case ProgramBindingResult:
			value = rich.Value
			content = rich.Content
			dispatchMeta = rich.Meta
			additionalContexts = rich.AdditionalContexts
			additionalContextMessages = cloneCodeContextMessages(rich.AdditionalContextMessages)
			concludesTurn = rich.ConcludesTurn
		case *ProgramBindingResult:
			if rich != nil {
				value = rich.Value
				content = rich.Content
				dispatchMeta = rich.Meta
				additionalContexts = rich.AdditionalContexts
				additionalContextMessages = cloneCodeContextMessages(rich.AdditionalContextMessages)
				concludesTurn = rich.ConcludesTurn
			}
		}
		if bindingErr != nil {
			content = []map[string]any{{"type": "text", "text": bindingErr.Error()}}
		}
		if len(additionalContextMessages) > 0 {
			additionalContextsMu.Lock()
			additionalContextBuckets[bindingOrder] = additionalContextMessages
			additionalContextsMu.Unlock()
		}
		if concludesTurn {
			additionalContextsMu.Lock()
			nestedConcludesTurn = true
			additionalContextsMu.Unlock()
		}
		emitErr := t.t.emitContext(bindingCtx, session.EventCodeDispatch, session.NewCodeDispatchWithContentMetaAndConclusion(
			parentCallID, parentCallID, request.CallID, request.Name, request.Args,
			bindingErr != nil, content, dispatchMeta, additionalContexts, additionalContextMessages, concludesTurn,
		))
		if emitErr != nil {
			if bindingErr != nil {
				return nil, errors.Join(bindingErr, fmt.Errorf("code dispatch: %w", emitErr))
			}
			return nil, fmt.Errorf("code dispatch: %w", emitErr)
		}
		return value, bindingErr
	}
	result, err := t.t.runtime.RunProgram(ctx, ProgramRequest{
		Code:                   a.Code,
		Cwd:                    cwd,
		Timeout:                t.t.DefaultTimeout,
		ComputeMS:              t.t.DefaultComputeMS,
		MaxWallMS:              t.t.DefaultMaxWallMS,
		MaxOutput:              t.t.DefaultMaxOutput,
		MaxOldGenerationSizeMB: t.t.MaxOldGenerationSizeMB,
		ParentCallID:           parentCallID,
		Binding:                binding,
		Bindings:               []ProgramBindingNamespace{{Global: "tools"}},
		MaxParallelSubCalls:    t.t.MaxParallelSubCalls,
		IsConcurrencySafe:      t.t.IsConcurrencySafe,
	})
	if err != nil {
		return ProgramResult{}, fmt.Errorf("run_code: %w", err)
	}
	if err := validateProgramOutputBudget(result, t.t.DefaultMaxOutput); err != nil {
		return ProgramResult{}, fmt.Errorf("run_code: %w", err)
	}
	additionalContextsMu.Lock()
	for index := 0; index < nextBindingOrder; index++ {
		result.AdditionalContextMessages = append(result.AdditionalContextMessages, additionalContextBuckets[index]...)
	}
	result.ConcludesTurn = nestedConcludesTurn
	additionalContextsMu.Unlock()
	if result.Failure != nil {
		// Keep the structured result alive so ExecuteResult can carry deferred
		// contexts even when the outer program fails. The legacy string Execute
		// adapter below converts this settled failure back to an error.
		return result, nil
	}
	if err := t.t.emitContext(ctx, session.EventCodeRun, session.NewCodeRun("typescript", 0, false, result.Truncated)); err != nil {
		return ProgramResult{}, fmt.Errorf("run_code: persist event: %w", err)
	}
	return result, nil
}

// validateProgramOutputBudget applies the same outer-result accounting to
// Code Mode's structured completion that the runtime already applies to log
// capture. The subprocess cannot know the final return value until the
// program completes, so this final gate prevents a large returned object from
// bypassing the configured output quota.
func validateProgramOutputBudget(result ProgramResult, limit int) error {
	if limit <= 0 {
		return nil
	}
	envelope := struct {
		Logs   []string `json:"logs"`
		Result any      `json:"result,omitempty"`
	}{Logs: result.Logs}
	if result.HasValue {
		envelope.Result = result.Value
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("program output is not lossless JSON: %w", err)
	}
	if len(encoded) > limit {
		return fmt.Errorf("program output exceeded %d bytes", limit)
	}
	return nil
}

func (t CodeRunTool) executeProgram(ctx context.Context, args any) (string, error) {
	result, err := t.executeProgramResult(ctx, args)
	if err != nil {
		return "", err
	}
	if result.Failure != nil {
		return "", fmt.Errorf("%s", programFailureMessage(result))
	}
	return formatProgramResult(result), nil
}

// ExecuteResult exposes DSH Code Mode's canonical outer value. Logs and the
// optional completion value stay typed for the SDK; Output remains the human
// readable projection used by the conversation and trajectory.
func (t CodeRunTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if t.t.runtime == nil {
		raw, err := t.Execute(ctx, args)
		if err != nil {
			return agenttools.ToolResult{}, err
		}
		return agenttools.ToolResult{Value: raw, Output: raw}, nil
	}
	result, err := t.executeProgramResult(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	if result.Failure != nil {
		return agenttools.ToolResult{
			Output:                    programFailureMessage(result),
			AdditionalContextMessages: cloneCodeContextMessages(result.AdditionalContextMessages),
			IsError:                   true,
			Error:                     &agenttools.ErrorInfo{Name: "CodeRunFailedError", Code: "CODE_RUN_FAILED"},
		}, nil
	}
	logs := make([]any, len(result.Logs))
	for i, line := range result.Logs {
		logs[i] = line
	}
	value := map[string]any{"logs": logs}
	if result.HasValue {
		value["result"] = result.Value
	}
	return agenttools.ToolResult{Value: value, Output: formatProgramResult(result), AdditionalContextMessages: cloneCodeContextMessages(result.AdditionalContextMessages)}, nil
}

func programFailureMessage(result ProgramResult) string {
	if result.Failure == nil {
		return "code run failed"
	}
	message := fmt.Sprintf("code run failed (%s): %s", result.Failure.Kind, result.Failure.Message)
	if len(result.Logs) > 0 {
		message += "\nCaptured output:\n" + strings.Join(result.Logs, "\n")
	}
	return message
}

func formatProgramResult(result ProgramResult) string {
	parts := make([]string, 0, 2)
	if len(result.Logs) > 0 {
		parts = append(parts, strings.Join(result.Logs, "\n"))
	}
	if result.HasValue {
		if encoded, err := json.MarshalIndent(result.Value, "", "  "); err == nil {
			parts = append(parts, string(encoded))
		} else {
			parts = append(parts, fmt.Sprintf("[invalid program result: %v]", err))
		}
	}
	if len(parts) == 0 {
		return "(run_code completed with no output)"
	}
	return strings.Join(parts, "\n")
}

func formatProgramBindingValue(value any) string {
	if value == nil {
		return "null"
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("[unrenderable tool result: %v]", err)
	}
	return string(encoded)
}

// formatResult renders one sandbox Result as model-facing text: the outcome
// markers (timeout, non-zero exit, truncation) followed by the bounded
// stdout/stderr.
func formatResult(res Result) string {
	var sb strings.Builder
	if res.TimedOut {
		fmt.Fprintf(&sb, "[timed out after %s]\n", res.Duration.Round(time.Millisecond))
	}
	if res.ExitCode != 0 {
		fmt.Fprintf(&sb, "[exit code: %d]\n", res.ExitCode)
	}
	if res.Truncated {
		sb.WriteString("[output truncated at the sandbox quota]\n")
	}
	if res.Stdout != "" {
		fmt.Fprintf(&sb, "[stdout]\n%s\n", strings.TrimRight(res.Stdout, "\n"))
	}
	if res.Stderr != "" {
		fmt.Fprintf(&sb, "[stderr]\n%s\n", strings.TrimRight(res.Stderr, "\n"))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
