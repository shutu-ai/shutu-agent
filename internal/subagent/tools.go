// tools.go — the M5b-2 Consumer half of the subagent seam (ADR
// 2026-08-18-m5-agent-core.md 决策 ② / dispatch-m5b-2 §2): subagent_spawn,
// subagent_status, subagent_cancel and subagent_list are registered into the
// tools.Registry by the composition root (cmd/pa) when subagent.enabled, and
// auto-whitelisted by config.applyDefaults the same way the job_* tools are.
// They implement the tools.Tool method set structurally (Go structural
// typing), so this package never imports the tools package — the seam stays
// decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false) before this code runs.
//
// D3 event logging follows the M5a-2 tool-layer decision (ADR 决策 ① 实施说明 /
// dispatch-m5b-2 §2): subagent_spawn emits subagent/start on a successful
// Start, and the observing tool subagent_status emits subagent/end exactly
// once per child once it observes a settled child. Every append happens inside
// a tool Execute — the serial main-loop path — so the session log is never
// touched from the background child goroutines (D5). The background goroutine
// that awaits a child's settle only caches the terminal Result in this bundle;
// it never appends to any session log.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/jabing/shutu-agent/internal/session"
)

// Tool names (whitelisted when subagent.enabled; see config.subagentToolNames).
const (
	ToolSpawnName     = "subagent_spawn"
	ToolStatusName    = "subagent_status"
	ToolCancelName    = "subagent_cancel"
	ToolListName      = "subagent_list"
	ToolSendName      = "subagent_send"
	ToolInterruptName = "subagent_interrupt"
	ToolReportName    = "subagent_report"
	ToolResumeName    = "subagent_resume"
)

// defaultProviderName is the provider subagent_spawn delegates to. v1 ships a
// single in-process provider ("spawn", spawn.go); config.subagent
// .default_provider also defaults to "spawn" (config package), so the tool
// always resolves to the same provider.
const defaultProviderName = "spawn"

// childInfo is the bundle's per-child record: the live Run (for status/cancel)
// plus the lineage facts needed for event payloads and status text.
type childInfo struct {
	run      *Run
	provider string
	label    string
	parent   string
}

// SubagentTools bundles the shared state of the four subagent_* tools: the
// Runtime service, the default delegation depth (from config.max_depth), the
// owner-session provider, the event sink, the child/settle registry shared
// across all tools, and the subagent/end tracker so the terminal event is
// emitted exactly once per child.
type SubagentTools struct {
	rt              Runtime
	defaultMaxDepth int
	owner           func() string
	onEvent         func(typ string, data any)
	endTracker      *subagentEndTracker

	mu       sync.Mutex
	children map[string]*childInfo
	settled  map[string]Result
}

// NewSubagentTools returns the shared subagent-tool bundle bound to a Runtime.
// defaultMaxDepth is applied when the model omits subagent_spawn.max_depth
// (the composition root passes config.subagent.max_depth). owner, when
// non-nil, returns the current session id and is used to default the spawn
// parent session and subagent_list's parent_session filter. onEvent, when
// non-nil, receives the subagent/* event payloads; the composition root wires
// it to the session log (D3).
func NewSubagentTools(r Runtime, defaultMaxDepth int, owner func() string, onEvent func(typ string, data any)) *SubagentTools {
	return &SubagentTools{
		rt:              r,
		defaultMaxDepth: defaultMaxDepth,
		owner:           owner,
		onEvent:         onEvent,
		endTracker:      newSubagentEndTracker(),
		children:        map[string]*childInfo{},
		settled:         map[string]Result{},
	}
}

// Spawn returns the subagent_spawn tool.
func (t *SubagentTools) Spawn() SubagentSpawnTool { return SubagentSpawnTool{t: t} }

// Status returns the subagent_status tool.
func (t *SubagentTools) Status() SubagentStatusTool { return SubagentStatusTool{t: t} }

// Cancel returns the subagent_cancel tool.
func (t *SubagentTools) Cancel() SubagentCancelTool { return SubagentCancelTool{t: t} }

// List returns the subagent_list tool.
func (t *SubagentTools) List() SubagentListTool { return SubagentListTool{t: t} }

// Send returns the continuable-child message tool.
func (t *SubagentTools) Send() SubagentSendTool { return SubagentSendTool{t: t} }

// Interrupt returns the dsh-compatible interrupt alias for cancellation.
func (t *SubagentTools) Interrupt() SubagentInterruptTool { return SubagentInterruptTool{t: t} }

// Report returns the explicit child-to-parent report event tool.
func (t *SubagentTools) Report() SubagentReportTool { return SubagentReportTool{t: t} }

