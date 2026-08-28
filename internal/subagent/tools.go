// tools.go — the M5b-2 Consumer half of the subagent seam (ADR
// 2026-08-18-m5-agent-core.md 决策 ② / dispatch-m5b-2 §2): subagent,
// subagent_fork, send_message and interrupt_agent are registered into the
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
// dispatch-m5b-2 §2): subagent emits subagent/start on a successful
// Start. The model-facing control tools send_message and interrupt_agent
// operate on the same runtime.
// once per child once it observes a settled child. Every append happens inside
// a tool Execute — the serial main-loop path — so the session log is never
// touched from the background child goroutines (D5). The background goroutine
// that awaits a child's settle only caches the terminal Result in this bundle;
// it never appends to any session log.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/session"
)

func valueJSON(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Tool names (whitelisted when subagent.enabled; see config.subagentToolNames).
const (
	ToolSpawnName      = "subagent"
	ToolTeammateName   = "spawn_teammate"
	ToolForkName       = "subagent_fork" // internal/provider compatibility; never model-visible
	ToolStatusName     = "subagent_status"
	ToolCancelName     = "subagent_cancel"
	ToolListName       = "subagent_list"
	ToolListAgentsName = "list_agents"
	ToolSendName       = "send_message"
	ToolFollowupName   = "followup_task"
	ToolWaitName       = "wait_agent"
	ToolInterruptName  = "interrupt_agent"
	ToolReportName     = "report"
	ToolResumeName     = "subagent_resume"
)

// defaultProviderName is the provider the subagent tool delegates to. v1 ships a
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
	rt                 Runtime
	defaultMaxDepth    int
	defaultContinuable bool
	owner              func() string
	onEvent            func(typ string, data any)
	endTracker         *subagentEndTracker

	mu         sync.Mutex
	children   map[string]*childInfo
	settled    map[string]Result
	jobs       jobs.Registry
	messageSeq uint64
	changeCh   chan struct{}
}

// NewSubagentTools returns the shared subagent-tool bundle bound to a Runtime.
// defaultMaxDepth is applied when the model omits subagent.max_depth
// (the composition root passes config.subagent.max_depth). owner, when
// non-nil, returns the current session id and is used to default the spawn
// parent session and subagent_list's parent_session filter. onEvent, when
// non-nil, receives the subagent/* event payloads; the composition root wires
// it to the session log (D3).
func NewSubagentTools(r Runtime, defaultMaxDepth int, owner func() string, onEvent func(typ string, data any)) *SubagentTools {
	return NewSubagentToolsWithContinuable(r, defaultMaxDepth, owner, onEvent, false)
}

// NewSubagentToolsWithContinuable configures the DSH standard behavior where
// the model-facing subagent tool keeps the child conversation continuable.
func NewSubagentToolsWithContinuable(r Runtime, defaultMaxDepth int, owner func() string, onEvent func(typ string, data any), defaultContinuable bool) *SubagentTools {
	return &SubagentTools{
		rt:                 r,
		defaultMaxDepth:    defaultMaxDepth,
		defaultContinuable: defaultContinuable,
		owner:              owner,
		onEvent:            onEvent,
		endTracker:         newSubagentEndTracker(),
		children:           map[string]*childInfo{},
		settled:            map[string]Result{},
		changeCh:           make(chan struct{}),
	}
}

// Spawn returns the subagent tool.
func (t *SubagentTools) Spawn() SubagentSpawnTool {
	return SubagentSpawnTool{t: t, provider: defaultProviderName, continuable: t.defaultContinuable}
}

// SpawnTeammate returns the DSH Agent Teams-compatible durable delegation
// surface. Team tasks are intentionally not implemented; this tool only
// creates a named continuable child and returns its member projection.
func (t *SubagentTools) SpawnTeammate() SubagentTeammateTool {
	return SubagentTeammateTool{t: t}
}

// Fork returns the DSH-named continuable delegation tool.
func (t *SubagentTools) Fork() SubagentForkTool { return SubagentForkTool{t: t} }

// Status returns the subagent_status tool.
func (t *SubagentTools) Status() SubagentStatusTool { return SubagentStatusTool{t: t} }

// Cancel returns the subagent_cancel tool.
func (t *SubagentTools) Cancel() SubagentCancelTool { return SubagentCancelTool{t: t} }

