// Package jobs defines the background-job capability seam (design.md §10 D5,
// ADR 2026-08-18-m5-agent-core.md 决策 ①). Jobs lets long-running work run in a
// background goroutine without ever entering the serial turn/step loop: it is
// started, observed, cancelled, awaited and notified through the Registry
// interface (the seam's Service), backed by the in-memory Local provider
// (Provider) and consumed by model-facing job_* tools in M5a-2 (Consumer).
//
// The registry is owner-fenced: each job belongs to an owner session and
// Get/Read/Kill/Wait authorize access by matching the caller session against
// the job's owner. A job without an owner (empty OwnerSession) is visible to
// any caller — the shared "unowned" bucket used for daemon work. Owner
// matching, not id secrecy, is the authorization boundary: ids are predictable
// (<kind>-N) and are not kept confidential.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jabing/shutu-agent/internal/runtimectx"
)

// Status is a job's lifecycle state (dsh jobs JobStatus): running, optionally
// stopping after a kill request, then exactly one terminal status.
type Status string

const (
	StatusRunning   Status = "running"   // work is executing
	StatusStopping  Status = "stopping"  // a kill/cancel request is in flight
	StatusCompleted Status = "completed" // terminal: work finished normally
	StatusKilled    Status = "killed"    // terminal: cancelled before finishing
	StatusFailed    Status = "failed"    // terminal: run returned an error or panicked
)

// Kind is an opaque id namespace for producers ("bash", "subagent",
// "extract", …). The registry never interprets a Kind — it only prefixes
// issued ids with it (<kind>-N) — so new producers need no registry change.
type Kind string

// JobSnapshot is a read-only projection of one job. Callers receive a fresh
// value copy, never live registry state.
type JobSnapshot struct {
	ID               string     // registry-issued id ("<kind>-N")
	Kind             Kind       // producer kind the job was registered with
	Label            string     // one-line model-facing label
	OwnerSession     string     // authorization boundary: owner session id ("" = unowned)
	Status           Status     // current lifecycle state
	Detail           string     // kind-specific detail ("" until the producer supplies one, usually terminal)
	StartedAt        time.Time  // when the job was registered
	FinishedAt       *time.Time // set once the job settles; nil while running/stopping
	OutputLimitBytes int        // per-job byte cap on the stored output (0 = unlimited)
	// Correlation is the runtime identity captured at registration. Background
	// work outlives the initiating tool context, so observers must not recover
	// identity from labels or durable human-facing event text.
	Correlation runtimectx.Correlation
}

// JobStart describes one job to start.
type JobStart struct {
	Kind         Kind
	Label        string
	OwnerSession string // "" = unowned job, visible to any caller
	// CWD is the addressed session's captured workspace. Host wiring should
	// populate it so a background command does not depend on process cwd.
	CWD              string
	OutputLimitBytes int // >0 truncates the stored terminal output to this cap
	// Correlation is copied from the initiating runtime context when available.
	// Empty fields are valid for host-created daemon jobs.
	Correlation runtimectx.Correlation
	// ReadOutput, when supplied, returns output produced since the previous
	// read. This is dsh's consuming stream contract; the final JobOutcome.Output
	// remains the fallback for jobs that do not expose a stream.
	ReadOutput func() (string, error)

	// Run is the foreground cancelable body. The registry invokes it once in a
	// background goroutine after registration and settles the job from its
	// returned outcome; a non-nil error settles the job as failed. Run observes
	// cancellation through its context (cancelled by Kill/Close).
	Run func(ctx context.Context) (JobOutcome, error)
	// Cancel is the producer's synchronous, idempotent cancel hook, invoked by
	// Kill and Close with the reason forwarded verbatim. A nil Cancel means
	// context cancellation is the only kill mechanism. It must not block.
	Cancel func(reason string) error
	// OnSettled is called once after the job reaches a terminal state. It is an
	// observation/delivery hook; the registry still owns lifecycle state.
	OnSettled func(JobSnapshot)
}

// IDReservation reserves a provider-issued job id in a durable namespace.
// Local remains fully usable in memory when this hook is nil.
type IDReservation func(context.Context, string, string) (bool, error)

