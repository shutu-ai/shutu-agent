// Package lsp provides the read-only model-facing LSP seam.
//
// The provider is intentionally a small stdio JSON-RPC client: the caller
// supplies a language-server executable and extension map, while this package
// owns workspace containment, one-based model coordinates, bounded rendering,
// and the initialize/open/query/close lifecycle.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ToolName            = "lsp"
	DefaultTimeout      = 60 * time.Second
	DefaultMaxLocations = 100
	DefaultMaxResult    = 16000
	DefaultMaxDocument  = 4 << 20
)

var operations = []string{"goToDefinition", "findReferences", "goToImplementation", "hover"}

// Config describes one generic stdio language-server host.
type Config struct {
	Command          string
	Args             []string
	ExtensionToLang  map[string]string
	Timeout          time.Duration
	MaxLocations     int
	MaxResultChars   int
	MaxDocumentBytes int
}

// Tool is the model-facing, read-only LSP consumer.
type Tool struct {
	config      Config
	root        func() string
	rootContext func(context.Context) string
}

// NewTool binds an LSP command to the active session workspace provider.
func NewTool(config Config, workspaceRoot func() string) *Tool {
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxLocations <= 0 {
		config.MaxLocations = DefaultMaxLocations
	}
	if config.MaxResultChars <= 0 {
		config.MaxResultChars = DefaultMaxResult
	}
	if config.MaxDocumentBytes <= 0 {
		config.MaxDocumentBytes = DefaultMaxDocument
	}
	if len(config.ExtensionToLang) == 0 {
		config.ExtensionToLang = map[string]string{".go": "go"}
	}
	normalized := make(map[string]string, len(config.ExtensionToLang))
	for ext, language := range config.ExtensionToLang {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext != "" && !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		normalized[ext] = strings.TrimSpace(language)
	}
	config.ExtensionToLang = normalized
	return &Tool{config: config, root: workspaceRoot}
}

// NewToolWithContext is the session-owned variant used by concurrent Agent
// runs. The context root takes precedence over the legacy zero-argument root.
func NewToolWithContext(config Config, workspaceRoot func() string, workspaceRootContext func(context.Context) string) *Tool {
	t := NewTool(config, workspaceRoot)
	t.rootContext = workspaceRootContext
	return t
}

func (Tool) Name() string { return ToolName }

// CancellationAware is explicit: the query timeout derives from the registry
// context and is attached to both the language-server process and RPC requests.
func (*Tool) CancellationAware() bool { return true }

func (Tool) Description() string {
	return "Query a language server for precise code navigation. operation is one of goToDefinition, findReferences, goToImplementation, hover. line and character are one-based UTF-16 cursor coordinates. findReferences includes the declaration."
}

func (Tool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"operation": map[string]any{"type": "string", "enum": operations, "description": "goToDefinition, findReferences, goToImplementation, or hover"},
			"file_path": map[string]any{"type": "string", "description": "The source file to query, relative to the workspace or absolute."},
			"line":      map[string]any{"type": "integer", "description": "One-based line of the cursor."},
			"character": map[string]any{"type": "integer", "description": "One-based UTF-16 column of the cursor."},
		},
		"required": []string{"operation", "file_path", "line", "character"},
	}
}

func (t *Tool) Execute(ctx context.Context, args any) (string, error) {
	_, output, err := t.execute(ctx, args)
	return output, err
}