// List returns the subagent_list tool.
func (t *SubagentTools) List() SubagentListTool { return SubagentListTool{t: t} }

// ListAgents returns the DSH control-plane discovery tool. The legacy List
// implementation remains available internally for status/UI callers.
func (t *SubagentTools) ListAgents() SubagentListAgentsTool { return SubagentListAgentsTool{t: t} }

// Send returns the continuable-child message tool.
func (t *SubagentTools) Send() SubagentSendTool { return SubagentSendTool{t: t} }

// DshSend returns the DSH-shaped send_message surface. The legacy Send tool
// remains available to package callers but is not registered by cmd/pa.
func (t *SubagentTools) DshSend() SubagentMessageTool {
	return SubagentMessageTool{t: t, name: ToolSendName, wakeup: false}
}

// FollowupTask returns the DSH-shaped waking follow-up surface.
func (t *SubagentTools) FollowupTask() SubagentMessageTool {
	return SubagentMessageTool{t: t, name: ToolFollowupName, wakeup: true}
}

// WaitAgent returns the DSH-shaped change waiter. It observes child changes
// and never starts or wakes a child.
func (t *SubagentTools) WaitAgent() SubagentWaitTool { return SubagentWaitTool{t: t} }

// Interrupt returns the dsh-compatible interrupt alias for cancellation.
func (t *SubagentTools) Interrupt() SubagentInterruptTool { return SubagentInterruptTool{t: t} }

// Report returns the explicit child-to-parent report event tool.
func (t *SubagentTools) Report() SubagentReportTool { return SubagentReportTool{t: t} }

// ReportFromChild validates the exact child identity and records a report on
// its direct parent. The provider uses this as the child-scoped report seam.
func (t *SubagentTools) ReportFromChild(childID, output string) (string, error) {
	childID = strings.TrimSpace(childID)
	output = strings.TrimSpace(output)
	if childID == "" || output == "" {
		return "", fmt.Errorf("%s: child id and output are required", ToolReportName)
	}
	info, _, ok := t.lookup(childID)
	if !ok || info == nil || strings.TrimSpace(info.parent) == "" {
		return "", fmt.Errorf("%s: direct parent is not live; report was not delivered", ToolReportName)
	}
	messageID := t.nextMessageID(childID)
	t.emit(session.EventSubagentReport, session.NewSubagentReport(childID, info.parent, output))
	return messageID, nil
}

// Resume returns the persisted-child cold-resume tool.
func (t *SubagentTools) Resume() SubagentResumeTool { return SubagentResumeTool{t: t} }

// SetJobs attaches the host job registry used by one-shot background
// delegation. It is optional: continuable background children do not need it.
func (t *SubagentTools) SetJobs(reg jobs.Registry) { t.jobs = reg }

func (t *SubagentTools) nextMessageID(childID string) string {
	return fmt.Sprintf("%s-message-%d", childID, atomic.AddUint64(&t.messageSeq, 1))
}

// SendTo queues one browser-originated follow-up for a live continuable child.
// The web adapter uses this method after it has performed the durable parent
// lineage check; the tool bundle remains the owner of the live Run reference.
func (t *SubagentTools) SendTo(ctx context.Context, childID, message string) error {
	info, _, _ := t.lookup(childID)
	if info == nil {
		return fmt.Errorf("subagent: unknown subagent %q", childID)
	}
	if info.run.Send == nil {
		return fmt.Errorf("%w: %s", ErrNotContinuable, childID)
	}
	return info.run.Send(ctx, message)
}

// InterruptTo cancels one browser-addressed child turn. The web adapter
// performs parent/mode authorization before calling this process-local seam.
func (t *SubagentTools) InterruptTo(childID, reason string) error {
	info, _, _ := t.lookup(childID)
	if info == nil {
		return fmt.Errorf("subagent: unknown subagent %q", childID)
	}
	if info.run.Cancel == nil {
		return fmt.Errorf("subagent: cancel is unavailable for %q", childID)
	}
	return info.run.Cancel(reason)
}

