package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// ID is an opaque Agent identity.
type ID string

// Status is the externally observable lifecycle state.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusRunning Status = "running"
	StatusClosed  Status = "closed"
)

var (
	ErrAgentClosed    = errors.New("agent is closed")
	ErrAgentStarted   = errors.New("agent is already started")
	ErrAgentNotFound  = errors.New("agent is not registered")
	ErrRegistryClosed = errors.New("agent registry is closed")
	ErrRunnerRequired = errors.New("agent runner is required")
	ErrAgentBusy      = errors.New("agent has active work")
)

// Event is a live lifecycle notification.  Durable session events remain the
// responsibility of the session/persistence layer.
type Event struct {
	AgentID  ID
	Previous Status
	Status   Status
	At       time.Time
}

// Runner processes one claimed turn.  The Agent is passed explicitly so a
// driver can claim additional next-step messages between model steps without
// reaching for process-global state.
type Runner func(context.Context, *Agent, TurnInput) error

// Options configures one Agent publication.
type Options struct {
	ID       ID
	ParentID ID
	Scope    *Scope
	Runner   Runner
	OnEvent  func(Event)
	// InboxJournal makes queue mutations durable before they are applied. It
	// is optional so the standalone runtime remains usable without storage.
	InboxJournal InboxJournal
	// InitialInbox is the replayed pending projection for a resumed Agent.
	InitialInbox []InboxEvent
	// InitialTurn is the next durable turn number used by inbox claim facts.
	InitialTurn int
}

// CancelOptions controls what happens to work that has not started yet.
// Cancellation discards queued work by default; KeepInbox preserves it for a
// later wake.
type CancelOptions struct {
	KeepInbox bool
}

// Agent is one independently owned runtime.  It is safe for concurrent use.
type Agent struct {
	id       ID
	parentID ID
	scope    *Scope
	inbox    *Inbox
	runner   Runner
	onEvent  func(Event)

	mu                sync.Mutex
	status            Status
	started           bool
	closed            bool
	disposed          bool
	runCancel         context.CancelFunc
	turnCancel        context.CancelFunc
	maintenance       bool
	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
	nextTurn          int
	activeTurn        int
	turnClaimed       bool
	steerPending      bool
	done              chan struct{}
	lastErr           error
	waiters           map[string]chan error
	claimMu           sync.Mutex // excludes maintenance from the turn claim boundary
	closeDone         chan struct{}
	closeDoneOnce     sync.Once
}

// newAgent creates an unpublished Agent.  Registry owns publication.
func newAgent(opts Options) (*Agent, error) {
	if opts.Runner == nil {
		return nil, ErrRunnerRequired
	}
	if opts.ID == "" {
		return nil, errors.New("agent id is required")
	}
	scope := opts.Scope
	if scope == nil {
		scope = NewScope(nil)
	}
	inbox, err := NewDurableInbox(opts.InboxJournal, opts.InitialInbox)
	if err != nil {
		return nil, err
	}
	nextTurn := opts.InitialTurn
	if nextTurn <= 0 {
		nextTurn = 1
	}
	return &Agent{
		id:        opts.ID,
		parentID:  opts.ParentID,
		scope:     scope,
		inbox:     inbox,
		runner:    opts.Runner,
		onEvent:   opts.OnEvent,
		status:    StatusIdle,
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
		waiters:   make(map[string]chan error),
		nextTurn:  nextTurn,
	}, nil
}

// ID returns the opaque Agent id.
func (a *Agent) ID() ID { return a.id }

// ParentID returns the parent lineage id, if any.
func (a *Agent) ParentID() ID { return a.parentID }

// Scope returns the Agent-owned capability scope.
func (a *Agent) Scope() *Scope { return a.scope }

// Inbox returns the Agent-owned inbox for driver integrations.
func (a *Agent) Inbox() *Inbox { return a.inbox }

// ClaimStep lets an active driver consume steering/injected work at a step
// boundary without stealing the next ordinary follow-up turn.
func (a *Agent) ClaimStep() (TurnInput, bool) {
	if a == nil {
		return TurnInput{}, false
	}
	input, ok, _ := a.ClaimStepWithError()
	return input, ok
}

// ClaimStepWithError exposes a durable claim failure to a loop bridge.
func (a *Agent) ClaimStepWithError() (TurnInput, bool, error) {
	if a == nil {
		return TurnInput{}, false, nil
	}
	a.mu.Lock()
	turn := a.activeTurn
	a.mu.Unlock()
	return a.inbox.ClaimStepWithError(turn)
}

