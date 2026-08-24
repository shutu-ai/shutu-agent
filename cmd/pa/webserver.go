// webserver.go — the M10a composition root for the unified web portal (ADR
// 2026-08-20-m10-web-portal.md D-WEB-7): when web_server.enabled (默认关 D10)
// it builds the bearer-authenticated net/http portal over the read-only store
// and starts the listener on a background goroutine. An empty token fails
// closed at startup (no bare server, D-WEB-2). main defers Close to shut the
// listener at shutdown (lifecycle reversible).
//
// M10 W1 (ADR 2026-08-20-m10-web-workspace.md D-WEB2-A/B/C): this file also
// owns the real-time event hub — attachSink publishes each persisted event and
// the web's SSE streams subscribe per session id — and injects the interactive
// handlers (message dispatch with implicit resume, session new/resume, the
// event source) into the otherwise generic webserver at registration time.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jabing/shutu-agent/internal/compaction"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/webserver"
)

// eventHub is the real-time event broadcaster (ADR D-WEB2-B): attachSink
// publishes every persisted event of the current session, and each SSE stream
// subscribes to one session id. Publish is non-blocking — a slow subscriber
// whose buffer is full is dropped (select default) so the hub can never stall
// the serial persist path; honest: under extreme load SSE may drop an event and
// the frontend falls back on the snapshot plus the later events.
const eventHubBuffer = 256

type eventHub struct {
	mu   sync.Mutex
	subs map[string]map[chan session.Event]struct{}
}

// NewEventHub returns an empty event hub.
func NewEventHub() *eventHub {
	return &eventHub{subs: make(map[string]map[chan session.Event]struct{})}
}

// Publish broadcasts ev to every subscriber of the session (non-blocking: a
// subscriber whose buffer is full is dropped rather than blocking the caller —
// the serial loop/persist path must never wait on a slow SSE consumer).
func (h *eventHub) Publish(sessionID string, ev session.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[sessionID] {
		select {
		case ch <- ev:
		default:
			// Buffer full: drop this slow subscriber.
		}
	}
}

// Subscribe registers a buffered subscriber channel for a session and returns
// the channel plus an unsubscribe closure. The closure unsubscribes and closes
// the channel, so a reader's range loop ends.
func (h *eventHub) Subscribe(sessionID string) (chan session.Event, func()) {
	ch := make(chan session.Event, eventHubBuffer)
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[chan session.Event]struct{})
	}
	h.subs[sessionID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if set := h.subs[sessionID]; set != nil {
			if _, ok := set[ch]; ok {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(h.subs, sessionID)
			}
		}
		h.mu.Unlock()
	}
}

// SubscribeInto subscribes to a session and forwards every event to sink on a
// background goroutine. The returned func unsubscribes and stops the forwarder
// (the subscriber channel is closed, ending the forwarder's range loop).
func (h *eventHub) SubscribeInto(sessionID string, sink func(session.Event)) func() {
	ch, unsub := h.Subscribe(sessionID)
	go func() {
		for ev := range ch {
			sink(ev)
		}
	}()
	return unsub
}