// isDescendant enforces DSH control authority: send_message is restricted to
// direct children, while interrupt_agent may target any depth below the caller.
func (t *SubagentTools) isDescendant(childID, ancestorID string) bool {
	childID = strings.TrimSpace(childID)
	ancestorID = strings.TrimSpace(ancestorID)
	if childID == "" || ancestorID == "" || childID == ancestorID {
		return false
	}
	t.mu.Lock()
	info, ok := t.children[childID]
	if ok && info != nil && strings.TrimSpace(info.parent) == ancestorID {
		t.mu.Unlock()
		return true
	}
	t.mu.Unlock()
	seen := map[string]bool{}
	for childID != "" && !seen[childID] {
		seen[childID] = true
		info, _, ok := t.lookup(childID)
		if !ok || info == nil {
			return false
		}
		if strings.TrimSpace(info.parent) == ancestorID {
			return true
		}
		childID = info.parent
	}
	return false
}

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

// signalChange wakes one or more wait_agent callers without retaining an
// unbounded notification queue. Every observer re-reads authoritative state.
func (t *SubagentTools) signalChange() {
	t.mu.Lock()
	close(t.changeCh)
	t.changeCh = make(chan struct{})
	t.mu.Unlock()
}

// register records a freshly started child and spawns the settle-await
// goroutine that caches its terminal Result. The goroutine never touches any
// session log (D5) — it only fills the bundle's settle cache, which the serial
// status tool reads to emit subagent/end.
func (t *SubagentTools) register(childID string, info *childInfo) {
	t.mu.Lock()
	t.children[childID] = info
	t.mu.Unlock()
	t.signalChange()
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
	info := t.children[childID]
	t.mu.Unlock()
	if info != nil && t.endTracker.mark(childID) {
		// DSH delivers a completion notice without requiring a polling status
		// call. The event sink is session-log safe and is the host's wake-up seam.
		t.emit(session.EventSubagentEnd, session.NewSubagentEnd(childID, info.provider, res.StopReason, res.Output))
	}
	t.mu.Lock()
	t.settled[childID] = res
	t.mu.Unlock()
	t.signalChange()
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
	t           *SubagentTools
	provider    string
	continuable bool
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
			"description": map[string]any{
				"type": "string", "minLength": 1,
				"description": "short display label for the delegated task",
			},
			"prompt": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the task given to the subagent as its first user message (required)",
			},
			"run_in_background": map[string]any{
				"type":        "boolean",
				"description": "whether to return immediately with a background id",
			},
		},
		"required":             []string{"description", "prompt"},
		"additionalProperties": false,
	}
}

