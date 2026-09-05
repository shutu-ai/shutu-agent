package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestServerInitializeAndCallTool(t *testing.T) {
	manifest := Manifest{
		ID: "demo", Name: "Demo", Version: "0.1.0", ExtensionAPI: "1.0",
		Capabilities: Capabilities{Tools: true, Health: true},
		Transport:    Transport{Type: "stdio", Command: "demo"},
		Tools: ToolsContribution{Definitions: []ToolDefinition{{
			Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}, Risk: ToolRiskRead,
		}}},
	}
	server := NewServer(ServerCallbacks{
		Manifest: manifest,
		Health:   func(context.Context) (HealthResult, error) { return HealthResult{Ready: true}, nil },
		CallTool: func(_ context.Context, request ToolCallRequest) (ToolCallResult, error) {
			return ToolCallResult{Value: request.Arguments["text"]}, nil
		},
	})
	var out bytes.Buffer
	request := RPCRequest{JSONRPC: "2.0", ID: 7, Method: MethodInitialize, Params: InitializeRequest{ProtocolVersion: ProtocolVersion, AgentAPIVersion: APIVersion}}
	encoded, _ := json.Marshal(request)
	if err := server.HandleLine(context.Background(), append(encoded, '\n'), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"protocolVersion":"shutu-extension/1"`) {
		t.Fatalf("initialize response = %s", out.String())
	}
	out.Reset()
	request = RPCRequest{JSONRPC: "2.0", ID: 8, Method: MethodCallTool, Params: ToolCallRequest{Name: "echo", Arguments: map[string]any{"text": "ok"}}}
	encoded, _ = json.Marshal(request)
	if err := server.HandleLine(context.Background(), append(encoded, '\n'), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"value":"ok"`) {
		t.Fatalf("tool response = %s", out.String())
	}
}
