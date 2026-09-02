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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
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

// OutputRenderer is the optional tool-owned renderer for a canonical output
// value.  The value remains the program-facing contract; the returned blocks
// are the model-facing projection.  Keeping this interface optional preserves
// compatibility with legacy text tools while allowing typed tools to match
// the reference value/render separation.
type OutputRenderer interface {
	RenderOutput(args any, value any) ([]llm.ContentBlock, error)
}

// OutputMetadata is the optional tool-owned presentation metadata hook.  It
// is evaluated only for direct registry executions and is refreshed when a
// post-execute policy replaces the canonical value.  Metadata is never used
// as the canonical value and is therefore safe to omit for legacy tools.
type OutputMetadata interface {
	PresentationMetadata(args any, value any) any
}

// OutputFinalizer is the optional last content-only projection hook. It runs
// exactly once after the execution and post-policy result has been
// normalized, including failures that bypass a successful value renderer.
// It cannot alter the canonical value, error classification, metadata, or
// deferred contexts.
type OutputFinalizer interface {
	FinalizeOutput(args any, result ToolResult) ([]llm.ContentBlock, error)
}

type Execution struct {
	CallID    string
	Name      string
	Arguments any
	Context   context.Context
}

// callIDContextKey carries the durable outer call id into tool implementations
// without widening Tool.Execute's long-standing signature. Code Mode uses it
// to give nested bindings the same parent:code:<n> identity as DSH.
type callIDContextKey struct{}

// WithCallID binds a tool call id to an execution context.
func WithCallID(ctx context.Context, callID string) context.Context {
	if callID == "" {
		return ctx
	}
	return context.WithValue(ctx, callIDContextKey{}, callID)
}

// CallIDFromContext returns the current durable tool call id, if one exists.
func CallIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	callID, _ := ctx.Value(callIDContextKey{}).(string)
	return callID
}

type PreToolDecision struct {
	Kind   string
	Reason string
}

type PreExecuteHook func(context.Context, Execution) (PreToolDecision, error)
type ExecuteHook func(context.Context, Execution, func(context.Context) (ToolResult, error)) (ToolResult, error)
type PostExecuteHook func(context.Context, Execution, ToolResult) (ToolResult, error)

// PostExecuteDecision is the explicit post-execute policy result used by the
// reference tool pipeline. An accept may replace either the JSON value or the
// rich content, but never both; a block turns the call into a policy failure
// and carries corrective feedback instead. Accepted deferred contexts retain
// tool order before decision order. Contexts are rich UserMessage values so
// source attribution survives the next model step.
type PostExecuteDecision struct {
	Kind  string
	Value any
	// ValueSet distinguishes an explicit JSON null replacement from an
	// omitted value. It mirrors the reference union's property-presence rule.
	ValueSet           bool
	Content            []llm.ContentBlock
	Feedback           []llm.ContentBlock
	AdditionalContexts []llm.Message
	ConcludesTurn      bool
}

// PostExecuteDecisionHook is a structured post-execute policy layer. It is
// separate from the legacy result-transform hook so callers cannot silently
// confuse “accepted value” with “blocked with feedback”.
type PostExecuteDecisionHook func(context.Context, Execution, ToolResult) (PostExecuteDecision, error)

// PostExecuteAroundHook is the composable post-execute middleware seam. It
// receives a continuation so multiple policy layers retain deterministic
// waterfall ordering while still being able to replace or block a result.
type PostExecuteAroundHook func(context.Context, Execution, ToolResult, func(context.Context, Execution, ToolResult) (ToolResult, error)) (ToolResult, error)
type ResultHook func(Execution, ToolResult)

var ErrToolNotRegistered = errors.New("tools: tool is not registered")

// StableCode values are the machine-readable tool-result taxonomy. Keeping
// these in one package prevents transports from inventing subtly different
// spellings for the same failure and lets replay/UI clients branch without
// parsing human-facing messages.
const (
	CodeUnknown               = "UNKNOWN"
	CodeUnknownTool           = "UNKNOWN_TOOL"
	CodeToolDenied            = "TOOL_DENIED"
	CodeInvalidArgs           = "INVALID_ARGS"
	CodeToolExecutionError    = "TOOL_EXECUTION_ERROR"
	CodeAbortedBeforeDispatch = "ABORTED_BEFORE_DISPATCH"
	CodeAborted               = "ABORTED"
	CodeStaleToolGeneration   = "STALE_TOOL_GENERATION"
	CodeInvalidToolOutput     = "INVALID_TOOL_OUTPUT"
	CodeToolPanic             = "TOOL_PANIC"
	CodeToolTimeout           = "TOOL_TIMEOUT"
)

