package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/sdkclient"
)

func fileURLForWindowsPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
}

func writeReferenceRuntimeFiles(t *testing.T, directory, referenceRoot, storage string) error {
	t.Helper()
	loaderPath := filepath.Join(directory, "reference-source-loader.mjs")
	loader, err := os.ReadFile(filepath.Join("testdata", "reference_source_loader.mjs"))
	if err != nil {
		return err
	}
	loaderText := strings.ReplaceAll(string(loader), "process.env.DSH_REFERENCE_ROOT", fmtReferencePathLiteral(referenceRoot))
	if err := os.WriteFile(loaderPath, []byte(loaderText), 0o600); err != nil {
		return err
	}

	rootJS := fmtReferencePathLiteral(referenceRoot)
	storageJS := fmtReferencePathLiteral(storage)
	helper := `import { pathToFileURL } from 'node:url'
import { resolve } from 'node:path'

const root = resolve(` + rootJS + `)
const load = relative => import(pathToFileURL(resolve(root, relative)).href)
const [
  { Context },
  AgentCore,
  { default: SubagentRuntime },
  { default: JsonlSessionPersistence },
  { JsonRpcLineTransport },
  { HarnessSdkJsonRpcServer },
] = await Promise.all([
  load('vendor/cordis/src/index.ts'),
  load('packages/examples/agent-spine-demo/src/index.ts'),
  load('packages/subagent/subagent/src/index.ts'),
  load('packages/session/session-persistence-jsonl/src/index.ts'),
  load('packages/sdk/protocol/src/index.ts'),
  load('packages/sdk/server/src/index.ts'),
])
const context = new Context()
await context.plugin(AgentCore, { workspaceContext: false })
await context.plugin(SubagentRuntime)
await context.plugin(JsonlSessionPersistence, { root: ` + storageJS + ` })
await new Promise(resolve => setTimeout(resolve, 100))
const transport = new JsonRpcLineTransport(process.stdin, process.stdout)
const server = new HarnessSdkJsonRpcServer(context, transport)
transport.onRequest(async (method, params) => {
  const result = await server.handleRequest(method, params)
  if (method === 'shutdown') {
    setImmediate(async () => {
      try {
        await transport.flush()
        await context.fiber.dispose()
      } finally {
        process.exit(0)
      }
    })
  }
  return result
})
transport.start()
`
	return os.WriteFile(filepath.Join(directory, "reference-sdk-helper.ts"), []byte(helper), 0o600)
}

func fmtReferencePathLiteral(path string) string {
	encoded, _ := json.Marshal(filepath.ToSlash(path))
	return string(encoded)
}

// TestPinnedReferenceSDKRuntimeExternalClient closes the A7.3 reference SDK
// runtime boundary: the read-only pinned reference implementation runs in a
// real Node subprocess, while Shutu supplies the external Go SDK client and a
// real local OpenAI-compatible provider.
func TestPinnedReferenceSDKRuntimeExternalClient(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	referenceRoot := filepath.Clean(filepath.Join(repoRoot, "..", "deepseek-harness"))
	if envRoot := strings.TrimSpace(os.Getenv("DSH_REFERENCE_ROOT")); envRoot != "" {
		referenceRoot = envRoot
	}
	for _, relative := range []string{
		"vendor/cordis/src/index.ts",
		"packages/examples/agent-spine-demo/src/index.ts",
		"packages/subagent/subagent/src/index.ts",
		"packages/session/session-persistence-jsonl/src/index.ts",
		"packages/sdk/protocol/src/index.ts",
		"packages/sdk/server/src/index.ts",
		"node_modules/typescript/lib/typescript.js",
	} {
		if _, err := os.Stat(filepath.Join(referenceRoot, filepath.FromSlash(relative))); err != nil {
			t.Skipf("pinned reference checkout is incomplete: %v", err)
		}
	}

	var mu sync.Mutex
	var requests []map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"reference runtime replay"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			"",
		}, "\n\n")))
	}))
	defer provider.Close()

	root := t.TempDir()
	storage := filepath.Join(root, "reference-storage")
	if err := writeReferenceRuntimeFiles(t, root, referenceRoot, storage); err != nil {
		t.Fatal(err)
	}
	loader := fileURLForWindowsPath(filepath.Join(root, "reference-source-loader.mjs"))
	options := sdkclient.ClientOptions{
		Command: "node",
		Args: []string{
			"--experimental-transform-types",
			"--loader", loader,
			filepath.Join(root, "reference-sdk-helper.ts"),
		},
		Dir:            referenceRoot,
		RequestTimeout: 15 * time.Second,
	}
	options.Env = append(os.Environ(),
		"DSH_REFERENCE_ROOT="+referenceRoot,
		"DEEPSEEK_API_KEY=pinned-reference-key",
		"DEEPSEEK_BASE_URL="+provider.URL,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := sdkclient.NewClient(options)
	subscription := client.Subscribe(nil)
	if err := client.Start(); err != nil {
		t.Fatalf("start pinned reference runtime: %v", err)
	}
	initialized, err := client.Initialize(ctx, sdkclient.InitializeParams{
		CWD: root, Provider: "deepseek-official", Model: "reference-replay-model", MaxTokens: 321,
	})
	if err != nil {
		t.Fatalf("reference initialize: %v", err)
	}
	if initialized.ServerInfo.Name != "deepseek-harness-sdk-runtime" {
		t.Fatalf("reference identity = %#v", initialized.ServerInfo)
	}

	messageID, err := client.Prompt(ctx, "reference-replay", []sdkclient.ContentBlock{
		sdkclient.TextContent("replay through the pinned runtime"),
	})
	if err != nil || messageID == "" {
		t.Fatalf("reference prompt = %q, %v", messageID, err)
	}
	var sawReceipt, sawAssistant, sawIdle bool
	for !sawAssistant || !sawIdle {
		notification, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			t.Fatalf("reference notifications: %v", nextErr)
		}
		switch notification.Method {
		case "session.event":
			var params struct {
				SessionID string                 `json:"sessionId"`
				Event     sdkclient.SessionEvent `json:"event"`
			}
			if json.Unmarshal(notification.Params, &params) != nil || params.SessionID != "reference-replay" {
				continue
			}
			if params.Event.Type == "agent/inbox/spliced" && strings.Contains(string(params.Event.Data), messageID) {
				sawReceipt = true
			}
			if params.Event.Type == "assistant/message" && strings.Contains(string(params.Event.Data), "reference runtime replay") {
				sawAssistant = true
			}
		case "session.status":
			var params struct {
				SessionID string `json:"sessionId"`
				Status    string `json:"status"`
			}
			if json.Unmarshal(notification.Params, &params) != nil || params.SessionID != "reference-replay" {
				continue
			}
			if params.Status == "idle" {
				sawIdle = true
			}
		}
	}
	if !sawReceipt || !sawAssistant || !sawIdle {
		t.Fatalf("reference turn receipt=%v assistant=%v idle=%v", sawReceipt, sawAssistant, sawIdle)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("reference shutdown: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("reference provider requests = %d, want exactly one", len(requests))
	}
	body := requests[0]
	if body["model"] != "reference-replay-model" || body["max_tokens"] != float64(321) {
		t.Fatalf("reference provider request = %#v", body)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("reference provider messages = %#v, want system plus user", messages)
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first reference provider message = %#v, want system", first)
	}
	sawUserPrompt := false
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].(string)
		if message["role"] == "user" && strings.Contains(content, "pinned runtime") {
			sawUserPrompt = true
		}
	}
	if !sawUserPrompt {
		t.Fatalf("reference provider messages did not preserve the user prompt: %#v", messages)
	}
}
