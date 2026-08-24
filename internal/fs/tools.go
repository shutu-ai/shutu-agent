// tools.go — the M6f-3 Consumer half of the safe-file-operation seam
// (design.md §8 Consumer / D2, dispatch-m6f-3 §4): read, write and
// list are registered into the tools.Registry by the composition root
// (cmd/pa) when fs.enabled, and auto-whitelisted by config.applyDefaults the
// same way the job_*/subagent_*/skill_*/schedule_*/plan_*/spill_*/interact_*/
// code_*/mcp_* tools are. The tools implement the tools.Tool method set
// structurally (Go structural typing), so this package never imports the
// tools package — the seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false; path/dir/content as plain strings) before this code runs; the checks
// are repeated here so a direct call can never bypass them.
//
// D3 event logging follows the M6e-2 tool-layer decision (ADR 决策 M6f /
// dispatch-m6f-3 §4): read emits fs/read (path + returned byte size) on a
// successful read, write emits fs/write (path) on a successful write, and
// list emits fs/list (dir + entry count) on a successful listing — each
// through the injected onEvent sink (the composition root wires it to the
// session log), inside a tool Execute on the serial main-loop path (D5). A
// failed operation (a path escaping the allowed root, a missing file, a file
// over the 1MiB read cap, a missing directory) returns an error message to the
// model and logs nothing — the loop surfaces it as tool/error. Failures are
// never a panic (dispatch-m6f-3 §4: 路径越界/不存在返回错误消息，非 panic).
package fs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

// Tool names — the dsh-aligned file tools (whitelisted when fs.enabled; see
// config.fsToolNames). read is the base tool (registered unconditionally);
// write / list / edit live on this seam (dsh: read/write/edit).
const (
	ToolReadName      = "read"
	ToolReadImageName = "read_image"
	ToolWriteName     = "write"
	ToolListName      = "list"
	ToolEditName      = "edit"
)

// FsTools bundles the shared state of the fs file tools: the FileService
// and the event sink. Keeping the bundle as fields keeps the constructor's
// signature the seam contract and the tool package decoupled from config (D2).
type FsTools struct {
	f        FileService
	onEvent  func(typ string, data any)
	observed map[string]string
}

// NewFsTools returns the file tool bundle bound to a FileService. onEvent,
// when non-nil, receives the fs/* event payloads; the composition root wires
// it to the session log (D3).
func NewFsTools(f FileService, onEvent func(typ string, data any)) *FsTools {
	return &FsTools{f: f, onEvent: onEvent, observed: make(map[string]string)}
}

