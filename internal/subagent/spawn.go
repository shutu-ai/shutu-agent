// SpawnProvider is the default in-process subagent provider (ADR
// 2026-08-18-m5-agent-core.md 决策 ②: spawn-in-process). A spawn creates a
// brand-new independent child session and a brand-new independent loop instance
// to drive the child agent — the loop is a library, instantiated once per child
// (D4: the child is just "another session + another loop instance", composed
// here, never by modifying the loop). The parent_session lineage and the
// delegation depth are tracked in the provider's in-memory registry; the child
// session log is independent, so the parent session log is never polluted.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// Deps wires the SpawnProvider to the core components it reuses for every child
// (the composition root supplies them at construction, M5b-2 wiring; tests use
// a fake LLM). Log is the parent/host session log the provider is bound to —
// it is never appended to by the provider (the child owns an independent log);
// M5b-2's subagent/* event recording will surface the parent lineage through
// it. Each spawned child gets its own fresh session.New() log.
type Deps struct {
	Log *session.Log
	// ParentLogFor resolves a durable parent runtime by session id. Fork uses
	// this only when the parent is not another live child owned by this
	// provider. A nil result means that the parent is unavailable; callers must
	// not silently substitute the process-global Log for an addressed runtime.
	ParentLogFor func(context.Context, string) *session.Log
	// BindSessionLog publishes a newly-created child log to the host runtime
	// index so session-aware nested tools can resolve the child runtime.
	BindSessionLog func(string, *session.Log)
	LLM            llm.LLM
	// LLMFor resolves the provider for a child using its parent's runtime
	// session. It takes precedence over LLM when supplied; the fixed field is
	// retained for library callers and legacy tests.
	LLMFor func(context.Context, string) llm.LLM
	Tools  *tools.Registry
	// ToolsFor resolves the parent session's scoped registry. It is used before
	// ToolFilter/structured-output overlays are applied to the child.
	ToolsFor func(context.Context, string) *tools.Registry
	Prompt   *prompt.Builder
	// PromptFor resolves the parent session's prompt snapshot.
	PromptFor func(context.Context, string) *prompt.Builder
	Model     string
	// ModelFor mirrors LLMFor for per-session model selection.
	ModelFor func(context.Context, string) string
	// MaxTokens is the default output-token cap for a root child. A child
	// inherits its parent's resolved cap unless StartRequest.MaxTokens overrides
	// it; zero leaves the provider default in control.
	MaxTokens int
	// MaxTokensFor resolves a cap inherited from a host-owned parent that is
	// not represented in this provider's in-memory child map. It is consulted
	// only for root children and never overrides an explicit request cap.
	MaxTokensFor func(context.Context, string) int
	// Store durably records the independent child session when provided. It is
	// optional so library users and existing tests can remain in-memory.
	Store store.Store
	// Report accepts a child-scoped report and returns its parent message id.
	// It is optional so the provider remains usable without host delivery.
	Report func(childID, parentID, output string) (string, error)
	// ReportContext is the context-aware variant used by child tool execution.
	// It lets the host preserve correlation/cancellation while still enforcing
	// the registered parent identity.
	ReportContext func(context.Context, string, string, string) (string, error)
}

// SpawnProvider spawns a brand-new child session + child loop for every Start.
// It is safe for concurrent use.
type SpawnProvider struct {
	deps                 Deps
	name                 string
	idPrefix             string
	inheritParentContext bool

	mu        sync.Mutex
	children  map[string]*childRun
	nextID    int
	closed    bool
	closeDone chan struct{}
}

// childRun is the provider's per-child record: the independent child log, the
// parent lineage, the delegation depth, and the settle/cancel bookkeeping. It
// is never handed out; callers receive fresh values (Run closures,
// ChildSummary, ChildLog).
type childRun struct {
	id          string
	label       string
	parent      string
	depth       int
	log         *session.Log
	cancel      context.CancelFunc  // cancels the child loop context (set in Start)
	done        chan struct{}       // closed once the child settles
	inbox       chan pendingMessage // waking follow-up messages
	continuable bool
	maxTokens   int    // resolved output-token cap inherited by nested children
	seedSeq     uint64 // parent-history watermark for fork result derivation

	mu            sync.Mutex
	cancelReason  string
	result        Result
	structured    any
	structuredSet bool
	settled       bool
	quietInbox    []string // accepted context waiting for a waking follow-up
}

// pendingMessage carries a waking follow-up and the Web provenance that must
// survive until the child loop admits it as an ordinary user message.
type pendingMessage struct {
	message  string
	content  []llm.ContentBlock
	metadata map[string]string
}

