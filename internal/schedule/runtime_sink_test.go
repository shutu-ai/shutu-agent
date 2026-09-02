package schedule

import (
	"context"
	"testing"

	"github.com/jabing/shutu-agent/internal/runtimectx"
)

func TestScheduleToolsRuntimeEmitWinsOverLegacySink(t *testing.T) {
	legacyRuns := 0
	runtimeRuns := 0
	tools := NewScheduleTools(nil, func(string, any) { legacyRuns++ })
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "schedule-session",
		Emit: func(string, any) error {
			runtimeRuns++
			return nil
		},
	})
	if err := tools.emitContext(ctx, "schedule/create", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if runtimeRuns != 1 || legacyRuns != 0 {
		t.Fatalf("runtime=%d legacy=%d, want 1/0", runtimeRuns, legacyRuns)
	}
}
