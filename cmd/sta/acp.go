package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/acp"
	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/compaction"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/fs"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/subagent"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// acpFactory creates session-owned runtimes. It deliberately does not copy
// app: registered tools may close over the REPL's global currentID/log. ACP
// sessions instead get a fresh log, registry, prompt and durable sink.
type acpFactory struct {
	app *app
}

func modelAcceptsImages(modalities string) bool {
	for _, modality := range strings.Split(modalities, ",") {
		if strings.TrimSpace(modality) == "image" {
			return true
		}
	}
	return false
}

func acpImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func decodeCanonicalBase64(encoded string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("base64 must use canonical RFC 4648 encoding")
	}
	if base64.StdEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("base64 must use canonical RFC 4648 encoding")
	}
	return decoded, nil
}

func (f *acpFactory) Capabilities(context.Context) map[string]bool {
	if f == nil || f.app == nil {
		return nil
	}
	return map[string]bool{
		"image": f.app.multimodalEnabled() && f.app.attachStore != nil && f.app.llmSupportsImagesForSession(""),
	}
}

// ToolCatalog exposes the production registry revision to ACP clients and
// makes inventory drift observable across initialize/new/resume/reconnect.
func (f *acpFactory) ToolCatalog(_ context.Context) (acp.ToolCatalog, error) {
	if f == nil || f.app == nil || f.app.reg == nil {
		return acp.ToolCatalog{}, errors.New("tool registry is unavailable")
	}
	manifest, err := f.app.reg.CatalogManifest()
	if err != nil {
		return acp.ToolCatalog{}, err
	}
	if err := tools.ValidateCatalogManifest(manifest); err != nil {
		return acp.ToolCatalog{}, err
	}
	catalog := acp.ToolCatalog{
		SchemaVersion: manifest.SchemaVersion,
		Revision:      manifest.Revision,
		Digest:        manifest.Digest,
		Tools:         make([]acp.ToolCatalogEntry, 0, len(manifest.Tools)),
	}
	for _, entry := range manifest.Tools {
		catalog.Tools = append(catalog.Tools, acp.ToolCatalogEntry{
			Name:       entry.Name,
			Profile:    entry.Profile,
			Provenance: entry.Provenance,
			Generation: entry.Registration.Generation,
			Visible:    entry.Visible,
		})
	}
	return catalog, nil
}

func (f *acpFactory) NewSession(ctx context.Context, cwd string) (acp.Session, error) {
	id, err := store.GenerateReservedID(ctx, f.app.store, "session", newSessionID)
	if err != nil {
		return nil, err
	}
	return f.openSession(ctx, cwd, id, true)
}

// ResumeSession reopens an existing durable ACP transcript. The ACP wire id
// is intentionally the durable session id so reconnecting clients can resume
// their cursor and history without creating a parallel log.
func (f *acpFactory) ResumeSession(ctx context.Context, id string) (acp.Session, error) {
	if f.app == nil || f.app.store == nil {
		return nil, errors.New("ACP app runtime is unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("ACP resume requires a session id")
	}
	meta, err := f.app.store.GetSessionMeta(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", acp.ErrSessionNotFound, id)
		}
		return nil, err
	}
	cwd := meta.CWD
	if strings.TrimSpace(cwd) == "" {
		cwd = f.app.sessionCWDFor(id)
	}
	return f.openSession(ctx, cwd, id, false)
}

