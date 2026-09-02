package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/contractfixture"
	"github.com/jabing/shutu-agent/internal/mcp"
)

// TestExternalMCPHelper is launched by TestExternalMCPClientContract through
// the exported NewStdioClient API. Keeping the fake server in this external
// package ensures the contract does not depend on mcp's unexported test hooks.
func TestExternalMCPHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=^TestExternalMCPHelper$") {
		t.Skip("helper process")
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "external-contract", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []any{map[string]any{
					"name": fixture.Tool.Name, "description": "shared fixture tool",
					"inputSchema": map[string]any{"type": "object"},
					"execution":   map[string]any{"taskSupport": "optional"},
				}},
			}
		case "tools/call":
			result = map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": fixture.Tool.Output}},
				"structuredContent": map[string]any{"scenario": fixture.Scenario},
				"isError":           false,
			}
		default:
			result = map[string]any{}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result})
	}
}

func TestExternalMCPClientContract(t *testing.T) {
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewStdioClient(os.Args[0], []string{"-test.run=^TestExternalMCPHelper$"})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != fixture.Tool.Name || tools[0].TaskSupport != "optional" {
		t.Fatalf("ListTools = %#v, want exported tool metadata", tools)
	}
	var arguments map[string]any
	if err := json.Unmarshal(fixture.Tool.Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	result, err := client.Call(context.Background(), fixture.Tool.Name, arguments)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	content, _ := result.Content[0].(map[string]any)
	structured, _ := result.StructuredContent.(map[string]any)
	if result.IsError || len(result.Content) != 1 || content["text"] != fixture.Tool.Output ||
		structured["scenario"] != fixture.Scenario || !result.StructuredContentSet {
		t.Fatalf("Call result = %#v, want content and structuredContent", result)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := client.ListTools(context.Background()); !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("ListTools after Close = %v, want mcp.ErrClosed", err)
	}
}