func (t *Tool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	value, output, err := t.execute(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	return agenttools.ToolResult{Value: value, Output: output}, nil
}

func (t *Tool) execute(ctx context.Context, args any) (any, string, error) {
	var in struct {
		Operation string `json:"operation"`
		FilePath  string `json:"file_path"`
		Line      int    `json:"line"`
		Character int    `json:"character"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return nil, "", fmt.Errorf("lsp: %w", err)
	}
	if !contains(operations, in.Operation) {
		return nil, "", fmt.Errorf("lsp: unsupported operation %q", in.Operation)
	}
	if strings.TrimSpace(in.FilePath) == "" || in.Line < 1 || in.Character < 1 {
		return nil, "", errors.New("lsp: file_path, line, and character are required and must be positive")
	}
	root := ""
	if t.rootContext != nil {
		root = strings.TrimSpace(t.rootContext(ctx))
	} else if t.root != nil {
		root = strings.TrimSpace(t.root())
	}
	if root == "" {
		return nil, "", errors.New("lsp: active session workspace cwd is unavailable")
	}
	root, path, err := resolveInside(root, in.FilePath)
	if err != nil {
		return nil, "", fmt.Errorf("lsp: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("lsp: source file: %w", err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("lsp: source path is a directory")
	}
	if info.Size() > int64(t.config.MaxDocumentBytes) {
		return nil, "", fmt.Errorf("lsp: source file exceeds %d-byte limit", t.config.MaxDocumentBytes)
	}
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("lsp: read source: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(path))
	language := t.config.ExtensionToLang[ext]
	if language == "" {
		return nil, "", fmt.Errorf("lsp: no configured language-server route for %q", filepath.Ext(path))
	}
	position := position{Line: in.Line - 1, Character: in.Character - 1}
	uri := fileURI(path)
	queryCtx, cancel := context.WithTimeout(ctx, t.config.Timeout)
	defer cancel()
	client, err := newClient(queryCtx, t.config.Command, t.config.Args, root)
	if err != nil {
		return nil, "", fmt.Errorf("lsp: start server: %w", err)
	}
	defer client.close()
	if err := client.initialize(queryCtx, root); err != nil {
		return nil, "", fmt.Errorf("lsp: initialize: %w", err)
	}
	if err := client.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": language, "version": 1, "text": string(source)},
	}); err != nil {
		return nil, "", fmt.Errorf("lsp: open document: %w", err)
	}
	result, err := client.query(queryCtx, in.Operation, uri, position)
	if err != nil {
		return nil, "", fmt.Errorf("lsp: query: %w", err)
	}
	if in.Operation == "hover" {
		return hoverValue(result), renderHover(result, t.config.MaxResultChars), nil
	}
	locations := parseLocations(result)
	if in.Operation == "findReferences" {
		// The LSP references request commonly omits the declaration. dsh's
		// contract includes it, so query definition and merge it below.
		if definition, defErr := client.query(queryCtx, "goToDefinition", uri, position); defErr == nil {
			locations = mergeLocations(locations, parseLocations(definition))
		}
	}
	return locationsValue(locations, fileURI(root)), renderLocations(in.Operation, locations, root, t.config.MaxLocations, t.config.MaxResultChars), nil
}

func locationsValue(locations []location, workspaceURI string) map[string]any {
	items := make([]any, 0, len(locations))
	for _, item := range locations {
		items = append(items, map[string]any{
			"uri": item.URI,
			"range": map[string]any{
				"start": map[string]any{"line": item.Range.Start.Line, "character": item.Range.Start.Character},
				"end":   map[string]any{"line": item.Range.End.Line, "character": item.Range.End.Character},
			},
		})
	}
	return map[string]any{"kind": "locations", "locations": items, "resolvedWorkspaceUri": workspaceURI}
}

func hoverValue(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{"kind": "hover", "hover": nil}
	}
	var value struct {
		Contents json.RawMessage `json:"contents"`
		Range    *lspRange       `json:"range"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return map[string]any{"kind": "hover", "hover": nil}
	}
	contents := hoverText(value.Contents)
	if contents == "" {
		return map[string]any{"kind": "hover", "hover": nil}
	}
	hover := map[string]any{"contents": contents}
	if value.Range != nil {
		hover["range"] = map[string]any{
			"start": map[string]any{"line": value.Range.Start.Line, "character": value.Range.Start.Character},
			"end":   map[string]any{"line": value.Range.End.Line, "character": value.Range.End.Character},
		}
	}
	return map[string]any{"kind": "hover", "hover": hover}
}

func resolveInside(root, name string) (string, string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootAbs, path)
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(rootAbs, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("file_path escapes the session workspace")
	}
	return rootAbs, path, nil
}

func fileURI(path string) string {
	abs, _ := filepath.Abs(path)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

type position struct{ Line, Character int }

type location struct {
	URI   string
	Range lspRange
}

type lspRange struct{ Start, End position }

type rpcClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID uint64
	closed bool
}

func newClient(ctx context.Context, command string, args []string, root string) (*rpcClient, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("lsp.command is empty")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = root
	cmd.Env = scrubbedEnv()
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	return &rpcClient{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

func scrubbedEnv() []string {
	const sensitive = "KEY SECRET TOKEN PASSWORD API"
	out := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(name)
		blocked := false
		for _, token := range strings.Fields(sensitive) {
			if strings.Contains(upper, token) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, entry)
		}
	}
	return out
}

func (c *rpcClient) initialize(ctx context.Context, root string) error {
	rootURI := fileURI(root)
	result, err := c.request(ctx, "initialize", map[string]any{
		"processId": nil, "rootUri": rootURI, "workspaceFolders": []any{map[string]any{"uri": rootURI, "name": filepath.Base(root)}}, "capabilities": map[string]any{},
	})
	if err != nil {
		return err
	}
	_ = result
	return c.notify("initialized", map[string]any{})
}