func (t SubagentSpawnTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

func (t SubagentSpawnTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Description     string `json:"description"`
		Prompt          string `json:"prompt"`
		RunInBackground *bool  `json:"run_in_background"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.Name(), err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: empty prompt", t.Name())
	}
	if strings.TrimSpace(a.Description) == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: empty description", t.Name())
	}
	parent := t.t.callerSession()
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", t.Name())
	}
	background := t.continuable
	if a.RunInBackground != nil {
		background = *a.RunInBackground
	}
	provider := t.provider
	if provider == "" {
		provider = defaultProviderName
	}
	if provider == "fork" {
		if _, ok := t.t.rt.GetProvider(provider); !ok {
			provider = defaultProviderName
		}
	}
	continuable := t.continuable && background
	inherit := provider == "fork"
	start := func(startCtx context.Context) (*Run, error) {
		return t.t.rt.Start(startCtx, provider, StartRequest{
			Label: labelOrPrompt(a.Description, a.Prompt), Prompt: a.Prompt,
			ParentSessionID: parent, MaxDepth: t.t.defaultMaxDepth,
			Continuable: continuable, InheritParentContext: inherit,
		})
	}
	if background && !t.continuable {
		if t.t.jobs == nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: background jobs unavailable", t.Name())
		}
		jobID, err := t.t.jobs.Start(ctx, jobs.JobStart{
			Kind: jobs.Kind("subagent"), Label: a.Description, OwnerSession: parent,
			Run: func(jobCtx context.Context) (jobs.JobOutcome, error) {
				run, err := start(jobCtx)
				if err != nil {
					return jobs.JobOutcome{Status: jobs.StatusFailed, Detail: err.Error()}, nil
				}
				t.t.register(run.ID, &childInfo{run: run, provider: provider, label: a.Description, parent: parent})
				t.t.emit(session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, a.Description))
				res, err := run.Result(jobCtx)
				if err != nil {
					return jobs.JobOutcome{Status: jobs.StatusFailed, Detail: err.Error()}, nil
				}
				return jobs.JobOutcome{Status: jobs.StatusCompleted, Detail: res.StopReason, Output: res.Output}, nil
			},
		})
		if err != nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.Name(), err)
		}
		return agenttools.ToolResult{Value: map[string]any{"kind": "background", "jobId": jobID}, Output: fmt.Sprintf("started background subagent job %s", jobID)}, nil
	}
	run, err := start(ctx)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.Name(), err)
	}
	t.t.register(run.ID, &childInfo{run: run, provider: provider, label: a.Description, parent: parent})
	t.t.emit(session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, a.Description))
	if continuable {
		return agenttools.ToolResult{Value: map[string]any{"kind": "continuable", "subagentId": run.ID}, Output: fmt.Sprintf("started subagent %s", run.ID)}, nil
	}
	res, err := run.Result(ctx)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.Name(), err)
	}
	if res.StopReason != StopCompleted {
		return agenttools.ToolResult{}, fmt.Errorf("%s: subagent run ended with %s: %s", t.Name(), res.StopReason, res.Output)
	}
	output := []any{}
	if res.Output != "" {
		output = append(output, map[string]any{"type": "text", "text": res.Output})
	}
	return agenttools.ToolResult{Value: map[string]any{"kind": "foreground", "runId": run.ID, "output": output}, Output: res.Output}, nil
}

func labelOrPrompt(label, prompt string) string {
	if strings.TrimSpace(label) != "" {
		return strings.TrimSpace(label)
	}
	return promptHead(prompt)
}

// SubagentTeammateTool is the DSH-named durable child creator. It mirrors the
// spawn_teammate contract without exposing the excluded shared team-task
// board: a teammate is a named continuable child with a fresh or forked
// context.
type SubagentTeammateTool struct{ t *SubagentTools }

func (SubagentTeammateTool) Name() string { return ToolTeammateName }
func (SubagentTeammateTool) Description() string {
	return "create one named, durable teammate; context fresh starts clean or fork inherits completed parent turns"
}
func (SubagentTeammateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "minLength": 1, "description": "unique teammate name"},
			"description": map[string]any{"type": "string", "minLength": 1, "description": "short delegated responsibility"},
			"prompt":      map[string]any{"type": "string", "minLength": 1, "description": "complete initial task"},
			"context":     map[string]any{"type": "string", "enum": []string{"fresh", "fork"}},
		},
		"required":             []string{"name", "description", "prompt"},
		"additionalProperties": false,
	}
}
func (t SubagentTeammateTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (t SubagentTeammateTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Context     string `json:"context"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolTeammateName, err)
	}
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	a.Prompt = strings.TrimSpace(a.Prompt)
	if a.Name == "" || a.Description == "" || a.Prompt == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: name, description and prompt are required", ToolTeammateName)
	}
	parent := t.t.callerSession()
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolTeammateName)
	}
	contextKind := a.Context
	if contextKind == "" {
		contextKind = "fresh"
	}
	provider := defaultProviderName
	inherit := false
	if contextKind == "fork" {
		provider = "fork"
		inherit = true
	}
	run, err := t.t.rt.Start(ctx, provider, StartRequest{
		Label: a.Name + ": " + a.Description, Prompt: a.Prompt,
		ParentSessionID: parent, MaxDepth: t.t.defaultMaxDepth,
		Continuable: true, InheritParentContext: inherit,
	})
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolTeammateName, err)
	}
	t.t.register(run.ID, &childInfo{run: run, provider: provider, label: a.Name + ": " + a.Description, parent: parent})
	t.t.emit(session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, a.Description))
	member := map[string]any{
		"id": run.ID, "name": a.Name, "role": "teammate", "status": "running",
		"description": a.Description, "provider": provider, "context": contextKind,
		"diagnostics": []string{},
	}
	return agenttools.ToolResult{
		Value:  map[string]any{"member": member},
		Output: fmt.Sprintf("started teammate %s (%s)", a.Name, run.ID),
	}, nil
}

// SubagentMessageTool is the DSH target/message contract shared by
// send_message and followup_task. Target accepts a direct child id or its
// teammate name/label; authority remains limited to direct children.
type SubagentMessageTool struct {
	t      *SubagentTools
	name   string
	wakeup bool
}

