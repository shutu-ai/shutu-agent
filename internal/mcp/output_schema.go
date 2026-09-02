// Code generated for the mandatory canonical output declaration.
package mcp

func (McpCallTool) OutputSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"content":           map[string]any{"type": "array", "items": map[string]any{}},
			"structuredContent": map[string]any{},
		},
		"required": []string{"content"},
	}
}
func (McpListTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
