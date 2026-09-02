package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"
	"time"
)

type DurableScheduleTools struct {
	s       *DurableScheduler
	resolve func(context.Context) (*DurableScheduler, error)
	now     func() time.Time
}

func NewDurableScheduleTools(s *DurableScheduler, now func() time.Time) *DurableScheduleTools {
	return newDurableScheduleTools(s, nil, now)
}

// NewDurableScheduleToolsWithResolver binds the tools to the scheduler owned by
// the caller's runtime context. This is the production seam for session-local
// reminders; the legacy constructor remains useful for isolated callers and
// package tests.
func NewDurableScheduleToolsWithResolver(resolve func(context.Context) (*DurableScheduler, error), now func() time.Time) *DurableScheduleTools {
	return newDurableScheduleTools(nil, resolve, now)
}

func newDurableScheduleTools(s *DurableScheduler, resolve func(context.Context) (*DurableScheduler, error), now func() time.Time) *DurableScheduleTools {
	if now == nil {
		now = time.Now
	}
	return &DurableScheduleTools{s: s, resolve: resolve, now: now}
}

func (t *DurableScheduleTools) scheduler(ctx context.Context) (*DurableScheduler, error) {
	if t == nil {
		return nil, ErrDurableClosed
	}
	if t.resolve != nil {
		return t.resolve(ctx)
	}
	if t.s == nil {
		return nil, ErrDurableClosed
	}
	return t.s, nil
}

