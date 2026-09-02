package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestStreamableHTTPFaultTaskAndSchemaMatrix consolidates the external client
// contract for caller-level faults, retained sessions, task metadata and rich
// output schemas. Connection-class faults are covered by the real stdio/HTTP
// reconnect suites; this matrix proves auth and malformed protocol payloads do
// not become automatic retries or lose generation metadata.
func TestStreamableHTTPFaultTaskAndSchemaMatrix(t *testing.T) {
	var mu sync.Mutex
	var initialized, notifications, lists, callAttempts int
	var effects, actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments struct {
					Action string `json:"action"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if len(body) != 0 && json.Unmarshal(body, &req) != nil {
			http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
			return
		}

		switch req.Method {
		case "initialize":
			mu.Lock()
			initialized++
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", "fault-schema-session")
			writeHTTPRPC(t, w, req.ID, map[string]any{"protocolVersion": protocolVersion})
			return
		case "notifications/initialized":
			mu.Lock()
			notifications++
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			mu.Lock()
			lists++
			attempt := lists
			mu.Unlock()
			if attempt == 1 {
				http.Error(w, "authorization expired", http.StatusForbidden)
				return
			}
			writeHTTPRPC(t, w, req.ID, map[string]any{"tools": []any{
				map[string]any{
					"name": "task-required", "inputSchema": map[string]any{"type": "object"},
					"execution": map[string]any{"taskSupport": "required"},
				},
				map[string]any{
					"name": "report", "inputSchema": map[string]any{"type": "object"},
					"outputSchema": map[string]any{
						"type": "object", "additionalProperties": false,
						"properties": map[string]any{"answer": map[string]any{"type": "string"}},
						"required":   []string{"answer"},
					},
					"execution": map[string]any{"taskSupport": "optional"},
				},
			}})
			return
		case "tools/call":
			mu.Lock()
			callAttempts++
			attempt := callAttempts
			action := req.Params.Arguments.Action
			actions = append(actions, action)
			mu.Unlock()
			if attempt == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"structuredContent":`, *req.ID)
				_, _ = io.WriteString(w, `{not-json}`)
				_, _ = io.WriteString(w, `}}`)
				return
			}
			if attempt == 2 {
				http.Error(w, "authorization expired", http.StatusUnauthorized)
				return
			}
			mu.Lock()
			effects = append(effects, action)
			mu.Unlock()
			writeHTTPRPC(t, w, req.ID, map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "published"}},
				"structuredContent": map[string]any{"answer": "42"},
				"isError":           false,
			})
			return
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client, err := newStreamableHTTPClient(server.URL, map[string]string{"Authorization": "Bearer test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, ErrServer) {
		t.Fatalf("forbidden discovery = %v, want ErrServer", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("explicit discovery retry: %v", err)
	}
	byName := map[string]Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	required, ok := byName["task-required"]
	if !ok || required.TaskSupport != "required" {
		t.Fatalf("required task metadata = %#v", required)
	}
	report, ok := byName["report"]
	if !ok || report.TaskSupport != "optional" || report.OutputSchema == nil ||
		report.OutputSchema["additionalProperties"] != false {
		t.Fatalf("report schema metadata = %#v", report)
	}

	if _, err := client.Call(context.Background(), "report", map[string]any{"action": "malformed"}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("malformed structuredContent = %v, want ErrProtocol", err)
	}
	if _, err := client.Call(context.Background(), "report", map[string]any{"action": "publish"}); !errors.Is(err, ErrServer) {
		t.Fatalf("unauthorized call = %v, want ErrServer", err)
	}
	result, err := client.Call(context.Background(), "report", map[string]any{"action": "publish"})
	if err != nil || result.IsError || result.StructuredContent.(map[string]any)["answer"] != "42" {
		t.Fatalf("explicit retry = %#v, %v", result, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if initialized != 1 || notifications != 1 || lists != 2 {
		t.Fatalf("session requests initialized=%d notifications=%d lists=%d, want auth faults to retain the session", initialized, notifications, lists)
	}
	if len(actions) != 3 || actions[0] != "malformed" || actions[1] != "publish" || actions[2] != "publish" {
		t.Fatalf("transport actions = %v, want one malformed probe and two explicit publish attempts", actions)
	}
	if len(effects) != 1 || effects[0] != "publish" {
		t.Fatalf("external effects = %v, want exactly one explicit success", effects)
	}
}