func (a *app) registerWebServer() error {
	if !a.cfg.WebServer.Enabled {
		return nil // D10: not registered when disabled
	}
	srv, err := webserver.New(a.store, a.cfg.WebServer.Token, a.cfg.WebServer.Addr)
	if err != nil {
		return fmt.Errorf("register web server: %w", err)
	}
	if a.hub == nil {
		a.hub = NewEventHub()
	}
	// M10 W1 (ADR D-WEB2): inject the interactive handlers — message dispatch
	// (with implicit resume), session new/resume and the real-time event source
	// (the hub). The webserver stays generic; cmd/pa provides the behavior.
	srv.SetMessageHandler(func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error {
		return a.webMessage(ctx, sessionID, text, images)
	})
	srv.SetSessionManager(func(ctx context.Context, action, id string) (string, error) {
		return a.webSessionManager(ctx, action, id)
	})
	srv.SetEventSource(func(sessionID string, sink func(session.Event)) func() {
		return a.hub.SubscribeInto(sessionID, sink)
	})
	// dsh-session-status: wire the live per-session status computation so the
	// sidebar renders the status dot + hover card from runtime state (running
	// turn / running subagents / pending interaction / finished-but-unviewed).
	srv.SetSessionStatusProvider(a.sessionStatus)
	// M10 W2 (ADR D-WEB2-D): inject the sanitized config view. webConfig never
	// exposes web_server.token or any key — the webserver only forwards it.
	srv.SetConfigProvider(a.webConfig)
	srv.SetContextWindow(a.contextWindowOf)
	srv.SetTurnStopper(a.stopTurn)
	// M10 W4 (ADR D-WEB2-H): inject the read-only subagent and background-job
	// panels. Each provider returns sanitized views (id/status/timestamps only);
	// a disabled capability answers an empty list, never an error.
	srv.SetSubagentProvider(a.webSubagents)
	srv.SetJobsProvider(a.webJobs)
	// M10 P5 (ADR D-WEB2-I): wire the image-attachment store when multimodal is
	// enabled (registerAttachments created it); otherwise the attachment APIs
	// stay at 501 and image-carrying messages answer 501/400.
	if a.attachStore != nil {
		srv.SetAttachmentStore(a.attachStore)
	}
	// P5.1 (模型选择实时生效): wire the live model switch.
	srv.SetModelSwitcher(func(ctx context.Context, provider, model, effort string) error {
		return a.webSwitchModel(ctx, provider, model, effort)
	})
	// M11 (增加提供方 / 增加自定义提供方): wire the provider-management API. A
	// "save" of a built-in provider stores only the API-key override (custom:false);
	// a "save" of a custom provider (custom:true) persists the full profile + key;
	// "delete" removes a custom provider. All apply immediately via registerLLM.
	srv.SetProviderManager(func(ctx context.Context, action string, edit webserver.ProviderEdit) error {
		switch action {
		case "save":
			models := make([]customModel, 0, len(edit.Models))
			for _, m := range edit.Models {
				models = append(models, customModel{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens})
			}
			if edit.Custom {
				return a.webSaveCustomProvider(ctx, edit.ID, edit.Name, edit.BaseURL, edit.Model, edit.APIKey, edit.Protocol, models)
			}
			return a.webSaveProvider(ctx, edit.ID, edit.APIKey, edit.BaseURL, edit.Model, models)
		case "delete":
			return a.webDeleteCustomProvider(ctx, edit.ID)
		default:
			return fmt.Errorf("unknown provider action %q", action)
		}
	})
	// M11-pi-ai (模型探测, dsh discovery 对齐): wire the 获取可用模型 API so the
	// 增加自定义提供方 / 编辑卡 can fill the multi-model list from the endpoint.
	srv.SetProviderDiscover(func(ctx context.Context, req webserver.ProviderDiscover) ([]webserver.ProviderModel, error) {
		models, err := a.webDiscoverModels(ctx, discoverRequest{
			Provider: req.Provider,
			BaseURL:  req.BaseURL,
			Protocol: req.Protocol,
			APIKey:   req.APIKey,
		})
		if err != nil {
			return nil, err
		}
		out := make([]webserver.ProviderModel, 0, len(models))
		for _, m := range models {
			out = append(out, webserver.ProviderModel{ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens})
		}
		return out, nil
	})
	// 技能设置页 (dsh-skill-mcp-panel 对齐): wire the skill-management API. The
	// manager is created lazily (independent of skill.enabled) so the page
	// always lists the skill files it manages.
	srv.SetSkillManager(a.webSkills)
	a.webserver = srv
	go func() {
		if err := srv.Serve(); err != nil {
			fmt.Fprintln(os.Stderr, "pa: web server:", err)
		}
	}()
	return nil
}

// webMessage handles one web chat message for a session (ADR D-WEB2-A): when
// the target session differs from the current one it is resumed first (attachSink
// already rebinds to the new session), then the turn runs under the global serial
// lock with a silent loop (chunks already persist; the SSE event stream renders
// the flow). P5: an images list logs a user/message event carrying the image
// blocks first (only the refs — the bytes live in the attachment store, same
// path as /attach, D4: the loop is untouched), then the text turn runs.
func (a *app) webMessage(ctx context.Context, sessionID, text string, images []llm.ImageRef) error {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return errors.New("empty message text")
	}
	if sessionID != "" && sessionID != a.currentID {
		if err := a.resumeSession(ctx, sessionID); err != nil {
			return err
		}
	}
	// dsh 斜杠命令 (输入条 "/"): a leading "/" routes to a command handler that
	// appends a rendered result to the session — no LLM turn. Session-switching
	// commands (/new, /resume) stay on the sidebar/+ menu, which already drive
	// them through the session manager.
	if len(images) == 0 && strings.HasPrefix(strings.TrimSpace(text), "/") {
		a.turnMu.Lock()
		err := a.webCommand(ctx, strings.TrimSpace(text))
		a.turnMu.Unlock()
		if err != nil {
			return err
		}
		return a.runIdleGoal(ctx, false)
	}
	if len(images) > 0 {
		if !a.multimodalEnabled() || a.attachStore == nil {
			return fmt.Errorf("multimodal disabled (llm.multimodal.enabled=false)")
		}
		blocks := make([]llm.ContentBlock, 0, len(images))
		for _, img := range images {
			blocks = append(blocks, llm.ContentBlock{Kind: llm.BlockImage, Image: img})
		}
		if a.log == nil {
			return fmt.Errorf("no active session")
		}
		if _, err := a.log.Append(session.EventUserMessage, session.NewUserMessageWithBlocks("", blocks)); err != nil {
			return fmt.Errorf("web message: log image: %w", err)
		}
	}
	// A cancellable turn context so POST /api/sessions/{id}/stop can abort this
	// turn (dsh 停止按钮) without touching the request context (which the
	// handler returns, cancelling it, after the turn completes).
	turnCtx, cancel := context.WithCancel(ctx)
	a.setTurnCancel(cancel)
	defer func() { a.clearTurnCancel(); cancel() }()
	if err := a.runTurn(turnCtx, text, false); err != nil {
		return err
	}
	// session-title alignment (dsh): after the first eligible message, the
	// deterministic fallback is stored and the asynchronous model title is
	// scheduled. This runs after the turn, outside turnMu, so it never delays
	// the answer.
	a.ensureSessionTitle(ctx, sessionID)
	// Goal driver idle/followup: the outer web turn has settled, so each
	// continuation round can acquire the shared turn lock independently.
	if err := a.runIdleGoal(ctx, false); err != nil {
		return err
	}
	// dsh-session-status: keep this session out of the finished-but-unviewed
	// reminder while the user is on it (the turn above bumped updated_at past
	// the previous view, so this restores last_viewed_at >= updated_at).
	a.markSessionViewed(ctx, sessionID)
	return nil
}

