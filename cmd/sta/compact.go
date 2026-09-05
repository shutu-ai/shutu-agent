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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/shutu-ai/shutu-agent/internal/compaction"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/session"
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

// defaultCompactionEstimator mirrors the zero-dependency estimator over the
// model-visible surface, but does so without calling DeriveHistory. Replaying a
// restored 100k-event session through the surface folder is much more work than
// the pressure gate needs and used to make the first pre-step unresponsive.
func defaultCompactionEstimator(log *session.Log) int {
	if log == nil {
		return 0
	}
	events := log.Events()
	replacements := make([]surfaceEstimateRange, 0)
	for _, event := range events {
		if event.Type != session.EventUserMessage {
			continue
		}
		var data struct {
			SurfaceOp *struct {
				Op    string `json:"op"`
				Start int64  `json:"start"`
				End   int64  `json:"end"`
			} `json:"surfaceOp,omitempty"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.SurfaceOp != nil &&
			data.SurfaceOp.Op == "replace" && data.SurfaceOp.Start >= 0 && data.SurfaceOp.End >= data.SurfaceOp.Start {
			replacements = append(replacements, surfaceEstimateRange{start: uint64(data.SurfaceOp.Start), end: uint64(data.SurfaceOp.End)})
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	replacements = mergeSurfaceEstimateRanges(replacements)
	total := 0
	for _, event := range events {
		if surfaceEstimateShadowed(replacements, event.Seq) {
			continue
		}
		total += estimateSurfaceEvent(event)
	}
	return total
}

type surfaceEstimateRange struct{ start, end uint64 }

func mergeSurfaceEstimateRanges(ranges []surfaceEstimateRange) []surfaceEstimateRange {
	if len(ranges) < 2 {
		return ranges
	}
	out := ranges[:1]
	for _, next := range ranges[1:] {
		last := &out[len(out)-1]
		if next.start <= last.end+1 {
			if next.end > last.end {
				last.end = next.end
			}
			continue
		}
		out = append(out, next)
	}
	return out
}

func surfaceEstimateShadowed(ranges []surfaceEstimateRange, seq uint64) bool {
	index := sort.Search(len(ranges), func(index int) bool { return ranges[index].end >= seq })
	return index < len(ranges) && ranges[index].start <= seq
}

func estimateSurfaceEvent(event session.Event) int {
	var total int
	add := func(text string) { total += len(text) / 4 }
	textBlocks := func(blocks []llm.ContentBlock) {
		for _, block := range blocks {
			if block.Kind == llm.BlockText || block.Kind == llm.BlockReasoning {
				add(block.Text)
			}
		}
	}
	switch event.Type {
	case session.EventUserMessage:
		var data struct {
			Text    string             `json:"text"`
			Content []llm.ContentBlock `json:"content,omitempty"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			if len(data.Content) > 0 {
				textBlocks(data.Content)
			} else {
				add(data.Text)
			}
		}
	case session.EventAssistantMessage:
		var data struct {
			Text      string         `json:"text"`
			Reasoning string         `json:"reasoning,omitempty"`
			ToolCalls []llm.ToolCall `json:"toolCalls,omitempty"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			add(data.Text)
			add(data.Reasoning)
			for _, call := range data.ToolCalls {
				add(call.Name)
				add(call.Arguments)
			}
		}
	case session.EventToolResult:
		var data struct {
			Output  string             `json:"output,omitempty"`
			Content []llm.ContentBlock `json:"content,omitempty"`
			Message *struct {
				Content []struct {
					Text string `json:"text,omitempty"`
				} `json:"content,omitempty"`
			} `json:"message,omitempty"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			if len(data.Content) > 0 {
				textBlocks(data.Content)
			} else if data.Message != nil {
				for _, block := range data.Message.Content {
					add(block.Text)
				}
			} else {
				add(data.Output)
			}
		}
	case "tool/error":
		var data struct {
			Error string `json:"error,omitempty"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			add(data.Error)
		}
	}
	return total
}

// registerCompaction creates the default BasicEngine when compaction.enabled,
// and wires nothing when disabled (D10, mirrors registerJobs/registerSubagent).
// Unlike jobs/subagent there are no consumer tools to register or whitelist
// (compaction has none): automatic triggering runs through the loop pre-step
// injector, manual through the /compact command. The engine holds no closable
// resources (it shares the caller-owned LLM), so there is no deferred Close.
func (a *app) registerCompaction() error {
	cfg := a.providerConfigSnapshot()
	if !config.Enabled(cfg.Compaction.Enabled) {
		return nil
	}
	a.compactionMu.Lock()
	a.compactionEngines = nil
	a.compactionMu.Unlock()
	a.compaction = compaction.NewBasic(compaction.BasicOpts{
		LLM:                   a.currentLLM(),
		Meter:                 a.compactionSurfaceMeter(a.currentID),
		Model:                 llmProviderModel(cfg, cfg.LLM.Provider),
		TokenThreshold:        cfg.Compaction.TokenThreshold,
		RetainTurns:           cfg.Compaction.RetainTurns,
		RetainTokens:          cfg.Compaction.RetainTokens,
		SummaryInputTokens:    cfg.Compaction.SummaryInputTokens,
		FrameSummary:          true,
		RequireSmallerSummary: true,
	})
	a.compactionMu.Lock()
	if a.compactionEngines == nil {
		a.compactionEngines = make(map[string]compaction.Engine)
	}
	a.compactionMu.Unlock()
	return nil
}

// compactionEngineFor returns the compaction projection owned by a runtime
// session. The legacy engine remains the fallback for direct CLI and tests
// without a runtime session. BasicEngine is deliberately cheap and has no
// external resources, so each session gets its own provider/model selection
// and compaction counter rather than sharing application-global state.
func (a *app) compactionEngineFor(ctx context.Context, sessionID string) compaction.Engine {
	if a.compaction == nil {
		return nil
	}
	if sessionID == "" || (a.agentRegistry == nil && sessionID == a.currentID) {
		return a.compaction
	}
	a.compactionMu.Lock()
	if existing := a.compactionEngines[sessionID]; existing != nil {
		a.compactionMu.Unlock()
		return existing
	}
	a.compactionMu.Unlock()
	provider, model, err := a.sessionProviderModelStrict(sessionID)
	if err != nil {
		// Keep compaction fail-closed on durable session-config failures. An
		// unavailable adapter causes the compaction request to report its error;
		// it must not summarize with another session's global provider.
		provider, model = "__session_config_unavailable__", ""
	}
	cfg := a.providerConfigSnapshot()
	engine := compaction.NewBasic(compaction.BasicOpts{
		LLM: a.llmFor(provider), Model: model,
		Meter:              a.compactionSurfaceMeter(sessionID),
		TokenThreshold:     cfg.Compaction.TokenThreshold,
		RetainTurns:        cfg.Compaction.RetainTurns,
		RetainTokens:       cfg.Compaction.RetainTokens,
		SummaryInputTokens: cfg.Compaction.SummaryInputTokens,
		FrameSummary:       true, RequireSmallerSummary: true,
	})
	a.compactionMu.Lock()
	if existing := a.compactionEngines[sessionID]; existing != nil {
		a.compactionMu.Unlock()
		return existing
	}
	if a.compactionEngines == nil {
		a.compactionEngines = make(map[string]compaction.Engine)
	}
	a.compactionEngines[sessionID] = engine
	a.compactionMu.Unlock()
	return engine
}

// compactionSurfaceMeter adapts the application-wide usage meter to the
// compaction package's provider-neutral seam. Both pressure admission and
// retainTokens selection now consume the same replacement-folded node prices
// that telemetry exposes; a nil meter deliberately leaves standalone/test
// engines on their injected estimator path.
func (a *app) compactionSurfaceMeter(sessionID string) compaction.SurfaceMeter {
	if a.usageMeter == nil {
		return nil
	}
	return func(value compaction.SessionLike) compaction.SurfaceMeasurement {
		log, ok := value.(*session.Log)
		if !ok || log == nil {
			return compaction.SurfaceMeasurement{TotalTokens: -1}
		}
		measurement := a.usageMeter.Measure(sessionID, log, nil)
		nodes := make([]compaction.SurfaceNode, 0, len(measurement.Nodes))
		for _, node := range measurement.Nodes {
			nodes = append(nodes, compaction.SurfaceNode{Seq: node.Seq, Tokens: node.Tokens})
		}
		return compaction.SurfaceMeasurement{
			LogRevision:             measurement.LogRevision,
			BaselineEstimatedTokens: measurement.BaselineEstimatedTokens,
			BaselineUsageTokens:     measurement.BaselineUsageTokens,
			SurfaceDeltaTokens:      measurement.SurfaceDeltaTokens,
			TotalTokens:             measurement.TotalTokens,
			SurfaceTokens:           measurement.SurfaceTokens,
			Nodes:                   nodes,
		}
	}
}

func (a *app) closeCompactionEngines() {
	a.compactionMu.Lock()
	a.compactionEngines = nil
	a.compactionMu.Unlock()
}

// preStepInjectors returns the loop's registered pre-step injectors for the
// current configuration: the "compaction" injector when the capability is
// enabled, then the "skill" catalog injector when skill is enabled. The loop
// runs the registered PreStep injectors in order, so the compaction injector
// lands before the skill catalog
// after compaction as required (dispatch-m5c-2 §4 / dispatch-m5d-2 §4). The
// turn/step structure is unchanged (D4).
func (a *app) preStepInjectors() []loop.PreStepInjector {
	return a.preStepInjectorsFor(a.log)
}

func (a *app) preStepInjectorsFor(log *session.Log) []loop.PreStepInjector {
	return a.preStepInjectorsForSession(a.currentID, log)
}

func (a *app) preStepInjectorsForSession(sessionID string, log *session.Log) []loop.PreStepInjector {
	var injectors []loop.PreStepInjector
	if a.compaction != nil {
		est := defaultCompactionEstimator
		if a.usageMeter != nil && sessionID != "" {
			// The pressure gate runs before the next request is assembled. The
			// replay-aware meter still knows the durable header and the latest
			// provider usage anchor, so use its full total rather than only the
			// surface. This keeps the admission gate and compaction engine on the
			// same replacement-aware accounting source.
			est = func(current *session.Log) int {
				return a.usageMeter.Measure(sessionID, current, nil).TotalTokens
			}
		}
		injectors = append(injectors, a.compactionInjectorFor(sessionID, est, log))
	}
	// AGENTS.md-compatible instructions are the workspace instruction baseline.
	// Keep them immediately after compaction and before runtime/skill context,
	// matching DSH's instruction-then-runtime ordering.
	if config.Enabled(a.cfg.AgentInstructions.Enabled) {
		injectors = append(injectors, a.agentInstructionsInjectorFor(sessionID, log))
	}
	// M5d-2: the "skill" catalog injector is appended after compaction so the
	// bounded skill catalog (re-read each turn, no file watching) reaches the
	// model's first request whenever skill is enabled (D10-gated here and by
	// registerSkills).
	if a.skills != nil {
		injectors = append(injectors, a.skillCatalogInjectorFor(log))
	}
	// Human skill references are resolved after the catalog so the first request
	// carries both discovery and the selected <skill_content> body. This keeps
	// the original /skill-name text in history while matching dsh's host
	// pre-step behavior.
	if a.skills != nil {
		injectors = append(injectors, a.skillInvocationInjectorFor(log))
	}
	// M6a-2: the "schedule" injector is appended after skill (ADR 决策 M6a /
	// dispatch-m6a-2 §4) so the serial schedule-clock advance — turning due
	// triggers into schedule/fire events and fired jobs — runs after the skill
	// catalog on every turn. It contributes no context message (schedule/fire
	// is log-only); the ordering is recall → compaction → skill → skill-invocation → schedule.
	// Durable scheduling has no legacy a.schedules engine: each addressed
	// session lazily owns a scheduler in goalSchedulers. Keep the injector on
	// Agent-owned child sessions as well as the old in-memory path.
	if a.schedules != nil || a.goalScheduler != nil || a.scheduleWake != nil {
		injectors = append(injectors, a.scheduleInjectorFor(log))
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
// durable lifecycle failure stops the model request instead of allowing the
// in-memory compaction result to diverge from the session transcript.
func (a *app) compactionInjector(est compactionEstimator) loop.PreStepInjector {
	return a.compactionInjectorFor(a.currentID, est, a.log)
}

func (a *app) compactionInjectorFor(sessionID string, est compactionEstimator, log *session.Log) loop.PreStepInjector {
	return loop.PreStepInjector{
		Name:            "compaction",
		Inject:          a.compactionPreStepFor(sessionID, est, log),
		InjectWithError: a.compactionPreStepForWithError(sessionID, est, log),
		OncePerTurn:     true,
	}
}

func (a *app) compactionPreStep(est compactionEstimator) func(context.Context, string) []llm.Message {
	return a.compactionPreStepFor(a.currentID, est, a.log)
}

func (a *app) compactionPreStepFor(sessionID string, est compactionEstimator, log *session.Log) func(context.Context, string) []llm.Message {
	return func(ctx context.Context, userText string) []llm.Message {
		messages, _ := a.compactionPreStepForWithError(sessionID, est, log)(ctx, userText)
		return messages
	}
}

func (a *app) compactionPreStepForWithError(sessionID string, est compactionEstimator, log *session.Log) func(context.Context, string) ([]llm.Message, error) {
	return func(ctx context.Context, userText string) ([]llm.Message, error) {
		engine := a.compactionEngineFor(ctx, sessionID)
		if engine == nil {
			return nil, nil
		}
		over, ok := a.overPressureContextFor(ctx, est, log)
		if !ok || !over {
			return nil, nil
		}
		// A pressure pre-step owns one compaction attempt. Retrying here without
		// observing a new replacement generation can repeatedly summarize the
		// retained tail (and, with a pending user claim, create an extra model
		// request). Canonical request-error recovery is the separate retry seam.
		res, err := a.compactAndLogOn(ctx, log, "surface token estimate exceeded threshold", "pressure",
			func() (*compaction.Result, error) {
				// The pressure gate has already derived the surface above. Use the
				// unconditional path so BasicEngine does not derive the same
				// 100k-event history a second time before it can observe cancel.
				return engine.CompactNow(ctx, log)
			})
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, nil
		}
		return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(compactedNotice)}}}, nil
	}
}

// recoverContextOverflow performs the dsh-style forced compaction retry for a
// provider context-window rejection. It runs on the loop's serial step path,
// so the retry observes the newly appended checkpoint marker immediately.
func (a *app) recoverContextOverflow(ctx context.Context) bool {
	return a.recoverContextOverflowFor(ctx, a.log)
}

func (a *app) recoverContextOverflowFor(ctx context.Context, log *session.Log) bool {
	if log == nil {
		return false
	}
	engine := a.compactionEngineFor(ctx, a.runtimeSessionID(ctx))
	if engine == nil {
		return false
	}
	res, err := a.compactAndLogOn(ctx, log, "provider rejected the request because the context window is full", "context-overflow",
		func() (*compaction.Result, error) {
			return engine.CompactIfNeeded(ctx, log, compaction.TriggerContextOverflow)
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

// overPressureContext runs the potentially expensive production-history
// estimator behind a cancellation boundary. A large restored session can make
// DeriveHistory take noticeable time; a canceled turn must not wait for that
// read before the loop can append its canceled lifecycle tail. The estimator
// only reads a snapshot, so letting it finish in the background is safe and the
// buffered result prevents a goroutine leak once it returns.
func (a *app) overPressureContext(ctx context.Context, est compactionEstimator) (bool, bool) {
	return a.overPressureContextFor(ctx, est, a.log)
}

func (a *app) overPressureContextFor(ctx context.Context, est compactionEstimator, log *session.Log) (bool, bool) {
	if err := ctx.Err(); err != nil {
		return false, false
	}
	done := make(chan int, 1)
	go func() { done <- est(log) }()
	select {
	case <-ctx.Done():
		return false, false
	case tokens := <-done:
		threshold := a.cfg.Compaction.TokenThreshold
		if threshold <= 0 {
			threshold = config.DefaultCompactionTokenThreshold
		}
		return threshold > 0 && tokens > threshold, true
	}
}

// compactAndLog runs one compaction attempt through the engine and appends the
// compaction/* observation events (D3) on the serial path: compaction/start is
// logged before the engine call (the attempt marker, with its reason/trigger),
// then compaction/summary + compaction/end when the attempt produced a result
// (bounded summary, shadowed range, tokens saved). A nil result (nothing
// foldable) or an engine error leaves only the start — the ADR's "orphan start
// reveals an interrupted/no-op attempt" signal. Event append failures are
// returned to the caller so no model request is issued against an uncommitted
// lifecycle fact.
func (a *app) compactAndLog(ctx context.Context, reason, trigger string, run func() (*compaction.Result, error)) (*compaction.Result, error) {
	return a.compactAndLogOn(ctx, a.log, reason, trigger, run)
}

func (a *app) compactAndLogOn(ctx context.Context, log *session.Log, reason, trigger string, run func() (*compaction.Result, error)) (*compaction.Result, error) {
	if log == nil {
		return nil, errors.New("compaction: session log is unavailable")
	}
	if _, err := log.Append(session.EventCompactionStart, session.NewCompactionStart(reason, trigger)); err != nil {
		return nil, fmt.Errorf("compaction/start: persist event: %w", err)
	}
	res, err := run()
	if err != nil {
		if _, appendErr := log.Append(session.EventCompactionEnd, session.NewCompactionEndError("", err.Error())); appendErr != nil {
			return res, errors.Join(err, fmt.Errorf("compaction/end error: persist event: %w", appendErr))
		}
		return res, err
	}
	if res == nil {
		return res, err
	}
	if _, err := log.Append(session.EventCompactionSummary,
		session.NewCompactionSummaryWithStats(res.CompactionID, res.Summary, res.ShadowedSeqs, res.ShadowedTokens, trigger)); err != nil {
		return res, fmt.Errorf("compaction/summary: persist event: %w", err)
	}
	if _, err := log.Append(session.EventCompactionEnd, session.NewCompactionEnd(res.CompactionID, res.ShadowedRange, res.ShadowedTokens)); err != nil {
		return res, fmt.Errorf("compaction/end: persist event: %w", err)
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
	if a.agentRegistry != nil {
		sessionID := a.currentID
		handle, err := a.sessionAgent(sessionID)
		if err != nil {
			return err
		}
		var res *compaction.Result
		err = handle.RunMaintenance(func(taskCtx context.Context) error {
			log, logErr := a.sessionLogForAgent(taskCtx, sessionID)
			if logErr != nil {
				return logErr
			}
			engine := a.compactionEngineFor(taskCtx, sessionID)
			if engine == nil {
				return nil
			}
			return a.compactSession(taskCtx, engine, log, args, &res)
		})
		if err != nil {
			return err
		}
		return printCompactionResult(res)
	}
	return a.compactLegacy(ctx, args)
}

func (a *app) compactLegacy(ctx context.Context, args []string) error {
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
	return printCompactionResult(res)
}

func (a *app) compactSession(ctx context.Context, engine compaction.Engine, log *session.Log, args []string, out **compaction.Result) error {
	var res *compaction.Result
	var err error
	switch {
	case len(args) == 3 && args[0] == "region":
		start, e1 := strconv.ParseInt(args[1], 10, 64)
		end, e2 := strconv.ParseInt(args[2], 10, 64)
		if e1 != nil || e2 != nil {
			return fmt.Errorf("usage: /compact region <start> <end> (integer event seqs)")
		}
		res, err = a.compactAndLogOn(ctx, log, "manual /compact region command", "manual",
			func() (*compaction.Result, error) { return engine.CompactRegion(ctx, log, start, end) })
	case len(args) != 0:
		return fmt.Errorf("usage: /compact or /compact region <start> <end>")
	default:
		res, err = a.compactAndLogOn(ctx, log, "manual /compact command", "manual",
			func() (*compaction.Result, error) { return engine.CompactNow(ctx, log) })
	}
	if out != nil {
		*out = res
	}
	return err
}

func printCompactionResult(res *compaction.Result) error {
	if res == nil {
		fmt.Println("compaction: nothing to compact")
		return nil
	}
	fmt.Printf("compacted %d events (seq %d..%d), saved %d tokens (id %s)\nsummary: %s\n",
		len(res.ShadowedSeqs), res.ShadowedRange[0], res.ShadowedRange[1], res.ShadowedTokens, res.CompactionID, res.Summary)
	return nil
}
