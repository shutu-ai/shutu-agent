// Package code defines the code-sandbox capability seam (design.md §10 D2,
// ADR 2026-08-19-m6-agent-full.md 决策 M6e): a Provider + Engine seam for
// executing code/commands in a controlled sandbox. Production PTC uses the
// TypeScript ProgramRuntime below; the Engine seam remains for the legacy
// shell provider tests. An Engine (the seam's
// Service) delegates to a Provider; the default Provider is the local
// subprocess sandbox (local.go). Consumers (M6e-2's run_code tool and the
// code/* event wiring) depend only on the seam's interfaces (D2), so swapping
// the backend never touches consumer code.
//
// Controlled isolation (ADR M6e) — the local provider enforces:
//   - process boundary: the code runs as an independent child process
//     (os/exec), never in-process;
//   - timeout: execution is bounded by RunRequest.Timeout (default 30s) via
//     exec.CommandContext; on expiry the direct child is hard-killed and
//     Result.TimedOut is set;
//   - output quota: Stdout and Stderr are each capped at RunRequest.MaxOutput
//     bytes (default 64KiB); overflow is truncated and Result.Truncated is
//     set;
//   - sandbox cwd: execution happens in an isolated working directory,
//     defaulting to <cwd base>/.sandbox and created on demand;
//   - default no network (declarative): the child inherits the parent
//     environment with credential-shaped entries removed, so no network
//     credentials are injected. This is a boundary, not strong isolation —
//     Windows has no network namespace, so denying network access at the OS
//     level is out of scope (recorded here; a config-level declarative switch
//     is deferred to M6e-2). A sandboxed child that knows where to connect can
//     still reach the network on its own.
//
// The local sandbox is controlled isolation, not a security boundary for
// hostile code (same posture as M3's run_command, docs/decisions/
// 2026-08-18-m3-sandbox-scope.md). A lingering grandchild that outlives the
// direct child (e.g. ping spawned by cmd.exe after the direct child is
// killed) is a documented residual risk — it never blocks Run because output
// is captured to temp files, not pipes.
//
// On Windows the code runs through cmd /C as a single non-interactive command
// line (same boundary as M3's run_command); embedded double quotes must be
// cmd-compatible because exec.Cmd re-quotes argv when it contains them. There
// is deliberately no interactive shell and no per-command quoting layer in v1.
//
// Execution is foreground and serial (design.md §10 D5): Run blocks until the
// child exits (or is hard-killed) and returns synchronously; there are no
// background goroutines or timers. Close is idempotent on both Provider and
// Engine.
package code

import (
	"context"
	"errors"
	"time"
)

// Result is the outcome of one sandbox run. Stdout/Stderr are bounded: each
// stream is capped at the request's MaxOutput bytes and truncated at the cap
// with Truncated set. TimedOut marks a run hard-killed by the timeout;
// Duration measures wall time from child start to exit (or kill). A non-zero
// ExitCode and a TimedOut run are normal sandbox outcomes — they are reported
// in Result with a nil error; the error return is reserved for failures to
// run at all (invalid request, closed sandbox, start failure, cancelled
// context).
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
}

// RunRequest is one sandbox invocation. The zero value runs Lang "sh",
// Code in the default sandbox cwd (<cwd base>/.sandbox) with a 30s timeout
// and a 64KiB per-stream output cap.
type RunRequest struct {
	Lang      string        // "sh" (default) | future languages
	Code      string        // command/script to execute (required, non-blank)
	Cwd       string        // sandbox working dir (default <cwd base>/.sandbox)
	Timeout   time.Duration // 0 → default 30s
	MaxOutput int           // per-stream byte cap; 0 → default 64KiB
}

// ProgramBindingRequest describes one tool call made by a TypeScript Code
// Mode program. The binding is deliberately name-based so this package stays
// independent from the host tool registry and its session types.
type ProgramBindingRequest struct {
	CallID string
	Name   string
	Args   any
}

// ProgramBinding is the host callback exposed to a Code Mode runtime. It must
// return a lossless JSON-compatible value or an error that the program can
// catch as a rejected tools.<name>() promise.
type ProgramBinding func(context.Context, ProgramBindingRequest) (any, error)

// ProgramRequest is one DSH-style TypeScript Code Mode execution. Code is the
// body of an async function, so top-level await and return are supported.
type ProgramRequest struct {
	Code         string
	Cwd          string
	Timeout      time.Duration
	MaxOutput    int
	ParentCallID string
	Binding      ProgramBinding
}

// ProgramFailure is a model-facing runtime failure. Program failures are
// returned as data by the runtime; only host misuse/startup failures use the
// Go error return.
type ProgramFailure struct {
	Kind    string
	Message string
}

// ProgramResult is the DSH CodeRuntime result: ordered captured logs, an
// optional lossless completion value, and an orthogonal failure field.
type ProgramResult struct {
	Value     any
	HasValue  bool
	Logs      []string
	Failure   *ProgramFailure
	Truncated bool
	Duration  time.Duration
}

// ProgramRuntime executes TypeScript Code Mode programs against host-provided
// asynchronous bindings. It is separate from the legacy shell Provider seam so
// the consumer can select the DSH runtime explicitly.
type ProgramRuntime interface {
	RunProgram(context.Context, ProgramRequest) (ProgramResult, error)
	Close() error
}

// Provider is one sandbox backend (design.md §10 D2: Service / Provider /
// Consumer three-piece seam). It executes a RunRequest under the controlled-
// isolation semantics documented in the package comment and returns the
// outcome. Close releases the backend and is idempotent.
type Provider interface {
	Name() string
	// Run executes req in the sandbox and returns the outcome. A non-zero exit
	// code and a timeout are normal outcomes returned as (Result, nil); the
	// error return signals the run did not happen (invalid request, closed
	// provider, cancelled context, start failure).
	Run(ctx context.Context, req RunRequest) (Result, error)
	// Close releases the backend; it is idempotent and subsequent Run calls
	// are rejected with ErrProviderClosed.
	Close() error
}

// Engine is the code-sandbox Service (design.md §10 D2, ADR 决策 M6e).
// Consumers depend only on this interface, never on a concrete backend. Run
// delegates to the configured Provider — when no Provider is set, the local
// subprocess sandbox (NewLocalProvider) is used. Close is idempotent and
// releases the Provider.
type Engine interface {
	// Run executes req through the configured Provider and returns the
	// outcome (see Provider.Run for the outcome-vs-error contract).
	Run(ctx context.Context, req RunRequest) (Result, error)
	// Close releases the Provider and marks the engine closed so further Run
	// calls are rejected with ErrEngineClosed. It is idempotent.
	Close() error
}

// closer is the optional extension a Provider implements so the Engine can
// release the backend when closed (mirrors the schedule and interact seams).
type closer interface {
	Close() error
}

// Sentinel errors returned by the seam so callers can distinguish failures
// without parsing message text.
var (
	ErrInvalidRequest  = errors.New("code: invalid request")
	ErrUnsupportedLang = errors.New("code: unsupported language")
	ErrEngineClosed    = errors.New("code: engine closed")
	ErrProviderClosed  = errors.New("code: provider closed")
)
