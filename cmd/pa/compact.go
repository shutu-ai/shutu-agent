// compact.go — the M5c-2b composition-root orchestration (dispatch-m5c-2b
// §1/§2/§3). This is where the context-compaction capability seam is wired into
// the REPL: registerCompaction creates the BasicEngine when compaction.enabled
// (D10), /compact (+ /compact region) are the manual command, and the loop's
// "compaction" pre-step injector runs the token-pressure auto-compaction. The
// compaction/start → compaction/summary → compaction/end observation events are
// appended to the active session log on the serial path — the command handler
// or the pre-step injector — never from a background goroutine (D5), mirroring
// the job/subagent onEvent sink pattern (D3). The log stays append-only (D1):
// the engine appends the summary as a surfaceOp.replace user/message (M5c-1a)
// and these events only record that fact; nothing is ever physically deleted.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jabing/shutu-agent/internal/compaction"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/loop"
	"github.com/jabing/shutu-agent/internal/session"
)

// compactedNotice is the short "context was compacted" hint injected by the
// auto-compaction pre-step injector. It is deliberately not the summary body —
// the folded history already carries the summary as a user/message
// (surfaceOp.replace, M5c-1a) — and the injection budget is bounded by the loop
// (per-injector 4000 rune cap, fail-open).
const compactedNotice = "Context notice: earlier conversation context was auto-compacted to free space. Continue from the summary message above and the retained recent turns."

// compactionEstimator estimates the model-visible surface tokens of a log. It
// is injectable so tests can drive pressure deterministically; the default
// mirrors BasicEngine's zero-dependency estimate (len/4 per string), keeping
// the wiring's pressure pre-check consistent with the engine's own gate.
type compactionEstimator func(log *session.Log) int

// defaultCompactionEstimator mirrors compaction.defaultTokenEstimate over the
// derived history (content + tool-call name/arguments), exactly like
// BasicEngine.estimateTokens.
func defaultCompactionEstimator(log *session.Log) int {
	total := 0
	for _, m := range log.DeriveHistory() {
		total += len(m.Text()) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Name)/4 + len(tc.Arguments)/4
		}
	}
	return total
}

// registerCompaction creates the default BasicEngine when compaction.enabled,
// and wires nothing when disabled (D10, mirrors registerJobs/registerSubagent).
// Unlike kb/jobs/subagent there are no consumer tools to register or whitelist
// (compaction has none): automatic triggering runs through the loop pre-step
// injector, manual through the /compact command. The engine holds no closable
// resources (it shares the caller-owned LLM), so there is no deferred Close.
func (a *app) registerCompaction() error {
	if !config.Enabled(a.cfg.Compaction.Enabled) {
		return nil
	}
	a.compaction = compaction.NewBasic(compaction.BasicOpts{
		LLM:                   a.currentLLM(),
		Model:                 llmProviderModel(a.cfg, a.cfg.LLM.Provider),
		TokenThreshold:        a.cfg.Compaction.TokenThreshold,
		RetainTurns:           a.cfg.Compaction.RetainTurns,
		RetainTokens:          a.cfg.Compaction.RetainTokens,
		FrameSummary:          true,
		RequireSmallerSummary: true,
	})
	return nil
}

