// sessionquery.go wires the read-only session-query seam into the composition
// root. The package owns all model-facing schemas and formatting; cmd/pa only
// supplies the durable store and current-session identity (D2/D4).
package main

import (
	"fmt"

	"github.com/jabing/shutu-agent/internal/sessionquery"
	"github.com/jabing/shutu-agent/internal/tools"
)

func (a *app) registerSessionQuery() error {
	if !a.cfg.SessionQuery.Enabled {
		return nil
	}
	query := sessionquery.NewTools(a.store, func() string { return a.currentID }, a.cfg.SessionQuery.MaxResults)
	for _, tool := range []tools.Tool{query.Search(), query.EventSearch(), query.Trace(), query.EventTrace(), query.Read()} {
		if err := a.reg.Register(tool); err != nil {
			return fmt.Errorf("pa: register session-query tool: %w", err)
		}
	}
	return nil
}
