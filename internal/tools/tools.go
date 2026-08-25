// Package tools implements the capability registry: registration, the
// model-facing schema projection, and the single validated execution gate
// (D7). Every Execute validates the model-generated arguments against the
// tool's JSON Schema before dispatch; tools never parse bare JSON themselves.
// M3 added the safety policy to this same gate: a name whitelist, a per-tool
// deadline (context.WithTimeout), and an output-size cap with spill-to-disk —
// all inside the tools package, never in the loop (design.md §5, D4).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/jabing/shutu-agent/internal/llm"
)

// Tool is the complete dsh-style definition of one capability. Arguments have
// already crossed the single JSON materialization boundary when Execute runs.
type Tool interface {
	Name() string
	// Description is a short human-readable summary of what the tool does. It
	// feeds the prompt's automatic tool catalog (design.md §7) and the
	// model-facing request schema.
	Description() string
	Schema() map[string]any       // JSON Schema of the arguments; also sent to the model
	OutputSchema() map[string]any // JSON Schema of the canonical tool value
	Execute(ctx context.Context, args any) (string, error)
}

// ConcurrencySafe is the opt-in dsh execution classifier. Tools must explicitly
// implement it and return true before sibling calls may overlap. Omission is
// exclusive, matching dsh's fail-closed concurrency contract.
type ConcurrencySafe interface {
	ConcurrencySafe(args any) bool
}

// ToolValue is the canonical value returned by a tool. Value is lossless JSON;
// Content is the optional model-facing rich projection.
type ToolValue struct {
	Value   any
	Content []llm.ContentBlock
}

// ResultExecutor is the canonical rich-result form for tools whose output is
// not adequately represented by a text value. It still receives parsed args;
// the registry owns normalization, timeout, and final presentation.
type ResultExecutor interface {
	ExecuteResult(ctx context.Context, args any) (ToolResult, error)
}

type Execution struct {
	CallID    string
	Name      string
	Arguments any
	Context   context.Context
}

type PreToolDecision struct {
	Kind   string
	Reason string
}

type PreExecuteHook func(context.Context, Execution) (PreToolDecision, error)
type ExecuteHook func(context.Context, Execution, func(context.Context) (ToolResult, error)) (ToolResult, error)
type PostExecuteHook func(context.Context, Execution, ToolResult) (ToolResult, error)
type ResultHook func(Execution, ToolResult)

// ToolResult is the structured outcome of one tool execution after the
// Execute pipeline has applied the timeout and output cap. Output is the
// model-facing text (the truncated head plus the locator notice when spilled);
// Content carries rich blocks, and IsError mirrors dsh's ToolResult error bit.
type ToolResult struct {
	Value      any
	Output     string
	SpillPath  string // non-empty => Output was truncated and the full text spilled
	SpillBytes int    // full output size in bytes (when spilled)
	Content    []llm.ContentBlock
	IsError    bool
	Error      *ErrorInfo // structured failure classification, when IsError is true
}

// ErrorInfo is the durable classification of a failed tool call. The model
// receives the human-readable message; the code/name pair is retained for
// replay, UI and policy consumers.
type ErrorInfo struct {
	Name string
	Code string
}

// ExecutionError is an error that already carries dsh-compatible tool failure
// metadata. The loop uses ErrorInfoOf to preserve the classification in the
// tool/result event instead of collapsing every failure to TOOL_EXECUTION_ERROR.
type ExecutionError struct {
	Info    ErrorInfo
	Message string
	Cause   error
}

func (e *ExecutionError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Info.Code
}

func (e *ExecutionError) Unwrap() error { return e.Cause }

// ErrorInfoOf returns the stable dsh-style classification for an execution
// failure. Plain tool errors retain their text and use the generic UNKNOWN
// code, while registry-owned failures expose their specific code.
func ErrorInfoOf(err error) ErrorInfo {
	var classified *ExecutionError
	if errors.As(err, &classified) {
		return classified.Info
	}
	return ErrorInfo{Name: "ToolError", Code: "UNKNOWN"}
}

// PreparedExecution is the ordered pre-dispatch portion of one call. It is
// intentionally opaque: callers may only pass it back to ExecutePrepared.
// Separating preparation from the body lets parallel-safe calls overlap while
// dsh's pre-execute/approval stage remains ordered.
type PreparedExecution struct {
	registry *Registry
	callID   string
	name     string
	args     any
	timeout  time.Duration
}

