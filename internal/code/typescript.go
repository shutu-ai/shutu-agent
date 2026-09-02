package code

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultProgramCompute = 60 * time.Second
	defaultProgramMaxWall = 10 * time.Minute
	defaultProgramOutput  = 64 * 1024 * 1024
	defaultProgramHeapMB  = 512
	// A Node program has no legitimate need for a process fan-out tree. The
	// permission model denies child-process APIs; this Job Object limit is the
	// kernel-level backstop if a hostile escape or runtime regression occurs.
	defaultProgramMaxProcesses = 16
	maxProgramLine             = 16 * 1024 * 1024
	maxNodeTimerMS             = 2_147_483_647
)

// TypeScriptRuntime is the Shutu host implementation of DSH's CodeRuntime.
// Each run is an isolated Node process executing the supplied code as the body
// of an async function. Tool calls travel over a newline-delimited JSON bridge
// to the host binding; the runtime never imports or knows about the registry.
type TypeScriptRuntime struct {
	mu        sync.Mutex
	closed    bool
	active    map[*exec.Cmd]activeProgram
	closeDone chan struct{}
}

// activeProgram keeps the process-tree stop operation beside the run's
// quiescence signal. Killing only exec.Cmd.Process is insufficient: a
// descendant can retain stdout/stderr and keep RunProgram blocked in its
// scanner even after the direct Node process has exited.
type activeProgram struct {
	done chan struct{}
	stop func()
}

// NewTypeScriptRuntime creates the native TypeScript Code Mode runtime. Node is
// resolved at execution time so construction remains cheap and testable.
func NewTypeScriptRuntime() *TypeScriptRuntime {
	return &TypeScriptRuntime{active: map[*exec.Cmd]activeProgram{}, closeDone: make(chan struct{})}
}

