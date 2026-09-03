package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

// TestNativeCLICrossEntryFixture drives the production pa binary through one
// real REPL turn using the shared cross-entry fixture prompt/response. It is
// the native-CLI leg of A9.1: output crosses stdout, durable events cross the
// production SQLite sink, and a fresh store handle must derive the same
// projection after the process exits.
func TestNativeCLICrossEntryFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("native CLI cross-entry builds the production binary")
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	prompt := fixture.Prompt[0]
	var request struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(prompt, &request); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	requests := make(chan map[string]any, 8)
	var requestBodies []map[string]any
	var requestSeq atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case requests <- body:
			requestBodies = append(requestBodies, body)
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		effectPath := filepath.Join(root, "native-fixture-effect.txt")
		if requestSeq.Add(1) == 1 {
			arguments := map[string]any{
				"file_path": effectPath,
				"content":   fixture.Tool.Output,
			}
			rawArguments, _ := json.Marshal(arguments)
			writeNativeSSE(w, map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index": 0, "id": "fixture-native-call", "type": "function",
							"function": map[string]any{
								"name": "write", "arguments": "",
							},
						}},
					},
					"finish_reason": nil,
				}},
			})
			writeNativeSSE(w, map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"index": 0,
							"function": map[string]any{
								"arguments": string(rawArguments),
							},
						}},
					},
					"finish_reason": nil,
				}},
			})
			writeNativeSSE(w, map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{}, "finish_reason": "tool_calls",
			}}})
			writeNativeDone(w)
			return
		}
		writeNativeSSE(w, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": fixture.Assistant}, "finish_reason": nil,
		}}})
		writeNativeSSE(w, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "stop",
		}}})
		writeNativeDone(w)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "pa.yaml")
	promptsDir, err := filepath.Abs(filepath.Join("config", "prompts"))
	if err != nil {
		t.Fatal(err)
	}
	rootForConfig := filepath.ToSlash(root)
	config := `model: cross-entry-model
base_url: ` + server.URL + `
mode: standard
reasoning_effort: off
data_dir: ` + dataDir + `
prompts_dir: ` + promptsDir + `
workspace:
  default_dir: '` + rootForConfig + `'
fs:
  root: '` + rootForConfig + `'
security:
  crash_dump_policy: external
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	seedStore, err := store.OpenSQLite(filepath.Join(dataDir, "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seedStore.SetSetting(context.Background(), "llm.key.deepseek-official", "cross-entry-key"); err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}

	binary := filepath.Join(root, "pa-under-test.exe")
	build := exec.Command("go", "build", "-o", binary, "./cmd/pa")
	build.Dir = "../.."
	build.Env = os.Environ()
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production CLI: %v\n%s", err, output)
	}

	cmd := exec.Command(binary, "--config", configPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DEEPSEEK_API_KEY=cross-entry-key")
	cmd.Stdin = strings.NewReader(request.Text + "\n/exit\n")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("production CLI: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("production CLI timed out\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	select {
	case body := <-requests:
		model, _ := body["model"].(string)
		if model != "cross-entry-model" {
			t.Fatalf("CLI request model = %#v", body)
		}
	default:
		t.Fatal("production CLI did not call the configured provider")
	}
	effect, err := os.ReadFile(filepath.Join(root, "native-fixture-effect.txt"))
	if err != nil {
		debug, debugErr := store.OpenSQLite(filepath.Join(dataDir, "pa.db"))
		if debugErr == nil {
			defer debug.Close()
			if metas, metaErr := debug.ListSessions(context.Background()); metaErr == nil && len(metas) == 1 {
				if debugEvents, eventErr := debug.LoadSession(context.Background(), metas[0].ID); eventErr == nil {
					for _, event := range debugEvents {
						t.Logf("debug event %d %s %s", event.Seq, event.Type, event.Data)
					}
				}
			}
		}
		t.Fatalf("read native CLI external effect: %v\nprovider requests=%d\nstdout=%q\nstderr=%q", err, len(requestBodies), stdout.String(), stderr.String())
	}
	if string(effect) != fixture.Tool.Output {
		t.Fatalf("native CLI external effect = %q, want %q", effect, fixture.Tool.Output)
	}

	st, err := store.OpenSQLite(filepath.Join(dataDir, "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	metas, err := st.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("CLI sessions = %#v, want one production native session", metas)
	}
	events, err := st.LoadSession(context.Background(), metas[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := projection.Build(events)
	if err != nil {
		t.Fatal(err)
	}
	var sawToolResult bool
	for _, event := range events {
		if event.Type == "tool/result" {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("native CLI durable events contain no fixture tool result; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if len(snapshot.History) < 2 || snapshot.History[len(snapshot.History)-1].Text() != fixture.Assistant {
		for _, event := range events {
			if strings.Contains(event.Type, "assistant") {
				t.Logf("assistant event %s: %s", event.Type, event.Data)
			}
		}
		t.Fatalf("native CLI projection history = %#v\nprovider requests = %#v\nstdout=%q\nstderr=%q",
			snapshot.History, requestBodies, stdout.String(), stderr.String())
	}
	if snapshot.AsOfSeq == 0 || len(snapshot.Surface) == 0 {
		t.Fatalf("native CLI projection = %#v", snapshot)
	}
}

func writeNativeSSE(w http.ResponseWriter, payload any) {
	encoded, _ := json.Marshal(payload)
	_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
}

func writeNativeDone(w http.ResponseWriter) {
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}
