// hooks.go wires the metadata-only event observer into the composition root.
package main

import (
	"fmt"
	"os"
	"time"

	hookrunner "github.com/jabing/shutu-agent/internal/hooks"
)

func (a *app) registerHooks() error {
	if !a.cfg.Hooks.Enabled {
		return nil
	}
	workingDir := a.cfg.Hooks.WorkingDir
	if workingDir == "" {
		workingDir, _ = os.Getwd()
	}
	runner, err := hookrunner.New(hookrunner.Config{
		Command:    a.cfg.Hooks.Command,
		Args:       a.cfg.Hooks.Args,
		Events:     a.cfg.Hooks.Events,
		Timeout:    time.Duration(a.cfg.Hooks.TimeoutMS) * time.Millisecond,
		WorkingDir: workingDir,
	})
	if err != nil {
		return fmt.Errorf("pa: configure hooks: %w", err)
	}
	a.hooks = runner
	return nil
}

func (a *app) closeHooks() {
	if a.hooks != nil {
		_ = a.hooks.Close()
		a.hooks = nil
	}
}