// preStepInjectors returns the loop's registered pre-step injectors for the
// current configuration: the "compaction" injector when the capability is
// enabled, then the "skill" catalog injector when skill is enabled. The loop
// runs the M4b Recall hook ("recall") first and then the PreStep injectors in
// order, so the compaction injector lands after recall and the skill catalog
// after compaction as required (dispatch-m5c-2 §4 / dispatch-m5d-2 §4). The
// turn/step structure is unchanged (D4).
func (a *app) preStepInjectors() []loop.PreStepInjector {
	var injectors []loop.PreStepInjector
	if a.compaction != nil {
		injectors = append(injectors, a.compactionInjector(defaultCompactionEstimator))
	}
	// M5d-2: the "skill" catalog injector is appended after compaction so the
	// bounded skill catalog (re-read each turn, no file watching) reaches the
	// model's first request whenever skill is enabled (D10-gated here and by
	// registerSkills).
	if a.skills != nil {
		injectors = append(injectors, a.skillCatalogInjector())
	}
	// Human skill references are resolved after the catalog so the first request
	// carries both discovery and the selected <skill_content> body. This keeps
	// the original /skill-name text in history while matching dsh's host
	// pre-step behavior.
	if a.skills != nil {
		injectors = append(injectors, a.skillInvocationInjector())
	}
	// M6a-2: the "schedule" injector is appended after skill (ADR 决策 M6a /
	// dispatch-m6a-2 §4) so the serial schedule-clock advance — turning due
	// triggers into schedule/fire events and fired jobs — runs after the skill
	// catalog on every turn. It contributes no context message (schedule/fire
	// is log-only); the ordering is recall → compaction → skill → skill-invocation → schedule.
	if a.schedules != nil {
		injectors = append(injectors, a.scheduleInjector())
	}
	return injectors
}

// compactionInjector builds the "compaction" pre-step injector (ADR 决策 ③ /
// dispatch-m5c-2b §2): once per turn — after user/message is appended, before
// the first step's model request — it estimates the current surface tokens and,
// when the estimate exceeds the configured threshold, triggers CompactIfNeeded
// under TriggerPressure and appends the compaction/start → compaction/summary →
// compaction/end observation events (D3). The injected context is a short
// notice, not the summary body — the folded history already carries the summary
// marker (M5c-1a). Every append happens here on the serial pre-step path (D5);
// a failing compaction is surfaced as a stderr warning and contributes no
// context (fail-open, the same contract as the kb recall injector).
func (a *app) compactionInjector(est compactionEstimator) loop.PreStepInjector {
	return loop.PreStepInjector{
		Name:        "compaction",
		Inject:      a.compactionPreStep(est),
		OncePerTurn: true,
	}
}

func (a *app) compactionPreStep(est compactionEstimator) func(context.Context, string) []llm.Message {
	return func(ctx context.Context, userText string) []llm.Message {
		if a.compaction == nil {
			return nil
		}
		if !a.overPressure(est) {
			return nil
		}
		// dsh gives pressure compaction one follow-up attempt when the first
		// summary did not bring the surface below the pressure threshold.
		var res *compaction.Result
		for attempt := 0; attempt < 2 && a.overPressure(est); attempt++ {
			result, err := a.compactAndLog(ctx, "surface token estimate exceeded threshold", "pressure",
				func() (*compaction.Result, error) {
					return a.compaction.CompactIfNeeded(ctx, a.log, compaction.TriggerPressure)
				})
			if err != nil {
				fmt.Fprintln(os.Stderr, "[compaction failed open]", err)
				return nil
			}
			if result == nil {
				break
			}
			res = result
		}
		if res == nil {
			return nil
		}
		return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(compactedNotice)}}}
	}
}

// recoverContextOverflow performs the dsh-style forced compaction retry for a
// provider context-window rejection. It runs on the loop's serial step path,
// so the retry observes the newly appended checkpoint marker immediately.
func (a *app) recoverContextOverflow(ctx context.Context) bool {
	if a.compaction == nil || a.log == nil {
		return false
	}
	res, err := a.compactAndLog(ctx, "provider rejected the request because the context window is full", "context-overflow",
		func() (*compaction.Result, error) {
			return a.compaction.CompactIfNeeded(ctx, a.log, compaction.TriggerContextOverflow)
		})
	return err == nil && res != nil
}

// overPressure reports whether the current surface token estimate exceeds the
// configured pressure threshold — the same gate BasicEngine applies under
// TriggerPressure, checked up front so a turn that does not need compaction
// never logs a compaction/start. Thresholds are clamped to the config default
// so a raw config (tests) never disables the gate accidentally.
func (a *app) overPressure(est compactionEstimator) bool {
	threshold := a.cfg.Compaction.TokenThreshold
	if threshold <= 0 {
		threshold = config.DefaultCompactionTokenThreshold
	}
	return threshold > 0 && est(a.log) > threshold
}

