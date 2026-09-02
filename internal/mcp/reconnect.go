package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// generationCloseTimeout is the maximum time the supervisor waits for a
// transport generation to acknowledge Close. A broken third-party Client must
// not wedge the reconnect goroutine forever or allow a later generation to be
// started while the old one is still potentially alive.
const generationCloseTimeout = 5 * time.Second

var errGenerationCloseTimeout = errors.New("mcp: generation close timed out")
var errReconnectQuiesceTimeout = errors.New("mcp: reconnect client did not quiesce")

// ReconnectOptions bounds the background connection supervisor. Attempts are
// counted after the initial delay; MaxAttempts == 0 selects the default and a
// negative value means retry indefinitely. The defaults deliberately remain
// finite enough to avoid an unbounded retry storm while still covering
// short-lived MCP server restarts.
type ReconnectOptions struct {
	Enabled      bool
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxAttempts  int
}

var DefaultReconnectOptions = ReconnectOptions{
	Enabled:      true,
	InitialDelay: 500 * time.Millisecond,
	MaxDelay:     30 * time.Second,
	MaxAttempts:  10,
}

// ReconnectingClient supervises a Client after transport loss. The wrapped
// client remains the source of truth for request serialization and protocol
// state; this type only owns bounded lifecycle recovery and never replays a
// failed tool call. Callers therefore retain the original error and decide
// whether a model-level retry is appropriate.
type ReconnectingClient struct {
	base Client
	// factory creates a fresh protocol client for each reconnect generation.
	// It is optional so existing embedders with a restartable Client retain the
	// old constructor contract.
	factory func(context.Context) (Client, error)
	opts    ReconnectOptions

	mu              sync.Mutex
	closed          bool
	reconnecting    bool
	reconnectCh     chan struct{}
	done            chan struct{}
	wg              sync.WaitGroup
	reconnected     func()
	exhausted       func()
	toolListChanged func()
	active          int
	idle            chan struct{}
	generation      uint64
	activeByGen     map[uint64]int
	generationIdle  map[uint64]chan struct{}
	startMu         sync.Mutex
	closeDone       chan struct{}
	superviseCtx    context.Context
	superviseCancel context.CancelFunc
	// failedAttempts is shared across reconnect generations. A server that
	// reconnects successfully and immediately dies again must not reset the
	// outage budget forever; only a stable connection window does that.
	failedAttempts int
	connectedAt    time.Time
}

func NewReconnectingClient(base Client, opts ReconnectOptions) Client {
	return newReconnectingClient(base, nil, opts)
}

// NewReconnectingClientWithFactory supervises a long-lived MCP bridge with a
// fresh Client/transport on every reconnect. The initial base is used for the
// first Start; factory is called only after a connection loss. A successful
// candidate is started before the old generation is retired, and Close waits
// for both generations and all callbacks to quiesce.
func NewReconnectingClientWithFactory(base Client, factory func(context.Context) (Client, error), opts ReconnectOptions) Client {
	return newReconnectingClient(base, factory, opts)
}

func newReconnectingClient(base Client, factory func(context.Context) (Client, error), opts ReconnectOptions) Client {
	if opts.InitialDelay <= 0 {
		opts.InitialDelay = DefaultReconnectOptions.InitialDelay
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = DefaultReconnectOptions.MaxDelay
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = DefaultReconnectOptions.MaxAttempts
	}
	superviseCtx, superviseCancel := context.WithCancel(context.Background())
	r := &ReconnectingClient{
		base: base, factory: factory, opts: opts, idle: make(chan struct{}),
		reconnectCh: make(chan struct{}, 1), done: make(chan struct{}), closeDone: make(chan struct{}),
		superviseCtx: superviseCtx, superviseCancel: superviseCancel,
		activeByGen: make(map[uint64]int), generationIdle: make(map[uint64]chan struct{}),
	}
	close(r.idle)
	initialGenerationIdle := make(chan struct{})
	close(initialGenerationIdle)
	r.generationIdle[0] = initialGenerationIdle
	r.attachBaseHandlers(base)
	r.wg.Add(1)
	go r.supervise()
	return r
}

