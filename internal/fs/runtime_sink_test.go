package fs

import (
	"context"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

func TestFsToolsRuntimeEmitWinsOverLegacySink(t *testing.T) {
	legacyRuns := 0
	runtimeRuns := 0
	tools := NewFsTools(nil, func(string, any) { legacyRuns++ })
	tools.SetErrorSink(func(string, any) error {
		legacyRuns++
		return nil
	})
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "fs-session",
		Emit: func(string, any) error {
			runtimeRuns++
			return nil
		},
	})
	if err := tools.emitContext(ctx, "fs/write", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if runtimeRuns != 1 || legacyRuns != 0 {
		t.Fatalf("runtime=%d legacy=%d, want 1/0", runtimeRuns, legacyRuns)
	}
}
