// tools.go — the M5a-2 Consumer half of the jobs seam (design.md §8 Consumer /
// D2/D9, dispatch-m5a-2 §2): dsh's job_output, job_kill and job_list
// projections are registered into the tools.Registry by the composition root
// (cmd/pa) when jobs.enabled, and auto-whitelisted by config.applyDefaults the
// same way the kb_* tools are. They implement the tools.Tool method set
// structurally (Go structural typing), so this package never imports the tools
// package — the seam stays decoupled.
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below before this code runs, so
// the tools only ever unmarshal already-valid arguments.
//
// D3 event logging lives here (dispatch-m5a-2 §4 decision — the tool-layer
// option): job_start emits job/start on successful registration, and the
// observing tools (job_output / job_kill / job_list) emit
// job/status on a newly-observed non-terminal status and job/done on a
// newly-observed terminal one, exactly once per (id, status) through a shared
// transition tracker. Every append happens inside a tool Execute — the serial
// main-loop path — so the session log is never touched from a background job
// goroutine (D5).
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

func decodeArgs(args any, dst any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// Tool names (whitelisted when jobs.enabled; see config.jobsToolNames).
const (
	ToolStartName  = "job_start"
	ToolStatusName = "job_status"
	ToolCancelName = "job_cancel"
	ToolWaitName   = "job_wait"
	ToolReadName   = "job_read"
)

// defaultJobKind is the kind applied when job_start omits kind. The registry
// treats kinds as opaque id namespaces, so "bash" is just this tool's default.
const defaultJobKind = "bash"

// defaultWaitSeconds is job_wait's timeout when the timeout_seconds argument
// is absent (dispatch-m5a-2 §2).
const defaultWaitSeconds = 30

// JobTools bundles the shared state of the five job_* tools: the Registry
// service, the owner-session provider, the event sink, and one transition
// tracker shared across all of them so job/status and job/done events are
// emitted exactly once per observed transition.
type JobTools struct {
	reg     Registry
	owner   func() string
	onEvent func(typ string, data any)
	tracker *transitionTracker
}

// NewJobTools returns the shared job-tool bundle bound to a Registry. owner,
// when non-nil, returns the current session id and is used both to default
// owner_session and to authorize every call (the job_* tools are owner-fenced
// by the registry, ADR 决策 ①). onEvent, when non-nil, receives the job/*
// event payloads; the composition root wires it to the session log (D3).
func NewJobTools(r Registry, owner func() string, onEvent func(typ string, data any)) *JobTools {
	return &JobTools{reg: r, owner: owner, onEvent: onEvent, tracker: newTransitionTracker()}
}

// Start returns the job_start tool.
func (t *JobTools) Start() JobStartTool { return JobStartTool{t: t} }

// Status returns the job_status tool.
func (t *JobTools) Status() JobStatusTool { return JobStatusTool{t: t} }

// Cancel returns the job_cancel tool.
func (t *JobTools) Cancel() JobCancelTool { return JobCancelTool{t: t} }

// Wait returns the job_wait tool.
func (t *JobTools) Wait() JobWaitTool { return JobWaitTool{t: t} }

// Read returns the job_read tool.
func (t *JobTools) Read() JobReadTool { return JobReadTool{t: t} }

// DshOutput, DshKill and DshList expose dsh's canonical job tools.
func (t *JobTools) DshOutput() DshJobOutputTool { return DshJobOutputTool{t: t} }
func (t *JobTools) DshKill() DshJobKillTool     { return DshJobKillTool{t: t} }
func (t *JobTools) DshList() DshJobListTool     { return DshJobListTool{t: t} }

// callerSession returns the active session id (the tool authorization
// boundary); "" when no owner provider is installed (unowned access).
func (t *JobTools) callerSession() string {
	if t.owner != nil {
		return t.owner()
	}
	return ""
}

// emit forwards one job/* event payload to the injected sink (D3).
func (t *JobTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

// reportTransition emits job/status for a newly-observed non-terminal status
// or job/done for a newly-observed terminal one, exactly once per (id, status)
// via the shared tracker. A terminal settle also reads the stored output so
// the job/done event carries its bounded summary (dispatch-m5a-2 §1).
func (t *JobTools) reportTransition(snap JobSnapshot) {
	if !t.tracker.track(snap.ID, snap.Status) {
		return // already reported for this status
	}
	if isTerminal(snap.Status) {
		summary := ""
		if out, _, err := t.reg.Read(context.Background(), snap.ID, snap.OwnerSession); err == nil {
			summary = out
		}
		t.emit(session.EventJobDone, session.NewJobDone(snap.ID, string(snap.Status), snap.Detail, summary))
		return
	}
	t.emit(session.EventJobStatus, session.NewJobStatus(snap.ID, string(snap.Status), snap.Detail))
}

// transitionTracker remembers the last status reported per job id so each
// transition is logged exactly once. It is the tools' only mutable shared
// state, guarded by a mutex (tool instances may be shared values).
type transitionTracker struct {
	mu   sync.Mutex
	last map[string]Status
}

func newTransitionTracker() *transitionTracker {
	return &transitionTracker{last: map[string]Status{}}
}

// track reports whether (id, status) is a newly-observed status worth logging
// (true) or was already reported (false), recording it as the latest.
func (tr *transitionTracker) track(id string, s Status) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if last, ok := tr.last[id]; ok && last == s {
		return false
	}
	tr.last[id] = s
	return true
}

// JobStartTool registers a background job that runs an external command
// (os/exec + context cancellation) and returns the registry-issued job id.
type JobStartTool struct {
	t *JobTools
}

func (JobStartTool) Name() string { return ToolStartName }

func (JobStartTool) Description() string {
	return "run a command line as a background job and return its job id; " +
		"observe it with job_output, list with job_list, and stop with job_kill"
}

func (JobStartTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the command line to run in the background (single non-interactive shell line)",
			},
			"kind": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "opaque job kind namespace (default bash)",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "one-line job label (default the command)",
			},
			"owner_session": map[string]any{
				"type":        "string",
				"description": "owning session id (default the current session)",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}
}

