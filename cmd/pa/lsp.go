// lsp.go wires the read-only language-server query seam into the composition
// root. The package owns the stdio protocol and result rendering; pa supplies
// the configured command and the active session's persisted cwd.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/lsp"
)

func (a *app) registerLSP() error {
	if !a.cfg.LSP.Enabled {
		return nil
	}
	if strings.TrimSpace(a.cfg.LSP.Command) == "" {
		return fmt.Errorf("pa: lsp.command is required when lsp.enabled=true")
	}
	root := func() string {
		if a.currentID != "" {
			if meta, err := a.store.GetSessionMeta(context.Background(), a.currentID); err == nil && meta.CWD != "" {
				return meta.CWD
			}
		}
		cwd, _ := os.Getwd()
		return cwd
	}
	tool := lsp.NewTool(lsp.Config{
		Command:          a.cfg.LSP.Command,
		Args:             a.cfg.LSP.Args,
		ExtensionToLang:  a.cfg.LSP.Extensions,
		Timeout:          time.Duration(a.cfg.LSP.TimeoutMS) * time.Millisecond,
		MaxLocations:     a.cfg.LSP.MaxLocations,
		MaxResultChars:   a.cfg.LSP.MaxResultChars,
		MaxDocumentBytes: a.cfg.LSP.MaxDocumentBytes,
	}, root)
	if err := a.reg.Register(tool); err != nil {
		return fmt.Errorf("pa: register %s: %w", tool.Name(), err)
	}
	return nil
}