func (SubagentMessageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":  map[string]any{"type": "string", "minLength": 1, "description": "direct teammate id or name"},
			"message": map[string]any{"type": "string", "minLength": 1, "description": "self-contained message for the target"},
		},
		"required":             []string{"target", "message"},
		"additionalProperties": false,
	}
}
func (t SubagentMessageTool) Name() string { return t.name }
func (t SubagentMessageTool) Description() string {
	if t.wakeup {
		return "send a durable follow-up task to a direct teammate and start a turn when needed"
	}
	return "send durable information to a direct teammate without changing the task"
}
func (t SubagentMessageTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (t SubagentMessageTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Target  string `json:"target"`
		Message string `json:"message"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	parent := t.t.callerSession()
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", t.name)
	}
	info, err := t.t.directChild(a.Target, parent)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if info.run.Send == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, ErrNotContinuable)
	}
	send := info.run.Send
	if !t.wakeup && info.run.SendQuiet != nil {
		send = info.run.SendQuiet
	}
	if send == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, ErrNotContinuable)
	}
	if err := send(ctx, a.Message); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	t.t.signalChange()
	messageID := t.t.nextMessageID(info.run.ID)
	return agenttools.ToolResult{
		Value:  map[string]any{"messageId": messageID, "status": "queued"},
		Output: fmt.Sprintf("message queued for %s as %s", info.run.ID, messageID),
	}, nil
}

func (t *SubagentTools) directChild(target, parent string) (*childInfo, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("target is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, info := range t.children {
		if info == nil || info.parent != parent {
			continue
		}
		if id == target || info.label == target || strings.HasPrefix(info.label, target+": ") {
			return info, nil
		}
	}
	return nil, fmt.Errorf("unknown direct teammate %q", target)
}

// SubagentWaitTool observes child changes without waking inactive children.
type SubagentWaitTool struct{ t *SubagentTools }

func (SubagentWaitTool) Name() string { return ToolWaitName }
func (SubagentWaitTool) Description() string {
	return "wait for the next teammate status, mailbox, or child change; never wakes inactive children"
}
func (SubagentWaitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timeout_ms": map[string]any{"type": "integer", "minimum": 10000, "maximum": 3600000, "description": "wait duration; defaults to 30000"},
		},
		"additionalProperties": false,
	}
}
func (t SubagentWaitTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (t SubagentWaitTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		TimeoutMS int `json:"timeout_ms"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolWaitName, err)
	}
	if a.TimeoutMS == 0 {
		a.TimeoutMS = 30000
	}
	if a.TimeoutMS < 10000 || a.TimeoutMS > 3600000 {
		return agenttools.ToolResult{}, fmt.Errorf("%s: timeout_ms must be between 10000 and 3600000", ToolWaitName)
	}
	parent := t.t.callerSession()
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolWaitName)
	}
	children, err := t.t.rt.ListChildren(ctx, parent)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolWaitName, err)
	}
	active := false
	for _, child := range children {
		if child.Running {
			active = true
			break
		}
	}
	if !active {
		value := map[string]any{"timedOut": false, "noProgress": map[string]any{
			"reason":  "no-active-peer",
			"message": "No other subagent is running. Re-list with list_agents, then use followup_task to wake an inactive child before waiting again.",
		}}
		return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
	}
	t.t.mu.Lock()
	changes := t.t.changeCh
	t.t.mu.Unlock()
	timer := time.NewTimer(time.Duration(a.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return agenttools.ToolResult{}, ctx.Err()
	case <-changes:
		value := map[string]any{"timedOut": false}
		return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
	case <-timer.C:
		value := map[string]any{"timedOut": true}
		return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
	}
}

// SubagentForkTool is the DSH-named one-shot fork delegation entry. The
// provider is separate in production and receives the parent context seed.
type SubagentForkTool struct{ t *SubagentTools }