func (r *TypeScriptRuntime) RunProgram(ctx context.Context, req ProgramRequest) (ProgramResult, error) {
	if err := ctx.Err(); err != nil {
		return ProgramResult{}, err
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ProgramResult{}, ErrEngineClosed
	}
	if strings.TrimSpace(req.Code) == "" {
		return ProgramResult{}, fmt.Errorf("code runtime: empty program")
	}
	if req.Binding == nil {
		return ProgramResult{}, fmt.Errorf("code runtime: binding callback is required")
	}
	namespaces, err := normalizeBindingNamespaces(req.Bindings)
	if err != nil {
		return ProgramResult{}, err
	}
	computeMS := req.ComputeMS
	if computeMS <= 0 {
		computeMS = int(defaultProgramCompute / time.Millisecond)
	}
	if computeMS <= 0 {
		return ProgramResult{}, fmt.Errorf("code runtime: computeMs must be positive")
	}
	maxWallMS := req.MaxWallMS
	if maxWallMS <= 0 {
		if req.Timeout > 0 {
			maxWallMS = int(req.Timeout / time.Millisecond)
		} else {
			maxWallMS = int(defaultProgramMaxWall / time.Millisecond)
		}
	}
	if maxWallMS <= 0 || maxWallMS > maxNodeTimerMS {
		return ProgramResult{}, fmt.Errorf("code runtime: maxWallMs must be a positive integer at most %d", maxNodeTimerMS)
	}
	maxOutput := req.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultProgramOutput
	}
	if maxOutput < 4 {
		return ProgramResult{}, fmt.Errorf("code runtime: maxOutputBytes must be at least 4")
	}
	if req.MaxOldGenerationSizeMB < 0 {
		return ProgramResult{}, fmt.Errorf("code runtime: max old generation size must not be negative")
	}
	heapMB := req.MaxOldGenerationSizeMB
	if heapMB == 0 {
		heapMB = defaultProgramHeapMB
	}
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ProgramResult{}, fmt.Errorf("code runtime: resolve cwd: %w", err)
		}
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: create cwd %s: %w", cwd, err)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: node executable not found: %w", err)
	}
	if err := requireNodePermissionModel(node); err != nil {
		return ProgramResult{}, err
	}
	script, err := os.CreateTemp(cwd, ".shutu-ptc-*.ts")
	if err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: create program: %w", err)
	}
	scriptPath := script.Name()
	defer os.Remove(scriptPath)
	if _, err := script.WriteString(renderTypeScriptProgram(req.Code, namespaces)); err != nil {
		script.Close()
		return ProgramResult{}, fmt.Errorf("code runtime: write program: %w", err)
	}
	if err := script.Close(); err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: close program: %w", err)
	}

	maxWall := time.Duration(maxWallMS) * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, maxWall)
	defer cancel()
	// Do not tie the OS process directly to runCtx: settlement must first let
	// host dispatches deliver awaited results and explicit queued abandonment.
	// killProcess is the bounded post-drain OS shutdown boundary; runCtx remains
	// the binding/call-scheduling cancellation authority.
	commandCtx, killProcess := context.WithCancel(ctx)
	defer killProcess()
	// Code Mode is allowed to call host tools through the explicit binding, but
	// the model-authored program itself must not become an ambient filesystem,
	// network, child-process, or worker escape hatch. Node's permission model is
	// the enforcing backend here; only the generated program file is readable.
	nodeArgs := []string{"--permission", "--allow-fs-read=" + scriptPath, "--experimental-strip-types"}
	if heapMB > 0 {
		nodeArgs = append(nodeArgs, fmt.Sprintf("--max-old-space-size=%d", heapMB))
	}
	nodeArgs = append(nodeArgs, scriptPath)
	cmd := exec.CommandContext(commandCtx, node, nodeArgs...)
	cmd.Dir = cwd
	// Code Mode is a separate runtime boundary. The reference worker receives
	// an explicitly empty environment; inheriting even non-credential host
	// variables would make program behavior deployment-dependent and could leak
	// loader/proxy/configuration details. The shell sandbox keeps its own
	// defensive scrubbed environment, but TypeScript gets no ambient env at all
	// (Windows may add its SYSTEMROOT process prerequisite at CreateProcess).
	cmd.Env = []string{}
	prepareProcessTree(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ProgramResult{}, fmt.Errorf("code runtime: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return ProgramResult{}, fmt.Errorf("code runtime: stdout: %w", err)
	}
	// Node diagnostics are not part of the framed stdout protocol, but they are
	// still attacker-controlled program output. Keep the parser/error preview
	// bounded just like the model-facing log/completion ledger.
	stderr := &boundedCapture{limit: maxOutput}
	cmd.Stderr = stderr
	// Admit and start under the same mutex as Close. This closes the small
	// startup race where Close could observe a command before it was tracked,
	// return, and then let the command start after teardown had supposedly
	// completed.
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		stdin.Close()
		return ProgramResult{}, ErrEngineClosed
	}
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		stdin.Close()
		// Cancellation may win the tiny admission/start race. Once the
		// runtime has passed host setup, expose the same resolved abort
		// contract as a process that started and was then cancelled.
		if ctx.Err() != nil {
			return ProgramResult{Failure: &ProgramFailure{Kind: ProgramFailureAbort, Message: "program aborted"}}, nil
		}
		return ProgramResult{}, fmt.Errorf("code runtime: start: %w", err)
	}
	processTree, err := attachProcessTree(cmd, processTreeLimits{
		// The polling monitor preserves the reference worker's failure
		// classification. The Windows Job Object is the kernel-level backstop
		// that also covers the process tree if polling is delayed or a child is
		// introduced before shutdown.
		perProcessCPU: time.Duration(computeMS) * time.Millisecond,
		maxProcesses:  defaultProgramMaxProcesses,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		r.mu.Unlock()
		stdin.Close()
		return ProgramResult{}, fmt.Errorf("code runtime: attach process tree: %w", err)
	}
	done := make(chan struct{})
	var treeOnce sync.Once
	closeTree := func() { treeOnce.Do(func() { _ = processTree.Close() }) }
	r.active[cmd] = activeProgram{done: done, stop: func() {
		// Close() must also release host bindings. Killing the child alone leaves
		// callsWG waiting forever when a binding honors the runtime context.
		cancel()
		closeTree()
	}}
	r.mu.Unlock()
	defer r.untrack(cmd, done)
	// Stop the owned tree as soon as the run is cancelled. This is deliberately
	// independent of cmd.Wait/scanner: descendants may inherit the protocol
	// pipes and otherwise keep both waits alive forever.
	treeStop := make(chan struct{})
	// Process-tree shutdown waits briefly for host dispatch goroutines to send
	// their terminal replies. Without this, settlement can kill the worker
	// before fire-and-forget and queued promises observe awaited results,
	// aborts, or explicit abandonment—turning deterministic lifecycle semantics
	// into an OS scheduling race.
	callsSettled := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			select {
			case <-callsSettled:
			case <-time.After(2 * time.Second):
			}
			killProcess()
		case <-treeStop:
		}
	}()
	go func() {
		select {
		case <-runCtx.Done():
			select {
			case <-callsSettled:
			case <-time.After(2 * time.Second):
			}
			closeTree()
		case <-treeStop:
		}
	}()
	defer close(treeStop)
	defer closeTree()
	// Poll the child CPU clock independently of wall time. A synchronous hot
	// loop must not be able to hide behind the host-binding bridge; waiting for
	// a pending binding, on the other hand, does not consume this budget.
	computeExpired := atomic.Bool{}
	computeAccountingLost := atomic.Bool{}
	var settled atomic.Bool
	monitorStop := make(chan struct{})
	var monitorWG sync.WaitGroup
	initialCPU, cpuErr := processCPUTime(cmd.Process.Pid)
	if cpuErr != nil {
		// A requested compute ceiling must never degrade into an unenforced
		// best-effort hint on an unsupported host. Tear down the worker and
		// fail closed; callers can choose a backend with real CPU accounting.
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ProgramResult{}, fmt.Errorf("code runtime: compute accounting unavailable: %w", cpuErr)
	}
	monitorWG.Add(1)
	go func() {
		defer monitorWG.Done()
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorStop:
				return
			case <-ticker.C:
				current, err := processCPUTime(cmd.Process.Pid)
				if err != nil {
					// A completed worker may disappear before the scanner reaches
					// its durable `done` frame (and the parent context is also
					// cancelled when that frame is handled).  Losing the OS
					// accounting handle in that settled/cancelled window is not a
					// compute-policy violation; classifying it as worker-exit
					// makes short successful programs flaky on Windows.
					if settled.Load() || runCtx.Err() != nil {
						return
					}
					computeAccountingLost.Store(true)
					cancel()
					return
				}
				if current-initialCPU >= time.Duration(computeMS)*time.Millisecond {
					computeExpired.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		close(monitorStop)
		monitorWG.Wait()
	}()

	started := time.Now()
	result := ProgramResult{}
	completed := false
	budget := newProgramOutputBudget(maxOutput)
	setOutputLimit := func() {
		message := outputLimitDiagnostic(maxOutput)
		result.Logs = fitProgramLogsForDiagnostic(result.Logs, maxOutput, message)
		result.Truncated = true
		result.Failure = &ProgramFailure{Kind: ProgramFailureOutputLimit, Message: message}
	}
	callGate := newProgramCallGate(req.MaxParallelSubCalls)
	encoderMu := sync.Mutex{}
	var callsWG sync.WaitGroup
	var bridgeErrMu sync.Mutex
	var bridgeErr error
	var nextCallIndex int64
	var firstCallStarted chan struct{}
	setBridgeErr := func(err error) {
		if err == nil || settled.Load() {
			return
		}
		bridgeErrMu.Lock()
		if bridgeErr == nil {
			bridgeErr = err
		}
		bridgeErrMu.Unlock()
		cancel()
	}
	writeReply := func(id int64, name string, value any, callErr error) error {
		message := programReply{Type: "reply", ID: id, ToolName: name, OK: callErr == nil}
		if callErr != nil {
			message.Message = callErr.Error()
		} else {
			// Rich host results carry a model-visible JSON value plus durable
			// content/context projection. Only the value crosses into the
			// JavaScript promise; the host retains the other fields for the
			// outer code-dispatch event.
			if rich, ok := value.(ProgramBindingResult); ok {
				message.Value = rich.Value
			} else if rich, ok := value.(*ProgramBindingResult); ok && rich != nil {
				message.Value = rich.Value
			} else {
				message.Value = value
			}
			if !isProgramJSONValue(message.Value) {
				message.OK = false
				message.Value = nil
				message.Message = "binding resolution must be lossless JSON"
			}
		}
		encoderMu.Lock()
		defer encoderMu.Unlock()
		return json.NewEncoder(stdin).Encode(message)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxProgramLine)
	answered := make(map[int64]struct{})
	for scanner.Scan() {
		var message programMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			// The reference worker treats hostile or malformed peer frames as
			// ignorable input. Do not turn a junk frame into a new public
			// failure kind; if the peer never settles, the process-exit path
			// below reports worker-exit.
			continue
		}
		// A terminal result owns settlement. Continue only long enough to
		// drain bounded worker-side abandonment logs before process exit;
		// a hostile peer cannot inject a second done/call after that point.
		if completed || result.Failure != nil {
			if message.Type == "log" {
				if !budget.append(&result.Logs, message.Text) {
					killProcess()
				}
			}
			continue
		}
		switch message.Type {
		case "log":
			if !budget.append(&result.Logs, message.Text) {
				setOutputLimit()
				cancel()
			}
		case "call":
			// The child process runs model-authored code and may forge duplicate
			// frames. Admit each correlation id at most once so a repeated frame
			// cannot repeat a host tool's side effect even though the JavaScript
			// pending map would ignore the second reply.
			if _, duplicate := answered[message.ID]; duplicate {
				continue
			}
			answered[message.ID] = struct{}{}
			if strings.TrimSpace(message.Name) == "" {
				if err := writeReply(message.ID, message.Name, nil, errors.New("tool name is required")); err != nil {
					result.Failure = &ProgramFailure{Kind: ProgramFailureWorkerExit, Message: "worker reply failed: " + err.Error()}
					cancel()
					break
				}
				continue
			}
			var args any
			if len(message.Args) == 0 || string(message.Args) == "null" {
				args = map[string]any{}
			} else if err := json.Unmarshal(message.Args, &args); err != nil {
				if err := writeReply(message.ID, message.Name, nil, fmt.Errorf("invalid tool arguments: %w", err)); err != nil {
					result.Failure = &ProgramFailure{Kind: ProgramFailureWorkerExit, Message: "worker reply failed: " + err.Error()}
					cancel()
				}
				continue
			}
			callID := fmt.Sprintf("code:%d", message.ID)
			if req.ParentCallID != "" {
				callID = fmt.Sprintf("%s:code:%d", req.ParentCallID, message.ID)
			}
			callsWG.Add(1)
			callID, callNamespace, callName, callArgs := callID, message.Namespace, message.Name, args
			callNumber := message.ID
			nextCallIndex++
			callIndex := nextCallIndex - 1
			started := make(chan struct{})
			if firstCallStarted == nil {
				firstCallStarted = started
			}
			go func() {
				defer callsWG.Done()
				safe := true // standalone callers historically allowed overlap
				if req.IsConcurrencySafe != nil {
					safe = safeCallClassification(req.IsConcurrencySafe, callName, callArgs)
				}
				if err := callGate.acquire(runCtx, callIndex, safe); err != nil {
					close(started)
					// A queued-unstarted dispatch must settle its worker-side
					// promise when the enclosing run terminates. This reply is
					// best-effort because the hostile/crashed peer may already
					// be gone; the enclosing terminal result remains owned by
					// timeout/abort/exception settlement, not by this cleanup.
					_ = writeReply(callNumber, callName, nil, errors.New(
						"run_code run is over (run_code settled); "+callName+" tool call abandoned",
					))
					return
				}
				defer callGate.release(safe)
				// The child can emit `done` immediately after a fire-and-forget
				// call. Admit the first call before honoring that completion so
				// cancellation drains a real host dispatch rather than silently
				// dropping it at the gate.
				close(started)
				value, callErr := req.Binding(runCtx, ProgramBindingRequest{CallID: callID, Namespace: callNamespace, Name: callName, Args: callArgs})
				if err := writeReply(callNumber, callName, value, callErr); err != nil {
					setBridgeErr(fmt.Errorf("write tool reply: %w", err))
				}
			}()
		case "done":
			if message.Error != nil {
				if !isProgramFailureKind(message.Error.Kind) || message.Error.Message == "" {
					// A malformed completion frame is ignored by the reference
					// hostile-peer parser. It must not be mistaken for a
					// successful settlement here.
					continue
				}
				if !budget.consume(jsonStringBytes(message.Error.Message)) {
					setOutputLimit()
				} else {
					result.Failure = &ProgramFailure{Kind: message.Error.Kind, Message: message.Error.Message}
				}
			} else if len(message.Value) > 0 {
				// Completion values share the same outer-output ledger as logs.
				// Without this check a program could bypass MaxOutput simply by
				// returning a giant object instead of printing it.
				if !budget.consume(len(message.Value)) {
					setOutputLimit()
				} else if err := json.Unmarshal(message.Value, &result.Value); err != nil {
					result.Failure = &ProgramFailure{Kind: ProgramFailureInvalidOutput, Message: "program completion is not valid JSON: " + err.Error()}
				} else {
					result.HasValue = true
				}
			} else if !budget.consume(0) {
				setOutputLimit()
			}
			completed = true
			settled.Store(true)
			// Let the first announced dispatch cross the admission gate before
			// canceling the enclosing context. This preserves the reference
			// fire-and-forget contract while still allowing later queued calls to
			// be abandoned and drained.
			if firstCallStarted != nil {
				select {
				case <-firstCallStarted:
				case <-time.After(time.Second):
					// A wedged classifier/gate must not make completion hang forever.
				}
			}
			// The program has settled. Stop the child process and cancel host
			// bindings that were fired without being awaited, then drain their
			// goroutines below. This mirrors the reference bridge's guarantee that
			// no sub-dispatch survives the enclosing run.
			cancel()
		default:
			// Unknown frames are an extension point and are intentionally
			// ignored, matching the reference hostile-peer parser.
			continue
		}
	}
	// Scanner stops consuming stdout when a hostile worker emits an over-sized
	// frame. Cancel first, then drain the remaining pipe so the child cannot
	// remain blocked on a full OS pipe while we wait for its process tree.
	scannerErr := scanner.Err()
	if scannerErr != nil {
		cancel()
		_, _ = io.Copy(io.Discard, stdout)
		// A settled worker is deliberately cancelled immediately after its done
		// frame. On some hosts the scanner observes the resulting closed pipe
		// before the process wait completes; that is a normal post-settlement
		// shutdown race, not a worker-exit failure.
		if result.Failure == nil && !completed {
			result.Failure = &ProgramFailure{Kind: ProgramFailureWorkerExit, Message: "worker emitted an oversized or malformed protocol frame"}
		}
	}
	callsWG.Wait()
	close(callsSettled)
	bridgeErrMu.Lock()
	if result.Failure == nil && bridgeErr != nil {
		result.Failure = &ProgramFailure{Kind: ProgramFailureWorkerExit, Message: bridgeErr.Error()}
	}
	bridgeErrMu.Unlock()
	_ = stdin.Close()
	waitErr := cmd.Wait()
	result.Duration = time.Since(started)
	if computeExpired.Load() {
		result.Failure = &ProgramFailure{Kind: ProgramFailureTimeout, Message: fmt.Sprintf("compute budget exhausted (%dms busy)", computeMS)}
	}
	// When the wall timer kills the worker before it can emit its `done` frame,
	// the scanner/Wait path may have already classified the abrupt EOF as
	// worker-exit. The enclosing request deadline is authoritative for this
	// outcome; otherwise the same program races between timeout and worker-exit
	// depending on host scheduling (especially under the full test suite).
	if runCtx.Err() == context.DeadlineExceeded && (result.Failure == nil || result.Failure.Kind == ProgramFailureWorkerExit) {
		result.Failure = &ProgramFailure{Kind: ProgramFailureTimeout, Message: fmt.Sprintf("wall-clock ceiling reached (%dms; compute budget %dms)", maxWallMS, computeMS)}
	}
	if result.Failure != nil {
		return result, nil
	}
	// A scanner framing error is a resolved hostile-worker outcome after the
	// cancellation/drain above, not a transport failure that can strand the
	// child process or force callers to retry an already-started program.
	if runCtx.Err() == context.DeadlineExceeded {
		result.Failure = &ProgramFailure{Kind: ProgramFailureTimeout, Message: fmt.Sprintf("wall-clock ceiling reached (%dms; compute budget %dms)", maxWallMS, computeMS)}
		return result, nil
	}
	if ctx.Err() != nil {
		// Code Runtime reports program outcomes as a resolved result. An
		// external cancellation is therefore a typed abort, not a transport
		// error; callers can distinguish it from a host setup failure.
		result.Failure = &ProgramFailure{Kind: ProgramFailureAbort, Message: "program aborted"}
		return result, nil
	}
	if completed {
		return result, nil
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		kind := ProgramFailureWorkerExit
		// Node reports strip/parser failures only on stderr because the wrapper
		// cannot reach its catch block. They are still program-authored failures
		// in the reference contract, not transport deaths.
		if isProgramSyntaxFailure(message) {
			kind = ProgramFailureException
		}
		result.Failure = &ProgramFailure{Kind: kind, Message: message}
		return result, nil
	}
	result.Failure = &ProgramFailure{Kind: ProgramFailureWorkerExit, Message: "runtime exited without a completion message"}
	return result, nil
}

