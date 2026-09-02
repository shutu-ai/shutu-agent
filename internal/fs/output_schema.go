// Code generated for the mandatory canonical output declaration.
package fs

func (FsEditTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (FsListTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (FsReadImageTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"path": map[string]any{"type": "string"},
		"image": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"attachmentId": map[string]any{"type": "string"},
			"mediaType":    map[string]any{"type": "string", "enum": []string{"image/png", "image/jpeg", "image/webp", "image/gif"}},
			"bytes":        map[string]any{"type": "integer"}, "width": map[string]any{"type": "integer"}, "height": map[string]any{"type": "integer"}, "name": map[string]any{"type": "string"},
		}, "required": []string{"attachmentId", "mediaType", "bytes", "width", "height"}},
	}, "required": []string{"path", "image"}}
}
func (FsReadTool) OutputSchema() map[string]any  { return map[string]any{"type": "string"} }
func (FsWriteTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
