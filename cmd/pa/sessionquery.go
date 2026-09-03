// sessionquery.go wires the read-only session-query seam into the composition
// root. The package owns all model-facing schemas and formatting; cmd/pa only
// supplies the durable store and current-session identity (D2/D4).
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/sessionquery"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func (a *app) registerSessionQuery() error {
	if !a.cfg.SessionQuery.Enabled {
		return nil
	}
	query := sessionquery.NewToolsWithConfigContext(a.store, func(ctx context.Context) string {
		return runtimectx.SessionID(ctx)
	}, a.cfg.SessionQuery.MaxResults, time.Duration(a.cfg.SessionQuery.SearchTimeoutMS)*time.Millisecond)
	for _, tool := range []tools.Tool{query.Search(), query.EventSearch(), query.Trace(), query.EventTrace(), query.Read()} {
		if err := a.reg.Register(tool); err != nil {
			return fmt.Errorf("sta: register session-query tool: %w", err)
		}
	}
	return nil
}