func (f *acpFactory) openSession(ctx context.Context, cwd, id string, created bool) (acp.Session, error) {
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
	cfg := f.app.providerConfigSnapshot()
	provider, model := f.app.sessionProviderModel(id)
	mode := cfg.Mode
	effort := cfg.ReasoningEffort
	permissionPolicy := cfg.Interact.Policy
	cleanupSession := func() {
		if created {
			_ = f.app.store.DeleteSession(context.Background(), id)
		}
	}
	var seed []session.Event
	if created {
		// Publish the durable row, immutable cwd, and per-session model policy in
		// one transaction when the production backend supports it. A separate
		// Create→CWD→Config sequence can expose an ACP session that is visible but
		// cannot be recreated with the same runtime after a crash.
		createdAt := time.Now().UTC()
		if atomic, ok := f.app.store.(store.SessionCreateStore); ok {
			err := atomic.CreateSessionWithOptions(ctx, id, createdAt, store.SessionCreateOptions{
				Header: store.SessionHeader{ID: id, CreatedAt: createdAt, CWD: cwd, AgentPreset: mode},
				Config: &store.SessionConfig{AgentPreset: mode, Provider: provider, Model: model, ReasoningEffort: effort, Permission: permissionPolicy},
			}, nil)
			if err != nil {
				return nil, err
			}
		} else {
			if err := f.app.store.CreateSession(ctx, id, createdAt); err != nil {
				return nil, err
			}
			if hs, ok := f.app.store.(interface {
				SetSessionCWD(context.Context, string, string) error
			}); ok {
				if err := hs.SetSessionCWD(ctx, id, cwd); err != nil {
					cleanupSession()
					return nil, err
				}
			}
			// Compatibility fallback for lightweight Store test doubles.
			if scs, ok := f.app.store.(store.SessionConfigStore); ok {
				if err := scs.SetSessionConfig(ctx, id, store.SessionConfig{
					AgentPreset: mode, Provider: provider, Model: model,
					ReasoningEffort: effort, Permission: permissionPolicy,
				}); err != nil {
					cleanupSession()
					return nil, err
				}
			}
		}
	} else {
		seed, err = f.app.store.LoadSession(ctx, id)
		if err != nil {
			return nil, err
		}
		if scs, ok := f.app.store.(store.SessionConfigStore); ok {
			if saved, cfgErr := scs.GetSessionConfig(ctx, id); cfgErr == nil {
				if saved.AgentPreset != "" {
					mode = saved.AgentPreset
				}
				if saved.ReasoningEffort != "" {
					effort = saved.ReasoningEffort
				}
				if saved.Permission != "" {
					switch saved.Permission {
					case string(interact.PolicyAsk), string(interact.PolicyNever):
						permissionPolicy = saved.Permission
					default:
						_, _, _, permissionPolicy = permissionBundle(saved.Permission)
					}
				}
			}
		}
	}

	log := session.New()
	f.app.configureImageResolver(log)
	if len(seed) > 0 {
		if err := log.Restore(seed); err != nil {
			return nil, fmt.Errorf("ACP restore session %q: %w", id, err)
		}
		if _, err := projection.Build(seed); err != nil {
			return nil, fmt.Errorf("ACP restore session %q: %w", id, err)
		}
	}
	var terminalService *acpTerminal
	if cfg.Terminal.ACPEnabled != nil && *cfg.Terminal.ACPEnabled && config.Enabled(cfg.Terminal.Enabled) {
		terminalService = newACPCTerminal(cfg.Terminal, id, cwd, log)
	}
	var mcpService *acpMCP
	if cfg.Mcp.ACPEnabled != nil && *cfg.Mcp.ACPEnabled && config.Enabled(cfg.Mcp.Enabled) {
		mcpService, err = newACPMCPWithConfig(ctx, f.app, id, log, cfg)
		if err != nil {
			cleanupSession()
			return nil, err
		}
	}
	registry, err := acpRegistryForMode(f.app, id, cwd, log, terminalService, mcpService, mode)
	if err != nil {
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, err
	}
	var compactionEngine compaction.Engine
	if config.Enabled(cfg.Compaction.Enabled) {
		threshold := cfg.Compaction.TokenThreshold
		if threshold <= 0 {
			threshold = config.DefaultCompactionTokenThreshold
		}
		compactionEngine = compaction.NewBasic(compaction.BasicOpts{
			LLM:                   f.app.llmFor(provider),
			SessionID:             id,
			Model:                 model,
			TokenThreshold:        threshold,
			RetainTurns:           cfg.Compaction.RetainTurns,
			RetainTokens:          cfg.Compaction.RetainTokens,
			SummaryInputTokens:    cfg.Compaction.SummaryInputTokens,
			FrameSummary:          true,
			RequireSmallerSummary: true,
		})
	}
	pb, err := buildPrompt(mode, cfg.PromptsDir)
	if err != nil {
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, err
	}
	pb.SetTools(func() []llm.ToolSchema { return registry.VisibleSpecs() })
	if err := registry.ValidateProjection(mode, registry.VisibleSpecs()); err != nil {
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, fmt.Errorf("sta: validate ACP canonical tool projection: %w", err)
	}
	var subagentRuntime subagent.Runtime
	if cfg.Subagent.ACPEnabled != nil && *cfg.Subagent.ACPEnabled && config.Enabled(cfg.Subagent.Enabled) {
		var subagentTools *subagent.SubagentTools
		subagentRuntime, subagentTools, err = newACPSubagentWithConfig(f.app, id, log, registry, pb, cfg, provider, model)
		if err != nil {
			if mcpService != nil {
				_ = mcpService.Close()
			}
			cleanupSession()
			return nil, err
		}
		if err := registerACPSubagentTools(registry, subagentTools); err != nil {
			_ = subagentRuntime.Close()
			if mcpService != nil {
				_ = mcpService.Close()
			}
			cleanupSession()
			return nil, err
		}
	}
	attachACPSink(f.app, id, log)
	approval := f.app.interacts
	sharedApproval := approval != nil
	if approval == nil {
		approval = interact.NewEngine(nil)
	}
	s := &acpSession{
		app:            f.app,
		id:             id,
		cwd:            cwd,
		log:            log,
		registry:       registry,
		prompt:         pb,
		compaction:     compactionEngine,
		terminal:       terminalService,
		mcp:            mcpService,
		subagents:      subagentRuntime,
		approval:       approval,
		sharedApproval: sharedApproval,
		provider:       provider,
		model:          model,
		effort:         effort,
		mode:           mode,
	}
	if policy := strings.TrimSpace(permissionPolicy); policy != "" {
		if controller, ok := s.approval.(interact.PolicyController); ok {
			var policyErr error
			if sharedApproval {
				// ACP sessions share the app approval service, so their policy
				// must be session-scoped and cannot mutate the CLI default.
				policyErr = controller.SetSessionPolicy(s.id, interact.ApprovalPolicy(policy))
			} else {
				policyErr = controller.SetDefaultPolicy(interact.ApprovalPolicy(policy))
			}
			if policyErr != nil {
				if !sharedApproval {
					_ = s.approval.Close()
				}
				if subagentRuntime != nil {
					_ = subagentRuntime.Close()
				}
				if mcpService != nil {
					_ = mcpService.Close()
				}
				cleanupSession()
				return nil, fmt.Errorf("ACP approval policy: %w", policyErr)
			}
		}
	}
	// ACP sessions use the Agent lifecycle/inbox seam. The direct-loop fallback
	// remains available for focused tests that construct an acpSession directly.
	inboxEvents, err := replaySessionInbox(log.Events())
	if err != nil {
		if subagentRuntime != nil {
			_ = subagentRuntime.Close()
		}
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, fmt.Errorf("ACP inbox restore: %w", err)
	}
	agentRuntime := agent.NewRegistry()
	handle, err := agentRuntime.Create(agent.Options{
		ID:           agent.ID(id),
		InboxJournal: sessionInboxJournal{log: log},
		InitialInbox: inboxEvents,
		InitialTurn:  log.NextTurn(),
		Runner: func(runCtx context.Context, runtimeAgent *agent.Agent, input agent.TurnInput) error {
			messages := make([]llm.Message, 0, len(input.Messages))
			for _, message := range input.Messages {
				if len(message.Content) > 0 {
					messages = append(messages, llm.Message{Role: llm.RoleUser, Content: append([]llm.ContentBlock(nil), message.Content...)})
				} else if strings.TrimSpace(message.Text) != "" {
					messages = append(messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(message.Text)}})
				}
			}
			return s.executePromptMessagesWithAgent(runCtx, messages, runtimeAgent)
		},
	})
	if err != nil {
		_ = agentRuntime.CloseAll()
		if subagentRuntime != nil {
			_ = subagentRuntime.Close()
		}
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, fmt.Errorf("ACP Agent runtime: %w", err)
	}
	if f.app.jobs != nil {
		if cleanupErr := handle.Scope().AddCleanup(func() error {
			return f.app.jobs.CloseOwner(id)
		}); cleanupErr != nil {
			_ = agentRuntime.Close(agent.ID(id))
			if subagentRuntime != nil {
				_ = subagentRuntime.Close()
			}
			if mcpService != nil {
				_ = mcpService.Close()
			}
			cleanupSession()
			return nil, fmt.Errorf("ACP job owner cleanup: %w", cleanupErr)
		}
	}
	if cleanupErr := handle.Scope().AddCleanup(func() error {
		return f.app.closeModelTerminalOwner(id)
	}); cleanupErr != nil {
		_ = agentRuntime.Close(agent.ID(id))
		if subagentRuntime != nil {
			_ = subagentRuntime.Close()
		}
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, fmt.Errorf("ACP terminal owner cleanup: %w", cleanupErr)
	}
	if cleanupErr := handle.Scope().AddCleanup(func() error {
		f.app.clearSessionApprovalPolicy(id)
		return nil
	}); cleanupErr != nil {
		_ = agentRuntime.Close(agent.ID(id))
		if subagentRuntime != nil {
			_ = subagentRuntime.Close()
		}
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, fmt.Errorf("ACP approval policy cleanup: %w", cleanupErr)
	}
	if err := handle.Start(context.Background()); err != nil {
		_ = agentRuntime.CloseAll()
		if subagentRuntime != nil {
			_ = subagentRuntime.Close()
		}
		if mcpService != nil {
			_ = mcpService.Close()
		}
		cleanupSession()
		return nil, fmt.Errorf("ACP Agent start: %w", err)
	}
	s.agentRuntime = agentRuntime
	s.agentHandle = handle
	if len(cfg.Interact.SensitiveTools) > 0 {
		registry.AddPreExecuteHook(s.acpPermissionGate(cfg.Interact.SensitiveTools))
	}
	return s, nil
}

