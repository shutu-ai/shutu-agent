// eval.go — the Eval-3b composition-root orchestration (dispatch-eval-3b
// §交付 1 / ADR 2026-08-20-eval-seam.md D-EVAL-2/3/7). This is where the
// task-evaluation capability seam is wired into the REPL: registerEval builds
// the CompositeEvaluator (rule assertions → LLM judge → human fallback,
// D-EVAL-3) over the app's LLM and interact engines, creates the eval Engine,
// registers the three eval_* tools and wires the D3 event sink so eval/run
// lands in the active session log, when eval.enabled (默认关 D10). /eval-status
// reports the seam's configuration and history summary. The loop's turn/step
// structure is untouched (D4): evaluation runs on the serial tool path (D5).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/eval"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
)

// registerEval wires the task-evaluation seam (ADR 2026-08-20-eval-seam.md)
// when eval.enabled (默认关 D10): it builds the CompositeEvaluator (rule
// assertions → LLM judge → human fallback, D-EVAL-3) over the app's LLM and
// interact engines, creates the eval Engine, registers the three eval_* tools,
// and wires the D3 event sink so eval/run lands in the active session log.
// config.applyDefaults already whitelisted the eval_* names when eval.enabled
// was true. The engine is in-memory (no persisted history) and its Close is
// idempotent. The loop's turn/step structure is untouched (D4): evaluation
// runs on the serial tool path.
func (a *app) registerEval() error {
	if !config.Enabled(a.cfg.Eval.Enabled) {
		return nil
	}
	manualFallback := true
	if a.cfg.Eval.ManualFallback != nil {
		manualFallback = *a.cfg.Eval.ManualFallback
	}
	composite := eval.CompositeEvaluator{
		Rule:           eval.RuleEvaluator{},
		LLM:            eval.LLMEvaluator{Judge: a.evalJudge()},
		Manual:         eval.ManualEvaluator{Manual: a.evalManual()},
		ManualFallback: manualFallback,
	}
	eng, err := eval.NewEngine(eval.EngineOpts{Evaluator: composite, MaxRecords: a.cfg.Eval.MaxRecords})
	if err != nil {
		return fmt.Errorf("sta: eval engine: %w", err)
	}
	a.evalEng = eng
	// DSH standard does not expose eval_* as model tools. The evaluator stays
	// available internally for goal/acceptance plumbing, but has no registry
	// entries and therefore cannot enter the model tool catalog.
	return nil
}

// evalJudgeSystemPrompt asks the judge model to return JSON only.
const evalJudgeSystemPrompt = "You are a rigorous evaluator. Given a deliverable and acceptance criteria, judge whether the deliverable satisfies them. Respond with JSON only: {\"verdict\": \"pass\"|\"fail\"|\"manual\", \"reason\": \"one-line justification\"}."

// judgeOutputMax bounds the deliverable head sent to the judge (D-EVAL-3).
const judgeOutputMax = 6000