func isProgramSyntaxFailure(message string) bool {
	return strings.Contains(message, "ERR_INVALID_TYPESCRIPT_SYNTAX") ||
		strings.Contains(message, "ERR_UNSUPPORTED_TYPESCRIPT_SYNTAX") ||
		strings.Contains(message, "SyntaxError")
}

func requireNodePermissionModel(node string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--help").Output()
	if err != nil {
		return fmt.Errorf("code runtime: cannot verify Node permission model: %w", err)
	}
	if !bytes.Contains(out, []byte("--permission")) {
		return fmt.Errorf("code runtime: Node permission model is unavailable")
	}
	return nil
}

func isProgramFailureKind(kind string) bool {
	switch kind {
	case ProgramFailureException, ProgramFailureTimeout, ProgramFailureAbort,
		ProgramFailureWorkerExit, ProgramFailureInvalidOutput, ProgramFailureOutputLimit:
		return true
	default:
		return false
	}
}

const defaultMaxParallelSubCalls = 10

// programCallGate preserves submission order while allowing a consecutive
// group of classified-safe calls to overlap. An exclusive call waits until
// every active safe call settles; once it starts, later safe calls wait behind
// it. This is deliberately a small per-program gate: cross-program isolation
// belongs to the host Registry/Agent scope, not this runtime.
type programCallGate struct {
	mu         sync.Mutex
	next       int64
	active     int
	activeSafe int
	exclusive  bool
	max        int
	changed    chan struct{}
}

