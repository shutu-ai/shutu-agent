package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
)

// StrReplaceEditorTool is DSH minimal's single filesystem tool. It combines
// view, create, literal replacement and line insertion behind one stable
// model-facing schema.
type StrReplaceEditorTool struct{ t *FsTools }

func (StrReplaceEditorTool) Name() string { return "str_replace_editor" }

func (StrReplaceEditorTool) Description() string {
	return "view, create, replace text in, or insert text into a file inside the allowed workspace"
}

func (StrReplaceEditorTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":     map[string]any{"type": "string", "enum": []string{"view", "create", "str_replace", "insert"}},
			"path":        map[string]any{"type": "string", "description": "absolute or workspace-relative file path"},
			"file_text":   map[string]any{"type": "string", "description": "file content for create"},
			"insert_line": map[string]any{"type": "integer", "description": "insert before this 0-based line index"},
			"new_str":     map[string]any{"type": "string", "description": "replacement or inserted text"},
			"old_str":     map[string]any{"type": "string", "description": "exact unique text to replace"},
			"view_range":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 2, "maxItems": 2},
		},
		"required":             []string{"command", "path"},
		"additionalProperties": false,
	}
}

func (StrReplaceEditorTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }

func (t StrReplaceEditorTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		Command    string  `json:"command"`
		Path       string  `json:"path"`
		FileText   *string `json:"file_text"`
		InsertLine *int    `json:"insert_line"`
		NewStr     *string `json:"new_str"`
		OldStr     string  `json:"old_str"`
		ViewRange  []int   `json:"view_range"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	if strings.TrimSpace(in.Path) == "" {
		return "", errors.New("str_replace_editor: path is required")
	}
	switch in.Command {
	case "view":
		content, err := t.t.f.Read(ctx, in.Path, 0)
		if err != nil {
			return "", fmt.Errorf("str_replace_editor: view: %w", err)
		}
		t.observe(ctx, in.Path)
		if len(in.ViewRange) == 2 {
			start, end := in.ViewRange[0], in.ViewRange[1]
			if start < 1 || (end != -1 && end < start) {
				return "", errors.New("str_replace_editor: invalid view_range")
			}
			if end == -1 {
				end = 1<<31 - 1
			}
			return formatReadWindow(content, start, end-start+1), nil
		}
		return formatReadWindow(content, 1, 0), nil
	case "create":
		if in.FileText == nil {
			return "", errors.New("str_replace_editor: file_text is required for create")
		}
		if _, err := t.t.f.Read(ctx, in.Path, 1); err == nil {
			return "", fmt.Errorf("str_replace_editor: file already exists: %s", in.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("str_replace_editor: create: %w", err)
		}
		if err := t.t.f.Write(ctx, in.Path, *in.FileText); err != nil {
			return "", fmt.Errorf("str_replace_editor: create: %w", err)
		}
		t.observe(ctx, in.Path)
		t.t.emit(session.EventFsWrite, session.NewFsWrite(in.Path))
		return fmt.Sprintf("New file created successfully at: %s", in.Path), nil
	case "str_replace":
		if in.OldStr == "" {
			return "", errors.New("str_replace_editor: old_str is required for str_replace")
		}
		content, err := t.readObserved(ctx, in.Path)
		if err != nil {
			return "", err
		}
		count := strings.Count(content, in.OldStr)
		if count == 0 {
			return "", fmt.Errorf("str_replace_editor: old_str did not appear in %s", in.Path)
		}
		if count > 1 {
			return "", fmt.Errorf("str_replace_editor: old_str appears %d times in %s; make it unique", count, in.Path)
		}
		replacement := ""
		if in.NewStr != nil {
			replacement = *in.NewStr
		}
		return t.writeEdited(ctx, in.Path, strings.Replace(content, in.OldStr, replacement, 1))
	case "insert":
		if in.InsertLine == nil {
			return "", errors.New("str_replace_editor: insert_line is required for insert")
		}
		if in.NewStr == nil {
			return "", errors.New("str_replace_editor: new_str is required for insert")
		}
		content, err := t.readObserved(ctx, in.Path)
		if err != nil {
			return "", err
		}
		lines := strings.Split(content, "\n")
		if *in.InsertLine < 0 || *in.InsertLine > len(lines) {
			return "", fmt.Errorf("str_replace_editor: insert_line %d is outside [0,%d]", *in.InsertLine, len(lines))
		}
		updated := strings.Join(append(append(append([]string{}, lines[:*in.InsertLine]...), strings.Split(*in.NewStr, "\n")...), lines[*in.InsertLine:]...), "\n")
		return t.writeEdited(ctx, in.Path, updated)
	default:
		return "", fmt.Errorf("str_replace_editor: unsupported command %q", in.Command)
	}
}

func (t StrReplaceEditorTool) readObserved(ctx context.Context, path string) (string, error) {
	if err := t.t.requireObserved(ctx, path); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	content, err := t.t.f.Read(ctx, path, 0)
	if err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	return content, nil
}

func (t StrReplaceEditorTool) writeEdited(ctx context.Context, path, content string) (string, error) {
	if err := t.t.requireObserved(ctx, path); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	if err := t.t.f.Write(ctx, path, content); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	t.observe(ctx, path)
	t.t.emit(session.EventFsWrite, session.NewFsWrite(path))
	return fmt.Sprintf("The file %s has been edited successfully.", path), nil
}

func (t StrReplaceEditorTool) observe(ctx context.Context, path string) {
	if version, err := t.t.f.Fingerprint(ctx, path); err == nil {
		t.t.observed[t.t.key(path)] = version
	}
}
