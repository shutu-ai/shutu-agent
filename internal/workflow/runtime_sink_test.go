package workflow

import (
	"context"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

func TestWorkflowRuntimeEmitWinsOverLegacySink(t *testing.T) {
	legacyRuns := 0
	runtimeRuns := 0
	tool := NewWorkflowRunTool(nil, func(string, any) { legacyRuns++ })
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "workflow-session",
		Emit: func(string, any) error {
			runtimeRuns++
			return nil
		},
	})
	if err := tool.emitContext(ctx, "workflow/run", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if runtimeRuns != 1 || legacyRuns != 0 {
		t.Fatalf("runtime=%d legacy=%d, want 1/0", runtimeRuns, legacyRuns)
	}
}
