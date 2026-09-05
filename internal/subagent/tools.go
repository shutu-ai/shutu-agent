// tools.go — the M5b-2 Consumer half of the subagent seam (ADR
// 2026-08-18-m5-agent-core.md 决策 ② / dispatch-m5b-2 §2): subagent,
// subagent_fork, send_message and interrupt_agent are registered into the
// tools.Registry by the composition root (cmd/sta) when subagent.enabled, and
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
// Terminal settlement is emitted once per child. Foreground control events
// use the serial tool path; settlement uses the explicitly parent-addressed
// sink for the terminal event and optional Agent wake, never process-global
// selection.
// a tool Execute — the serial main-loop path — so the session log is never
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

func valueJSON(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// agentOptionsInput is the model-facing subset of DSH AgentOptions that this
// in-process tool can enforce locally. Provider selection remains a
// composition-bound property of the mounted tool; model and maxTokens are
// per-child overrides and therefore belong on the request.
type agentOptionsInput struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"maxTokens"`
}

func resolveAgentOptions(options *agentOptionsInput) (string, int, error) {
	if options == nil {
		return "", 0, nil
	}
	model := strings.TrimSpace(options.Model)
	if options.Model != "" && model == "" {
		return "", 0, fmt.Errorf("agentOptions.model must be non-empty")
	}
	if options.MaxTokens <= 0 {
		return "", 0, fmt.Errorf("agentOptions.maxTokens must be positive")
	}
	return model, options.MaxTokens, nil
}

// Tool names (whitelisted when subagent.enabled; see config.subagentToolNames).
const (
	ToolSpawnName      = "subagent"
	ToolTeammateName   = "spawn_teammate"
	ToolForkName       = "subagent_fork"
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

// TeammateProvision is the adapter result for a host-owned Agent Teams
// roster. The subagent package owns message/control bookkeeping, while the
// composition root owns the concrete Agent Registry and durable team board.
type TeammateProvision struct {
	ID          string
	Name        string
	Description string
	Provider    string
	Context     string
	Status      string
	Run         *Run
}

type TeammateProvisioner func(context.Context, string, string, string, string, string) (TeammateProvision, error)

// Teammate is the process-local control projection of a durable Team member.
// The roster owns identity and authorization; callbacks are deliberately
// capability-shaped so this package does not import the Agent/Team runtime.
type Teammate struct {
	ID          string
	Label       string
	Parent      string
	Running     bool
	Continuable bool
	Send        func(context.Context, string, map[string]string) error
	SendContent func(context.Context, []llm.ContentBlock, map[string]string) error
	SendQuiet   func(context.Context, string) error
	Followup    func(context.Context, string) error
	Cancel      func(string) error
}

// TeammateDirectory is the cold-restore seam for Agent Teams. A directory is
// queried in addition to the in-memory provider registry, so durable members
// remain addressable after process restart/rebind.
type TeammateDirectory interface {
	List(context.Context, string) ([]Teammate, error)
	Direct(context.Context, string, string) (Teammate, error)
	Parent(context.Context, string) (string, bool, error)
}

// CompletionWake delivers a settled child notification to its direct parent.
// The composition root decides whether a live parent exists and how the
// notification enters that parent's Agent inbox; the subagent package only
// supplies the stable child result and lineage.
type CompletionWake func(context.Context, string, string, Result) error

// ReportDelivery sends one child-authored report to the live parent Agent.
// The composition root owns inbox placement; the tool bundle owns sender and
// parent authorization and commits the durable report fact first.
type ReportDelivery func(ctx context.Context, parentSessionID, childID, messageID, output string) error

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
	ownerContext       func(context.Context) string
	onEvent            func(typ string, data any)
	onSessionEvent     func(sessionID, typ string, data any) error
	onCompletionWake   CompletionWake
	onReportDelivery   ReportDelivery
	endTracker         *subagentEndTracker

	mu         sync.Mutex
	children   map[string]*childInfo
	settled    map[string]Result
	jobs       jobs.Registry
	messageSeq uint64
	changeCh   chan struct{}
	teammate   TeammateProvisioner
	directory  TeammateDirectory
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

// NewSubagentToolsWithContinuableContext is the Agent-owned constructor. The
// owner resolver receives the addressed runtime context for each operation;
// the older constructor remains for direct embedders and tests.
func NewSubagentToolsWithContinuableContext(r Runtime, defaultMaxDepth int, owner func(context.Context) string, onEvent func(typ string, data any), defaultContinuable bool) *SubagentTools {
	return &SubagentTools{
		rt:                 r,
		defaultMaxDepth:    defaultMaxDepth,
		defaultContinuable: defaultContinuable,
		ownerContext:       owner,
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
// surface. The composition root may bind it to a durable Team roster; the
// library fallback still creates a named continuable child and returns its
// member projection without importing Team storage.
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
// remains available to package callers but is not registered by cmd/sta.
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
	return t.ReportFromChildContext(context.Background(), childID, output)
}

// ReportFromChildContext is the context-aware child-to-parent report seam.
// The parent identity is taken from the registered child record, never from
// model-provided arguments.
func (t *SubagentTools) ReportFromChildContext(ctx context.Context, childID, output string) (string, error) {
	childID = strings.TrimSpace(childID)
	output = strings.TrimSpace(output)
	if childID == "" || output == "" {
		return "", fmt.Errorf("%s: child id and output are required", ToolReportName)
	}
	// lookup's third return reports settlement, not registry membership. A
	// continuable child remains authorized to report after its turn completes.
	info, _, _ := t.lookup(childID)
	if info == nil || strings.TrimSpace(info.parent) == "" {
		return "", fmt.Errorf("%s: direct parent is not live; report was not delivered", ToolReportName)
	}
	messageID := t.nextMessageID(childID)
	if err := t.emitForSession(ctx, info.parent, session.EventSubagentReport, session.NewSubagentReportWithID(childID, info.parent, output, messageID)); err != nil {
		return "", err
	}
	t.mu.Lock()
	deliver := t.onReportDelivery
	t.mu.Unlock()
	if deliver != nil {
		if err := deliver(ctx, info.parent, childID, messageID, output); err != nil {
			return "", err
		}
	}
	return messageID, nil
}

// Resume returns the persisted-child cold-resume tool.
func (t *SubagentTools) Resume() SubagentResumeTool { return SubagentResumeTool{t: t} }

// SetJobs attaches the host job registry used by one-shot background
// delegation. It is optional: continuable background children do not need it.
func (t *SubagentTools) SetJobs(reg jobs.Registry) { t.jobs = reg }

// SetSessionEventSink binds asynchronous child events to an addressed parent
// session. Background child callbacks cannot safely consult a process-global
// current session, so the host receives the parent id explicitly.
func (t *SubagentTools) SetSessionEventSink(sink func(sessionID, typ string, data any) error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onSessionEvent = sink
	t.mu.Unlock()
}

// SetCompletionWake binds the optional parent-Agent wakeup used by the host.
// It is intentionally separate from SetSessionEventSink: durable event
// publication must remain useful for cold sessions, while a live inbox wake
// is only possible when the parent Agent is currently registered.
func (t *SubagentTools) SetCompletionWake(wake CompletionWake) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onCompletionWake = wake
	t.mu.Unlock()
}

// SetReportDelivery binds the parent-inbox relay used by the host. It remains
// optional so library callers can retain the older log-only behavior.
func (t *SubagentTools) SetReportDelivery(delivery ReportDelivery) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onReportDelivery = delivery
	t.mu.Unlock()
}

// SetTeammateProvisioner binds the DSH spawn_teammate surface to a durable
// Agent Teams roster. Without it, the legacy subagent provider remains the
// compatibility fallback for library users.
func (t *SubagentTools) SetTeammateProvisioner(provisioner TeammateProvisioner) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.teammate = provisioner
	t.mu.Unlock()
}

// SetTeammateDirectory binds the recovered Team control projection. It is
// separate from provisioning: a cold roster may be queried before a new
// teammate is created in this process.
func (t *SubagentTools) SetTeammateDirectory(directory TeammateDirectory) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.directory = directory
	t.mu.Unlock()
}

func (t *SubagentTools) nextMessageID(childID string) string {
	return fmt.Sprintf("%s-message-%d", childID, atomic.AddUint64(&t.messageSeq, 1))
}

// SendTo queues one browser-originated follow-up for a live continuable child.
// The web adapter uses this method after it has performed the durable parent
// lineage check; the tool bundle remains the owner of the live Run reference.
func (t *SubagentTools) SendTo(ctx context.Context, childID, message string) error {
	return t.SendToWithMetadata(ctx, childID, message, nil)
}

// SendToWithMetadata queues one browser-originated follow-up while preserving
// DSH user-rpc provenance for providers that support metadata-aware send.
func (t *SubagentTools) SendToWithMetadata(
	ctx context.Context, childID, message string, metadata map[string]string,
) error {
	return t.SendContentToWithMetadata(ctx, childID, []llm.ContentBlock{llm.Text(message)}, metadata)
}

// SendContentToWithMetadata queues one browser-originated rich follow-up while
// preserving DSH user-rpc provenance for providers that support content send.
func (t *SubagentTools) SendContentToWithMetadata(
	ctx context.Context, childID string, content []llm.ContentBlock, metadata map[string]string,
) error {
	var textMessage string
	var textOnly bool
	if len(content) == 1 && content[0].Kind == llm.BlockText {
		textMessage, textOnly = content[0].Text, true
	}
	info, _, _ := t.lookup(childID)
	if info == nil {
		member, err := t.directoryMember(ctx, childID)
		if err != nil {
			return fmt.Errorf("subagent: unknown subagent %q", childID)
		}
		if member.Send == nil {
			return fmt.Errorf("%w: %s", ErrNotContinuable, childID)
		}
		if member.SendContent != nil {
			return member.SendContent(ctx, content, metadata)
		}
		if len(content) != 1 || content[0].Kind != llm.BlockText {
			return fmt.Errorf("%w: rich content", ErrNotContinuable)
		}
		return member.Send(ctx, content[0].Text, metadata)
	}
	if info.run.Send == nil {
		return fmt.Errorf("%w: %s", ErrNotContinuable, childID)
	}
	if info.run.SendContentWithMetadata != nil {
		return info.run.SendContentWithMetadata(ctx, content, metadata)
	}
	if info.run.SendWithMetadata != nil && len(content) == 1 && content[0].Kind == llm.BlockText {
		return info.run.SendWithMetadata(ctx, content[0].Text, metadata)
	}
	if !textOnly {
		return fmt.Errorf("%w: rich content", ErrNotContinuable)
	}
	return info.run.Send(ctx, textMessage)
}

// InterruptTo cancels one browser-addressed child turn. The web adapter
// performs parent/mode authorization before calling this process-local seam.
func (t *SubagentTools) InterruptTo(childID, reason string) error {
	info, _, _ := t.lookup(childID)
	if info == nil {
		member, err := t.directoryMember(context.Background(), childID)
		if err != nil {
			return fmt.Errorf("subagent: unknown subagent %q", childID)
		}
		if member.Cancel == nil {
			return fmt.Errorf("subagent: cancel is unavailable for %q", childID)
		}
		return member.Cancel(reason)
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
	for current := childID; current != "" && !seen[current]; {
		seen[current] = true
		info, _, ok := t.lookup(current)
		if !ok || info == nil {
			break
		}
		if strings.TrimSpace(info.parent) == ancestorID {
			return true
		}
		current = strings.TrimSpace(info.parent)
	}
	t.mu.Lock()
	directory := t.directory
	t.mu.Unlock()
	if directory != nil {
		// Team members currently have direct lead lineage. Walking Parent keeps
		// this check correct if nested Team rosters are enabled later.
		for candidate := strings.TrimSpace(childID); candidate != ""; {
			parent, ok, err := t.directory.Parent(context.Background(), candidate)
			if err != nil || !ok {
				break
			}
			if parent == ancestorID {
				return true
			}
			candidate = strings.TrimSpace(parent)
		}
	}
	return false
}

func (t *SubagentTools) directoryMember(ctx context.Context, id string) (Teammate, error) {
	t.mu.Lock()
	directory := t.directory
	t.mu.Unlock()
	if directory == nil {
		return Teammate{}, errors.New("subagent: teammate directory unavailable")
	}
	return directory.Direct(ctx, "", id)
}

// listChildren merges process-local provider children and durable Team
// members, preferring the roster projection when an identity is present in
// both views. Stable IDs prevent duplicate descendants after rebind.
func (t *SubagentTools) listChildren(ctx context.Context, parent string) ([]ChildSummary, error) {
	children, err := t.rt.ListChildren(ctx, parent)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	directory := t.directory
	t.mu.Unlock()
	if directory == nil {
		return children, nil
	}
	members, err := directory.List(ctx, parent)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]ChildSummary, len(children)+len(members))
	for _, child := range children {
		byID[child.ID] = child
	}
	for _, member := range members {
		byID[member.ID] = ChildSummary{ID: member.ID, Label: member.Label, Running: member.Running, Continuable: member.Continuable}
	}
	merged := make([]ChildSummary, 0, len(byID))
	for _, child := range byID {
		merged = append(merged, child)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged, nil
}

// callerSession returns the active session id (the delegating session for a
// spawn and the parent filter for subagent_list); "" when no owner provider is
// installed.
func (t *SubagentTools) callerSession(ctx ...context.Context) string {
	if len(ctx) > 0 {
		if sessionID := runtimectx.SessionID(ctx[0]); sessionID != "" {
			return sessionID
		}
		if t.ownerContext != nil {
			return t.ownerContext(ctx[0])
		}
	}
	if t.owner != nil {
		return t.owner()
	}
	return ""
}

func (t *SubagentTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

func (t *SubagentTools) emitForSession(_ context.Context, sessionID, typ string, data any) error {
	t.mu.Lock()
	sink := t.onSessionEvent
	t.mu.Unlock()
	if sink != nil {
		return sink(sessionID, typ, data)
	}
	t.emit(typ, data)
	return nil
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
// goroutine that caches its terminal Result. Settlement may publish the
// explicitly parent-addressed terminal event and wake a live parent Agent; it
// never consults process-global session selection. The goroutine never touches
// session log (D5) — it only fills the bundle's settle cache, which the serial
// addressed completion sink above.
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
		// call. Address the parent explicitly: this goroutine has no safe notion
		// of the process-global current session.
		if err := t.emitForSession(context.Background(), info.parent, session.EventSubagentEnd,
			session.NewSubagentEnd(childID, info.provider, res.StopReason, res.Output)); err == nil {
			t.mu.Lock()
			wake := t.onCompletionWake
			t.mu.Unlock()
			if wake != nil && strings.TrimSpace(info.parent) != "" {
				// The durable end event is published first. A failed wake
				// therefore cannot make the live Agent claim a result that is
				// absent from the replay source; a later status/list or cold
				// restore can recover it.
				_ = wake(context.Background(), info.parent, childID, res)
			}
		}
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

// ConcurrencySafe marks sibling delegations as safe to admit to the loop's
// rolling pool. The tool only creates an independently owned child and the
// bundle serializes its own registries; the parent transcript still commits
// each result in model order.
func (SubagentSpawnTool) ConcurrencySafe(any) bool { return true }

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
			"agentOptions": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"model":     map[string]any{"type": "string", "minLength": 1},
					"maxTokens": map[string]any{"type": "integer", "minimum": 1},
				},
				"additionalProperties": false,
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
		Description     string             `json:"description"`
		Prompt          string             `json:"prompt"`
		RunInBackground *bool              `json:"run_in_background"`
		AgentOptions    *agentOptionsInput `json:"agentOptions"`
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
	model, maxTokens, err := resolveAgentOptions(a.AgentOptions)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.Name(), err)
	}
	parent := t.t.callerSession(ctx)
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
			Model: model, MaxTokens: maxTokens,
			Continuable: continuable, InheritParentContext: inherit,
		})
	}
	if background && !t.continuable {
		if t.t.jobs == nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: background jobs unavailable", t.Name())
		}
		jobID, err := t.t.jobs.Start(ctx, jobs.JobStart{
			Kind: jobs.Kind("subagent"), Label: a.Description, OwnerSession: parent,
			Correlation: jobs.CorrelationFromContext(ctx),
			Run: func(jobCtx context.Context) (jobs.JobOutcome, error) {
				run, err := start(jobCtx)
				if err != nil {
					return jobs.JobOutcome{Status: jobs.StatusFailed, Detail: err.Error()}, nil
				}
				t.t.register(run.ID, &childInfo{run: run, provider: provider, label: a.Description, parent: parent})
				if err := t.t.emitContext(jobCtx, session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, a.Description)); err != nil {
					_ = run.Cancel("subagent/start persistence failed")
					return jobs.JobOutcome{Status: jobs.StatusFailed, Detail: "subagent/start persistence failed: " + err.Error()}, nil
				}
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
	if err := t.t.emitContext(ctx, session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, a.Description)); err != nil {
		_ = run.Cancel("subagent/start persistence failed")
		return agenttools.ToolResult{}, fmt.Errorf("%s: persist event: %w", t.Name(), err)
	}
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

func (SubagentTeammateTool) Name() string             { return ToolTeammateName }
func (SubagentTeammateTool) ConcurrencySafe(any) bool { return true }
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
	parent := t.t.callerSession(ctx)
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolTeammateName)
	}
	contextKind := a.Context
	if contextKind == "" {
		contextKind = "fresh"
	}
	t.t.mu.Lock()
	provisioner := t.t.teammate
	t.t.mu.Unlock()
	if provisioner != nil {
		member, err := provisioner(ctx, parent, a.Name, a.Description, a.Prompt, contextKind)
		if err != nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", ToolTeammateName, err)
		}
		if member.ID == "" || member.Run == nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: provisioner returned an incomplete member", ToolTeammateName)
		}
		t.t.register(member.ID, &childInfo{run: member.Run, provider: member.Provider, label: a.Name + ": " + a.Description, parent: parent})
		memberStatus := member.Status
		if memberStatus == "" {
			memberStatus = "running"
		}
		if err := t.t.emitContext(ctx, session.EventSubagentStart, session.NewSubagentStart(member.ID, member.Provider, parent, a.Description)); err != nil {
			_ = member.Run.Cancel("subagent/start persistence failed")
			return agenttools.ToolResult{}, fmt.Errorf("%s: persist event: %w", ToolTeammateName, err)
		}
		memberView := map[string]any{
			"id": member.ID, "name": a.Name, "role": "teammate", "status": memberStatus,
			"description": a.Description, "provider": member.Provider, "context": contextKind,
			"diagnostics": []string{},
		}
		return agenttools.ToolResult{Value: map[string]any{"member": memberView}, Output: fmt.Sprintf("started teammate %s (%s)", a.Name, member.ID)}, nil
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
	if err := t.t.emitContext(ctx, session.EventSubagentStart, session.NewSubagentStart(run.ID, provider, parent, a.Description)); err != nil {
		_ = run.Cancel("subagent/start persistence failed")
		return agenttools.ToolResult{}, fmt.Errorf("%s: persist event: %w", ToolTeammateName, err)
	}
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
	parent := t.t.callerSession(ctx)
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", t.name)
	}
	info, err := t.t.directChild(a.Target, parent)
	var id string
	var send func(context.Context, string) error
	if err == nil {
		if info.run.Send == nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, ErrNotContinuable)
		}
		send = info.run.Send
		if !t.wakeup && info.run.SendQuiet != nil {
			send = info.run.SendQuiet
		}
		id = info.run.ID
	} else {
		t.t.mu.Lock()
		directory := t.t.directory
		t.t.mu.Unlock()
		if directory == nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
		}
		member, memberErr := directory.Direct(ctx, parent, a.Target)
		if memberErr != nil {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
		}
		if !member.Continuable {
			return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, ErrNotContinuable)
		}
		if t.wakeup {
			send = member.Followup
		} else {
			send = member.SendQuiet
			if send == nil {
				member := member
				send = func(ctx context.Context, message string) error {
					return member.Send(ctx, message, nil)
				}
			}
		}
		id = member.ID
	}
	if send == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, ErrNotContinuable)
	}
	if err := send(ctx, a.Message); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	t.t.signalChange()
	messageID := t.t.nextMessageID(id)
	return agenttools.ToolResult{
		Value:  map[string]any{"messageId": messageID, "status": "queued"},
		Output: fmt.Sprintf("message queued for %s as %s", id, messageID),
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

// CancellationAware is explicit: wait_agent selects on ctx.Done while its
// bounded timeout and roster poll remain active.
func (SubagentWaitTool) CancellationAware() bool { return true }
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
	parent := t.t.callerSession(ctx)
	if parent == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolWaitName)
	}
	children, err := t.t.listChildren(ctx, parent)
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
	_, hasDirectory := t.t.directory.(TeammateDirectory)
	t.t.mu.Unlock()
	timer := time.NewTimer(time.Duration(a.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	var poll *time.Ticker
	if hasDirectory {
		// Rebound Agent handles do not share the provider's change channel.
		// Polling the authoritative roster state gives wait_agent a bounded,
		// leak-free completion signal after restart as well.
		poll = time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
	}
	select {
	case <-ctx.Done():
		return agenttools.ToolResult{}, ctx.Err()
	case <-changes:
		value := map[string]any{"timedOut": false}
		return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
	case <-timer.C:
		value := map[string]any{"timedOut": true}
		return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
	case <-tickerChannel(poll):
		current, listErr := t.t.listChildren(ctx, parent)
		if listErr == nil && !sameChildStates(children, current) {
			value := map[string]any{"timedOut": false}
			return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
		}
		// A ticker case is only a sampling opportunity. Continue waiting for a
		// real change, cancellation, or the caller's timeout.
		return t.t.waitForChange(ctx, parent, children, changes, timer, poll)
	}
}

func tickerChannel(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}

func (t *SubagentTools) waitForChange(ctx context.Context, parent string, previous []ChildSummary, changes <-chan struct{}, timer *time.Timer, poll *time.Ticker) (agenttools.ToolResult, error) {
	for {
		select {
		case <-ctx.Done():
			return agenttools.ToolResult{}, ctx.Err()
		case <-changes:
			value := map[string]any{"timedOut": false}
			return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
		case <-timer.C:
			value := map[string]any{"timedOut": true}
			return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
		case <-tickerChannel(poll):
			current, err := t.listChildren(ctx, parent)
			if err == nil && !sameChildStates(previous, current) {
				value := map[string]any{"timedOut": false}
				return agenttools.ToolResult{Value: value, Output: valueJSON(value)}, nil
			}
		}
	}
}

func sameChildStates(a, b []ChildSummary) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Running != b[i].Running || a[i].Continuable != b[i].Continuable {
			return false
		}
	}
	return true
}

// SubagentForkTool is the DSH-named one-shot fork delegation entry. The
// provider is separate in production and receives the parent context seed.
type SubagentForkTool struct{ t *SubagentTools }

func (SubagentForkTool) Name() string             { return ToolForkName }
func (SubagentForkTool) ConcurrencySafe(any) bool { return true }
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
	parent := t.t.callerSession(ctx)
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
	if t.t.callerSession(ctx) == "" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: requires a calling agent", ToolInterruptName)
	}
	if !t.t.isDescendant(a.ID, t.t.callerSession(ctx)) {
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
	childID        string
	parent         string
	deliver        func(childID, parentID, output string) (string, error)
	deliverContext func(context.Context, string, string, string) (string, error)
}

func newChildReportTool(childID, parent string, deliver func(string, string, string) (string, error)) childReportTool {
	return childReportTool{childID: childID, parent: parent, deliver: deliver}
}

func newChildReportToolWithContext(childID, parent string, deliver func(context.Context, string, string, string) (string, error)) childReportTool {
	return childReportTool{childID: childID, parent: parent, deliverContext: deliver}
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
	if t.deliver == nil && t.deliverContext == nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: direct parent is not live; report was not delivered", ToolReportName)
	}
	var messageID string
	var err error
	if t.deliverContext != nil {
		messageID, err = t.deliverContext(ctx, t.childID, t.parent, input.Output)
	} else {
		messageID, err = t.deliver(t.childID, t.parent, input.Output)
	}
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
	parent := t.t.callerSession(ctx)
	if parent == "" {
		return "", fmt.Errorf("%s: requires a calling agent", ToolResumeName)
	}
	// Resume is a mutating operation on a durable child. The supplied id is
	// untrusted input and must remain within the caller's descendant boundary,
	// including after a restart when the process-local child map is empty.
	if !t.t.isDescendant(a.ID, parent) {
		return "", fmt.Errorf("%s: subagent %q is not a descendant of the calling agent", ToolResumeName, a.ID)
	}
	provider := strings.TrimSpace(a.Provider)
	if info, _, ok := t.t.lookup(a.ID); ok && info != nil && info.provider != "" {
		if provider != "" && provider != info.provider {
			return "", fmt.Errorf("%s: provider %q does not match durable provider %q", ToolResumeName, provider, info.provider)
		}
		provider = info.provider
	} else if provider == "" {
		provider = defaultProviderName
	}
	run, err := t.t.rt.Resume(ctx, provider, a.ID, a.Message, a.Continuable)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ToolResumeName, err)
	}
	t.t.register(run.ID, &childInfo{run: run, provider: provider, label: "resumed", parent: parent})
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
	parent := t.t.callerSession(ctx)
	if parent == "" {
		return "", fmt.Errorf("subagent_status: requires a calling agent")
	}
	if !t.t.isDescendant(a.ID, parent) {
		return "", fmt.Errorf("subagent_status: agent %q is not a descendant of the calling agent", a.ID)
	}
	info, res, settled := t.t.lookup(a.ID)
	if info == nil {
		t.t.mu.Lock()
		directory := t.t.directory
		t.t.mu.Unlock()
		if directory == nil {
			return "", fmt.Errorf("subagent_status: unknown subagent %q", a.ID)
		}
		member, err := directory.Direct(ctx, parent, a.ID)
		if err != nil {
			return "", fmt.Errorf("subagent_status: unknown subagent %q", a.ID)
		}
		state := "idle"
		if member.Running {
			state = "running"
		}
		return fmt.Sprintf("subagent %s: %s (provider=agent-registry, label=%q)", member.ID, state, member.Label), nil
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
	parent := t.t.callerSession(ctx)
	if parent == "" {
		return "", fmt.Errorf("subagent_cancel: requires a calling agent")
	}
	if !t.t.isDescendant(a.ID, parent) {
		return "", fmt.Errorf("subagent_cancel: agent %q is not a descendant of the calling agent", a.ID)
	}
	info, _, _ := t.t.lookup(a.ID)
	if info == nil {
		t.t.mu.Lock()
		directory := t.t.directory
		t.t.mu.Unlock()
		if directory == nil {
			return "", fmt.Errorf("subagent_cancel: unknown subagent %q", a.ID)
		}
		member, err := directory.Direct(ctx, parent, a.ID)
		if err != nil || member.Cancel == nil {
			return "", fmt.Errorf("subagent_cancel: unknown subagent %q", a.ID)
		}
		if err := member.Cancel(a.Reason); err != nil {
			return "already-finished", nil
		}
		return "requested", nil
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
		parent = t.t.callerSession(ctx)
	}
	caller := t.t.callerSession(ctx)
	if caller == "" {
		return "", fmt.Errorf("subagent_list: requires a calling agent")
	}
	if parent != caller && !t.t.isDescendant(parent, caller) {
		return "", fmt.Errorf("subagent_list: parent %q is outside the calling agent scope", parent)
	}
	children, err := t.t.listChildren(ctx, parent)
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
	parent := t.t.callerSession(ctx)
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
	children, err := t.listChildren(ctx, parent)
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