// Owner binds the registry's spill naming to the active session. It is set by
// the REPL whenever the current session changes; the agent loop is strictly
// serial (D5), so a single owner is safe.
type Owner struct {
	SessionID string
	// NextSeq returns the Seq the upcoming tool/result event will receive; it
	// is consulted only when a spill is about to be written.
	NextSeq func() uint64
}

// Registry owns the registered tools and their compiled schemas.
type Registry struct {
	tools         map[string]Tool
	schemas       map[string]*jsonschema.Schema
	outputSchemas map[string]*jsonschema.Schema
	policy        Policy
	owner         Owner
	// gate, when installed (M6d-2), is the optional pre-execution hook the
	// sensitive-tool approval runs through. The registry owns the hook so the
	// gate stays inside the Execute pipeline (policy lives here, never in the
	// loop, D4); the gate function itself is provided by the composition root
	// (cmd/pa). nil means no gating.
	preHooks     []PreExecuteHook
	executeHooks []ExecuteHook
	postHooks    []PostExecuteHook
	resultHooks  []ResultHook
	// fallbackSeq is used only when Owner.NextSeq is nil (a spill with no
	// bound session); it keeps spill filenames unique.
	fallbackSeq uint64
	spillMu     sync.Mutex
}

// IsConcurrencySafe reports whether a registered call may join a parallel
// group. The caller must pass the already-materialized arguments from Prepare;
// re-materializing here would violate dsh's single parse boundary.
func (r *Registry) IsConcurrencySafe(name string, args any) (safe bool) {
	t, ok := r.tools[name]
	if !ok {
		return false
	}
	// dsh treats a missing, throwing or non-true classifier as exclusive. The
	// recover is deliberate: classification must never turn a bad tool into a
	// scheduler failure.
	defer func() {
		if recover() != nil {
			safe = false
		}
	}()
	if classifier, ok := t.(ConcurrencySafe); ok {
		return classifier.ConcurrencySafe(args) == true
	}
	return false
}

// New returns a registry with the safe-by-default policy (M3): read-only
// whitelist, 30s deadline, 64KB output cap.
func New() *Registry {
	return &Registry{
		tools:         map[string]Tool{},
		schemas:       map[string]*jsonschema.Schema{},
		outputSchemas: map[string]*jsonschema.Schema{},
		policy:        DefaultPolicy(),
	}
}

// Clone returns an independent registry view for a child scope. Tool
// implementations are intentionally shared, while the schema map, policy,
// owner and gate are copied so a scoped capability (for example dsh's
// structured_output tool) cannot mutate the parent registry.
func (r *Registry) Clone() *Registry {
	clone := &Registry{
		tools:         make(map[string]Tool, len(r.tools)),
		schemas:       make(map[string]*jsonschema.Schema, len(r.schemas)),
		outputSchemas: make(map[string]*jsonschema.Schema, len(r.outputSchemas)),
		policy:        r.policy,
		owner:         r.owner,
		preHooks:      append([]PreExecuteHook(nil), r.preHooks...),
		executeHooks:  append([]ExecuteHook(nil), r.executeHooks...),
		postHooks:     append([]PostExecuteHook(nil), r.postHooks...),
		resultHooks:   append([]ResultHook(nil), r.resultHooks...),
		fallbackSeq:   r.fallbackSeq,
	}
	clone.policy.Enabled = append([]string(nil), r.policy.Enabled...)
	for name, tool := range r.tools {
		clone.tools[name] = tool
	}
	for name, schema := range r.schemas {
		clone.schemas[name] = schema
	}
	for name, schema := range r.outputSchemas {
		clone.outputSchemas[name] = schema
	}
	return clone
}

// SetPolicy installs the Execute pipeline's safety policy (M3). The REPL
// installs the config-derived policy at startup.
func (r *Registry) SetPolicy(p Policy) { r.policy = p }

// SetOwner binds the registry to the active session for spill naming (M3).
func (r *Registry) SetOwner(o Owner) { r.owner = o }

func (r *Registry) AddPreExecuteHook(hook PreExecuteHook)   { r.preHooks = append(r.preHooks, hook) }
func (r *Registry) AddExecuteHook(hook ExecuteHook)         { r.executeHooks = append(r.executeHooks, hook) }
func (r *Registry) AddPostExecuteHook(hook PostExecuteHook) { r.postHooks = append(r.postHooks, hook) }
func (r *Registry) AddResultHook(hook ResultHook)           { r.resultHooks = append(r.resultHooks, hook) }