func (c *rpcClient) query(ctx context.Context, operation, uri string, pos position) (json.RawMessage, error) {
	method := map[string]string{"goToDefinition": "textDocument/definition", "findReferences": "textDocument/references", "goToImplementation": "textDocument/implementation", "hover": "textDocument/hover"}[operation]
	params := map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": pos.Line, "character": pos.Character}}
	if operation == "findReferences" {
		params["context"] = map[string]any{"includeDeclaration": true}
	}
	return c.request(ctx, method, params)
}

func (c *rpcClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	if err := c.writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		body, err := c.readMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		var response struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &response) != nil || string(response.ID) != strconv.FormatUint(id, 10) {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("server error %d: %s", response.Error.Code, response.Error.Message)
		}
		return response.Result, nil
	}
}

func (c *rpcClient) notify(method string, params any) error {
	return c.writeMessage(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcClient) writeMessage(value any) error {
	if c.closed {
		return errors.New("client is closed")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (c *rpcClient) readMessage() ([]byte, error) {
	length := -1
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			length, err = strconv.Atoi(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
			if err != nil || length < 0 || length > DefaultMaxDocument*4 {
				return nil, errors.New("invalid LSP Content-Length")
			}
		}
	}
	if length < 0 {
		return nil, errors.New("LSP response omitted Content-Length")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(c.stdout, body)
	return body, err
}

func (c *rpcClient) close() {
	if c.closed {
		return
	}
	_, _ = c.request(context.Background(), "shutdown", nil)
	_ = c.notify("exit", nil)
	c.closed = true
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
}

func parseLocations(raw json.RawMessage) []location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var many []json.RawMessage
	if json.Unmarshal(raw, &many) != nil {
		many = []json.RawMessage{raw}
	}
	var out []location
	for _, item := range many {
		var value struct {
			URI         string   `json:"uri"`
			Range       lspRange `json:"range"`
			TargetURI   string   `json:"targetUri"`
			TargetRange lspRange `json:"targetRange"`
		}
		if json.Unmarshal(item, &value) != nil {
			continue
		}
		if value.URI == "" {
			value.URI, value.Range = value.TargetURI, value.TargetRange
		}
		if value.URI != "" {
			out = append(out, location{URI: value.URI, Range: value.Range})
		}
	}
	return out
}

func mergeLocations(first, second []location) []location {
	out := append([]location(nil), first...)
	seen := make(map[string]bool, len(out))
	for _, item := range out {
		seen[locationKey(item)] = true
	}
	for _, item := range second {
		if !seen[locationKey(item)] {
			seen[locationKey(item)] = true
			out = append(out, item)
		}
	}
	return out
}

func locationKey(item location) string {
	return fmt.Sprintf("%s:%d:%d:%d:%d", item.URI, item.Range.Start.Line, item.Range.Start.Character, item.Range.End.Line, item.Range.End.Character)
}

func renderLocations(operation string, locations []location, root string, maxLocations, maxChars int) string {
	if len(locations) == 0 {
		return fmt.Sprintf("LSP %s: no locations found.", operation)
	}
	sort.SliceStable(locations, func(i, j int) bool { return locationKey(locations[i]) < locationKey(locations[j]) })
	var b strings.Builder
	fmt.Fprintf(&b, "LSP %s results (%d):\n", operation, len(locations))
	for i, item := range locations {
		if i >= maxLocations {
			fmt.Fprintf(&b, "... %d more locations omitted\n", len(locations)-i)
			break
		}
		line := fmt.Sprintf("- %s:%d:%d-%d:%d (%s)\n", displayURI(item.URI, root), item.Range.Start.Line+1, item.Range.Start.Character+1, item.Range.End.Line+1, item.Range.End.Character+1, item.URI)
		if b.Len()+len(line) > maxChars {
			b.WriteString("... result truncated by lsp.max_result_chars\n")
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderHover(raw json.RawMessage, maxChars int) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "LSP hover: no information found."
	}
	var value struct {
		Contents json.RawMessage `json:"contents"`
		Range    lspRange        `json:"range"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "LSP hover: malformed response."
	}
	text := hoverText(value.Contents)
	if text == "" {
		return "LSP hover: no information found."
	}
	out := "LSP hover:\n" + text
	if len(out) > maxChars {
		out = out[:maxChars] + "\n... result truncated by lsp.max_result_chars"
	}
	return out
}

func hoverText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var marked struct {
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	if json.Unmarshal(raw, &marked) == nil && marked.Value != "" {
		return marked.Value
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var parts []string
		for _, item := range list {
			if part := hoverText(item); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func displayURI(uri, root string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	path, err := filepath.Abs(filepath.FromSlash(u.Path))
	if err != nil {
		return uri
	}
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return path
}
