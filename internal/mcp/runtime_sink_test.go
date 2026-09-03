package mcp

import (
	"context"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

func TestMcpToolsRuntimeEmitWinsOverLegacySink(t *testing.T) {
	legacyRuns := 0
	runtimeRuns := 0
	tools := NewMcpTools(nil, nil, func(string, any) { legacyRuns++ })
	tools.SetErrorSink(func(string, any) error {
		legacyRuns++
		return nil
	})
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "mcp-session",
		Emit: func(string, any) error {
			runtimeRuns++
			return nil
		},
	})
	if err := tools.emitContext(ctx, "mcp/call", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if runtimeRuns != 1 || legacyRuns != 0 {
		t.Fatalf("runtime=%d legacy=%d, want 1/0", runtimeRuns, legacyRuns)
	}
}