func (r *ReconnectingClient) attachBaseHandlers(base Client) {
	if base == nil {
		return
	}
	if lost, ok := base.(ConnectionLostHandler); ok {
		lost.SetConnectionLostHandler(func(error) { r.requestReconnect() })
	}
	if notifier, ok := base.(ToolListChangedHandler); ok {
		notifier.SetToolListChangedHandler(func() { r.dispatchToolListChanged(base) })
	}
}

func (r *ReconnectingClient) Start(ctx context.Context) error {
	release, _, err := r.beginOperation()
	if err != nil {
		return err
	}
	defer release()
	base := r.currentBase()
	if base == nil {
		return ErrStartFailed
	}
	r.startMu.Lock()
	err = base.Start(ctx)
	r.startMu.Unlock()
	if err == nil {
		r.markConnected()
		r.clearReconnect()
	} else if shouldReconnect(err) {
		// The reference supervisor applies one outage budget to the initial
		// connection as well. Report the first failure to the caller while the
		// background supervisor keeps the configured retry contract.
		r.requestReconnect()
	}
	return err
}

func (r *ReconnectingClient) ListTools(ctx context.Context) ([]Tool, error) {
	release, _, err := r.beginOperation()
	if err != nil {
		return nil, err
	}
	defer release()
	base := r.currentBase()
	if base == nil {
		return nil, ErrNotStarted
	}
	tools, err := base.ListTools(ctx)
	if shouldReconnect(err) {
		r.requestReconnect()
	}
	return tools, err
}

func (r *ReconnectingClient) Call(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	release, _, err := r.beginOperation()
	if err != nil {
		return CallResult{}, err
	}
	defer release()
	base := r.currentBase()
	if base == nil {
		return CallResult{}, ErrNotStarted
	}
	result, err := base.Call(ctx, name, args)
	if shouldReconnect(err) {
		r.requestReconnect()
	}
	return result, err
}

func (r *ReconnectingClient) Close() error {
	r.mu.Lock()
	if r.closed {
		closeDone := r.closeDone
		r.mu.Unlock()
		<-closeDone
		return nil
	}
	r.closed = true
	close(r.done)
	cancel := r.superviseCancel
	idle := r.idle
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	baseErr := error(nil)
	if !waitChannelWithin(idle, generationCloseTimeout) {
		baseErr = fmt.Errorf("%w before closing generation after %s", errReconnectQuiesceTimeout, generationCloseTimeout)
	}
	base := r.currentBase()
	if base != nil {
		baseErr = errors.Join(baseErr, closeClientWithin(base, generationCloseTimeout))
	}
	if !waitWaitGroupWithin(&r.wg, generationCloseTimeout) {
		baseErr = errors.Join(baseErr, fmt.Errorf("%w while stopping supervisor after %s", errReconnectQuiesceTimeout, generationCloseTimeout))
	}
	close(r.closeDone)
	return baseErr
}

func (r *ReconnectingClient) SetToolListChangedHandler(handler func()) {
	r.mu.Lock()
	r.toolListChanged = handler
	base := r.base
	r.mu.Unlock()
	if notifier, ok := base.(ToolListChangedHandler); ok {
		notifier.SetToolListChangedHandler(func() { r.dispatchToolListChanged(base) })
	}
}

// dispatchToolListChanged makes notifications generation-aware, like the
// reference bridge. A callback from a retired generation is ignored, and a
// callback during reconnect/backoff cannot trigger a stale resync from the old
// base. The replacement receives a fresh handler and invokes the consumer only
// while it is the current generation.
func (r *ReconnectingClient) dispatchToolListChanged(base Client) {
	r.mu.Lock()
	handler, current, reconnecting, closed := r.toolListChanged, r.base, r.reconnecting, r.closed
	r.mu.Unlock()
	if handler == nil || closed || reconnecting || current != base {
		return
	}
	handler()
}

func (r *ReconnectingClient) currentBase() Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.base
}

