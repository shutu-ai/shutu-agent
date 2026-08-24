package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/acp"
	"github.com/jabing/shutu-agent/internal/compaction"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/loop"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/tools"
)

// acpFactory creates session-owned runtimes. It deliberately does not copy
// app: registered tools may close over the REPL's global currentID/log. ACP
// sessions instead get a fresh log, registry, prompt and durable sink.
type acpFactory struct {
	app *app
}

func (f *acpFactory) NewSession(ctx context.Context, cwd string) (acp.Session, error) {
	if f.app == nil || f.app.store == nil {
		return nil, errors.New("ACP app runtime is unavailable")
	}
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("session cwd must be absolute")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return nil, fmt.Errorf("session cwd: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("session cwd must be a directory")
	}
	cwd = filepath.Clean(cwd)
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	if err := f.app.store.CreateSession(ctx, id, time.Now().UTC()); err != nil {
		return nil, err
	}
	if hs, ok := f.app.store.(interface {
		SetSessionCWD(context.Context, string, string) error
	}); ok {
		if err := hs.SetSessionCWD(ctx, id, cwd); err != nil {
			_ = f.app.store.DeleteSession(context.Background(), id)
			return nil, err
		}
	}

	log := session.New()
	var terminalService *acpTerminal
	if f.app.cfg.Terminal.ACPEnabled != nil && *f.app.cfg.Terminal.ACPEnabled && config.Enabled(f.app.cfg.Terminal.Enabled) {
		terminalService = newACPCTerminal(f.app.cfg.Terminal, id, cwd, log)
	}
	var mcpService *acpMCP
	if f.app.cfg.Mcp.ACPEnabled != nil && *f.app.cfg.Mcp.ACPEnabled && config.Enabled(f.app.cfg.Mcp.Enabled) {
		mcpService, err = newACPMCP(ctx, f.app, id, log)
		if err != nil {
			_ = f.app.store.DeleteSession(context.Background(), id)
			return nil, err
		}
	}
	registry, err := acpRegistry(f.app, id, cwd, log, terminalService, mcpService)
	if err != nil {
		if mcpService != nil {
			_ = mcpService.Close()
		}
		_ = f.app.store.DeleteSession(context.Background(), id)
		return nil, err
	}
	var compactionEngine compaction.Engine
	if config.Enabled(f.app.cfg.Compaction.Enabled) {
		threshold := f.app.cfg.Compaction.TokenThreshold
		if threshold <= 0 {
			threshold = config.DefaultCompactionTokenThreshold
		}
		compactionEngine = compaction.NewBasic(compaction.BasicOpts{
			LLM:                   f.app.llmFor(""),
			Model:                 llmProviderModel(f.app.cfg, f.app.cfg.LLM.Provider),
			TokenThreshold:        threshold,
			RetainTurns:           f.app.cfg.Compaction.RetainTurns,
			RetainTokens:          f.app.cfg.Compaction.RetainTokens,
			FrameSummary:          true,
			RequireSmallerSummary: true,
		})
	}
	pb, err := buildPrompt(f.app.cfg.Mode, f.app.cfg.PromptsDir)
	if err != nil {
		if mcpService != nil {
			_ = mcpService.Close()
		}
		_ = f.app.store.DeleteSession(context.Background(), id)
		return nil, err
	}
	pb.SetTools(func() []llm.ToolSchema { return registry.Specs() })
	var subagentRuntime subagent.Runtime
	if f.app.cfg.Subagent.ACPEnabled != nil && *f.app.cfg.Subagent.ACPEnabled && config.Enabled(f.app.cfg.Subagent.Enabled) {
		var subagentTools *subagent.SubagentTools
		subagentRuntime, subagentTools, err = newACPSubagent(f.app, id, log, registry, pb)
		if err != nil {
			if mcpService != nil {
				_ = mcpService.Close()
			}
			_ = f.app.store.DeleteSession(context.Background(), id)
			return nil, err
		}
		if err := registerACPSubagentTools(registry, subagentTools); err != nil {
			_ = subagentRuntime.Close()
			if mcpService != nil {
				_ = mcpService.Close()
			}
			_ = f.app.store.DeleteSession(context.Background(), id)
			return nil, err
		}
	}
	attachACPSink(f.app, id, log)
	return &acpSession{
		app:        f.app,
		id:         id,
		cwd:        cwd,
		log:        log,
		registry:   registry,
		prompt:     pb,
		compaction: compactionEngine,
		terminal:   terminalService,
		mcp:        mcpService,
		subagents:  subagentRuntime,
	}, nil
}

// acpRegistry is intentionally a narrow, session-safe capability profile.
// Tools whose implementations capture app.currentID, app.log, terminal state,
// jobs, schedules, MCP clients, or approval input stay out until those seams
// accept an explicit session runtime.
func acpRegistry(a *app, id, cwd string, log *session.Log, terminalService *acpTerminal, mcpService *acpMCP) (*tools.Registry, error) {
	registry := tools.New()
	policy := a.basePolicy
	policy.Enabled = nil
	allowed := modeToolWhitelist(a.cfg.Mode, a.basePolicy.Enabled)
	if containsString(allowed, "get_time") {
		if err := registry.Register(tools.GetTime{}); err != nil {
			return nil, err
		}
		policy.Enabled = append(policy.Enabled, "get_time")
	}
	if containsString(allowed, "read") {
		if err := registry.Register(tools.NewReadFile(cwd)); err != nil {
			return nil, err
		}
		policy.Enabled = append(policy.Enabled, "read")
	}
	if terminalService != nil {
		for _, name := range []string{acpTerminalStart, acpTerminalWrite, acpTerminalRead, acpTerminalSignal, acpTerminalStop} {
			if err := registry.Register(acpTerminalTool{service: terminalService, name: name}); err != nil {
				return nil, err
			}
			policy.Enabled = append(policy.Enabled, name)
		}
	}
	if mcpService != nil {
		for _, tool := range mcpService.tools() {
			if err := registry.Register(tool); err != nil {
				return nil, err
			}
			policy.Enabled = append(policy.Enabled, tool.Name())
		}
	}
	registry.SetPolicy(policy)
	registry.SetOwner(tools.Owner{
		SessionID: id,
		NextSeq:   log.NextSeq,
	})
	return registry, nil
}