// Allow adds names to the policy whitelist after it was installed. The
// composition root uses it for dynamically discovered tool names that cannot
// be known at config time — the MCP server tools bridged as mcp.<server>.<tool>
// (M6f-2 §4). Idempotent for names already whitelisted; empty names are
// ignored.
func (r *Registry) Allow(names ...string) {
	for _, name := range names {
		if name == "" || r.policy.Allows(name) {
			continue
		}
		r.policy.Enabled = append(r.policy.Enabled, name)
	}
}

// Register adds a tool. A duplicate name is rejected. The argument schema is
// compiled once at registration so Execute has no per-call compile cost.
func (r *Registry) Register(t Tool) error {
	if _, ok := r.tools[t.Name()]; ok {
		return fmt.Errorf("tools: tool %q already registered", t.Name())
	}
	raw, err := json.Marshal(t.Schema())
	if err != nil {
		return fmt.Errorf("tools: marshal schema for %q: %w", t.Name(), err)
	}
	compiler := jsonschema.NewCompiler()
	url := "tool://" + t.Name()
	if err := compiler.AddResource(url, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("tools: add schema for %q: %w", t.Name(), err)
	}
	sch, err := compiler.Compile(url)
	if err != nil {
		return fmt.Errorf("tools: compile schema for %q: %w", t.Name(), err)
	}
	outputRaw, err := json.Marshal(t.OutputSchema())
	if err != nil {
		return fmt.Errorf("tools: marshal output schema for %q: %w", t.Name(), err)
	}
	outputURL := "tool-output://" + t.Name()
	if err := compiler.AddResource(outputURL, bytes.NewReader(outputRaw)); err != nil {
		return fmt.Errorf("tools: add output schema for %q: %w", t.Name(), err)
	}
	outputSchema, err := compiler.Compile(outputURL)
	if err != nil {
		return fmt.Errorf("tools: compile output schema for %q: %w", t.Name(), err)
	}
	r.tools[t.Name()] = t
	r.schemas[t.Name()] = sch
	r.outputSchemas[t.Name()] = outputSchema
	return nil
}

// Specs returns the model-facing tool schemas, sorted by name for a stable
// prompt/request.
func (r *Registry) Specs() []llm.ToolSchema {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	specs := make([]llm.ToolSchema, 0, len(names))
	for _, name := range names {
		specs = append(specs, llm.ToolSchema{
			Name:        name,
			Description: r.tools[name].Description(),
			Parameters:  r.tools[name].Schema(),
		})
	}
	return specs
}

// codeRunToolName is the M6e-2 code-sandbox tool (registered by the composition
// root from internal/code when code.enabled). The name is mirrored here —
// exactly like runCommandName in run_command.go — so the Execute pipeline can
// apply its per-tool timeout override without the tools package importing the
// seam.
const codeRunToolName = "run_code"

// Execute is the single execution gate. It performs dsh's ordered preparation
// and then dispatches the tool body. All policy lives here, never in the loop.
func (r *Registry) Execute(ctx context.Context, name string, args any) (ToolResult, error) {
	prepared, err := r.Prepare(ctx, "", name, args)
	if err != nil {
		return ToolResult{}, err
	}
	return r.ExecutePrepared(ctx, prepared)
}

