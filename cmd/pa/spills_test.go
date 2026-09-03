package main

import (
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestRegisterSpillsKeepsMemoryInternalOnly(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		a := &app{
			cfg: config.Config{Spill: config.SpillConfig{Enabled: config.Bool(enabled)}},
			reg: tools.New(),
			log: session.New(),
		}
		if err := a.registerSpills(); err != nil {
			t.Fatalf("registerSpills(%v): %v", enabled, err)
		}
		for _, spec := range a.reg.Specs() {
			if len(spec.Name) >= 6 && spec.Name[:6] == "spill_" {
				t.Fatalf("spill tool %q must not be model-facing", spec.Name)
			}
		}
		if a.spills != nil {
			a.spills.Close()
		}
	}
}