// ClaimSteerStepWithError consumes only a steering request that caused the
// active runner cancellation. Ordinary next-step input remains owned by the
// normal turn-stopping hook; this distinction lets a loop resume the same
// durable turn after an interrupted provider step.
func (a *Agent) ClaimSteerStepWithError() (TurnInput, bool, error) {
	if a == nil {
		return TurnInput{}, false, nil
	}
	a.mu.Lock()
	if !a.steerPending {
		a.mu.Unlock()
		return TurnInput{}, false, nil
	}
	a.steerPending = false
	turn := a.activeTurn
	a.mu.Unlock()
	return a.inbox.ClaimStepWithError(turn)
}

// Status returns the current lifecycle status.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// LastError returns the last turn error.  The returned error is immutable from
// the Agent's perspective and is intended for diagnostics, not control flow.
func (a *Agent) LastError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// Start publishes the Agent's background driver loop.  Start is deliberately
// separate from Create so a caller can finish scoped setup before execution.
func (a *Agent) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrAgentClosed
	}
	if a.started {
		a.mu.Unlock()
		return ErrAgentStarted
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.runCancel = cancel
	a.started = true
	a.mu.Unlock()
	go a.run(runCtx)
	return nil
}

func (a *Agent) run(ctx context.Context) {
	defer func() {
		a.mu.Lock()
		if !a.closed {
			a.closed = true
			a.setStatusLocked(StatusClosed)
		}
		waiters := make([]chan error, 0, len(a.waiters))
		for id, waiter := range a.waiters {
			delete(a.waiters, id)
			waiters = append(waiters, waiter)
		}
		maintenanceCancel := a.maintenanceCancel
		maintenanceDone := a.maintenanceDone
		a.mu.Unlock()
		if maintenanceCancel != nil {
			maintenanceCancel()
		}
		a.inbox.Close()
		for _, waiter := range waiters {
			waiter <- ErrAgentClosed
			close(waiter)
		}
		if maintenanceDone != nil {
			<-maintenanceDone
		}
		_ = a.scope.Close()
		a.mu.Lock()
		a.disposed = true
		a.mu.Unlock()
		close(a.done)
		a.closeDoneOnce.Do(func() { close(a.closeDone) })
	}()
	for {
		// Maintenance owns the idle phase without changing the public status to
		// running. Wakeups arriving during it remain in the inbox and are
		// claimed only after the maintenance barrier closes.
		a.mu.Lock()
		maintenance := a.maintenance
		maintenanceDone := a.maintenanceDone
		a.mu.Unlock()
		if maintenance {
			select {
			case <-maintenanceDone:
				continue
			case <-ctx.Done():
				return
			}
		}
		a.claimMu.Lock()
		a.mu.Lock()
		// RunMaintenance takes claimMu before publishing its phase, so a
		// message cannot be claimed in the small interval between the gate
		// check above and this claim.
		if a.maintenance {
			maintenanceDone = a.maintenanceDone
			a.mu.Unlock()
			a.claimMu.Unlock()
			<-maintenanceDone
			continue
		}
		turn := a.nextTurn
		a.nextTurn++
		input, ok, claimErr := a.inbox.ClaimWithError(turn)
		if !ok {
			a.nextTurn--
			a.mu.Unlock()
			a.claimMu.Unlock()
			if claimErr != nil {
				a.setError(claimErr)
				return
			}
			if err := a.inbox.Wait(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, ErrInboxClosed) {
					return
				}
				a.setError(err)
				return
			}
			continue
		}
		if claimErr != nil {
			a.mu.Unlock()
			a.claimMu.Unlock()
			a.setError(claimErr)
			return
		}
		a.turnClaimed = true
		a.mu.Unlock()
		a.claimMu.Unlock()

		turnCtx, cancel := context.WithCancel(ctx)
		a.mu.Lock()
		if a.closed {
			cancel()
			a.turnClaimed = false
			a.mu.Unlock()
			return
		}
		a.turnCancel = cancel
		a.activeTurn = turn
		a.setStatusLocked(StatusRunning)
		a.mu.Unlock()

		err := a.runner(turnCtx, a, input)
		cancel()
		a.mu.Lock()
		a.turnCancel = nil
		a.activeTurn = 0
		a.turnClaimed = false
		a.steerPending = false
		a.completeWaitersLocked(input, err)
		if err != nil && !errors.Is(err, context.Canceled) {
			a.lastErr = err
		}
		if !a.closed {
			a.setStatusLocked(StatusIdle)
		}
		closed := a.closed
		a.mu.Unlock()
		// A parent cancellation is a terminal Agent boundary. Do not claim a
		// second queued turn with an already-canceled context; queued Run waiters
		// must be settled by the close barrier as ErrAgentClosed.
		if closed || ctx.Err() != nil {
			return
		}
	}
}

