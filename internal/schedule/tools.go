// tools.go — the M6a-2 Consumer half of the schedule seam (design.md §8
// Consumer / D2, dispatch-m6a-2 §3): schedule_create, schedule_list and
// schedule_delete are registered into the tools.Registry by the composition
// root (cmd/pa) when schedule.enabled, and auto-whitelisted by
// config.applyDefaults the same way the job_*/subagent_*/skill_* tools are.
// They implement the tools.Tool method set structurally (Go structural
// typing), so this package never imports the tools package — the seam stays
// decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false; kind restricted to the interval|cron enum) before this code runs; the
// kind check is repeated here so a direct call can never bypass it.
//
// D3 event logging follows the M5a-2 tool-layer decision (ADR 决策 ① 实施说明 /
// dispatch-m6a-2 §3): schedule_create emits schedule/create on a successful
// store, schedule_list emits schedule/list, schedule_delete emits
// schedule/delete — all through the injected onEvent sink (the composition
// root wires it to the session log), and each append happens inside a tool
// Execute — the serial main-loop path (D5). schedule/fire is emitted by the
// wiring layer's pre-step path (see cmd/pa), not by a tool.
package schedule

import (
	"context"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/session"
)

// Tool names (whitelisted when schedule.enabled; see config.scheduleToolNames).
const (
	ToolCreateName = "schedule_create"
	ToolListName   = "schedule_list"
	ToolDeleteName = "schedule_delete"
)

// ScheduleTools bundles the shared state of the three schedule_* tools: the
// Engine service and the event sink.
type ScheduleTools struct {
	e       Engine
	onEvent func(typ string, data any)
}

// NewScheduleTools returns the shared schedule-tool bundle bound to an Engine.
// onEvent, when non-nil, receives the schedule/* event payloads; the
// composition root wires it to the session log (D3).
func NewScheduleTools(e Engine, onEvent func(typ string, data any)) *ScheduleTools {
	return &ScheduleTools{e: e, onEvent: onEvent}
}

// Create returns the schedule_create tool.
func (t *ScheduleTools) Create() ScheduleCreateTool { return ScheduleCreateTool{t: t} }

// List returns the schedule_list tool.
func (t *ScheduleTools) List() ScheduleListTool { return ScheduleListTool{t: t} }

// Delete returns the schedule_delete tool.
func (t *ScheduleTools) Delete() ScheduleDeleteTool { return ScheduleDeleteTool{t: t} }

// emit forwards one schedule/* event payload to the injected sink (D3).
func (t *ScheduleTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *ScheduleTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// ScheduleCreateTool stores one recurring trigger (interval or cron) and
// returns the provider-issued schedule id.
type ScheduleCreateTool struct {
	t *ScheduleTools
}

func (ScheduleCreateTool) Name() string { return ToolCreateName }

func (ScheduleCreateTool) Description() string {
	return "create a recurring schedule (interval or cron) that fires its payload as a background job when due; observe it with schedule_list, remove it with schedule_delete"
}

func (ScheduleCreateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{string(KindInterval), string(KindCron)},
				"description": "trigger family: interval (a Go duration like 30m) or cron (a 5-field cron expression)",
			},
			"spec": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "interval: a Go duration string like \"30m\" or \"1h30m\"; cron: a 5-field expression like \"0 9 * * *\"",
			},
			"payload": map[string]any{
				"type":        "string",
				"description": "action text handed to the executor when the trigger fires (recorded by the fired background job)",
			},
		},
		"required":             []string{"kind", "spec"},
		"additionalProperties": false,
	}
}

func (t ScheduleCreateTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Kind    string `json:"kind"`
		Spec    string `json:"spec"`
		Payload string `json:"payload"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	kind := TriggerKind(a.Kind)
	if kind != KindInterval && kind != KindCron {
		return "", fmt.Errorf("schedule_create: unknown trigger kind %q (expected %q or %q)", a.Kind, KindInterval, KindCron)
	}
	s, err := t.t.e.Add(ctx, kind, a.Spec, a.Payload)
	if err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	// schedule/create is a log-only fact (D3); the created schedule id/kind/spec
	// are logged, and the returned text is what the loop logs as tool/result.
	if err := t.t.emitContext(ctx, session.EventScheduleCreate, session.NewScheduleCreate(s.ID, string(s.Kind), s.Spec)); err != nil {
		// The model must not receive a successful create when its durable fact
		// failed. Remove the provider row as a best-effort transaction rollback;
		// the Engine has no append hook, so this is the only way to avoid an
		// unlogged schedule surviving a retried tool call.
		_ = t.t.e.Remove(context.Background(), s.ID)
		return "", fmt.Errorf("schedule_create: persist event: %w", err)
	}
	return fmt.Sprintf("created schedule %s (kind=%s, spec=%q, next fire %s)",
		s.ID, s.Kind, s.Spec, s.NextFire.UTC().Format(time.RFC3339)), nil
}

// ScheduleListTool returns the current schedule table as text.
type ScheduleListTool struct {
	t *ScheduleTools
}

func (ScheduleListTool) Name() string { return ToolListName }

func (ScheduleListTool) Description() string {
	return "list every current schedule (id, kind, spec, enabled, next fire)"
}

func (ScheduleListTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

func (t ScheduleListTool) Execute(ctx context.Context, args any) (string, error) {
	all, err := t.t.e.List(ctx)
	if err != nil {
		return "", fmt.Errorf("schedule_list: %w", err)
	}
	// schedule/list is a log-only fact (D3) carrying the returned table size.
	if err := t.t.emitContext(ctx, session.EventScheduleList, session.NewScheduleList(len(all))); err != nil {
		return "", fmt.Errorf("schedule_list: persist event: %w", err)
	}
	return formatScheduleList(all), nil
}

// ScheduleDeleteTool removes one schedule; an unknown id is rejected.
type ScheduleDeleteTool struct {
	t *ScheduleTools
}

func (ScheduleDeleteTool) Name() string { return ToolDeleteName }

func (ScheduleDeleteTool) Description() string {
	return "remove one schedule by its id (returned by schedule_create/schedule_list)"
}

func (ScheduleDeleteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the schedule id returned by schedule_create or schedule_list",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t ScheduleDeleteTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	if err := t.t.e.Remove(ctx, a.ID); err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	// schedule/delete is a log-only fact (D3).
	if err := t.t.emitContext(ctx, session.EventScheduleDelete, session.NewScheduleDelete(a.ID)); err != nil {
		return "", fmt.Errorf("schedule_delete: persist event: %w", err)
	}
	return "deleted schedule " + a.ID, nil
}

// formatScheduleList renders the schedule table as model-facing text: one
// "- <id>: <kind> <spec> (<state>, next <time>)" line per schedule.
func formatScheduleList(all []Schedule) string {
	if len(all) == 0 {
		return "no schedules"
	}
	var sb strings.Builder
	for _, s := range all {
		state := "enabled"
		if !s.Enabled {
			state = "disabled"
		}
		fmt.Fprintf(&sb, "- %s: %s %q (%s, next %s)\n",
			s.ID, s.Kind, s.Spec, state, s.NextFire.UTC().Format(time.RFC3339))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
