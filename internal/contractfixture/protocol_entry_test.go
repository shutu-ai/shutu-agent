package contractfixture_test

// This fixture intentionally drives two external protocol boundaries from the
// same transport-neutral lifecycle record. It does not claim that ACP and MCP
// are the same wire protocol; it checks that they preserve the same session,
// tool, and terminal-turn facts at their public boundaries.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/acp"
	"github.com/jabing/shutu-agent/internal/contractfixture"
	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/sdkclient"
)

func TestCrossEntryProtocolLifecycleFixture(t *testing.T) {
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ACP", func(t *testing.T) {
		prompt := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[%s]}}`, fixture.SessionID, fixture.Prompt[0])
		first := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%q}}`, fixture.Workspace),
		}, "\n") + "\n"
		var output bytes.Buffer
		registered := make(chan struct{})
		server := &acp.Server{
			Factory: protocolACPFactory{fixture: fixture, registered: registered},
			In:      &protocolACPReader{first: first, second: prompt, registered: registered},
			Out:     &output,
		}
		if err := server.Run(context.Background()); err != nil {
			t.Fatalf("ACP Run: %v", err)
		}
		if !strings.Contains(output.String(), fixture.SessionID) ||
			!strings.Contains(output.String(), fixture.Assistant) ||
			!strings.Contains(output.String(), `"stopReason":"end_turn"`) {
			t.Fatalf("ACP lifecycle output = %s", output.String())
		}
	})

	t.Run("MCP stdio", func(t *testing.T) {
		client := mcp.NewStdioClient(os.Args[0], []string{"-test.run=^TestCrossEntryMCPHelper$"})
		if err := client.Start(context.Background()); err != nil {
			t.Fatalf("MCP Start: %v", err)
		}
		defer client.Close()
		listed, err := client.ListTools(context.Background())
		if err != nil {
			t.Fatalf("MCP tools/list: %v", err)
		}
		if len(listed) != 1 || listed[0].Name != fixture.Tool.Name {
			t.Fatalf("MCP tools/list = %#v", listed)
		}
		var arguments map[string]any
		if err := json.Unmarshal(fixture.Tool.Arguments, &arguments); err != nil {
			t.Fatal(err)
		}
		result, err := client.Call(context.Background(), fixture.Tool.Name, arguments)
		if err != nil {
			t.Fatalf("MCP tools/call: %v", err)
		}
		if result.IsError || len(result.Content) != 1 || result.StructuredContent == nil {
			t.Fatalf("MCP tools/call result = %#v", result)
		}
		content, ok := result.Content[0].(map[string]any)
		if !ok || content["text"] != fixture.Tool.Output {
			t.Fatalf("MCP content = %#v", result.Content)
		}
	})

	t.Run("SDK client", func(t *testing.T) {
		client := sdkclient.NewClient(sdkclient.ClientOptions{
			Command:        os.Args[0],
			Args:           []string{"-test.run=^TestCrossEntrySDKHelper$"},
			RequestTimeout: 2 * time.Second,
		})
		defer client.Close()
		initialized, err := client.Initialize(context.Background(), sdkclient.InitializeParams{CWD: fixture.Workspace})
		if err != nil {
			t.Fatalf("SDK initialize: %v", err)
		}
		if initialized.ServerInfo.Name != "cross-entry-sdk" {
			t.Fatalf("SDK server identity = %#v", initialized.ServerInfo)
		}
		subscription := client.Subscribe(func(notification sdkclient.Notification) bool {
			return notification.Method == "session.event"
		})
		defer subscription.Close()
		messageID, err := client.Prompt(context.Background(), fixture.SessionID, []sdkclient.ContentBlock{sdkclient.TextContent("Inspect the shared fixture.")})
		if err != nil || messageID != fixture.MessageID {
			t.Fatalf("SDK prompt = %q, err=%v", messageID, err)
		}
		notification, err := subscription.Next(context.Background())
		if err != nil {
			t.Fatalf("SDK event: %v", err)
		}
		var params struct {
			SessionID string `json:"sessionId"`
			Event     struct {
				Type string `json:"type"`
				Data struct {
					Text string `json:"text"`
				} `json:"data"`
			} `json:"event"`
		}
		if err := json.Unmarshal(notification.Params, &params); err != nil ||
			params.SessionID != fixture.SessionID || params.Event.Type != "assistant/message" ||
			params.Event.Data.Text != fixture.Assistant {
			t.Fatalf("SDK event params = %s", notification.Params)
		}
	})
}

type protocolACPFactory struct {
	fixture    contractfixture.ProtocolLifecycle
	registered chan struct{}
}

func (f protocolACPFactory) NewSession(context.Context, string) (acp.Session, error) {
	return &protocolACPSession{fixture: f.fixture, registered: f.registered}, nil
}

func (f protocolACPFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{}
}

type protocolACPSession struct {
	fixture    contractfixture.ProtocolLifecycle
	registered chan struct{}
}

func (s protocolACPSession) SessionID() string { return s.fixture.SessionID }

func (s protocolACPSession) Prompt(_ context.Context, prompt string, emit func(acp.Update)) (acp.StopReason, error) {
	if !strings.Contains(prompt, "Inspect the shared fixture") {
		return "", fmt.Errorf("unexpected ACP prompt %q", prompt)
	}
	emit(acp.Update{Text: s.fixture.Assistant})
	return acp.StopEndTurn, nil
}

func (s *protocolACPSession) SetPermissionRequester(acp.PermissionRequester) {
	select {
	case <-s.registered:
	default:
		close(s.registered)
	}
}

func (*protocolACPSession) Cancel() error { return nil }
func (*protocolACPSession) Close() error  { return nil }

type protocolACPReader struct {
	first, second string
	registered    <-chan struct{}
	phase         int
}

func (r *protocolACPReader) Read(p []byte) (int, error) {
	switch r.phase {
	case 0:
		r.phase++
		return copy(p, r.first), nil
	case 1:
		<-r.registered
		r.phase++
		return copy(p, r.second), nil
	default:
		return 0, io.EOF
	}
}

// TestCrossEntryMCPHelper is the real child process for the MCP stdio leg.
func TestCrossEntryMCPHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=^TestCrossEntryMCPHelper$") {
		t.Skip("helper process")
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "cross-entry", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": fixture.Tool.Name, "description": fixture.Scenario, "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fixture.Tool.Output}}, "structuredContent": map[string]any{"assistant": fixture.Assistant}, "isError": false}
		default:
			result = map[string]any{}
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestCrossEntrySDKHelper is the child runtime for the SDK client leg.
func TestCrossEntrySDKHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=^TestCrossEntrySDKHelper$") {
		t.Skip("helper process")
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]string{"name": "cross-entry-sdk", "version": "1"}}
		case "session/prompt":
			if err := encoder.Encode(map[string]any{
				"jsonrpc": "2.0", "method": "session.event",
				"params": map[string]any{"sessionId": fixture.SessionID, "event": map[string]any{
					"seq": 1, "type": "assistant/message", "time": 1,
					"data": map[string]string{"text": fixture.Assistant},
				}},
			}); err != nil {
				return
			}
			result = map[string]string{"messageId": fixture.MessageID}
		case "shutdown":
			result = map[string]any{}
		default:
			result = map[string]any{}
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return
		}
		if request.Method == "shutdown" {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