func (r *ReconnectingClient) SetReconnectedHandler(handler func()) {
	r.mu.Lock()
	r.reconnected = handler
	r.mu.Unlock()
}

func (r *ReconnectingClient) SetReconnectExhaustedHandler(handler func()) {
	r.mu.Lock()
	r.exhausted = handler
	r.mu.Unlock()
}

func (r *ReconnectingClient) notifyExhausted() {
	r.mu.Lock()
	handler := r.exhausted
	r.exhausted = nil
	r.mu.Unlock()
	if handler != nil {
		handler()
	}
}

// RecoveryPending reports whether a failed Start/ListTools operation scheduled
// a bounded background retry. Composition roots use this to retain shutdown
// ownership of a supervised client while accepting the first startup error.
func (r *ReconnectingClient) RecoveryPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconnecting
}

func (r *ReconnectingClient) beginOperation() (func(), uint64, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, 0, ErrClosed
	}
	if r.active == 0 {
		r.idle = make(chan struct{})
	}
	generation := r.generation
	if r.activeByGen[generation] == 0 {
		idle := make(chan struct{})
		r.generationIdle[generation] = idle
	}
	r.active++
	r.activeByGen[generation]++
	r.mu.Unlock()
	return func() { r.endOperation(generation) }, generation, nil
}

func (r *ReconnectingClient) endOperation(generation uint64) {
	r.mu.Lock()
	r.active--
	if r.activeByGen[generation] > 0 {
		r.activeByGen[generation]--
		if r.activeByGen[generation] == 0 {
			close(r.generationIdle[generation])
		}
	}
	if r.active == 0 {
		close(r.idle)
	}
	r.mu.Unlock()
}

func (r *ReconnectingClient) clearReconnect() {
	r.mu.Lock()
	r.reconnecting = false
	r.mu.Unlock()
}

func (r *ReconnectingClient) requestReconnect() {
	r.mu.Lock()
	if r.closed || !r.opts.Enabled || r.reconnecting {
		r.mu.Unlock()
		return
	}
	if !r.connectedAt.IsZero() && time.Since(r.connectedAt) >= r.opts.MaxDelay {
		r.failedAttempts = 0
	}
	r.connectedAt = time.Time{}
	r.reconnecting = true
	r.mu.Unlock()
	select {
	case r.reconnectCh <- struct{}{}:
	default:
	}
}

func (r *ReconnectingClient) supervise() {
	defer r.wg.Done()
	for {
		select {
		case <-r.reconnectCh:
			r.reconnect()
		case <-r.done:
			return
		}
	}
}

func (r *ReconnectingClient) reconnect() {
	if !r.reconnectPending() {
		return
	}
	for {
		r.mu.Lock()
		r.failedAttempts++
		attempt := r.failedAttempts
		maxAttempts := r.opts.MaxAttempts
		r.mu.Unlock()
		if maxAttempts > 0 && attempt > maxAttempts {
			r.clearReconnect()
			r.notifyExhausted()
			return
		}
		delay := r.reconnectDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-r.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			r.clearReconnect()
			return
		}
		if !r.reconnectPending() {
			return
		}
		if r.currentBase() == nil && r.factory == nil {
			r.clearReconnect()
			return
		}
		r.mu.Lock()
		superviseCtx := r.superviseCtx
		r.mu.Unlock()
		if superviseCtx == nil {
			superviseCtx = context.Background()
		}
		if err := r.startBase(superviseCtx); err == nil {
			r.mu.Lock()
			handler := r.reconnected
			r.reconnecting = false
			r.mu.Unlock()
			if handler != nil {
				handler()
			}
			return
		} else if errors.Is(err, errGenerationCloseTimeout) {
			// The old generation may still own a process or transport. Stop
			// here rather than creating an overlapping child; an explicit
			// reload/restart is required to establish a clean lifecycle.
			r.clearReconnect()
			r.notifyExhausted()
			return
		}
	}
}

func (r *ReconnectingClient) markConnected() {
	r.mu.Lock()
	r.connectedAt = time.Now()
	r.mu.Unlock()
}

