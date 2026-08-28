// Code generated for the mandatory canonical output declaration.
package interact

func (InteractAskTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (AskUserQuestionTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"answers": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false, "properties": map[string]any{
				"id":       map[string]any{"type": "string"},
				"selected": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"custom":   map[string]any{"type": "string"},
			}, "required": []string{"id", "selected"},
		}},
	}, "required": []string{"answers"}}
}
func (InteractStatusTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
