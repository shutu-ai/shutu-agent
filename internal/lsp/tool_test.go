package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agenttools "github.com/jabing/shutu-agent/internal/tools"
)

func TestResolveInsideRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, _, err := resolveInside(root, filepath.Join("..", "outside.go")); err == nil {
		t.Fatal("resolveInside accepted an escaping path")
	}
	gotRoot, gotPath, err := resolveInside(root, "pkg/main.go")
	if err != nil || gotRoot != root || !strings.HasSuffix(filepath.ToSlash(gotPath), "pkg/main.go") {
		t.Fatalf("resolveInside = %q, %q, %v", gotRoot, gotPath, err)
	}
}

func TestParseLocationsAndHover(t *testing.T) {
	locations := parseLocations(json.RawMessage(`[{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":4}}}]`))
	if len(locations) != 1 || locations[0].Range.Start.Line != 1 {
		t.Fatalf("locations = %#v", locations)
	}
	if got := renderHover(json.RawMessage(`{"contents":{"kind":"markdown","value":"func main()"}}`), 100); !strings.Contains(got, "func main()") {
		t.Fatalf("hover = %q", got)
	}
}

func TestToolExecuteWithFakeStdioServer(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LSP_HELPER", "1")
	tool := NewTool(Config{Command: os.Args[0], Args: []string{"-test.run=TestLSPHelperProcess"}, ExtensionToLang: map[string]string{".go": "go"}, Timeout: 5 * time.Second}, func() string { return root })
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{"operation": "goToDefinition", "file_path": "main.go", "line": 2, "character": 6}))
	if err != nil || !strings.Contains(out, "main.go:2:1") {
		t.Fatalf("lsp output = %q, err=%v", out, err)
	}
}

func TestLSPRegistryPreservesDSHStructuredLocations(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LSP_HELPER", "1")
	tool := NewTool(Config{Command: os.Args[0], Args: []string{"-test.run=TestLSPHelperProcess"}, ExtensionToLang: map[string]string{".go": "go"}, Timeout: 5 * time.Second}, func() string { return root })
	reg := agenttools.New()
	reg.SetPolicy(agenttools.Policy{Enabled: []string{ToolName}})
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register lsp: %v", err)
	}
	result, err := reg.Execute(context.Background(), ToolName, mustJSON(map[string]any{"operation": "goToDefinition", "file_path": "main.go", "line": 2, "character": 6}))
	if err != nil || result.IsError {
		t.Fatalf("lsp registry result = %+v, err=%v", result, err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["kind"] != "locations" || value["resolvedWorkspaceUri"] == nil {
		t.Fatalf("lsp value = %#v, want DSH locations object", result.Value)
	}
	if entry := reg.Catalog()[0]; !entry.Cancellable {
		t.Fatalf("lsp catalog contract = %+v, want cancellable", entry)
	}
}

func TestLSPHelperProcess(t *testing.T) {
	if os.Getenv("LSP_HELPER") != "1" || !containsArg(os.Args, "-test.run=TestLSPHelperProcess") {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readTestMessage(reader)
		if err != nil {
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &request) != nil {
			return
		}
		if len(request.ID) == 0 {
			if request.Method == "exit" {
				return
			}
			continue
		}
		result := any(map[string]any{})
		switch request.Method {
		case "textDocument/definition":
			result = []map[string]any{{"uri": "file:///fake/main.go", "range": map[string]any{"start": map[string]int{"line": 1, "character": 0}, "end": map[string]int{"line": 1, "character": 4}}}}
		case "textDocument/hover":
			result = map[string]any{"contents": map[string]string{"kind": "markdown", "value": "fake hover"}}
		case "shutdown":
			result = nil
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result})
		_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(payload), payload)
	}
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func containsArg(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func readTestMessage(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			_, _ = fmt.Sscanf(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), "%d", &length)
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err := io.ReadFull(reader, body)
	return body, err
}