// acpRegistry is intentionally a narrow, session-safe capability profile.
// Tools whose implementations capture app.currentID, app.log, terminal state,
// jobs, schedules, MCP clients, or approval input stay out until those seams
// accept an explicit session runtime.
func acpRegistry(a *app, id, cwd string, log *session.Log, terminalService *acpTerminal, mcpService *acpMCP) (*tools.Registry, error) {
	return acpRegistryForMode(a, id, cwd, log, terminalService, mcpService, a.providerConfigSnapshot().Mode)
}

func acpRegistryForMode(a *app, id, cwd string, log *session.Log, terminalService *acpTerminal, mcpService *acpMCP, mode string) (*tools.Registry, error) {
	registry := tools.New()
	policy := a.basePolicy
	policy.Profile = mode
	policy.Enabled = nil
	allowed := modeToolWhitelist(mode, a.basePolicy.Enabled)
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
	if containsString(allowed, "str_replace_editor") {
		fsTools := fs.NewFsTools(fs.NewLocalFS(cwd), func(typ string, data any) {
			_, _ = log.Append(typ, data)
		})
		fsTools.SetErrorSink(func(typ string, data any) error {
			if log == nil {
				return errors.New("ACP session log is unavailable")
			}
			_, err := log.Append(typ, data)
			return err
		})
		if err := registry.Register(fsTools.StrReplaceEditor()); err != nil {
			return nil, err
		}
		policy.Enabled = append(policy.Enabled, "str_replace_editor")
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
	// ACP owns the live runtime log for its durable id. Register it so
	// application-side writers (titles, approvals, jobs, schedules, runtime
	// context) reuse the same append authority as the prompt loop. Resume
	// deliberately replaces this entry with the restored runtime log.
	a.runtimeMu.Lock()
	if a.runtimeLogs == nil {
		a.runtimeLogs = make(map[string]*session.Log)
	}
	a.runtimeLogs[id] = log
	a.runtimeMu.Unlock()

	pctx := a.baseCtx
	if pctx == nil {
		pctx = context.Background()
	}
	log.SetSink(func(ev session.Event) error {
		return a.store.AppendEvents(pctx, id, []session.Event{ev})
	})
	log.SetObserver(func(ev session.Event) {
		if a.hub != nil {
			a.hub.Publish(id, ev)
		}
		if a.hooks != nil {
			a.hooks.Notify(id, ev)
		}
		if a.extensions != nil {
			a.extensions.PublishSessionEvent(id, ev)
		}
		if a.telemetry != nil {
			if ev.Type == session.EventFeedbackRecord {
				a.telemetry.ObserveSession(id, log.Events(), ev.Seq)
			} else {
				a.telemetry.Observe(id, ev)
			}
		}
	})
}

type acpSession struct {
	app            *app
	id             string
	cwd            string
	log            *session.Log
	registry       *tools.Registry
	prompt         *prompt.Builder
	compaction     compaction.Engine
	terminal       *acpTerminal
	mcp            *acpMCP
	subagents      subagent.Runtime
	agentRuntime   *agent.Registry
	agentHandle    *agent.Handle
	mu             sync.Mutex
	busy           bool
	closed         bool
	cancel         context.CancelFunc
	permissionMu   sync.RWMutex
	permission     acp.PermissionRequester
	approval       interact.Engine
	approvalMu     sync.Mutex
	sharedApproval bool
	provider       string
	model          string
	effort         string
	mode           string
}

// SessionID exposes the durable id to ACP. The protocol id must be the same
// identity that backs the persisted transcript; otherwise a client cannot
// reconnect using the id returned by session/new.
func (s *acpSession) SessionID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// ResumeMetadata returns persisted runtime facts that reconnecting ACP clients
// can display without replaying the transcript. The event cursor identifies the
// restored log length; nextTurn is the inbox continuation boundary.
func (s *acpSession) ResumeMetadata() map[string]any {
	if s == nil {
		return nil
	}
	eventCursor := uint64(0)
	nextTurn := int64(0)
	projectionError := ""
	if s.log != nil {
		events := s.log.Events()
		snapshot, err := projection.Build(events)
		switch {
		case err != nil:
			projectionError = err.Error()
		default:
			eventCursor = snapshot.AsOfSeq
		}
		nextTurn = int64(s.log.NextTurn())
	}
	result := map[string]any{
		"durable":     true,
		"cwd":         s.cwd,
		"provider":    s.provider,
		"model":       s.model,
		"effort":      s.effort,
		"mode":        s.mode,
		"eventCursor": eventCursor,
		"nextTurn":    nextTurn,
	}
	if projectionError != "" {
		result["projectionError"] = projectionError
	}
	return result
}

func (s *acpSession) SetPermissionRequester(requester acp.PermissionRequester) {
	s.permissionMu.Lock()
	s.permission = requester
	s.permissionMu.Unlock()
}

func (s *acpSession) permissionRequester() acp.PermissionRequester {
	s.permissionMu.RLock()
	defer s.permissionMu.RUnlock()
	return s.permission
}

// resolveACPApproval commits the live decision and its durable audit fact as
// one serialized application transition. The approval engine is in-memory in
// the current host, so an append failure must restore the pending request;
// otherwise a reconnect could observe a different answer than the live tool
// execution did.
func (s *acpSession) resolveACPApproval(ctx context.Context, req interact.Request, status interact.ApprovalStatus, decision map[string]any) error {
	// CLI, Web and ACP answerers must share the same application transition
	// when they use the durable approval service. This keeps ownership checks,
	// CAS, rollback and approval/decided event projection identical. A directly
	// constructed ACP session may still use its private in-memory engine below.
	if s.app != nil && s.sharedApproval {
		answer := ""
		if value, ok := decision["answer"].(string); ok {
			answer = value
		}
		return s.app.approvalAnswerer().Resolve(ctx, s.id, req.ID, status, answer, false)
	}
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	engine := s.approval
	if engine == nil {
		return errors.New("ACP approval engine is unavailable")
	}
	raw, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode ACP approval decision: %w", err)
	}
	event := session.Event{Seq: s.log.NextSeq(), Type: session.EventApprovalDecided, At: time.Now().UTC(), Version: session.EventVersion, Data: raw}
	atomic := false
	if resolver, ok := engine.(interact.AtomicEventResolver); ok {
		_, atomic, err = resolver.ResolveForSessionWithAnswerAndEvent(ctx, s.id, req.ID, status, "", event)
	} else if resolver, ok := engine.(interact.SessionResolver); ok {
		_, err = resolver.ResolveForSession(ctx, s.id, req.ID, status)
	} else {
		_, err = engine.Resolve(ctx, req.ID, status)
	}
	if err != nil {
		return err
	}
	if atomic {
		if err := s.log.AppendPersisted(event); err != nil {
			return fmt.Errorf("project committed ACP approval decision: %w", err)
		}
		return nil
	}
	if _, err := engine.Await(ctx, req.ID); err != nil {
		return err
	}
	if _, err := s.log.Append(session.EventApprovalDecided, decision); err != nil {
		restorer, ok := engine.(interact.RequestRestorer)
		if !ok {
			return fmt.Errorf("persist ACP approval decision: %w (approval state cannot be rolled back)", err)
		}
		if restoreErr := restorer.Restore(context.Background(), []interact.Request{req}); restoreErr != nil {
			return errors.Join(fmt.Errorf("persist ACP approval decision: %w", err), fmt.Errorf("rollback ACP approval state: %w", restoreErr))
		}
		if s.app != nil {
			s.app.forgetInteraction(req.ID)
			s.app.rememberInteraction(req.ID, s.id, req.CallID)
		}
		return fmt.Errorf("persist ACP approval decision: %w", err)
	}
	return nil
}