// webCommand handles a leading "/" in a web composer message (dsh 斜杠命令
// 对齐, ①③⑤): it appends the command as a user/message, dispatches it, and
// appends the result as an assistant/message so the web chat renders the whole
// exchange without an LLM turn. The events flow through the same event hub as
// a turn, so the SSE stream renders them. Session-switching commands (/new,
// /resume) are deliberately not routed here — the sidebar and the composer "+"
// menu already drive them through the session manager.
func (a *app) webCommand(ctx context.Context, line string) error {
	fields := strings.Fields(line)
	name := fields[0]
	args := fields[1:]
	if _, err := a.log.Append(session.EventUserMessage, session.NewUserMessage(line)); err != nil {
		return err
	}
	result, err := a.execWebCommand(ctx, name, args)
	if err != nil {
		result = "⚠ " + err.Error()
	}
	if _, err := a.log.Append(session.EventAssistantMessage, session.NewAssistantMessage(result, nil, "stop")); err != nil {
		return err
	}
	return nil
}

// execWebCommand dispatches a single web slash command and returns the
// model-facing result text (a non-nil error means an invalid command or bad
// args; the caller renders it as the assistant reply).
func (a *app) execWebCommand(ctx context.Context, name string, args []string) (string, error) {
	switch name {
	case "/help":
		return a.webHelp(), nil
	case "/status":
		return a.webStatus(), nil
	case "/compact":
		return a.webCompact(ctx, args)
	case "/permission":
		return a.webPermission(ctx, args)
	case "/goal", "/plan":
		return a.webPlanGoal(ctx, args)
	default:
		return "", fmt.Errorf("unknown command %q (try /help)", name)
	}
}

// webHelp returns the web composer's slash-command table (dsh 输入条命令对齐).
func (a *app) webHelp() string {
	return "可用的斜杠命令:\n" +
		"  /help               显示本命令表\n" +
		"  /status             显示当前 provider / model / mode\n" +
		"  /compact [region <start> <end>]  手动压缩上下文\n" +
		"  /permission [readonly|standard|full]  查看或切换权限\n" +
		"  /goal <标题> [说明]   创建目标 (plan_goal)\n" +
		"  /plan <标题> [说明]   创建目标 (plan 模式入口)\n" +
		"  其他文本             发送给智能体"
}

// webStatus returns the current provider / model / mode summary (dsh 输入条
// 状态命令, mirrors the REPL /help's llm line).
func (a *app) webStatus() string {
	return fmt.Sprintf("provider=%s model=%s mode=%s",
		a.cfg.LLM.Provider, llmProviderModel(a.cfg, a.cfg.LLM.Provider), a.cfg.Mode)
}

// webCompact runs the same manual compaction as the REPL and formats the
// report as an assistant message for the Web SSE stream.
func (a *app) webCompact(ctx context.Context, args []string) (string, error) {
	if a.compaction == nil {
		return "compaction: disabled (compaction.enabled=false)", nil
	}
	var res *compaction.Result
	var err error
	switch {
	case len(args) == 3 && args[0] == "region":
		start, e1 := strconv.ParseInt(args[1], 10, 64)
		end, e2 := strconv.ParseInt(args[2], 10, 64)
		if e1 != nil || e2 != nil {
			return "", fmt.Errorf("usage: /compact region <start> <end> (integer event seqs)")
		}
		res, err = a.compactAndLog(ctx, "manual /compact region command", "manual",
			func() (*compaction.Result, error) { return a.compaction.CompactRegion(ctx, a.log, start, end) })
	case len(args) != 0:
		return "", fmt.Errorf("usage: /compact or /compact region <start> <end>")
	default:
		res, err = a.compactAndLog(ctx, "manual /compact command", "manual",
			func() (*compaction.Result, error) { return a.compaction.CompactNow(ctx, a.log) })
	}
	if err != nil {
		return "", err
	}
	if res == nil {
		return "compaction: nothing to compact", nil
	}
	return fmt.Sprintf("compacted %d events (seq %d..%d), saved %d tokens (id %s)\nsummary: %s",
		len(res.ShadowedSeqs), res.ShadowedRange[0], res.ShadowedRange[1], res.ShadowedTokens, res.CompactionID, res.Summary), nil
}

