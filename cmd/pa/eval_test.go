package main

import (
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func makeEvalApp(enabled bool) *app {
	return &app{
		cfg: config.Config{Eval: config.EvalConfig{Enabled: config.Bool(enabled)}},
		reg: tools.New(),
	}
}

func TestRegisterEvalKeepsEvaluatorInternalOnly(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		a := makeEvalApp(enabled)
		if err := a.registerEval(); err != nil {
			t.Fatalf("registerEval(%v): %v", enabled, err)
		}
		if !enabled && a.evalEng != nil {
			t.Fatal("disabled eval must not create an evaluator")
		}
		for _, spec := range a.reg.Specs() {
			if len(spec.Name) >= 5 && spec.Name[:5] == "eval_" {
				t.Fatalf("eval tool %q must not be model-facing", spec.Name)
			}
		}
		if a.evalEng != nil {
			a.evalEng.Close()
		}
	}
}