// NewSpawnProvider returns a SpawnProvider bound to the given core components.
func NewSpawnProvider(deps Deps) *SpawnProvider {
	return &SpawnProvider{deps: deps, name: "spawn", idPrefix: "spawn", children: map[string]*childRun{}, closeDone: make(chan struct{})}
}

// NewForkProvider returns the independent fork backend. It uses the same local
// runner as spawn, but has its own provider identity and seeds the child from
// the parent's completed event history when available.
func NewForkProvider(deps Deps) *SpawnProvider {
	return &SpawnProvider{deps: deps, name: "fork", idPrefix: "fork", inheritParentContext: true, children: map[string]*childRun{}, closeDone: make(chan struct{})}
}

// Name returns the provider name ("spawn"), the default subagent provider.
func (p *SpawnProvider) Name() string {
	if p.name == "" {
		return "spawn"
	}
	return p.name
}

// Capabilities declares what the spawn provider actually enforces: delegation
// depth (MaxDepth ⇒ ErrDepthExceeded). ToolFilter/Persona application and
// structured output is captured through a child-scoped structured_output tool.
func (p *SpawnProvider) Capabilities() Capabilities {
	return Capabilities{OutputSchema: true, DepthLimit: true, ToolFilter: true, Persona: true, ContextInheritance: p.inheritParentContext}
}