// Prepare performs the ordered pre-dispatch stage: name/policy lookup, JSON
// argument validation and the approval gate. It never invokes the tool body.
// Callers may safely run this stage serially before dispatching a parallel-safe
// group, matching dsh's pre-execute ordering.
func (r *Registry) Prepare(ctx context.Context, callID, name string, args any) (*PreparedExecution, error) {
	_, ok := r.tools[name]
	if !ok {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolNotFoundError", Code: "UNKNOWN_TOOL"}, Message: fmt.Sprintf("unknown tool %q", name)}
	}
	if !r.policy.Allows(name) {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolPolicyError", Code: "TOOL_DENIED"}, Message: fmt.Sprintf("tool %q is not enabled (see tools.enabled)", name)}
	}
	v, err := ParseArguments(args)
	if err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolArgsError", Code: "INVALID_ARGS"}, Message: fmt.Sprintf("invalid arguments: %v", err), Cause: err}
	}
	if err := r.schemas[name].Validate(v); err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolArgsError", Code: "INVALID_ARGS"}, Message: fmt.Sprintf("invalid arguments: %v", err), Cause: err}
	}

	// M6d-2 sensitive-tool gate: runs after whitelist + D7 validation but
	// before the per-tool deadline is applied — the user's approval answer is
	// an interactive terminal read on the CLI serial path (D5), not a tool
	// computation, so it must not be bounded by tools.timeout. A non-nil return
	// is the gate's verdict and is returned verbatim (the tool never runs).
	execution := Execution{CallID: callID, Name: name, Arguments: v, Context: ctx}
	for _, hook := range r.preHooks {
		decision, err := hook(ctx, execution)
		if err != nil {
			return nil, &ExecutionError{
				Info:    ErrorInfo{Name: "ToolError", Code: "TOOL_EXECUTION_ERROR"},
				Message: err.Error(),
				Cause:   err,
			}
		}
		if decision.Kind == "deny" || decision.Kind == "ask" {
			reason := decision.Reason
			if reason == "" {
				reason = "tool execution denied"
			}
			return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolPolicyError", Code: "TOOL_DENIED"}, Message: reason}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "AbortError", Code: "ABORTED_BEFORE_DISPATCH"}, Message: "tool call aborted before dispatch", Cause: err}
	}

	timeout := r.policy.Timeout
	if name == runCommandName {
		timeout = r.policy.RunCommand.Timeout
		if timeout <= 0 {
			timeout = DefaultRunCommandTimeout
		}
	}
	if name == codeRunToolName && r.policy.CodeRun.Timeout > 0 {
		timeout = r.policy.CodeRun.Timeout
	}
	return &PreparedExecution{registry: r, callID: callID, name: name, args: v, timeout: timeout}, nil
}

// ExecutePrepared dispatches a previously prepared call. Preparation is
// deliberately separate so approval and policy do not run concurrently with
// sibling calls, while tool bodies may still overlap.
func (r *Registry) ExecutePrepared(ctx context.Context, prepared *PreparedExecution) (ToolResult, error) {
	if prepared == nil || prepared.registry != r {
		return ToolResult{}, &ExecutionError{Info: ErrorInfo{Name: "ToolExecutionError", Code: "UNKNOWN"}, Message: "invalid prepared tool execution"}
	}
	t, ok := r.tools[prepared.name]
	if !ok {
		return ToolResult{}, &ExecutionError{Info: ErrorInfo{Name: "ToolNotFoundError", Code: "UNKNOWN_TOOL"}, Message: fmt.Sprintf("unknown tool %q", prepared.name)}
	}
	execCtx := ctx
	if prepared.timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, prepared.timeout)
		defer cancel()
	}

	execution := Execution{CallID: prepared.callID, Name: prepared.name, Arguments: prepared.args, Context: ctx}
	finish := func(result ToolResult) (ToolResult, error) {
		if !result.IsError {
			if result.Value == nil {
				result.Value = result.Output
			}
			if schema := r.outputSchemas[prepared.name]; schema != nil {
				if err := schema.Validate(result.Value); err != nil {
					info := &ErrorInfo{Name: "ToolOutputError", Code: "INVALID_TOOL_OUTPUT"}
					result = ToolResult{
						Output:  "Error: tool returned invalid output: " + err.Error(),
						IsError: true,
						Error:   info,
					}
				}
			}
		}
		for _, hook := range r.postHooks {
			var err error
			result, err = hook(ctx, execution, result)
			if err != nil {
				result = ToolResult{
					Output:  "Error: " + err.Error(),
					IsError: true,
					Error:   &ErrorInfo{Name: "ToolError", Code: "TOOL_EXECUTION_ERROR"},
				}
			}
		}
		if !result.IsError {
			if result.Value == nil {
				result.Value = result.Output
			}
			if schema := r.outputSchemas[prepared.name]; schema != nil {
				if err := schema.Validate(result.Value); err != nil {
					result = ToolResult{
						Output:  "Error: tool returned invalid output: " + err.Error(),
						IsError: true,
						Error:   &ErrorInfo{Name: "ToolOutputError", Code: "INVALID_TOOL_OUTPUT"},
					}
				}
			}
		}
		result = boundedResult(r, prepared.name, result)
		for _, hook := range r.resultHooks {
			hook(execution, result)
		}
		return result, nil
	}
	dispatch := func(dispatchCtx context.Context) (ToolResult, error) {
		var result ToolResult
		var err error
		if rich, ok := t.(ResultExecutor); ok {
			result, err = rich.ExecuteResult(dispatchCtx, prepared.args)
		} else {
			var raw string
			raw, err = t.Execute(dispatchCtx, prepared.args)
			if err == nil {
				result, err = normalizeToolValue(raw)
			}
		}
		return result, err
	}
	run := dispatch
	for i := len(r.executeHooks) - 1; i >= 0; i-- {
		next := run
		hook := r.executeHooks[i]
		run = func(runCtx context.Context) (ToolResult, error) {
			return hook(runCtx, execution, next)
		}
	}
	result, err := run(execCtx)
	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return finish(timeoutResult(prepared.timeout))
		}
		info := ErrorInfo{Name: "ToolError", Code: "TOOL_EXECUTION_ERROR"}
		if ctx.Err() != nil {
			info = ErrorInfo{Name: "AbortError", Code: "ABORTED"}
		}
		return finish(ToolResult{Output: "Error: " + err.Error(), IsError: true, Error: &info})
	}
	if execCtx.Err() == context.DeadlineExceeded {
		return finish(timeoutResult(prepared.timeout))
	}
	if ctx.Err() != nil {
		return finish(ToolResult{
			Output:  "Error: tool call aborted",
			IsError: true,
			Error:   &ErrorInfo{Name: "AbortError", Code: "ABORTED"},
		})
	}
	return finish(result)
}