func (s *acpSession) acpPermissionGate(sensitive []string) tools.PreExecuteHook {
	return func(ctx context.Context, exec tools.Execution) (tools.PreToolDecision, error) {
		if !containsSensitive(sensitive, exec.Name) {
			return tools.PreToolDecision{Kind: "allow"}, nil
		}
		if correlation, ok := runtimectx.CorrelationOf(ctx); ok && correlation.TurnID != "" && !session.HasOpenTurn(s.log.Events()) {
			return tools.PreToolDecision{}, errors.New("ACP approval request requires an open turn")
		}
		callID := exec.CallID
		if callID == "" {
			callID = tools.CallIDFromContext(ctx)
		}
		if callID == "" {
			callID = fmt.Sprintf("tool-%d", s.log.NextSeq())
		}
		args, _ := json.Marshal(exec.Arguments)
		argsText := boundRunes(string(args))
		prompt := fmt.Sprintf("Allow the sensitive tool %s to run?", exec.Name)
		engine := s.approval
		if engine == nil {
			// Direct unit-test constructions may omit the app factory. Keep the
			// same service semantics instead of silently bypassing approval.
			engine = interact.NewEngine(nil)
			defer engine.Close()
		}
		requester := s.permissionRequester()
		requesterAvailable := requester != nil
		var req interact.Request
		var err error
		var askedEvent session.Event
		atomicAsked := false
		if sessionRequester, ok := engine.(interact.AtomicSessionCallRequester); ok {
			req, atomicAsked, err = sessionRequester.RequestForSessionWithCallIDAndEvent(ctx, s.id, callID, prompt, exec.Name, argsText, func(created interact.Request) session.Event {
				asked := map[string]any{"id": created.ID, "callId": callID, "toolName": exec.Name, "prompt": created.Prompt, "reason": prompt, "args": argsText, "questions": created.Questions}
				askedEvent = session.Event{Seq: s.log.NextSeq(), Type: session.EventApprovalAsked, At: time.Now().UTC(), Version: session.EventVersion, Data: marshalACPEventData(asked)}
				return askedEvent
			})
		} else if sessionRequester, ok := engine.(interact.SessionCallRequester); ok {
			// Keep the ACP tool call identity on the durable approval request;
			// otherwise reconnect/replay sees an approval with no causal call.
			req, err = sessionRequester.RequestForSessionWithCallID(ctx, s.id, callID, prompt, exec.Name, argsText)
		} else if sessionRequester, ok := engine.(interact.SessionRequester); ok {
			req, err = sessionRequester.RequestForSession(ctx, s.id, prompt, exec.Name, argsText)
		} else {
			req, err = engine.Request(ctx, prompt, exec.Name, argsText)
		}
		if err != nil {
			return tools.PreToolDecision{}, err
		}
		if req.Status == interact.StatusUnavailable || (req.Status == interact.StatusRejected && req.ID == "") {
			reason := "ACP approval is rejected by policy"
			if req.Status == interact.StatusUnavailable {
				reason = "ACP approval is unavailable: no answerer is registered"
			}
			return tools.PreToolDecision{Kind: "deny", Reason: reason}, nil
		}
		if s.app != nil {
			s.app.rememberInteraction(req.ID, s.id, callID)
		}
		asked := map[string]any{
			"id": req.ID, "callId": callID, "toolName": exec.Name, "prompt": req.Prompt,
			"reason": prompt, "args": argsText, "questions": req.Questions,
		}
		if atomicAsked {
			if err := s.log.AppendPersisted(askedEvent); err != nil {
				if s.app != nil {
					s.app.forgetInteraction(req.ID)
				}
				return tools.PreToolDecision{}, fmt.Errorf("ACP approval event adoption: %w", err)
			}
		} else if _, err := s.log.Append(session.EventApprovalAsked, asked); err != nil {
			if canceler, ok := engine.(interact.Canceler); ok {
				_, _ = canceler.Cancel(context.Background(), req.ID)
			}
			if s.app != nil {
				s.app.forgetInteraction(req.ID)
			}
			return tools.PreToolDecision{}, err
		}
		if !requesterAvailable {
			if err := s.resolveACPApproval(context.Background(), req, interact.StatusUnavailable, map[string]any{"id": req.ID, "callId": callID, "outcome": string(interact.StatusUnavailable)}); err != nil {
				return tools.PreToolDecision{}, fmt.Errorf("ACP unavailable decision: %w", err)
			}
			return tools.PreToolDecision{Kind: "deny", Reason: "ACP permission client is unavailable"}, nil
		}
		outcome, err := requester.RequestPermission(ctx, acp.PermissionRequest{
			SessionID: s.id, ToolCallID: callID, ToolName: exec.Name, Reason: prompt,
			Options: []acp.PermissionOption{
				{ID: "allow-once", Label: "Allow once", Kind: "allow_once"},
				{ID: "reject-once", Label: "Reject", Kind: "reject_once"},
			},
		})
		if err != nil {
			status := interact.StatusUnavailable
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = interact.StatusCanceled
			}
			if resolveErr := s.resolveACPApproval(context.Background(), req, status, map[string]any{"id": req.ID, "callId": callID, "outcome": string(status)}); resolveErr != nil {
				return tools.PreToolDecision{}, fmt.Errorf("ACP unavailable decision: %w (request error: %v)", resolveErr, err)
			}
			return tools.PreToolDecision{Kind: "deny", Reason: err.Error()}, nil
		}
		// The client is an answerer, not an authority to invent new approval
		// outcomes. Treat unknown outcome/option combinations as unavailable so
		// malformed or rogue clients cannot silently turn into a rejection that
		// looks like a valid human decision in the audit trail.
		allowed := outcome.Outcome == "approved" || outcome.Outcome == "allowed-once" || outcome.Outcome == "allow" || outcome.OptionID == "allow-once" || outcome.OptionID == "allow_once"
		rejected := outcome.Outcome == "rejected" || outcome.Outcome == "reject" || outcome.Outcome == "rejected-once" || outcome.OptionID == "reject-once" || outcome.OptionID == "reject_once"
		if !allowed && !rejected {
			decision := map[string]any{"id": req.ID, "callId": callID, "outcome": string(interact.StatusUnavailable), "answererOutcome": outcome.Outcome, "optionId": outcome.OptionID}
			if resolveErr := s.resolveACPApproval(context.Background(), req, interact.StatusUnavailable, decision); resolveErr != nil {
				return tools.PreToolDecision{}, fmt.Errorf("ACP unavailable decision: %w", resolveErr)
			}
			return tools.PreToolDecision{Kind: "deny", Reason: "ACP client returned an invalid permission outcome"}, nil
		}
		// Cancellation wins over a response that raced with the prompt signal;
		// the late answer must not authorize a tool after the owning turn ended.
		if err := ctx.Err(); err != nil {
			if resolveErr := s.resolveACPApproval(context.Background(), req, interact.StatusCanceled, map[string]any{"id": req.ID, "callId": callID, "outcome": string(interact.StatusCanceled)}); resolveErr != nil {
				return tools.PreToolDecision{}, fmt.Errorf("ACP cancelled decision: %w", resolveErr)
			}
			return tools.PreToolDecision{Kind: "deny", Reason: "ACP permission request was cancelled"}, nil
		}
		status := interact.StatusRejected
		decision := map[string]any{"id": req.ID, "callId": callID, "outcome": string(status)}
		if allowed {
			status = interact.StatusAllowedOnce
			decision["outcome"] = string(status)
		}
		if outcome.OptionID != "" {
			decision["optionId"] = outcome.OptionID
		}
		if err := s.resolveACPApproval(ctx, req, status, decision); err != nil {
			return tools.PreToolDecision{}, err
		}
		if !allowed {
			return tools.PreToolDecision{Kind: "deny", Reason: "ACP client rejected tool execution"}, nil
		}
		return tools.PreToolDecision{Kind: "allow"}, nil
	}
}