// Start registers a brand-new child session (depth = parent depth + 1, tracked
// in the provider's registry), rejects an over-deep spawn, and drives the child
// with a fresh loop instance in a background goroutine. It does not block: the
// returned Run's Result awaits the terminal outcome. The child loop runs on an
// independent context, so it outlives Start's ctx; Cancel/Close cancel it.
func (p *SpawnProvider) Start(ctx context.Context, req StartRequest) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("%w: prompt is required", ErrInvalidRequest)
	}
	if req.MaxTokens < 0 {
		return nil, fmt.Errorf("%w: max_tokens must be non-negative", ErrInvalidRequest)
	}
	if req.OutputSchema != nil {
		if root, ok := req.OutputSchema["type"].(string); !ok || root != "object" {
			return nil, fmt.Errorf("%w: output schema must have an object root", ErrInvalidRequest)
		}
		if p.deps.Tools == nil {
			return nil, fmt.Errorf("%w: structured output requires a tool registry", ErrInvalidRequest)
		}
		probe := tools.New()
		if err := probe.Register(structuredOutputTool{schema: req.OutputSchema}); err != nil {
			return nil, fmt.Errorf("%w: invalid output schema: %v", ErrInvalidRequest, err)
		}
	}
	if err := p.syncNextID(ctx); err != nil {
		return nil, err
	}
	req.Prompt = withAcceptance(req.Prompt, req.AcceptanceCriteria)
	if req.OutputSchema != nil {
		req.Prompt = structuredPrompt(req.Prompt, req.OutputSchema)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrProviderClosed
	}
	parentDepth := 0
	parentMaxTokens := 0
	if parent, ok := p.children[req.ParentSessionID]; ok {
		parentDepth = parent.depth
		parentMaxTokens = parent.maxTokens
	} else if p.deps.MaxTokensFor != nil && strings.TrimSpace(req.ParentSessionID) != "" {
		parentMaxTokens = p.deps.MaxTokensFor(ctx, req.ParentSessionID)
	}
	depth := parentDepth + 1
	if req.MaxDepth > 0 && depth > req.MaxDepth {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: depth %d exceeds max depth %d (parent %q)",
			ErrDepthExceeded, depth, req.MaxDepth, req.ParentSessionID)
	}
	prefix := p.idPrefix
	if prefix == "" {
		prefix = "spawn"
	}
	id, err := store.GenerateReservedID(ctx, p.deps.Store, "session", func() (string, error) {
		p.nextID++
		return fmt.Sprintf("%s-%d", prefix, p.nextID), nil
	})
	if err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("subagent: reserve child session id: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	child := &childRun{
		id:          id,
		label:       req.Label,
		parent:      req.ParentSessionID,
		depth:       depth,
		log:         session.New(),
		cancel:      cancel,
		done:        make(chan struct{}),
		inbox:       make(chan pendingMessage, 16),
		continuable: req.Continuable,
	}
	if req.MaxTokens > 0 {
		child.maxTokens = req.MaxTokens
	} else if parentMaxTokens > 0 {
		child.maxTokens = parentMaxTokens
	} else {
		child.maxTokens = p.deps.MaxTokens
	}
	var parentEvents []session.Event
	if req.InheritParentContext {
		if parent, ok := p.children[req.ParentSessionID]; ok {
			parentEvents = parent.log.Events()
		}
	}
	p.children[id] = child
	p.mu.Unlock()
	// A fork from a live application Agent is not represented in this
	// provider's child map. Resolve that addressed parent explicitly instead
	// of falling back to the mutable legacy current session. The empty-parent
	// case remains a compatibility path for library callers that intentionally
	// bind the provider to one host log.
	if req.InheritParentContext && len(parentEvents) == 0 {
		parentID := strings.TrimSpace(req.ParentSessionID)
		if req.ParentSessionID != "" {
			if p.deps.ParentLogFor != nil {
				if parentLog := p.deps.ParentLogFor(ctx, req.ParentSessionID); parentLog != nil {
					parentEvents = parentLog.Events()
				}
			}
		} else if p.deps.ParentLogFor != nil {
			parentID = runtimectx.SessionID(ctx)
			if parentID != "" {
				if parentLog := p.deps.ParentLogFor(ctx, parentID); parentLog != nil {
					parentEvents = parentLog.Events()
				}
			}
		} else if p.deps.Log != nil {
			parentEvents = p.deps.Log.Events()
		}
		// A parent Agent may be cold after restart and therefore absent from the
		// live runtime index. The durable session is still authoritative for a
		// fork seed; use the raw tail when available so an in-flight parent turn
		// can be excluded by completedTurnPrefix below.
		var parentLoadErr error
		if len(parentEvents) == 0 && parentID != "" && p.deps.Store != nil {
			if raw, ok := p.deps.Store.(store.SessionRawStore); ok {
				parentEvents, parentLoadErr = raw.LoadSessionRaw(ctx, parentID)
			} else {
				parentEvents, parentLoadErr = p.deps.Store.LoadSession(ctx, parentID)
			}
			if parentLoadErr != nil && !errors.Is(parentLoadErr, store.ErrNotFound) {
				cancel()
				p.mu.Lock()
				delete(p.children, id)
				p.mu.Unlock()
				return nil, fmt.Errorf("subagent: load fork parent %q: %w", parentID, parentLoadErr)
			}
		}
	}
	if req.InheritParentContext {
		parentEvents = completedTurnPrefix(parentEvents)
	}
	if len(parentEvents) > 0 {
		if err := child.log.Restore(parentEvents); err != nil {
			cancel()
			p.mu.Lock()
			delete(p.children, id)
			p.mu.Unlock()
			return nil, fmt.Errorf("subagent: seed fork history %q: %w", req.ParentSessionID, err)
		}
		child.seedSeq = parentEvents[len(parentEvents)-1].Seq
	}
	if p.deps.Store != nil {
		created := time.Now().UTC()
		header := store.SessionHeader{
			ID: id, CreatedAt: created, Parent: child.parent,
			SeedLength: len(parentEvents), Origin: "subagent", DelegationDepth: child.depth,
		}
		var committedStart *session.Event
		// Prefer the transaction that publishes the header and fork seed as one
		// durable unit. The fallback keeps older Store implementations usable,
		// but still writes the seed before exposing the live child sink.
		if atomic, ok := p.deps.Store.(store.SessionCreateStore); ok {
			// The first lifecycle fact is part of publication. This closes the
			// remaining window in which a crash could leave a durable child row
			// and seed but no subagent/start identity.
			startData, marshalErr := json.Marshal(session.NewSubagentStartWithDepth(id, p.Name(), child.parent, child.label, child.depth))
			if marshalErr != nil {
				cancel()
				p.mu.Lock()
				delete(p.children, id)
				p.mu.Unlock()
				return nil, fmt.Errorf("subagent: encode child metadata %q: %w", id, marshalErr)
			}
			start := session.Event{
				Seq: child.log.NextSeq(), Type: session.EventSubagentStart,
				At: time.Now().UTC(), Version: session.EventVersion,
				Data: startData,
			}
			durableEvents := append(append([]session.Event(nil), parentEvents...), start)
			// The lifecycle marker is published in the same transaction, but it
			// is not inherited parent context. Keep SeedLength as the exact
			// parent-prefix boundary so replay and lineage consumers agree on
			// where the forked context ends.
			header.SeedLength = len(parentEvents)
			if err := atomic.CreateSessionWithOptions(ctx, id, created, store.SessionCreateOptions{Header: header}, durableEvents); err != nil {
				cancel()
				p.mu.Lock()
				delete(p.children, id)
				p.mu.Unlock()
				return nil, fmt.Errorf("subagent: persist child session %q: %w", id, err)
			}
			committedStart = &start
		} else if atomic, ok := p.deps.Store.(store.SessionCreateEventStore); ok {
			if err := atomic.CreateSessionWithEvents(ctx, id, created, header, parentEvents); err != nil {
				cancel()
				p.mu.Lock()
				delete(p.children, id)
				p.mu.Unlock()
				return nil, fmt.Errorf("subagent: persist child session %q: %w", id, err)
			}
		} else {
			if err := p.deps.Store.CreateSession(ctx, id, created); err != nil {
				cancel()
				p.mu.Lock()
				delete(p.children, id)
				p.mu.Unlock()
				return nil, fmt.Errorf("subagent: persist child session %q: %w", id, err)
			}
			if lineage, ok := p.deps.Store.(store.SessionLineageStore); ok {
				if err := lineage.SetSessionHeader(ctx, id, header); err != nil {
					cancel()
					p.mu.Lock()
					delete(p.children, id)
					p.mu.Unlock()
					return nil, fmt.Errorf("subagent: persist child header %q: %w", id, err)
				}
			}
			if len(parentEvents) > 0 {
				if err := p.deps.Store.AppendEvents(ctx, id, parentEvents); err != nil {
					cancel()
					p.mu.Lock()
					delete(p.children, id)
					p.mu.Unlock()
					return nil, fmt.Errorf("subagent: persist child seed %q: %w", id, err)
				}
			}
		}
		child.log.SetSink(func(ev session.Event) error {
			return p.deps.Store.AppendEvents(context.Background(), id, []session.Event{ev})
		})
		var startErr error
		if committedStart != nil {
			startErr = child.log.AppendPersisted(*committedStart)
		} else {
			_, startErr = child.log.Append(session.EventSubagentStart,
				session.NewSubagentStartWithDepth(id, p.Name(), child.parent, child.label, child.depth))
		}
		if startErr != nil {
			cancel()
			p.mu.Lock()
			delete(p.children, id)
			p.mu.Unlock()
			return nil, fmt.Errorf("subagent: persist child metadata %q: %w", id, startErr)
		}
	}
	// Publish the child only after seed restoration, durable header/seed
	// publication, and the first lifecycle event have all succeeded. Publishing
	// earlier leaves a runtime-index entry behind when any initialization step
	// fails, allowing a later nested tool to resolve a ghost child.
	if p.deps.BindSessionLog != nil {
		p.deps.BindSessionLog(id, child.log)
	}

	go p.runChild(child, req, runCtx)
	return &Run{
		ID:                      id,
		Result:                  p.resultFunc(child),
		Send:                    p.sendFunc(child),
		SendWithMetadata:        p.sendWithMetadataFunc(child),
		SendContentWithMetadata: p.sendContentWithMetadataFunc(child),
		SendQuiet:               p.sendQuietFunc(child),
		Cancel:                  p.cancelFunc(child),
	}, nil
}