func (a *Agent) completeWaitersLocked(input TurnInput, err error) {
	for _, message := range input.Messages {
		waiter, waiting := a.waiters[message.ID]
		if waiting {
			delete(a.waiters, message.ID)
			waiter <- err
			close(waiter)
		}
	}
}

func (a *Agent) setError(err error) {
	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
}

func (a *Agent) setStatusLocked(next Status) {
	if a.status == next {
		return
	}
	previous := a.status
	a.status = next
	if a.onEvent != nil {
		event := Event{AgentID: a.id, Previous: previous, Status: next, At: time.Now().UTC()}
		// Lifecycle observers are not allowed to break the Agent driver.
		go func() {
			defer func() { _ = recover() }()
			a.onEvent(event)
		}()
	}
}

// Send queues model-visible next-step input and wakes the Agent.
func (a *Agent) Send(text string, metadata map[string]string) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAgentClosed
	}
	_, err := a.inbox.Send(text, metadata)
	return err
}

// SendContent queues rich model-visible next-step input and wakes the Agent.
func (a *Agent) SendContent(content []llm.ContentBlock, metadata map[string]string) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAgentClosed
	}
	_, err := a.inbox.SendContent(content, metadata)
	return err
}

// Followup queues one next-turn input and wakes the Agent.
func (a *Agent) Followup(text string, metadata map[string]string) error {
	_, err := a.FollowupWithID(text, metadata)
	return err
}

// FollowupWithID queues one next-turn input and returns its durable message
// identity for transport-level enqueue receipts.
func (a *Agent) FollowupWithID(text string, metadata map[string]string) (Message, error) {
	if a == nil {
		return Message{}, ErrAgentClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return Message{}, ErrAgentClosed
	}
	return a.inbox.Followup(text, metadata)
}

// FollowupContent queues rich next-turn input and wakes the Agent if idle.
func (a *Agent) FollowupContent(content []llm.ContentBlock, metadata map[string]string) error {
	_, err := a.FollowupContentWithID(content, metadata)
	return err
}

// FollowupContentWithID is the rich-content form of FollowupWithID.
func (a *Agent) FollowupContentWithID(content []llm.ContentBlock, metadata map[string]string) (Message, error) {
	if a == nil {
		return Message{}, ErrAgentClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return Message{}, ErrAgentClosed
	}
	return a.inbox.FollowupContent(content, metadata)
}

// Inject queues quiet context without waking an idle Agent.
func (a *Agent) Inject(text string, metadata map[string]string) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAgentClosed
	}
	_, err := a.inbox.Inject(text, metadata)
	return err
}

// InjectContent queues rich quiet context without waking an idle Agent.
func (a *Agent) InjectContent(content []llm.ContentBlock, metadata map[string]string) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAgentClosed
	}
	_, err := a.inbox.InjectContent(content, metadata)
	return err
}

// Steer cancels the current runner and queues a next-step message.
func (a *Agent) Steer(text string, metadata map[string]string) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrAgentClosed
	}
	if _, err := a.inbox.Steer(text, metadata); err != nil {
		a.mu.Unlock()
		return err
	}
	turnCancel := a.turnCancel
	if turnCancel != nil {
		a.steerPending = true
	}
	a.mu.Unlock()
	if turnCancel != nil {
		turnCancel()
	}
	return nil
}

// SteerContent cancels the current runner and queues a rich next-step
// message, preserving content blocks such as image references.
func (a *Agent) SteerContent(content []llm.ContentBlock, metadata map[string]string) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrAgentClosed
	}
	if _, err := a.inbox.SteerContent(content, metadata); err != nil {
		a.mu.Unlock()
		return err
	}
	turnCancel := a.turnCancel
	if turnCancel != nil {
		a.steerPending = true
	}
	a.mu.Unlock()
	if turnCancel != nil {
		turnCancel()
	}
	return nil
}

