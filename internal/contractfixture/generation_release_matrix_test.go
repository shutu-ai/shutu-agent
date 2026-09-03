package contractfixture_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/credential"
	"github.com/shutu-ai/shutu-agent/internal/mcp"
	"github.com/shutu-ai/shutu-agent/internal/plugin"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// TestGenerationRotationReloadAndReconnectMatrix consolidates the three
// generation-boundary fault families required by A9.3. Credential rotation is
// observed through two independent SQLite-backed vault handles, plugin reload
// owns a real disposer/effect boundary, and MCP reconnect uses two real child
// processes. The matrix proves each replacement settles and never replays the
// failed operation.
func TestGenerationRotationReloadAndReconnectMatrix(t *testing.T) {
	t.Run("credential rotation settles across independent handles", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "credentials.db")
		firstStore, err := store.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		secondStore, err := store.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		first, err := credential.New(context.Background(), firstStore)
		if err != nil {
			t.Fatal(err)
		}
		second, err := credential.New(context.Background(), secondStore)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Set(context.Background(), "RELEASE_CREDENTIAL", "generation-one"); err != nil {
			t.Fatal(err)
		}
		inFlight, err := second.Acquire(context.Background(), "RELEASE_CREDENTIAL")
		if err != nil {
			t.Fatal(err)
		}
		if got := inFlight.Value(); got != "generation-one" {
			t.Fatalf("in-flight credential = %q, want generation-one", got)
		}
		if err := first.Set(context.Background(), "RELEASE_CREDENTIAL", "generation-two"); err != nil {
			t.Fatal(err)
		}
		if got := inFlight.Value(); got != "generation-one" {
			t.Fatalf("rotation changed an in-flight lease to %q", got)
		}
		replacement, err := second.Acquire(context.Background(), "RELEASE_CREDENTIAL")
		if err != nil {
			t.Fatal(err)
		}
		if got := replacement.Value(); got != "generation-two" {
			t.Fatalf("replacement credential = %q, want generation-two", got)
		}
		replacement.Release()
		if err := first.Unset(context.Background(), "RELEASE_CREDENTIAL"); err != nil {
			t.Fatal(err)
		}
		inFlight.Release()
		if _, err := second.Acquire(context.Background(), "RELEASE_CREDENTIAL"); !errors.Is(err, credential.ErrNotFound) && !errors.Is(err, credential.ErrRevoked) {
			t.Fatalf("cross-handle acquire after revocation = %v", err)
		}
		record, err := secondStore.GetCredentialRecord(context.Background(), "RELEASE_CREDENTIAL")
		if !errors.Is(err, store.ErrCredentialNotFound) && (err != nil || !record.Revoked) {
			t.Fatalf("durable credential after revoke = %+v, %v", record, err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		if err := firstStore.Close(); err != nil {
			t.Fatal(err)
		}
		if err := secondStore.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("plugin reload disposes old generation and rejects stale execution", func(t *testing.T) {
		root := t.TempDir()
		effectPath := filepath.Join(root, "plugin.effects")
		cleanupPath := filepath.Join(root, "plugin.cleanup")
		toolRegistry := tools.New()
		registry := plugin.NewRegistryWithTools(nil, toolRegistry)
		spec := func(value string) plugin.Spec {
			return plugin.Spec{
				ID: "generation-plugin",
				Manifest: plugin.Manifest{
					ID: "generation-plugin", Version: value,
				},
				Mount: func(*agent.Scope) (func() error, error) {
					return func() error {
						return os.WriteFile(cleanupPath, []byte(value), 0o600)
					}, nil
				},
				Tools: func(_ *agent.Scope, publisher plugin.ToolPublisher) error {
					return publisher.Publish(generationPluginTool{name: "generation_tool", value: value, effectPath: effectPath})
				},
			}
		}
		if err := registry.Mount(spec("one")); err != nil {
			t.Fatal(err)
		}
		toolRegistry.SetPolicy(tools.Policy{Enabled: []string{"generation_tool"}})
		result, err := toolRegistry.Execute(context.Background(), "generation_tool", json.RawMessage(`{}`))
		if err != nil || result.Output != "one" {
			t.Fatalf("old plugin execution = %+v, %v", result, err)
		}
		if err := registry.Reload(spec("two")); err != nil {
			t.Fatal(err)
		}
		if cleanup, err := os.ReadFile(cleanupPath); err != nil || string(cleanup) != "one" {
			t.Fatalf("old plugin cleanup = %q, %v; want one", cleanup, err)
		}
		result, err = toolRegistry.Execute(context.Background(), "generation_tool", json.RawMessage(`{}`))
		if err != nil || result.Output != "two" {
			t.Fatalf("new plugin execution = %+v, %v", result, err)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(effectPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(data)) != "one\ntwo" {
			t.Fatalf("plugin effect journal = %q, want one/two", string(data))
		}
	})

	t.Run("MCP reconnect replaces a killed generation without replay", func(t *testing.T) {
		root := t.TempDir()
		logPath := filepath.Join(root, "mcp.requests")
		effectPath := filepath.Join(root, "mcp.effects")
		t.Setenv("SHUTU_GENERATION_MCP_LOG", logPath)
		t.Setenv("SHUTU_GENERATION_MCP_EFFECT", effectPath)
		args := []string{"-test.run=^TestGenerationReleaseMCPHelper$"}
		newClient := func() mcp.Client {
			return mcp.NewStdioClient(os.Args[0], args)
		}
		client := mcp.NewReconnectingClientWithFactory(newClient(), func(context.Context) (mcp.Client, error) {
			return newClient(), nil
		}, mcp.ReconnectOptions{
			Enabled: true, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 1,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if tools, err := client.ListTools(ctx); err != nil || len(tools) != 1 || tools[0].Name != "generation_echo" {
			t.Fatalf("first MCP tools = %#v, %v", tools, err)
		}
		if _, err := client.Call(ctx, "generation_echo", map[string]any{}); !errors.Is(err, mcp.ErrConnection) {
			t.Fatalf("MCP call during child death = %v, want ErrConnection", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			result, callErr := client.Call(ctx, "generation_echo", map[string]any{})
			if callErr == nil {
				if result.IsError || len(result.Content) != 1 || result.Content[0].(map[string]any)["text"] != "stable" {
					t.Fatalf("replacement MCP result = %#v", result)
				}
				break
			}
			if (!errors.Is(callErr, mcp.ErrConnection) && !errors.Is(callErr, mcp.ErrNotStarted)) || time.Now().After(deadline) {
				t.Fatalf("replacement MCP call = %v", callErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		effect, err := os.ReadFile(effectPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(effect)) != "committed" {
			t.Fatalf("MCP effect journal = %q, want exactly one committed effect", string(effect))
		}
		assertGenerationRequestJournal(t, logPath, []string{
			"first initialize",
			"first tools/list",
			"first tools/call",
			"replacement initialize",
			"replacement tools/call",
		})
	})
}

func assertGenerationRequestJournal(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
	if len(got) != len(want) {
		t.Fatalf("MCP request journal = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MCP request journal = %v, want %v", got, want)
		}
	}
}

func TestGenerationReleaseMCPHelper(t *testing.T) {
	logPath := os.Getenv("SHUTU_GENERATION_MCP_LOG")
	effectPath := os.Getenv("SHUTU_GENERATION_MCP_EFFECT")
	if logPath == "" || effectPath == "" {
		t.Skip("generation release MCP helper")
	}
	record := func(method string) {
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer file.Close()
		_, _ = file.WriteString(generationMCPMode() + " " + method + "\n")
		_ = file.Sync()
	}
	in := make(chan map[string]any, 8)
	go func() {
		defer close(in)
		var line string
		for {
			var buffer [1]byte
			_, err := os.Stdin.Read(buffer[:])
			if err != nil {
				return
			}
			if buffer[0] == '\n' {
				var request map[string]any
				if json.Unmarshal([]byte(line), &request) == nil {
					in <- request
				}
				line = ""
				continue
			}
			line += string(buffer[:])
		}
	}()
	encoder := json.NewEncoder(os.Stdout)
	for request := range in {
		id := request["id"]
		method, _ := request["method"].(string)
		switch method {
		case "initialize":
			record(method)
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
					"serverInfo": map[string]any{"name": generationMCPMode(), "version": "1"},
				},
			})
		case "tools/list":
			record(method)
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"tools": []any{map[string]any{
					"name": "generation_echo", "inputSchema": map[string]any{"type": "object"},
				}}},
			})
		case "tools/call":
			record(method)
			if _, err := os.Stat(effectPath); errors.Is(err, os.ErrNotExist) {
				file, createErr := os.OpenFile(effectPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
				if createErr == nil {
					_, _ = file.WriteString("committed")
					_ = file.Sync()
					_ = file.Close()
					os.Exit(87)
				}
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "stable"}}},
			})
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}
}

func generationMCPMode() string {
	if _, err := os.Stat(os.Getenv("SHUTU_GENERATION_MCP_EFFECT")); err == nil {
		return "replacement"
	}
	return "first"
}

type generationPluginTool struct {
	name       string
	value      string
	effectPath string
}

func (t generationPluginTool) Name() string           { return t.name }
func (t generationPluginTool) Description() string    { return "generation release plugin tool" }
func (t generationPluginTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t generationPluginTool) OutputSchema() map[string]any {
	return map[string]any{"type": "string"}
}
func (t generationPluginTool) Execute(context.Context, any) (string, error) {
	file, err := os.OpenFile(t.effectPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.WriteString(t.value + "\n"); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return t.value, nil
}