func (SubagentForkTool) Name() string { return ToolForkName }
func (SubagentForkTool) Description() string {
	return "delegate a task to a subagent that inherits the completed conversation context and return its result"
}
func (t SubagentForkTool) Schema() map[string]any {
	return (SubagentSpawnTool{t: t.t, provider: "fork", continuable: false}).Schema()
}
func (t SubagentForkTool) Execute(ctx context.Context, args any) (string, error) {
	return (SubagentSpawnTool{t: t.t, provider: "fork", continuable: false}).Execute(ctx, args)
}
func (t SubagentForkTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	return (SubagentSpawnTool{t: t.t, provider: "fork", continuable: false}).ExecuteResult(ctx, args)
}

// SubagentSendTool queues a follow-up message for a live continuable child.
type SubagentSendTool struct{ t *SubagentTools }

func (SubagentSendTool) Name() string { return ToolSendName }
func (SubagentSendTool) Description() string {
	return "send a message to a background subagent by id; it becomes the next turn in the same conversation"
}
func (SubagentSendTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subagent_id": map[string]any{"type": "string", "minLength": 1},
			"message":     map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []string{"subagent_id", "message"},
		"additionalProperties": false,
	}
}
func (t SubagentSendTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (t SubagentSendTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		ID      string `json:"subagent_id"`
		Message string `json:"message"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolSendName, err)
	}
	parent := t.t.callerSession()
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolSendName)
	}
	info, _, _ := t.t.lookup(a.ID)
	if info == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: unknown subagent %q", ToolSendName, a.ID)
	}
	if info.parent != parent {
		return agenttools.ToolResult{}, fmt.Errorf("%s: subagent %q is not a direct child of the calling agent", ToolSendName, a.ID)
	}
	if info.run.Send == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolSendName, ErrNotContinuable)
	}
	if err := info.run.Send(ctx, a.Message); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolSendName, err)
	}
	messageID := t.t.nextMessageID(a.ID)
	return agenttools.ToolResult{
		Value:  map[string]any{"messageId": messageID},
		Output: fmt.Sprintf("message queued as the next turn for subagent %s", a.ID),
	}, nil
}

// SubagentInterruptTool is the dsh-compatible name for interrupting the
// current child turn. It shares cancellation semantics with subagent_cancel.
type SubagentInterruptTool struct{ t *SubagentTools }

func (SubagentInterruptTool) Name() string { return ToolInterruptName }
func (SubagentInterruptTool) Description() string {
	return "request cancellation of a background agent's current turn; direct and deeper descendants are allowed"
}
func (SubagentInterruptTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent_id": map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []string{"agent_id"},
		"additionalProperties": false,
	}
}
func (t SubagentInterruptTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (t SubagentInterruptTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		ID string `json:"agent_id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolInterruptName, err)
	}
	if t.t.callerSession() == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolInterruptName)
	}
	if !t.t.isDescendant(a.ID, t.t.callerSession()) {
		return agenttools.ToolResult{}, fmt.Errorf("%s: agent %q is not a descendant of the calling agent", ToolInterruptName, a.ID)
	}
	if err := t.t.InterruptTo(a.ID, "interrupted via interrupt_agent"); err != nil {
		// DSH defines an already-finished interrupt as an accepted no-op.
		if info, _, ok := t.t.lookup(a.ID); !ok || info == nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolInterruptName, err)
		}
	}
	return agenttools.ToolResult{Value: map[string]any{"accepted": true}, Output: fmt.Sprintf("interrupt requested for agent %s", a.ID)}, nil
}

// SubagentReportTool records an explicit report on the parent session's event
// stream. The report is intentionally bounded by the normal tool output cap;
// the event payload itself remains a simple opaque session fact.
type SubagentReportTool struct{ t *SubagentTools }

// childReportTool is installed only in a continuable child registry. Its
// identity is minted by the provider, so the model cannot choose a sender or
// recipient.
type childReportTool struct {
	childID string
	parent  string
	deliver func(childID, parentID, output string) (string, error)
}

func newChildReportTool(childID, parent string, deliver func(string, string, string) (string, error)) childReportTool {
	return childReportTool{childID: childID, parent: parent, deliver: deliver}
}