// emit forwards one fs/* event payload to the injected sink (D3).
func (t *FsTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

// Write returns the write tool.
func (t *FsTools) Read() FsReadTool { return FsReadTool{t: t} }

func (t *FsTools) ReadImage() FsReadImageTool { return FsReadImageTool{t: t} }

func (t *FsTools) Write() FsWriteTool { return FsWriteTool{t: t} }

// List returns the list tool.
func (t *FsTools) List() FsListTool { return FsListTool{t: t} }

// Edit returns the edit tool.
func (t *FsTools) Edit() FsEditTool { return FsEditTool{t: t} }

// FsReadTool implements bounded, line-numbered read and records the observed
// file version for the later write/edit observation policy.
type FsReadTool struct {
	t *FsTools
}

func (FsReadTool) Name() string { return ToolReadName }

func (FsReadTool) Description() string {
	return "read a bounded, line-numbered text window from a file inside the allowed fs root"
}

func (FsReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string"},
			"offset": map[string]any{"type": "integer", "minimum": 1},
			"limit":  map[string]any{"type": "integer", "minimum": 1},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

func (t FsReadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("read: empty path")
	}
	content, err := t.t.f.Read(ctx, a.Path, 0)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	version, err := t.t.f.Fingerprint(ctx, a.Path)
	if err != nil {
		return "", fmt.Errorf("read: fingerprint: %w", err)
	}
	t.t.observed[t.t.key(a.Path)] = version
	t.t.emit(session.EventFsRead, session.NewFsRead(a.Path, len(content)))
	return formatReadWindow(content, a.Offset, a.Limit), nil
}

const (
	maxReadLines = 2000
	maxReadChars = 2000
	maxReadBytes = 51200
)

func formatReadWindow(content string, offset, limit int) string {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 || limit > maxReadLines {
		limit = maxReadLines
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
	for i := start; i < end && chars < maxReadChars; i++ {
		line := lines[i]
		remaining := maxReadChars - chars
		runes := []rune(line)
		if len(runes) > remaining {
			line = string(runes[:remaining])
		}
		row := fmt.Sprintf("%d\t%s\n", i+1, line)
		if out.Len()+len(row) > maxReadBytes {
			break
		}
		out.WriteString(row)
		chars += len([]rune(line))
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func (t *FsTools) key(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(t.f.Root(), filepath.Clean(path))
}

func (t *FsTools) requireObserved(ctx context.Context, path string) error {
	version, err := t.f.Fingerprint(ctx, path)
	if err != nil {
		return err
	}
	observed, ok := t.observed[t.key(path)]
	if !ok {
		return fmt.Errorf("file must be read before write/edit")
	}
	if observed != version {
		return fmt.Errorf("file changed since it was read")
	}
	return nil
}

// FsReadImageTool returns an image attachment reference as a rich tool result.
// The bytes stay on disk and are loaded by the provider only when it serializes
// the next request.
type FsReadImageTool struct {
	t *FsTools
}

func (FsReadImageTool) Name() string { return ToolReadImageName }

func (FsReadImageTool) Description() string {
	return "read an image inside the allowed fs root and provide it to a vision-capable model"
}

func (FsReadImageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

func (t FsReadImageTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	_, text, err := t.ExecuteContent(ctx, args)
	return text, err
}

func (t FsReadImageTool) ExecuteContent(ctx context.Context, args json.RawMessage) ([]llm.ContentBlock, string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, "", fmt.Errorf("read_image: %w", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return nil, "", fmt.Errorf("read_image: empty path")
	}
	mediaType := map[string]string{
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".webp": "image/webp", ".gif": "image/gif",
	}[strings.ToLower(filepath.Ext(a.Path))]
	if mediaType == "" {
		return nil, "", fmt.Errorf("read_image: unsupported image type")
	}
	data, err := t.t.f.ReadBytes(ctx, a.Path, 20*1024*1024)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: %w", err)
	}
	full := a.Path
	if !filepath.IsAbs(full) {
		full = filepath.Join(t.t.f.Root(), full)
	}
	ref := llm.ImageRef{
		MediaType: mediaType,
		Bytes:     int64(len(data)),
		Path:      filepath.Clean(full),
	}
	t.t.emit(session.EventFsRead, session.NewFsRead(a.Path, len(data)))
	return []llm.ContentBlock{{Kind: llm.BlockImage, Image: ref}}, "image " + a.Path, nil
}

func (t *FsTools) checkWriteObservation(ctx context.Context, path string) error {
	version, err := t.f.Fingerprint(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // creating a new file needs no prior observation
		}
		return err
	}
	observed, ok := t.observed[t.key(path)]
	if !ok {
		return fmt.Errorf("file must be read before overwriting")
	}
	if observed != version {
		return fmt.Errorf("file changed since it was read")
	}
	return nil
}

// FsWriteTool creates or overwrites a text file inside the allowed fs root
// (missing parent directories are created on demand) and returns the written
// path. A path that escapes the root is rejected before anything is touched.
type FsWriteTool struct {
	t *FsTools
}

func (FsWriteTool) Name() string { return ToolWriteName }

func (FsWriteTool) Description() string {
	return "create or overwrite a text file inside the allowed fs root (missing parent directories are created); returns the written path"
}

func (FsWriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "file path inside the allowed fs root (relative to fs.root, or an absolute path within it)",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "the text content to write (creates or overwrites the file)",
			},
		},
		"required":             []string{"path", "content"},
		"additionalProperties": false,
	}
}