// evalJudge adapts the app's LLM to the eval seam's JudgeFunc (D-EVAL-3). It
// sends a single non-streaming-style request (no tools ⇒ plain stream) and
// parses the model's JSON verdict, tolerantly mapping unrecognized output to
// manual.
func (a *app) evalJudge() eval.JudgeFunc {
	return func(ctx context.Context, output string, llmCriteria []string) (eval.Verdict, string, error) {
		provider, model := "", ""
		if sessionID := runtimectx.SessionID(ctx); sessionID != "" {
			var err error
			provider, model, err = a.sessionProviderModelStrict(sessionID)
			if err != nil {
				return eval.VerdictManual, "", err
			}
		} else {
			cfg := a.providerConfigSnapshot()
			provider = cfg.LLM.Provider
			model = llmProviderModel(cfg, provider)
		}
		head := runeHead(output, judgeOutputMax)
		user := "Deliverable:\n" + head + "\n\nAcceptance criteria to judge:\n" + strings.Join(llmCriteria, "\n")
		msgs := []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(evalJudgeSystemPrompt)}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(user)}},
		}
		providerLLM := a.llmFor(provider)
		if providerLLM == nil {
			return eval.VerdictManual, "", errors.New("eval: selected LLM is unavailable")
		}
		reader, err := providerLLM.Stream(ctx, llm.ChatRequest{Model: model, Messages: msgs})
		if err != nil {
			return eval.VerdictManual, "", err
		}
		var b strings.Builder
		for {
			ev, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return eval.VerdictManual, "", err
			}
			if ev.Kind == llm.StreamTextDelta {
				b.WriteString(ev.Text)
			}
		}
		var parsed struct {
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		}
		text := strings.TrimSpace(b.String())
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			// Tolerant fallback: scan for a verdict keyword.
			switch {
			case strings.Contains(text, `"fail"`):
				parsed.Verdict = "fail"
			case strings.Contains(text, `"pass"`):
				parsed.Verdict = "pass"
			default:
				parsed.Verdict = "manual"
			}
		}
		switch parsed.Verdict {
		case "pass":
			return eval.VerdictPass, parsed.Reason, nil
		case "fail":
			return eval.VerdictFail, parsed.Reason, nil
		default:
			return eval.VerdictManual, parsed.Reason, nil
		}
	}
}

// runeHead returns the first max runes of s (append "…" when cut).
func runeHead(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// evalManual adapts the app's interact engine to the eval seam's ManualFunc
// (D-EVAL-7): an undecidable evaluation becomes an interact approval request
// (approved→pass, rejected→fail). When interact is disabled the fallback
// reports manual (undecided) rather than failing.
func (a *app) evalManual() eval.ManualFunc {
	return func(ctx context.Context, taskID, output string, manualCriteria []string) (eval.Verdict, string, error) {
		if a.interacts == nil {
			return eval.VerdictManual, "interact disabled: no human fallback", nil
		}
		promptText := "无法自动判定以下交付是否满足验收标准，请人工审批。\n"
		if taskID != "" {
			promptText += "任务：" + taskID + "\n"
		}
		promptText += "验收标准：\n" + strings.Join(manualCriteria, "\n") + "\n交付摘要：\n" + runeHead(output, 2000)
		var req interact.Request
		var err error
		sessionID := runtimectx.SessionID(ctx)
		if sessionID != "" {
			if requester, ok := a.interacts.(interact.SessionRequester); ok {
				req, err = requester.RequestForSession(ctx, sessionID, promptText, "eval_manual", runeHead(output, 2000))
			} else {
				err = errors.New("eval: interact engine lacks session-scoped requests")
			}
		} else {
			req, err = a.interacts.Request(ctx, promptText, "eval_manual", runeHead(output, 2000))
		}
		if err != nil {
			return eval.VerdictManual, "", err
		}
		var res interact.Request
		if sessionID != "" {
			awaiter, ok := a.interacts.(interact.SessionAwaiter)
			if !ok {
				return eval.VerdictManual, "", errors.New("eval: interact engine lacks session-scoped waiting")
			}
			res, err = awaiter.AwaitForSession(ctx, sessionID, req.ID)
		} else {
			res, err = a.interacts.Await(ctx, req.ID)
		}
		if err != nil {
			return eval.VerdictManual, "", err
		}
		switch res.Status {
		case interact.StatusApproved:
			return eval.VerdictPass, "approved by human", nil
		case interact.StatusRejected:
			return eval.VerdictFail, "rejected by human", nil
		default:
			return eval.VerdictManual, "no human decision", nil
		}
	}
}

// evalStatus prints the eval seam configuration and history summary.
func (a *app) evalStatus() error {
	if a.evalEng == nil {
		fmt.Println("eval: disabled (eval.enabled=false)")
		return nil
	}
	recs, err := a.evalEng.List(context.Background())
	if err != nil {
		return err
	}
	manual := true
	if a.cfg.Eval.ManualFallback != nil {
		manual = *a.cfg.Eval.ManualFallback
	}
	fmt.Printf("eval: enabled (records=%d, max_records=%d, manual_fallback=%v)\n",
		len(recs), a.cfg.Eval.MaxRecords, manual)
	return nil
}