// syncNextID prevents a freshly-created provider from reusing an in-memory
// child id that already exists after a process restart. The durable create
// transaction remains authoritative for true cross-process races; this scan
// closes the common cold-start collision without changing the lightweight
// in-memory provider contract.
func (p *SpawnProvider) syncNextID(ctx context.Context) error {
	if p.deps.Store == nil {
		return nil
	}
	metas, err := p.deps.Store.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("subagent: inspect existing child sessions: %w", err)
	}
	prefix := p.idPrefix
	if prefix == "" {
		prefix = "spawn"
	}
	maxID := 0
	for _, meta := range metas {
		if !strings.HasPrefix(meta.ID, prefix+"-") {
			continue
		}
		if n := parseSpawnID(meta.ID); n > maxID {
			maxID = n
		}
	}
	p.mu.Lock()
	if maxID > p.nextID {
		p.nextID = maxID
	}
	p.mu.Unlock()
	return nil
}

// completedTurnPrefix returns the contiguous parent history through the last
// completed turn. The live parent may currently be inside a tool-call turn;
// copying that open tail would make the child seed fail session replay and
// would expose an in-flight operation to a new authority boundary.
func completedTurnPrefix(events []session.Event) []session.Event {
	lastEnd := -1
	for i, event := range events {
		if event.Type == session.EventTurnEnd {
			lastEnd = i
		}
	}
	if lastEnd < 0 {
		return nil
	}
	prefix := make([]session.Event, lastEnd+1)
	copy(prefix, events[:lastEnd+1])
	return prefix
}