func (t FsWriteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path    string  `json:"path"`
		Content *string `json:"content"` // nil = key absent (rejected); *"" = an explicitly empty file (valid)
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("write: empty path")
	}
	if a.Content == nil {
		return "", fmt.Errorf("write: missing content")
	}
	if err := t.t.checkWriteObservation(ctx, a.Path); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if err := t.t.f.Write(ctx, a.Path, *a.Content); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	delete(t.t.observed, t.t.key(a.Path))
	t.t.emit(session.EventFsWrite, session.NewFsWrite(a.Path))
	return fmt.Sprintf("wrote %s (%d bytes)", a.Path, len(*a.Content)), nil
}

// FsListTool lists the direct (non-recursive) children of a directory inside
// the allowed fs root, sorted by name, and returns a formatted table. The
// returned paths are relative to the root so they round-trip into
// read/write/list.
type FsListTool struct {
	t *FsTools
}

func (FsListTool) Name() string { return ToolListName }

func (FsListTool) Description() string {
	return "list the direct children of a directory inside the allowed fs root (non-recursive, sorted by name)"
}

func (FsListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dir": map[string]any{
				"type":        "string",
				"description": "directory path inside the allowed fs root (use \".\" for the root itself)",
			},
		},
		"required":             []string{"dir"},
		"additionalProperties": false,
	}
}

func (t FsListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	if strings.TrimSpace(a.Dir) == "" {
		return "", fmt.Errorf("list: empty dir")
	}
	entries, err := t.t.f.List(ctx, a.Dir)
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	t.t.emit(session.EventFsList, session.NewFsList(a.Dir, len(entries)))
	return formatEntries(a.Dir, entries), nil
}

// FsEditTool replaces literal text in a file inside the allowed fs root
// (dsh edit): it reads the file, replaces the FIRST occurrence of old_string
// with new_string (or all occurrences when replace_all is true), and writes
// the file back. An old_string that does not occur is an error — the file is
// left untouched. The fs/write event records the write as a log fact.
type FsEditTool struct {
	t *FsTools
}

func (FsEditTool) Name() string { return ToolEditName }

func (FsEditTool) Description() string {
	return "replace literal text in a file inside the allowed fs root (first occurrence, or all with replace_all); the file is left untouched when old_string is absent"
}

func (FsEditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "file path inside the allowed fs root (relative to fs.root, or an absolute path within it)",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "the exact literal text to replace",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "the replacement text",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"description": "replace every occurrence instead of only the first",
			},
		},
		"required":             []string{"path", "old_string", "new_string"},
		"additionalProperties": false,
	}
}

func (t FsEditTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("edit: empty path")
	}
	if a.OldString == "" {
		return "", fmt.Errorf("edit: empty old_string")
	}
	if err := t.t.requireObserved(ctx, a.Path); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	content, err := t.t.f.Read(ctx, a.Path, 0)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	n := 1
	if a.ReplaceAll {
		n = -1
	}
	if !strings.Contains(content, a.OldString) {
		return "", fmt.Errorf("edit: old_string not found in %s", a.Path)
	}
	updated := strings.Replace(content, a.OldString, a.NewString, n)
	if updated == content {
		return "", fmt.Errorf("edit: old_string not found in %s", a.Path)
	}
	if err := t.t.requireObserved(ctx, a.Path); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if err := t.t.f.Write(ctx, a.Path, updated); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	delete(t.t.observed, t.t.key(a.Path))
	t.t.emit(session.EventFsWrite, session.NewFsWrite(a.Path))
	return fmt.Sprintf("edited %s", a.Path), nil
}

// formatEntries renders one listing as model-facing text: a header with the
// directory and entry count followed by one line per entry (name, kind, and
// byte size for files).
func formatEntries(dir string, entries []Entry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s] %d entries\n", dir, len(entries))
	for _, e := range entries {
		if e.IsDir {
			fmt.Fprintf(&sb, "%s  dir\n", e.Name)
		} else {
			fmt.Fprintf(&sb, "%s  file  %d bytes\n", e.Name, e.Size)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