func marshalACPEventData(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func (s *acpSession) Prompt(ctx context.Context, text string, emit func(acp.Update)) (acp.StopReason, error) {
	return s.runPrompt(ctx, emit, func(pctx context.Context) error {
		if s.agentHandle != nil {
			return s.agentHandle.Run(pctx, text, nil)
		}
		return s.executePrompt(pctx, text)
	})
}

// PromptContent accepts ACP text and inline raster-image blocks. Image bytes
// are validated and admitted to the attachment store before entering the
// durable Agent loop, so logs contain references rather than payload bytes.
func (s *acpSession) PromptContent(ctx context.Context, blocks []acp.PromptContentBlock, emit func(acp.Update)) (acp.StopReason, error) {
	pctx, finish, err := s.beginPrompt(ctx)
	if err != nil {
		return "", err
	}
	defer finish()

	// Match the reference admission boundary: inspect and decode the complete
	// batch before touching durable attachment storage. Otherwise a malformed
	// later block can leave an earlier image object unreachable from the log.
	type imageInput struct {
		mediaType string
		data      []byte
	}
	images := make([]imageInput, 0, len(blocks))
	maxBytes := config.DefaultMultimodalMaxImageBytes
	if s.app != nil {
		cfg := s.app.providerConfigSnapshot()
		maxBytes = cfg.LLM.Multimodal.MaxImageBytes
		if maxBytes <= 0 {
			maxBytes = config.DefaultMultimodalMaxImageBytes
		}
	}
	if err := pctx.Err(); err != nil {
		return acp.StopCancelled, nil
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == "" {
				return "", errors.New("text prompt blocks must be non-empty")
			}
		case "resource_link":
			if block.Name == "" || block.URI == "" {
				return "", errors.New("resource_link prompt blocks require name and uri")
			}
		case "image":
			provider, model := s.provider, s.model
			if s.app != nil && (provider == "" || model == "") {
				sessionProvider, sessionModel := s.app.sessionProviderModel(s.id)
				if provider == "" {
					provider = sessionProvider
				}
				if model == "" {
					model = sessionModel
				}
			}
			if s.app == nil || !s.app.multimodalEnabled() || s.app.attachStore == nil ||
				!s.app.llmSupportsImagesForRoute(provider, model) {
				return "", errors.New("image prompt capability is unavailable")
			}
			if !acpImageMediaType(block.MimeType) {
				return "", fmt.Errorf("unsupported image mime type %q", block.MimeType)
			}
			decoded, err := decodeCanonicalBase64(block.Data)
			if err != nil {
				return "", fmt.Errorf("invalid image data: %w", err)
			}
			if len(decoded) == 0 {
				return "", errors.New("image data is empty")
			}
			if maxBytes > 0 && len(decoded) > maxBytes {
				return "", fmt.Errorf("image exceeds maximum size of %d bytes", maxBytes)
			}
			images = append(images, imageInput{mediaType: block.MimeType, data: decoded})
		default:
			return "", fmt.Errorf("unsupported prompt content type %q", block.Type)
		}
	}
	if err := pctx.Err(); err != nil {
		return acp.StopCancelled, nil
	}
	content := make([]llm.ContentBlock, 0, len(blocks))
	imageIndex := 0
	refs := make([]llm.ImageRef, 0, len(images))
	if len(images) > 0 {
		inputs := make([]attachment.ImageInput, 0, len(images))
		for _, input := range images {
			inputs = append(inputs, attachment.ImageInput{MediaType: input.mediaType, Data: input.data})
		}
		batch, err := s.app.attachStore.SaveImages(inputs, maxBytes)
		if err != nil {
			return "", fmt.Errorf("save image prompt: %w", err)
		}
		refs = append(refs, batch...)
	}
	appendText := func(value string) {
		if value == "" {
			return
		}
		if len(content) > 0 && content[len(content)-1].Kind == llm.BlockText {
			content[len(content)-1].Text += value
			return
		}
		content = append(content, llm.Text(value))
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			appendText(block.Text)
		case "resource_link":
			appendText(acp.ResourceLinkText(block.Name, block.URI))
		case "image":
			if err := pctx.Err(); err != nil {
				return acp.StopCancelled, nil
			}
			ref := refs[imageIndex]
			imageIndex++
			content = append(content, llm.ContentBlock{Kind: llm.BlockImage, Image: ref})
		}
	}
	if len(content) == 0 {
		return "", errors.New("prompt content is empty")
	}
	if err := pctx.Err(); err != nil {
		return acp.StopCancelled, nil
	}
	return s.runStartedPrompt(pctx, emit, func(pctx context.Context) error {
		if s.agentHandle != nil {
			return s.agentHandle.RunContent(pctx, content, nil)
		}
		return s.executePromptMessages(pctx, []llm.Message{{Role: llm.RoleUser, Content: content}})
	})
}