// RunMaintenance runs one Agent-owned non-turn operation while the Agent is
// idle. Messages arriving during the operation remain queued and are claimed
// only after the maintenance task settles. Cancel and Close abort the task
// through its context. The task must not call Close on the same Agent.
func (a *Agent) RunMaintenance(task func(context.Context) error) error {
	if a == nil {
		return ErrAgentClosed
	}
	if task == nil {
		return errors.New("agent maintenance task is required")
	}
	a.claimMu.Lock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.claimMu.Unlock()
		return ErrAgentClosed
	}
	if a.status == StatusRunning || a.maintenance {
		a.mu.Unlock()
		a.claimMu.Unlock()
		return ErrAgentBusy
	}
	if a.inbox.HasWork() {
		a.mu.Unlock()
		a.claimMu.Unlock()
		return ErrAgentBusy
	}
	maintenanceCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.maintenance = true
	a.maintenanceCancel = cancel
	a.maintenanceDone = done
	a.mu.Unlock()
	a.claimMu.Unlock()

	result := make(chan error, 1)
	go func() {
		var err error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("maintenance panic: %v", recovered)
				}
			}()
			err = task(maintenanceCtx)
		}()
		// Publish completion before clearing the pointer. A concurrent Agent
		// shutdown may observe maintenanceDone == nil; in that case the close
		// must already have happened so scope disposal cannot overtake the task.
		close(done)
		a.mu.Lock()
		a.maintenance = false
		a.maintenanceCancel = nil
		a.maintenanceDone = nil
		a.mu.Unlock()
		result <- err
	}()
	return <-result
}

// Cancel aborts the active turn and clears queued work by default. The Agent
// remains published and can accept new work after it reaches idle.
func (a *Agent) Cancel() error {
	return a.CancelWithOptions(CancelOptions{})
}

// CancelWithOptions aborts the active turn. KeepInbox preserves queued
// next-step, next-turn and quiet-injection messages for a later wake.
func (a *Agent) CancelWithOptions(options CancelOptions) error {
	if a == nil {
		return ErrAgentClosed
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrAgentClosed
	}
	cancel := a.turnCancel
	maintenanceCancel := a.maintenanceCancel
	a.mu.Unlock()
	if !options.KeepInbox {
		a.mu.Lock()
		a.steerPending = false
		a.mu.Unlock()
		if err := a.inbox.ClearWithError(); err != nil {
			if cancel != nil {
				cancel()
			}
			return err
		}
	}
	if cancel != nil {
		cancel()
	}
	if maintenanceCancel != nil {
		maintenanceCancel()
	}
	return nil
}

// Run submits one waking next-turn message and waits for the turn containing
// that message to settle. Long-lived integrations should use Start plus
// Send/Followup directly.
func (a *Agent) Run(ctx context.Context, text string, metadata map[string]string) error {
	return a.runSubmitted(ctx, func() (Message, error) {
		return a.inbox.Followup(text, metadata)
	})
}

// RunContent submits one rich next-turn message and waits for its turn to
// settle. It is the typed seam used by ACP image prompts; ordinary callers
// should continue to use Run.
func (a *Agent) RunContent(ctx context.Context, content []llm.ContentBlock, metadata map[string]string) error {
	return a.runSubmitted(ctx, func() (Message, error) {
		return a.inbox.FollowupContent(content, metadata)
	})
}

func (a *Agent) runSubmitted(ctx context.Context, enqueue func() (Message, error)) error {
	if a == nil {
		return ErrAgentClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	started, closed := a.started, a.closed
	a.mu.Unlock()
	if closed {
		return ErrAgentClosed
	}
	if !started {
		if err := a.Start(context.Background()); err != nil {
			return err
		}
	}
	// Hold the Agent lock while enqueueing and registering the waiter. The
	// driver must acquire the same lock before invoking the runner, so a fast
	// runner cannot settle the message before its waiter is visible.
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrAgentClosed
	}
	message, err := enqueue()
	if err != nil {
		a.mu.Unlock()
		return err
	}
	waiter := make(chan error, 1)
	a.waiters[message.ID] = waiter
	a.mu.Unlock()
	select {
	case err := <-waiter:
		return err
	case <-ctx.Done():
		// A caller cancelling a queued Run owns only that submitted message.
		// Do not cancel an unrelated active turn which happens to be ahead of
		// it; cancel the Agent turn only after this message has been claimed.
		if removed, removeErr := a.inbox.RemoveWithError(message.ID); removed {
			a.mu.Lock()
			delete(a.waiters, message.ID)
			a.mu.Unlock()
			if removeErr != nil {
				return errors.Join(ctx.Err(), removeErr)
			}
			return ctx.Err()
		} else if removeErr != nil {
			return errors.Join(ctx.Err(), removeErr)
		}
		_ = a.Cancel()
		return ctx.Err()
	}
}

