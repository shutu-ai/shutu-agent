package skill

import (
	"context"
	"testing"

	"github.com/jabing/shutu-agent/internal/runtimectx"
)

func TestSkillToolsRuntimeEmitWinsOverLegacySink(t *testing.T) {
	legacyRuns := 0
	runtimeRuns := 0
	tools := NewSkillTools(nil, 1, func(string, any) { legacyRuns++ })
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "skill-session",
		Emit: func(string, any) error {
			runtimeRuns++
			return nil
		},
	})
	if err := tools.emitContext(ctx, "skill/load", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if runtimeRuns != 1 || legacyRuns != 0 {
		t.Fatalf("runtime=%d legacy=%d, want 1/0", runtimeRuns, legacyRuns)
	}
}
