// Code generated for the mandatory canonical output declaration.
package ralph

func (RalphTool) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"runId":         map[string]any{"type": "string"},
			"agentsStarted": map[string]any{"type": "integer", "minimum": 1},
			"result":        map[string]any{"type": "object"},
		},
		"required": []string{"runId", "agentsStarted", "result"},
	}
}