func (t JobStartTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Command      string `json:"command"`
		Kind         string `json:"kind"`
		Label        string `json:"label"`
		OwnerSession string `json:"owner_session"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_start: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("job_start: empty command")
	}
	kind := a.Kind
	if kind == "" {
		kind = defaultJobKind
	}
	label := a.Label
	if label == "" {
		label = a.Command
	}
	owner := a.OwnerSession
	if owner == "" {
		owner = t.t.callerSession()
	}
	id, err := t.t.reg.Start(ctx, JobStart{
		Kind:         Kind(kind),
		Label:        label,
		OwnerSession: owner,
		Run:          runCommandLine(a.Command),
	})
	if err != nil {
		return "", fmt.Errorf("job_start: %w", err)
	}
	// Establish the running baseline so a later job_status does not re-log
	// "running"; job/start is the registration event.
	t.t.tracker.track(id, StatusRunning)
	t.t.emit(session.EventJobStart, session.NewJobStart(id, kind, label, owner))
	// The command may settle before Start returns; report a terminal settle
	// immediately so job/done is logged even for a fast job.
	if snap, err := t.t.reg.Get(ctx, id, owner); err == nil {
		t.t.reportTransition(snap)
	}
	return fmt.Sprintf("started job %s (kind=%s, label=%q); observe with job_output, list with job_list, stop with job_kill", id, kind, label), nil
}

// JobStatusTool returns the current status snapshot of one job as text.
type JobStatusTool struct {
	t *JobTools
}

func (JobStatusTool) Name() string { return ToolStatusName }

func (JobStatusTool) Description() string {
	return "show the current status snapshot of one background job"
}

func (JobStatusTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the job id returned by job_start",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t JobStatusTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_status: %w", err)
	}
	snap, err := t.t.reg.Get(ctx, a.ID, t.t.callerSession())
	if err != nil {
		return "", fmt.Errorf("job_status: %w", err)
	}
	t.t.reportTransition(snap)
	return formatSnapshot(snap), nil
}

// JobCancelTool requests cancellation of one live job (idempotent).
type JobCancelTool struct {
	t *JobTools
}

func (JobCancelTool) Name() string { return ToolCancelName }

func (JobCancelTool) Description() string {
	return "request cancellation of one background job; returns requested or already-finished"
}

func (JobCancelTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the job id returned by job_start",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "optional reason recorded in the job detail",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t JobCancelTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_cancel: %w", err)
	}
	if a.Reason == "" {
		a.Reason = "cancelled via job_cancel"
	}
	res, err := t.t.reg.Kill(ctx, a.ID, t.t.callerSession(), a.Reason)
	if err != nil {
		return "", fmt.Errorf("job_cancel: %w", err)
	}
	// Observe the post-kill state so the running→stopping transition (or an
	// immediate terminal settle) is logged.
	if snap, err := t.t.reg.Get(ctx, a.ID, t.t.callerSession()); err == nil {
		t.t.reportTransition(snap)
	}
	return res, nil
}

// JobWaitTool blocks (bounded) until one job settles, then returns its
// terminal snapshot; on timeout it returns the current snapshot.
type JobWaitTool struct {
	t *JobTools
}

func (JobWaitTool) Name() string { return ToolWaitName }

func (JobWaitTool) Description() string {
	return "wait (bounded) for one background job to settle and return its terminal snapshot; on timeout returns the current status"
}

func (JobWaitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the job id returned by job_start",
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "max wait in seconds (default 30)",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t JobWaitTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID             string `json:"id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_wait: %w", err)
	}
	if a.TimeoutSeconds <= 0 {
		a.TimeoutSeconds = defaultWaitSeconds
	}
	snap, err := t.t.reg.Wait(ctx, a.ID, t.t.callerSession(), time.Duration(a.TimeoutSeconds)*time.Second)
	if err != nil {
		return "", fmt.Errorf("job_wait: %w", err)
	}
	t.t.reportTransition(snap)
	if isTerminal(snap.Status) {
		return "job " + snap.ID + " settled: " + formatSnapshot(snap), nil
	}
	return fmt.Sprintf("job %s did not settle within %ds; current status: %s", snap.ID, a.TimeoutSeconds, snap.Status), nil
}

