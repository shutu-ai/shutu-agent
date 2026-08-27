package workflow

// OutputSchema is the DSH workflow envelope. The result value is arbitrary
// JSON, so it intentionally has no type restriction.
func (WorkflowRunTool) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"runId", "agentsStarted", "result"},
		"properties": map[string]any{
			"runId":         map[string]any{"type": "string"},
			"agentsStarted": map[string]any{"type": "integer", "minimum": 0},
			"result":        map[string]any{},
		},
	}
}