// webPermission implements dsh's /permission command. With no argument it
// reports the effective preset; with one argument it persists a session
// override when a session is active, otherwise the global default.
func (a *app) webPermission(ctx context.Context, args []string) (string, error) {
	const available = "readonly, standard, full"
	if len(args) > 1 {
		return "", fmt.Errorf("usage: /permission [readonly|standard|full]")
	}
	current := "standard"
	if a.currentID != "" {
		if scs, ok := a.store.(store.SessionConfigStore); ok {
			cfg, err := scs.GetSessionConfig(ctx, a.currentID)
			if err != nil {
				return "", err
			}
			if cfg.Permission != "" {
				current = cfg.Permission
			} else {
				settings, err := a.store.GetSettings(ctx)
				if err != nil {
					return "", err
				}
				if value := settings["permission_preset"]; value != "" {
					current = value
				}
			}
		}
	} else {
		settings, err := a.store.GetSettings(ctx)
		if err != nil {
			return "", err
		}
		if value := settings["permission_preset"]; value != "" {
			current = value
		}
	}
	if len(args) == 0 {
		return fmt.Sprintf("current preset %s (available: %s)", current, available), nil
	}
	next := args[0]
	if next != "readonly" && next != "standard" && next != "full" {
		return "", fmt.Errorf("unknown preset %q (available: %s)", next, available)
	}
	if a.currentID != "" {
		scs, ok := a.store.(store.SessionConfigStore)
		if !ok {
			return "", errors.New("session permission overrides are unsupported by this store")
		}
		cfg, err := scs.GetSessionConfig(ctx, a.currentID)
		if err != nil {
			return "", err
		}
		if err := scs.UpdateSessionConfig(ctx, a.currentID, cfg.Provider, cfg.Model, cfg.ReasoningEffort, next); err != nil {
			return "", err
		}
	} else if err := a.store.SetSetting(ctx, "permission_preset", next); err != nil {
		return "", err
	}
	return fmt.Sprintf("preset %s", next), nil
}

// webCommandCatalog is the backend-owned discovery view used by the web
// composer.
func (a *app) webCommandCatalog() []map[string]string {
	out := make([]map[string]string, 6)
	out[0] = make(map[string]string)
	out[0][`name`] = `help`
	out[0][`hint`] = `Show available slash commands`
	out[1] = make(map[string]string)
	out[1][`name`] = `status`
	out[1][`hint`] = `Show provider, model and mode`
	out[2] = make(map[string]string)
	out[2][`name`] = `compact`
	out[2][`hint`] = `Compact context: /compact [region start end]`
	out[3] = make(map[string]string)
	out[3][`name`] = `permission`
	out[3][`hint`] = `Show or set permission: /permission [readonly|standard|full]`
	out[4] = make(map[string]string)
	out[4][`name`] = `goal`
	out[4][`hint`] = `Create a goal: /goal title [details]`
	out[5] = make(map[string]string)
	out[5][`name`] = `plan`
	out[5][`hint`] = `Create a goal in plan mode`
	return out
}

// webPlanGoal creates a goal via the plan_goal tool (dsh /goal /plan entry). It
// returns the tool's model-facing output; the plan/create fact also lands in
// the session log (D3). When plan is disabled the tool is unregistered and
// Execute reports it.
func (a *app) webPlanGoal(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: /goal <标题> [目标说明]")
	}
	title := args[0]
	objective := strings.Join(args[1:], " ")
	payload, err := json.Marshal(map[string]any{"title": title, "objective": objective})
	if err != nil {
		return "", err
	}
	res, err := a.reg.Execute(ctx, "plan_goal", payload)
	if err != nil {
		return "", err
	}
	return res.Output, nil
}

// webSessionManager implements the session new/resume API (ADR D-WEB2-C),
// reusing the REPL's newSession/resumeSession.
func (a *app) webSessionManager(ctx context.Context, action, id string) (string, error) {
	switch action {
	case "new":
		if err := a.newSession(ctx); err != nil {
			return "", err
		}
		return a.currentID, nil
	case "resume":
		if err := a.resumeSession(ctx, id); err != nil {
			return "", err
		}
		return a.currentID, nil
	default:
		return "", fmt.Errorf("unknown session action %q", action)
	}
}

// webConfig returns the sanitized, flat configuration view served by
// GET /api/config (M10 W2, ADR D-WEB2-D): model/provider/mode, each capability
// gate's enabled flag and the web-server address. Secrets never leave —
// web_server.token is omitted entirely
// (keys live in the environment, never in this config), so a compromised
// settings page cannot leak credentials. Field names are snake_case. P5.1 adds
// the live model panel: the currently active provider's model plus the
// registered providers (id/available/model/candidates) for the pickers.
// builtinContextWindows are the known DeepSeek catalog defaults (dsh
// llm-deepseek DEFAULT_CONTEXT_WINDOW: 1,000,000 for both V4 models); unknown
// models fall back to the webserver's defaultContextWindow (1M, dsh default).
var builtinContextWindows = map[string]int{
	"deepseek-v4-flash": 1000000,
	"deepseek-v4-pro":   1000000,
}

// contextWindowOf resolves the effective model's context window for the
// ContextMeter (dsh resolveModelInfo: the configured model-directory entry's
// capacity wins, then the catalog default). It honors the per-session
// provider+model selection (store assertion, same as the webserver's config
// handlers) and falls back to the global selection. An unknown model returns
// 0 and the webserver applies its own defaultContextWindow.
func (a *app) contextWindowOf(sessionID string) int {
	provider, model := "", ""
	if scs, ok := a.store.(store.SessionConfigStore); ok && sessionID != "" {
		if cfg, err := scs.GetSessionConfig(context.Background(), sessionID); err == nil {
			if cfg.Provider != "" {
				provider = cfg.Provider
			}
			if cfg.Model != "" {
				model = cfg.Model
			}
		}
	}
	if provider == "" {
		provider = a.cfg.LLM.Provider
	}
	if model == "" {
		model = a.cfg.Model
	}
	if model == "" {
		return 0
	}
	// The configured directory is authoritative for its provider (dsh
	// resolveModelInfo: configured?.contextWindow first).
	if w := a.directoryContextWindow(provider, model); w > 0 {
		return w
	}
	if w, ok := builtinContextWindows[model]; ok {
		return w
	}
	return 0
}

