// Code generated for the mandatory canonical output declaration.
package skill

func (SkillLoadTool) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":     map[string]any{"type": "string"},
			"provider": map[string]any{"type": "string"},
			"resourceBase": map[string]any{"oneOf": []any{
				map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "const": "directory"},
						"path": map[string]any{"type": "string"},
					}, "required": []string{"kind", "path"},
				},
				map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "const": "url"},
						"url":  map[string]any{"type": "string"},
					}, "required": []string{"kind", "url"},
				},
				map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"kind":        map[string]any{"type": "string", "const": "opaque"},
						"description": map[string]any{"type": "string"},
					}, "required": []string{"kind", "description"},
				},
			}},
			"content": map[string]any{"type": "string"},
		},
		"required": []string{"name", "provider", "content"},
	}
}
