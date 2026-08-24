package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GetTime returns the current time. It takes no arguments.
type GetTime struct{}

func (GetTime) Name() string { return "get_time" }

func (GetTime) Description() string { return "return the current time in RFC 3339 format" }

func (GetTime) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

func (GetTime) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return time.Now().Format(time.RFC3339), nil
}

// ReadFile returns a bounded, line-numbered text window. Root is optional for
// compatibility with direct unit tests; the composition root uses
// NewReadFile, which pins the tool to the workspace.
type ReadFile struct {
	Root string
}

func NewReadFile(root string) ReadFile {
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = filepath.Clean(abs)
		}
	}
	return ReadFile{Root: root}
}

func (ReadFile) Name() string { return "read" }

func (ReadFile) Description() string { return "read a text file from the local filesystem (read-only)" }

func (ReadFile) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "file path inside the workspace",
			},
			"offset": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "1-based line number; defaults to 1",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "maximum lines; defaults to 2000",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

func (r ReadFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	var extras map[string]json.RawMessage
	if err := json.Unmarshal(args, &extras); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	var offset, limit int
	if raw, ok := extras["offset"]; ok {
		if err := json.Unmarshal(raw, &offset); err != nil {
			return "", fmt.Errorf("read: offset: %w", err)
		}
	}
	if raw, ok := extras["limit"]; ok {
		if err := json.Unmarshal(raw, &limit); err != nil {
			return "", fmt.Errorf("read: limit: %w", err)
		}
	}
	path := a.Path
	if r.Root != "" {
		var err error
		path, err = resolveReadPath(r.Root, path)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return formatReadWindow(string(b), offset, limit), nil
}

func resolveReadPath(root, path string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	var full string
	if filepath.IsAbs(path) {
		full = filepath.Clean(path)
	} else {
		full = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace root")
	}
	return full, nil
}

func formatReadWindow(content string, offset, limit int) string {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	start := offset - 1
	if start >= len(lines) {
		return ""
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	var out strings.Builder
	chars := 0
	for i := start; i < end && chars < 2000; i++ {
		line := lines[i]
		remaining := 2000 - chars
		runes := []rune(line)
		if len(runes) > remaining {
			line = string(runes[:remaining])
		}
		row := fmt.Sprintf("%d\t%s\n", i+1, line)
		if out.Len()+len(row) > 51200 {
			break
		}
		out.WriteString(row)
		chars += len([]rune(line))
	}
	return strings.TrimSuffix(out.String(), "\n")
}
