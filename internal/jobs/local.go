package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// defaultMaxConcurrentJobsPerOwner is the active-job cap applied when
// LocalOpts.MaxConcurrentJobsPerOwner is <= 0 (dsh jobs-local default 10).
const defaultMaxConcurrentJobsPerOwner = 10

// LocalOpts configures the in-memory Local provider.
type LocalOpts struct {
	// MaxConcurrentJobsPerOwner caps the number of running+stopping jobs in one
	// exact-owner bucket (and the shared unowned bucket). <= 0 means the
	// default (10).
	MaxConcurrentJobsPerOwner int
}

// Local is the default in-memory Registry provider (ADR 决策 ①). Every record
// lives in memory only — nothing is persisted and no files are touched — so a
// process restart clears the job table by construction. It is safe for
// concurrent use by many owners.
type Local struct {
	maxConcurrentJobsPerOwner int

	mu       sync.Mutex
	jobs     map[string]*jobRecord
	counters map[Kind]int
	closed   bool
}

// jobRecord is the registry's mutable per-job record. It is never handed out:
// callers receive fresh JobSnapshot copies.
type jobRecord struct {
	id          string
	kind        Kind
	label       string
	owner       string
	status      Status
	detail      string
	output      string
	outputLimit int
	startedAt   time.Time
	finishedAt  *time.Time

	run       func(ctx context.Context) (JobOutcome, error)
	cancel    func(reason string) error
	ctx       context.Context
	cancelJob context.CancelFunc
	done      chan struct{} // closed once the job settles

	reported   bool // terminal state has been reported to some consumer (read/kill/wait)
	waiters    int  // live waits, for settlement-time reported marking
	readMu     sync.Mutex
	readOutput func() (string, error)
}

// NewLocal returns a Local provider. MaxConcurrentJobsPerOwner <= 0 selects the
// default of 10.
func NewLocal(opts LocalOpts) *Local {
	max := opts.MaxConcurrentJobsPerOwner
	if max <= 0 {
		max = defaultMaxConcurrentJobsPerOwner
	}
	return &Local{
		maxConcurrentJobsPerOwner: max,
		jobs:                      map[string]*jobRecord{},
		counters:                  map[Kind]int{},
	}
}

// Start registers a running job and returns its registry-issued id.
//
// Preflight runs before any registration: the spec is validated (non-empty
// kind/label, non-nil Run, non-negative output limit) and the owner's active-job
// cap is checked, then registration and the background goroutine follow
// atomically. Owner-existence against a live session registry is M5a-2 wiring;
// this layer validates the spec fields and the concurrency cap only. The job's
// Run context is registry-owned, independent of Start's ctx, so a background
// job outlives the call that started it.
func (l *Local) Start(ctx context.Context, spec JobStart) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if spec.Kind == "" {
		return "", errors.New("jobs: invalid job kind: expected a non-empty value")
	}
	if spec.Label == "" {
		return "", errors.New("jobs: invalid job label: expected a non-empty value")
	}
	if spec.Run == nil {
		return "", errors.New("jobs: invalid job run: nil Run")
	}
	if spec.OutputLimitBytes < 0 {
		return "", errors.New("jobs: invalid output limit: must be non-negative")
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return "", ErrRegistryClosed
	}
	if l.activeCountLocked(spec.OwnerSession) >= l.maxConcurrentJobsPerOwner {
		l.mu.Unlock()
		return "", fmt.Errorf("%w (limit: %d)", ErrLimitReached, l.maxConcurrentJobsPerOwner)
	}

	count := l.counters[spec.Kind] + 1
	l.counters[spec.Kind] = count
	id := fmt.Sprintf("%s-%d", spec.Kind, count)

	jobCtx, cancelJob := context.WithCancel(context.Background())
	rec := &jobRecord{
		id:          id,
		kind:        spec.Kind,
		label:       spec.Label,
		owner:       spec.OwnerSession,
		status:      StatusRunning,
		outputLimit: spec.OutputLimitBytes,
		startedAt:   time.Now(),
		run:         spec.Run,
		cancel:      spec.Cancel,
		ctx:         jobCtx,
		cancelJob:   cancelJob,
		done:        make(chan struct{}),
		readOutput:  spec.ReadOutput,
	}
	l.jobs[id] = rec
	l.mu.Unlock()

	go l.runJob(rec)
	return id, nil
}

