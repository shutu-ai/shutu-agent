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
//   - child resource ceilings: Linux controlled shells use prlimit before
//     sandbox exec to install hard virtual-memory, file-size, and
//     process-count limits. macOS uses inherited shell hard limits. Windows
//     uses Job Object per-process memory and active-process ceilings where the
//     host supports it; Windows ACL does not claim a native per-file quota.
//   - sandbox cwd: execution happens in an isolated working directory,
//     defaulting to <cwd base>/.sandbox and created on demand;
//   - default no network: on Linux, the bubblewrap backend is advertised as
//     network-isolated only after a functional network-namespace probe and
//     executes non-full-access calls with --unshare-net. macOS Seatbelt is
//     file-effect enforcing but does not claim network isolation. Hosts without
//     an enforcing backend fail closed when controlled isolation is required.
//     Credential-shaped environment entries are scrubbed on every host as an
//     additional boundary.
//
// The local sandbox is an enforcing backend for Linux file/network modes when
// bubblewrap is available, macOS file modes when Seatbelt is available, and
// Windows file-effect modes when the ACL restricted-token probe passes. The
// Windows ACL path does not claim network/read/process isolation. A lingering
// grandchild is contained by the process-tree cleanup seam;
// output is captured to temp files rather than pipes so it cannot block Run.
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

	"github.com/shutu-ai/shutu-agent/internal/llm"
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
	// Mode selects the requested filesystem authority. An empty mode means
	// workspace-write for backwards compatibility; providers must reject modes
	// they cannot enforce rather than silently widening authority.
	Mode                    SandboxMode
	Root                    string // policy root; Cwd must remain below it when set
	RequireStrongIsolation  bool
	RequireNetworkIsolation bool
	AllowNetwork            bool
	Lang                    string        // "sh" (default) | future languages
	Code                    string        // command/script to execute (required, non-blank)
	Cwd                     string        // sandbox working dir (default <cwd base>/.sandbox)
	Timeout                 time.Duration // 0 → default 30s
	MaxOutput               int           // per-stream byte cap; 0 → default 64KiB
	// MaxMemoryBytes, MaxFileSizeBytes, and MaxProcesses are hard child
	// limits for controlled shell modes. Zero selects the provider defaults;
	// they are ignored for explicit danger-full-access runs.
	MaxMemoryBytes   int64
	MaxFileSizeBytes int64
	MaxProcesses     int
}

// SandboxMode is the per-call authority tier requested from a sandbox.
type SandboxMode string

const (
	// SandboxReadOnly is the canonical wire spelling used by DeepSeek Harness.
	SandboxReadOnly SandboxMode = "read-only"
	// SandboxReadOnlyLegacy remains accepted for existing Shutu config files;
	// it is normalized before capability checks and execution.
	SandboxReadOnlyLegacy SandboxMode = "readonly"
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	SandboxFullAccess     SandboxMode = "danger-full-access"
)

// IsolationLevel names the security contract actually supplied by a backend.
// "containment" is a filesystem-effect fence against accidental escape; it is
// not a claim of equivalence to an OS strong-isolation boundary.
type IsolationLevel string

const (
	IsolationNone        IsolationLevel = "none"
	IsolationContainment IsolationLevel = "containment"
	IsolationStrong      IsolationLevel = "strong"
)

func normalizeSandboxMode(mode SandboxMode) SandboxMode {
	if mode == SandboxReadOnlyLegacy {
		return SandboxReadOnly
	}
	return mode
}

// ProgramBindingRequest describes one tool call made by a TypeScript Code
// Mode program. The binding is deliberately name-based so this package stays
// independent from the host tool registry and its session types.
type ProgramBindingRequest struct {
	CallID    string
	Namespace string
	Name      string
	Args      any
}

// ProgramBindingNamespace declares one program-visible binding object. Member
// names are intentionally not enumerated: the host registry remains the
// authority and can expose arbitrary property names such as "__proto__" or
// "read-file" without prototype collisions.
type ProgramBindingNamespace struct {
	Global                  string
	ErrorClassName          string
	ErrorMemberNameProperty string
}

// ProgramBindingResult keeps the JSON value returned to TypeScript separate
// from the rich content projection retained by the host's durable dispatch
// event. This mirrors the reference bridge's value/content split.
type ProgramBindingResult struct {
	Value                     any
	Content                   []map[string]any
	Meta                      any
	AdditionalContexts        []string
	AdditionalContextMessages []llm.Message
	ConcludesTurn             bool
}

func cloneCodeContextMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = append([]llm.ContentBlock(nil), message.Content...)
		out[i].ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
	}
	return out
}