// JobReadTool returns one job's stored output plus its status snapshot.
type JobReadTool struct {
	t *JobTools
}

func (JobReadTool) Name() string { return ToolReadName }

func (JobReadTool) Description() string {
	return "read one background job's output (empty while running) plus its status snapshot"
}

func (JobReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the job id returned by job_start",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t JobReadTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_read: %w", err)
	}
	out, snap, err := t.t.reg.Read(ctx, a.ID, t.t.callerSession())
	if err != nil {
		return "", fmt.Errorf("job_read: %w", err)
	}
	t.t.reportTransition(snap)
	if !isTerminal(snap.Status) {
		return fmt.Sprintf("job %s is %s (no output yet)\n%s", snap.ID, snap.Status, formatSnapshot(snap)), nil
	}
	if out == "" {
		return formatSnapshot(snap) + "\n  output: (empty)", nil
	}
	return formatSnapshot(snap) + "\n  output:\n" + out, nil
}

// DshJobOutputTool is the dsh-compatible replacement for job_read. It uses
// job_id and can optionally wait before returning the accumulated output.
type DshJobOutputTool struct{ t *JobTools }

func (DshJobOutputTool) Name() string { return "job_output" }
func (DshJobOutputTool) Description() string {
	return "read a background job's output; set wait=true when blocked on completion"
}
func (DshJobOutputTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"job_id":     map[string]any{"type": "string", "minLength": 1},
		"wait":       map[string]any{"type": "boolean"},
		"timeout_ms": map[string]any{"type": "integer", "minimum": 1},
	}, "required": []string{"job_id"}, "additionalProperties": false}
}
func (t DshJobOutputTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		JobID     string `json:"job_id"`
		Wait      bool   `json:"wait"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_output: %w", err)
	}
	if a.Wait {
		timeout := 30 * time.Second
		if a.TimeoutMS != nil {
			if *a.TimeoutMS <= 0 {
				return "", fmt.Errorf("job_output: timeout_ms must be positive")
			}
			timeout = time.Duration(*a.TimeoutMS) * time.Millisecond
		}
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
		if _, err := t.t.reg.Wait(ctx, a.JobID, t.t.callerSession(), timeout); err != nil {
			return "", fmt.Errorf("job_output: %w", err)
		}
	}
	var out string
	var snap JobSnapshot
	var err error
	if deltaReader, ok := t.t.reg.(DeltaReader); ok {
		out, snap, err = deltaReader.ReadDelta(ctx, a.JobID, t.t.callerSession())
	} else {
		out, snap, err = t.t.reg.Read(ctx, a.JobID, t.t.callerSession())
	}
	if err != nil {
		return "", fmt.Errorf("job_output: %w", err)
	}
	t.t.reportTransition(snap)
	status := dshStatusLine(snap)
	if out == "" {
		out = "(no new output)"
	}
	separator := "\n"
	if strings.HasSuffix(out, "\n") {
		separator = ""
	}
	return out + separator + status, nil
}

// DshJobKill is the dsh-compatible replacement for job_cancel.
type DshJobKillTool struct{ t *JobTools }

func (DshJobKillTool) Name() string        { return "job_kill" }
func (DshJobKillTool) Description() string { return "stop a background job that is no longer needed" }
func (DshJobKillTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"job_id": map[string]any{"type": "string", "minLength": 1},
		"reason": map[string]any{"type": "string"},
	}, "required": []string{"job_id"}, "additionalProperties": false}
}
func (t DshJobKillTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		JobID  string `json:"job_id"`
		Reason string `json:"reason"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("job_kill: %w", err)
	}
	if a.Reason == "" {
		a.Reason = "killed via job_kill"
	}
	res, err := t.t.reg.Kill(ctx, a.JobID, t.t.callerSession(), a.Reason)
	if err != nil {
		return "", fmt.Errorf("job_kill: %w", err)
	}
	snap, getErr := t.t.reg.Get(ctx, a.JobID, t.t.callerSession())
	if getErr == nil {
		t.t.reportTransition(snap)
	}
	if res == "already-finished" {
		return fmt.Sprintf("job %s had already finished %s", a.JobID, dshStatusLine(snap)), nil
	}
	return fmt.Sprintf("requested cancellation of job %s", a.JobID), nil
}

