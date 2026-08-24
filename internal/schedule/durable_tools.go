package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type DurableScheduleTools struct {
	s   *DurableScheduler
	now func() time.Time
}

func NewDurableScheduleTools(s *DurableScheduler, now func() time.Time) *DurableScheduleTools {
	if now == nil {
		now = time.Now
	}
	return &DurableScheduleTools{s: s, now: now}
}

func (t DurableCreateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt       string `json:"prompt"`
		AfterSeconds *int64 `json:"after_seconds"`
		At           string `json:"at"`
		EverySeconds *int64 `json:"every_seconds"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	record, err := t.t.s.Create(ctx, DurableCreateRequest{Prompt: in.Prompt, AfterSeconds: in.AfterSeconds, At: in.At, EverySeconds: in.EverySeconds}, t.t.now())
	if err != nil {
		return "", fmt.Errorf("schedule_create: %w", err)
	}
	return marshalScheduleValue(record)
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
			"at":            map[string]any{"type": "string", "minLength": 1},
			"every_seconds": map[string]any{"type": "integer", "minimum": MinEverySeconds},
		},
		"required": []string{"prompt"}, "additionalProperties": false,
		"oneOf": []any{map[string]any{"required": []string{"after_seconds"}}, map[string]any{"required": []string{"at"}}, map[string]any{"required": []string{"every_seconds"}}},
	}
}
func (DurableCreateTool) Name() string        { return ToolCreateName }
func (DurableCreateTool) Description() string { return "schedule a durable Session-local reminder" }

type DurableListTool struct{ t *DurableScheduleTools }

func (DurableListTool) Name() string        { return ToolListName }
func (DurableListTool) Description() string { return "list active Session-local reminders" }
func (DurableListTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func (t DurableListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	views, err := t.t.s.List(ctx, t.t.now())
	if err != nil {
		return "", fmt.Errorf("schedule_list: %w", err)
	}
	return marshalScheduleValue(views)
}

type DurableDeleteTool struct{ t *DurableScheduleTools }

func (DurableDeleteTool) Name() string        { return ToolDeleteName }
func (DurableDeleteTool) Description() string { return "delete a durable Session-local reminder by id" }
func (DurableDeleteTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"id"}, "additionalProperties": false}
}

func (t DurableDeleteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	if in.ID == "" || strings.TrimSpace(in.ID) != in.ID {
		return "", fmt.Errorf("schedule_delete: invalid id")
	}
	deleted, err := t.t.s.Delete(ctx, in.ID)
	if err != nil {
		return "", fmt.Errorf("schedule_delete: %w", err)
	}
	if !deleted {
		return fmt.Sprintf(`{"id":%q,"deleted":false,"code":"schedule_not_found"}`, in.ID), nil
	}
	return fmt.Sprintf(`{"id":%q,"deleted":true}`, in.ID), nil
}

func marshalScheduleValue(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("schedule: encode result: %w", err)
	}
	return string(raw), nil
}