func newProgramCallGate(max int) *programCallGate {
	if max <= 0 {
		max = defaultMaxParallelSubCalls
	}
	return &programCallGate{max: max, changed: make(chan struct{})}
}

func (g *programCallGate) acquire(ctx context.Context, index int64, safe bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		ready := index == g.next && ((safe && !g.exclusive && g.activeSafe < g.max) || (!safe && g.active == 0))
		if ready {
			g.next++
			g.active++
			if safe {
				g.activeSafe++
			} else {
				g.exclusive = true
			}
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (g *programCallGate) release(safe bool) {
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	if safe && g.activeSafe > 0 {
		g.activeSafe--
	}
	if !safe {
		g.exclusive = false
	}
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

func safeCallClassification(classifier func(string, any) bool, name string, args any) (safe bool) {
	defer func() {
		if recover() != nil {
			safe = false
		}
	}()
	return classifier(name, args)
}

func (r *TypeScriptRuntime) untrack(cmd *exec.Cmd, done chan struct{}) {
	r.mu.Lock()
	delete(r.active, cmd)
	close(done)
	r.mu.Unlock()
}

// Close stops all in-flight program processes and prevents future runs.
func (r *TypeScriptRuntime) Close() error {
	r.mu.Lock()
	if r.closed {
		closeDone := r.closeDone
		r.mu.Unlock()
		<-closeDone
		return nil
	}
	r.closed = true
	active := make([]struct {
		cmd *exec.Cmd
		run activeProgram
	}, 0, len(r.active))
	for cmd, run := range r.active {
		active = append(active, struct {
			cmd *exec.Cmd
			run activeProgram
		}{cmd: cmd, run: run})
	}
	r.mu.Unlock()
	for _, entry := range active {
		entry.run.stop()
		if entry.cmd.Process != nil {
			_ = entry.cmd.Process.Kill()
		}
	}
	for _, entry := range active {
		<-entry.run.done
	}
	close(r.closeDone)
	return nil
}

type programMessage struct {
	Type      string          `json:"type"`
	ID        int64           `json:"id"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	Text      string          `json:"text"`
	Value     json.RawMessage `json:"value"`
	Error     *programError   `json:"error"`
}

type programReply struct {
	Type     string `json:"type"`
	ID       int64  `json:"id"`
	ToolName string `json:"toolName,omitempty"`
	OK       bool   `json:"ok"`
	// Do not omit nil: a successful Go nil is the JSON null value at the
	// JavaScript boundary, not an omitted/undefined completion.
	Value   any    `json:"value"`
	Message string `json:"message,omitempty"`
}

// isProgramJSONValue is the host-side half of the Code Runtime lossless JSON
// boundary. json.Marshal alone is insufficient here because it silently
// converts structs (for example time.Time), []byte, and some integer values
// into a different JavaScript value. Tool binding failures must remain
// catchable program errors rather than becoming worker-exit transport errors.
func isProgramJSONValue(value any) bool {
	return isProgramJSONReflect(reflect.ValueOf(value), make(map[uintptr]bool))
}

func isProgramJSONReflect(value reflect.Value, active map[uintptr]bool) bool {
	if !value.IsValid() {
		return true
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return true
		}
		return isProgramJSONReflect(value.Elem(), active)
	}
	switch value.Kind() {
	case reflect.Bool, reflect.String:
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		const maxSafe = int64(1<<53 - 1)
		n := value.Int()
		return n >= -maxSafe && n <= maxSafe
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		const maxSafe = uint64(1<<53 - 1)
		return value.Uint() <= maxSafe
	case reflect.Float32, reflect.Float64:
		n := value.Float()
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return false
		}
		return n != 0 || !math.Signbit(n)
	case reflect.Slice, reflect.Array:
		// encoding/json encodes []byte as a base64 string, which is not the
		// plain JSON array the worker contract describes.
		if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		if value.Kind() == reflect.Slice {
			pointer := value.Pointer()
			if pointer != 0 {
				if active[pointer] {
					return false
				}
				active[pointer] = true
				defer delete(active, pointer)
			}
		}
		for index := 0; index < value.Len(); index++ {
			if !isProgramJSONReflect(value.Index(index), active) {
				return false
			}
		}
		return true
	case reflect.Map:
		if value.IsNil() || value.Type().Key().Kind() != reflect.String {
			return value.IsNil()
		}
		pointer := value.Pointer()
		if pointer != 0 {
			if active[pointer] {
				return false
			}
			active[pointer] = true
			defer delete(active, pointer)
		}
		iter := value.MapRange()
		for iter.Next() {
			if !isProgramJSONReflect(iter.Value(), active) {
				return false
			}
		}
		return true
	default:
		// Pointers, structs, functions, channels, and unsafe values are not
		// plain JSON even when encoding/json happens to have a representation.
		return false
	}
}

type programError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type programOutputBudget struct {
	limit   int
	used    int
	entries int
}

func newProgramOutputBudget(limit int) programOutputBudget {
	// The reference ledger starts with the serialized empty log array ([]).
	return programOutputBudget{limit: limit, used: 2}
}

func (b *programOutputBudget) consume(size int) bool {
	if size < 0 || b.used > b.limit || size > b.limit-b.used {
		return false
	}
	b.used += size
	return true
}

func (b *programOutputBudget) append(logs *[]string, text string) bool {
	separator := 0
	if b.entries > 0 {
		separator = 1
	}
	remaining := b.limit - b.used - separator
	if remaining <= 0 {
		return false
	}
	if encoded := jsonStringBytes(text); encoded <= remaining {
		*logs = append(*logs, text)
		b.used += separator + encoded
		b.entries++
		return true
	}
	prefix := truncateJSONText(text, remaining)
	if prefix == "" {
		return false
	}
	*logs = append(*logs, prefix)
	b.used += separator + jsonStringBytes(prefix)
	b.entries++
	return false
}

func jsonStringBytes(value string) int {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 0
	}
	return encoded.Len() - 1 // Encoder adds one newline; JSON.stringify does not.
}

func truncateJSONText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	low, high := 0, len(runes)
	for low < high {
		middle := low + (high-low+1)/2
		if jsonStringBytes(string(runes[:middle])) <= limit {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return string(runes[:low])
}

func outputLimitDiagnostic(limit int) string {
	message := fmt.Sprintf("outer output exceeded %d bytes", limit)
	return truncateJSONText(message, limit-2)
}

// fitProgramLogsForDiagnostic keeps the public result within the same
// serialized budget after an overflow diagnostic is selected. The worker
// reference reserves space for that diagnostic and may trim the final log
// entry; retaining a full log prefix while adding an unbounded failure text
// would only be a superficial quota implementation.
func fitProgramLogsForDiagnostic(logs []string, limit int, diagnostic string) []string {
	available := limit - jsonStringBytes(diagnostic)
	if available < 2 {
		return nil
	}
	used := 2 // serialized empty logs array: []
	retained := make([]string, 0, len(logs))
	for _, text := range logs {
		separator := 0
		if len(retained) > 0 {
			separator = 1
		}
		remaining := available - used - separator
		if remaining < 2 {
			break
		}
		encoded := jsonStringBytes(text)
		if encoded <= remaining {
			retained = append(retained, text)
			used += separator + encoded
			continue
		}
		prefix := truncateJSONText(text, remaining)
		if prefix == "" {
			break
		}
		retained = append(retained, prefix)
		break
	}
	return retained
}

var portableBindingGlobal = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedBindingGlobals = map[string]struct{}{
	// The portable identifier contract is the union of ECMAScript and Python
	// reserved words. This is deliberately shared even though this backend is
	// TypeScript-only today: a namespace accepted here must remain usable if a
	// caller routes the same declaration to the Python backend.
	"await": {}, "break": {}, "case": {}, "catch": {}, "class": {}, "const": {}, "continue": {},
	"debugger": {}, "default": {}, "delete": {}, "do": {}, "else": {}, "enum": {}, "export": {},
	"extends": {}, "false": {}, "finally": {}, "for": {}, "function": {}, "if": {}, "implements": {},
	"import": {}, "in": {}, "instanceof": {}, "interface": {}, "let": {}, "new": {}, "null": {},
	"package": {}, "private": {}, "protected": {}, "public": {}, "return": {}, "static": {},
	"super": {}, "switch": {}, "this": {}, "throw": {}, "true": {}, "try": {}, "typeof": {},
	"var": {}, "void": {}, "while": {}, "with": {}, "yield": {}, "arguments": {}, "eval": {},
	"False": {}, "None": {}, "True": {}, "and": {}, "as": {}, "assert": {}, "async": {}, "def": {},
	"del": {}, "elif": {}, "except": {}, "from": {}, "global": {}, "is": {}, "lambda": {},
	"match": {}, "nonlocal": {}, "not": {}, "or": {}, "pass": {}, "raise": {}, "type": {}, "_": {},
	// Names owned by the generated substrate or by Node must not be shadowed.
	"console": {}, "globalThis": {}, "process": {}, "require": {}, "__dirname": {}, "__filename": {},
	"__dsh_main__": {}, "__builtins__": {}, "__name__": {}, "__debug__": {}, "__shutuProgram": {},
	"emit": {}, "pending": {}, "input": {}, "rawStdoutWrite": {}, "rawStderrWrite": {},
}

var reservedBindingErrorMembers = map[string]struct{}{
	"name": {}, "message": {}, "stack": {},
	// Python exception protocol members are reserved for cross-backend parity.
	"args": {}, "with_traceback": {}, "add_note": {},
}

func normalizeBindingNamespaces(input []ProgramBindingNamespace) ([]ProgramBindingNamespace, error) {
	if len(input) == 0 {
		return []ProgramBindingNamespace{{Global: "tools"}}, nil
	}
	seen := make(map[string]struct{}, len(input))
	out := make([]ProgramBindingNamespace, 0, len(input))
	for _, namespace := range input {
		global := strings.TrimSpace(namespace.Global)
		if !portableBindingGlobal.MatchString(global) {
			return nil, fmt.Errorf("code runtime: invalid binding namespace %q", namespace.Global)
		}
		if _, reserved := reservedBindingGlobals[global]; reserved {
			return nil, fmt.Errorf("code runtime: reserved binding namespace %q", global)
		}
		if _, duplicate := seen[global]; duplicate {
			return nil, fmt.Errorf("code runtime: duplicate binding namespace %q", global)
		}
		seen[global] = struct{}{}
		errorClass := strings.TrimSpace(namespace.ErrorClassName)
		memberProperty := strings.TrimSpace(namespace.ErrorMemberNameProperty)
		if errorClass != "" {
			if !portableBindingGlobal.MatchString(errorClass) {
				return nil, fmt.Errorf("code runtime: invalid binding error class %q", namespace.ErrorClassName)
			}
			if _, reserved := reservedBindingGlobals[errorClass]; reserved {
				return nil, fmt.Errorf("code runtime: reserved binding error class %q", errorClass)
			}
			if memberProperty == "" {
				return nil, fmt.Errorf("code runtime: invalid binding error member property %q", namespace.ErrorMemberNameProperty)
			}
			if _, reserved := reservedBindingErrorMembers[memberProperty]; reserved {
				return nil, fmt.Errorf("code runtime: reserved binding error member property %q", memberProperty)
			}
			if len(memberProperty) > 4 && strings.HasPrefix(memberProperty, "__") && strings.HasSuffix(memberProperty, "__") {
				return nil, fmt.Errorf("code runtime: reserved binding error member property %q", memberProperty)
			}
		} else if memberProperty != "" {
			return nil, fmt.Errorf("code runtime: error member property requires an error class")
		}
		out = append(out, ProgramBindingNamespace{Global: global, ErrorClassName: errorClass, ErrorMemberNameProperty: memberProperty})
	}
	return out, nil
}

func renderTypeScriptProgram(program string, namespaces []ProgramBindingNamespace) string {
	var namespaceSource strings.Builder
	for _, namespace := range namespaces {
		if namespace.ErrorClassName == "" {
			fmt.Fprintf(&namespaceSource, "const %s = createBindingNamespace(%s)\n", namespace.Global, strconv.Quote(namespace.Global))
		} else {
			fmt.Fprintf(&namespaceSource, "const %s = createBindingNamespace(%s, createBindingErrorClass(%s, %s))\n", namespace.Global, strconv.Quote(namespace.Global), strconv.Quote(namespace.ErrorClassName), strconv.Quote(namespace.ErrorMemberNameProperty))
		}
	}
	return `import * as readline from 'node:readline'
import { inspect } from 'node:util'

const rawStdoutWrite = process.stdout.write.bind(process.stdout)
const rawStderrWrite = process.stderr.write.bind(process.stderr)
const safeArrayIsArray = Array.isArray
const safeGetPrototypeOf = Object.getPrototypeOf
const safeGetOwnPropertyNames = Object.getOwnPropertyNames
const safeGetOwnPropertySymbols = Object.getOwnPropertySymbols
const safeHasOwn = Object.prototype.hasOwnProperty
const safeObjectPrototype = Object.prototype
const safeNumberIsFinite = Number.isFinite
const safeObjectIs = Object.is
const pending = new Map<number, { resolve: (value: unknown) => void, reject: (error: Error) => void, makeError: (member: string, message: string) => Error }>()
let nextCallId = 0

function emit(message: unknown): void {
  rawStdoutWrite(JSON.stringify(message) + '\n')
}

function render(value: unknown): string {
  return typeof value === 'string' ? value : inspect(value, { depth: 4, maxArrayLength: 100, maxStringLength: 10000 })
}

function log(...values: unknown[]): void {
  emit({ type: 'log', text: values.map(render).join(' ') })
}

const capturedConsole = { log, info: log, warn: log, error: log, debug: log }
globalThis.console = capturedConsole as Console
process.stdout.write = ((chunk: unknown): boolean => { emit({ type: 'log', text: String(chunk) }); return true }) as typeof process.stdout.write
process.stderr.write = ((chunk: unknown): boolean => { emit({ type: 'log', text: String(chunk) }); return true }) as typeof process.stderr.write

class ToolCallError extends Error {
  toolName: string
  constructor(toolName: string, message: string) {
    super(message)
    this.name = 'ToolCallError'
    this.toolName = toolName
  }
}
globalThis.ToolCallError = ToolCallError

// The host boundary is JSON, but JSON.stringify silently changes several
// JavaScript values (Date, -0, sparse arrays, functions, cycles, and objects
// with hidden or exotic properties). Reject those values before they cross the
// boundary so a Code Mode program observes the same lossless-value contract as
// the reference runtime instead of silently receiving a different value.
function isLossless(value: unknown, seen = new Set<object>()): boolean {
  if (value === null || typeof value === 'boolean' || typeof value === 'string') return true
  if (typeof value === 'number') return safeNumberIsFinite(value) && !safeObjectIs(value, -0)
  if (typeof value !== 'object') return false
  if (seen.has(value)) return false
  seen.add(value)
  try {
    if (safeArrayIsArray(value)) {
      const array = value as unknown[]
		const keys = safeGetOwnPropertyNames(value)
		if (safeGetOwnPropertySymbols(value).length > 0 || keys.length !== array.length + 1 || !keys.includes('length')) return false
      for (let index = 0; index < array.length; index++) {
        if (!safeHasOwn.call(value, String(index)) || !isLossless(array[index], seen)) return false
      }
      return true
    }
    const prototype = safeGetPrototypeOf(value)
    if (prototype !== null && prototype !== safeObjectPrototype) return false
    if (safeGetOwnPropertySymbols(value).length > 0) return false
    for (const key of safeGetOwnPropertyNames(value)) {
      if (!safeHasOwn.call(value, key) || !isLossless((value as Record<string, unknown>)[key], seen)) return false
    }
    return true
  } catch {
    return false
  } finally {
    seen.delete(value)
  }
}

const input = readline.createInterface({ input: process.stdin, crlfDelay: Infinity })
input.on('line', (line: string) => {
  try {
    const message = JSON.parse(line) as { type?: string, id?: number, ok?: boolean, value?: unknown, message?: string, toolName?: string }
    if (message.type !== 'reply' || typeof message.id !== 'number') return
    const call = pending.get(message.id)
    if (!call) return
    pending.delete(message.id)
    if (message.ok) call.resolve(message.value)
    else call.reject(call.makeError(message.toolName ?? '', message.message ?? 'tool call failed'))
  } catch {
    // Host protocol errors are surfaced when the pending call times out/aborts.
  }
})

function createBindingErrorClass(name: string, memberNameProperty: string): (member: string, message: string) => Error {
  class BindingError extends Error {
    constructor(member: string, message: string) {
      super(message)
      this.name = name
      Object.defineProperty(this, memberNameProperty, { value: member, enumerable: true, configurable: true })
    }
  }
  return (member: string, message: string) => new BindingError(member, message)
}

function invoke(namespace: string, name: string, args: unknown, errorClass?: (member: string, message: string) => Error): Promise<unknown> {
  const member = namespace + '.' + name
  const makeError = errorClass ?? ((memberName: string, message: string) => new ToolCallError(memberName, message))
  if (!isLossless(args)) return Promise.reject(makeError(name, 'binding arguments must be lossless JSON'))
  const id = ++nextCallId
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject, makeError })
    emit({ type: 'call', id, namespace, name, args: args ?? {} })
  })
}

