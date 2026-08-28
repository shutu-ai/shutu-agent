// Code generated for the mandatory canonical output declaration.
package web

func (WebSearchTool) OutputSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"content": map[string]any{"type": "string"},
			"sources": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"url": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"},
					"snippet": map[string]any{"type": "string"}, "publishedAt": map[string]any{"type": "string"},
				}, "required": []string{"url"},
			}},
			"truncated": map[string]any{"type": "boolean"},
		},
		"required": []string{"sources", "truncated"},
	}
}

func (WebFetchTool) OutputSchema() map[string]any {
	body := map[string]any{"oneOf": []any{
		map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"kind": map[string]any{"type": "string", "const": "html"}, "content": map[string]any{"type": "string"},
		}, "required": []string{"kind", "content"}},
		map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"kind": map[string]any{"type": "string", "const": "text"}, "content": map[string]any{"type": "string"},
		}, "required": []string{"kind", "content"}},
	}}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"url": map[string]any{"type": "string"}, "statusCode": map[string]any{"type": "integer"},
			"body": body, "truncated": map[string]any{"type": "boolean"},
		},
		"required": []string{"url", "statusCode", "body", "truncated"},
	}
}