// runJob drives one job's Run body in a background goroutine and settles the
// record from its outcome. A panic is contained and settles the job as failed
// so a misbehaving producer can never crash the process.
func (l *Local) runJob(rec *jobRecord) {
	outcome, err := func() (out JobOutcome, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return rec.run(rec.ctx)
	}()
	if err != nil {
		l.settle(rec, JobOutcome{Status: StatusFailed, Detail: err.Error()})
		return
	}
	l.settle(rec, outcome)
}

// settle records the first terminal outcome for a job: status, detail, a
// bounded output, finishedAt, then releases waiters and marks the job
// reported. First-wins: a later settlement (e.g. a Close force-failure racing
// the producer's own terminal outcome) is ignored.
func (l *Local) settle(rec *jobRecord, outcome JobOutcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if isTerminal(rec.status) {
		return
	}
	status := outcome.Status
	if status != StatusCompleted && status != StatusKilled && status != StatusFailed {
		status = StatusFailed
	}
	now := time.Now()
	rec.status = status
	rec.detail = outcome.Detail
	rec.output = capOutput(outcome.Output, rec.outputLimit)
	rec.finishedAt = &now
	if rec.waiters > 0 {
		rec.reported = true
	}
	close(rec.done)
}

// List returns every snapshot visible to callerSession, sorted by id.
func (l *Local) List(ctx context.Context, callerSession string) ([]JobSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]JobSnapshot, 0, len(l.jobs))
	for _, rec := range l.jobs {
		if !canAccess(rec, callerSession) {
			continue
		}
		out = append(out, snapshotOf(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the snapshot for id, rejecting cross-owner access.
func (l *Local) Get(ctx context.Context, id, callerSession string) (JobSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return JobSnapshot{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, err := l.lookupLocked(id, callerSession)
	if err != nil {
		return JobSnapshot{}, err
	}
	return snapshotOf(rec), nil
}

// Read returns the job's output and snapshot. For a final-output job the output
// is "" while live and the terminal Output once settled — idempotent, never
// consumed. Cross-owner access is rejected.
func (l *Local) Read(ctx context.Context, id, callerSession string) (string, JobSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return "", JobSnapshot{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, err := l.lookupLocked(id, callerSession)
	if err != nil {
		return "", JobSnapshot{}, err
	}
	var text string
	if isTerminal(rec.status) {
		text = rec.output
		rec.reported = true
	}
	return text, snapshotOf(rec), nil
}

// ReadDelta returns output since the previous read for this caller. Producers
// with ReadOutput own the consuming cursor, matching dsh's job registry; jobs
// without a stream expose their final output after settlement.
func (l *Local) ReadDelta(ctx context.Context, id, callerSession string) (string, JobSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return "", JobSnapshot{}, err
	}
	l.mu.Lock()
	rec, err := l.lookupLocked(id, callerSession)
	if err != nil {
		l.mu.Unlock()
		return "", JobSnapshot{}, err
	}
	l.mu.Unlock()
	rec.readMu.Lock()
	defer rec.readMu.Unlock()
	l.mu.Lock()
	snap := snapshotOf(rec)
	reader := rec.readOutput
	limit := rec.outputLimit
	finalOutput := rec.output
	l.mu.Unlock()

	output := finalOutput
	if reader != nil {
		live, readErr := reader()
		if readErr != nil {
			return "", snap, readErr
		}
		output = live
	}
	if limit > 0 {
		output = capOutput(output, limit)
	}
	// The producer may have settled while ReadOutput was running. Refresh the
	// snapshot after the callback so job_output reports the state observed by
	// the same read, matching dsh's registry behavior.
	l.mu.Lock()
	if current, ok := l.jobs[id]; ok && canAccess(current, callerSession) {
		snap = snapshotOf(current)
		if reader == nil && isTerminal(current.status) {
			output = current.output
			if limit > 0 {
				output = capOutput(output, limit)
			}
		}
	}
	l.mu.Unlock()
	return output, snap, nil
}

// Kill requests cancellation of a live job: it invokes the producer's Cancel
// hook (if any), cancels the job context, and marks the job "stopping". It
// returns "requested" for a live job or "already-finished" for a terminal one
// and is idempotent. Cross-owner access is rejected.
func (l *Local) Kill(ctx context.Context, id, callerSession, reason string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	l.mu.Lock()
	rec, err := l.lookupLocked(id, callerSession)
	if err != nil {
		l.mu.Unlock()
		return "", err
	}
	if isTerminal(rec.status) {
		rec.reported = true
		l.mu.Unlock()
		return "already-finished", nil
	}
	cancelHook := rec.cancel
	cancelJob := rec.cancelJob
	l.mu.Unlock()

	// Invoke the producer hook first so a throwing cancel leaves lifecycle
	// state unchanged; then cancel the job context so Run observes it. Both are
	// called outside the registry lock — a producer hook must never run under
	// it. The job may settle between the hook call and the status transition;
	// the re-check below reports that as already-finished.
	if cancelHook != nil {
		if err := cancelHook(reason); err != nil {
			return "", fmt.Errorf("jobs: kill %s: cancel: %w", id, err)
		}
	}
	cancelJob()

	l.mu.Lock()
	if isTerminal(rec.status) {
		rec.reported = true
		l.mu.Unlock()
		return "already-finished", nil
	}
	rec.status = StatusStopping
	rec.reported = true
	l.mu.Unlock()
	return "requested", nil
}

// Wait blocks until the job settles, ctx is cancelled, or timeout elapses. On
// settlement it returns the terminal snapshot; on timeout it returns the
// current snapshot with a nil error; on ctx cancellation it returns the
// context error. Cross-owner access is rejected.
func (l *Local) Wait(ctx context.Context, id, callerSession string, timeout time.Duration) (JobSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return JobSnapshot{}, err
	}
	if timeout <= 0 {
		return JobSnapshot{}, errors.New("jobs: invalid wait timeout: must be positive")
	}
	l.mu.Lock()
	rec, err := l.lookupLocked(id, callerSession)
	if err != nil {
		l.mu.Unlock()
		return JobSnapshot{}, err
	}
	if isTerminal(rec.status) {
		rec.reported = true
		snap := snapshotOf(rec)
		l.mu.Unlock()
		return snap, nil
	}
	rec.waiters++
	done := rec.done
	l.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var waitErr error
	select {
	case <-done:
	case <-ctx.Done():
		waitErr = ctx.Err()
	case <-timer.C:
	}

	l.mu.Lock()
	rec.waiters--
	if isTerminal(rec.status) && waitErr == nil {
		rec.reported = true
	}
	snap := snapshotOf(rec)
	l.mu.Unlock()

	if waitErr != nil {
		return JobSnapshot{}, waitErr
	}
	return snap, nil
}

// Close cancels and awaits every live job so no background goroutine leaks
// (lifecycle is reversible, ADR 决策 ①). Start after Close is rejected; reads on
// already-settled jobs still work. Close is idempotent.
func (l *Local) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	recs := make([]*jobRecord, 0, len(l.jobs))
	for _, rec := range l.jobs {
		recs = append(recs, rec)
	}
	live := make([]*jobRecord, 0, len(recs))
	for _, rec := range recs {
		if !isTerminal(rec.status) {
			rec.status = StatusStopping
			rec.reported = true
			live = append(live, rec)
		}
	}
	l.mu.Unlock()

	// Invoke producer cancel hooks outside the registry lock. The job context is
	// always cancelled so a ctx-respecting Run can settle; a throwing cancel
	// hook force-fails the record (closing done) so Close can neither hang nor
	// leave a running record — though the work itself may be orphaned (producer
	// contract violation, same stance as dsh jobs-local).
	for _, rec := range live {
		var cancelErr error
		if rec.cancel != nil {
			cancelErr = rec.cancel("jobs service closed")
		}
		rec.cancelJob()
		if cancelErr != nil {
			l.settle(rec, JobOutcome{Status: StatusFailed, Detail: "cancel failed during close: " + cancelErr.Error()})
		}
	}
	for _, rec := range recs {
		<-rec.done
	}
	return nil
}

// lookupLocked returns the record for id and checks owner access. The caller
// must hold l.mu.
func (l *Local) lookupLocked(id, callerSession string) (*jobRecord, error) {
	rec, ok := l.jobs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, id)
	}
	if !canAccess(rec, callerSession) {
		return nil, fmt.Errorf("%w: %s", ErrForbidden, id)
	}
	return rec, nil
}

