package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Compile-time assertions: the legacy and dsh job tools implement the tool method set
// the composition root boxes into a tools.Registry.
var (
	_ = (*JobStartTool)(nil)
	_ = (*JobStatusTool)(nil)
	_ = (*JobCancelTool)(nil)
	_ = (*JobWaitTool)(nil)
	_ = (*JobReadTool)(nil)
	_ = (*DshJobOutputTool)(nil)
	_ = (*DshJobKillTool)(nil)
	_ = (*DshJobListTool)(nil)
)

// eventLog is a minimal onEvent recorder for tool tests.
type eventLog struct {
	evts []string
}

func (e *eventLog) record(typ string, data any) { e.evts = append(e.evts, typ) }
func (e *eventLog) counts() map[string]int {
	m := map[string]int{}
	for _, t := range e.evts {
		m[t]++
	}
	return m
}

// delayedGateRun is gateRun with a settle delay after ctx cancellation, giving
// an observer a deterministic window to see the "stopping" state before the job
// settles killed.
func delayedGateRun(gate <-chan struct{}, delay time.Duration) func(ctx context.Context) (JobOutcome, error) {
	return func(ctx context.Context) (JobOutcome, error) {
		select {
		case <-gate:
			return JobOutcome{Status: StatusCompleted, Detail: "done"}, nil
		case <-ctx.Done():
			time.Sleep(delay)
			return JobOutcome{Status: StatusKilled, Detail: "cancelled"}, nil
		}
	}
}

// TestJobStartExecutesRealCommand verifies job_start runs a real external
// command in the background (os/exec), returns a registry-issued id, and that
// job_wait settles it and job_read returns the captured output.
func TestJobStartExecutesRealCommand(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	log := &eventLog{}
	jt := NewJobTools(l, func() string { return "sess-1" }, log.record)

	out, err := jt.Start().Execute(context.Background(), json.RawMessage(`{"command":"echo hello-job"}`))
	if err != nil {
		t.Fatalf("job_start: %v", err)
	}
	if !strings.Contains(out, "started job ") || !strings.HasPrefix(out, "started job bash-") {
		t.Fatalf("job_start output = %q, want started job bash-...", out)
	}

	waitOut, err := jt.Wait().Execute(context.Background(), json.RawMessage(`{"id":"bash-1","timeout_seconds":10}`))
	if err != nil {
		t.Fatalf("job_wait: %v", err)
	}
	if !strings.Contains(waitOut, "settled") || !strings.Contains(waitOut, "completed") {
		t.Fatalf("job_wait output = %q, want settled completed", waitOut)
	}
	readOut, err := jt.Read().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_read: %v", err)
	}
	if !strings.Contains(readOut, "hello-job") || !strings.Contains(readOut, "completed") {
		t.Fatalf("job_read output = %q, want captured output + completed status", readOut)
	}
	// The whole lifecycle is logged: job/start then job/done (the echo job is
	// fast, so it may already be terminal before job_wait).
	c := log.counts()
	if c["job/start"] != 1 || c["job/done"] != 1 {
		t.Fatalf("event counts = %v, want exactly one job/start and one job/done", c)
	}
}

// TestJobCancellationClassification pins the cancellation boundary: bounded
// waits return on registry cancellation, while a started background job is
// intentionally owned beyond the starting tool call.
func TestJobCancellationClassification(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-classification" }, nil)
	for _, tool := range []any{jt.Wait(), jt.DshOutput()} {
		classified, ok := tool.(interface{ CancellationAware() bool })
		if !ok || !classified.CancellationAware() {
			t.Fatalf("%T must classify its bounded wait as cancellable", tool)
		}
	}
	if _, ok := any(jt.Start()).(interface{ CancellationAware() bool }); ok {
		t.Fatal("job_start must not claim registry-call cancellation for a background job")
	}
}