// WhenIdle waits until the Agent has no active turn.  It returns an error if
// the context is canceled before the Agent becomes idle.
func (a *Agent) WhenIdle(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		a.mu.Lock()
		status := a.status
		maintenance := a.maintenance
		claimed := a.turnClaimed
		a.mu.Unlock()
		if !maintenance && !claimed && (status == StatusIdle || status == StatusClosed) && !a.inbox.HasWork() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// Close stops the Agent, waits for its driver, closes its inbox and disposes
// its owned scope.  It is idempotent.
func (a *Agent) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if a.disposed {
		a.mu.Unlock()
		return nil
	}
	if a.closed {
		closeDone := a.closeDone
		a.mu.Unlock()
		<-closeDone
		return nil
	}
	a.closed = true
	a.setStatusLocked(StatusClosed)
	if a.turnCancel != nil {
		a.turnCancel()
	}
	if a.runCancel != nil {
		a.runCancel()
	}
	maintenanceCancel := a.maintenanceCancel
	maintenanceDone := a.maintenanceDone
	started := a.started
	waiters := make([]chan error, 0, len(a.waiters))
	for id, waiter := range a.waiters {
		delete(a.waiters, id)
		waiters = append(waiters, waiter)
	}
	a.mu.Unlock()
	a.inbox.Close()
	if maintenanceCancel != nil {
		maintenanceCancel()
	}
	for _, waiter := range waiters {
		waiter <- ErrAgentClosed
		close(waiter)
	}
	if started {
		<-a.closeDone
		return nil
	}
	if maintenanceDone != nil {
		<-maintenanceDone
	}
	a.mu.Lock()
	a.disposed = true
	a.mu.Unlock()
	err := a.scope.Close()
	a.closeDoneOnce.Do(func() { close(a.closeDone) })
	return err
}

// Handle is the stable owned reference exposed by Registry.
type Handle struct{ agent *Agent }

func (h *Handle) Agent() *Agent                   { return h.agent }
func (h *Handle) ID() ID                          { return h.agent.ID() }
func (h *Handle) ParentID() ID                    { return h.agent.ParentID() }
func (h *Handle) Scope() *Scope                   { return h.agent.Scope() }
func (h *Handle) ClaimStep() (TurnInput, bool)    { return h.agent.ClaimStep() }
func (h *Handle) Status() Status                  { return h.agent.Status() }
func (h *Handle) Start(ctx context.Context) error { return h.agent.Start(ctx) }
func (h *Handle) Run(ctx context.Context, text string, metadata map[string]string) error {
	return h.agent.Run(ctx, text, metadata)
}
func (h *Handle) RunContent(ctx context.Context, content []llm.ContentBlock, metadata map[string]string) error {
	return h.agent.RunContent(ctx, content, metadata)
}
func (h *Handle) Send(text string, metadata map[string]string) error {
	return h.agent.Send(text, metadata)
}
func (h *Handle) SendContent(content []llm.ContentBlock, metadata map[string]string) error {
	return h.agent.SendContent(content, metadata)
}
func (h *Handle) Followup(text string, metadata map[string]string) error {
	return h.agent.Followup(text, metadata)
}
func (h *Handle) FollowupWithID(text string, metadata map[string]string) (Message, error) {
	return h.agent.FollowupWithID(text, metadata)
}
func (h *Handle) FollowupContent(content []llm.ContentBlock, metadata map[string]string) error {
	return h.agent.FollowupContent(content, metadata)
}
func (h *Handle) FollowupContentWithID(content []llm.ContentBlock, metadata map[string]string) (Message, error) {
	return h.agent.FollowupContentWithID(content, metadata)
}
func (h *Handle) Inject(text string, metadata map[string]string) error {
	return h.agent.Inject(text, metadata)
}
func (h *Handle) InjectContent(content []llm.ContentBlock, metadata map[string]string) error {
	return h.agent.InjectContent(content, metadata)
}
func (h *Handle) Steer(text string, metadata map[string]string) error {
	return h.agent.Steer(text, metadata)
}
func (h *Handle) SteerContent(content []llm.ContentBlock, metadata map[string]string) error {
	return h.agent.SteerContent(content, metadata)
}
func (h *Handle) RunMaintenance(task func(context.Context) error) error {
	return h.agent.RunMaintenance(task)
}
func (h *Handle) Cancel() error { return h.agent.Cancel() }
func (h *Handle) CancelWithOptions(options CancelOptions) error {
	return h.agent.CancelWithOptions(options)
}
func (h *Handle) WhenIdle(ctx context.Context) error { return h.agent.WhenIdle(ctx) }
func (h *Handle) Close() error                       { return h.agent.Close() }