// ProgramBinding is the host callback exposed to a Code Mode runtime. It must
// return a lossless JSON-compatible value or an error that the program can
// catch as a rejected tools.<name>() promise.
type ProgramBinding func(context.Context, ProgramBindingRequest) (any, error)

// ProgramRequest is one DSH-style TypeScript Code Mode execution. Code is the
// body of an async function, so top-level await and return are supported.
type ProgramRequest struct {
	Code    string
	Cwd     string
	Timeout time.Duration
	// ComputeMS is the measured busy-time budget for the TypeScript worker.
	ComputeMS int
	// MaxWallMS is the hard wall-clock ceiling, independent of awaited host
	// bindings. Timeout remains the compatibility fallback when this is zero.
	MaxWallMS int
	MaxOutput int
	// MaxOldGenerationSizeMB applies a process heap ceiling when non-zero. It
	// is enforced by V8 for the subprocess backend; process-wide CPU is
	// enforced separately by the platform process/job boundary or monitor.
	MaxOldGenerationSizeMB int
	ParentCallID           string
	Binding                ProgramBinding
	// Bindings declares the globals exposed to the program. An empty list keeps
	// the legacy single `tools` namespace for standalone callers.
	Bindings []ProgramBindingNamespace
	// IsConcurrencySafe classifies a prepared host call. A supplied
	// classifier that returns false (or panics) makes the call exclusive;
	// leaving it nil retains the legacy runtime behavior for standalone users.
	IsConcurrencySafe func(name string, args any) bool
	// MaxParallelSubCalls bounds one consecutive safe group. Zero uses the
	// runtime default of ten.
	MaxParallelSubCalls int
}

// ProgramFailure is a model-facing runtime failure. Program failures are
// returned as data by the runtime; only host misuse/startup failures use the
// Go error return. Kind is one of the public CodeRuntime failure kinds below.
type ProgramFailure struct {
	Kind    string
	Message string
}

// CodeRuntime failure kinds intentionally match the reference worker
// contract. In particular, timeout/abort/worker-exit are settled outcomes,
// not transport errors, so callers can persist and replay them uniformly.
const (
	ProgramFailureException     = "exception"
	ProgramFailureTimeout       = "timeout"
	ProgramFailureAbort         = "abort"
	ProgramFailureWorkerExit    = "worker-exit"
	ProgramFailureInvalidOutput = "invalid-output"
	ProgramFailureOutputLimit   = "output-limit"
)

// ProgramResult is the DSH CodeRuntime result: ordered captured logs, an
// optional lossless completion value, and an orthogonal failure field.
type ProgramResult struct {
	Value    any
	HasValue bool
	Logs     []string
	// AdditionalContextMessages are deferred by nested bindings and exposed to
	// the outer tool result in dispatch order. They are deliberately kept out
	// of the JSON value returned to the TypeScript program.
	AdditionalContextMessages []llm.Message
	ConcludesTurn             bool
	Failure                   *ProgramFailure
	Truncated                 bool
	Duration                  time.Duration
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
// SandboxCapabilities reports backend guarantees. It is optional for
// compatibility; explicit isolation requirements fail closed if absent.
type SandboxCapabilities struct {
	Available        bool
	Backend          string         `json:"backend,omitempty"`
	IsolationLevel   IsolationLevel `json:"isolationLevel,omitempty"`
	StrongIsolation  bool
	NetworkIsolation bool
	// SupportedModes is optional for old providers. A non-empty list is an
	// allow-list; an omitted list retains the historical provider contract.
	SupportedModes []SandboxMode
}

type capabilityReporter interface {
	Capabilities() SandboxCapabilities
}

// SandboxDiagnostic is the stable audit-facing answer to "what is active?".
// It deliberately distinguishes backend identity from security level so a
// containment backend cannot be mistaken for strong OS isolation.
type SandboxDiagnostic struct {
	Available        bool           `json:"available"`
	Backend          string         `json:"backend"`
	IsolationLevel   IsolationLevel `json:"isolationLevel"`
	StrongIsolation  bool           `json:"strongIsolation"`
	NetworkIsolation bool           `json:"networkIsolation"`
	Reason           string         `json:"reason,omitempty"`
	Summary          string         `json:"summary"`
}

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
	ErrInvalidRequest     = errors.New("code: invalid request")
	ErrUnsupportedLang    = errors.New("code: unsupported language")
	ErrEngineClosed       = errors.New("code: engine closed")
	ErrProviderClosed     = errors.New("code: provider closed")
	ErrSandboxUnavailable = errors.New("SANDBOX_UNAVAILABLE")
)