func (childReportTool) Name() string { return ToolReportName }
func (childReportTool) Description() string {
	return "report a self-contained result to the direct parent without ending this turn"
}
func (childReportTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"output": map[string]any{"type": "string", "minLength": 1, "description": "actionable content for the direct parent"},
		},
		"required": []string{"output"}, "additionalProperties": false,
	}
}
func (t childReportTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (t childReportTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var input struct {
		Output string `json:"output"`
	}
	if err := agenttools.DecodeArgs(args, &input); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolReportName, err)
	}
	if strings.TrimSpace(input.Output) == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: output is required", ToolReportName)
	}
	if err := ctx.Err(); err != nil {
		return agenttools.ToolResult{}, err
	}
	if t.deliver == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: direct parent is not live; report was not delivered", ToolReportName)
	}
	messageID, err := t.deliver(t.childID, t.parent, input.Output)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolReportName, err)
	}
	value := map[string]any{"messageId": messageID}
	return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
}

func (SubagentReportTool) Name() string { return ToolReportName }
func (SubagentReportTool) Description() string {
	return "report a self-contained result to the direct parent without ending this turn"
}
func (SubagentReportTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"output": map[string]any{"type": "string", "minLength": 1, "description": "actionable content for the direct parent"},
		},
		"required":             []string{"output"},
		"additionalProperties": false,
	}
}
func (t SubagentReportTool) Execute(ctx context.Context, args any) (string, error) {
	return "", fmt.Errorf("%s: only available inside a continuable child scope", ToolReportName)
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
func (t SubagentResumeTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID          string `json:"id"`
		Message     string `json:"message"`
		Provider    string `json:"provider"`
		Continuable bool   `json:"continuable"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
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

func (t SubagentStatusTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("subagent_status: %w", err)
	}
	info, res, settled := t.t.lookup(a.ID)
	if info == nil {
		return "", fmt.Errorf("subagent_status: unknown subagent %q", a.ID)
	}
	if !settled {
		return fmt.Sprintf("subagent %s: running (provider=%s, label=%q)", a.ID, info.provider, info.label), nil
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

func (t SubagentCancelTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
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

func (t SubagentListTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ParentSession string `json:"parent_session"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
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

// SubagentListAgentsTool is the DSH control-plane discovery surface. It only
// exposes continuable children and returns a lossless array for consumers such
// as the model and Web UI; one-shot children remain status/job concerns.
type SubagentListAgentsTool struct{ t *SubagentTools }

func (SubagentListAgentsTool) Name() string { return ToolListAgentsName }
func (SubagentListAgentsTool) Description() string {
	return "list continuable background subagents by id, label and status; use descendants to walk the full child tree"
}
func (SubagentListAgentsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope": map[string]any{"type": "string", "enum": []string{"children", "descendants"}, "description": "children by default, or all descendants in stable pre-order"},
		},
		"additionalProperties": false,
	}
}

func (t SubagentListAgentsTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

func (t SubagentListAgentsTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	var a struct {
		Scope string `json:"scope"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolListAgentsName, err)
	}
	parent := t.t.callerSession()
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolListAgentsName)
	}
	scope := a.Scope
	if scope == "" {
		scope = "children"
	}
	entries := make([]any, 0)
	if err := t.t.collectAgents(ctx, parent, 1, scope == "descendants", &entries); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolListAgentsName, err)
	}
	return agenttools.ToolResult{Value: entries, Output: formatAgentList(entries, scope)}, nil
}

func (t *SubagentTools) collectAgents(ctx context.Context, parent string, depth int, recurse bool, out *[]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	children, err := t.rt.ListChildren(ctx, parent)
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		if child.Continuable {
			status := "idle"
			if child.Running {
				status = "running"
			}
			entry := map[string]any{"kind": "child", "id": child.ID, "label": child.Label, "status": status}
			if depth > 1 || recurse {
				entry["parent"] = parent
				entry["depth"] = depth
			}
			*out = append(*out, entry)
		}
		if recurse {
			if err := t.collectAgents(ctx, child.ID, depth+1, true, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatAgentList(entries []any, scope string) string {
	if len(entries) == 0 {
		return "(no subagents)"
	}
	lines := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		id, _ := entry["id"].(string)
		if entry["kind"] == "child" {
			status, _ := entry["status"].(string)
			label, _ := entry["label"].(string)
			at := ""
			if scope == "descendants" {
				at = fmt.Sprintf(" parent=%v depth=%v", entry["parent"], entry["depth"])
			}
			lines = append(lines, fmt.Sprintf("%s [%s]%s — %s", id, status, at, label))
		}
	}
	return strings.Join(lines, "\n")
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