func (s *acpSession) runPrompt(ctx context.Context, emit func(acp.Update), run func(context.Context) error) (acp.StopReason, error) {
	pctx, finish, err := s.beginPrompt(ctx)
	if err != nil {
		return "", err
	}
	defer finish()
	return s.runStartedPrompt(pctx, emit, run)
}

// beginPrompt reserves the session's prompt slot before admission starts.
// Image decoding and attachment persistence happen before the Agent turn is
// queued, but ACP cancellation must still own that interval and prevent a
// late user message from being admitted after the client has cancelled it.
func (s *acpSession) beginPrompt(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errors.New("session is closed")
	}
	if s.busy {
		s.mu.Unlock()
		return nil, nil, errors.New("a prompt is already in flight for this session")
	}
	pctx, cancel := context.WithCancel(ctx)
	s.busy = true
	s.cancel = cancel
	s.mu.Unlock()
	return pctx, func() {
		cancel()
		s.mu.Lock()
		s.busy = false
		s.cancel = nil
		s.mu.Unlock()
	}, nil
}

func (s *acpSession) runStartedPrompt(pctx context.Context, emit func(acp.Update), run func(context.Context) error) (acp.StopReason, error) {
	startSeq := s.log.NextSeq()
	err := run(pctx)
	if err == nil && s.app != nil {
		// ACP is another user-facing turn surface. Keep the same non-blocking
		// post-turn projections as Web/REPL, but detach them from a cancelled
		// prompt context so a transport disconnect cannot discard durable memory
		// or title work after the Agent has committed its turn.
		postCtx := context.WithoutCancel(pctx)
		s.app.spillAutoSpillFor(postCtx, s.id, s.log)
		s.app.ensureSessionTitle(postCtx, s.id)
	}
	// ACP exposes committed assistant messages only. Provider chunks remain
	// durable observability events but cannot leak as a partial answer before
	// the owning Agent turn has committed its assistant/message row. Rich
	// blocks are preflighted before any callback so a missing image cannot
	// produce a partially delivered assistant response.
	if deliveryErr := s.emitCommittedAssistantContent(startSeq, emit); err == nil && deliveryErr != nil {
		err = deliveryErr
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

func (s *acpSession) executePrompt(ctx context.Context, text string) error {
	return s.executePromptMessages(ctx, []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(text)}}})
}

func (s *acpSession) executePromptMessages(ctx context.Context, messages []llm.Message) error {
	return s.executePromptMessagesWithAgent(ctx, messages, nil)
}

