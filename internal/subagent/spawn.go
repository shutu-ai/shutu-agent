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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/loop"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

// Deps wires the SpawnProvider to the core components it reuses for every child
// (the composition root supplies them at construction, M5b-2 wiring; tests use
// a fake LLM). Log is the parent/host session log the provider is bound to —
// it is never appended to by the provider (the child owns an independent log);
// M5b-2's subagent/* event recording will surface the parent lineage through
// it. Each spawned child gets its own fresh session.New() log.
type Deps struct {
	Log    *session.Log
	LLM    llm.LLM
	Tools  *tools.Registry
	Prompt *prompt.Builder
	Model  string
	// Store durably records the independent child session when provided. It is
	// optional so library users and existing tests can remain in-memory.
	Store store.Store
}

// SpawnProvider spawns a brand-new child session + child loop for every Start.
// It is safe for concurrent use.
type SpawnProvider struct {
	deps Deps

	mu       sync.Mutex
	children map[string]*childRun
	nextID   int
	closed   bool
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
	cancel      context.CancelFunc // cancels the child loop context (set in Start)
	done        chan struct{}      // closed once the child settles
	inbox       chan string        // follow-up messages for continuable children
	continuable bool

	mu            sync.Mutex
	cancelReason  string
	result        Result
	structured    any
	structuredSet bool
	settled       bool
}

// NewSpawnProvider returns a SpawnProvider bound to the given core components.
func NewSpawnProvider(deps Deps) *SpawnProvider {
	return &SpawnProvider{deps: deps, children: map[string]*childRun{}}
}

// Name returns the provider name ("spawn"), the default subagent provider.
func (p *SpawnProvider) Name() string { return "spawn" }

// Capabilities declares what the spawn provider actually enforces: delegation
// depth (MaxDepth ⇒ ErrDepthExceeded). ToolFilter/Persona application and
// structured output is captured through a child-scoped structured_output tool.
func (p *SpawnProvider) Capabilities() Capabilities {
	return Capabilities{DepthLimit: true, OutputSchema: true}
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
	if parent, ok := p.children[req.ParentSessionID]; ok {
		parentDepth = parent.depth
	}
	depth := parentDepth + 1
	if req.MaxDepth > 0 && depth > req.MaxDepth {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: depth %d exceeds max depth %d (parent %q)",
			ErrDepthExceeded, depth, req.MaxDepth, req.ParentSessionID)
	}
	p.nextID++
	id := fmt.Sprintf("spawn-%d", p.nextID)
	runCtx, cancel := context.WithCancel(context.Background())
	child := &childRun{
		id:          id,
		label:       req.Label,
		parent:      req.ParentSessionID,
		depth:       depth,
		log:         session.New(),
		cancel:      cancel,
		done:        make(chan struct{}),
		inbox:       make(chan string, 16),
		continuable: req.Continuable,
	}
	p.children[id] = child
	p.mu.Unlock()
	if p.deps.Store != nil {
		if err := p.deps.Store.CreateSession(context.Background(), id, time.Now().UTC()); err != nil {
			cancel()
			p.mu.Lock()
			delete(p.children, id)
			p.mu.Unlock()
			return nil, fmt.Errorf("subagent: persist child session %q: %w", id, err)
		}
		child.log.SetSink(func(ev session.Event) error {
			return p.deps.Store.AppendEvents(context.Background(), id, []session.Event{ev})
		})
		if _, err := child.log.Append(session.EventSubagentStart,
			session.NewSubagentStartWithDepth(id, p.Name(), child.parent, child.label, child.depth)); err != nil {
			cancel()
			p.mu.Lock()
			delete(p.children, id)
			p.mu.Unlock()
			return nil, fmt.Errorf("subagent: persist child metadata %q: %w", id, err)
		}
	}

	go p.runChild(child, req, runCtx)
	return &Run{
		ID:     id,
		Result: p.resultFunc(child),
		Send:   p.sendFunc(child),
		Cancel: p.cancelFunc(child),
	}, nil
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
	for _, ev := range events {
		if ev.Type == session.EventSubagentStart && json.Unmarshal(ev.Data, &meta) == nil {
			break
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
		log: log, cancel: cancel, done: make(chan struct{}), inbox: make(chan string, 16), continuable: continuable,
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
	go p.runChild(child, StartRequest{Prompt: message, Continuable: continuable}, runCtx)
	return &Run{ID: sessionID, Result: p.resultFunc(child), Send: p.sendFunc(child), Cancel: p.cancelFunc(child)}, nil
}

func parseSpawnID(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "spawn-%d", &n); err != nil {
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
	if req.Model != "" {
		model = req.Model
	}
	childTools := p.deps.Tools
	if req.OutputSchema != nil {
		childTools = p.deps.Tools.Clone()
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
	lp := loop.New(loop.Config{
		LLM:    p.deps.LLM,
		Log:    child.log,
		Tools:  childTools,
		Prompt: p.deps.Prompt,
		Model:  model,
	})
	message := req.Prompt
	for {
		runErr := lp.Run(runCtx, message)
		if !child.continuable || runErr != nil {
			p.settle(child, p.deriveResult(child, runErr))
			return
		}
		select {
		case message = <-child.inbox:
		case <-runCtx.Done():
			p.settle(child, p.deriveResult(child, runCtx.Err()))
			return
		}
	}
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
	last := lastAssistantEvent(child.log)
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
		result.StopReason = StopError
	default:
		result.StopReason = mapStopReason(last.finishReason)
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
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("subagent: message is empty")
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
		case child.inbox <- message:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			return fmt.Errorf("subagent: %s: message queue is full", child.id)
		}
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
		out = append(out, ChildSummary{ID: c.id, Label: c.label, Running: running})
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
		p.mu.Unlock()
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
	return nil
}

// assistantEvent is the derived projection of the most recent assistant/message
// row of a child session log.
type assistantEvent struct {
	text         string
	finishReason string
}

// lastAssistantEvent scans a child session log for the most recent
// assistant/message row, returning the last non-empty text and the finish
// reason (D1: the log is the source of truth — the result is derived, never
// stored separately).
func lastAssistantEvent(log *session.Log) assistantEvent {
	var ev assistantEvent
	for _, e := range log.Events() {
		if e.Type != session.EventAssistantMessage {
			continue
		}
		var d struct {
			Text         string `json:"text"`
			FinishReason string `json:"finishReason"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		if d.Text != "" {
			ev.text = d.Text
		}
		ev.finishReason = d.FinishReason
	}
	return ev
}

// mapStopReason maps a model finish reason onto the subagent StopReason
// vocabulary (ADR 决策 ②). "length" is DeepSeek's max-token finish;
// content-filter finishes map to refusal. Anything else on a clean completion
// is completed.
func mapStopReason(finishReason string) string {
	switch finishReason {
	case "length", "max_tokens":
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