// activeCountLocked counts running+stopping jobs in one bucket: the exact
// owner, or the shared unowned bucket for "". Terminal settlement releases the
// slot implicitly because the count is derived from live records. The caller
// must hold l.mu.
func (l *Local) activeCountLocked(owner string) int {
	count := 0
	for _, rec := range l.jobs {
		if rec.owner == owner && (rec.status == StatusRunning || rec.status == StatusStopping) {
			count++
		}
	}
	return count
}

// isTerminal reports whether s is one of the three terminal statuses.
func isTerminal(s Status) bool {
	return s == StatusCompleted || s == StatusKilled || s == StatusFailed
}

// canAccess reports whether callerSession may observe a job: an unowned job is
// open to any caller; an owned job is visible only to its owner.
func canAccess(rec *jobRecord, callerSession string) bool {
	if rec.owner == "" {
		return true
	}
	return rec.owner == callerSession
}

// snapshotOf projects a fresh read-only copy of a record. The caller must hold
// l.mu. FinishedAt is copied so the returned snapshot never aliases the
// record's pointer.
func snapshotOf(rec *jobRecord) JobSnapshot {
	snap := JobSnapshot{
		ID:               rec.id,
		Kind:             rec.kind,
		Label:            rec.label,
		OwnerSession:     rec.owner,
		Status:           rec.status,
		Detail:           rec.detail,
		StartedAt:        rec.startedAt,
		OutputLimitBytes: rec.outputLimit,
	}
	if rec.finishedAt != nil {
		t := *rec.finishedAt
		snap.FinishedAt = &t
	}
	return snap
}

// capOutput bounds an output string to at most limit bytes (limit <= 0 means
// unlimited): it keeps a UTF-8-safe head and appends a truncation notice
// carrying the omitted byte count (mirrors internal/tools truncateResult, but
// spill-to-disk is M5a-2's concern — this layer only truncates and marks).
func capOutput(out string, limit int) string {
	if limit <= 0 || len(out) <= limit {
		return out
	}
	const prefix = "\n\n[output truncated: "
	const suffix = " bytes omitted]"
	placeholder := prefix + strconv.Itoa(len(out)) + suffix
	budget := limit - len(placeholder)
	if budget < 0 {
		budget = 0
	}
	head := truncateUTF8(out, budget)
	omitted := len(out) - len(head)
	notice := prefix + strconv.Itoa(omitted) + suffix
	return head + notice
}

// truncateUTF8 shortens s to at most maxBytes bytes, backing off until the
// prefix is valid UTF-8 (never splits a rune).
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes < 0 || len(s) <= maxBytes {
		return s
	}
	b := []byte(s)
	b = b[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
