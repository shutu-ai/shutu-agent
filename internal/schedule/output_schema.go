// Code generated for the mandatory canonical output declaration.
package schedule

func durableViewSchema() map[string]any {
	shared := map[string]any{
		"id":           map[string]any{"type": "string"},
		"prompt":       map[string]any{"type": "string"},
		"scheduledAt":  map[string]any{"type": "string"},
		"state":        map[string]any{"type": "string", "enum": []string{"scheduled", "overdue"}},
		"deliveryMode": map[string]any{"type": "string", "const": "session-local"},
	}
	view := func(kind string, extra map[string]any, required []string) map[string]any {
		properties := map[string]any{}
		for key, value := range shared {
			properties[key] = value
		}
		properties["kind"] = map[string]any{"type": "string", "const": kind}
		for key, value := range extra {
			properties[key] = value
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	}
	return map[string]any{"oneOf": []any{
		view("after", map[string]any{"afterSeconds": map[string]any{"type": "integer"}}, []string{"id", "kind", "prompt", "afterSeconds", "scheduledAt", "state", "deliveryMode"}),
		view("at", nil, []string{"id", "kind", "prompt", "scheduledAt", "state", "deliveryMode"}),
		view("every", map[string]any{"everySeconds": map[string]any{"type": "integer"}}, []string{"id", "kind", "prompt", "everySeconds", "scheduledAt", "state", "deliveryMode"}),
	}}
}

func scheduleErrorSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
	}, "required": []string{"code", "message"}}
}

func (DurableCreateTool) OutputSchema() map[string]any {
	return map[string]any{"oneOf": []any{durableViewSchema(), scheduleErrorSchema()}}
}
func (DurableDeleteTool) OutputSchema() map[string]any {
	return map[string]any{"oneOf": []any{
		map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"id": map[string]any{"type": "string"}, "deleted": map[string]any{"type": "boolean", "const": true}}, "required": []string{"id", "deleted"}},
		map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"id": map[string]any{"type": "string"}, "deleted": map[string]any{"type": "boolean", "const": false}, "code": map[string]any{"type": "string", "const": "schedule_not_found"}}, "required": []string{"id", "deleted", "code"}},
		scheduleErrorSchema(),
	}}
}
func (DurableListTool) OutputSchema() map[string]any {
	return map[string]any{"oneOf": []any{map[string]any{"type": "array", "items": durableViewSchema()}, scheduleErrorSchema()}}
}
func (ScheduleCreateTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (ScheduleDeleteTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (ScheduleListTool) OutputSchema() map[string]any   { return map[string]any{"type": "string"} }