func (r *ReconnectingClient) reconnectDelay(attempt int) time.Duration {
	delay := r.opts.InitialDelay
	for i := 1; i < attempt && delay < r.opts.MaxDelay; i++ {
		if delay > r.opts.MaxDelay/2 {
			return r.opts.MaxDelay
		}
		delay *= 2
	}
	if delay > r.opts.MaxDelay {
		return r.opts.MaxDelay
	}
	return delay
}

func (r *ReconnectingClient) startBase(ctx context.Context) error {
	release, _, err := r.beginOperation()
	if err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	r.mu.Lock()
	base := r.base
	factory := r.factory
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if base == nil {
		return ErrStartFailed
	}
	if factory != nil {
		candidate, err := factory(ctx)
		if err != nil {
			return err
		}
		if candidate == nil {
			return ErrStartFailed
		}
		r.attachBaseHandlers(candidate)
		r.startMu.Lock()
		err = candidate.Start(ctx)
		r.startMu.Unlock()
		if err != nil {
			if closeErr := closeClientWithin(candidate, generationCloseTimeout); closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return errors.Join(ErrClosed, closeClientWithin(candidate, generationCloseTimeout))
		}
		oldGeneration := r.generation
		r.base = candidate
		r.generation++
		r.mu.Unlock()
		// Release the supervisor's operation before waiting for the old
		// generation. New requests are now counted against the candidate's
		// generation and must not prevent the old transport from being retired.
		release()
		released = true
		if !waitGenerationIdle(r, oldGeneration, generationCloseTimeout) {
			// Do not close a transport while one of its requests is still
			// executing. A stuck request is a hard lifecycle failure; the
			// caller must explicitly restart the bridge rather than allowing
			// overlapping generations.
			r.mu.Lock()
			if r.base == candidate {
				r.base = nil
			}
			r.mu.Unlock()
			_ = closeClientWithin(candidate, generationCloseTimeout)
			return fmt.Errorf("%w before retiring generation after %s", errGenerationCloseTimeout, generationCloseTimeout)
		}
		// The candidate is live and the old generation is quiescent. Retire
		// it only after the swap so stale callbacks cannot affect the new
		// generation.
		if base != candidate {
			if err := closeClientWithin(base, generationCloseTimeout); err != nil {
				// Do not report the reconnect as healthy when the previous
				// generation could not be retired. Continuing would permit
				// overlapping server processes and duplicate tool ownership.
				r.mu.Lock()
				if r.base == candidate {
					r.base = nil
				}
				r.mu.Unlock()
				_ = closeClientWithin(candidate, generationCloseTimeout)
				return err
			}
		}
		r.markConnected()
		return nil
	}
	r.startMu.Lock()
	err = base.Start(ctx)
	r.startMu.Unlock()
	if err == nil {
		r.markConnected()
	}
	return err
}

func waitGenerationIdle(r *ReconnectingClient, generation uint64, timeout time.Duration) bool {
	r.mu.Lock()
	idle := r.generationIdle[generation]
	active := r.activeByGen[generation]
	r.mu.Unlock()
	if active == 0 {
		return true
	}
	if idle == nil {
		return false
	}
	return waitChannelWithin(idle, timeout)
}

// closeClientWithin applies the generation close barrier to the deliberately
// context-free Client interface. On timeout the caller must stop progressing
// the generation state; otherwise a later reconnect could overlap a live
// process. The close goroutine is intentionally not detached by callers that
// continue reconnecting: timeout is returned as a hard lifecycle failure.
func closeClientWithin(client Client, timeout time.Duration) error {
	if client == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("%w after %s", errGenerationCloseTimeout, timeout)
	}
}

func waitChannelWithin(ch <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

func waitWaitGroupWithin(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return waitChannelWithin(done, timeout)
}

func (r *ReconnectingClient) reconnectPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.reconnecting
}

func shouldReconnect(err error) bool {
	return errors.Is(err, ErrConnection) ||
		errors.Is(err, ErrStartFailed) ||
		errors.Is(err, ErrHandshake) ||
		errors.Is(err, ErrTimeout)
}