// directoryContextWindow looks up the configured model-directory entry's
// context window for (provider, model): the persisted built-in profile models
// (llm.profile.<id>.models) or the custom provider's model list. 0 means the
// entry is absent or carries no capacity.
func (a *app) directoryContextWindow(provider, model string) int {
	if provider == "" {
		return 0
	}
	if bp, ok := a.builtinProfiles[provider]; ok {
		for _, m := range bp.Models {
			if m.ID == model {
				return m.ContextWindow
			}
		}
	}
	for _, cp := range a.customProviders {
		if cp.ID == provider {
			for _, m := range cp.Models {
				if m.ID == model {
					return m.ContextWindow
				}
			}
		}
	}
	return 0
}

// setTurnCancel registers the web turn's cancel func for the running turn.
func (a *app) setTurnCancel(cancel context.CancelFunc) {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	a.turnCancel = cancel
}

// clearTurnCancel drops the registered cancel func once the turn settles.
func (a *app) clearTurnCancel() {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	a.turnCancel = nil
}

// stopTurn cancels the running web turn (POST /api/sessions/{id}/stop). It is a
// no-op when no turn is in flight; returns an error only when the id does not
// match the session whose turn is running.
func (a *app) stopTurn(sessionID string) error {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.turnCancel == nil {
		return errors.New("no turn running")
	}
	if sessionID != "" && sessionID != a.currentID {
		return errors.New("turn belongs to another session")
	}
	a.turnCancel()
	return nil
}

func (a *app) webConfig() map[string]any {
	return map[string]any{
		`commands`:         a.webCommandCatalog(),
		"model":            llmProviderModel(a.cfg, a.cfg.LLM.Provider),
		"base_url":         a.cfg.BaseURL,
		"llm_provider":     a.cfg.LLM.Provider,
		"reasoning_effort": a.cfg.ReasoningEffort,
		"mode":             a.cfg.Mode,
		"providers":        a.webProviders(), // P5.1 live model pickers

		// Capability gates (dsh 对齐: 默认全开, nil*bool→on; 显式 enabled:false 关).
		"terminal_enabled":   config.Enabled(a.cfg.Terminal.Enabled),
		"fs_enabled":         config.Enabled(a.cfg.Fs.Enabled),
		"fs_search_enabled":  config.Enabled(a.cfg.FsSearch.Enabled),
		"ralph_enabled":      config.Enabled(a.cfg.Ralph.Enabled),
		"workflow_enabled":   config.Enabled(a.cfg.Workflow.Enabled),
		"kb_enabled":         config.Enabled(a.cfg.KB.Enabled),
		"jobs_enabled":       config.Enabled(a.cfg.Jobs.Enabled),
		"subagent_enabled":   config.Enabled(a.cfg.Subagent.Enabled),
		"web_enabled":        config.Enabled(a.cfg.Web.Enabled),
		"eval_enabled":       config.Enabled(a.cfg.Eval.Enabled),
		"code_enabled":       config.Enabled(a.cfg.Code.Enabled),
		"interact_enabled":   config.Enabled(a.cfg.Interact.Enabled),
		"mcp_enabled":        config.Enabled(a.cfg.Mcp.Enabled),
		"skill_enabled":      config.Enabled(a.cfg.Skill.Enabled),
		"schedule_enabled":   config.Enabled(a.cfg.Schedule.Enabled),
		"plan_enabled":       config.Enabled(a.cfg.Plan.Enabled),
		"spill_enabled":      config.Enabled(a.cfg.Spill.Enabled),
		"compaction_enabled": config.Enabled(a.cfg.Compaction.Enabled),
		"multimodal_enabled": a.multimodalEnabled(),

		"web_server_addr": a.cfg.WebServer.Addr,
	}
}

// modelReasoning describes one model's selectable thinking efforts (dsh
// ModelSelect 思考强度 对齐): the offered levels and the default. effort IDs
// match the deepseek wire (`reasoning_effort`), "off" meaning thinking
// disabled. Absent means the model offers no effort choice.
type modelReasoning struct {
	Efforts       []modelEffort `json:"efforts"`
	DefaultEffort string        `json:"default_effort,omitempty"`
}

// modelEffort is one selectable effort level.
type modelEffort struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// deepseekReasoning mirrors dsh's llm-deepseek catalog for the V4 models
// (REASONING_EFFORTS: off/low/high/max, default high).
var deepseekReasoning = modelReasoning{
	Efforts: []modelEffort{
		{ID: "off", Name: "Off"},
		{ID: "low", Name: "Low"},
		{ID: "high", Name: "High"},
		{ID: "max", Name: "Max"},
	},
	DefaultEffort: "high",
}