// RegistrationInfo describes the owner of a model-facing tool definition.
// Empty fields are valid for legacy callers; scoped plugin runtimes should
// provide the plugin, generation and session that own the registration.
type RegistrationInfo struct {
	Owner      string `json:"owner,omitempty"`
	Plugin     string `json:"plugin,omitempty"`
	Generation uint64 `json:"generation"`
	SessionID  string `json:"sessionId,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// CatalogEntry is the generated, model-facing definition of one registered
// capability. Visibility and execution both derive from this same registry
// snapshot; a transport must not maintain a second handwritten tool list.
type CatalogEntry struct {
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	Parameters   map[string]any   `json:"parameters"`
	OutputSchema map[string]any   `json:"outputSchema"`
	Registration RegistrationInfo `json:"registration"`
	Provenance   string           `json:"provenance"`
	Profile      string           `json:"profile"`
	ErrorCodes   []string         `json:"errorCodes"`
	TimeoutMS    int64            `json:"timeoutMs"`
	Cancellable  bool             `json:"cancellable"`
	Events       []string         `json:"events"`
	Policy       CatalogPolicy    `json:"policy"`
	Visible      bool             `json:"visible"`
}

// CatalogPolicy is the registry-owned admission contract for one entry. The
// whitelist remains the execution authority; this generated projection lets a
// transport report and test the policy instead of reconstructing it from names.
type CatalogPolicy struct {
	Execution   string `json:"execution"`
	Approval    string `json:"approval"`
	Concurrency string `json:"concurrency"`
}

// CancellationAware is the explicit opt-in for a tool that observes the
// registry-supplied context and settles its owned work when it is cancelled.
// Registry timeout metadata must not claim cooperative cancellation merely
// because a context is passed to Execute.
type CancellationAware interface {
	CancellationAware() bool
}

// CatalogSnapshot is the stable machine-readable form shared by prompt,
// transport and release tooling. The registry remains the source of truth;
// serializing this snapshot avoids each adapter inventing its own catalog.
type CatalogSnapshot struct {
	SchemaVersion int            `json:"schemaVersion"`
	Tools         []CatalogEntry `json:"tools"`
}

// CatalogManifest is a verifiable snapshot for transport inventories and
// release artifacts. Digest is computed over the embedded canonical snapshot;
// Revision is the highest registration generation represented by Tools.
type CatalogManifest struct {
	CatalogSnapshot
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// ToolResult is the structured outcome of one tool execution after the
// Execute pipeline has applied the timeout and output cap. Output is the
// model-facing text (the truncated head plus the locator notice when spilled);
// Content carries rich blocks, and IsError mirrors dsh's ToolResult error bit.
type ToolResult struct {
	Value any
	// ValueSet distinguishes an explicit JSON null from an omitted value.
	// Ordinary legacy tools leave it false and use Output as their value.
	ValueSet   bool
	Output     string
	SpillPath  string // non-empty => Output was truncated and the full text spilled
	SpillBytes int    // full output size in bytes (when spilled)
	Content    []llm.ContentBlock
	Meta       any // provider/tool metadata retained on the durable result
	// AdditionalContexts is the pre-rich compatibility form used by older
	// embedders. New code should use AdditionalContextMessages: the reference
	// contract carries identified, source-attributed UserMessage values.
	AdditionalContexts        []string
	AdditionalContextMessages []llm.Message
	// ConcludesTurn is the successful terminal marker used by tools that own
	// the turn boundary. Normalization clears it from failures.
	ConcludesTurn bool
	IsError       bool
	Error         *ErrorInfo // structured failure classification, when IsError is true
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
		return llm.RedactDiagnostic(e.Message)
	}
	if e.Cause != nil {
		return llm.RedactDiagnostic(e.Cause.Error())
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
	return ErrorInfo{Name: "ToolError", Code: CodeUnknown}
}

// PreparedExecution is the ordered pre-dispatch portion of one call. It is
// intentionally opaque: callers may only pass it back to ExecutePrepared.
// Separating preparation from the body lets parallel-safe calls overlap while
// dsh's pre-execute/approval stage remains ordered.
type PreparedExecution struct {
	registry   *Registry
	callID     string
	name       string
	args       any
	timeout    time.Duration
	generation uint64
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
	mu             sync.RWMutex
	tools          map[string]Tool
	schemas        map[string]*jsonschema.Schema
	outputSchemas  map[string]*jsonschema.Schema
	registrations  map[string]RegistrationInfo
	nextGeneration uint64
	policy         Policy
	owner          Owner
	// gate, when installed (M6d-2), is the optional pre-execution hook the
	// sensitive-tool approval runs through. The registry owns the hook so the
	// gate stays inside the Execute pipeline (policy lives here, never in the
	// loop, D4); the gate function itself is provided by the composition root
	// (cmd/pa). nil means no gating.
	preHooks          []PreExecuteHook
	executeHooks      []ExecuteHook
	postHooks         []PostExecuteHook
	postDecisionHooks []PostExecuteDecisionHook
	postAroundHooks   []PostExecuteAroundHook
	resultHooks       []ResultHook
	// fallbackSeq is used only when Owner.NextSeq is nil (a spill with no
	// bound session); it keeps spill filenames unique.
	fallbackSeq uint64
	spillMu     sync.Mutex
}

// IsConcurrencySafe reports whether a registered call may join a parallel
// group. The caller must pass the already-materialized arguments from Prepare;
// re-materializing here would violate dsh's single parse boundary.
func (r *Registry) IsConcurrencySafe(name string, args any) (safe bool) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
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
		registrations: map[string]RegistrationInfo{},
		policy:        DefaultPolicy(),
	}
}

// Clone returns an independent registry view for a child scope. Tool
// implementations are intentionally shared, while the schema map, policy,
// owner and gate are copied so a scoped capability (for example dsh's
// structured_output tool) cannot mutate the parent registry.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := &Registry{
		tools:             make(map[string]Tool, len(r.tools)),
		schemas:           make(map[string]*jsonschema.Schema, len(r.schemas)),
		outputSchemas:     make(map[string]*jsonschema.Schema, len(r.outputSchemas)),
		registrations:     make(map[string]RegistrationInfo, len(r.registrations)),
		policy:            r.policy,
		owner:             r.owner,
		preHooks:          append([]PreExecuteHook(nil), r.preHooks...),
		executeHooks:      append([]ExecuteHook(nil), r.executeHooks...),
		postHooks:         append([]PostExecuteHook(nil), r.postHooks...),
		postDecisionHooks: append([]PostExecuteDecisionHook(nil), r.postDecisionHooks...),
		postAroundHooks:   append([]PostExecuteAroundHook(nil), r.postAroundHooks...),
		resultHooks:       append([]ResultHook(nil), r.resultHooks...),
		fallbackSeq:       r.fallbackSeq,
	}
	clone.policy.Enabled = append([]string(nil), r.policy.Enabled...)
	clone.policy.Profile = r.policy.Profile
	for name, tool := range r.tools {
		clone.tools[name] = tool
	}
	for name, schema := range r.schemas {
		clone.schemas[name] = schema
	}
	for name, schema := range r.outputSchemas {
		clone.outputSchemas[name] = schema
	}
	for name, registration := range r.registrations {
		clone.registrations[name] = registration
	}
	clone.nextGeneration = r.nextGeneration
	return clone
}

// SetPolicy installs the Execute pipeline's safety policy (M3). The REPL
// installs the config-derived policy at startup.
func (r *Registry) SetPolicy(p Policy) {
	p.Enabled = append([]string(nil), p.Enabled...)
	r.mu.Lock()
	r.policy = p
	r.mu.Unlock()
}

// Policy returns a detached copy of the current execution policy. Scoped
// runtimes use it as a base when narrowing the visible tool whitelist.
func (r *Registry) Policy() Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p := r.policy
	p.Enabled = append([]string(nil), p.Enabled...)
	return p
}

// SetOwner binds the registry to the active session for spill naming (M3).
func (r *Registry) SetOwner(o Owner) { r.mu.Lock(); r.owner = o; r.mu.Unlock() }

func (r *Registry) AddPreExecuteHook(hook PreExecuteHook) {
	r.mu.Lock()
	r.preHooks = append(r.preHooks, hook)
	r.mu.Unlock()
}
func (r *Registry) AddExecuteHook(hook ExecuteHook) {
	r.mu.Lock()
	r.executeHooks = append(r.executeHooks, hook)
	r.mu.Unlock()
}
func (r *Registry) AddPostExecuteHook(hook PostExecuteHook) {
	if hook == nil {
		return
	}
	r.mu.Lock()
	r.postHooks = append(r.postHooks, hook)
	r.mu.Unlock()
}
func (r *Registry) AddPostExecuteDecisionHook(hook PostExecuteDecisionHook) {
	if hook == nil {
		return
	}
	r.mu.Lock()
	r.postDecisionHooks = append(r.postDecisionHooks, hook)
	r.mu.Unlock()
}
func (r *Registry) AddPostExecuteAroundHook(hook PostExecuteAroundHook) {
	if hook == nil {
		return
	}
	r.mu.Lock()
	r.postAroundHooks = append(r.postAroundHooks, hook)
	r.mu.Unlock()
}
func (r *Registry) AddResultHook(hook ResultHook) {
	r.mu.Lock()
	r.resultHooks = append(r.resultHooks, hook)
	r.mu.Unlock()
}

// Allow adds names to the policy whitelist after it was installed. The
// composition root uses it for dynamically discovered tool names that cannot
// be known at config time — the MCP server tools bridged as mcp.<server>.<tool>
// (M6f-2 §4). Idempotent for names already whitelisted; empty names are
// ignored.
func (r *Registry) Allow(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	return r.RegisterWithInfo(t, RegistrationInfo{
		Owner:      "builtin",
		Plugin:     "builtin",
		Provenance: "builtin",
	})
}

// RegisterWithInfo registers a tool with explicit ownership metadata. The
// registry assigns a fresh generation when the caller leaves Generation zero.
// A generation is immutable for the lifetime of one registration.
func (r *Registry) RegisterWithInfo(t Tool, info RegistrationInfo) error {
	sch, outputSchema, err := compileToolSchemas(t)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name()]; ok {
		return fmt.Errorf("tools: tool %q already registered", t.Name())
	}
	if info.Generation == 0 {
		r.nextGeneration++
		info.Generation = r.nextGeneration
	} else if info.Generation > r.nextGeneration {
		r.nextGeneration = info.Generation
	}
	info = normalizedRegistrationInfo(info)
	r.tools[t.Name()] = t
	r.schemas[t.Name()] = sch
	r.outputSchemas[t.Name()] = outputSchema
	r.registrations[t.Name()] = info
	return nil
}

// RegisterOwned registers a tool and returns the disposer that removes the
// exact definition and its compiled schemas. Plugin scopes should retain this
// disposer in Scope.AddCleanup so hot unload cannot leave a stale model-facing
// tool behind.
func (r *Registry) RegisterOwned(t Tool) (func() error, error) {
	return r.RegisterOwnedWithInfo(t, RegistrationInfo{})
}

// RegisterOwnedWithInfo is RegisterOwned with an explicit registration owner.
func (r *Registry) RegisterOwnedWithInfo(t Tool, info RegistrationInfo) (func() error, error) {
	if err := r.RegisterWithInfo(t, info); err != nil {
		return nil, err
	}
	var once sync.Once
	var disposeErr error
	return func() error {
		once.Do(func() { disposeErr = r.Unregister(t.Name()) })
		return disposeErr
	}, nil
}

func compileToolSchemas(t Tool) (*jsonschema.Schema, *jsonschema.Schema, error) {
	if t == nil || t.Name() == "" {
		return nil, nil, errors.New("tools: tool name is required")
	}
	raw, err := json.Marshal(t.Schema())
	if err != nil {
		return nil, nil, fmt.Errorf("tools: marshal schema for %q: %w", t.Name(), err)
	}
	compiler := jsonschema.NewCompiler()
	url := "tool://" + t.Name()
	if err := compiler.AddResource(url, bytes.NewReader(raw)); err != nil {
		return nil, nil, fmt.Errorf("tools: add schema for %q: %w", t.Name(), err)
	}
	sch, err := compiler.Compile(url)
	if err != nil {
		return nil, nil, fmt.Errorf("tools: compile schema for %q: %w", t.Name(), err)
	}
	outputRaw, err := json.Marshal(t.OutputSchema())
	if err != nil {
		return nil, nil, fmt.Errorf("tools: marshal output schema for %q: %w", t.Name(), err)
	}
	outputURL := "tool-output://" + t.Name()
	if err := compiler.AddResource(outputURL, bytes.NewReader(outputRaw)); err != nil {
		return nil, nil, fmt.Errorf("tools: add output schema for %q: %w", t.Name(), err)
	}
	outputSchema, err := compiler.Compile(outputURL)
	if err != nil {
		return nil, nil, fmt.Errorf("tools: compile output schema for %q: %w", t.Name(), err)
	}
	return sch, outputSchema, nil
}

// ReplaceWithInfo publishes a new generation of an already-owned plugin tool.
// Ownership must match and generations must advance; otherwise a stale reload
// or cross-plugin collision cannot overwrite a live capability. Prepared calls
// from the old generation remain stale through the existing generation fence.
func (r *Registry) ReplaceWithInfo(t Tool, info RegistrationInfo) error {
	sch, outputSchema, err := compileToolSchemas(t)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.registrations[t.Name()]
	if !ok {
		return fmt.Errorf("tools: tool %q is not registered", t.Name())
	}
	if old.Plugin != info.Plugin || old.Owner != info.Owner {
		return fmt.Errorf("tools: tool %q is owned by %q/%q, not %q/%q", t.Name(), old.Owner, old.Plugin, info.Owner, info.Plugin)
	}
	if info.Generation <= old.Generation {
		return fmt.Errorf("tools: replacement generation for %q must be greater than %d", t.Name(), old.Generation)
	}
	if info.Generation > r.nextGeneration {
		r.nextGeneration = info.Generation
	}
	info = normalizedRegistrationInfo(info)
	r.tools[t.Name()] = t
	r.schemas[t.Name()] = sch
	r.outputSchemas[t.Name()] = outputSchema
	r.registrations[t.Name()] = info
	return nil
}

// RestoreWithInfo reinstates the exact previous plugin generation after a
// failed transactional reload. Unlike Replace, it intentionally permits a
// lower generation because rollback restores the currently published owner.
func (r *Registry) RestoreWithInfo(t Tool, info RegistrationInfo) error {
	sch, outputSchema, err := compileToolSchemas(t)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.registrations[t.Name()]
	if !ok {
		return fmt.Errorf("tools: tool %q is not registered", t.Name())
	}
	if old.Plugin != info.Plugin || old.Owner != info.Owner {
		return fmt.Errorf("tools: tool %q is owned by %q/%q, not %q/%q", t.Name(), old.Owner, old.Plugin, info.Owner, info.Plugin)
	}
	info = normalizedRegistrationInfo(info)
	r.tools[t.Name()] = t
	r.schemas[t.Name()] = sch
	r.outputSchemas[t.Name()] = outputSchema
	r.registrations[t.Name()] = info
	return nil
}

// Unregister removes a tool definition. It is idempotent to make scope
// disposal safe when a plugin explicitly unloads before its parent closes.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return nil
	}
	delete(r.tools, name)
	delete(r.schemas, name)
	delete(r.outputSchemas, name)
	delete(r.registrations, name)
	return nil
}

// Registration returns detached ownership metadata for a currently
// registered tool. The boolean is false when the tool is absent.
func (r *Registry) Registration(name string) (RegistrationInfo, bool) {
	r.mu.RLock()
	info, ok := r.registrations[name]
	r.mu.RUnlock()
	return info, ok
}

// RegistrationTool returns the current executable tool together with its
// ownership metadata. Plugin reload uses this to capture a transactional
// rollback target without exposing registry internals to tool authors.
func (r *Registry) RegistrationTool(name string) (Tool, RegistrationInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	if !ok {
		return nil, RegistrationInfo{}, false
	}
	return tool, r.registrations[name], true
}

// Specs returns the model-facing tool schemas, sorted by name for a stable
// prompt/request.
func (r *Registry) Specs() []llm.ToolSchema {
	catalog := r.Catalog()
	specs := make([]llm.ToolSchema, 0, len(catalog))
	for _, entry := range catalog {
		specs = append(specs, llm.ToolSchema{
			Name:        entry.Name,
			Description: entry.Description,
			Parameters:  entry.Parameters,
		})
	}
	return specs
}

// Catalog returns one generated snapshot of registration metadata, schemas,
// and policy visibility. It is the canonical source for prompt catalogs and
// transport capability reports.
func (r *Registry) Catalog() []CatalogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	catalog := make([]CatalogEntry, 0, len(names))
	for _, name := range names {
		tool := r.tools[name]
		registration := r.registrations[name]
		visible := r.policy.Allows(name)
		catalog = append(catalog, CatalogEntry{
			Name:         name,
			Description:  tool.Description(),
			Parameters:   cloneSchemaMap(tool.Schema()),
			OutputSchema: cloneSchemaMap(tool.OutputSchema()),
			Registration: catalogRegistration(registration),
			Provenance:   catalogProvenance(registration),
			Profile:      catalogProfile(r.policy),
			ErrorCodes:   canonicalToolErrorCodes(),
			TimeoutMS:    catalogTimeoutMS(effectiveToolTimeoutLocked(r.policy, name)),
			Cancellable:  catalogCancellable(tool),
			Events:       canonicalToolEvents(),
			Policy:       catalogPolicy(r.policy, tool, visible),
			Visible:      visible,
		})
	}
	return catalog
}

// CatalogJSON returns a deterministic JSON snapshot suitable for artifact
// generation, diagnostics and drift checks. Catalog entries are already sorted
// by name and all schema maps are detached from the registry.
func (r *Registry) CatalogJSON() ([]byte, error) {
	snapshot := CatalogSnapshot{SchemaVersion: 1, Tools: r.Catalog()}
	return json.MarshalIndent(snapshot, "", "  ")
}

// CatalogManifest returns one detached, integrity-checkable registry snapshot.
// The canonical bytes are sorted and schemas are detached, so repeated calls
// have identical digests unless registration or policy content changed.
func (r *Registry) CatalogManifest() (CatalogManifest, error) {
	return NewCatalogManifest(r.Catalog())
}

// NewCatalogManifest computes the release manifest for an already captured
// canonical snapshot. Callers that need one consistent registry read should
// prefer Registry.CatalogManifest.
func NewCatalogManifest(entries []CatalogEntry) (CatalogManifest, error) {
	snapshot := CatalogSnapshot{SchemaVersion: 1, Tools: entries}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return CatalogManifest{}, fmt.Errorf("tools: encode catalog snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	var revision uint64
	for _, entry := range entries {
		if entry.Registration.Generation > revision {
			revision = entry.Registration.Generation
		}
	}
	return CatalogManifest{
		CatalogSnapshot: snapshot,
		Revision:        revision,
		Digest:          hex.EncodeToString(sum[:]),
	}, nil
}

// ValidateCatalogManifest recomputes and compares the embedded snapshot. A
// changed schema version, tool payload, revision, or digest fails closed.
func ValidateCatalogManifest(manifest CatalogManifest) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("tools: unsupported catalog schema version %d", manifest.SchemaVersion)
	}
	if manifest.Digest == "" {
		return errors.New("tools: catalog digest is required")
	}
	expected, err := NewCatalogManifest(manifest.Tools)
	if err != nil {
		return err
	}
	if manifest.Revision != expected.Revision {
		return fmt.Errorf("tools: catalog revision drift: got %d, want %d", manifest.Revision, expected.Revision)
	}
	if manifest.Digest != expected.Digest {
		return fmt.Errorf("tools: catalog digest drift: got %s, want %s", manifest.Digest, expected.Digest)
	}
	return nil
}

// Names returns the manifest's stable tool-name projection.
func (m CatalogManifest) Names() []string {
	out := make([]string, 0, len(m.Tools))
	for _, entry := range m.Tools {
		out = append(out, entry.Name)
	}
	return out
}

// VisibleSpecs projects only tools enabled by the registry policy. It is
// derived from Catalog so model visibility and Execute admission consume the
// same generated definition.
func (r *Registry) VisibleSpecs() []llm.ToolSchema {
	catalog := r.Catalog()
	out := make([]llm.ToolSchema, 0, len(catalog))
	for _, entry := range catalog {
		if entry.Visible {
			out = append(out, llm.ToolSchema{
				Name:        entry.Name,
				Description: entry.Description,
				Parameters:  entry.Parameters,
			})
		}
	}
	return out
}

// ValidateProjection fail-closes a transport inventory against the current
// canonical catalog. It is intended for adapter conformance tests and direct
// capability reports; model-facing schemas continue to come from
// VisibleSpecs/Catalog so the normal paths cannot invent a definition.
func (r *Registry) ValidateProjection(profile string, specs []llm.ToolSchema) error {
	allowed := make(map[string]CatalogEntry)
	for _, entry := range r.Catalog() {
		if entry.Visible && entry.Profile == profile {
			allowed[entry.Name] = entry
		}
	}
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if seen[spec.Name] {
			return fmt.Errorf("tools: projection repeats tool %q", spec.Name)
		}
		seen[spec.Name] = true
		entry, ok := allowed[spec.Name]
		if !ok {
			return fmt.Errorf("tools: projection exposes tool %q outside canonical catalog profile %q", spec.Name, profile)
		}
		if spec.Description != entry.Description {
			return fmt.Errorf("tools: projected description drift for %q", spec.Name)
		}
		if !reflect.DeepEqual(spec.Parameters, entry.Parameters) {
			return fmt.Errorf("tools: projected schema drift for %q", spec.Name)
		}
	}
	return nil
}

func catalogRegistration(info RegistrationInfo) RegistrationInfo {
	if info.Owner == "" {
		if info.Plugin == "" {
			info.Owner = "builtin"
		} else {
			info.Owner = info.Plugin
		}
	}
	return info
}

func catalogProvenance(info RegistrationInfo) string {
	if info.Provenance != "" {
		return info.Provenance
	}
	switch {
	case info.Plugin != "":
		return "plugin"
	case info.Owner == "" || info.Owner == "builtin":
		return "builtin"
	default:
		return "external"
	}
}

func normalizedRegistrationInfo(info RegistrationInfo) RegistrationInfo {
	if info.Provenance != "" {
		return info
	}
	switch {
	case info.Plugin == "builtin":
		info.Provenance = "builtin"
	case info.Plugin != "":
		info.Provenance = "plugin"
	case info.Owner != "":
		info.Provenance = "external"
	default:
		info.Owner = "builtin"
		info.Plugin = "builtin"
		info.Provenance = "builtin"
	}
	return info
}

func catalogProfile(policy Policy) string {
	if policy.Profile == "" {
		return "standard"
	}
	return policy.Profile
}

func canonicalToolErrorCodes() []string {
	return []string{
		CodeUnknownTool,
		CodeToolDenied,
		CodeInvalidArgs,
		CodeAbortedBeforeDispatch,
		CodeAborted,
		CodeStaleToolGeneration,
		CodeInvalidToolOutput,
		CodeToolPanic,
		CodeToolTimeout,
		CodeToolExecutionError,
		CodeUnknown,
	}
}

func effectiveToolTimeoutLocked(policy Policy, name string) time.Duration {
	switch name {
	case runCommandName:
		if policy.RunCommand.Timeout > 0 {
			return policy.RunCommand.Timeout
		}
		return DefaultRunCommandTimeout
	case codeRunToolName:
		if policy.CodeRun.Timeout > 0 {
			return policy.CodeRun.Timeout
		}
	}
	return policy.Timeout
}

func catalogTimeoutMS(timeout time.Duration) int64 {
	if timeout <= 0 {
		return 0
	}
	return int64(timeout / time.Millisecond)
}

func catalogCancellable(tool Tool) bool {
	aware, ok := tool.(CancellationAware)
	return ok && aware.CancellationAware()
}

func canonicalToolEvents() []string {
	return []string{"tool/call", "tool/result"}
}

func catalogPolicy(policy Policy, tool Tool, visible bool) CatalogPolicy {
	contract := CatalogPolicy{
		Execution:   "denied",
		Approval:    "registry-whitelist",
		Concurrency: "exclusive",
	}
	if visible {
		contract.Execution = "allowed"
	}
	if _, ok := tool.(ConcurrencySafe); ok {
		contract.Concurrency = "argument-dependent"
	}
	return contract
}

func cloneSchemaMap(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		return schema
	}
	return clone
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
	return r.ExecuteWithCallID(ctx, "", name, args)
}

// ExecuteWithCallID is the correlated execution entry point used by nested
// transports such as Code Mode. Keeping the call id at the registry boundary
// ensures pre/around/post/result hooks and spill ownership observe the same
// deterministic identity that the durable dispatch event records.
func (r *Registry) ExecuteWithCallID(ctx context.Context, callID, name string, args any) (ToolResult, error) {
	prepared, err := r.Prepare(ctx, callID, name, args)
	if err != nil {
		// DSH publishes one terminal tools/result even when the call cannot
		// enter the prepared pipeline. Keep the Go error API for existing
		// callers, but make the observer surface complete and deterministic.
		r.notifyFailure(ctx, callID, name, args, err)
		return ToolResult{}, err
	}
	return r.ExecutePrepared(ctx, prepared)
}

// Prepare performs the ordered pre-dispatch stage: name/policy lookup, JSON
// argument validation and the approval gate. It never invokes the tool body.
// Callers may safely run this stage serially before dispatching a parallel-safe
// group, matching dsh's pre-execute ordering.
func (r *Registry) Prepare(ctx context.Context, callID, name string, args any) (*PreparedExecution, error) {
	r.mu.RLock()
	_, ok := r.tools[name]
	policy := r.policy
	sch := r.schemas[name]
	registration := r.registrations[name]
	preHooks := append([]PreExecuteHook(nil), r.preHooks...)
	r.mu.RUnlock()
	v, err := ParseArguments(args)
	if err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolArgsError", Code: CodeInvalidArgs}, Message: fmt.Sprintf("invalid arguments: %v", err), Cause: err}
	}
	v, err = snapshotArguments(v)
	if err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolArgsError", Code: CodeInvalidArgs}, Message: fmt.Sprintf("invalid arguments: %v", err), Cause: err}
	}
	// DSH materializes arguments before any policy or dispatch phase. Once
	// materialization succeeds, a caller that is already cancelled must settle
	// at the pre-dispatch boundary: approval/pre-hooks must not observe a call
	// that cannot be dispatched, and cancellation takes precedence over the
	// later policy/schema classifications. Keep unknown-tool resolution after
	// this boundary as well, matching the reference's single materialization
	// path for every invocation.
	if err := ctx.Err(); err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "AbortError", Code: CodeAbortedBeforeDispatch}, Message: "tool call aborted before dispatch", Cause: err}
	}
	if !ok {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolNotFoundError", Code: CodeUnknownTool}, Message: fmt.Sprintf("unknown tool %q", name)}
	}
	if !policy.Allows(name) {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolPolicyError", Code: CodeToolDenied}, Message: fmt.Sprintf("tool %q is not enabled (see tools.enabled)", name)}
	}
	if err := sch.Validate(v); err != nil {
		return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolArgsError", Code: CodeInvalidArgs}, Message: fmt.Sprintf("invalid arguments: %v", err), Cause: err}
	}

	// M6d-2 sensitive-tool gate: runs after whitelist + D7 validation but
	// before the per-tool deadline is applied — the user's approval answer is
	// an interactive terminal read on the CLI serial path (D5), not a tool
	// computation, so it must not be bounded by tools.timeout. A non-nil return
	// is the gate's verdict and is returned verbatim (the tool never runs).
	execution := Execution{CallID: callID, Name: name, Arguments: v, Context: ctx}
	for _, hook := range preHooks {
		decision, err := runPreHookSafely(hook, ctx, execution)
		if err != nil {
			info := ErrorInfoOf(err)
			if info.Code == CodeUnknown {
				info = ErrorInfo{Name: "ToolError", Code: CodeToolExecutionError}
			}
			return nil, &ExecutionError{
				Info:    info,
				Message: llm.RedactDiagnostic(err.Error()),
				Cause:   err,
			}
		}
		if decision.Kind == "deny" || decision.Kind == "ask" {
			reason := decision.Reason
			if reason == "" {
				reason = "tool execution denied"
			}
			return nil, &ExecutionError{Info: ErrorInfo{Name: "ToolPolicyError", Code: CodeToolDenied}, Message: reason}
		}
	}
	timeout := policy.Timeout
	if name == runCommandName {
		timeout = policy.RunCommand.Timeout
		if timeout <= 0 {
			timeout = DefaultRunCommandTimeout
		}
	}
	if name == codeRunToolName && policy.CodeRun.Timeout > 0 {
		timeout = policy.CodeRun.Timeout
	}
	return &PreparedExecution{registry: r, callID: callID, name: name, args: v, timeout: timeout, generation: registration.Generation}, nil
}

// ExecutePrepared dispatches a previously prepared call. Preparation is
// deliberately separate so approval and policy do not run concurrently with
// sibling calls, while tool bodies may still overlap.
func (r *Registry) ExecutePrepared(ctx context.Context, prepared *PreparedExecution) (ToolResult, error) {
	if prepared == nil || prepared.registry != r {
		return ToolResult{}, &ExecutionError{Info: ErrorInfo{Name: "ToolExecutionError", Code: CodeUnknown}, Message: "invalid prepared tool execution"}
	}
	r.mu.RLock()
	t, ok := r.tools[prepared.name]
	registration := r.registrations[prepared.name]
	outputSchema := r.outputSchemas[prepared.name]
	postHooks := append([]PostExecuteHook(nil), r.postHooks...)
	postDecisionHooks := append([]PostExecuteDecisionHook(nil), r.postDecisionHooks...)
	postAroundHooks := append([]PostExecuteAroundHook(nil), r.postAroundHooks...)
	executeHooks := append([]ExecuteHook(nil), r.executeHooks...)
	resultHooks := append([]ResultHook(nil), r.resultHooks...)
	r.mu.RUnlock()
	if !ok {
		err := &ExecutionError{Info: ErrorInfo{Name: "ToolNotFoundError", Code: CodeUnknownTool}, Message: fmt.Sprintf("unknown tool %q", prepared.name)}
		r.notifyPreparedFailure(ctx, prepared, err)
		return ToolResult{}, err
	}
	if prepared.generation != 0 && registration.Generation != prepared.generation {
		err := &ExecutionError{Info: ErrorInfo{Name: "ToolGenerationError", Code: CodeStaleToolGeneration}, Message: fmt.Sprintf("tool %q registration changed before dispatch", prepared.name)}
		r.notifyPreparedFailure(ctx, prepared, err)
		return ToolResult{}, err
	}
	// Preparation and dispatch are intentionally separate so a caller can
	// authorize/prepare a whole sibling group before running the bodies. The
	// cancellation boundary must therefore be checked again here: a context
	// may have been cancelled after Prepare returned but before this call got a
	// dispatch slot. Starting the body in that window would create an external
	// side effect after the DSH pre-dispatch abort boundary.
	if err := ctx.Err(); err != nil {
		err := &ExecutionError{
			Info:    ErrorInfo{Name: "AbortError", Code: CodeAbortedBeforeDispatch},
			Message: "tool call aborted before dispatch",
			Cause:   err,
		}
		r.notifyPreparedFailure(ctx, prepared, err)
		return ToolResult{}, err
	}
	execCtx := ctx
	if prepared.timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, prepared.timeout)
		defer cancel()
	}
	execCtx = WithCallID(execCtx, prepared.callID)

	execution := Execution{CallID: prepared.callID, Name: prepared.name, Arguments: prepared.args, Context: execCtx}
	finish := func(result ToolResult) (ToolResult, error) {
		if err := validateContentBlocks(result.Content); err != nil {
			result = toolFailurePreservingContexts(result, "Error: tool returned invalid content: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolContentError", Code: CodeInvalidToolOutput})
		}
		if !result.IsError {
			if result.Value == nil && !result.ValueSet {
				result.Value = result.Output
			}
			if outputSchema != nil {
				if err := outputSchema.Validate(result.Value); err != nil {
					info := &ErrorInfo{Name: "ToolOutputError", Code: CodeInvalidToolOutput}
					result = toolFailurePreservingContexts(result, "Error: tool returned invalid output: "+llm.RedactDiagnostic(err.Error()), info)
				}
			}
		}
		if !result.IsError {
			if err := materializeOutputProjection(t, prepared.args, &result, false); err != nil {
				result = toolFailurePreservingContexts(result, "Error: tool output rendering failed: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolRenderError", Code: CodeInvalidToolOutput})
			}
		}
		if len(postAroundHooks) > 0 {
			base := func(_ context.Context, _ Execution, current ToolResult) (ToolResult, error) {
				return current, nil
			}
			next := base
			for i := len(postAroundHooks) - 1; i >= 0; i-- {
				hook := postAroundHooks[i]
				downstream := next
				next = func(h PostExecuteAroundHook, downstream func(context.Context, Execution, ToolResult) (ToolResult, error)) func(context.Context, Execution, ToolResult) (ToolResult, error) {
					return func(hctx context.Context, current Execution, value ToolResult) (ToolResult, error) {
						return runPostAroundHookSafely(h, hctx, current, value, downstream)
					}
				}(hook, downstream)
			}
			beforeAround := result
			var err error
			var aroundResult ToolResult
			aroundResult, err = next(ctx, execution, result)
			if err != nil {
				info := ErrorInfoOf(err)
				if info.Code == CodeUnknown {
					info = ErrorInfo{Name: "ToolError", Code: CodeToolExecutionError}
				}
				result = toolFailurePreservingContexts(beforeAround, "Error: "+llm.RedactDiagnostic(err.Error()), &info)
			} else {
				result = aroundResult
			}
		}
		for _, hook := range postHooks {
			var err error
			var postResult ToolResult
			postResult, err = runPostHookSafely(hook, ctx, execution, result)
			if err != nil {
				info := ErrorInfoOf(err)
				if info.Code == CodeUnknown {
					info = ErrorInfo{Name: "ToolError", Code: CodeToolExecutionError}
				}
				result = toolFailurePreservingContexts(result, "Error: "+llm.RedactDiagnostic(err.Error()), &info)
			} else {
				result = postResult
			}
		}
		for _, hook := range postDecisionHooks {
			decision, err := runPostDecisionHookSafely(hook, ctx, execution, result)
			if err != nil {
				info := ErrorInfoOf(err)
				if info.Code == CodeUnknown {
					info = ErrorInfo{Name: "ToolError", Code: CodeToolExecutionError}
				}
				result = toolFailurePreservingContexts(result, "Error: "+llm.RedactDiagnostic(err.Error()), &info)
				break
			}
			var decisionErr error
			result, decisionErr = applyPostExecuteDecision(result, decision)
			if decisionErr != nil {
				result = toolFailurePreservingContexts(result, "Error: "+llm.RedactDiagnostic(decisionErr.Error()), &ErrorInfo{Name: "ToolDecisionError", Code: CodeInvalidToolOutput})
				break
			}
			if !result.IsError && decisionReplacesValue(decision) {
				if err := materializeOutputProjection(t, prepared.args, &result, true); err != nil {
					result = toolFailurePreservingContexts(result, "Error: tool output rendering failed: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolRenderError", Code: CodeInvalidToolOutput})
					break
				}
			}
			if result.IsError {
				break
			}
		}
		if !result.IsError {
			if err := validateContentBlocks(result.Content); err != nil {
				result = toolFailurePreservingContexts(result, "Error: tool returned invalid content: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolContentError", Code: CodeInvalidToolOutput})
			}
		}
		if !result.IsError {
			if err := materializeOutputProjection(t, prepared.args, &result, false); err != nil {
				result = toolFailurePreservingContexts(result, "Error: tool output rendering failed: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolRenderError", Code: CodeInvalidToolOutput})
			}
		}
		if finalizer, ok := t.(OutputFinalizer); ok {
			content, err := runOutputFinalizerSafely(finalizer, prepared.args, result)
			if err != nil {
				result = toolFailurePreservingContexts(result, "Error: tool output finalization failed: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolRenderError", Code: CodeInvalidToolOutput})
			} else {
				result.Content = content
			}
		}
		if !result.IsError {
			if result.Value == nil && !result.ValueSet {
				result.Value = result.Output
			}
			if outputSchema != nil {
				if err := outputSchema.Validate(result.Value); err != nil {
					result = toolFailurePreservingContexts(result, "Error: tool returned invalid output: "+llm.RedactDiagnostic(err.Error()), &ErrorInfo{Name: "ToolOutputError", Code: CodeInvalidToolOutput})
				}
			}
		}
		result = boundedResult(r, prepared.name, result)
		for _, hook := range resultHooks {
			// Result hooks are observers. A faulty observer must not change the
			// already-normalized result or skip the loop's durable tool/result
			// commit.
			runResultHookSafely(hook, execution, result)
		}
		return result, nil
	}
	bodyStarted := false
	dispatch := func(dispatchCtx context.Context) (ToolResult, error) {
		bodyStarted = true
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
	for i := len(executeHooks) - 1; i >= 0; i-- {
		next := run
		hook := executeHooks[i]
		run = func(runCtx context.Context) (ToolResult, error) {
			return hook(runCtx, execution, next)
		}
	}
	result, err := runSafely(run, execCtx)
	if err != nil {
		info := ErrorInfoOf(err)
		// A typed tool/wrapper failure is the settlement authority. DSH only
		// substitutes an abort/timeout for a plain cancellation outcome; it
		// must not erase a more specific failure merely because the caller
		// cancelled while that failure was unwinding.
		if info.Code != CodeUnknown {
			failure := toolFailureFromDispatch(result, "Error: "+llm.RedactDiagnostic(err.Error()), &info)
			return finish(failure)
		}
		if execCtx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
			return finish(timeoutResult(prepared.timeout))
		}
		if errors.Is(err, context.Canceled) {
			code := CodeAborted
			message := "tool call aborted"
			if !bodyStarted {
				code = CodeAbortedBeforeDispatch
				message = "tool call aborted before dispatch"
			}
			failure := toolFailureFromDispatch(result, "Error: "+message, &ErrorInfo{Name: "AbortError", Code: code})
			return finish(failure)
		}
		info = ErrorInfo{Name: "ToolError", Code: CodeToolExecutionError}
		// ResultExecutor and execute hooks may have produced deferred context
		// or a rich diagnostic before returning an error. The error is the
		// settlement signal, but dropping the partial result would lose the
		// reference pipeline's next-step context. Preserve only validated
		// presentation/context metadata; canonical values from a failed call
		// must not become successful model output.
		failure := toolFailureFromDispatch(result, "Error: "+llm.RedactDiagnostic(err.Error()), &info)
		return finish(failure)
	}
	if execCtx.Err() == context.DeadlineExceeded {
		return finish(timeoutResult(prepared.timeout))
	}
	if ctx.Err() != nil {
		// Cancellation can overtake a wrapper/post phase after the body has
		// already settled. DSH replaces only a successful outcome in that case,
		// preserving deferred contexts; a wrapper that short-circuits before the
		// body gets the stronger before-dispatch classification. Tool-owned
		// failures remain authoritative even if the caller cancels while they
		// settle.
		if result.IsError {
			return finish(result)
		}
		code := CodeAborted
		message := "tool call aborted"
		if !bodyStarted {
			code = CodeAbortedBeforeDispatch
			message = "tool call aborted before dispatch"
		}
		return finish(toolFailurePreservingContexts(result, "Error: "+message, &ErrorInfo{Name: "AbortError", Code: code}))
	}
	return finish(result)
}

func toolFailurePreservingContexts(previous ToolResult, output string, info *ErrorInfo) ToolResult {
	return ToolResult{
		Output: output, Meta: previous.Meta,
		AdditionalContexts:        append([]string(nil), previous.AdditionalContexts...),
		AdditionalContextMessages: cloneContextMessages(previous.AdditionalContextMessages),
		IsError:                   true, Error: info,
	}
}

func toolFailureFromDispatch(previous ToolResult, output string, info *ErrorInfo) ToolResult {
	failure := toolFailurePreservingContexts(previous, output, info)
	if err := validateContentBlocks(previous.Content); err == nil {
		failure.Content = append([]llm.ContentBlock(nil), previous.Content...)
	}
	return failure
}

// validateContentBlocks is the registry-side boundary for rich tool output.
// A tool may return arbitrary JSON in Value, but model-visible Content is a
// tagged union shared with session history and provider adapters. Rejecting an
// unknown tag here prevents one capability from smuggling a shape that a
// downstream projection silently rewrites or drops.
func validateContentBlocks(blocks []llm.ContentBlock) error {
	for index, block := range blocks {
		switch block.Kind {
		case llm.BlockText, llm.BlockReasoning:
			// Empty text is valid for a streaming/projection chunk.
		case llm.BlockImage:
			if block.Image.ID == "" && block.Image.Path == "" {
				return fmt.Errorf("content[%d] image is missing an attachment reference", index)
			}
		case llm.BlockToolCall:
			if block.CallID == "" || block.Name == "" {
				return fmt.Errorf("content[%d] tool-call requires callId and name", index)
			}
		case llm.BlockToolResult:
			if block.CallID == "" {
				return fmt.Errorf("content[%d] tool-result requires callId", index)
			}
			if err := validateContentBlocks(block.Blocks); err != nil {
				return fmt.Errorf("content[%d] tool-result: %w", index, err)
			}
		default:
			// DSH's ContentBlock map is merge-extensible. An unknown kind is a
			// valid rich block only when its original wire representation was
			// retained; stripping Raw would turn audio/resource/vendor data into
			// an invented text-only result before durable replay.
			if len(block.Raw) == 0 {
				return fmt.Errorf("content[%d] has unsupported block kind %q", index, block.Kind)
			}
		}
	}
	return nil
}

func runPreHookSafely(hook PreExecuteHook, ctx context.Context, execution Execution) (decision PreToolDecision, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &ExecutionError{Info: ErrorInfo{Name: "ToolHookPanicError", Code: CodeToolPanic}, Message: llm.RedactDiagnostic(fmt.Sprintf("pre-execute hook panicked: %v", recovered))}
		}
	}()
	return hook(ctx, execution)
}

func runPostHookSafely(hook PostExecuteHook, ctx context.Context, execution Execution, result ToolResult) (out ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &ExecutionError{Info: ErrorInfo{Name: "ToolHookPanicError", Code: CodeToolPanic}, Message: llm.RedactDiagnostic(fmt.Sprintf("post-execute hook panicked: %v", recovered))}
		}
	}()
	return hook(ctx, execution, result)
}

func runPostAroundHookSafely(hook PostExecuteAroundHook, ctx context.Context, execution Execution, result ToolResult, next func(context.Context, Execution, ToolResult) (ToolResult, error)) (out ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &ExecutionError{Info: ErrorInfo{Name: "ToolHookPanicError", Code: CodeToolPanic}, Message: llm.RedactDiagnostic(fmt.Sprintf("post-execute hook panicked: %v", recovered))}
		}
	}()
	return hook(ctx, execution, result, next)
}

func runPostDecisionHookSafely(hook PostExecuteDecisionHook, ctx context.Context, execution Execution, result ToolResult) (decision PostExecuteDecision, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &ExecutionError{Info: ErrorInfo{Name: "ToolHookPanicError", Code: CodeToolPanic}, Message: llm.RedactDiagnostic(fmt.Sprintf("post-execute decision hook panicked: %v", recovered))}
		}
	}()
	return hook(ctx, execution, result)
}

func applyPostExecuteDecision(result ToolResult, decision PostExecuteDecision) (ToolResult, error) {
	kind := decision.Kind
	if kind == "" {
		kind = "accept"
	}
	switch kind {
	case "accept":
		hasValue := decision.ValueSet || decision.Value != nil
		hasContent := decision.Content != nil || len(decision.Content) > 0
		if result.IsError && (hasValue || hasContent) {
			return result, errors.New("an accepted decision cannot replace a failed tool result")
		}
		if hasValue && hasContent {
			return result, errors.New("an accepted decision cannot provide both value and content")
		}
		if err := validateContentBlocks(decision.Content); err != nil {
			return result, fmt.Errorf("invalid accepted content: %w", err)
		}
		if hasValue {
			result.Value = decision.Value
			result.ValueSet = true
			if rendered, ok := decision.Value.(string); ok {
				result.Output = rendered
			} else if encoded, err := json.Marshal(decision.Value); err == nil {
				result.Output = string(encoded)
			}
		}
		if len(decision.Content) > 0 {
			result.Content = append([]llm.ContentBlock(nil), decision.Content...)
		}
		// The tool's deferred contexts were produced first; the policy layer's
		// contexts are appended in the same post-execute order as the reference
		// waterfall.
		result.AdditionalContextMessages = append(cloneContextMessages(result.AdditionalContextMessages), cloneContextMessages(decision.AdditionalContexts)...)
		result.ConcludesTurn = decision.ConcludesTurn || result.ConcludesTurn
		return result, nil
	case "block":
		feedback := append([]llm.ContentBlock(nil), decision.Feedback...)
		if err := validateContentBlocks(feedback); err != nil {
			return result, fmt.Errorf("invalid blocked feedback: %w", err)
		}
		output := "Error: tool execution blocked by post-execute policy"
		if text := contentBlocksText(feedback); text != "" {
			output = text
		}
		return ToolResult{
			Output: output, Content: feedback, IsError: true,
			Error:                     &ErrorInfo{Name: "ToolPolicyError", Code: CodeToolDenied},
			AdditionalContextMessages: cloneContextMessages(decision.AdditionalContexts),
		}, nil
	default:
		return result, fmt.Errorf("unknown post-execute decision kind %q", decision.Kind)
	}
}

func decisionReplacesValue(decision PostExecuteDecision) bool {
	kind := decision.Kind
	if kind == "" {
		kind = "accept"
	}
	return kind == "accept" && (decision.ValueSet || decision.Value != nil)
}

// materializeOutputProjection runs the optional tool-owned value renderer and
// metadata hook.  Rendering is deliberately fail-closed: a renderer panic or
// error is an invalid tool output, not an empty successful result.  A content
// decision is authoritative, so non-forced calls preserve an already supplied
// content projection.
func materializeOutputProjection(t Tool, args any, result *ToolResult, force bool) (err error) {
	if result == nil || result.IsError {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("renderer panicked: %v", recovered)
		}
	}()
	if renderer, ok := t.(OutputRenderer); ok && (force || result.Content == nil) {
		content, renderErr := renderer.RenderOutput(args, result.Value)
		if renderErr != nil {
			return renderErr
		}
		if err := validateContentBlocks(content); err != nil {
			return err
		}
		result.Content = content
	}
	if metadata, ok := t.(OutputMetadata); ok {
		result.Meta = metadata.PresentationMetadata(args, result.Value)
	}
	return nil
}

func runOutputFinalizerSafely(finalizer OutputFinalizer, args any, result ToolResult) (content []llm.ContentBlock, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("finalizer panicked: %v", recovered)
		}
	}()
	content, err = finalizer.FinalizeOutput(args, result)
	if err != nil {
		return nil, err
	}
	if err := validateContentBlocks(content); err != nil {
		return nil, err
	}
	return content, nil
}

func contentBlocksText(blocks []llm.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Kind == llm.BlockText {
			parts = append(parts, block.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[%s content]", block.Kind))
		}
	}
	return strings.Join(parts, "\n")
}

func runResultHookSafely(hook ResultHook, execution Execution, result ToolResult) {
	defer func() { _ = recover() }()
	hook(execution, result)
}

// notifyFailure closes the observer contract for failures raised by Prepare.
// Prepare intentionally remains a staged API and therefore does not publish
// on its own; ExecuteWithCallID calls this method after that stage fails. A
// second lossless snapshot is best-effort for the observer only. If the
// original value was not materializable, DSH exposes an undefined argument
// value rather than retrying the getter or inventing a partial object.
func (r *Registry) notifyFailure(ctx context.Context, callID, name string, args any, err error) {
	var detached any
	if parsed, parseErr := ParseArguments(args); parseErr == nil {
		detached, _ = snapshotArguments(parsed)
	}
	r.notifyFailureWithArgs(ctx, Execution{CallID: callID, Name: name, Arguments: detached, Context: ctx}, err)
}

func (r *Registry) notifyPreparedFailure(ctx context.Context, prepared *PreparedExecution, err error) {
	if prepared == nil {
		return
	}
	r.notifyFailureWithArgs(ctx, Execution{
		CallID: prepared.callID, Name: prepared.name, Arguments: prepared.args, Context: ctx,
	}, err)
}

func (r *Registry) notifyFailureWithArgs(ctx context.Context, execution Execution, err error) {
	if ctx == nil {
		execution.Context = context.Background()
	}
	r.mu.RLock()
	hooks := append([]ResultHook(nil), r.resultHooks...)
	r.mu.RUnlock()
	if len(hooks) == 0 {
		return
	}
	message := "Error: " + llm.RedactDiagnostic(err.Error())
	result := ToolResult{
		Output: message, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: message}}, IsError: true,
	}
	info := ErrorInfoOf(err)
	if info.Code != CodeUnknown {
		result.Error = &info
	}
	for _, hook := range hooks {
		runResultHookSafely(hook, execution, result)
	}
}

// runSafely turns a hostile or buggy tool panic into a durable tool failure.
// A plugin boundary must never let a model-invoked implementation unwind the
// agent loop or skip the tool/result event that closes the call.
func runSafely(run func(context.Context) (ToolResult, error), ctx context.Context) (result ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = ToolResult{
				Output:  llm.RedactDiagnostic(fmt.Sprintf("Error: tool panicked: %v", recovered)),
				IsError: true,
				Error:   &ErrorInfo{Name: "ToolPanicError", Code: CodeToolPanic},
			}
			err = nil
		}
	}()
	return run(ctx)
}

func timeoutResult(timeout time.Duration) ToolResult {
	return ToolResult{
		Output:  fmt.Sprintf("Error: tool call timed out after %s", timeout),
		IsError: true,
		Error:   &ErrorInfo{Name: "ToolTimeoutError", Code: CodeToolTimeout},
	}
}

// parseArguments mirrors dsh's model-argument normalization: empty input is an
// empty object, valid JSON is passed as parsed JSON for classification, and
// malformed JSON stays non-parallel and is rejected by Prepare.
func ParseArguments(args any) (any, error) {
	if args == nil {
		return map[string]any{}, nil
	}
	// The loop hands its parsed value to classification and preparation. Keep
	// this normalization side-effect free; Prepare establishes the detached
	// lossless snapshot after classification and before policy/dispatch.
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

// snapshotArguments establishes the registry's lossless argument boundary.
// ParseArguments intentionally accepts already-decoded values so the loop can
// classify concurrency without reparsing model JSON; Prepare must still detach
// that value before policy/dispatch so callers cannot mutate the arguments
// after preparation. JSON round-tripping also rejects functions, channels,
// NaN/Inf, and other values that cannot cross the DSH execution boundary.
func snapshotArguments(args any) (any, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("tool execution arguments must be losslessly JSON-serializable: %w", err)
	}
	var detached any
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return nil, fmt.Errorf("tool execution arguments could not be materialized: %w", err)
	}
	return detached, nil
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
	return ToolResult{Value: value, ValueSet: true, Output: output, Content: content}, nil
}

func boundedResult(r *Registry, name string, result ToolResult) ToolResult {
	// dsh-spill-policy only transforms a result whose rendered content is
	// entirely plain text. A rich result (image/file/reference blocks) is an
	// atomic projection: truncating its auxiliary Output while leaving Content
	// intact would make the durable and model-facing projections disagree.
	if len(result.Content) > 0 {
		return result
	}
	bounded := r.applyOutputCap(name, result.Output)
	bounded.Value = result.Value
	bounded.ValueSet = result.ValueSet
	if result.SpillPath != "" {
		bounded.SpillPath = result.SpillPath
		bounded.SpillBytes = result.SpillBytes
	}
	bounded.Content = result.Content
	bounded.Meta = result.Meta
	// AdditionalContexts are opaque handles returned by nested Code Mode
	// dispatch. They are independent of the text spill projection and must
	// survive both the bounded and the inline result paths.
	bounded.AdditionalContexts = append([]string(nil), result.AdditionalContexts...)
	bounded.AdditionalContextMessages = cloneContextMessages(result.AdditionalContextMessages)
	bounded.ConcludesTurn = result.ConcludesTurn && !result.IsError
	bounded.IsError = result.IsError
	bounded.Error = result.Error
	return bounded
}

func cloneContextMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = append([]llm.ContentBlock(nil), message.Content...)
		out[i].ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
	}
	return out
}

// applyOutputCap truncates oversized results and spills the full text. A spill
// failure is best-effort: it must never turn a successful tool call into an
// error, so the inline result is kept unchanged (mirrors dsh-spill-policy).
func (r *Registry) applyOutputCap(name, out string) ToolResult {
	r.spillMu.Lock()
	defer r.spillMu.Unlock()
	r.mu.RLock()
	policy := r.policy
	owner := r.owner
	r.mu.RUnlock()
	limit := policy.OutputLimit
	if limit <= 0 || len(out) <= limit {
		return ToolResult{Output: out}
	}
	store := &SpillStore{dir: policy.spillDir()}
	locator, err := store.Save(owner.SessionID, r.nextSeq(), out)
	if err != nil {
		return ToolResult{Output: out}
	}
	return truncateResult(out, locator, limit)
}

// nextSeq returns the spill sequence number: the bound session's next event
// seq when an owner is installed, else a per-registry counter.
func (r *Registry) nextSeq() uint64 {
	r.mu.Lock()
	owner := r.owner
	if owner.NextSeq == nil {
		r.fallbackSeq++
		seq := r.fallbackSeq
		r.mu.Unlock()
		return seq
	}
	r.mu.Unlock()
	return owner.NextSeq()
}