// Resume rehydrates a persisted spawn session and runs one new turn against
// the restored history. Only the local provider supports this operation;
// external providers remain explicitly non-resumable.
func (p *SpawnProvider) Resume(ctx context.Context, sessionID, message string, continuable bool) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.deps.Store == nil {
		return nil, fmt.Errorf("%w: spawn provider has no Store", ErrCapabilityNotSupported)
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("%w: session id and prompt are required", ErrInvalidRequest)
	}
	events, err := p.deps.Store.LoadSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("subagent: load child session %q: %w", sessionID, err)
	}
	var meta struct {
		ID            string `json:"id"`
		Provider      string `json:"provider"`
		ParentSession string `json:"parentSession"`
		Label         string `json:"label"`
		Depth         int    `json:"depth"`
	}
	maxTokens := 0
	for _, ev := range events {
		if ev.Type == session.EventSubagentStart && json.Unmarshal(ev.Data, &meta) == nil {
			continue
		}
		if ev.Type == session.EventRequestHeader {
			var header struct {
				MaxTokens int `json:"maxTokens"`
			}
			if json.Unmarshal(ev.Data, &header) == nil && header.MaxTokens > 0 {
				maxTokens = header.MaxTokens
			}
		}
	}
	if meta.ID != sessionID || meta.Provider != p.Name() {
		return nil, fmt.Errorf("%w: session %q is not a persisted spawn child", ErrInvalidRequest, sessionID)
	}
	if meta.Depth <= 0 {
		meta.Depth = 1
	}
	log := session.New()
	if err := log.Restore(events); err != nil {
		return nil, fmt.Errorf("subagent: restore child session %q: %w", sessionID, err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	child := &childRun{
		id: sessionID, label: meta.Label, parent: meta.ParentSession, depth: meta.Depth,
		log: log, cancel: cancel, done: make(chan struct{}), inbox: make(chan pendingMessage, 16), continuable: continuable,
		maxTokens: maxTokens,
	}
	// A resumed run must derive its result from the new turn only. Otherwise a
	// no-content completion can accidentally return the previous persisted
	// assistant answer (the same seed-boundary rule used by fork).
	if len(events) > 0 {
		child.seedSeq = events[len(events)-1].Seq
	}
	log.SetSink(func(ev session.Event) error {
		return p.deps.Store.AppendEvents(context.Background(), sessionID, []session.Event{ev})
	})
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		return nil, ErrProviderClosed
	}
	if _, exists := p.children[sessionID]; exists {
		p.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("subagent: child %q is already active", sessionID)
	}
	p.children[sessionID] = child
	if n := parseSpawnID(sessionID); n > p.nextID {
		p.nextID = n
	}
	p.mu.Unlock()
	if p.deps.BindSessionLog != nil {
		p.deps.BindSessionLog(sessionID, log)
	}
	go p.runChild(child, StartRequest{Prompt: message, Continuable: continuable}, runCtx)
	return &Run{
		ID: sessionID, Result: p.resultFunc(child), Send: p.sendFunc(child),
		SendWithMetadata: p.sendWithMetadataFunc(child), SendContentWithMetadata: p.sendContentWithMetadataFunc(child),
		SendQuiet: p.sendQuietFunc(child), Cancel: p.cancelFunc(child),
	}, nil
}

func parseSpawnID(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "spawn-%d", &n); err == nil {
		return n
	}
	if _, err := fmt.Sscanf(id, "fork-%d", &n); err != nil {
		return 0
	}
	return n
}