func attachACPSink(a *app, id string, log *session.Log) {
	pctx := a.baseCtx
	if pctx == nil {
		pctx = context.Background()
	}
	log.SetSink(func(ev session.Event) error {
		if err := a.store.AppendEvents(pctx, id, []session.Event{ev}); err != nil {
			return err
		}
		if a.hub != nil {
			a.hub.Publish(id, ev)
		}
		if a.hooks != nil {
			a.hooks.Notify(id, ev)
		}
		return nil
	})
}

type acpSession struct {
	app        *app
	id         string
	cwd        string
	log        *session.Log
	registry   *tools.Registry
	prompt     *prompt.Builder
	compaction compaction.Engine
	terminal   *acpTerminal
	mcp        *acpMCP
	subagents  subagent.Runtime
	mu         sync.Mutex
	busy       bool
	closed     bool
	cancel     context.CancelFunc
}

func (s *acpSession) Prompt(ctx context.Context, text string, emit func(acp.Update)) (acp.StopReason, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("session is closed")
	}
	if s.busy {
		s.mu.Unlock()
		return "", errors.New("a prompt is already in flight for this session")
	}
	pctx, cancel := context.WithCancel(ctx)
	s.busy = true
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.busy = false
		s.cancel = nil
		s.mu.Unlock()
	}()

	before := len(s.log.Events())
	turn := loop.New(loop.Config{
		LLM:             s.app.llmFor(""),
		Log:             s.log,
		Tools:           s.registry,
		ToolSpecs:       func() []llm.ToolSchema { return toolSpecsForMode(s.app.cfg.Mode, s.registry.Specs()) },
		Prompt:          s.prompt,
		Model:           s.app.cfg.Model,
		Provider:        s.app.cfg.LLM.Provider,
		ReasoningEffort: s.app.cfg.ReasoningEffort,
		PreStep:         s.compactionPreSteps(),
		OnText:          func(string) {},
		OnError:         func(error) {},
	})
	err := turn.Run(pctx, text)
	for _, ev := range s.log.Events()[before:] {
		if ev.Type != session.EventAssistantMessage {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil && payload.Text != "" {
			emit(acp.Update{Text: payload.Text})
		}
	}
	if err != nil {
		if errors.Is(pctx.Err(), context.Canceled) {
			return acp.StopCancelled, nil
		}
		return "", err
	}
	if errors.Is(pctx.Err(), context.Canceled) {
		return acp.StopCancelled, nil
	}
	return acp.StopEndTurn, nil
}

func (s *acpSession) compactionPreSteps() []loop.PreStepInjector {
	if s.compaction == nil {
		return nil
	}
	return []loop.PreStepInjector{{Name: "compaction", Inject: s.compactionPreStep()}}
}

func (s *acpSession) compactionPreStep() func(context.Context, string) []llm.Message {
	return func(ctx context.Context, _ string) []llm.Message {
		threshold := s.app.cfg.Compaction.TokenThreshold
		if threshold <= 0 {
			threshold = config.DefaultCompactionTokenThreshold
		}
		if s.compaction == nil || acpSurfaceTokens(s.log) <= threshold {
			return nil
		}
		_, _ = s.log.Append(session.EventCompactionStart,
			session.NewCompactionStart("surface token estimate exceeded threshold", "pressure"))
		result, err := s.compaction.CompactIfNeeded(ctx, s.log, compaction.TriggerPressure)
		if err != nil {
			_, _ = s.log.Append(session.EventCompactionEnd, session.NewCompactionEndError("", err.Error()))
			return nil
		}
		if result == nil {
			return nil
		}
		_, _ = s.log.Append(session.EventCompactionSummary,
			session.NewCompactionSummaryWithStats(result.CompactionID, result.Summary, result.ShadowedSeqs, result.ShadowedTokens, "pressure"))
		_, _ = s.log.Append(session.EventCompactionEnd,
			session.NewCompactionEnd(result.CompactionID, result.ShadowedRange, result.ShadowedTokens))
		return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.Text(compactedNotice),
		}}}
	}
}

func acpSurfaceTokens(log *session.Log) int {
	total := 0
	for _, message := range log.DeriveHistory() {
		total += len(message.Text()) / 4
		for _, call := range message.ToolCalls {
			total += len(call.Name)/4 + len(call.Arguments)/4
		}
	}
	return total
}

func (s *acpSession) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *acpSession) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	var first error
	// Stop child agents before closing the session-owned resources their child
	// registries may use (MCP, terminal, etc.). Runtime.Close is idempotent and
	// waits for every live child to settle.
	if s.subagents != nil {
		first = s.subagents.Close()
	}
	if s.terminal != nil {
		if err := s.terminal.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.mcp != nil {
		if err := s.mcp.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