// reasoningFor returns the reasoning capability for a candidate model of a
// provider ("" → none). DeepSeek's V4 models offer off/high/max; everything
// else has no effort selector yet.
func reasoningFor(provider, model string) *modelReasoning {
	if provider == "deepseek-official" && (model == "deepseek-v4-flash" || model == "deepseek-v4-pro") {
		r := deepseekReasoning
		return &r
	}
	return nil
}

// providerReasoning returns the per-model reasoning catalog for a built-in
// provider: model id → its effort choices. Only providers whose models declare
// a reasoning capability contribute entries; the rest return an empty map.
func providerReasoning(id string) map[string]modelReasoning {
	out := map[string]modelReasoning{}
	for _, m := range modelCandidates(id) {
		if r := reasoningFor(id, m); r != nil {
			out[m] = *r
		}
	}
	return out
}

// webProviders returns the known providers for the P5.1/M11 model pickers:
// every built-in provider (deepseek always; openai/anthropic even when their
// credential is absent, so the "增加提供方" setup flow can configure them) plus
// every M11 custom provider declared in settings. Each entry carries its id,
// whether it is a custom provider, registration/availability state, the
// configured-key state (configured = a key is present in settings or env), its
// current model, base_url, suggested model candidates and the env var that
// carries its credential. Only these leaf fields leave the process — never
// keys, prompts or tokens.
func (a *app) webProviders() []map[string]any {
	a.llmMu.RLock()
	reg := a.llmReg
	a.llmMu.RUnlock()
	if reg == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(builtinProviders)+len(a.customProviders))
	// Built-in provider directory (M11-pi-ai): every provider pi-ai can
	// authenticate with an API key is listed, registered or not, so the settings
	// page can add their key. deepseek/openai/anthropic keep their
	// config-driven model/base_url (config.yaml llm.* sections); the rest carry
	// the directory default.
	for _, bp := range builtinProviders {
		model := bp.model
		baseURL := bp.baseURL
		if bp.id == "deepseek-official" || bp.id == "openai" || bp.id == "anthropic" {
			model = llmProviderModel(a.cfg, bp.id)
			baseURL = llmProviderBaseURL(a.cfg, bp.id)
		}
		// dsh ProviderEditor 自定义设置 对齐: a persisted llm.profile.<id>
		// override wins over config.yaml for base URL / model / model list.
		prof, overridden := a.builtinProfiles[bp.id]
		if overridden {
			if prof.BaseURL != "" {
				baseURL = prof.BaseURL
			}
			if prof.Model != "" {
				model = prof.Model
			}
		}
		registered := false
		available := false
		if p, err := reg.Get(bp.id); err == nil {
			registered = true
			available = p.Available()
		}
		entry := map[string]any{
			"id":               bp.id,
			"name":             bp.name,
			"protocol":         string(bp.protocol),
			"protocol_label":   protocolLabel(bp.protocol),
			"custom":           false,
			"registered":       registered,
			"available":        available,
			"configured":       a.providerKey(bp.id) != "",
			"model":            model,
			"base_url":         baseURL,
			"candidates":       modelCandidates(bp.id),
			"env_var":          providerEnv(bp.id),
			"reasoning":        providerReasoning(bp.id),
			"profile_override": overridden,
		}
		if overridden && len(prof.Models) > 0 {
			entry["models"] = prof.Models
		}
		out = append(out, entry)
	}
	// M11 custom providers from settings.
	for _, cp := range a.customProviders {
		registered := false
		available := false
		if p, err := reg.Get(cp.ID); err == nil {
			registered = true
			available = p.Available()
		}
		out = append(out, map[string]any{
			"id":             cp.ID,
			"name":           cp.Name,
			"custom":         true,
			"registered":     registered,
			"available":      available,
			"configured":     a.providerKey(cp.ID) != "",
			"model":          cp.Model,
			"base_url":       cp.BaseURL,
			"candidates":     nil,
			"env_var":        llmKeyEnv(cp.ID),
			"protocol":       cp.Protocol,
			"protocol_label": protocolLabel(providerProtocol(cp.Protocol)),
			"models":         cp.Models,
			"reasoning":      nil,
		})
	}
	return out
}

// webSaveProvider persists a provider edit for a built-in provider (M11, POST
// /api/config/provider): it writes the API-key override (llm.key.<id>) and,
// when the edit carries 自定义设置 changes (dsh ProviderEditor 对齐), a profile
// override (llm.profile.<id> = base_url / model / model list), then rebuilds the
// registry so the change applies immediately (no restart). It runs under turnMu
// (D5 serial). An empty api_key removes the key override, falling back to the
// env var; an empty base_url/model/models removes the profile override.
func (a *app) webSaveProvider(ctx context.Context, id, apiKey, baseURL, model string, models []customModel) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.llmReg == nil {
		return fmt.Errorf("llm not registered")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("provider id is required")
	}
	if apiKey != "" {
		if err := a.store.SetSetting(ctx, "llm.key."+id, apiKey); err != nil {
			return err
		}
		if a.llmKeys == nil {
			a.llmKeys = map[string]string{}
		}
		a.llmKeys[id] = apiKey
	} else {
		if err := a.store.DeleteSetting(ctx, "llm.key."+id); err != nil {
			return err
		}
		delete(a.llmKeys, id)
	}
	// 自定义设置 override: persist base_url / model / model list when any is
	// non-empty; an all-empty edit clears a stored profile back to config.yaml.
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	cleaned := models[:0]
	for _, m := range models {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" {
			continue
		}
		cleaned = append(cleaned, m)
	}
	models = cleaned
	if baseURL != "" || model != "" || len(models) > 0 {
		if model == "" && len(models) > 0 {
			model = models[0].ID
		}
		raw, err := json.Marshal(builtinProviderProfile{BaseURL: baseURL, Model: model, Models: models})
		if err != nil {
			return err
		}
		if err := a.store.SetSetting(ctx, "llm.profile."+id, string(raw)); err != nil {
			return err
		}
		if a.builtinProfiles == nil {
			a.builtinProfiles = map[string]builtinProviderProfile{}
		}
		a.builtinProfiles[id] = builtinProviderProfile{BaseURL: baseURL, Model: model, Models: models}
	} else if _, ok := a.builtinProfiles[id]; ok {
		if err := a.store.DeleteSetting(ctx, "llm.profile."+id); err != nil {
			return err
		}
		delete(a.builtinProfiles, id)
	}
	// Rebuild the registry so the new key/profile is live immediately.
	if err := a.registerLLM(); err != nil {
		return err
	}
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	return nil
}