// runChild drives one child agent to completion against its own independent
// log with its own loop instance, then settles the child's result. A panic is
// contained so a misbehaving child can never crash the provider (fail-open).
func (p *SpawnProvider) runChild(child *childRun, req StartRequest, runCtx context.Context) {
	defer close(child.done)
	defer func() {
		if r := recover(); r != nil {
			p.settle(child, Result{StopReason: StopError})
		}
	}()
	model := p.deps.Model
	if p.deps.ModelFor != nil {
		if resolved := p.deps.ModelFor(runCtx, child.parent); resolved != "" {
			model = resolved
		}
	}
	if req.Model != "" {
		model = req.Model
	}
	childLLM := p.deps.LLM
	if p.deps.LLMFor != nil {
		if resolved := p.deps.LLMFor(runCtx, child.parent); resolved != nil {
			childLLM = resolved
		}
	}
	childTools := p.deps.Tools
	if p.deps.ToolsFor != nil {
		if resolved := p.deps.ToolsFor(runCtx, child.parent); resolved != nil {
			childTools = resolved
		}
	}
	if len(req.ToolFilter) > 0 {
		if childTools == nil {
			p.settle(child, Result{StopReason: StopError})
			return
		}
		filtered := childTools.Clone()
		policy := filtered.Policy()
		policy.Enabled = append([]string(nil), req.ToolFilter...)
		filtered.SetPolicy(policy)
		childTools = filtered
	}
	if req.Continuable && (p.deps.Report != nil || p.deps.ReportContext != nil) && childTools != nil {
		childTools = childTools.Clone()
		var reportTool childReportTool
		if p.deps.ReportContext != nil {
			reportTool = newChildReportToolWithContext(child.id, child.parent, p.deps.ReportContext)
		} else {
			reportTool = newChildReportTool(child.id, child.parent, p.deps.Report)
		}
		if err := childTools.Register(reportTool); err != nil {
			p.settle(child, Result{StopReason: StopError})
			return
		}
		childTools.Allow(ToolReportName)
	}
	if req.OutputSchema != nil {
		if childTools == p.deps.Tools {
			childTools = p.deps.Tools.Clone()
		}
		if err := childTools.Register(structuredOutputTool{
			schema: req.OutputSchema,
			capture: func(value any) error {
				child.mu.Lock()
				defer child.mu.Unlock()
				if child.structuredSet {
					return fmt.Errorf("structured output already recorded")
				}
				child.structured = value
				child.structuredSet = true
				return nil
			},
		}); err != nil {
			p.settle(child, Result{StopReason: StopError})
			return
		}
		childTools.Allow(structuredOutputToolName)
	}
	if childTools != nil {
		childTools.SetOwner(tools.Owner{SessionID: child.id, NextSeq: child.log.NextSeq})
	}
	childPrompt := p.deps.Prompt
	if p.deps.PromptFor != nil {
		if resolved := p.deps.PromptFor(runCtx, child.parent); resolved != nil {
			childPrompt = resolved
		}
	}
	if childPrompt != nil && (req.Persona != "" || childTools != p.deps.Tools || req.Continuable || req.OutputSchema != nil) {
		childPrompt = childPrompt.Clone()
		if req.Persona != "" {
			childPrompt.Add(prompt.Section{Name: "persona", Order: -100, Text: req.Persona})
		}
		childPrompt.SetTools(func() []llm.ToolSchema { return childTools.VisibleSpecs() })
	}
	lp := loop.New(loop.Config{
		LLM:              childLLM,
		Log:              child.log,
		Tools:            childTools,
		ToolSpecs:        func() []llm.ToolSchema { return childTools.VisibleSpecs() },
		Prompt:           childPrompt,
		Model:            model,
		MaxTokens:        child.maxTokens,
		RuntimeSessionID: child.id,
		RuntimeEmit: func(typ string, data any) error {
			_, err := child.log.Append(typ, data)
			return err
		},
	})
	pending := pendingMessage{message: req.Prompt}
	for {
		runErr := lp.RunMessages(runCtx, []llm.Message{childInputMessage(pending)})
		if !child.continuable || runErr != nil {
			p.settle(child, p.deriveResult(child, runErr))
			return
		}
		select {
		case next := <-child.inbox:
			if len(next.content) > 0 {
				next.content = child.withQuietContent(next.content)
			} else {
				next.message = child.withQuiet(next.message)
			}
			pending = next
		case <-runCtx.Done():
			p.settle(child, p.deriveResult(child, runCtx.Err()))
			return
		}
	}
}

// childInputMessage projects a pending provider queue item as the canonical
// ordinary user message. Web provenance rides in durable source metadata only.
func childInputMessage(pending pendingMessage) llm.Message {
	if len(pending.content) > 0 {
		message := llm.Message{Role: llm.RoleUser, Content: append([]llm.ContentBlock(nil), pending.content...)}
		message.SourceRPCID = pending.metadata["rpc_id"]
		message.SourceClientTimeZone = pending.metadata["client_time_zone"]
		return message
	}
	message := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.Text(pending.message)},
	}
	message.SourceRPCID = pending.metadata["rpc_id"]
	message.SourceClientTimeZone = pending.metadata["client_time_zone"]
	return message
}

func (c *childRun) withQuietContent(content []llm.ContentBlock) []llm.ContentBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.quietInbox) == 0 {
		return content
	}
	parts := append([]string(nil), c.quietInbox...)
	c.quietInbox = nil
	quiet := llm.Text(strings.Join(parts, "\n\n"))
	return append([]llm.ContentBlock{quiet}, content...)
}

func (c *childRun) withQuiet(message string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.quietInbox) == 0 {
		return message
	}
	parts := append([]string(nil), c.quietInbox...)
	c.quietInbox = nil
	parts = append(parts, message)
	return strings.Join(parts, "\n\n")
}

// settle records the first terminal result for a child. First-wins: a Close
// force-settle racing the child's own outcome is ignored.
func (p *SpawnProvider) settle(child *childRun, res Result) {
	child.mu.Lock()
	if !child.settled {
		child.result = res
		child.settled = true
	}
	child.mu.Unlock()
}