// Resume returns the persisted-child cold-resume tool.
func (t *SubagentTools) Resume() SubagentResumeTool { return SubagentResumeTool{t: t} }

// callerSession returns the active session id (the delegating session for a
// spawn and the parent filter for subagent_list); "" when no owner provider is
// installed.
func (t *SubagentTools) callerSession() string {
	if t.owner != nil {
		return t.owner()
	}
	return ""
}

// emit forwards one subagent/* event payload to the injected sink (D3).
func (t *SubagentTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

// register records a freshly started child and spawns the settle-await
// goroutine that caches its terminal Result. The goroutine never touches any
// session log (D5) — it only fills the bundle's settle cache, which the serial
// status tool reads to emit subagent/end.
func (t *SubagentTools) register(childID string, info *childInfo) {
	t.mu.Lock()
	t.children[childID] = info
	t.mu.Unlock()
	go t.awaitSettle(childID, info.run)
}

// awaitSettle blocks until the child settles and caches its Result. It runs on
// a background goroutine but performs no side effects beyond updating the
// bundle's cache: the session log is only ever appended on the serial tool
// path (D5, dispatch-m5b-2 §2). Once the Runtime is closed (composition-root
// defer), every child settles and this goroutine returns.
func (t *SubagentTools) awaitSettle(childID string, run *Run) {
	res, err := run.Result(context.Background())
	if err != nil {
		res = Result{}
	}
	t.mu.Lock()
	t.settled[childID] = res
	t.mu.Unlock()
}

// lookup returns the child record and, when the settle cache already holds its
// Result, that result and true. The settle cache is eventual: a child that has
// just settled may briefly report running until its await goroutine writes the
// Result (a subsequent observation settles it).
func (t *SubagentTools) lookup(childID string) (*childInfo, Result, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.children[childID]
	if !ok {
		return nil, Result{}, false
	}
	res, settled := t.settled[childID]
	return info, res, settled
}

// SubagentSpawnTool delegates one task to a brand-new child agent and returns
// its child session id. It does not block: the child runs in the background,
// observed with subagent_status / subagent_list.
type SubagentSpawnTool struct {
	t *SubagentTools
}

func (SubagentSpawnTool) Name() string { return ToolSpawnName }

func (SubagentSpawnTool) Description() string {
	return "delegate a task to a new subagent (independent session + agent) and return its child id; " +
		"it runs in the background — observe with subagent_status/subagent_list, cancel with subagent_cancel"
}

func (SubagentSpawnTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the task given to the subagent as its first user message (required)",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "one-line subagent label (default the prompt head)",
			},
			"owner_session": map[string]any{
				"type":        "string",
				"description": "delegating parent session id (default the current session)",
			},
			"max_depth": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "delegation depth cap (default from config.subagent.max_depth)",
			},
			"acceptance_criteria": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "minLength": 1},
				"description": "optional acceptance criteria the deliverable must satisfy (eval); injected into the subagent prompt for self-check",
			},
			"provider": map[string]any{
				"type":        "string",
				"enum":        []string{"spawn", "codex", "claude-code"},
				"description": "subagent provider: spawn (default, local) | codex | claude-code (external CLI; must be enabled in config)",
			},
			"continuable": map[string]any{
				"type":        "boolean",
				"description": "keep the child alive after a turn so subagent_send can queue follow-up messages",
			},
		},
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
}

