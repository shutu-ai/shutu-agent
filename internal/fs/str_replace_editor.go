package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
)

// StrReplaceEditorTool is DSH minimal's single filesystem tool. It combines
// view, create, literal replacement and line insertion behind one stable
// model-facing schema.
type StrReplaceEditorTool struct{ t *FsTools }

const editorMaxOutputChars = 16_000

const editorTruncatedMessage = "<response clipped><NOTE>To save on context only part of this file has been shown to you. You should retry this tool after you have searched inside the file with `grep -n` in order to find the line numbers of what you are looking for.</NOTE>"

func (StrReplaceEditorTool) Name() string { return "str_replace_editor" }

func (StrReplaceEditorTool) Description() string {
	return "Custom editing tool for viewing, creating and editing files. Paths must be absolute. View supports files and directories; create never overwrites; str_replace requires one exact unique match; insert uses a zero-based line boundary."
}

func (StrReplaceEditorTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":     map[string]any{"type": "string", "enum": []string{"view", "create", "str_replace", "insert"}},
			"path":        map[string]any{"type": "string", "description": "absolute path to a file or directory"},
			"file_text":   map[string]any{"type": "string", "description": "file content for create"},
			"insert_line": map[string]any{"type": "integer", "description": "insert after this zero-based line boundary"},
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
	if !filepath.IsAbs(in.Path) {
		return "", fmt.Errorf("str_replace_editor: path %q is not absolute; provide the full workspace path", in.Path)
	}
	switch in.Command {
	case "view":
		if entries, err := t.t.f.List(ctx, in.Path); err == nil {
			if in.ViewRange != nil {
				return "", errors.New("str_replace_editor: view_range is not allowed for directories")
			}
			return formatEditorDirectory(ctx, t.t.f, in.Path, entries), nil
		}
		content, err := t.t.f.Read(ctx, in.Path, 0)
		if err != nil {
			return "", fmt.Errorf("str_replace_editor: view: %w", err)
		}
		t.observe(ctx, in.Path)
		if in.ViewRange != nil {
			if len(in.ViewRange) != 2 {
				return "", errors.New("str_replace_editor: view_range must contain exactly two integers")
			}
			start, end := in.ViewRange[0], in.ViewRange[1]
			lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
			if start < 1 || start > len(lines) || (end != -1 && (end < start || end > len(lines))) {
				return "", errors.New("str_replace_editor: invalid view_range")
			}
			return formatEditorFile(in.Path, content, in.ViewRange), nil
		}
		return formatEditorFile(in.Path, content, nil), nil
	case "create":
		if in.FileText == nil {
			return "", errors.New("str_replace_editor: file_text is required for create")
		}
		if _, err := t.t.f.Read(ctx, in.Path, 1); err == nil || !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("str_replace_editor: file already exists: %s", in.Path)
		} else if _, listErr := t.t.f.List(ctx, in.Path); listErr == nil {
			return "", fmt.Errorf("str_replace_editor: file already exists: %s", in.Path)
		}
		if err := t.t.f.Write(ctx, in.Path, *in.FileText); err != nil {
			return "", fmt.Errorf("str_replace_editor: create: %w", err)
		}
		t.observe(ctx, in.Path)
		if err := t.t.emitContext(ctx, session.EventFsWrite, session.NewFsWrite(in.Path)); err != nil {
			return "", fmt.Errorf("str_replace_editor: persist create event: %w", err)
		}
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
	if err := t.ensureObserved(ctx, path); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	content, err := t.t.f.Read(ctx, path, 0)
	if err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	return content, nil
}

func (t StrReplaceEditorTool) writeEdited(ctx context.Context, path, content string) (string, error) {
	if err := t.ensureObserved(ctx, path); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	if err := t.t.f.Write(ctx, path, content); err != nil {
		return "", fmt.Errorf("str_replace_editor: %w", err)
	}
	t.observe(ctx, path)
	if err := t.t.emitContext(ctx, session.EventFsWrite, session.NewFsWrite(path)); err != nil {
		return "", fmt.Errorf("str_replace_editor: persist write event: %w", err)
	}
	return fmt.Sprintf("The file %s has been edited successfully.", path), nil
}

func (t StrReplaceEditorTool) observe(ctx context.Context, path string) {
	if version, err := t.t.f.Fingerprint(ctx, path); err == nil {
		t.t.observed[t.t.key(ctx, path)] = version
	}
}

func (t StrReplaceEditorTool) ensureObserved(ctx context.Context, path string) error {
	key := t.t.key(ctx, path)
	if _, ok := t.t.observed[key]; ok {
		return t.t.requireObserved(ctx, path)
	}
	version, err := t.t.f.Fingerprint(ctx, path)
	if err != nil {
		return err
	}
	t.t.observed[key] = version
	return nil
}

func formatEditorFile(path, content string, viewRange []int) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	allLines := strings.Split(content, "\n")
	start, end := 1, len(allLines)
	if len(viewRange) == 2 {
		start, end = viewRange[0], viewRange[1]
		if end == -1 {
			end = len(allLines)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Here's the content of %s with line numbers (which has a total of %d lines):\n", path, len(allLines))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%6d  %s\n", i, allLines[i-1])
	}
	return editorMaybeTruncate(b.String())
}

func formatEditorDirectory(ctx context.Context, fs FileService, path string, entries []Entry) string {
	rows := []string{"d\t" + filepath.Clean(path)}
	var visit func(string, int)
	visit = func(dir string, depth int) {
		if depth > 2 {
			return
		}
		children, err := fs.List(ctx, dir)
		if err != nil {
			return
		}
		for _, entry := range children {
			if entry.Name == "" || strings.HasPrefix(entry.Name, ".") || entry.Name == "node_modules" || entry.Name == "__pycache__" {
				continue
			}
			kind := "f"
			if entry.IsDir {
				kind = "d"
			}
			childPath := filepath.Join(dir, entry.Name)
			rows = append(rows, kind+"\t"+childPath)
			if entry.IsDir {
				visit(childPath, depth+1)
			}
		}
	}
	for _, entry := range entries {
		if entry.Name == "" || strings.HasPrefix(entry.Name, ".") || entry.Name == "node_modules" || entry.Name == "__pycache__" {
			continue
		}
		kind := "f"
		if entry.IsDir {
			kind = "d"
		}
		childPath := filepath.Join(path, entry.Name)
		rows = append(rows, kind+"\t"+childPath)
		if entry.IsDir {
			visit(childPath, 2)
		}
	}
	sort.Strings(rows)
	return editorMaybeTruncate("Here're the files and directories up to 2 levels deep in " + filepath.Clean(path) + ", excluding hidden items, node_modules, and Python cache directories:\n" + strings.Join(rows, "\n") + "\n")
}

func editorMaybeTruncate(content string) string {
	if len(content) <= editorMaxOutputChars {
		return content
	}
	runes := []rune(content)
	if len(runes) <= editorMaxOutputChars {
		return content
	}
	return string(runes[:editorMaxOutputChars]) + editorTruncatedMessage
}