// deriveResult maps the child loop's outcome to the subagent Result (ADR 决策
// ②: completed | aborted | error | max-tokens | refusal). Output is the child's
// last non-empty assistant/message text, derived from the child's own log (D1).
func (p *SpawnProvider) deriveResult(child *childRun, runErr error) Result {
	last := lastAssistantEventAfter(child.log, child.seedSeq)
	child.mu.Lock()
	cancelled := child.cancelReason != ""
	structured := child.structured
	structuredSet := child.structuredSet
	child.mu.Unlock()
	result := Result{Output: last.text}
	switch {
	case cancelled:
		result.StopReason = StopAborted
	case runErr != nil:
		if failure, ok := llm.FailureFacts(runErr); ok && (failure.Code == "REFUSAL" || failure.Code == "CONTENT_FILTER") {
			// The parent loop keeps the structured failure for durable replay;
			// the subagent surface retains its historical refusal bucket.
			result.StopReason = StopRefusal
		} else {
			result.StopReason = StopError
		}
	default:
		finishReason := last.finishReason
		if finishReason == "" {
			finishReason = last.turnReason
		}
		result.StopReason = mapStopReason(finishReason)
	}
	if result.StopReason == StopCompleted && structuredSet {
		result.Structured = structured
	}
	return result
}

// resultFunc returns the Run.Result closure for a child: it blocks until the
// child settles (or ctx is cancelled) and returns the terminal outcome.
func (p *SpawnProvider) resultFunc(child *childRun) func(ctx context.Context) (Result, error) {
	return func(ctx context.Context) (Result, error) {
		select {
		case <-child.done:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		child.mu.Lock()
		defer child.mu.Unlock()
		return child.result, nil
	}
}

// sendFunc queues one follow-up turn for a live continuable child. The queue
// is bounded so a caller cannot create unbounded memory growth while a model
// request is in flight.
func (p *SpawnProvider) sendFunc(child *childRun) func(context.Context, string) error {
	return func(ctx context.Context, message string) error {
		return p.sendWithMetadataFunc(child)(ctx, message, nil)
	}
}

func (p *SpawnProvider) sendWithMetadataFunc(child *childRun) func(context.Context, string, map[string]string) error {
	return func(ctx context.Context, message string, metadata map[string]string) error {
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("subagent: message is empty")
		}
		return p.sendContentWithMetadataFunc(child)(ctx, []llm.ContentBlock{llm.Text(message)}, metadata)
	}
}

func (p *SpawnProvider) sendContentWithMetadataFunc(child *childRun) func(context.Context, []llm.ContentBlock, map[string]string) error {
	return func(ctx context.Context, content []llm.ContentBlock, metadata map[string]string) error {
		if len(content) == 0 {
			return fmt.Errorf("subagent: message is empty")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		child.mu.Lock()
		settled := child.settled
		continuable := child.continuable
		child.mu.Unlock()
		if settled {
			return fmt.Errorf("subagent: %s: already finished", child.id)
		}
		if !continuable {
			return fmt.Errorf("%w: %s", ErrNotContinuable, child.id)
		}
		select {
		case child.inbox <- pendingMessage{content: content, metadata: metadata}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			return fmt.Errorf("subagent: %s: message queue is full", child.id)
		}
	}
}

// sendQuietFunc accepts context without putting a wake-up item on the inbox.
// A later follow-up consumes the queued context together with its message.
func (p *SpawnProvider) sendQuietFunc(child *childRun) func(context.Context, string) error {
	return func(ctx context.Context, message string) error {
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("subagent: message is empty")
		}
		child.mu.Lock()
		settled := child.settled
		continuable := child.continuable
		if !settled && continuable {
			if len(child.quietInbox) >= 16 {
				child.mu.Unlock()
				return fmt.Errorf("subagent: %s: quiet message queue is full", child.id)
			}
			child.quietInbox = append(child.quietInbox, message)
		}
		child.mu.Unlock()
		if settled {
			return fmt.Errorf("subagent: %s: already finished", child.id)
		}
		if !continuable {
			return fmt.Errorf("%w: %s", ErrNotContinuable, child.id)
		}
		return nil
	}
}

