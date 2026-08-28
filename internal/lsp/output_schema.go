// Code generated for the mandatory canonical output declaration.
package lsp

func (Tool) OutputSchema() map[string]any {
	position := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"line": map[string]any{"type": "integer"}, "character": map[string]any{"type": "integer"},
	}, "required": []string{"line", "character"}}
	rng := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"start": position, "end": position,
	}, "required": []string{"start", "end"}}
	locations := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"kind": map[string]any{"type": "string", "const": "locations"},
		"locations": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"uri": map[string]any{"type": "string"}, "range": rng,
		}, "required": []string{"uri", "range"}}},
		"resolvedWorkspaceUri": map[string]any{"type": "string"},
	}, "required": []string{"kind", "locations", "resolvedWorkspaceUri"}}
	hover := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"kind": map[string]any{"type": "string", "const": "hover"},
		"hover": map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"contents": map[string]any{"type": "string"}, "range": rng,
		}, "required": []string{"contents"}}}},
	}, "required": []string{"kind", "hover"}}
	return map[string]any{"oneOf": []any{locations, hover}}
}