function createBindingNamespace(namespace: string, errorClass?: (member: string, message: string) => Error): Record<string, (args?: unknown) => Promise<unknown>> {
  return new Proxy(Object.create(null), {
  get(_target: object, property: string | symbol): unknown {
    if (typeof property !== 'string') return undefined
    return (args: unknown = {}) => invoke(namespace, property, args, errorClass)
  },
  }) as Record<string, (args?: unknown) => Promise<unknown>>
}

` + namespaceSource.String() + `
async function __shutuProgram(): Promise<unknown> {
` + program + `
}

try {
  const value = await __shutuProgram()
  // An omitted return is a successful completion without a value. The
  // reference runtime reserves invalid-output for values that are present but
  // cannot cross the lossless JSON boundary; undefined is intentionally
  // omitted from the done frame.
  if (value === undefined || isLossless(value)) emit({ type: 'done', ...value === undefined ? {} : { value } })
  else emit({ type: 'done', error: { kind: 'invalid-output', message: 'program completion must be lossless JSON' } })
} catch (error) {
  const message = error instanceof Error ? error.stack ?? error.message : String(error)
  emit({ type: 'done', error: { kind: 'exception', message } })
}
// The host settles the enclosing run before the process exits. Give replies to
// awaited and fire-and-forget calls one event-loop drain so queued calls receive
// their explicit abandonment instead of an indistinguishable process death. A
// hostile worker cannot extend its wall/Compute budget: the host has already
// armed cancellation and owns the kill boundary.
await new Promise<void>(resolve => {
  const drain = (): void => {
    if (pending.size === 0) setTimeout(resolve, 0)
    else setTimeout(drain, 1)
  }
  drain()
})
await new Promise<void>(resolve => rawStdoutWrite('', () => resolve()))
input.close()
process.stdin.pause()
`
}
