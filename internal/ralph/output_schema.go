// Code generated for the mandatory canonical output declaration.
package ralph

// RoundReportSchema is the fixed object contract requested from each fresh
// Ralph worker. The composition root passes it through the subagent runtime so
// structured reports are captured by the child-scoped structured_output tool,
// matching DSH's workflow-backed Ralph implementation.
func RoundReportSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"status":    map[string]any{"type": "string", "enum": []string{"continue", "complete", "blocked"}},
			"summary":   map[string]any{"type": "string"},
			"evidence":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"nextSteps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"blocker":   map[string]any{"type": "string"},
		},
		"required": []string{"status", "summary", "evidence", "nextSteps", "blocker"},
	}
}

func (RalphTool) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"runId":         map[string]any{"type": "string"},
			"agentsStarted": map[string]any{"type": "integer", "minimum": 1},
			"result":        map[string]any{},
		},
		"required": []string{"runId", "agentsStarted", "result"},
	}
}