// Registry owns publication and lookup of Agents.
type Registry struct {
	mu     sync.RWMutex
	nextID uint64
	agents map[ID]*Agent
	root   *Scope
	closed bool
}

// NewRegistry creates an empty Agent registry with a root capability scope.
func NewRegistry() *Registry {
	return &Registry{agents: make(map[ID]*Agent), root: NewScope(nil)}
}

// RootScope returns the registry-level scope used by root Agents.
func (r *Registry) RootScope() *Scope { return r.root }

// Create constructs and publishes one Agent.  It is not started until Start
// succeeds, allowing callers to complete setup and rollback atomically.
func (r *Registry) Create(opts Options) (*Handle, error) {
	if r == nil {
		return nil, ErrAgentClosed
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrRegistryClosed
	}
	if opts.ID == "" {
		r.nextID++
		opts.ID = ID(fmt.Sprintf("agent-%d", r.nextID))
	}
	if _, exists := r.agents[opts.ID]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("agent %q already exists", opts.ID)
	}
	var parentAgent *Agent
	if opts.Scope == nil {
		parent := r.root
		if opts.ParentID != "" {
			parentAgent = r.agents[opts.ParentID]
			if parentAgent == nil {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: parent %q", ErrAgentNotFound, opts.ParentID)
			}
			parent = parentAgent.Scope()
		}
		opts.Scope = NewScope(parent)
	}
	agent, err := newAgent(opts)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.agents[agent.ID()] = agent
	if parentAgent != nil {
		childID := agent.ID()
		// Parent scope disposal is the Agent hierarchy disposal boundary. The
		// registry removal happens inside the cleanup so a child cannot remain
		// published after its parent has closed. Explicit child closure is
		// idempotent: its later parent cleanup observes ErrAgentNotFound and
		// treats that as already disposed.
		if err := parentAgent.Scope().AddCleanup(func() error {
			err := r.Close(childID)
			if errors.Is(err, ErrAgentNotFound) {
				return nil
			}
			return err
		}); err != nil {
			delete(r.agents, agent.ID())
			r.mu.Unlock()
			_ = agent.Close()
			return nil, err
		}
	}
	r.mu.Unlock()
	return &Handle{agent: agent}, nil
}

// Lookup returns a published Agent handle.
func (r *Registry) Lookup(id ID) (*Handle, error) {
	if r == nil {
		return nil, ErrAgentNotFound
	}
	r.mu.RLock()
	agent := r.agents[id]
	r.mu.RUnlock()
	if agent == nil {
		return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	return &Handle{agent: agent}, nil
}

// List returns a stable snapshot of currently published handles.
func (r *Registry) List() []*Handle {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	ids := make([]ID, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*Handle, 0, len(ids))
	for _, id := range ids {
		out = append(out, &Handle{agent: r.agents[id]})
	}
	r.mu.RUnlock()
	return out
}

// Close removes and closes one Agent.  Removal happens before cleanup so a
// reentrant lookup cannot observe an Agent after ownership has ended.
func (r *Registry) Close(id ID) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	agent := r.agents[id]
	if agent == nil {
		r.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrAgentNotFound, id)
	}
	delete(r.agents, id)
	r.mu.Unlock()
	return agent.Close()
}

// CloseAll closes every published Agent and the registry root scope.
func (r *Registry) CloseAll() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	ids := make([]ID, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	agents := make([]*Agent, 0, len(ids))
	for _, id := range ids {
		agent := r.agents[id]
		delete(r.agents, id)
		agents = append(agents, agent)
	}
	r.mu.Unlock()
	var first error
	for _, agent := range agents {
		if err := agent.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := r.root.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