func (s *acpSession) executePromptMessagesWithAgent(ctx context.Context, messages []llm.Message, runtimeAgent *agent.Agent) error {
	provider, model := s.provider, s.model
	if provider == "" && s.app != nil {
		provider, model = s.app.sessionProviderModel(s.id)
	}
	// ACP owns a real Agent turn too: capture the selected provider generation
	// for the whole prompt rather than only for each Stream call. This keeps a
	// concurrent Web model switch from closing credentials midway through an
	// ACP turn and matches native Agent lifetime semantics.
	runtime := s.app.providerRuntimeSnapshotPinned(provider)
	if runtime.selected == nil {
		return errors.New("ACP LLM provider is unavailable")
	}
	if unavailable, ok := runtime.selected.(unavailableLLM); ok {
		return unavailable.err
	}
	capability := s.app.modelCapabilityForRouteWithConfig(runtime.cfg, provider, model)
	effort := effectiveModelReasoningEffort(capability, s.effort)
	if runtime.release != nil {
		defer runtime.release()
	}
	turn := loop.New(loop.Config{
		LLM:             runtime.selected,
		Log:             s.log,
		Tools:           s.registry,
		ToolSpecs:       func() []llm.ToolSchema { return modelToolSpecs(capability, s.mode, s.registry.VisibleSpecs()) },
		Prompt:          s.prompt,
		Model:           model,
		MaxTokens:       effectiveModelOutputLimit(runtime.cfg.MaxTokens, capability.DefaultMaxTokens, capability.MaxTokens),
		ContextWindow:   capability.ContextWindow,
		Provider:        provider,
		ReasoningEffort: effort,
		PreStep:         append(s.compactionPreSteps(), s.app.extensionPreSteps()...),
		OnText: func(text string) {
			// Deliberately buffered until assistant/message commits; see
			// emitCommittedAssistantContent.
		},
		OnError: func(error) {},
		// ACP tools must use the addressed session's durable event sink just
		// like native Agent runs. Without this runtime binding, context-aware
		// tools fall back to their legacy void callback and can report success
		// after an event append failed.
		RuntimeSessionID: s.id,
		RuntimeAgentID:   s.id,
		RuntimeEmit: func(typ string, data any) error {
			if s.log == nil {
				return errors.New("ACP session log is unavailable")
			}
			_, err := s.log.Append(typ, data)
			return err
		},
	})
	if runtimeAgent != nil {
		// Claim addressed inbox work before compaction and other pre-step
		// projections, and repeat that claim for every proposed continuation
		// step. This is the same ordering as the native Agent bridge; ACP must
		// not defer a message queued during a tool batch to the next prompt.
		turn.PrependPreStepHook(func(hctx context.Context, payload loop.PreStepPayload, next loop.PreStepNext) (loop.PreStepDecision, error) {
			if len(payload.Messages) > 0 {
				return next(hctx, payload)
			}
			input, ok, claimErr := runtimeAgent.ClaimStepWithError()
			if claimErr != nil {
				return loop.PreStepDecision{}, claimErr
			}
			if !ok {
				return next(hctx, payload)
			}
			claimed := make([]llm.Message, 0, len(input.Messages))
			for _, message := range input.Messages {
				content := message.Content
				if len(content) == 0 && strings.TrimSpace(message.Text) != "" {
					content = []llm.ContentBlock{llm.Text(message.Text)}
				}
				if len(content) == 0 {
					continue
				}
				modelMessage := llm.Message{Role: llm.RoleUser, Content: content}
				if message.Kind == agent.MessageSteering {
					if _, appendErr := s.log.Append(session.EventUserMessage, session.NewSteeringUserMessageWithBlocks(message.Text, message.Metadata["source"], message.Content)); appendErr != nil {
						return loop.PreStepDecision{}, appendErr
					}
					modelMessage.Persisted = true
				}
				if teamMessageID := message.Metadata["team_message_id"]; teamMessageID != "" {
					modelMessage.SourceKind = "team-message"
					modelMessage.SourceTeamID = message.Metadata["team_id"]
					modelMessage.SourceMessageID = teamMessageID
					modelMessage.SourceSenderID = message.Metadata["team_sender_id"]
					modelMessage.SourceSenderName = message.Metadata["team_sender_name"]
					modelMessage.Persisted = message.Metadata["team_message_recorded"] == "true"
				}
				claimed = append(claimed, modelMessage)
			}
			proposal := payload
			proposal.Messages = append(cloneRuntimeMessages(payload.Messages), claimed...)
			return next(hctx, proposal)
		})
		turn.AddTurnStoppingHook(func(hctx context.Context, payload loop.TurnStoppingPayload, next loop.TurnStoppingNext) (loop.TurnStoppingDecision, error) {
			input, ok, claimErr := runtimeAgent.ClaimStepWithError()
			if claimErr != nil {
				return loop.TurnStoppingDecision{}, claimErr
			}
			if !ok {
				return next(hctx, payload)
			}
			nextMessages := make([]llm.Message, 0, len(input.Messages))
			for _, message := range input.Messages {
				if message.Kind == agent.MessageSteering {
					if _, err := s.log.Append(session.EventUserMessage, session.NewSteeringUserMessageWithBlocks(message.Text, message.Metadata["source"], message.Content)); err != nil {
						return loop.TurnStoppingDecision{}, err
					}
				}
				content := message.Content
				if len(content) == 0 {
					content = []llm.ContentBlock{llm.Text(message.Text)}
				}
				nextMessages = append(nextMessages, llm.Message{Role: llm.RoleUser, Content: content})
			}
			return loop.TurnStoppingDecision{Stop: false, Messages: nextMessages}, nil
		})
		turn.SetContinueOnCancel(func(hctx context.Context) ([]llm.Message, bool, error) {
			input, ok, claimErr := runtimeAgent.ClaimSteerStepWithError()
			if claimErr != nil || !ok {
				return nil, false, claimErr
			}
			nextMessages := make([]llm.Message, 0, len(input.Messages))
			for _, message := range input.Messages {
				content := message.Content
				if len(content) == 0 {
					content = []llm.ContentBlock{llm.Text(message.Text)}
				}
				persisted := false
				if message.Kind == agent.MessageSteering {
					if _, err := s.log.Append(session.EventUserMessage, session.NewSteeringUserMessageWithBlocks(message.Text, message.Metadata["source"], message.Content)); err != nil {
						return nil, false, err
					}
					persisted = true
				}
				nextMessages = append(nextMessages, llm.Message{Role: llm.RoleUser, Content: content, Persisted: persisted})
			}
			if err := hctx.Err(); err != nil && len(nextMessages) == 0 {
				return nil, false, err
			}
			return nextMessages, true, nil
		})
	}
	return turn.RunMessages(ctx, messages)
}

