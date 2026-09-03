package subagent

import (
	"context"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

func TestSubagentRuntimeEmitWinsOverLegacySink(t *testing.T) {
	legacyRuns := 0
	runtimeRuns := 0
	tools := NewSubagentToolsWithContinuable(nil, 1, func() string { return "legacy" }, func(string, any) {
		legacyRuns++
	}, true)
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "subagent-session",
		Emit: func(string, any) error {
			runtimeRuns++
			return nil
		},
	})
	if err := tools.emitContext(ctx, "subagent/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if runtimeRuns != 1 || legacyRuns != 0 {
		t.Fatalf("runtime=%d legacy=%d, want 1/0", runtimeRuns, legacyRuns)
	}
}