func (t DurableCreateTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		Prompt       string          `json:"prompt"`
		AfterSeconds *int64          `json:"after_seconds"`
		At           json.RawMessage `json:"at"`
		EverySeconds *int64          `json:"every_seconds"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	at, atLocal, err := decodeAtInput(in.At)
	if err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	scheduler, err := t.t.scheduler(ctx)
	if err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	record, err := scheduler.Create(ctx, DurableCreateRequest{Prompt: in.Prompt, AfterSeconds: in.AfterSeconds, At: at, AtLocal: atLocal, EverySeconds: in.EverySeconds}, t.t.now())
	if err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	view := DurableView{DurableRecord: record, State: scheduleState(record, t.t.now()), DeliveryMode: "session-local"}
	return marshalScheduleValue(view)
}

func (t DurableCreateTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	raw, err := t.Execute(ctx, args)
	if err != nil {
		if value, ok := scheduleErrorValue(err); ok {
			return marshalScheduleResult(value)
		}
		return agenttools.ToolResult{}, err
	}
	return parseScheduleResult(raw)
}

func (t *DurableScheduleTools) Create() DurableCreateTool { return DurableCreateTool{t: t} }
func (t *DurableScheduleTools) List() DurableListTool     { return DurableListTool{t: t} }
func (t *DurableScheduleTools) Delete() DurableDeleteTool { return DurableDeleteTool{t: t} }

type DurableCreateTool struct{ t *DurableScheduleTools }

func (DurableCreateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":        map[string]any{"type": "string", "minLength": 1},
			"after_seconds": map[string]any{"type": "integer", "minimum": 1},
			"at": map[string]any{"oneOf": []any{
				map[string]any{"type": "string", "minLength": 1},
				map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
					"date": map[string]any{"type": "string"}, "time": map[string]any{"type": "string"}, "time_zone": map[string]any{"type": "string"},
				}, "required": []string{"date", "time", "time_zone"}},
			}},
			"every_seconds": map[string]any{"type": "integer", "minimum": MinEverySeconds},
		},
		"required": []string{"prompt"}, "additionalProperties": false,
		"oneOf": []any{map[string]any{"required": []string{"after_seconds"}}, map[string]any{"required": []string{"at"}}, map[string]any{"required": []string{"every_seconds"}}},
	}
}

func decodeAtInput(raw json.RawMessage) (string, *LocalAtInput, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var instant string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &instant); err != nil {
			return "", nil, fmt.Errorf("at must be a strict offset date-time or local date/time object")
		}
		return instant, nil, nil
	}
	var local struct {
		Date     string `json:"date"`
		Time     string `json:"time"`
		TimeZone string `json:"time_zone"`
	}
	if err := json.Unmarshal(raw, &local); err != nil {
		return "", nil, fmt.Errorf("at must be a strict offset date-time or local date/time object")
	}
	return "", &LocalAtInput{Date: local.Date, Time: local.Time, TimeZone: local.TimeZone}, nil
}
func (DurableCreateTool) Name() string { return ToolCreateName }
func (DurableCreateTool) Description() string {
	return "Create one reminder in the current session. Supply a non-empty prompt and exactly one selector: a positive safe-integer after_seconds delay, at as a strict offset date-time or local date/time object, or safe-integer every_seconds of at least 300. Fixed-rate reminders stay creation-aligned, skip missed occurrences, and batch one latest occurrence per overdue rule. Delivery is session-local: the reminder runs on time only while this session is live and otherwise becomes overdue until the session is resumed."
}

type DurableListTool struct{ t *DurableScheduleTools }

func (DurableListTool) Name() string { return ToolListName }
func (DurableListTool) Description() string {
	return "List every active reminder in the current session in creation order, including its exact id, UTC target, scheduled or overdue state, and session-local delivery mode."
}
func (DurableListTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func (t DurableListTool) Execute(ctx context.Context, args any) (string, error) {
	scheduler, err := t.t.scheduler(ctx)
	if err != nil {
		return "", fmt.Errorf("schedule_list: %w", err)
	}
	views, err := scheduler.List(ctx, t.t.now())
	if err != nil {
		return "", fmt.Errorf("schedule_list: %w", err)
	}
	return marshalScheduleValue(views)
}

func (t DurableListTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	raw, err := t.Execute(ctx, args)
	if err != nil {
		if value, ok := scheduleErrorValue(err); ok {
			return marshalScheduleResult(value)
		}
		return agenttools.ToolResult{}, err
	}
	return parseScheduleResult(raw)
}

type DurableDeleteTool struct{ t *DurableScheduleTools }

func (DurableDeleteTool) Name() string { return ToolDeleteName }
func (DurableDeleteTool) Description() string {
	return "Delete one active reminder in the current session by the exact id returned by schedule_create or schedule_list. Unknown or already-finished ids return deleted false."
}
func (DurableDeleteTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"id"}, "additionalProperties": false}
}

func (t DurableDeleteTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	if in.ID == "" || strings.TrimSpace(in.ID) != in.ID {
		return "", fmt.Errorf("schedule_delete: invalid id")
	}
	scheduler, err := t.t.scheduler(ctx)
	if err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	deleted, err := scheduler.Delete(ctx, in.ID)
	if err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	if !deleted {
		return fmt.Sprintf(`{"id":%q,"deleted":false,"code":"schedule_not_found"}`, in.ID), nil
	}
	return fmt.Sprintf(`{"id":%q,"deleted":true}`, in.ID), nil
}

func (t DurableDeleteTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	raw, err := t.Execute(ctx, args)
	if err != nil {
		if value, ok := scheduleErrorValue(err); ok {
			return marshalScheduleResult(value)
		}
		return agenttools.ToolResult{}, err
	}
	return parseScheduleResult(raw)
}

func scheduleState(record DurableRecord, now time.Time) string {
	if !record.ScheduledAt.After(now) {
		return "overdue"
	}
	return "scheduled"
}

func parseScheduleResult(raw string) (agenttools.ToolResult, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("schedule: encode result: %w", err)
	}
	return agenttools.ToolResult{Value: value, Output: raw}, nil
}

func marshalScheduleResult(value any) (agenttools.ToolResult, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("schedule: encode result: %w", err)
	}
	return agenttools.ToolResult{Value: value, Output: string(raw)}, nil
}

func scheduleErrorValue(err error) (map[string]any, bool) {
	message := err.Error()
	value := map[string]any{"code": "internal_error", "message": message}
	switch {
	case errors.Is(err, ErrInvalidPrompt):
		value["code"] = "invalid_prompt"
	case errors.Is(err, ErrInvalidSelector):
		value["code"] = "invalid_selector"
	case errors.Is(err, ErrNotFuture):
		value["code"] = "not_future"
	case errors.Is(err, ErrInvalidTimeZone):
		value["code"] = "invalid_time_zone"
	case errors.Is(err, ErrFrequencyTooHigh):
		value["code"] = "frequency_too_high"
	case errors.Is(err, ErrInvalidSpec):
		value["code"] = "invalid_rule"
	default:
		return nil, false
	}
	value["message"] = strings.TrimPrefix(message, "schedule_create: ")
	return value, true
}

func marshalScheduleValue(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("schedule: encode result: %w", err)
	}
	return string(raw), nil
}