// JobOutcome is the terminal result of a job's Run.
type JobOutcome struct {
	Status Status // completed | killed | failed
	Detail string // kind-specific detail ("exit code: 3", "max-tokens", …)
	Output string // final output; truncated to OutputLimitBytes when bounded
}

// CorrelationFromContext snapshots the initiating runtime identity for a
// background job. It returns the zero value for host-created work that has no
// runtime context; callers can pass the result directly to JobStart.
func CorrelationFromContext(ctx context.Context) runtimectx.Correlation {
	if correlation, ok := runtimectx.CorrelationOf(ctx); ok {
		return correlation
	}
	return runtimectx.Correlation{}
}

// CompletionObserver receives every first terminal transition together with
// the registry-owned, already-bounded final output. It is deliberately
// provider-level rather than tied to one model-facing tool: schedule,
// subagent and terminal producers must be able to wake the owning Agent even
// when no job_* observation call is made.
type CompletionObserver func(JobSnapshot, string)

// Registry is the background-job Service (design.md §10 D2). Consumers depend
// only on this interface, never on a concrete backend, so swapping the
// provider never touches consumer code.
//
// Lifecycle (ADR 决策 ①): Start preflights (spec validation + concurrency cap)
// then registers and runs the job in a background goroutine; List/Get observe
// owner-filtered snapshots; Read returns the terminal output idempotently (""
// while running); Kill requests cancellation and returns "requested" or
// "already-finished"; Wait blocks up to timeout and returns the current
// snapshot; Close cancels and awaits every live job so no goroutine leaks.
type Registry interface {
	// Start validates the spec and the concurrency cap, registers a running job
	// under a fresh "<kind>-N" id, and returns that registry-issued id.
	Start(ctx context.Context, spec JobStart) (string, error)
	// List returns every snapshot visible to callerSession (jobs it owns plus
	// all unowned jobs), sorted by id.
	List(ctx context.Context, callerSession string) ([]JobSnapshot, error)
	// Get returns the snapshot for id. Cross-owner access is rejected.
	Get(ctx context.Context, id, callerSession string) (JobSnapshot, error)
	// Read returns the job's output and snapshot. For a final-output job the
	// output is "" while live and the terminal Output once settled — idempotent,
	// never consumed. Cross-owner access is rejected.
	Read(ctx context.Context, id, callerSession string) (string, JobSnapshot, error)
	// Kill requests cancellation of a live job: it invokes the producer's Cancel
	// hook (if any), cancels the job context, and marks the job "stopping". It
	// returns "requested" for a live job or "already-finished" for a terminal
	// one and is idempotent. Cross-owner access is rejected.
	Kill(ctx context.Context, id, callerSession, reason string) (string, error)
	// Wait blocks until the job settles, ctx is cancelled, or timeout elapses.
	// On settlement it returns the terminal snapshot; on timeout it returns the
	// current snapshot with a nil error; on ctx cancellation it returns the
	// context error. Cross-owner access is rejected.
	Wait(ctx context.Context, id, callerSession string, timeout time.Duration) (JobSnapshot, error)
	// Close cancels and awaits every live job so no background goroutine leaks
	// (lifecycle is reversible, ADR 决策 ①). Start after Close is rejected.
	Close() error
}

// DeltaReader is the optional streaming-output extension implemented by job
// providers that can expose output before settlement. Consumers must fall back
// to Registry.Read when a provider does not implement it.
type DeltaReader interface {
	ReadDelta(ctx context.Context, id, callerSession string) (string, JobSnapshot, error)
}

// Sentinel errors returned by Registry implementations so callers can
// distinguish authorization failures, unknown ids, closed registries, and the
// concurrency cap without parsing message text.
var (
	ErrUnknownJob     = errors.New("jobs: unknown job")
	ErrForbidden      = errors.New("jobs: job belongs to another session")
	ErrRegistryClosed = errors.New("jobs: registry closed")
	ErrOwnerClosed    = errors.New("jobs: owner is closed")
	ErrLimitReached   = errors.New("jobs: background job limit reached for this owner")
)
