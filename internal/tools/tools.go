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
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/jabing/shutu-agent/internal/llm"
)

// Tool is the model-facing description of one capability. Execution is kept
// behind one of the structured executors below so the registry never exposes
// a raw string as its tool-result contract.
type Tool interface {
	Name() string
	// Description is a short human-readable summary of what the tool does. It
	// feeds the prompt's automatic tool catalog (design.md §7) and the
	// model-facing request schema.
	Description() string
	Schema() map[string]any // JSON Schema of the arguments; also sent to the model
}

// TextExecutor is the small adapter used by existing text-producing tools.
// The registry immediately wraps its output in ToolResult.
type TextExecutor interface {
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// StructuredExecutor is the dsh-aligned execution contract. Implementations
// can return multiple content blocks and an explicit model-visible error
// result without encoding either fact into a string.
type StructuredExecutor interface {
	ExecuteResult(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ConcurrencySafe is the opt-in dsh execution classifier. Tools must explicitly
// implement it and return true before sibling calls may overlap. Omission is
// exclusive, matching dsh's fail-closed concurrency contract.
type ConcurrencySafe interface {
	ConcurrencySafe(args json.RawMessage) bool
}

// ContentTool is an optional rich-result extension for tools such as
// read_image. Text-only tools continue to implement Tool unchanged.
type ContentTool interface {
	ExecuteContent(ctx context.Context, args json.RawMessage) ([]llm.ContentBlock, string, error)
}

// ToolResult is the structured outcome of one tool execution after the
// Execute pipeline has applied the timeout and output cap. Output is the
// model-facing text (the truncated head plus the locator notice when spilled);
// Content carries rich blocks, and IsError mirrors dsh's ToolResult error bit.
type ToolResult struct {
	Output     string
	SpillPath  string // non-empty => Output was truncated and the full text spilled
	SpillBytes int    // full output size in bytes (when spilled)
	Content    []llm.ContentBlock
	IsError    bool
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
	tools     map[string]Tool
	executors map[string]any
	schemas   map[string]*jsonschema.Schema
	policy    Policy
	owner     Owner
	// gate, when installed (M6d-2), is the optional pre-execution hook the
	// sensitive-tool approval runs through. The registry owns the hook so the
	// gate stays inside the Execute pipeline (policy lives here, never in the
	// loop, D4); the gate function itself is provided by the composition root
	// (cmd/pa). nil means no gating.
	gate func(ctx context.Context, name string, args json.RawMessage) error
	// fallbackSeq is used only when Owner.NextSeq is nil (a spill with no
	// bound session); it keeps spill filenames unique.
	fallbackSeq uint64
	spillMu     sync.Mutex
}

// IsConcurrencySafe reports whether a registered call may join a parallel
// group. The schema gate remains authoritative; this method only reads the
// optional classifier after the loop has received a model call.
func (r *Registry) IsConcurrencySafe(name string, args json.RawMessage) bool {
	t, ok := r.tools[name]
	if !ok {
		return false
	}
	classifier, ok := t.(ConcurrencySafe)
	if !ok {
		return false
	}
	return classifier.ConcurrencySafe(args)
}

// New returns a registry with the safe-by-default policy (M3): read-only
// whitelist, 30s deadline, 64KB output cap.
func New() *Registry {
	return &Registry{
		tools:     map[string]Tool{},
		executors: map[string]any{},
		schemas:   map[string]*jsonschema.Schema{},
		policy:    DefaultPolicy(),
	}
}

// Clone returns an independent registry view for a child scope. Tool
// implementations are intentionally shared, while the schema map, policy,
// owner and gate are copied so a scoped capability (for example dsh's
// structured_output tool) cannot mutate the parent registry.
func (r *Registry) Clone() *Registry {
	clone := &Registry{
		tools:       make(map[string]Tool, len(r.tools)),
		executors:   make(map[string]any, len(r.executors)),
		schemas:     make(map[string]*jsonschema.Schema, len(r.schemas)),
		policy:      r.policy,
		owner:       r.owner,
		gate:        r.gate,
		fallbackSeq: r.fallbackSeq,
	}
	clone.policy.Enabled = append([]string(nil), r.policy.Enabled...)
	for name, tool := range r.tools {
		clone.tools[name] = tool
	}
	for name, executor := range r.executors {
		clone.executors[name] = executor
	}
	for name, schema := range r.schemas {
		clone.schemas[name] = schema
	}
	return clone
}

// SetPolicy installs the Execute pipeline's safety policy (M3). The REPL
// installs the config-derived policy at startup.
func (r *Registry) SetPolicy(p Policy) { r.policy = p }

// SetOwner binds the registry to the active session for spill naming (M3).
func (r *Registry) SetOwner(o Owner) { r.owner = o }

// SetGate installs the optional pre-execution gate (M6d-2, ADR
// 2026-08-19-m6-agent-full.md 决策 M6d / dispatch-m6d-2 §4). When non-nil,
// Execute invokes gate before dispatching a whitelisted tool: a nil return lets
// the tool run, a non-nil return is the gate's verdict (denial or failure) and
// is returned verbatim (never dispatched). The gate runs with the caller's ctx
// — outside the per-tool deadline, because a human approval answer is an
// interactive read, not a tool computation. The gate function lives in cmd/pa
// (the composition root); the registry merely owns the hook so the loop's
// turn/step structure stays untouched (D4).
func (r *Registry) SetGate(gate func(ctx context.Context, name string, args json.RawMessage) error) {
	r.gate = gate
}

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
	if _, ok := t.(StructuredExecutor); !ok {
		if _, ok := t.(TextExecutor); !ok {
			return fmt.Errorf("tools: %q has no structured or text executor", t.Name())
		}
	}
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
	r.tools[t.Name()] = t
	r.executors[t.Name()] = t
	r.schemas[t.Name()] = sch
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

// Execute is the single execution gate. In order it rejects unknown names,
// enforces the M3 whitelist (未启用 ⇒ 拒绝执行), validates the arguments against
// the compiled JSON Schema (D7), runs the tool under a per-tool deadline, and
// applies the output cap (truncate + spill). All policy lives here, never in
// the loop.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	t, ok := r.tools[name]
	if !ok {
		return ToolResult{}, fmt.Errorf("tools: unknown tool %q", name)
	}
	if !r.policy.Allows(name) {
		return ToolResult{}, fmt.Errorf("tools: tool %q is not enabled (see tools.enabled)", name)
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return ToolResult{}, fmt.Errorf("tools: %s: invalid arguments JSON: %w", name, err)
	}
	if err := r.schemas[name].Validate(v); err != nil {
		return ToolResult{}, fmt.Errorf("tools: %s: invalid arguments: %w", name, err)
	}

	// M6d-2 sensitive-tool gate: runs after whitelist + D7 validation but
	// before the per-tool deadline is applied — the user's approval answer is
	// an interactive terminal read on the CLI serial path (D5), not a tool
	// computation, so it must not be bounded by tools.timeout. A non-nil return
	// is the gate's verdict and is returned verbatim (the tool never runs).
	if r.gate != nil {
		if err := r.gate(ctx, name, args); err != nil {
			return ToolResult{}, err
		}
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
	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var result ToolResult
	var err error
	executor := r.executors[name]
	if structured, ok := executor.(StructuredExecutor); ok {
		result, err = structured.ExecuteResult(execCtx, args)
	} else if rich, ok := t.(ContentTool); ok {
		result.Content, result.Output, err = rich.ExecuteContent(execCtx, args)
	} else {
		text, ok := executor.(TextExecutor)
		if !ok {
			return ToolResult{}, fmt.Errorf("tools: %s: executor is not structured", name)
		}
		result.Output, err = text.Execute(execCtx, args)
	}
	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return ToolResult{}, fmt.Errorf("tools: %s: timed out after %s: %w", name, timeout, err)
		}
		return ToolResult{}, err
	}
	bounded := r.applyOutputCap(name, result.Output)
	bounded.Content = result.Content
	bounded.IsError = result.IsError
	return bounded, nil
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
