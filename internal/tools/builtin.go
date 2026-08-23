package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// ReadFile returns the contents of a file. It is strictly read-only; the
// execution-class tools (bash etc.) arrive with the M3 safety whitelist (D10).
type ReadFile struct{}

func (ReadFile) Name() string { return "read" }

func (ReadFile) Description() string { return "read a text file from the local filesystem (read-only)" }

func (ReadFile) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "absolute path of the file to read",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

func (ReadFile) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	b, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return string(b), nil
}