// webSaveCustomProvider persists a custom provider declaration (M11, POST
// /api/config/provider with custom:true): it validates the profile (id/name/
// base_url + at least one model, wire protocol when given), stores
// llm.custom.<id> + an optional llm.key.<id> override and rebuilds the registry
// immediately. The protocol is one of the four supported wire protocols
// (M11-pi-ai); an empty protocol means the OpenAI-compatible default. models is
// the multi-model list (M11-pi-ai ModelListEditor 对齐): the effective default
// model is the first entry, or the legacy single model argument when the list
// is empty (a hand-declared provider needs at least one).
func (a *app) webSaveCustomProvider(ctx context.Context, id, name, baseURL, model, apiKey, protocol string, models []customModel) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.llmReg == nil {
		return fmt.Errorf("llm not registered")
	}
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	protocol = strings.TrimSpace(protocol)
	if id == "" || baseURL == "" {
		return errors.New("id and base_url are required")
	}
	if name == "" {
		// dsh CustomProviderCard: the display name is optional and defaults to
		// the route id (displayName.length === 0 → omitted from the profile).
		name = id
	}
	if _, ok := builtinProviderByID(id); ok {
		return errors.New("custom provider id conflicts with a built-in provider")
	}
	if !validProviderRoute(id) {
		return errors.New("provider id must start with a lowercase letter and contain only lowercase letters, digits or single '-' separators")
	}
	if protocol != "" && !validProtocol(protocol) {
		return errors.New("protocol must be one of openai-completions, anthropic-messages, google-generative-ai, openai-responses")
	}
	if protocol == "" {
		protocol = string(protocolCompletions)
	}
	// Validate the model list: at least one entry, each with a non-empty id.
	// The effective default model is the first entry; a legacy single model
	// (no list) is accepted as-is.
	if len(models) > 0 {
		cleaned := models[:0]
		for _, m := range models {
			m.ID = strings.TrimSpace(m.ID)
			if m.ID == "" {
				continue
			}
			cleaned = append(cleaned, m)
		}
		models = cleaned
	}
	if len(models) > 0 {
		model = models[0].ID
	} else if model == "" {
		return errors.New("at least one model is required")
	}
	raw, err := json.Marshal(customProviderProfile{ID: id, Name: name, BaseURL: baseURL, Model: model, Protocol: protocol, Models: models})
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(ctx, "llm.custom."+id, string(raw)); err != nil {
		return err
	}
	if apiKey != "" {
		if err := a.store.SetSetting(ctx, "llm.key."+id, apiKey); err != nil {
			return err
		}
		if a.llmKeys == nil {
			a.llmKeys = map[string]string{}
		}
		a.llmKeys[id] = apiKey
	}
	profile := customProviderProfile{ID: id, Name: name, BaseURL: baseURL, Model: model, Protocol: protocol, Models: models}
	replaced := false
	for i := range a.customProviders {
		if a.customProviders[i].ID == id {
			a.customProviders[i] = profile
			replaced = true
			break
		}
	}
	if !replaced {
		a.customProviders = append(a.customProviders, profile)
	}
	if err := a.registerLLM(); err != nil {
		return err
	}
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	return nil
}

// webDeleteCustomProvider removes a custom provider declaration (M11, DELETE
// /api/config/provider): it deletes llm.custom.<id> and its key override, then
// rebuilds the registry. Built-in providers cannot be removed.
func (a *app) webDeleteCustomProvider(ctx context.Context, id string) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.llmReg == nil {
		return fmt.Errorf("llm not registered")
	}
	id = strings.TrimSpace(id)
	if _, ok := builtinProviderByID(id); ok {
		return errors.New("built-in providers cannot be removed")
	}
	found := false
	for _, cp := range a.customProviders {
		if cp.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("custom provider %q not found", id)
	}
	if err := a.store.DeleteSetting(ctx, "llm.custom."+id); err != nil {
		return err
	}
	if err := a.store.DeleteSetting(ctx, "llm.key."+id); err != nil {
		return err
	}
	kept := a.customProviders[:0]
	for _, cp := range a.customProviders {
		if cp.ID != id {
			kept = append(kept, cp)
		}
	}
	a.customProviders = kept
	delete(a.llmKeys, id)
	if err := a.registerLLM(); err != nil {
		return err
	}
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	return nil
}

