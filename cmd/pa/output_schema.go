// Code generated for the mandatory canonical output declaration.
package main

func (acpMCPCallTool) OutputSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"content":           map[string]any{"type": "array", "items": map[string]any{}},
			"structuredContent": map[string]any{},
		},
		"required": []string{"content"},
	}
}

func (acpMCPListTool) OutputSchema() map[string]any  { return map[string]any{"type": "string"} }
func (acpTerminalTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
