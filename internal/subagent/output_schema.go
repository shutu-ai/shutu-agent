// Code generated for the mandatory canonical output declaration.
package subagent

func objectSchema(properties map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func (structuredOutputTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (SubagentCancelTool) OutputSchema() map[string]any   { return map[string]any{"type": "string"} }
func (SubagentInterruptTool) OutputSchema() map[string]any {
	return objectSchema(map[string]any{"accepted": map[string]any{"const": true}}, []string{"accepted"})
}
func (SubagentListTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (SubagentListAgentsTool) OutputSchema() map[string]any {
	child := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind": map[string]any{"const": "child"}, "id": map[string]any{"type": "string"},
			"label": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"running", "idle", "ready"}},
			"parent": map[string]any{"type": "string"}, "depth": map[string]any{"type": "integer"},
		}, "required": []string{"kind", "id", "label", "status"},
	}
	diagnostic := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind": map[string]any{"const": "diagnostic"}, "id": map[string]any{"type": "string"},
			"reason": map[string]any{"type": "string", "enum": []string{"corrupt", "unsupported", "unavailable"}},
			"parent": map[string]any{"type": "string"}, "depth": map[string]any{"type": "integer"},
		}, "required": []string{"kind", "id", "reason"},
	}
	return map[string]any{"type": "array", "items": map[string]any{"oneOf": []any{child, diagnostic}}}
}
func (SubagentReportTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (SubagentResumeTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (SubagentSendTool) OutputSchema() map[string]any {
	return objectSchema(map[string]any{"messageId": map[string]any{"type": "string"}}, []string{"messageId"})
}
func (SubagentForkTool) OutputSchema() map[string]any   { return delegationOutputSchema(false) }
func (SubagentStatusTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }

func (SubagentSpawnTool) OutputSchema() map[string]any { return delegationOutputSchema(true) }
func (SubagentTeammateTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"member": map[string]any{"type": "object", "additionalProperties": true},
	}}
}
func (SubagentMessageTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"messageId": map[string]any{"type": "string"},
		"status":    map[string]any{"type": "string", "enum": []string{"accepted", "queued"}},
	}}
}
func (SubagentWaitTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true}
}

func delegationOutputSchema(continuable bool) map[string]any {
	variants := []any{
		objectSchema(map[string]any{"kind": map[string]any{"const": "foreground"}, "runId": map[string]any{"type": "string"}, "output": map[string]any{"type": "array"}}, []string{"kind", "runId", "output"}),
		objectSchema(map[string]any{"kind": map[string]any{"const": "background"}, "jobId": map[string]any{"type": "string"}}, []string{"kind", "jobId"}),
	}
	if continuable {
		variants = append(variants, objectSchema(map[string]any{"kind": map[string]any{"const": "continuable"}, "subagentId": map[string]any{"type": "string"}}, []string{"kind", "subagentId"}))
	}
	return map[string]any{"oneOf": variants}
}