// cancelFunc returns the Run.Cancel closure for a child: it records the reason
// and cancels the child's loop context (synchronous and idempotent; a second
// cancel on the same live child is a no-op). It fails once the child has
// settled.
func (p *SpawnProvider) cancelFunc(child *childRun) func(reason string) error {
	return func(reason string) error {
		child.mu.Lock()
		if child.settled {
			child.mu.Unlock()
			return fmt.Errorf("subagent: %s: already finished", child.id)
		}
		if child.cancelReason == "" {
			child.cancelReason = reason
		}
		cancel := child.cancel
		child.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
}

// ListChildren returns a projection of every child this provider spawned under
// parentSessionID, sorted by id.
func (p *SpawnProvider) ListChildren(ctx context.Context, parentSessionID string) ([]ChildSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ChildSummary
	for _, c := range p.children {
		if c.parent != parentSessionID {
			continue
		}
		c.mu.Lock()
		running := !c.settled
		c.mu.Unlock()
		out = append(out, ChildSummary{ID: c.id, Label: c.label, Running: running, Continuable: c.continuable})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ChildLog returns the independent session log of a spawned child (for M5b-2
// wiring that inspects/persists a child session, and for tests asserting the
// child session is complete and replayable). The second return reports whether
// the child exists.
func (p *SpawnProvider) ChildLog(id string) (*session.Log, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.children[id]
	if !ok {
		return nil, false
	}
	return c.log, true
}

// Close cancels every live child and waits for all children to settle, so no
// background goroutine leaks (lifecycle reversible, ADR 决策 ②). Start after
// Close is rejected; Close is idempotent.
func (p *SpawnProvider) Close() error {
	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	p.closed = true
	children := make([]*childRun, 0, len(p.children))
	for _, c := range p.children {
		children = append(children, c)
	}
	p.mu.Unlock()

	for _, c := range children {
		c.mu.Lock()
		if !c.settled && c.cancelReason == "" {
			c.cancelReason = "provider closed"
		}
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel() // no-op for an already-settled child's context
		}
	}
	for _, c := range children {
		<-c.done
	}
	close(p.closeDone)
	return nil
}

// assistantEvent is the derived projection of the most recent assistant/message
// row of a child session log.
type assistantEvent struct {
	text         string
	finishReason string
	turnReason   string
}

// lastAssistantEvent scans a child session log for the most recent
// assistant/message row, returning the last non-empty text and the finish
// reason (D1: the log is the source of truth — the result is derived, never
// stored separately).
func lastAssistantEvent(log *session.Log) assistantEvent {
	return lastAssistantEventAfter(log, 0)
}

func lastAssistantEventAfter(log *session.Log, afterSeq uint64) assistantEvent {
	var ev assistantEvent
	for _, e := range log.Events() {
		if e.Seq <= afterSeq {
			continue
		}
		if e.Type != session.EventAssistantMessage {
			continue
		}
		var d struct {
			Text         string `json:"text"`
			FinishReason string `json:"finishReason"`
			Message      struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		text := d.Text
		if text == "" {
			for _, block := range d.Message.Content {
				if block.Type == "text" {
					text += block.Text
				}
			}
		}
		if text != "" {
			ev.text = text
		}
		ev.finishReason = d.FinishReason
	}
	for _, e := range log.Events() {
		if e.Seq <= afterSeq || e.Type != session.EventTurnEnd {
			continue
		}
		var d struct {
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		}
		if json.Unmarshal(e.Data, &d) == nil {
			ev.turnReason = d.Reason.Kind
		}
	}
	return ev
}

// mapStopReason maps a model finish reason onto the subagent StopReason
// vocabulary (ADR 决策 ②). "length" is DeepSeek's max-token finish;
// content-filter finishes map to refusal. Anything else on a clean completion
// is completed.
func mapStopReason(finishReason string) string {
	switch finishReason {
	case "length", "max_tokens", "max-tokens":
		return StopMaxTokens
	case "content_filter", "refusal":
		return StopRefusal
	default:
		return StopCompleted
	}
}

// acceptanceSection is the eval self-check section appended to a child prompt
// when acceptance criteria are given (ADR D-EVAL-4).
const acceptanceSection = "\n\n## 验收标准（交付自检）\n你的交付必须满足以下验收标准，完成后逐条自检，并在最终回复中逐条说明每条的满足情况：\n"

// withAcceptance appends the acceptance criteria section to prompt when
// criteria are non-empty; otherwise it returns prompt unchanged.
func withAcceptance(prompt string, criteria []string) string {
	if len(criteria) == 0 {
		return prompt
	}
	var sb strings.Builder
	sb.WriteString(prompt)
	sb.WriteString(acceptanceSection)
	for _, c := range criteria {
		if strings.TrimSpace(c) == "" {
			continue
		}
		sb.WriteString("- ")
		sb.WriteString(strings.TrimSpace(c))
		sb.WriteByte('\n')
	}
	return sb.String()
}