func TestJobStartBoundsNoisyOutput(t *testing.T) {
	command := fmt.Sprintf("yes x | head -c %d", defaultJobOutputLimit*4)
	if runtime.GOOS == "windows" {
		command = fmt.Sprintf("powershell -NoProfile -NonInteractive -Command ('x'*%d)", defaultJobOutputLimit*4)
	}
	outcome, err := runCommandLineBounded(command, t.TempDir())(context.Background())
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if outcome.Status != StatusCompleted || !strings.Contains(outcome.Output, "[output truncated]") {
		t.Fatalf("outcome = %+v, want completed bounded output", outcome)
	}
	if len(outcome.Output) > defaultJobOutputLimit+64 {
		t.Fatalf("output length = %d, want bounded near %d", len(outcome.Output), defaultJobOutputLimit)
	}
}

func TestJobStartUsesBoundSessionCWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("portable cwd assertion uses pwd")
	}
	dir := t.TempDir()
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobToolsWithCWD(l, func() string { return "sess-cwd" }, func(context.Context) string { return dir }, nil)
	if _, err := jt.Start().Execute(context.Background(), json.RawMessage(`{"command":"pwd"}`)); err != nil {
		t.Fatalf("job_start: %v", err)
	}
	if _, err := jt.Wait().Execute(context.Background(), json.RawMessage(`{"id":"bash-1","timeout_seconds":5}`)); err != nil {
		t.Fatalf("job_wait: %v", err)
	}
	out, err := jt.Read().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_read: %v", err)
	}
	if !strings.Contains(out, dir) {
		t.Fatalf("job output = %q, want workspace %q", out, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workspace disappeared: %v", err)
	}
}