func (t SubagentSpawnTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Prompt             string   `json:"prompt"`
		Label              string   `json:"label"`
		OwnerSession       string   `json:"owner_session"`
		MaxDepth           int      `json:"max_depth"`
		AcceptanceCriteria []string `json:"acceptance_criteria"`
		Provider           string   `json:"provider"`
		Continuable        bool     `json:"continuable"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("subagent_spawn: %w", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("subagent_spawn: empty prompt")
	}
	label := a.Label
	if label == "" {
		label = promptHead(a.Prompt)
	}
	parent := a.OwnerSession
	if parent == "" {
		parent = t.t.callerSession()
	}
	maxDepth := a.MaxDepth
	if maxDepth <= 0 {
		maxDepth = t.t.defaultMaxDepth
	}
	// provider defaults to the local in-process provider; an explicit
	// provider selects an external backend when it is enabled and registered.
	// An unknown or unregistered provider is surfaced as an error by
	// Runtime.Start (fail-closed, no silent fallback to the local provider).
	provider := a.Provider
	if provider == "" {
		provider = defaultProviderName
	}
	run, err := t.t.rt.Start(ctx, provider, StartRequest{
		Label:              label,
		Prompt:             a.Prompt,
		ParentSessionID:    parent,
		MaxDepth:           maxDepth,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Continuable:        a.Continuable,
	})
	if err != nil {
		return "", fmt.Errorf("subagent_spawn: %w", err)
	}
	t.t.register(run.ID, &childInfo{run: run, provider: provider, label: label, parent: parent})
	t.t.emit(session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, label))
	return fmt.Sprintf("started subagent %s (provider=%s, label=%q, parent=%s); "+
		"observe with subagent_status/subagent_list, cancel with subagent_cancel",
		run.ID, provider, label, parent), nil
}

// SubagentSendTool queues a follow-up message for a live continuable child.
type SubagentSendTool struct{ t *SubagentTools }

func (SubagentSendTool) Name() string { return ToolSendName }
func (SubagentSendTool) Description() string {
	return "send a follow-up message to a live continuable subagent"
}
func (SubagentSendTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":      map[string]any{"type": "string", "minLength": 1},
			"message": map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []string{"id", "message"},
		"additionalProperties": false,
	}
}
func (t SubagentSendTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("%s: %w", ToolSendName, err)
	}
	info, _, _ := t.t.lookup(a.ID)
	if info == nil {
		return "", fmt.Errorf("%s: unknown subagent %q", ToolSendName, a.ID)
	}
	if info.run.Send == nil {
		return "", fmt.Errorf("%s: %w", ToolSendName, ErrNotContinuable)
	}
	if err := info.run.Send(ctx, a.Message); err != nil {
		return "", fmt.Errorf("%s: %w", ToolSendName, err)
	}
	return "queued", nil
}

// SubagentInterruptTool is the dsh-compatible name for interrupting the
// current child turn. It shares cancellation semantics with subagent_cancel.
type SubagentInterruptTool struct{ t *SubagentTools }

func (SubagentInterruptTool) Name() string        { return ToolInterruptName }
func (SubagentInterruptTool) Description() string { return "interrupt a live subagent turn" }
func (SubagentInterruptTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "minLength": 1},
			"reason": map[string]any{"type": "string"},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}
func (t SubagentInterruptTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return SubagentCancelTool{t: t.t}.Execute(ctx, args)
}

// SubagentReportTool records an explicit report on the parent session's event
// stream. The report is intentionally bounded by the normal tool output cap;
// the event payload itself remains a simple opaque session fact.
type SubagentReportTool struct{ t *SubagentTools }

func (SubagentReportTool) Name() string { return ToolReportName }
func (SubagentReportTool) Description() string {
	return "send a bounded report from a subagent to its parent session"
}
func (SubagentReportTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":      map[string]any{"type": "string", "minLength": 1},
			"content": map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []string{"id", "content"},
		"additionalProperties": false,
	}
}
func (t SubagentReportTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("%s: %w", ToolReportName, err)
	}
	info, _, _ := t.t.lookup(a.ID)
	if info == nil {
		return "", fmt.Errorf("%s: unknown subagent %q", ToolReportName, a.ID)
	}
	t.t.emit(session.EventSubagentReport, session.NewSubagentReport(a.ID, info.parent, a.Content))
	return "reported", nil
}

// SubagentResumeTool reactivates a persisted local child session after a
// process restart. The provider restores the child log; this tool only adds
// the live run to the current process registry.
type SubagentResumeTool struct{ t *SubagentTools }

func (SubagentResumeTool) Name() string { return ToolResumeName }
func (SubagentResumeTool) Description() string {
	return "resume a persisted local subagent session with a new message"
}
func (SubagentResumeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":          map[string]any{"type": "string", "minLength": 1},
			"message":     map[string]any{"type": "string", "minLength": 1},
			"provider":    map[string]any{"type": "string", "description": "original provider when resuming after a process restart"},
			"continuable": map[string]any{"type": "boolean"},
		},
		"required":             []string{"id", "message"},
		"additionalProperties": false,
	}
}
func (t SubagentResumeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID          string `json:"id"`
		Message     string `json:"message"`
		Provider    string `json:"provider"`
		Continuable bool   `json:"continuable"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("%s: %w", ToolResumeName, err)
	}
	provider := strings.TrimSpace(a.Provider)
	if provider == "" {
		if info, _, ok := t.t.lookup(a.ID); ok && info.provider != "" {
			provider = info.provider
		} else {
			provider = defaultProviderName
		}
	}
	run, err := t.t.rt.Resume(ctx, provider, a.ID, a.Message, a.Continuable)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ToolResumeName, err)
	}
	t.t.register(run.ID, &childInfo{run: run, provider: provider, label: "resumed"})
	return fmt.Sprintf("resumed subagent %s (provider=%s)", run.ID, provider), nil
}