// validProviderRoute reports whether id is a safe custom-provider route
// (dsh ROUTE_PATTERN /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/ 对齐): lowercase letters
// and digits, '-' only as a separator between alphanumeric runs — a leading
// letter is required so the derived credential name (<ROUTE>_API_KEY) is a
// valid shell identifier, and a trailing or doubled '-' is rejected.
func validProviderRoute(id string) bool {
	if id == "" {
		return false
	}
	if !(id[0] >= 'a' && id[0] <= 'z') {
		return false
	}
	prevDash := false
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			prevDash = false
		case c == '-':
			if prevDash {
				return false // doubled '-'
			}
			prevDash = true
		default:
			return false
		}
	}
	return !prevDash // no trailing '-'
}

// webSwitchModel implements POST /api/config/model (P5.1, 模型选择实时生效): it
// validates and applies a live provider/model/reasoning-effort change, then
// rebuilds the selected LLM provider — no restart. It runs under turnMu (D5
// serial: no turn is in flight while the selection swaps) and registerLLM
// publishes the new pointer under llmMu, so the very next message (buildLoop
// re-wires every turn) talks to the new provider. The change is runtime-only:
// config.yaml stays the source of truth for the next launch. Fail-closed: on
// error the previous selection is fully restored.
func (a *app) webSwitchModel(ctx context.Context, provider, model, effort string) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.llmReg == nil {
		return fmt.Errorf("llm not registered")
	}
	if provider != "" {
		p, err := a.llmReg.Get(provider)
		if err != nil {
			return fmt.Errorf("unknown provider %q (registered: %s)", provider, llmProviderIDs(a.llmReg))
		}
		if !p.Available() {
			return fmt.Errorf("provider %q not available (missing %s)", provider, llmCredentialEnv(provider))
		}
	}
	target := provider
	if target == "" {
		target = a.cfg.LLM.Provider
	}
	// Snapshot for rollback.
	oldProvider := a.cfg.LLM.Provider
	oldModel, oldOpenAI, oldAnthropic := a.cfg.Model, a.cfg.LLM.OpenAI.Model, a.cfg.LLM.Anthropic.Model
	oldEffort := a.cfg.ReasoningEffort
	if provider != "" {
		a.cfg.LLM.Provider = provider
	}
	if model != "" {
		switch target {
		case "openai":
			a.cfg.LLM.OpenAI.Model = model
		case "anthropic":
			a.cfg.LLM.Anthropic.Model = model
		default:
			a.cfg.Model = model
		}
	}
	// dsh 思考强度 (ModelSelect effort): "off"|"low"|"high"|"max"; a change to
	// "" clears the runtime selection back to the provider default.
	switch effort {
	case "", "off", "low", "high", "max":
		a.cfg.ReasoningEffort = effort
	default:
		return fmt.Errorf("unknown reasoning effort %q (want off|low|high|max)", effort)
	}
	if err := a.registerLLM(); err != nil {
		// Restore the previous selection — never leave a half-applied switch.
		a.cfg.LLM.Provider = oldProvider
		a.cfg.Model, a.cfg.LLM.OpenAI.Model, a.cfg.LLM.Anthropic.Model = oldModel, oldOpenAI, oldAnthropic
		a.cfg.ReasoningEffort = oldEffort
		return err
	}
	// Rebuild compaction on the new provider so auto-summaries follow the switch.
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	return nil
}

// webSubagents returns the sanitized active sub-agent views for GET
// /api/subagents (ADR D-WEB2-H): only id/label/running — never prompts or
// outputs. A disabled subagent capability answers an empty list, not an error.
func (a *app) webSubagents(ctx context.Context, sessionID string) ([]map[string]any, error) {
	if a.subagents == nil {
		return []map[string]any{}, nil
	}
	// dsh session-scoped catalog: the popover shows the requested session's
	// subagents; a blank session_id falls back to the backend's current session.
	if sessionID == "" {
		sessionID = a.currentID
	}
	children, err := a.subagents.ListChildren(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(children))
	for _, c := range children {
		out = append(out, map[string]any{"id": c.ID, "label": c.Label, "running": c.Running})
	}
	return out, nil
}

// webJobs returns the sanitized background-job views for GET /api/jobs (ADR
// D-WEB2-H): id/kind/label/status/detail/started_at/finished_at — never outputs
// or owner-session internals. A disabled jobs capability answers an empty list.
// session_id scopes it to one session (dsh session-header action); blank falls
// back to the backend's current session.
func (a *app) webJobs(ctx context.Context, sessionID string) ([]map[string]any, error) {
	if a.jobs == nil {
		return []map[string]any{}, nil
	}
	if sessionID == "" {
		sessionID = a.currentID
	}
	snaps, err := a.jobs.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(snaps))
	for _, j := range snaps {
		item := map[string]any{
			"id": j.ID, "kind": j.Kind, "label": j.Label,
			"status": j.Status, "detail": j.Detail,
			"started_at": j.StartedAt,
		}
		if j.FinishedAt != nil {
			item["finished_at"] = *j.FinishedAt
		}
		out = append(out, item)
	}
	return out, nil
}