// TestDshJobProjectionUsesCanonicalArguments verifies the dsh-facing job
// surface reads the same registry state with job_id and returns the status
// marker shape consumed by the shell tools.
func TestDshJobProjectionUsesCanonicalArguments(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-dsh" }, nil)

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "sess-dsh", gate)

	out, err := jt.DshOutput().Execute(context.Background(), json.RawMessage(`{"job_id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_output (running): %v", err)
	}
	if !strings.Contains(out, "[status: running") {
		t.Fatalf("job_output (running) = %q, want running status", out)
	}
	list, err := jt.DshList().Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("job_list: %v", err)
	}
	if !strings.Contains(list, id) {
		t.Fatalf("job_list = %q, want %s", list, id)
	}

	close(gate)
	out, err = jt.DshOutput().Execute(context.Background(), json.RawMessage(`{"job_id":"bash-1","wait":true,"timeout_ms":5000}`))
	if err != nil {
		t.Fatalf("job_output (settled): %v", err)
	}
	if !strings.Contains(out, "[status: completed") {
		t.Fatalf("job_output (settled) = %q, want completed status", out)
	}
}

func TestDshJobOutputReturnsLiveDeltas(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-dsh" }, nil)

	gate := make(chan struct{})
	var liveMu sync.Mutex
	chunks := []string{"", "line one\n", " second", ""}
	id, err := l.Start(context.Background(), JobStart{
		Kind:         "stream",
		Label:        "streaming output",
		OwnerSession: "sess-dsh",
		ReadOutput: func() (string, error) {
			liveMu.Lock()
			defer liveMu.Unlock()
			chunk := chunks[0]
			chunks = chunks[1:]
			return chunk, nil
		},
		Run: func(context.Context) (JobOutcome, error) {
			<-gate
			return JobOutcome{Status: StatusCompleted, Output: "final marker"}, nil
		},
	})
	if err != nil {
		t.Fatalf("start streaming job: %v", err)
	}

	read := func(want string) {
		t.Helper()
		out, err := jt.DshOutput().Execute(context.Background(), json.RawMessage(`{"job_id":"`+id+`"}`))
		if err != nil {
			t.Fatalf("job_output: %v", err)
		}
		if !strings.Contains(out, want) {
			t.Fatalf("job_output = %q, want %q", out, want)
		}
	}
	read("(no new output)")
	read("line one\n[status: running]")
	read(" second")

	close(gate)
	waitForTerminal(t, l, id, "sess-dsh", 5*time.Second)
	read("(no new output)")
}

// TestJobStartDefaults verifies job_start's argument defaults: kind "bash",
// a bounded non-sensitive label, and owner_session = the current session via
// the injected owner callback.
func TestJobStartDefaults(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-9" }, nil)

	if _, err := jt.Start().Execute(context.Background(), json.RawMessage(`{"command":"echo x"}`)); err != nil {
		t.Fatalf("job_start: %v", err)
	}
	snap, err := l.Get(context.Background(), "bash-1", "sess-9")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.Kind != "bash" || snap.Label != "background command" || snap.OwnerSession != "sess-9" {
		t.Fatalf("defaulted snapshot = %+v, want kind=bash label=background command owner=sess-9", snap)
	}
}

func TestJobStartRedactsDurableLabel(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-redact" }, nil)
	if _, err := jt.Start().Execute(context.Background(), json.RawMessage(`{"command":"echo safe","label":"deploy token=super-secret"}`)); err != nil {
		t.Fatalf("job_start: %v", err)
	}
	snap, err := l.Get(context.Background(), "bash-1", "sess-redact")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(snap.Label, "super-secret") || !strings.Contains(snap.Label, "[REDACTED]") {
		t.Fatalf("durable job label = %q, want redacted secret", snap.Label)
	}
}

// TestJobStartRejectsEmptyCommand verifies the tool's own defensive check
// (the registry schema enforces minLength 1, but whitespace-only commands
// would slip through a bare minLength).
func TestJobStartRejectsEmptyCommand(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-1" }, nil)
	if _, err := jt.Start().Execute(context.Background(), json.RawMessage(`{"command":"   "}`)); err == nil {
		t.Fatal("job_start with a blank command must fail")
	}
}

// TestJobStatusAndReadReflectSnapshot verifies job_status and job_read project
// the registry snapshot: running while live, terminal with output once settled.
func TestJobStatusAndReadReflectSnapshot(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-1" }, nil)

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "sess-1", gate)

	statusOut, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_status: %v", err)
	}
	if !strings.Contains(statusOut, "running") || !strings.Contains(statusOut, id) {
		t.Fatalf("job_status output = %q, want running for %s", statusOut, id)
	}
	readOut, err := jt.Read().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_read (live): %v", err)
	}
	if !strings.Contains(readOut, "running") || strings.Contains(readOut, "output:") {
		t.Fatalf("job_read (live) output = %q, want running without output", readOut)
	}

	close(gate)
	waitForTerminal(t, l, id, "sess-1", 5*time.Second)
	statusOut, err = jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_status (terminal): %v", err)
	}
	if !strings.Contains(statusOut, "completed") {
		t.Fatalf("job_status (terminal) output = %q, want completed", statusOut)
	}
}

// TestJobCancelRequestsCancellation verifies job_cancel returns "requested"
// for a live job (which then settles killed) and "already-finished" for a
// terminal one, and is idempotent.
func TestJobCancelRequestsCancellation(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-1" }, nil)

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "sess-1", gate)

	out, err := jt.Cancel().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_cancel: %v", err)
	}
	if out != "requested" {
		t.Fatalf("job_cancel = %q, want requested", out)
	}
	waitForTerminal(t, l, id, "sess-1", 5*time.Second)
	// A second cancel on the terminal job reports already-finished.
	out, err = jt.Cancel().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err != nil {
		t.Fatalf("job_cancel (terminal): %v", err)
	}
	if out != "already-finished" {
		t.Fatalf("job_cancel (terminal) = %q, want already-finished", out)
	}
	if snap, _ := l.Get(context.Background(), id, "sess-1"); snap.Status != StatusKilled {
		t.Fatalf("job status after cancel = %q, want killed", snap.Status)
	}
}

// TestJobWaitTimesOut verifies job_wait returns the current snapshot (not an
// error) when the timeout elapses before the job settles.
func TestJobWaitTimesOut(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-1" }, nil)

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "sess-1", gate)

	out, err := jt.Wait().Execute(context.Background(), json.RawMessage(`{"id":"bash-1","timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("job_wait: %v", err)
	}
	if !strings.Contains(out, "did not settle within 1s") || !strings.Contains(out, "running") {
		t.Fatalf("job_wait timeout output = %q, want timeout notice + current status", out)
	}
	_ = id
}

// TestJobTransitionEventsLoggedOnce verifies the D3 event scheme
// (dispatch-m5a-2 §4 decision — tool-layer): job/status on each observed
// non-terminal transition and job/done on the terminal settle, each exactly
// once, with no duplicates on repeated observation.
func TestJobTransitionEventsLoggedOnce(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	log := &eventLog{}
	jt := NewJobTools(l, func() string { return "sess-1" }, log.record)

	gate := make(chan struct{})
	id := mustStart(t, l, "bash", "sess-1", gate)

	// First observation of the running job logs job/status.
	if _, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil {
		t.Fatalf("job_status: %v", err)
	}
	// Repeated observation of the same status must not re-log.
	if _, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil {
		t.Fatalf("job_status (2nd): %v", err)
	}
	// Cancel moves running→stopping: job/status.
	if out, err := jt.Cancel().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil || out != "requested" {
		t.Fatalf("job_cancel = %q, err = %v", out, err)
	}
	// Terminal settle: job/done.
	if _, err := jt.Wait().Execute(context.Background(), json.RawMessage(`{"id":"bash-1","timeout_seconds":5}`)); err != nil {
		t.Fatalf("job_wait: %v", err)
	}
	// Post-terminal observation must not re-log job/done.
	if _, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil {
		t.Fatalf("job_status (post): %v", err)
	}

	c := log.counts()
	if c["job/start"] != 0 {
		t.Fatalf("unexpected job/start events: %d (job was started directly)", c["job/start"])
	}
	if c["job/status"] != 2 {
		t.Fatalf("job/status count = %d, want 2 (running, stopping)", c["job/status"])
	}
	if c["job/done"] != 1 {
		t.Fatalf("job/done count = %d, want 1", c["job/done"])
	}
	_ = id
}

// TestJobStartEventSequence verifies a tool-started job logs job/start on
// registration and job/done on a terminal settle (via a real command).
func TestJobStartEventSequence(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	log := &eventLog{}
	jt := NewJobTools(l, func() string { return "sess-1" }, log.record)

	long := "sleep 30"
	if runtime.GOOS == "windows" {
		long = "ping -n 30 127.0.0.1"
	}
	if _, err := jt.Start().Execute(context.Background(), json.RawMessage(`{"command":"`+long+`"}`)); err != nil {
		t.Fatalf("job_start: %v", err)
	}
	if _, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil {
		t.Fatalf("job_status: %v", err)
	}
	// Cancel the long job; the running→stopping transition logs job/status.
	if out, err := jt.Cancel().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil || out != "requested" {
		t.Fatalf("job_cancel = %q, err = %v", out, err)
	}
	if _, err := jt.Wait().Execute(context.Background(), json.RawMessage(`{"id":"bash-1","timeout_seconds":10}`)); err != nil {
		t.Fatalf("job_wait: %v", err)
	}

	c := log.counts()
	if c["job/start"] != 1 {
		t.Fatalf("job/start count = %d, want 1", c["job/start"])
	}
	if c["job/done"] != 1 {
		t.Fatalf("job/done count = %d, want 1", c["job/done"])
	}
	if c["job/status"] < 1 {
		t.Fatalf("job/status count = %d, want >= 1 (running→stopping)", c["job/status"])
	}
}

// TestJobToolsOwnerFencing verifies the tools authorize access through the
// injected owner session: a different session is rejected (ADR 决策 ①).
func TestJobToolsOwnerFencing(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-1" }, nil)

	gate := make(chan struct{})
	mustStart(t, l, "bash", "sess-1", gate)

	other := NewJobTools(l, func() string { return "sess-2" }, nil)
	_, err := other.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`))
	if err == nil {
		t.Fatal("cross-owner job_status must be rejected")
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-owner error = %v, want ErrForbidden", err)
	}
	// The owner session itself is served.
	if _, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"bash-1"}`)); err != nil {
		t.Fatalf("owner job_status: %v", err)
	}
}

// TestJobToolsUnknownJob verifies an unknown id is surfaced as an error.
func TestJobToolsUnknownJob(t *testing.T) {
	l := NewLocal(LocalOpts{})
	defer l.Close()
	jt := NewJobTools(l, func() string { return "sess-1" }, nil)
	if _, err := jt.Status().Execute(context.Background(), json.RawMessage(`{"id":"nope-1"}`)); err == nil {
		t.Fatal("job_status on an unknown id must fail")
	}
}