// SubagentStatusTool returns one subagent's summary: running while live, or
// the terminal result (output + stop reason) once settled. On the first
// observation of a settled child it emits subagent/end exactly once.
type SubagentStatusTool struct {
	t *SubagentTools
}

func (SubagentStatusTool) Name() string { return ToolStatusName }

func (SubagentStatusTool) Description() string {
	return "show the current state of one subagent: running, or its terminal result (output + stop reason) once settled"
}

func (SubagentStatusTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the child id returned by subagent_spawn",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t SubagentStatusTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("subagent_status: %w", err)
	}
	info, res, settled := t.t.lookup(a.ID)
	if info == nil {
		return "", fmt.Errorf("subagent_status: unknown subagent %q", a.ID)
	}
	if !settled {
		return fmt.Sprintf("subagent %s: running (provider=%s, label=%q)", a.ID, info.provider, info.label), nil
	}
	if t.t.endTracker.mark(a.ID) {
		t.t.emit(session.EventSubagentEnd, session.NewSubagentEnd(a.ID, info.provider, res.StopReason, res.Output))
	}
	return formatSubagentResult(a.ID, info, res), nil
}

// SubagentCancelTool requests cancellation of one live subagent (idempotent);
// a settled subagent reports already-finished.
type SubagentCancelTool struct {
	t *SubagentTools
}

func (SubagentCancelTool) Name() string { return ToolCancelName }

func (SubagentCancelTool) Description() string {
	return "request cancellation of one live subagent; returns requested or already-finished"
}

func (SubagentCancelTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the child id returned by subagent_spawn",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "optional reason recorded for the cancellation",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t SubagentCancelTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("subagent_cancel: %w", err)
	}
	info, _, _ := t.t.lookup(a.ID)
	if info == nil {
		return "", fmt.Errorf("subagent_cancel: unknown subagent %q", a.ID)
	}
	if a.Reason == "" {
		a.Reason = "cancelled via subagent_cancel"
	}
	if err := info.run.Cancel(a.Reason); err != nil {
		return "already-finished", nil
	}
	return "requested", nil
}

// SubagentListTool projects the children spawned under one parent session
// (default the current session), with their running/settled state.
type SubagentListTool struct {
	t *SubagentTools
}

func (SubagentListTool) Name() string { return ToolListName }

func (SubagentListTool) Description() string {
	return "list the subagents spawned by the current (or given) session, with their running/settled state"
}

func (SubagentListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"parent_session": map[string]any{
				"type":        "string",
				"description": "delegating parent session id (default the current session)",
			},
		},
		"additionalProperties": false,
	}
}

func (t SubagentListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ParentSession string `json:"parent_session"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("subagent_list: %w", err)
	}
	parent := a.ParentSession
	if parent == "" {
		parent = t.t.callerSession()
	}
	children, err := t.t.rt.ListChildren(ctx, parent)
	if err != nil {
		return "", fmt.Errorf("subagent_list: %w", err)
	}
	if len(children) == 0 {
		return "no subagents under parent " + parent, nil
	}
	var sb strings.Builder
	for _, c := range children {
		state := "settled"
		if c.Running {
			state = "running"
		}
		fmt.Fprintf(&sb, "subagent %s (%s): %s\n", c.ID, c.Label, state)
	}
	return strings.TrimSuffix(sb.String(), "\n"), nil
}

// subagentEndTracker remembers which child ids have already had their
// subagent/end event emitted, so the terminal event is logged exactly once per
// child across repeated observations.
type subagentEndTracker struct {
	mu   sync.Mutex
	done map[string]bool
}

func newSubagentEndTracker() *subagentEndTracker {
	return &subagentEndTracker{done: map[string]bool{}}
}

// mark reports whether this child id has not yet had its end event emitted
// (true) and records it; a repeated observation returns false.
func (tr *subagentEndTracker) mark(id string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.done[id] {
		return false
	}
	tr.done[id] = true
	return true
}

// promptHead returns a compact, bounded head of the prompt for use as the
// default subagent label when the model omits label.
func promptHead(prompt string) string {
	compact := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(compact)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return compact
}

// formatSubagentResult renders a settled subagent as model-facing text.
func formatSubagentResult(id string, info *childInfo, res Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "subagent %s: settled (provider=%s, stop_reason=%s)", id, info.provider, res.StopReason)
	if info.label != "" {
		fmt.Fprintf(&sb, ", label=%q", info.label)
	}
	if res.Output != "" {
		fmt.Fprintf(&sb, "\n  output: %s", res.Output)
	}
	return sb.String()
}