// compactAndLog runs one compaction attempt through the engine and appends the
// compaction/* observation events (D3) on the serial path: compaction/start is
// logged before the engine call (the attempt marker, with its reason/trigger),
// then compaction/summary + compaction/end when the attempt produced a result
// (bounded summary, shadowed range, tokens saved). A nil result (nothing
// foldable) or an engine error leaves only the start — the ADR's "orphan start
// reveals an interrupted/no-op attempt" signal. Event append failures are
// surfaced as stderr warnings and never block the attempt (fail-open, same as
// the job/subagent onEvent sinks).
func (a *app) compactAndLog(ctx context.Context, reason, trigger string, run func() (*compaction.Result, error)) (*compaction.Result, error) {
	if _, err := a.log.Append(session.EventCompactionStart, session.NewCompactionStart(reason, trigger)); err != nil {
		fmt.Fprintln(os.Stderr, "pa: compaction/start event:", err)
	}
	res, err := run()
	if err != nil {
		if _, appendErr := a.log.Append(session.EventCompactionEnd, session.NewCompactionEndError("", err.Error())); appendErr != nil {
			fmt.Fprintln(os.Stderr, "pa: compaction/end error event:", appendErr)
		}
		return res, err
	}
	if res == nil {
		return res, err
	}
	if _, err := a.log.Append(session.EventCompactionSummary,
		session.NewCompactionSummaryWithStats(res.CompactionID, res.Summary, res.ShadowedSeqs, res.ShadowedTokens, trigger)); err != nil {
		fmt.Fprintln(os.Stderr, "pa: compaction/summary event:", err)
	}
	if _, err := a.log.Append(session.EventCompactionEnd, session.NewCompactionEnd(res.CompactionID, res.ShadowedRange, res.ShadowedTokens)); err != nil {
		fmt.Fprintln(os.Stderr, "pa: compaction/end event:", err)
	}
	return res, nil
}

// compactCommand handles the /compact and /compact region <start> <end>
// commands (dispatch-m5c-2b §1). It reports the capability as unavailable when
// compaction is disabled (D10); otherwise it performs one manual compaction on
// the serial command path and prints the summary, the shadowed surface range
// and the tokens saved. The compaction/* observation events are appended by
// compactAndLog — the command itself adds no extra event type.
func (a *app) compactCommand(ctx context.Context, args []string) error {
	if a.compaction == nil {
		fmt.Println("compaction: disabled (compaction.enabled=false)")
		return nil
	}
	var res *compaction.Result
	var err error
	switch {
	case len(args) == 3 && args[0] == "region":
		start, e1 := strconv.ParseInt(args[1], 10, 64)
		end, e2 := strconv.ParseInt(args[2], 10, 64)
		if e1 != nil || e2 != nil {
			return fmt.Errorf("usage: /compact region <start> <end> (integer event seqs)")
		}
		res, err = a.compactAndLog(ctx, "manual /compact region command", "manual",
			func() (*compaction.Result, error) { return a.compaction.CompactRegion(ctx, a.log, start, end) })
	case len(args) != 0:
		return fmt.Errorf("usage: /compact or /compact region <start> <end>")
	default:
		res, err = a.compactAndLog(ctx, "manual /compact command", "manual",
			func() (*compaction.Result, error) { return a.compaction.CompactNow(ctx, a.log) })
	}
	if err != nil {
		return err
	}
	if res == nil {
		fmt.Println("compaction: nothing to compact")
		return nil
	}
	fmt.Printf("compacted %d events (seq %d..%d), saved %d tokens (id %s)\nsummary: %s\n",
		len(res.ShadowedSeqs), res.ShadowedRange[0], res.ShadowedRange[1], res.ShadowedTokens, res.CompactionID, res.Summary)
	return nil
}