func timeoutResult(timeout time.Duration) ToolResult {
	return ToolResult{
		Output:  fmt.Sprintf("Error: tool call timed out after %s", timeout),
		IsError: true,
		Error:   &ErrorInfo{Name: "ToolTimeoutError", Code: "TOOL_TIMEOUT"},
	}
}

// parseArguments mirrors dsh's model-argument normalization: empty input is an
// empty object, valid JSON is passed as parsed JSON for classification, and
// malformed JSON stays non-parallel and is rejected by Prepare.
func ParseArguments(args any) (any, error) {
	if args == nil {
		return map[string]any{}, nil
	}
	// The loop hands the exact parsed value from its single materialization
	// boundary back through classification and preparation. Preserve it by
	// identity; marshaling it again would create a second argument snapshot.
	switch args.(type) {
	case map[string]any, []any, string, bool, float64:
		return args, nil
	}
	if raw, ok := args.([]byte); ok {
		args = json.RawMessage(raw)
	}
	if raw, ok := args.(json.RawMessage); ok {
		if len(bytes.TrimSpace(raw)) == 0 {
			return map[string]any{}, nil
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		return parsed, nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func normalizeToolValue(raw any) (ToolResult, error) {
	if result, ok := raw.(ToolResult); ok {
		return result, nil
	}
	value := raw
	content := []llm.ContentBlock(nil)
	if rich, ok := raw.(ToolValue); ok {
		value = rich.Value
		content = rich.Content
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ToolResult{}, err
	}
	output := string(encoded)
	if text, ok := value.(string); ok {
		output = text
	}
	return ToolResult{Value: value, Output: output, Content: content}, nil
}

func boundedResult(r *Registry, name string, result ToolResult) ToolResult {
	bounded := r.applyOutputCap(name, result.Output)
	bounded.Value = result.Value
	if result.SpillPath != "" {
		bounded.SpillPath = result.SpillPath
		bounded.SpillBytes = result.SpillBytes
	}
	bounded.Content = result.Content
	bounded.IsError = result.IsError
	bounded.Error = result.Error
	return bounded
}

// applyOutputCap truncates oversized results and spills the full text. A spill
// failure is best-effort: it must never turn a successful tool call into an
// error, so the inline result is kept unchanged (mirrors dsh-spill-policy).
func (r *Registry) applyOutputCap(name, out string) ToolResult {
	r.spillMu.Lock()
	defer r.spillMu.Unlock()
	limit := r.policy.OutputLimit
	if limit <= 0 || len(out) <= limit {
		return ToolResult{Output: out}
	}
	store := &SpillStore{dir: r.policy.spillDir()}
	locator, err := store.Save(r.owner.SessionID, r.nextSeq(), out)
	if err != nil {
		return ToolResult{Output: out}
	}
	return truncateResult(out, locator, limit)
}

// nextSeq returns the spill sequence number: the bound session's next event
// seq when an owner is installed, else a per-registry counter.
func (r *Registry) nextSeq() uint64 {
	if r.owner.NextSeq != nil {
		return r.owner.NextSeq()
	}
	r.fallbackSeq++
	return r.fallbackSeq
}