func dshStatusLine(snap JobSnapshot) string {
	if snap.Detail == "" {
		return fmt.Sprintf("[status: %s]", snap.Status)
	}
	return fmt.Sprintf("[status: %s, %s]", snap.Status, snap.Detail)
}

// DshJobListTool is the dsh job_list projection.
type DshJobListTool struct{ t *JobTools }

func (DshJobListTool) Name() string        { return "job_list" }
func (DshJobListTool) Description() string { return "list background jobs visible in this session" }
func (DshJobListTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (t DshJobListTool) Execute(ctx context.Context, args any) (string, error) {
	snaps, err := t.t.reg.List(ctx, t.t.callerSession())
	if err != nil {
		return "", fmt.Errorf("job_list: %w", err)
	}
	if len(snaps) == 0 {
		return "(no background jobs)", nil
	}
	var b strings.Builder
	for i, snap := range snaps {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s [%s] %s", snap.ID, snap.Kind, snap.Status)
		if snap.Label != "" {
			fmt.Fprintf(&b, " — %s", snap.Label)
		}
	}
	return b.String(), nil
}

// formatSnapshot renders a job snapshot as model-facing text.
func formatSnapshot(snap JobSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "job %s: %s", snap.ID, snap.Status)
	if snap.Detail != "" {
		fmt.Fprintf(&sb, " (%s)", snap.Detail)
	}
	fmt.Fprintf(&sb, "\n  kind: %s", snap.Kind)
	fmt.Fprintf(&sb, "\n  label: %s", snap.Label)
	if snap.OwnerSession != "" {
		fmt.Fprintf(&sb, "\n  owner: %s", snap.OwnerSession)
	}
	fmt.Fprintf(&sb, "\n  started: %s", snap.StartedAt.UTC().Format(time.RFC3339))
	if snap.FinishedAt != nil {
		fmt.Fprintf(&sb, "\n  finished: %s", snap.FinishedAt.UTC().Format(time.RFC3339))
	}
	return sb.String()
}