func (s *acpSession) emitCommittedAssistantContent(after uint64, emit func(acp.Update)) error {
	if emit == nil || s.log == nil {
		return nil
	}
	// Rebuild the committed model-visible surface through the shared
	// projection.  Walking assistant event payloads directly here used to
	// create an ACP-only history authority: it could re-emit a message that a
	// later compaction replacement had already shadowed, or disagree with the
	// loop's cold-recovered history.  SurfaceEntry retains the owning durable
	// sequence, so the delivery cursor can still select only content committed
	// after this turn started.
	snapshot, err := projection.Build(s.log.Events())
	if err != nil {
		return fmt.Errorf("assistant output delivery projection: %w", err)
	}
	updates := make([]acp.Update, 0)
	for _, entry := range snapshot.Surface {
		if entry.Seq < after || entry.Message.Role != llm.RoleAssistant {
			continue
		}
		if len(entry.Message.Content) == 0 && strings.TrimSpace(entry.Message.Text()) != "" {
			updates = append(updates, acp.Update{Text: entry.Message.Text()})
		}
		// Keep the legacy ACP text update shape for the common single-text
		// assistant message. Rich or mixed content stays in typed content
		// updates so image ordering and attachment admission remain explicit.
		if len(entry.Message.Content) == 1 && entry.Message.Content[0].Kind == llm.BlockText {
			if entry.Message.Content[0].Text != "" {
				updates = append(updates, acp.Update{Text: entry.Message.Content[0].Text})
			}
			continue
		}
		for _, block := range entry.Message.Content {
			switch block.Kind {
			case llm.BlockText:
				if block.Text != "" {
					updates = append(updates, acp.Update{Content: &acp.PromptContentBlock{Type: "text", Text: block.Text}})
				}
			case llm.BlockImage:
				if block.Image.ID == "" {
					return errors.New("assistant output delivery failed: image attachment reference is missing")
				}
				if s.app == nil || s.app.attachStore == nil {
					return errors.New("assistant output delivery failed: no attachment store")
				}
				ref, err := s.app.attachStore.GetByID(block.Image.ID)
				if err != nil {
					return fmt.Errorf("assistant output delivery failed: attachment %s is unavailable: %w", block.Image.ID, err)
				}
				data, err := s.app.attachStore.Read(ref)
				if err != nil {
					return fmt.Errorf("assistant output delivery failed: attachment %s is unreadable: %w", block.Image.ID, err)
				}
				mediaType := block.Image.MediaType
				if mediaType == "" {
					mediaType = ref.MediaType
				}
				updates = append(updates, acp.Update{Content: &acp.PromptContentBlock{
					Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: mediaType,
				}})
			}
		}
	}
	for _, update := range updates {
		emit(update)
	}
	return nil
}

func (s *acpSession) compactionPreSteps() []loop.PreStepInjector {
	if s.compaction == nil {
		return nil
	}
	return []loop.PreStepInjector{{Name: "compaction", Inject: s.compactionPreStep(), InjectWithError: s.compactionPreStepWithError(), OncePerTurn: true}}
}

func (s *acpSession) compactionPreStep() func(context.Context, string) []llm.Message {
	return func(ctx context.Context, _ string) []llm.Message {
		messages, _ := s.compactionPreStepWithError()(ctx, "")
		return messages
	}
}

func (s *acpSession) compactionPreStepWithError() func(context.Context, string) ([]llm.Message, error) {
	return func(ctx context.Context, _ string) ([]llm.Message, error) {
		threshold := s.app.providerConfigSnapshot().Compaction.TokenThreshold
		if threshold <= 0 {
			threshold = config.DefaultCompactionTokenThreshold
		}
		if s.compaction == nil {
			return nil, nil
		}
		surfaceTokens, err := acpSurfaceTokens(s.log)
		if err != nil {
			return nil, fmt.Errorf("acp history projection: %w", err)
		}
		if surfaceTokens <= threshold {
			return nil, nil
		}
		if _, err := s.log.Append(session.EventCompactionStart,
			session.NewCompactionStart("surface token estimate exceeded threshold", "pressure")); err != nil {
			return nil, fmt.Errorf("acp compaction start: persist event: %w", err)
		}
		result, err := s.compaction.CompactIfNeeded(ctx, s.log, compaction.TriggerPressure)
		if err != nil {
			if endErr := appendACPCompactionEndError(s.log, err); endErr != nil {
				return nil, errors.Join(err, endErr)
			}
			return nil, err
		}
		if result == nil {
			return nil, nil
		}
		if _, err := s.log.Append(session.EventCompactionSummary,
			session.NewCompactionSummaryWithStats(result.CompactionID, result.Summary, result.ShadowedSeqs, result.ShadowedTokens, "pressure")); err != nil {
			return nil, fmt.Errorf("acp compaction summary: persist event: %w", err)
		}
		if _, err := s.log.Append(session.EventCompactionEnd,
			session.NewCompactionEnd(result.CompactionID, result.ShadowedRange, result.ShadowedTokens)); err != nil {
			return nil, fmt.Errorf("acp compaction end: persist event: %w", err)
		}
		return []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.Text(compactedNotice),
		}}}, nil
	}
}

func appendACPCompactionEndError(log *session.Log, err error) error {
	_, appendErr := log.Append(session.EventCompactionEnd, session.NewCompactionEndError("", err.Error()))
	if appendErr != nil {
		return fmt.Errorf("acp compaction end: persist error event: %w", appendErr)
	}
	return nil
}

func acpSurfaceTokens(log *session.Log) (int, error) {
	if log == nil {
		return 0, errors.New("nil session log")
	}
	snapshot, err := projection.Build(log.Events())
	if err != nil {
		return 0, err
	}
	total := 0
	for _, message := range snapshot.History {
		total += len(message.Text()) / 4
		for _, call := range message.ToolCalls {
			total += len(call.Name)/4 + len(call.Arguments)/4
		}
	}
	return total, nil
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
	// Stop continuable child agents before disposing the parent Agent and the
	// session-owned resources their child registries may use (MCP, terminal,
	// etc.). This is the same child-first ownership boundary used by the
	// reference ACP bridge; otherwise a child can retain a pointer to a parent
	// registry after its owner has already been released.
	if s.subagents != nil {
		if err := s.subagents.Close(); err != nil {
			first = err
		}
	}
	if s.agentRuntime != nil {
		if err := s.agentRuntime.CloseAll(); err != nil && first == nil {
			first = err
		}
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
	if s.approval != nil && !s.sharedApproval {
		if canceler, ok := s.approval.(interact.SessionCanceler); ok {
			cancelled, err := canceler.CancelForSession(context.Background(), s.id)
			if err != nil && first == nil {
				first = err
			}
			for _, request := range cancelled {
				payload := map[string]any{"id": request.ID, "outcome": string(interact.StatusCanceled)}
				if request.CallID != "" {
					payload["callId"] = request.CallID
				}
				if s.log != nil {
					if _, err := s.log.Append(session.EventApprovalDecided, payload); err != nil && first == nil {
						first = err
					}
				}
			}
		}
		if err := s.approval.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.sharedApproval && s.app != nil {
		s.app.clearSessionApprovalPolicy(s.id)
	}
	return first
}
