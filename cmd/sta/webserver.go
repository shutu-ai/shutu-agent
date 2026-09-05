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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/compaction"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/meter"
	"github.com/shutu-ai/shutu-agent/internal/plan"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/spill"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

// eventHub is the real-time event broadcaster (ADR D-WEB2-B): attachSink
// publishes every persisted event of the current session, and each SSE stream
// subscribes to one session id. Publish is non-blocking — a slow subscriber
// whose buffer is full is dropped (select default) so the hub can never stall
// the serial persist path; honest: under extreme load SSE may drop an event and
// the frontend falls back on the snapshot plus the later events.
const eventHubBuffer = 256

type eventHub struct {
	mu      sync.Mutex
	subs    map[string]map[chan session.Event]struct{}
	allSubs map[chan eventHubEvent]struct{}
}

type eventHubEvent struct {
	sessionID string
	event     session.Event
}

type webQueueMessage struct {
	ID        string
	SessionID string
	Text      string
	Images    []llm.ImageRef
	Content   []llm.ContentBlock
	CreatedAt time.Time
	// Context is snapshotted at enqueue so a queued mention preserves the
	// source surface it referenced, not whichever surface is newest at drain.
	Context *llm.Message
	Meta    webserver.PromptMeta
}

// NewEventHub returns an empty event hub.
func NewEventHub() *eventHub {
	return &eventHub{subs: make(map[string]map[chan session.Event]struct{}), allSubs: make(map[chan eventHubEvent]struct{})}
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
	for ch := range h.allSubs {
		select {
		case ch <- eventHubEvent{sessionID: sessionID, event: ev}:
		default:
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

// SubscribeAll registers a process-lifetime observer. SDK clients need this
// rather than a per-session subscription because a session tree can create
// child sessions after the prompt has already been admitted.
func (h *eventHub) SubscribeAll() (chan eventHubEvent, func()) {
	ch := make(chan eventHubEvent, eventHubBuffer)
	h.mu.Lock()
	if h.allSubs == nil {
		h.allSubs = make(map[chan eventHubEvent]struct{})
	}
	h.allSubs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.allSubs[ch]; ok {
			delete(h.allSubs, ch)
			close(ch)
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
	if err := srv.SetFrontendDist(a.cfg.WebServer.DistDir); err != nil {
		return fmt.Errorf("register web server frontend: %w", err)
	}
	if a.extensions != nil {
		srv.SetExtensionRoutes(a.extensionRoutes())
	}
	srv.SetDefaultWorkdir(a.defaultWorkdir())
	if a.hub == nil {
		a.hub = NewEventHub()
	}
	// M10 W1 (ADR D-WEB2): inject the interactive handlers — message dispatch
	// (with implicit resume), session new/resume and the real-time event source
	// (the hub). The webserver stays generic; cmd/sta provides the behavior.
	srv.SetMessageHandler(func(ctx context.Context, sessionID, text string, images []llm.ImageRef, meta webserver.PromptMeta) error {
		return a.webMessage(ctx, sessionID, text, images, meta)
	})
	srv.SetNativeCommandManager(a.nativeCommandManager())
	srv.SetQueueManager(a.webQueueList, a.webQueueEnqueue, a.webQueueUpdate)
	srv.SetNativeQueueUpdater(func(ctx context.Context, sessionID, itemID, action, text string) error {
		if action == "edit" {
			return a.webQueueEdit(ctx, sessionID, itemID, text)
		}
		return a.webQueueUpdate(ctx, sessionID, itemID, map[string]string{"remove": "delete", "steer": "steer"}[action])
	})
	srv.SetSessionManager(func(ctx context.Context, action, id string) (string, error) {
		return a.webSessionManager(ctx, action, id)
	})
	srv.SetNativeSessionCreator(a.nativeCreateAgentSession)
	srv.SetEventSource(func(sessionID string, sink func(session.Event)) func() {
		return a.hub.SubscribeInto(sessionID, sink)
	})
	// dsh-session-status: wire the live per-session status computation so the
	// sidebar renders the status dot + hover card from runtime state (running
	// turn / running subagents / pending interaction / finished-but-unviewed).
	srv.SetSessionStatusProvider(a.sessionStatus)
	// host.describe reports the live Agent registry, not durable session rows;
	// List is nil-safe and returns a stable snapshot for the response.
	srv.SetLiveAgentCounter(func() int {
		return len(a.agentRegistry.List())
	})
	// M10 W2 (ADR D-WEB2-D): inject the sanitized config view. webConfig never
	// exposes web_server.token or any key — the webserver only forwards it.
	srv.SetConfigProvider(a.webConfig)
	srv.SetNativeSettingsApplier(a.applyNativeProviderSettingsRuntime)
	srv.SetContextWindow(a.contextWindowOf)
	srv.SetContextMeter(func(sessionID string, events []session.Event) (meter.Measurement, error) {
		if a.usageMeter == nil {
			return meter.Measurement{}, errors.New("usage meter is unavailable")
		}
		var restored session.Log
		if err := restored.Restore(events); err != nil {
			return meter.Measurement{}, err
		}
		return a.usageMeter.Measure(sessionID, &restored, nil), nil
	})
	srv.SetSessionStateProvider(a.webSessionState)
	srv.SetTurnStopper(a.stopTurn)
	// M10 W4 (ADR D-WEB2-H): inject the read-only subagent and background-job
	// panels. Each provider returns sanitized views (id/status/timestamps only);
	// a disabled capability answers an empty list, never an error.
	srv.SetSubagentProvider(a.webSubagents)
	if a.subagentTools != nil {
		srv.SetNativeSubagentManager(
			func(ctx context.Context, childSessionID string, content []llm.ContentBlock, meta webserver.PromptMeta) error {
				metadata := map[string]string{}
				if meta.RPCID != "" {
					metadata["rpc_id"] = meta.RPCID
				}
				if meta.ClientTimeZone != "" {
					metadata["client_time_zone"] = meta.ClientTimeZone
				}
				return a.subagentTools.SendContentToWithMetadata(ctx, childSessionID, content, metadata)
			},
			func(childSessionID, reason string) error {
				return a.subagentTools.InterruptTo(childSessionID, reason)
			},
		)
	}
	if a.plans != nil {
		srv.SetNativeGoalManager(a.nativeGoalMutation)
	}
	srv.SetJobsProvider(a.webJobs)
	// M10 P5 (ADR D-WEB2-I): wire the image-attachment store when multimodal is
	// enabled (registerAttachments created it); otherwise the attachment APIs
	// stay at 501 and image-carrying messages answer 501/400.
	if a.attachStore != nil {
		srv.SetAttachmentStore(a.attachStore)
	}
	srv.SetNativeImageCapabilityResolver(func(_ context.Context, sessionID string) bool {
		return a.multimodalEnabled() && a.attachStore != nil && a.llmSupportsImagesForSession(sessionID)
	})
	// P5.1 (模型选择实时生效): wire the live model switch.
	srv.SetModelSwitcher(func(ctx context.Context, provider, model, effort string) error {
		return a.webSwitchModel(ctx, provider, model, effort)
	})
	srv.SetSessionModelValidator(a.validateSessionModelSelection)
	srv.SetNativeDefaultModelSaver(a.saveNativeDefaultModel)
	// Native session.rename must use the same live log as prompts and mux
	// projections, so the returned seq is a real session/title event boundary.
	srv.SetNativeSessionRenamer(a.nativeRenameSession)
	// M11 (增加提供方 / 增加自定义提供方): wire the provider-management API. A
	// "save" of a built-in provider stores only the API-key override (custom:false);
	// a "save" of a custom provider (custom:true) persists the full profile + key;
	// "delete" removes a custom provider. All apply immediately via registerLLM.
	srv.SetProviderManager(func(ctx context.Context, action string, edit webserver.ProviderEdit) error {
		switch action {
		case "save":
			models := make([]customModel, 0, len(edit.Models))
			for _, m := range edit.Models {
				models = append(models, customModel{
					ID: m.ID, Name: m.Name, Input: append([]string(nil), m.Input...), ReasoningEfforts: cloneReasoningEfforts(m.ReasoningEfforts), DefaultReasoningEffort: m.DefaultReasoningEffort, DefaultMaxTokens: m.DefaultMaxTokens, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens,
					Reasoning: m.Reasoning, Tools: m.Tools, Vision: m.Vision, Audio: m.Audio,
				})
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
	srv.SetNativeCredentialManager(a.nativeCredentialSet, a.nativeCredentialUnset)
	if a.agentPresets != nil {
		srv.SetNativeAgentPresetManager(a.agentPresets)
	}
	srv.SetNativeSettingsDocumentOpener(func(ctx context.Context) error {
		path, err := filepath.Abs(a.configPath)
		if err != nil {
			return err
		}
		return webserver.OpenNativePath(ctx, path)
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
			out = append(out, webserver.ProviderModel{
				ID: m.ID, Name: m.Name, Input: append([]string(nil), m.Input...), ReasoningEfforts: cloneReasoningEfforts(m.ReasoningEfforts), DefaultReasoningEffort: m.DefaultReasoningEffort, DefaultMaxTokens: m.DefaultMaxTokens, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens,
				Reasoning: m.Reasoning, Tools: m.Tools, Vision: m.Vision, Audio: m.Audio,
			})
		}
		return out, nil
	})
	// 技能设置页 (dsh-skill-mcp-panel 对齐): wire the skill-management API. The
	// manager is created lazily (independent of skill.enabled) so the page
	// always lists the skill files it manages.
	srv.SetSkillManager(a.webSkills)
	srv.SetSkillCatalogProvider(a.nativeSkillCatalog)
	srv.SetMCPManager(a.webRefreshMCP)
	srv.SetMCPConfigManager(a.webManageMCP)
	// DSH Web approval surface: resolve the same live interact engine that the
	// sensitive-tool gate is waiting on. The engine is optional by capability;
	// an unconfigured interact seam answers 501 from the generic server.
	if a.interacts != nil {
		answerer := a.approvalAnswerer()
		srv.SetInteractionManager(
			answerer.List,
			func(ctx context.Context, sessionID, id string, status interact.ApprovalStatus, answer string) error {
				return answerer.Resolve(ctx, sessionID, id, status, answer, false)
			},
		)
		srv.SetInteractionSessionResolver(a.interactionSession)
	}
	a.webserver = srv
	go func() {
		if err := srv.Serve(); err != nil {
			fmt.Fprintln(os.Stderr, "sta: web server:", err)
		}
	}()
	return nil
}

func (a *app) extensionRoutes() []webserver.ExtensionRoute {
	if a == nil || a.extensions == nil {
		return nil
	}
	var routes []webserver.ExtensionRoute
	for _, contribution := range a.extensions.WebContributions() {
		routes = append(routes, webserver.ExtensionRoute{
			ExtensionID: contribution.ExtensionID, Title: contribution.Title, Route: contribution.Route,
			Icon: contribution.Icon, NavigationEnabled: contribution.NavigationEnabled,
			NavigationGroup: contribution.NavigationGroup, Order: contribution.Order,
			Ready: contribution.Ready, ServiceURL: contribution.ServiceURL,
		})
	}
	return routes
}

type nativeCommandManager struct {
	app *app
}

func (a *app) nativeCommandManager() webserver.NativeCommandManager {
	return nativeCommandManager{app: a}
}

func (m nativeCommandManager) List(_ context.Context, _ string) ([]webserver.NativeCommand, error) {
	commands := m.app.webCommandCatalog()
	out := make([]webserver.NativeCommand, 0, len(commands))
	for _, command := range commands {
		if command["kind"] != "command" {
			// User-invocable skills are model-turn inputs in this application,
			// not entries in the native human-command registry. Advertising them
			// here would make commands/execute claim a command while Web routes
			// the same line into a normal Agent turn.
			continue
		}
		name := strings.TrimSpace(command["name"])
		hint := strings.TrimSpace(command["hint"])
		if name == "" || hint == "" {
			continue
		}
		description := hint
		if strings.HasPrefix(description, "Skill: ") {
			description = strings.TrimSpace(strings.TrimPrefix(description, "Skill: "))
		}
		out = append(out, webserver.NativeCommand{
			Name: name, Description: description, InputHint: hint, Images: name == "plan",
		})
	}
	return out, nil
}

func (m nativeCommandManager) Execute(ctx context.Context, sessionID, line string, images []llm.ImageRef) (webserver.NativeCommandExecution, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "/") {
		return webserver.NativeCommandExecution{}, false, nil
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return webserver.NativeCommandExecution{}, false, nil
	}
	name := strings.TrimPrefix(fields[0], "/")
	known := false
	for _, command := range m.app.webCommandCatalog() {
		if command["kind"] == "command" && command["name"] == name {
			known = true
			break
		}
	}
	if !known {
		return webserver.NativeCommandExecution{}, false, nil
	}
	commandLogCtx := runtimectx.With(ctx, runtimectx.Runtime{SessionID: sessionID})
	log := m.app.webLog(commandLogCtx)
	if log == nil && m.app.agentRegistry != nil {
		var err error
		log, err = m.app.sessionLogForAgent(ctx, sessionID)
		if err != nil {
			return webserver.NativeCommandExecution{}, true, err
		}
	}
	if log == nil {
		return webserver.NativeCommandExecution{}, true, errors.New("session log unavailable")
	}
	startSeq := log.NextSeq()
	if err := m.app.webMessage(ctx, sessionID, line, images, webserver.PromptMeta{}); err != nil {
		return webserver.NativeCommandExecution{}, true, err
	}
	return nativeCommandExecutionFromEvents(log.Events(), startSeq), true, nil
}

// nativeCommandExecutionFromEvents returns the command execution that the
// Web command path actually committed. Keeping this derived from the same
// durable rows avoids the old native adapter bug that invented a second
// command ID and discarded the command result text.
func nativeCommandExecutionFromEvents(events []session.Event, startSeq uint64) webserver.NativeCommandExecution {
	var execution webserver.NativeCommandExecution
	for _, event := range events {
		if event.Seq < startSeq {
			continue
		}
		switch event.Type {
		case session.EventCommandRun:
			var data struct {
				CommandID string `json:"commandId"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				execution.CommandID = data.CommandID
			}
		case session.EventCommandDone:
			var data struct {
				CommandID      string  `json:"commandId"`
				Kind           string  `json:"kind"`
				Text           string  `json:"text"`
				SourceEventSeq *uint64 `json:"sourceEventSeq"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.CommandID == execution.CommandID {
				execution.Result.Kind = data.Kind
				execution.Result.SourceEventSeq = data.SourceEventSeq
			}
		case session.EventWebCommandResult:
			var data struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				execution.Result.Text = data.Text
			}
		}
	}
	return execution
}

// webSessionState reconstructs the durable state projection for an arbitrary
// session without switching the process's active session. This is important
// for the Web sidebar: opening a state card must not redirect tool events or
// the next turn to another session.
func (a *app) webSessionState(ctx context.Context, sessionID string) (map[string]any, error) {
	events, err := a.store.LoadSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	snapshot, err := projection.Build(events)
	if err != nil {
		return nil, err
	}
	state := map[string]any{
		"session_id":   sessionID,
		"plan_mode":    snapshot.PlanMode.Active,
		"plan_enabled": a.plans != nil,
	}
	if a.plans != nil {
		state["goals"] = projectionEntitiesForWeb(snapshot.Goals)
		state["plans"] = projectionEntitiesForWeb(snapshot.Plans)
	} else {
		state["goals"] = []plan.Goal{}
		state["plans"] = []plan.Plan{}
	}
	if snapshot.Team != nil {
		state["team"] = snapshot.Team
	}
	if a.spills == nil {
		state["memory_enabled"] = false
		state["memories"] = []spill.Memo{}
	} else {
		memories, err := a.spills.List(ctx)
		if err != nil {
			return nil, err
		}
		state["memory_enabled"] = true
		state["memories"] = memories
	}
	return state, nil
}

// projectionEntitiesForWeb is the sole wire adapter for generic control
// entities. The durable projection owns the fold; this function only adds the
// canonical identity/status fields expected by the existing Web contract.
func projectionEntitiesForWeb(entities []projection.Entity) []map[string]any {
	out := make([]map[string]any, 0, len(entities))
	for _, entity := range entities {
		record := make(map[string]any, len(entity.Data)+4)
		for key, value := range entity.Data {
			record[key] = value
		}
		record["id"] = entity.ID
		if entity.Scope != "" {
			record["scope"] = entity.Scope
		}
		if entity.Name != "" {
			record["name"] = entity.Name
		}
		if entity.Status != "" {
			record["status"] = entity.Status
		}
		record["seq"] = entity.Seq
		out = append(out, record)
	}
	return out
}

func (a *app) webQueueList(ctx context.Context, sessionID string) ([]webserver.QueueItem, error) {
	if err := a.requireAddressedSession(ctx, sessionID); err != nil {
		return nil, err
	}
	a.webQueueMu.Lock()
	defer a.webQueueMu.Unlock()
	queued := a.webQueue[sessionID]
	items := make([]webserver.QueueItem, 0, len(queued))
	for _, item := range queued {
		items = append(items, webserver.QueueItem{
			ID: item.ID, Text: item.Text, Content: item.Content, CreatedAt: item.CreatedAt, Placement: "queued",
		})
	}
	return items, nil
}

func (a *app) webQueueEnqueue(ctx context.Context, sessionID, text string, queuedContent []llm.ContentBlock, meta webserver.PromptMeta) (webserver.QueueItem, error) {
	if err := a.requireRunning(); err != nil {
		return webserver.QueueItem{}, err
	}
	text = strings.TrimSpace(text)
	if sessionID == "" {
		return webserver.QueueItem{}, errors.New("session id is required")
	}
	if err := a.requireAddressedSession(ctx, sessionID); err != nil {
		return webserver.QueueItem{}, err
	}
	if text == "" && len(queuedContent) == 0 {
		return webserver.QueueItem{}, errors.New("text or images are required")
	}
	var images []llm.ImageRef
	content := make([]llm.ContentBlock, 0, len(queuedContent))
	for _, block := range queuedContent {
		content = append(content, block)
		switch block.Kind {
		case llm.BlockText:
			text += block.Text
		case llm.BlockImage:
			images = append(images, block.Image)
		}
	}
	text = strings.TrimSpace(text)
	readable, referenceContext, err := a.prepareSessionReference(ctx, sessionID, text)
	if err != nil {
		return webserver.QueueItem{}, err
	}
	text = readable
	a.webQueueMu.Lock()
	if a.webQueue == nil {
		a.webQueue = make(map[string][]webQueueMessage)
	}
	if a.webQueueRunning == nil {
		a.webQueueRunning = make(map[string]bool)
	}
	a.webQueueSeq++
	item := webQueueMessage{
		ID: fmt.Sprintf("webq-%d", a.webQueueSeq), SessionID: sessionID,
		Text: text, Images: images, Content: content, CreatedAt: time.Now().UTC(), Context: referenceContext, Meta: meta,
	}
	a.webQueue[sessionID] = append(a.webQueue[sessionID], item)
	a.webQueueMu.Unlock()

	// If no turn is active, start the queue immediately. During an active turn
	// webMessage's defer drains it as soon as the current turn settles.
	a.drainWebQueue(sessionID)
	return webserver.QueueItem{ID: item.ID, Text: item.Text, Content: content, CreatedAt: item.CreatedAt, Placement: "queued"}, nil
}

func (a *app) webQueueUpdate(ctx context.Context, sessionID, itemID, action string) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if err := a.requireAddressedSession(ctx, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", webserver.ErrQueueItemNotFound, itemID)
		}
		return err
	}
	if action != "move_first" && action != "delete" && action != "steer" {
		return fmt.Errorf("unsupported queue action %q", action)
	}
	// DSH only accepts a strict steer while a next-turn target can join the
	// live turn. Check before mutating so an idle queue item stays queued.
	if action == "steer" && !a.webTurnRunning(sessionID) {
		return fmt.Errorf("%w: %s", webserver.ErrSteerUnavailable, itemID)
	}
	a.webQueueMu.Lock()
	queued := a.webQueue[sessionID]
	idx := -1
	for i := range queued {
		if queued[i].ID == itemID {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.webQueueMu.Unlock()
		return fmt.Errorf("%w: %s", webserver.ErrQueueItemNotFound, itemID)
	}
	item := queued[idx]
	queued = append(queued[:idx], queued[idx+1:]...)
	if action == "move_first" || action == "steer" {
		queued = append([]webQueueMessage{item}, queued...)
	}
	a.webQueue[sessionID] = queued
	a.webQueueMu.Unlock()

	if action == "steer" && a.webTurnRunning(sessionID) {
		if err := a.stopTurn(sessionID); err != nil {
			return err
		}
	}
	a.drainWebQueue(sessionID)
	return nil
}

// requireAddressedSession is the application-side trust fence for Web
// callbacks that mutate process-local session state. Generic webserver tests
// may use an in-memory callback without a store; production app callbacks
// always have the durable session catalog and must reject guessed IDs before
// queueing work.
func (a *app) requireAddressedSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	if a == nil || a.store == nil {
		return nil
	}
	if _, err := a.store.GetSessionMeta(ctx, strings.TrimSpace(sessionID)); err != nil {
		return err
	}
	return nil
}

func (a *app) webQueueEdit(ctx context.Context, sessionID, itemID, text string) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if err := a.requireAddressedSession(ctx, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", webserver.ErrQueueItemNotFound, itemID)
		}
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("text is required")
	}
	a.webQueueMu.Lock()
	defer a.webQueueMu.Unlock()
	queued := a.webQueue[sessionID]
	for index := range queued {
		if queued[index].ID == itemID {
			queued[index].Text = text
			a.webQueue[sessionID] = queued
			return nil
		}
	}
	return fmt.Errorf("%w: %s", webserver.ErrQueueItemNotFound, itemID)
}

func (a *app) webTurnRunning(sessionID string) bool {
	if sessionID == "" {
		a.runningMu.Lock()
		running := len(a.runningSessions) > 0
		a.runningMu.Unlock()
		return running
	}
	return a.isSessionRunning(sessionID)
}

func (a *app) drainWebQueue(sessionID string) {
	if sessionID == "" {
		return
	}
	a.webQueueMu.Lock()
	if a.webQueueRunning == nil {
		a.webQueueRunning = make(map[string]bool)
	}
	if a.webQueueRunning[sessionID] || a.webTurnRunning(sessionID) || len(a.webQueue[sessionID]) == 0 {
		a.webQueueMu.Unlock()
		return
	}
	item := a.webQueue[sessionID][0]
	a.webQueue[sessionID] = a.webQueue[sessionID][1:]
	a.webQueueRunning[sessionID] = true
	a.webQueueMu.Unlock()

	go func() {
		if item.Context != nil {
			if err := a.appendSessionReferenceContext(context.Background(), sessionID, item.Context); err != nil {
				fmt.Fprintln(os.Stderr, "sta: queued session reference:", err)
				return
			}
		}
		_ = a.webMessageWithContent(context.Background(), sessionID, item.Text, item.Images, item.Content, item.Meta)
		a.webQueueMu.Lock()
		a.webQueueRunning[sessionID] = false
		a.webQueueMu.Unlock()
		a.drainWebQueue(sessionID)
	}()
}

// webMessage handles one web chat message for a session (ADR D-WEB2-A): when
// the target session differs from the current one it is resumed first (attachSink
// already rebinds to the new session), then the turn runs under the global serial
// lock with a silent loop (chunks already persist; the SSE event stream renders
// the flow). P5: an images list logs a user/message event carrying the image
// blocks first (only the refs — the bytes live in the attachment store, same
// path as /attach, D4: the loop is untouched), then the text turn runs.
func (a *app) webMessage(ctx context.Context, sessionID, text string, images []llm.ImageRef, meta webserver.PromptMeta) error {
	return a.webMessageWithContent(ctx, sessionID, text, images, nil, meta)
}

// webMessageWithContent lets the queue replay the exact ordered prompt
// admission while legacy callers keep the text/image convenience contract.
func (a *app) webMessageWithContent(ctx context.Context, sessionID, text string, images []llm.ImageRef, queuedContent []llm.ContentBlock, meta webserver.PromptMeta) error {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return errors.New("empty message text")
	}
	defer a.drainWebQueue(sessionID)
	// Agent-backed sessions already own independent loop/runtime state. Keep
	// their command and ordinary-message paths serialized only within the
	// addressed session; using the process-global legacy turn lock here would make two
	// browser conversations contend for one another's turn boundary.
	if a.agentRegistry != nil && sessionID != "" {
		unlock := a.lockWebSession(sessionID)
		defer unlock()
	}
	// Sensitive-tool approvals raised during a Web turn are resolved by the
	// browser approval card, not by the REPL stdin prompt.
	ctx = withWebApprovalContext(ctx)
	// Agent-backed sessions are independent runtime objects. Only the legacy
	// command path needs to activate the process-global compatibility session.
	if a.agentRegistry == nil && sessionID != "" && sessionID != a.currentID {
		if err := a.resumeSession(ctx, sessionID); err != nil {
			return err
		}
	}
	if a.agentRegistry != nil && sessionID != "" {
		log, err := a.sessionLogForAgent(ctx, sessionID)
		if err != nil {
			return err
		}
		// Slash commands run before the Agent loop installs its own runtime
		// context. Install the addressed session here so command-side durable
		// projections cannot fall back to the process-global a.log/currentID.
		ctx = runtimectx.With(ctx, runtimectx.Runtime{
			SessionID: sessionID,
			Emit: func(typ string, data any) error {
				if log == nil {
					return errors.New("no active session")
				}
				_, err := log.Append(typ, data)
				return err
			},
		})
	}
	// dsh 斜杠命令 (输入条 "/"): a leading "/" routes to a command handler that
	// appends a rendered result to the session — no LLM turn. Session-switching
	// commands (/new, /resume) stay on the sidebar/+ menu, which already drive
	// them through the session manager.
	trimmedText := strings.TrimSpace(text)
	isPlanCommand := isPlanCommandLine(trimmedText)
	if strings.HasPrefix(trimmedText, "/") && !a.isUserSkillInvocation(ctx, trimmedText) {
		if len(images) > 0 && !isPlanCommand {
			return fmt.Errorf("command %q does not accept image attachments", strings.Fields(trimmedText)[0])
		}
		trimmed := trimmedText
		if isPlanCommand {
			planLog := a.webLog(ctx)
			planID, startErr := appendCommandRun(planLog, "plan", strings.TrimSpace(trimmed[len("/plan"):]))
			if startErr != nil {
				return startErr
			}
			finishPlan := func(kind, result string) error {
				return appendCommandDone(planLog, planID, kind, result)
			}
			var submit bool
			var err error
			if a.agentRegistry == nil {
				a.sessionStateMu.Lock()
				submit, err = a.webPlanCommandWithImages(ctx, strings.TrimSpace(trimmed[len("/plan"):]), images)
				a.sessionStateMu.Unlock()
			} else {
				submit, err = a.webPlanCommandWithImages(ctx, strings.TrimSpace(trimmed[len("/plan"):]), images)
			}
			if err != nil {
				_ = finishPlan("error", err.Error())
				return err
			}
			if submit {
				content := planContent(strings.TrimSpace(trimmed[len("/plan"):]), images)
				var turnErr error
				if len(content) > 0 {
					turnErr = a.runTurnContentFor(ctx, sessionID, content, false)
				} else {
					turnErr = a.runTurnFor(ctx, sessionID, strings.TrimSpace(trimmed[len("/plan"):]), false)
				}
				if err := turnErr; err != nil {
					_ = finishPlan("error", err.Error())
					return err
				}
			}
			if err := finishPlan("success", ""); err != nil {
				return err
			}
			return a.runIdleGoalFor(ctx, sessionID, a.webLog(ctx), false)
		}
		var err error
		if a.agentRegistry == nil {
			a.sessionStateMu.Lock()
			err = a.webCommand(ctx, trimmed)
			a.sessionStateMu.Unlock()
		} else {
			err = a.webCommand(ctx, trimmed)
		}
		if err != nil {
			return err
		}
		return a.runIdleGoalFor(ctx, sessionID, a.webLog(ctx), false)
	}
	// Session references are prepared at message admission: malformed or
	// self-referencing mentions fail before durable prompt admission, and the
	// source sessions are projected once at their then-current surface.
	referenceText, referenceContext, err := a.prepareSessionReference(ctx, sessionID, text)
	if err != nil {
		return err
	}
	text = referenceText
	var turnContent []llm.ContentBlock
	switch {
	case len(queuedContent) > 0:
		// Reapply the reference rewrite to the queued ordered content while
		// preserving image/text placement.
		orderedContent := append([]llm.ContentBlock(nil), queuedContent...)
		replacedText := false
		for index := range orderedContent {
			if orderedContent[index].Kind != llm.BlockText {
				continue
			}
			if replacedText {
				orderedContent[index].Text = ""
				continue
			}
			orderedContent[index].Text = referenceText
			replacedText = true
		}
		queuedContent = orderedContent
		hasImage := false
		for _, block := range queuedContent {
			if block.Kind == llm.BlockImage {
				hasImage = true
				break
			}
		}
		if hasImage && (!a.multimodalEnabled() || a.attachStore == nil) {
			return fmt.Errorf("multimodal disabled (llm.multimodal.enabled=false)")
		}
		turnContent = queuedContent
	case len(images) > 0:
		if !a.multimodalEnabled() || a.attachStore == nil {
			return fmt.Errorf("multimodal disabled (llm.multimodal.enabled=false)")
		}
		blocks := make([]llm.ContentBlock, 0, len(images))
		for _, img := range images {
			blocks = append(blocks, llm.ContentBlock{Kind: llm.BlockImage, Image: img})
		}
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, llm.Text(text))
		}
		turnContent = blocks
	}
	if err := a.appendSessionReferenceContext(ctx, sessionID, referenceContext); err != nil {
		return fmt.Errorf("web session reference: %w", err)
	}
	// A cancellable turn context so POST /api/sessions/{id}/stop can abort this
	// turn (dsh 停止按钮). The turn must not inherit the HTTP request context:
	// dsh keeps the agent turn alive when the chat tab disconnects or crashes,
	// and the explicit stop endpoint is the cancellation boundary. Keep the
	// process context so application shutdown still cancels active work, and
	// re-apply the Web approval marker because the request context is detached.
	turnBase := a.baseCtx
	if turnBase == nil {
		// Tests and embedders may not install the process context. WithoutCancel
		// preserves context values while dropping the request's cancellation.
		turnBase = context.WithoutCancel(ctx)
	}
	turnBase = withWebApprovalContext(turnBase)
	turnCtx, cancel := context.WithCancel(turnBase)
	a.setTurnCancel(sessionID, cancel)
	defer func() { a.clearTurnCancel(sessionID); cancel() }()
	if len(turnContent) > 0 {
		if err := a.runTurnContentForWithMeta(turnCtx, sessionID, turnContent, false, meta); err != nil {
			return err
		}
	} else if err := a.runTurnForWithMeta(turnCtx, sessionID, text, false, meta); err != nil {
		return err
	}
	// Keep Web and REPL turn completion identical: persist long-term memories
	// before title/goal continuation runs.
	postLog := a.log
	if a.agentRegistry != nil {
		postLog, _ = a.sessionLogForAgent(turnCtx, sessionID)
	}
	a.spillAutoSpillFor(turnCtx, sessionID, postLog)
	// session-title alignment (dsh): after the first eligible message, the
	// deterministic fallback is stored and the asynchronous model title is
	// scheduled. This runs after the turn, outside the legacy turn lock, so it never delays
	// the answer.
	a.ensureSessionTitle(turnCtx, sessionID)
	// Goal driver idle/followup: the outer web turn has settled, so each
	// continuation round can acquire the shared turn lock independently.
	if err := a.runIdleGoalFor(turnCtx, sessionID, postLog, false); err != nil {
		return err
	}
	// dsh-session-status: keep this session out of the finished-but-unviewed
	// reminder while the user is on it (the turn above bumped updated_at past
	// the previous view, so this restores last_viewed_at >= updated_at).
	a.markSessionViewed(turnCtx, sessionID)
	return nil
}

// lockWebSession returns an unlock function for command/turn mutations in one
// Agent-backed Web session. The lock objects are process-local coordination;
// durable event sequence allocation remains owned by session.Log/store.
func (a *app) lockWebSession(sessionID string) func() {
	a.webSessionMu.Lock()
	if a.webSessionLocks == nil {
		a.webSessionLocks = make(map[string]*sync.Mutex)
	}
	lock := a.webSessionLocks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		a.webSessionLocks[sessionID] = lock
	}
	a.webSessionMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// webCommand handles a leading "/" in a web composer message (dsh 斜杠命令
// 对齐, ①③⑤): it appends the command as a user/message, dispatches it, and
// appends the result as an assistant/message so the web chat renders the whole
// exchange without an LLM turn. The events flow through the same event hub as
// a turn, so the SSE stream renders them. Session-switching commands (/new,
// /resume) are deliberately not routed here — the sidebar and the composer "+"
// menu already drive them through the session manager.
func (a *app) webCommandLegacy(ctx context.Context, line string) (err error) {
	// Keep the historical entry point on the canonical command lifecycle. The
	// old implementation below is retained only for source compatibility and
	// must never execute because it predates the command result contract.
	if a != nil {
		return a.webCommand(ctx, line)
	}

	log := a.webLog(ctx)
	if log == nil {
		return errors.New("no active session")
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return errors.New("empty command")
	}
	name := fields[0]
	args := fields[1:]
	commandID := ""
	commandKind := "success"
	commandText := ""
	if isWebCommandName(strings.TrimPrefix(name, "/")) {
		commandID, err = appendCommandRun(log, strings.TrimPrefix(name, "/"), commandArgs(line, name))
		if err != nil {
			return err
		}
		defer func() {
			if err != nil {
				commandKind = "error"
				commandText = err.Error()
			}
			if appendErr := appendCommandDone(log, commandID, commandKind, commandText); err == nil && appendErr != nil {
				err = appendErr
			}
		}()
	}
	if name == "/feedback" {
		result, err := a.webFeedback(ctx, strings.TrimSpace(line[len(name):]))
		if err != nil {
			commandKind = "error"
			commandText = err.Error()
		} else {
			commandText = result
			result = "⚠ " + err.Error()
		}
		_, appendErr := log.Append(session.EventWebCommandResult, session.NewWebCommandResult(result, "feedback"))
		if appendErr != nil {
			return appendErr
		}
		return nil
	}
	if name == "/plan" {
		_, err = a.webPlanCommand(ctx, strings.TrimSpace(line[len(name):]))
		return err
	}
	if name == "/export" {
		if len(args) > 0 {
			commandKind = "error"
			commandText = "The Web /export command does not accept a path."
			return a.appendWebCommandResultOn(log, "The Web /export command does not accept a path.")
		}
		commandText = "Session log download requested."
		return a.appendWebCommandResultOn(log, "Session log download requested.", "export")
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage(line)); err != nil {
		return err
	}
	result, err := a.execWebCommand(ctx, name, args)
	if err != nil {
		commandKind = "error"
		commandText = err.Error()
	} else {
		commandText = result
		result = "⚠ " + err.Error()
	}
	if _, err := log.Append(session.EventAssistantMessage, session.NewAssistantMessage(result, nil, "stop")); err != nil {
		return err
	}
	return nil
}

// execWebCommand dispatches a single web slash command and returns its UI
// result text (a non-nil error means an invalid command or bad args). The
// caller persists this as web/command-result, never as assistant history.
func (a *app) execWebCommand(ctx context.Context, name string, args []string) (string, error) {
	switch name {
	case "/help":
		return a.webHelp(), nil
	case "/status":
		return a.webStatus(ctx), nil
	case "/compact":
		return a.webCompact(ctx, args)
	case "/permission":
		return a.webPermission(ctx, args)
	case "/feedback":
		return a.webFeedback(ctx, strings.Join(args, " "))
	case "/goal":
		return a.webGoalCommand(ctx, strings.Join(args, " "))
	case "/plan":
		_, err := a.webPlanCommand(ctx, strings.Join(args, " "))
		return "", err
	case "/export":
		if len(args) > 0 {
			return "", errors.New("The Web /export command does not accept a path.")
		}
		return "Session log download requested.", nil
	default:
		return "", fmt.Errorf("unknown command %q (try /help)", name)
	}
}

// webHelp returns the same discovery view used by the Web composer. Keeping
// this derived from webCommandCatalog makes /help follow dsh's command
// registry semantics: built-in commands first, then user-invocable skills.
func (a *app) webHelp() string {
	var b strings.Builder
	catalog := a.webCommandCatalog()
	b.WriteString("可用的斜杠命令:\n")
	for _, item := range catalog {
		if item[`kind`] != `command` {
			continue
		}
		fmt.Fprintf(&b, "  /%-16s %s\n", item[`name`], item[`hint`])
	}

	hasSkill := false
	for _, item := range catalog {
		if item[`kind`] == `skill` {
			if !hasSkill {
				b.WriteString("\n可用技能:\n")
				hasSkill = true
			}
			hint := strings.TrimPrefix(item[`hint`], "Skill: ")
			fmt.Fprintf(&b, "  /%-16s %s\n", item[`name`], hint)
		}
	}
	return b.String()
}

// webFeedback mirrors dsh's /feedback <text> command: surrounding whitespace
// is removed, the text is recorded as a log-only feedback/record event, and no
// model turn or user/message event is created. The Web-only acknowledgement is
// emitted separately by webCommand so it remains visible in the transcript.
func (a *app) webFeedback(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("Feedback text is required. Usage: /feedback <text>")
	}
	log := a.webLog(ctx)
	if log == nil {
		return "", errors.New("no active session")
	}
	if _, err := log.Append(session.EventFeedbackRecord, session.NewFeedbackRecord(text)); err != nil {
		return "", err
	}
	anonymousID, err := a.feedbackAnonymousUserID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Feedback recorded for session %s\nAnonymous user: %s. Session sharing is not configured.", a.webSessionID(ctx), anonymousID), nil
}

// webStatus returns the current provider / model / mode summary (dsh 输入条
// 状态命令, mirrors the REPL /help's llm line).
func (a *app) webStatus(ctx context.Context) string {
	cfg := a.providerConfigSnapshot()
	provider := cfg.LLM.Provider
	model := llmProviderModel(cfg, provider)
	mode := cfg.Mode
	if id := a.webSessionID(ctx); id != "" {
		if scs, ok := a.store.(store.SessionConfigStore); ok {
			if selected, err := scs.GetSessionConfig(ctx, id); err == nil {
				if selected.Provider != "" {
					provider = selected.Provider
				}
				if selected.Model != "" {
					model = selected.Model
				} else {
					model = llmProviderModel(cfg, provider)
				}
				if selected.AgentPreset != "" {
					mode = selected.AgentPreset
				}
			}
		}
	}
	return fmt.Sprintf("provider=%s model=%s mode=%s", provider, model, mode)
}

// webCompact runs the same manual compaction as the REPL and formats the
// report as an assistant message for the Web SSE stream.
func (a *app) webCompact(ctx context.Context, args []string) (string, error) {
	sessionID := a.webSessionID(ctx)
	log := a.webLog(ctx)
	engine := a.compactionEngineFor(ctx, sessionID)
	if log == nil || engine == nil {
		return "compaction: disabled (compaction.enabled=false)", nil
	}
	if a.agentRegistry != nil && sessionID != "" {
		handle, err := a.sessionAgent(sessionID)
		if err != nil {
			return "", err
		}
		var res *compaction.Result
		err = handle.RunMaintenance(func(taskCtx context.Context) error {
			return a.compactSession(taskCtx, engine, log, args, &res)
		})
		if err != nil {
			return "", err
		}
		if res == nil {
			return "compaction: nothing to compact", nil
		}
		return fmt.Sprintf("compacted %d events (seq %d..%d), saved %d tokens (id %s)\nsummary: %s",
			len(res.ShadowedSeqs), res.ShadowedRange[0], res.ShadowedRange[1], res.ShadowedTokens, res.CompactionID, res.Summary), nil
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
		res, err = a.compactAndLogOn(ctx, log, "manual /compact region command", "manual",
			func() (*compaction.Result, error) { return engine.CompactRegion(ctx, log, start, end) })
	case len(args) != 0:
		return "", fmt.Errorf("usage: /compact or /compact region <start> <end>")
	default:
		res, err = a.compactAndLogOn(ctx, log, "manual /compact command", "manual",
			func() (*compaction.Result, error) { return engine.CompactNow(ctx, log) })
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
	sessionID := a.webSessionID(ctx)
	if sessionID != "" {
		if scs, ok := a.store.(store.SessionConfigStore); ok {
			cfg, err := scs.GetSessionConfig(ctx, sessionID)
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
	if next != "readonly" && next != "standard" && next != "full" && next != "read-only" && next != "workspace-write" && next != "danger-full-access" {
		return "", fmt.Errorf("unknown preset %q (available: %s)", next, available)
	}
	stored, preset, sandboxMode, approvalPolicy := permissionBundle(next)
	if sessionID != "" {
		scs, ok := a.store.(store.SessionConfigStore)
		if !ok {
			return "", errors.New("session permission overrides are unsupported by this store")
		}
		cfg, err := scs.GetSessionConfig(ctx, sessionID)
		if err != nil {
			return "", err
		}
		if err := scs.UpdateSessionConfig(ctx, sessionID, cfg.Provider, cfg.Model, cfg.ReasoningEffort, stored); err != nil {
			return "", err
		}
		if a.interacts != nil {
			if controller, ok := a.interacts.(interact.PolicyController); ok {
				if err := controller.SetSessionPolicy(sessionID, interact.ApprovalPolicy(approvalPolicy)); err != nil {
					return "", err
				}
			}
		}
		log := a.webLog(ctx)
		if log == nil {
			return "", errors.New("permission switch requires a durable session log")
		}
		for _, item := range []struct {
			typ  string
			data any
		}{
			{session.EventPermissionPreset, session.NewPermissionPreset(preset)},
			{session.EventSandboxMode, session.NewSandboxMode(sandboxMode)},
			{session.EventApprovalPolicy, session.NewApprovalPolicy(approvalPolicy)},
		} {
			if _, err := log.Append(item.typ, item.data); err != nil {
				return "", fmt.Errorf("persist permission change: %w", err)
			}
		}
	} else if err := a.store.SetSetting(ctx, "permission_preset", stored); err != nil {
		return "", err
	}
	return fmt.Sprintf("preset %s", next), nil
}

// permissionBundle keeps the legacy storage values used by the Go execution
// policy while emitting the reference Harness vocabulary on the session log.
func permissionBundle(value string) (stored, preset, sandboxMode, approvalPolicy string) {
	switch value {
	case "read-only":
		return "readonly", "read-only", "read-only", "ask"
	case "workspace-write":
		return "standard", "workspace-write", "workspace-write", "ask"
	case "danger-full-access":
		return "full", "danger-full-access", "danger-full-access", "never"
	case "readonly":
		return "readonly", "read-only", "read-only", "ask"
	case "full":
		return "full", "danger-full-access", "danger-full-access", "never"
	default:
		return "standard", "workspace-write", "workspace-write", "ask"
	}
}

// webCommandCatalog is the backend-owned discovery view used by the web
// composer.
func (a *app) webCommandCatalog() []map[string]string {
	out := make([]map[string]string, 7)
	out[0] = make(map[string]string)
	out[0][`name`] = `help`
	out[0][`hint`] = `Show available slash commands`
	out[0][`kind`] = `command`
	out[1] = make(map[string]string)
	out[1][`name`] = `status`
	out[1][`hint`] = `Show provider, model and mode`
	out[1][`kind`] = `command`
	out[2] = make(map[string]string)
	out[2][`name`] = `compact`
	out[2][`hint`] = `Compact context: /compact [region start end]`
	out[2][`kind`] = `command`
	out[3] = make(map[string]string)
	out[3][`name`] = `permission`
	out[3][`hint`] = `Show or set permission: /permission [readonly|standard|full]`
	out[3][`kind`] = `command`
	out[4] = make(map[string]string)
	out[4][`name`] = `feedback`
	out[4][`hint`] = `Record feedback: /feedback <text>`
	out[4][`kind`] = `command`
	out[5] = make(map[string]string)
	out[5][`name`] = `goal`
	out[5][`hint`] = `Manage the goal: /goal [objective|clear|edit <objective>|pause|resume]`
	out[5][`kind`] = `command`
	out[6] = make(map[string]string)
	out[6][`name`] = `plan`
	out[6][`hint`] = `Plan mode: /plan [off|message]`
	out[6][`kind`] = `command`
	out = append(out, map[string]string{
		`name`: `export`,
		`hint`: `Download Session log: /export`,
		`kind`: `command`,
	})
	// dsh appends user-invocable skills after the host command directory. Keep
	// this order stable: the composer presents commands first and skills last.
	out = append(out, a.webSkillCatalog()...)
	return out
}

// isWebCommandName reports whether name belongs to the built-in Web command
// plane. A same-named skill never claims a command slot; dsh command
// adjudication gives the host command precedence.
func isWebCommandName(name string) bool {
	switch name {
	case "help", "status", "compact", "permission", "feedback", "goal", "plan", "export":
		return true
	default:
		return false
	}
}

// webSkillCatalog returns the user-facing skill entries for the Web composer.
// The registry List call is deliberately followed by Get so invocation policy
// comes from the same parsed frontmatter as skill execution. Discovery is
// fail-open for /api/config: one unreadable skill must not hide built-in
// commands or make the Web UI unavailable.
func (a *app) webSkillCatalog() []map[string]string {
	if a.skills == nil {
		return nil
	}
	cands, err := a.skills.List(context.Background())
	if err != nil {
		return nil
	}
	out := make([]map[string]string, 0, len(cands))
	for _, c := range cands {
		if isWebCommandName(c.Name) {
			continue
		}
		def, err := a.skills.Get(context.Background(), c.Name)
		if err != nil || def == nil || !def.UserInvocable {
			continue
		}
		out = append(out, map[string]string{
			`name`: c.Name,
			`hint`: `Skill: ` + c.Description,
			`kind`: `skill`,
		})
	}
	return out
}

// webPlanCommand mirrors dsh's plan-mode command. The mode switch is a
// durable session fact and the acknowledgement is Web-only, so it never
// becomes model history. A non-empty suffix tells webMessage to submit that
// suffix as the next ordinary user turn after enabling plan mode.
func (a *app) webPlanCommand(ctx context.Context, suffix string) (bool, error) {
	return a.webPlanCommandWithImages(ctx, suffix, nil)
}

func (a *app) webPlanCommandWithImages(ctx context.Context, suffix string, images []llm.ImageRef) (bool, error) {
	log := a.webLog(ctx)
	if log == nil {
		return false, errors.New("no active session")
	}
	active, err := currentPlanModeActive(log)
	if err != nil {
		return false, err
	}
	trimmed := strings.TrimSpace(suffix)
	if len(images) > 0 {
		if trimmed == "off" {
			return false, errors.New("Image attachments cannot accompany /plan off.")
		}
		if !a.multimodalEnabled() || a.attachStore == nil {
			return false, fmt.Errorf("multimodal disabled (llm.multimodal.enabled=false)")
		}
	}
	if a.agentRegistry != nil {
		sessionID := a.runtimeSessionID(ctx)
		if trimmed == "off" {
			action, err := a.setPlanModeFor(ctx, sessionID, log, false)
			if err != nil {
				return false, err
			}
			text := "Plan mode off."
			switch {
			case action == planModeQueued:
				text = "Leaving plan mode (applies from the next step)."
			case action == planModeCancelled:
				text = "Plan mode entry cancelled."
			case action == planModeNoop && !active:
				text = "Plan mode is already inactive."
			}
			return false, a.appendWebCommandResultOn(log, text, "plan")
		}
		action, err := a.setPlanModeFor(ctx, sessionID, log, true)
		if err != nil {
			return false, err
		}
		content := planContent(trimmed, images)
		if len(content) > 0 && action == planModeQueued {
			if steered, steerErr := a.steerPlanContent(sessionID, content); steerErr != nil {
				return false, steerErr
			} else if steered {
				return false, a.appendWebCommandResultOn(log, "Entering plan mode (applies from the next step). Use /plan off to leave.", "plan")
			}
		}
		if len(content) == 0 {
			if action == planModeQueued {
				return false, a.appendWebCommandResultOn(log, "Entering plan mode (applies from the next step). Use /plan off to leave.", "plan")
			}
			return false, a.appendWebCommandResultOn(log, "Plan mode on. Use /plan off to leave.", "plan")
		}
		return true, a.appendWebCommandResultOn(log, "Plan mode on. Use /plan off to leave.", "plan")
	}
	if trimmed == "off" {
		if !active {
			return false, a.appendWebCommandResultOn(log, "Plan mode is already inactive.", "plan")
		}
		if _, err := log.Append(session.EventPlanMode, session.NewPlanMode(false)); err != nil {
			return false, err
		}
		return false, a.appendWebCommandResultOn(log, "Plan mode off.", "plan")
	}
	if !active {
		if _, err := log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
			return false, err
		}
		if trimmed == "" && len(images) == 0 {
			return false, a.appendWebCommandResultOn(log, "Plan mode on. Use /plan off to leave.", "plan")
		}
		return true, a.appendWebCommandResultOn(log, "Plan mode on. Use /plan off to leave.", "plan")
	}
	if trimmed == "" && len(images) == 0 {
		return false, a.appendWebCommandResultOn(log, "Plan mode is already active. Use /plan off to leave.", "plan")
	}
	return true, a.appendWebCommandResultOn(log, "Plan mode already active. Submitting the message in plan mode.", "plan")
}

func (a *app) appendWebCommandResult(text string, command ...string) error {
	return a.appendWebCommandResultOn(a.log, text, command...)
}

func (a *app) appendWebCommandResultOn(log *session.Log, text string, command ...string) error {
	if log == nil {
		return errors.New("no active session")
	}
	_, err := log.Append(session.EventWebCommandResult, session.NewWebCommandResult(text, command...))
	return err
}

// webSessionID/webLog are the command-path equivalent of runtimeSessionID and
// runtimeLog. They keep Web slash commands on the addressed Agent session.
// Once Agent runtimes are mounted, a missing request identity fails closed;
// only the legacy REPL keeps the current-session fallback.
func (a *app) webSessionID(ctx context.Context) string {
	if id := runtimectx.SessionID(ctx); id != "" {
		return id
	}
	if a.agentRegistry != nil {
		return ""
	}
	return a.currentID
}

func (a *app) webLog(ctx context.Context) *session.Log {
	if id := runtimectx.SessionID(ctx); id != "" {
		return a.runtimeLog(ctx)
	}
	if a.agentRegistry != nil {
		return nil
	}
	return a.log
}

// currentGoal returns the newest non-terminal goal in the current session.
func (a *app) currentGoal(ctx context.Context) (plan.Goal, bool, error) {
	engine, err := a.planEngineFor(ctx)
	if err != nil {
		return plan.Goal{}, false, err
	}
	goals, err := engine.List(ctx)
	if err != nil {
		return plan.Goal{}, false, err
	}
	byID := make(map[string]plan.Goal, len(goals))
	for _, g := range goals {
		byID[g.ID] = g
	}
	if log := a.webLog(ctx); log != nil {
		events := log.Events()
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type != session.EventPlanCreate {
				continue
			}
			var data struct {
				Scope string `json:"scope"`
				ID    string `json:"id"`
			}
			if json.Unmarshal(events[i].Data, &data) == nil && data.Scope == string(plan.ScopeGoal) {
				g, ok := byID[data.ID]
				if ok && g.Status != plan.StatusDone && g.Status != plan.StatusCancelled {
					return g, true, nil
				}
			}
		}
	}
	return plan.Goal{}, false, nil
}

func renderWebGoal(g plan.Goal) string {
	status := string(g.Status)
	switch g.Status {
	case plan.StatusPending, plan.StatusInProgress:
		status = "active"
	case plan.StatusDone, plan.StatusCancelled:
		status = "complete"
	}
	maxRounds := g.MaxRounds
	if maxRounds <= 0 {
		maxRounds = plan.DefaultMaxGoalRounds
	}
	blocked := ""
	if g.BlockedReason != "" {
		blocked = "\nBlocked reason: " + g.BlockedReason
	}
	return fmt.Sprintf("Goal\nStatus: %s\nObjective: %s\nID: %s\nRounds: %d/%d\nPlans: %d%s\nCommands: /goal edit <objective> | /goal pause | /goal resume | /goal clear", status, g.Objective, g.ID, g.RoundsStarted, maxRounds, len(g.Plans), blocked)
}

// webGoalCommand follows dsh's /goal grammar while using the existing plan
// engine as the durable projection.
func (a *app) webGoalCommand(ctx context.Context, input string) (string, error) {
	log := a.webLog(ctx)
	engine, engineErr := a.planEngineFor(ctx)
	if engineErr != nil {
		return "", engineErr
	}
	sessionID := a.webSessionID(ctx)
	input = strings.TrimSpace(input)
	if input == "" {
		g, ok, err := a.currentGoal(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "No goal is currently set.\nUsage: /goal [<objective>|clear|edit <objective>|pause|resume]", nil
		}
		return renderWebGoal(g), nil
	}
	if input == "clear" {
		g, ok, err := a.currentGoal(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "No goal to clear.", nil
		}
		if err := engine.Remove(ctx, string(plan.ScopeGoal), g.ID); err != nil {
			return "", err
		}
		if log == nil {
			return "", errors.New("no active session")
		}
		if _, err := log.Append(session.EventPlanDelete, session.NewPlanDelete(string(plan.ScopeGoal), g.ID)); err != nil {
			return "", err
		}
		a.setGoalActivation(sessionID, false)
		return "Goal cleared.", nil
	}
	if input == "pause" || input == "resume" {
		g, ok, err := a.currentGoal(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "No goal is currently set.", nil
		}
		st := plan.StatusPaused
		message := "Goal paused."
		if input == "resume" {
			st = plan.StatusInProgress
			message = "Goal resumed."
		}
		var statusErr error
		if setter, ok := engine.(interface {
			SetGoalStatusIfRevision(context.Context, string, int, plan.Status) error
		}); ok {
			statusErr = setter.SetGoalStatusIfRevision(ctx, g.ID, g.Revision, st)
		} else {
			statusErr = engine.SetStatus(ctx, string(plan.ScopeGoal), g.ID, st)
		}
		if statusErr != nil {
			return "", statusErr
		}
		if log == nil {
			return "", errors.New("no active session")
		}
		if _, err := log.Append(session.EventPlanStatus, session.NewPlanStatus(string(plan.ScopeGoal), g.ID, string(st))); err != nil {
			return "", err
		}
		if input == "resume" {
			a.setGoalActivation(sessionID, true)
		} else {
			a.setGoalActivation(sessionID, false)
		}
		return message, nil
	}
	if input == "edit" {
		return "", errors.New("usage: /goal edit <objective>")
	}
	if strings.HasPrefix(input, "edit ") {
		g, ok, err := a.currentGoal(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "No goal is currently set.", nil
		}
		objective := strings.TrimSpace(strings.TrimPrefix(input, "edit "))
		title := strings.Fields(objective)[0]
		updated, err := engine.UpdateGoal(ctx, g.ID, title, objective)
		if err != nil {
			return "", err
		}
		if log == nil {
			return "", errors.New("no active session")
		}
		if _, err := log.Append(session.EventPlanUpdate, session.NewPlanUpdate(string(plan.ScopeGoal), updated.ID, map[string]any{"title": updated.Title, "objective": updated.Objective})); err != nil {
			return "", err
		}
		return "Goal updated.\n" + renderWebGoal(updated), nil
	}
	if _, ok, err := a.currentGoal(ctx); err != nil {
		return "", err
	} else if ok {
		return "", errors.New("a goal is already active; use /goal edit <objective> or /goal clear")
	}
	res, err := a.webPlanGoal(ctx, []string{input})
	return res, err
}

// webPlanGoal creates a goal via the DSH create_goal tool (dsh /goal entry). It
// returns the tool's model-facing output; the plan/create fact also lands in
// the session log (D3). When plan is disabled the tool is unregistered and
// Execute reports it.
func (a *app) webPlanGoal(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: /goal <标题> [目标说明]")
	}
	objective := strings.TrimSpace(strings.Join(args, " "))
	payload, err := json.Marshal(map[string]any{"objective": objective})
	if err != nil {
		return "", err
	}
	if a.reg == nil {
		return "", errors.New("tool registry is unavailable")
	}
	registry := a.reg
	var restore func()
	// Agent-backed Web commands must use the same session-owned registry view
	// as an Agent turn. Executing against a.reg would bypass the addressed
	// session's permission/mode policy and let tool closures observe the legacy
	// process-global owner. The legacy REPL keeps the original registry path.
	if sessionID := a.webSessionID(ctx); a.agentRegistry != nil && sessionID != "" {
		log := a.webLog(ctx)
		if log == nil {
			return "", fmt.Errorf("session %q runtime is unavailable", sessionID)
		}
		registry = a.reg.Clone()
		registry.SetOwner(tools.Owner{SessionID: sessionID, NextSeq: log.NextSeq})
		_, restore, err = a.applySessionRuntimeOnStrict(sessionID, log, registry)
		if err != nil {
			return "", err
		}
		defer restore()
	}
	res, err := registry.Execute(ctx, "create_goal", payload)
	if err != nil {
		return "", err
	}
	a.setGoalActivation(a.webSessionID(ctx), true)
	return res.Output, nil
}

// webSessionManager implements the session new/resume API (ADR D-WEB2-C),
// reusing the REPL's newSession/resumeSession.
func (a *app) webSessionManager(ctx context.Context, action, id string) (string, error) {
	// Agent-backed web sessions are independent runtime objects. Do not reuse
	// the REPL's currentID/log switch, otherwise two browser tabs can redirect
	// each other's command fallbacks and workspace selection.
	if a.agentRegistry != nil {
		switch action {
		case "new":
			return a.newAgentSession(ctx)
		case "resume":
			if err := a.resumeAgentSession(ctx, id); err != nil {
				return "", err
			}
			return id, nil
		default:
			return "", fmt.Errorf("unknown session action %q", action)
		}
	}
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

// nativeCreateAgentSession implements DSH's named-identity create/adoption
// semantics. Fresh identities are created atomically with their cwd, workspace
// and preset; existing identities are adopted only when their immutable runtime
// inputs agree, preserving DSH's stable conflict shapes.
func (a *app) nativeCreateAgentSession(ctx context.Context, spec webserver.NativeSessionCreateSpec) (webserver.NativeSessionCreateResult, error) {
	zero := webserver.NativeSessionCreateResult{}
	if err := a.requireRunning(); err != nil {
		return zero, err
	}
	if a.store == nil {
		return zero, errors.New("session store is unavailable")
	}
	sessionID := strings.TrimSpace(spec.SessionID)
	createdSession := false
	if sessionID != "" {
		meta, err := a.store.GetSessionMeta(ctx, sessionID)
		switch {
		case err == nil:
			if meta.CWD != spec.CWD {
				return zero, webserver.NewNativeSessionCreateError("session-conflict",
					fmt.Sprintf("session %q already exists with cwd %q; requested %q", sessionID, meta.CWD, spec.CWD),
					map[string]any{
						"sessionId": sessionID, "requestedCwd": spec.CWD, "existingCwd": meta.CWD,
					})
			}
			configs, ok := a.store.(store.SessionConfigStore)
			if !ok {
				return zero, errors.New("session configuration store is unavailable")
			}
			config, err := configs.GetSessionConfig(ctx, sessionID)
			if err != nil {
				return zero, err
			}
			if spec.AgentPresetRequested && config.AgentPreset != spec.AgentPreset {
				message := fmt.Sprintf(
					"session %q already runs agent preset %q; requested %q. A session's preset is fixed at creation.",
					sessionID, config.AgentPreset, spec.AgentPreset)
				if config.AgentPreset == "" {
					message = fmt.Sprintf(
						"session %q records no agent preset, so it cannot be adopted under one; "+
							"a deployment composing no roster records none on any session — requested %q. "+
							"A session's preset is fixed at creation.", sessionID, spec.AgentPreset)
				}
				details := map[string]any{
					"sessionId": sessionID, "requestedPreset": spec.AgentPreset,
				}
				if config.AgentPreset != "" {
					details["existingPreset"] = config.AgentPreset
				}
				return zero, webserver.NewNativeSessionCreateError("agent-preset-conflict", message, details)
			}
			if spec.WorkspaceID != "" {
				if err := a.store.SetSessionWorkspace(ctx, sessionID, spec.WorkspaceID); err != nil {
					return zero, webserver.NewNativeSessionCreateError("workspace-attach-failed",
						fmt.Sprintf("session %q was created but could not attach to workspace %q: %v",
							sessionID, spec.WorkspaceID, err),
						map[string]any{"sessionId": sessionID, "workspaceId": spec.WorkspaceID})
				}
			}
			a.markSessionViewed(ctx, sessionID)
			return webserver.NativeSessionCreateResult{
				SessionID: sessionID, AgentPreset: config.AgentPreset, CWD: meta.CWD,
			}, nil
		case errors.Is(err, store.ErrNotFound):
		default:
			return zero, err
		}
	} else {
		generated, err := store.GenerateReservedID(ctx, a.store, "session", newSessionID)
		if err != nil {
			return zero, fmt.Errorf("generate session id: %w", err)
		}
		sessionID = generated
	}

	// DSH creates a fresh session's project directory before composing/publishing
	// it. Adoption must not touch the existing project directory.
	if err := os.MkdirAll(spec.CWD, 0o755); err != nil {
		return zero, fmt.Errorf("failed to ensure project directory %q: %w", spec.CWD, err)
	}

	created := time.Now().UTC()
	cfg := a.providerConfigSnapshot()
	if spec.AgentPreset != "" {
		cfg.Mode = spec.AgentPreset
	}
	config := &store.SessionConfig{
		AgentPreset:     spec.AgentPreset,
		Provider:        cfg.LLM.Provider,
		Model:           llmProviderModel(cfg, cfg.LLM.Provider),
		ReasoningEffort: cfg.ReasoningEffort,
	}
	if atomic, ok := a.store.(store.SessionCreateStore); ok {
		err := atomic.CreateSessionWithOptions(ctx, sessionID, created, store.SessionCreateOptions{
			Header: store.SessionHeader{
				ID: sessionID, CreatedAt: created, CWD: spec.CWD, AgentPreset: spec.AgentPreset,
			},
			WorkspaceID: spec.WorkspaceID,
			Config:      config,
		}, nil)
		if err != nil {
			return zero, err
		}
		createdSession = true
	} else {
		if err := a.store.CreateSession(ctx, sessionID, created); err != nil {
			return zero, err
		}
		if err := a.setSessionCWD(ctx, sessionID, spec.CWD); err != nil {
			_ = a.store.DeleteSession(context.Background(), sessionID)
			return zero, err
		}
		if spec.WorkspaceID != "" {
			if err := a.store.SetSessionWorkspace(ctx, sessionID, spec.WorkspaceID); err != nil {
				_ = a.store.DeleteSession(context.Background(), sessionID)
				return zero, err
			}
		}
		configs, ok := a.store.(store.SessionConfigStore)
		if !ok {
			_ = a.store.DeleteSession(context.Background(), sessionID)
			return zero, errors.New("session configuration store is unavailable")
		}
		if err := configs.SetSessionConfig(ctx, sessionID, *config); err != nil {
			_ = a.store.DeleteSession(context.Background(), sessionID)
			return zero, err
		}
		createdSession = true
	}
	if _, err := a.sessionLogForAgent(ctx, sessionID); err != nil {
		_ = a.store.DeleteSession(context.Background(), sessionID)
		return zero, err
	}
	a.markSessionViewed(ctx, sessionID)
	if createdSession {
		a.extensions.PublishSessionStarted(sessionID)
	}
	return webserver.NativeSessionCreateResult{
		SessionID: sessionID, AgentPreset: spec.AgentPreset, CWD: spec.CWD,
	}, nil
}

// webConfig returns the sanitized, flat configuration view served by
// GET /api/config (M10 W2, ADR D-WEB2-D): model/provider/mode, each capability
// gate's enabled flag and the web-server address. Secrets never leave —
// web_server.token is omitted entirely
// (keys live in the environment, never in this config), so a compromised
// settings page cannot leak credentials. Field names are snake_case. P5.1 adds
// the live model panel: the currently active provider's model plus the
// registered providers (id/available/model/candidates) for the pickers.
// contextWindowOf resolves the effective model's context window for the
// ContextMeter (dsh resolveModelInfo: the configured model-directory entry's
// capacity wins, then the catalog default). It honors the per-session
// provider+model selection (store assertion, same as the webserver's config
// handlers) and falls back to the global selection. An unknown model returns
// 0 and the webserver applies its own defaultContextWindow.
func (a *app) contextWindowOf(sessionID string) int {
	return a.modelCapabilityFor(sessionID).ContextWindow
}

// persistDefaultModelSelection stores the shared DSH Agent default. The
// native picker calls this after its session override is accepted; the legacy
// model endpoint uses it too so both Web surfaces have one behavior.
func (a *app) persistDefaultModelSelection(ctx context.Context, provider, model, effort string) {
	if a.store == nil {
		return
	}
	raw, err := encodePersistedModelSelection(provider, model, effort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sta: encode default model selection: %v\n", err)
		return
	}
	persistCtx := ctx
	if a.baseCtx != nil {
		persistCtx = a.baseCtx
	}
	if err := a.store.SetSetting(persistCtx, defaultModelSettingKey, raw); err != nil {
		// DSH keeps the accepted session selection when its settings write fails;
		// report the failure without turning a valid model switch into a false
		// selection error.
		fmt.Fprintf(os.Stderr, "sta: persist default model selection: %v\n", err)
	}
}

// saveNativeDefaultModel applies and persists a native DSH model selection for
// sessions created afterwards. The current session has already received its
// own durable override in nativeSessionSelectModel.
func (a *app) saveNativeDefaultModel(ctx context.Context, provider, model, effort string) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	selection := persistedModelSelection{
		Provider:        strings.TrimSpace(provider),
		Model:           strings.TrimSpace(model),
		ReasoningEffort: strings.TrimSpace(effort),
	}
	a.providerMu.Lock()
	applyModelSelectionToConfig(&a.cfg, selection)
	a.providerMu.Unlock()
	a.persistDefaultModelSelection(ctx, selection.Provider, selection.Model, selection.ReasoningEffort)
}

// setTurnCancel registers the web turn's cancel func for the running turn.
func (a *app) setTurnCancel(sessionID string, cancel context.CancelFunc) {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.turnCancels == nil {
		a.turnCancels = make(map[string]context.CancelFunc)
	}
	a.turnCancels[sessionID] = cancel
}

// clearTurnCancel drops the registered cancel func once the turn settles.
func (a *app) clearTurnCancel(sessionID string) {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	delete(a.turnCancels, sessionID)
}

// stopTurn cancels the running web turn (POST /api/sessions/{id}/stop). It is a
// no-op when no turn is in flight; returns an error only when the id does not
// match the session whose turn is running.
func (a *app) stopTurn(sessionID string) error {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	cancel := a.turnCancels[sessionID]
	if cancel == nil {
		return errors.New("no turn running")
	}
	cancel()
	return nil
}

func (a *app) webConfig() map[string]any {
	_, catalog, err := a.webToolCatalog()
	if err != nil {
		// The legacy config endpoint has no error return. A broken canonical
		// inventory must not silently look like an empty deployment.
		panic(fmt.Sprintf("sta: build web tool catalog: %v", err))
	}
	a.providerMu.RLock()
	cfg := a.cfg
	a.providerMu.RUnlock()
	return map[string]any{
		`commands`:            a.webCommandCatalog(),
		"model":               llmProviderModel(cfg, cfg.LLM.Provider),
		"base_url":            cfg.BaseURL,
		"llm_provider":        cfg.LLM.Provider,
		"reasoning_effort":    cfg.ReasoningEffort,
		"mode":                cfg.Mode,
		"providers":           a.webProviders(), // P5.1 live model pickers
		"mcp_servers":         a.webMCPServers(),
		"tools_enabled":       catalog.Names(),
		"tools_enabled_count": len(catalog.Tools),
		"tool_catalog":        catalog,

		// Capability gates (dsh 对齐: 默认全开, nil*bool→on; 显式 enabled:false 关).
		"terminal_enabled":  config.Enabled(cfg.Terminal.Enabled),
		"fs_enabled":        config.Enabled(cfg.Fs.Enabled),
		"fs_search_enabled": config.Enabled(cfg.FsSearch.Enabled),
		"ralph_enabled":     config.Enabled(cfg.Ralph.Enabled),
		"workflow_enabled":  config.Enabled(cfg.Workflow.Enabled),
		"jobs_enabled":      config.Enabled(cfg.Jobs.Enabled),
		"subagent_enabled":  config.Enabled(cfg.Subagent.Enabled),
		"web_enabled":       config.Enabled(cfg.Web.Enabled),
		"eval_enabled":      config.Enabled(cfg.Eval.Enabled),
		"code_enabled":      config.Enabled(cfg.Code.Enabled) && a.code != nil,
		// code_available is the runtime truth, not merely the configured
		// preference. Native/Web clients must not advertise the code preset when
		// registerCode could not install run_code (for example when the external
		// Node permission runtime is unavailable).
		"code_available":     a.code != nil,
		"interact_enabled":   config.Enabled(cfg.Interact.Enabled),
		"mcp_enabled":        config.Enabled(cfg.Mcp.Enabled),
		"skill_enabled":      config.Enabled(cfg.Skill.Enabled),
		"schedule_enabled":   config.Enabled(cfg.Schedule.Enabled),
		"plan_enabled":       config.Enabled(cfg.Plan.Enabled),
		"spill_enabled":      config.Enabled(cfg.Spill.Enabled),
		"compaction_enabled": config.Enabled(cfg.Compaction.Enabled),
		"multimodal_enabled": cfg.LLM.Multimodal.Enabled != nil && *cfg.LLM.Multimodal.Enabled,

		"web_server_addr": cfg.WebServer.Addr,
	}
}

func (a *app) webToolCatalog() ([]string, tools.CatalogManifest, error) {
	if a.reg == nil {
		manifest, err := tools.NewCatalogManifest(nil)
		return []string{}, manifest, err
	}
	manifest, err := a.reg.CatalogManifest()
	if err != nil {
		return nil, tools.CatalogManifest{}, err
	}
	if err := tools.ValidateCatalogManifest(manifest); err != nil {
		return nil, tools.CatalogManifest{}, err
	}
	return manifest.Names(), manifest, nil
}

func (a *app) webMCPServers() []map[string]any {
	a.providerMu.RLock()
	cfg := a.cfg
	a.providerMu.RUnlock()
	servers := make([]map[string]any, 0, len(cfg.Mcp.Servers))
	for _, server := range cfg.Mcp.Servers {
		prefix := "mcp__" + server.Name + "__"
		toolCount := 0
		if a.reg != nil {
			for _, spec := range a.reg.Specs() {
				if strings.HasPrefix(spec.Name, prefix) {
					toolCount++
				}
			}
		}
		_, connected := a.mcpClientForServer(server.Name)
		servers = append(servers, map[string]any{
			"name": server.Name, "transport": server.Transport, "cmd": server.Cmd, "args": redactMCPArgs(server.Args), "url": redactMCPURL(server.URL), "headers": redactMCPHeaders(server.Headers), "env": redactMCPEnv(server.Env), "cwd": server.Cwd, "tool_call_timeout_ms": server.ToolCallTimeoutMS,
			"fail_on_startup_error": server.FailOnStartupError,
			"enabled":               config.Enabled(cfg.Mcp.Enabled), "connected": connected,
			"tool_count": toolCount,
		})
	}
	return servers
}

func (a *app) webRefreshMCP(ctx context.Context) ([]map[string]any, error) {
	cfg := a.providerConfigSnapshot()
	servers := make([]map[string]any, 0, len(cfg.Mcp.Servers))
	for _, server := range cfg.Mcp.Servers {
		row := map[string]any{"name": server.Name, "transport": server.Transport, "cmd": server.Cmd, "args": redactMCPArgs(server.Args), "url": redactMCPURL(server.URL), "headers": redactMCPHeaders(server.Headers), "env": redactMCPEnv(server.Env), "cwd": server.Cwd, "tool_call_timeout_ms": server.ToolCallTimeoutMS, "fail_on_startup_error": server.FailOnStartupError, "enabled": config.Enabled(cfg.Mcp.Enabled), "connected": false, "tool_count": 0}
		client, ok := a.mcpClientForServer(server.Name)
		if !ok {
			row["error"] = "client is not connected"
			servers = append(servers, row)
			continue
		}
		if err := client.Start(ctx); err != nil {
			row["error"] = redactMCPError(err, server)
			servers = append(servers, row)
			continue
		}
		advertised, err := client.ListTools(ctx)
		if err != nil {
			row["error"] = redactMCPError(err, server)
		} else {
			row["connected"] = true
			row["tool_count"] = len(advertised)
		}
		servers = append(servers, row)
	}
	return servers, nil
}

func redactMCPHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return map[string]string{}
	}
	redacted := make(map[string]string, len(headers))
	for key, value := range headers {
		if strings.TrimSpace(value) == "" {
			redacted[key] = ""
		} else {
			redacted[key] = "[redacted]"
		}
	}
	return redacted
}

func redactMCPEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return map[string]string{}
	}
	redacted := make(map[string]string, len(env))
	for key, value := range env {
		if value == "" {
			redacted[key] = ""
		} else {
			redacted[key] = redactedMCPValue
		}
	}
	return redacted
}

func restoreMCPEnv(projected, previous map[string]string) map[string]string {
	if projected == nil {
		return cloneMCPHeaders(previous)
	}
	result := make(map[string]string, len(projected))
	for key, value := range projected {
		if value == redactedMCPValue {
			if old, ok := previous[key]; ok {
				result[key] = old
			}
			continue
		}
		result[key] = value
	}
	return result
}

const redactedMCPValue = "[redacted]"

// redactMCPArgs protects credential-shaped command-line values crossing the
// Web inventory boundary. It intentionally preserves the flag spelling and
// argument count so diagnostics remain useful. A masked value sent back by a
// settings client is restored from the existing configuration by
// restoreMCPArgs; this makes projection redaction non-destructive.
func redactMCPArgs(args []string) []string {
	if len(args) == 0 {
		return []string{}
	}
	out := append([]string(nil), args...)
	awaitingValue := false
	for i, arg := range args {
		if awaitingValue {
			out[i] = redactedMCPValue
			awaitingValue = false
			continue
		}
		name, value, hasValue := splitMCPArg(arg)
		if hasValue && sensitiveMCPArgName(name) {
			out[i] = strings.TrimSuffix(arg, value) + redactedMCPValue
			continue
		}
		if !hasValue && sensitiveMCPArgName(name) {
			awaitingValue = true
		}
		if !hasValue && sensitiveMCPEnvAssignment(arg) {
			key := strings.SplitN(arg, "=", 2)[0]
			out[i] = key + "=" + redactedMCPValue
		}
	}
	return out
}

func splitMCPArg(arg string) (name, value string, hasValue bool) {
	trimmed := strings.TrimSpace(arg)
	if strings.HasPrefix(trimmed, "--") {
		if at := strings.IndexByte(trimmed, '='); at > 2 {
			return trimmed[:at], trimmed[at+1:], true
		}
		return trimmed, "", false
	}
	if at := strings.IndexByte(trimmed, '='); at > 0 {
		return trimmed[:at], trimmed[at+1:], true
	}
	return trimmed, "", false
}

func normalizedMCPArgName(name string) string {
	name = strings.TrimLeft(strings.ToLower(strings.TrimSpace(name)), "-")
	name = strings.ReplaceAll(name, "-", "")
	name = strings.ReplaceAll(name, "_", "")
	return name
}

func sensitiveMCPArgName(name string) bool {
	switch normalizedMCPArgName(name) {
	case "apikey", "token", "accesstoken", "password", "passwd", "secret", "clientsecret", "authorization", "auth", "bearer", "header", "credential", "credentials", "privatekey":
		return true
	default:
		return false
	}
}

func sensitiveMCPEnvAssignment(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return false
	}
	key, _, ok := strings.Cut(arg, "=")
	return ok && sensitiveMCPArgName(key)
}

func restoreMCPArgs(masked, previous []string) []string {
	out := append([]string(nil), masked...)
	for i := range out {
		if i >= len(previous) {
			continue
		}
		if out[i] == redactedMCPValue || strings.HasSuffix(out[i], "="+redactedMCPValue) {
			out[i] = previous[i]
		}
	}
	return out
}

func mergeMCPHeaders(next, previous map[string]string) map[string]string {
	merged := cloneMCPHeaders(next)
	for key, value := range merged {
		if value == redactedMCPValue {
			if old, ok := previous[key]; ok {
				merged[key] = old
			}
		}
	}
	return merged
}

func redactMCPURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			parsed.User = url.UserPassword(parsed.User.Username(), redactedMCPValue)
		}
	}
	query := parsed.Query()
	changed := false
	for key, values := range query {
		if !sensitiveMCPArgName(key) {
			continue
		}
		for i := range values {
			if values[i] != "" {
				values[i] = redactedMCPValue
				changed = true
			}
		}
		query[key] = values
	}
	if changed {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

func restoreMCPURL(masked, previous string) string {
	maskedURL, maskedErr := url.Parse(masked)
	previousURL, previousErr := url.Parse(previous)
	if maskedErr != nil || previousErr != nil {
		return masked
	}
	if maskedURL.User != nil && previousURL.User != nil {
		if password, ok := maskedURL.User.Password(); ok && password == redactedMCPValue {
			if previousPassword, previousOK := previousURL.User.Password(); previousOK {
				maskedURL.User = url.UserPassword(maskedURL.User.Username(), previousPassword)
			}
		}
	}
	query := maskedURL.Query()
	previousQuery := previousURL.Query()
	changed := false
	for key, values := range query {
		if !sensitiveMCPArgName(key) {
			continue
		}
		oldValues := previousQuery[key]
		for i := range values {
			if values[i] == redactedMCPValue && i < len(oldValues) {
				values[i] = oldValues[i]
				changed = true
			}
		}
		query[key] = values
	}
	if changed {
		maskedURL.RawQuery = query.Encode()
	}
	return maskedURL.String()
}

func redactMCPError(err error, server config.McpServer) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	maskedArgs := redactMCPArgs(server.Args)
	for i, value := range server.Args {
		if i < len(maskedArgs) && maskedArgs[i] != value && value != "" {
			message = strings.ReplaceAll(message, value, maskedArgs[i])
		}
	}
	for key, value := range server.Headers {
		if value != "" {
			message = strings.ReplaceAll(message, value, redactedMCPValue)
		}
		message = strings.ReplaceAll(message, key+": "+value, key+": "+redactedMCPValue)
	}
	for key, value := range server.Env {
		if value != "" {
			message = strings.ReplaceAll(message, value, redactedMCPValue)
		}
		message = strings.ReplaceAll(message, key+"="+value, key+"="+redactedMCPValue)
	}
	if server.URL != "" {
		message = strings.ReplaceAll(message, server.URL, redactMCPURL(server.URL))
	}
	return message
}

func (a *app) webManageMCP(ctx context.Context, action string, edit webserver.MCPServerEdit) ([]map[string]any, error) {
	name := strings.TrimSpace(edit.Name)
	original := strings.TrimSpace(edit.OriginalName)
	if action == "delete" {
		if original == "" {
			original = name
		}
		if original == "" {
			return nil, errors.New("mcp server name is required")
		}
	} else {
		transport := strings.ToLower(strings.TrimSpace(edit.Transport))
		if transport == "" {
			transport = "stdio"
		}
		if transport == "http" || transport == "https" {
			transport = "streamable-http"
		}
		if transport != "stdio" && transport != "streamable-http" {
			return nil, fmt.Errorf("unsupported mcp transport %q", edit.Transport)
		}
		if name == "" || (transport == "stdio" && strings.TrimSpace(edit.Cmd) == "") || (transport == "streamable-http" && strings.TrimSpace(edit.URL) == "") {
			return nil, errors.New("mcp server name and transport endpoint are required")
		}
		if strings.ContainsAny(name, `/\\`) || strings.ContainsAny(name, " \t\r\n") {
			return nil, errors.New("mcp server name must not contain spaces or path separators")
		}
	}
	a.providerMu.RLock()
	next := append([]config.McpServer(nil), a.cfg.Mcp.Servers...)
	a.providerMu.RUnlock()
	find := func(key string) int {
		for i := range next {
			if next[i].Name == key {
				return i
			}
		}
		return -1
	}
	switch action {
	case "add":
		if find(name) >= 0 {
			return nil, fmt.Errorf("mcp server %q already exists", name)
		}
		failOnStartup := false
		if edit.FailOnStartupError != nil {
			failOnStartup = *edit.FailOnStartupError
		}
		timeout := 60000
		if edit.ToolCallTimeoutMS != nil {
			timeout = *edit.ToolCallTimeoutMS
		}
		if timeout <= 0 {
			return nil, errors.New("mcp tool call timeout must be positive")
		}
		cwd := ""
		if edit.Cwd != nil {
			cwd = strings.TrimSpace(*edit.Cwd)
		}
		next = append(next, config.McpServer{Name: name, Transport: normalizedMCPTransport(edit.Transport), Cmd: strings.TrimSpace(edit.Cmd), Args: append([]string(nil), edit.Args...), URL: strings.TrimSpace(edit.URL), Headers: cloneMCPHeaders(edit.Headers), Env: cloneMCPHeaders(edit.Env), Cwd: cwd, ToolCallTimeoutMS: timeout, FailOnStartupError: failOnStartup})
	case "update":
		if original == "" {
			original = name
		}
		idx := find(original)
		if idx < 0 {
			return nil, fmt.Errorf("mcp server %q not found", original)
		}
		if name != original && find(name) >= 0 {
			return nil, fmt.Errorf("mcp server %q already exists", name)
		}
		previous := next[idx]
		failOnStartup := previous.FailOnStartupError
		if edit.FailOnStartupError != nil {
			failOnStartup = *edit.FailOnStartupError
		}
		timeout := previous.ToolCallTimeoutMS
		if edit.ToolCallTimeoutMS != nil {
			timeout = *edit.ToolCallTimeoutMS
		}
		if timeout <= 0 {
			return nil, errors.New("mcp tool call timeout must be positive")
		}
		cwd := previous.Cwd
		if edit.Cwd != nil {
			cwd = strings.TrimSpace(*edit.Cwd)
		}
		next[idx] = config.McpServer{Name: name, Transport: normalizedMCPTransport(edit.Transport), Cmd: strings.TrimSpace(edit.Cmd), Args: restoreMCPArgs(edit.Args, previous.Args), URL: restoreMCPURL(strings.TrimSpace(edit.URL), previous.URL), Headers: mergeMCPHeaders(edit.Headers, previous.Headers), Env: restoreMCPEnv(edit.Env, previous.Env), Cwd: cwd, ToolCallTimeoutMS: timeout, FailOnStartupError: failOnStartup}
	case "delete":
		idx := find(original)
		if idx < 0 {
			return nil, fmt.Errorf("mcp server %q not found", original)
		}
		next = append(next[:idx], next[idx+1:]...)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := a.store.SetSetting(ctx, "mcp.servers", string(raw)); err != nil {
		return nil, err
	}
	a.providerMu.Lock()
	a.cfg.Mcp.Servers = append([]config.McpServer(nil), next...)
	a.providerMu.Unlock()
	return a.webMCPServers(), nil
}

func normalizedMCPTransport(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "stdio"
	}
	if value == "http" || value == "https" {
		return "streamable-http"
	}
	return value
}

func cloneMCPHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[key] = value
	}
	return out
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

// providerReasoning returns the per-model reasoning catalog for a built-in
// provider: model id → its effort choices. Only providers whose models declare
// a reasoning capability contribute entries; the rest return an empty map.
func providerReasoning(id string) map[string]modelReasoning {
	return reasoningCatalogForModels(builtinModelCatalog[id])
}

func reasoningCatalogForModels(models []customModel) map[string]modelReasoning {
	out := map[string]modelReasoning{}
	for _, m := range models {
		if m.Reasoning == nil || !*m.Reasoning {
			if len(m.ReasoningEfforts) == 0 {
				continue
			}
		}
		if len(m.ReasoningEfforts) > 0 {
			ids := make([]string, 0, len(m.ReasoningEfforts))
			for id := range m.ReasoningEfforts {
				ids = append(ids, strings.ToLower(strings.TrimSpace(id)))
			}
			sort.Slice(ids, func(i, j int) bool { return reasoningEffortRank(ids[i]) < reasoningEffortRank(ids[j]) })
			efforts := make([]modelEffort, 0, len(ids))
			for _, id := range ids {
				efforts = append(efforts, modelEffort{ID: id, Name: reasoningEffortName(id)})
			}
			defaultEffort := strings.ToLower(strings.TrimSpace(m.DefaultReasoningEffort))
			if defaultEffort == "" {
				for _, effort := range efforts {
					if effort.ID != "off" {
						defaultEffort = effort.ID
						break
					}
				}
			}
			out[m.ID] = modelReasoning{Efforts: efforts, DefaultEffort: defaultEffort}
			continue
		}
		defaultEffort := strings.ToLower(strings.TrimSpace(m.DefaultReasoningEffort))
		if defaultEffort == "" {
			defaultEffort = "high"
		}
		out[m.ID] = modelReasoning{
			Efforts: []modelEffort{
				{ID: "off", Name: "Off"},
				{ID: "low", Name: "Low"},
				{ID: "high", Name: "High"},
				{ID: "max", Name: "Max"},
			},
			DefaultEffort: defaultEffort,
		}
	}
	return out
}

func reasoningEffortRank(id string) int {
	for rank, candidate := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if id == candidate {
			return rank
		}
	}
	return 100
}

func reasoningEffortName(id string) string {
	if id == "" {
		return id
	}
	return strings.ToUpper(id[:1]) + id[1:]
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
	a.providerMu.RLock()
	cfg := a.cfg
	profiles := cloneBuiltinProfiles(a.builtinProfiles)
	keys := cloneStringMap(a.llmKeys)
	customProviders := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()
	out := make([]map[string]any, 0, len(builtinProviders)+len(customProviders))
	// Built-in provider directory (M11-pi-ai): every provider pi-ai can
	// authenticate with an API key is listed, registered or not, so the settings
	// page can add their key. deepseek/openai/anthropic keep their
	// config-driven model/base_url (config.yaml llm.* sections); the rest carry
	// the directory default.
	for _, bp := range builtinProviders {
		model := bp.model
		baseURL := bp.baseURL
		if bp.id == "deepseek-official" || bp.id == "openai" || bp.id == "anthropic" {
			model = llmProviderModel(cfg, bp.id)
			baseURL = llmProviderBaseURL(cfg, bp.id)
		}
		// dsh ProviderEditor 自定义设置 对齐: a persisted llm.profile.<id>
		// override wins over config.yaml for base URL / model / model list.
		prof, overridden := profiles[bp.id]
		if overridden {
			if prof.BaseURL != "" {
				baseURL = prof.BaseURL
			}
			model = effectiveProfileModel(prof, model)
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
			"configured":       providerKeyFromSnapshot(keys, bp.id) != "",
			"model":            model,
			"base_url":         baseURL,
			"candidates":       modelCandidates(bp.id),
			"env_var":          providerEnv(bp.id),
			"reasoning":        providerReasoning(bp.id),
			"profile_override": overridden,
		}
		if overridden && len(prof.Models) > 0 {
			entry["reasoning"] = reasoningCatalogForModels(prof.Models)
		}
		if rows := modelCatalogRows(bp.id, profiles, customProviders); len(rows) > 0 {
			entry["catalog_models"] = rows
		}
		if overridden && len(prof.Models) > 0 {
			entry["models"] = prof.Models
		}
		out = append(out, entry)
	}
	// M11 custom providers from settings.
	for _, cp := range customProviders {
		model := effectiveCustomProviderModel(cp)
		registered := false
		available := false
		if p, err := reg.Get(cp.ID); err == nil {
			registered = true
			available = p.Available()
		}
		entry := map[string]any{
			"id":             cp.ID,
			"name":           cp.Name,
			"custom":         true,
			"registered":     registered,
			"available":      available,
			"configured":     providerKeyFromSnapshot(keys, cp.ID) != "",
			"model":          model,
			"base_url":       cp.BaseURL,
			"candidates":     nil,
			"env_var":        llmKeyEnv(cp.ID),
			"protocol":       cp.Protocol,
			"protocol_label": protocolLabel(providerProtocol(cp.Protocol)),
			"models":         cp.Models,
			"reasoning":      reasoningCatalogForModels(cp.Models),
		}
		if rows := modelCatalogRows(cp.ID, profiles, customProviders); len(rows) > 0 {
			entry["catalog_models"] = rows
		}
		out = append(out, entry)
	}
	return out
}

// webSaveProvider persists a provider edit for a built-in provider (M11, POST
// /api/config/provider): it writes the API-key override (llm.key.<id>) and,
// when the edit carries 自定义设置 changes (dsh ProviderEditor 对齐), a profile
// override (llm.profile.<id> = base_url / model / model list), then rebuilds the
// registry so the change applies immediately (no restart). It runs under the
// control lock and the legacy turn lock
// (D5 serial). An empty api_key removes the key override, falling back to the
// env var; an empty base_url/model/models removes the profile override.
func (a *app) webSaveProvider(ctx context.Context, id, apiKey, baseURL, model string, models []customModel) (err error) {
	if err := validateCustomModels(models); err != nil {
		return err
	}
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	a.providerStateMu.Lock()
	stateReleased := false
	defer func() {
		if !stateReleased {
			a.providerStateMu.Unlock()
		}
	}()
	if a.llmReg == nil {
		return fmt.Errorf("llm not registered")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("provider id is required")
	}
	oldSettings, err := a.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	oldCredential, oldCredentialPresent := a.credentialOverride(id)
	a.providerMu.RLock()
	oldKeys := cloneStringMap(a.llmKeys)
	oldProfiles := cloneBuiltinProfiles(a.builtinProfiles)
	a.providerMu.RUnlock()
	mutated := false
	committed := false
	defer func() {
		if committed || !mutated {
			return
		}
		rollbackErr := a.restoreProviderCredential(ctx, id, oldCredential, oldCredentialPresent)
		rollbackErr = errors.Join(rollbackErr, a.restoreProviderSettings(oldSettings, id, "llm.profile."))
		a.providerMu.Lock()
		a.llmKeys = oldKeys
		a.builtinProfiles = oldProfiles
		a.providerMu.Unlock()
		if rebuildErr := a.registerLLMUnlocked(); rebuildErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("provider registry rollback: %w", rebuildErr))
		}
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("provider edit rollback: %w", rollbackErr))
		}
	}()
	if apiKey != "" {
		if err := a.setProviderCredential(ctx, id, apiKey); err != nil {
			return err
		}
		mutated = true
		a.providerMu.Lock()
		if a.llmKeys == nil {
			a.llmKeys = map[string]string{}
		}
		a.llmKeys[id] = apiKey
		a.providerMu.Unlock()
	} else {
		if err := a.deleteProviderCredential(ctx, id); err != nil {
			return err
		}
		mutated = true
		a.providerMu.Lock()
		delete(a.llmKeys, id)
		a.providerMu.Unlock()
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
		mutated = true
		a.providerMu.Lock()
		if a.builtinProfiles == nil {
			a.builtinProfiles = map[string]builtinProviderProfile{}
		}
		a.builtinProfiles[id] = builtinProviderProfile{BaseURL: baseURL, Model: model, Models: models}
		a.providerMu.Unlock()
	} else {
		a.providerMu.RLock()
		_, profileExists := a.builtinProfiles[id]
		a.providerMu.RUnlock()
		if profileExists {
			if err := a.store.DeleteSetting(ctx, "llm.profile."+id); err != nil {
				return err
			}
			mutated = true
			a.providerMu.Lock()
			delete(a.builtinProfiles, id)
			a.providerMu.Unlock()
		}
	}
	// Rebuild the registry so the new key/profile is live immediately.
	if err := a.registerLLMUnlocked(); err != nil {
		return err
	}
	a.providerStateMu.Unlock()
	stateReleased = true
	if a.webserver != nil {
		a.webserver.NotifyNativeLLMAdaptersUpdated()
	}
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	committed = true
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
func (a *app) webSaveCustomProvider(ctx context.Context, id, name, baseURL, model, apiKey, protocol string, models []customModel) (err error) {
	if err := validateCustomModels(models); err != nil {
		return err
	}
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	a.providerStateMu.Lock()
	stateReleased := false
	defer func() {
		if !stateReleased {
			a.providerStateMu.Unlock()
		}
	}()
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
	oldSettings, err := a.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	oldCredential, oldCredentialPresent := a.credentialOverride(id)
	a.providerMu.RLock()
	oldKeys := cloneStringMap(a.llmKeys)
	oldProviders := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()
	mutated := false
	committed := false
	defer func() {
		if committed || !mutated {
			return
		}
		rollbackErr := a.restoreProviderCredential(ctx, id, oldCredential, oldCredentialPresent)
		rollbackErr = errors.Join(rollbackErr, a.restoreProviderSettings(oldSettings, id, "llm.custom."))
		a.providerMu.Lock()
		a.llmKeys = oldKeys
		a.customProviders = oldProviders
		a.providerMu.Unlock()
		if rebuildErr := a.registerLLMUnlocked(); rebuildErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("provider registry rollback: %w", rebuildErr))
		}
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("custom provider rollback: %w", rollbackErr))
		}
	}()
	raw, err := json.Marshal(customProviderProfile{ID: id, Name: name, BaseURL: baseURL, Model: model, Protocol: protocol, Models: models})
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(ctx, "llm.custom."+id, string(raw)); err != nil {
		return err
	}
	mutated = true
	if apiKey != "" {
		if err := a.setProviderCredential(ctx, id, apiKey); err != nil {
			return err
		}
		mutated = true
		a.providerMu.Lock()
		if a.llmKeys == nil {
			a.llmKeys = map[string]string{}
		}
		a.llmKeys[id] = apiKey
		a.providerMu.Unlock()
	}
	profile := customProviderProfile{ID: id, Name: name, BaseURL: baseURL, Model: model, Protocol: protocol, Models: models}
	a.providerMu.Lock()
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
	a.providerMu.Unlock()
	if err := a.registerLLMUnlocked(); err != nil {
		return err
	}
	a.providerStateMu.Unlock()
	stateReleased = true
	if a.webserver != nil {
		a.webserver.NotifyNativeLLMAdaptersUpdated()
	}
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	committed = true
	return nil
}

// applyNativeProviderSettingsRuntime bridges compatible native settings
// documents to Shutu's live runtime. The webserver owns the redacted settings
// view; this composition-root callback owns runtime facts and rebuilds the
// registered provider generation, default model, or permission default.
func (a *app) applyNativeProviderSettingsRuntime(ctx context.Context, namespace string, resolved map[string]any) (err error) {
	switch namespace {
	case "agent-default-model":
		provider := strings.TrimSpace(nativeRuntimeString(resolved["provider"]))
		model := strings.TrimSpace(nativeRuntimeString(resolved["model"]))
		if provider == "" || model == "" {
			return errors.New("agent-default-model requires provider and model")
		}
		return a.webSwitchModel(ctx, provider, model, strings.TrimSpace(nativeRuntimeString(resolved["reasoningEffort"])))
	case "permission":
		preset, _, _, _ := permissionBundle(nativeRuntimeString(resolved["defaultPreset"]))
		if preset == "" {
			return errors.New("permission.defaultPreset is required")
		}
		return a.store.SetSetting(ctx, "permission_preset", preset)
	case "shell":
		a.applyNativeShellSettings(resolved)
		return nil
	case "agent-loop":
		a.applyNativeAgentLoopSettings(resolved)
		return nil
	case "web-search-deepseek":
		a.applyNativeWebSearchSettings(resolved)
		return nil
	case "ui-theme", "locale", "ui-conversation":
		// Browser-owned presentation preferences have no additional
		// host-owned runtime fact.
		return nil
	}
	if namespace != "llm-deepseek" && namespace != "llm-pi-ai" {
		return nil
	}
	desiredProfiles, desiredCustom, err := nativeProviderRuntimeFacts(namespace, resolved)
	if err != nil {
		return err
	}

	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	a.providerStateMu.Lock()
	stateReleased := false
	defer func() {
		if !stateReleased {
			a.providerStateMu.Unlock()
		}
	}()
	if a.llmReg == nil {
		return errors.New("llm not registered")
	}
	oldSettings, err := a.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	a.providerMu.RLock()
	oldProfiles := cloneBuiltinProfiles(a.builtinProfiles)
	oldCustom := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()

	writes := make(map[string]*string, len(desiredProfiles)+len(desiredCustom))
	for id, profile := range desiredProfiles {
		if profile.BaseURL == "" && profile.Model == "" && len(profile.Models) == 0 {
			writes["llm.profile."+id] = nil
			continue
		}
		raw, marshalErr := json.Marshal(profile)
		if marshalErr != nil {
			return marshalErr
		}
		value := string(raw)
		writes["llm.profile."+id] = &value
	}
	for id, profile := range desiredCustom {
		raw, marshalErr := json.Marshal(profile)
		if marshalErr != nil {
			return marshalErr
		}
		value := string(raw)
		writes["llm.custom."+id] = &value
	}
	for id := range oldProfiles {
		if namespace == "llm-deepseek" && id != "deepseek-official" {
			continue
		}
		if namespace == "llm-pi-ai" && id == "deepseek-official" {
			continue
		}
		if _, wanted := desiredProfiles[id]; !wanted {
			writes["llm.profile."+id] = nil
		}
	}
	for _, old := range oldCustom {
		if _, wanted := desiredCustom[old.ID]; !wanted {
			writes["llm.custom."+old.ID] = nil
		}
	}

	if err := a.commitNativeProviderSettings(ctx, oldSettings, writes); err != nil {
		return err
	}
	mutated := len(writes) > 0
	a.providerMu.Lock()
	a.builtinProfiles = desiredProfiles
	nextCustom := make([]customProviderProfile, 0, len(desiredCustom))
	for _, profile := range desiredCustom {
		nextCustom = append(nextCustom, profile)
	}
	sort.Slice(nextCustom, func(left, right int) bool { return nextCustom[left].ID < nextCustom[right].ID })
	a.customProviders = nextCustom
	a.providerMu.Unlock()
	if err := a.registerLLMUnlocked(); err != nil {
		if !mutated {
			return err
		}
		rollbackErr := a.restoreNativeProviderSettings(ctx, oldSettings, writes)
		a.providerMu.Lock()
		a.builtinProfiles = oldProfiles
		a.customProviders = oldCustom
		a.providerMu.Unlock()
		if rebuildErr := a.registerLLMUnlocked(); rebuildErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("provider registry rollback: %w", rebuildErr))
		}
		return errors.Join(err, rollbackErr)
	}
	a.providerStateMu.Unlock()
	stateReleased = true
	if a.webserver != nil {
		a.webserver.NotifyNativeLLMAdaptersUpdated()
	}
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	return nil
}

// nativeProviderRuntimeFacts converts the DSH resolved settings value into the
// provider facts owned by Shutu's runtime. The provider id is the map key; the
// DeepSeek namespace has one root profile while pi-ai carries a provider dict.
func nativeProviderRuntimeFacts(namespace string, resolved map[string]any) (map[string]builtinProviderProfile, map[string]customProviderProfile, error) {
	profiles := map[string]builtinProviderProfile{}
	custom := map[string]customProviderProfile{}
	if resolved == nil {
		return profiles, custom, nil
	}
	entries := map[string]map[string]any{}
	if namespace == "llm-deepseek" {
		entries["deepseek-official"] = resolved
	} else {
		providers, _ := resolved["providers"].(map[string]any)
		for id, raw := range providers {
			profile, _ := raw.(map[string]any)
			entries[id] = profile
		}
	}
	for id, raw := range entries {
		id = strings.TrimSpace(id)
		if id == "" || raw == nil {
			continue
		}
		models := nativeProviderRuntimeModels(raw["models"])
		model := strings.TrimSpace(nativeRuntimeString(raw["model"]))
		if model == "" && len(models) > 0 {
			model = models[0].ID
		}
		baseURL := strings.TrimSpace(nativeRuntimeString(raw["baseURL"]))
		if _, builtin := builtinProviderByID(id); builtin {
			profiles[id] = builtinProviderProfile{BaseURL: baseURL, Model: model, Models: models}
			continue
		}
		if baseURL == "" || model == "" {
			return nil, nil, fmt.Errorf("provider %q requires baseURL and model", id)
		}
		protocol := strings.TrimSpace(nativeRuntimeString(raw["api"]))
		if protocol != "" && !validProtocol(protocol) {
			return nil, nil, fmt.Errorf("provider %q has unsupported protocol %q", id, protocol)
		}
		if protocol == "" {
			protocol = string(protocolCompletions)
		}
		name := strings.TrimSpace(nativeRuntimeString(raw["displayName"]))
		if name == "" {
			name = id
		}
		custom[id] = customProviderProfile{
			ID: id, Name: name, BaseURL: baseURL, Model: model,
			Protocol: protocol, Models: models,
		}
	}
	for _, profile := range custom {
		if err := validateCustomModels(profile.Models); err != nil {
			return nil, nil, err
		}
	}
	return profiles, custom, nil
}

func nativeProviderRuntimeModels(raw any) []customModel {
	rows, _ := raw.([]any)
	models := make([]customModel, 0, len(rows))
	for _, row := range rows {
		item, _ := row.(map[string]any)
		if item == nil {
			continue
		}
		id := strings.TrimSpace(nativeRuntimeString(item["id"]))
		if id == "" {
			continue
		}
		model := customModel{ID: id, Name: strings.TrimSpace(nativeRuntimeString(item["name"]))}
		model.ContextWindow = nativeRuntimeInt(item["contextWindow"])
		model.MaxTokens = nativeRuntimeInt(item["maxTokens"])
		model.DefaultMaxTokens = nativeRuntimeInt(item["defaultMaxTokens"])
		models = append(models, model)
	}
	return models
}

func nativeRuntimeString(value any) string {
	text, _ := value.(string)
	return text
}

func nativeRuntimeInt(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	default:
		return 0
	}
}

func (a *app) commitNativeProviderSettings(ctx context.Context, oldSettings map[string]string, writes map[string]*string) error {
	if len(writes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(writes))
	for key := range writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	written := make([]string, 0, len(keys))
	for _, key := range keys {
		var err error
		if value := writes[key]; value == nil {
			err = a.store.DeleteSetting(ctx, key)
		} else {
			err = a.store.SetSetting(ctx, key, *value)
		}
		if err != nil {
			restoreErr := a.restoreNativeProviderSettings(ctx, oldSettings, writes)
			return errors.Join(err, restoreErr)
		}
		written = append(written, key)
	}
	return nil
}

func (a *app) restoreNativeProviderSettings(ctx context.Context, oldSettings map[string]string, writes map[string]*string) error {
	var first error
	for key := range writes {
		old, existed := oldSettings[key]
		var err error
		switch {
		case !existed || old == "":
			err = a.store.DeleteSetting(ctx, key)
		default:
			err = a.store.SetSetting(ctx, key, old)
		}
		if err != nil {
			first = errors.Join(first, fmt.Errorf("restore %s: %w", key, err))
		}
	}
	return first
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneBuiltinProfiles(in map[string]builtinProviderProfile) map[string]builtinProviderProfile {
	if in == nil {
		return nil
	}
	out := make(map[string]builtinProviderProfile, len(in))
	for key, profile := range in {
		profile.Models = cloneCustomModels(profile.Models)
		out[key] = profile
	}
	return out
}

func cloneCustomModels(in []customModel) []customModel {
	if in == nil {
		return nil
	}
	out := make([]customModel, len(in))
	for i, model := range in {
		out[i] = model
		out[i].Input = append([]string(nil), model.Input...)
		out[i].ReasoningEfforts = cloneReasoningEfforts(model.ReasoningEfforts)
	}
	return out
}

func cloneReasoningEfforts(in map[string]*string) map[string]*string {
	if in == nil {
		return nil
	}
	out := make(map[string]*string, len(in))
	for effort, wire := range in {
		if wire == nil {
			out[effort] = nil
			continue
		}
		value := *wire
		out[effort] = &value
	}
	return out
}

// setProviderCredential is the single Web provider-key persistence seam.
// Production uses the dedicated credential backend; the llm.key setting branch
// is retained only for lightweight embedders that predate the credential vault.
func (a *app) setProviderCredential(ctx context.Context, provider, value string) error {
	if a.credentials != nil {
		return a.credentials.Set(ctx, llmKeyEnv(provider), value)
	}
	return a.store.SetSetting(ctx, "llm.key."+provider, value)
}

func (a *app) deleteProviderCredential(ctx context.Context, provider string) error {
	if a.credentials != nil {
		return a.credentials.Unset(ctx, llmKeyEnv(provider))
	}
	return a.store.DeleteSetting(ctx, "llm.key."+provider)
}

// restoreProviderCredential returns the dedicated credential boundary to its
// pre-edit state. Presence—not non-empty generic settings—is authoritative for
// whether an override existed before the failed Web mutation.
func (a *app) restoreProviderCredential(ctx context.Context, provider, value string, present bool) error {
	if present && value != "" {
		return a.setProviderCredential(ctx, provider, value)
	}
	return a.deleteProviderCredential(ctx, provider)
}

// restoreProviderSettings restores only settings touched by one provider edit.
// It uses a process context because rollback must still run when the HTTP
// request that initiated the edit has already been canceled.
func (a *app) restoreProviderSettings(previous map[string]string, id string, prefixes ...string) error {
	if a.store == nil {
		return nil
	}
	ctx := context.Background()
	if a.baseCtx != nil && a.baseCtx.Err() == nil {
		ctx = a.baseCtx
	}
	var first error
	for _, prefix := range prefixes {
		key := prefix + id
		value, present := previous[key]
		var err error
		if present {
			err = a.store.SetSetting(ctx, key, value)
		} else {
			err = a.store.DeleteSetting(ctx, key)
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// webDeleteCustomProvider removes a custom provider declaration (M11, DELETE
// /api/config/provider): it deletes llm.custom.<id> and its key override, then
// rebuilds the registry. Built-in providers cannot be removed.
func (a *app) webDeleteCustomProvider(ctx context.Context, id string) (err error) {
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	a.providerStateMu.Lock()
	stateReleased := false
	defer func() {
		if !stateReleased {
			a.providerStateMu.Unlock()
		}
	}()
	if a.llmReg == nil {
		return fmt.Errorf("llm not registered")
	}
	id = strings.TrimSpace(id)
	if _, ok := builtinProviderByID(id); ok {
		return errors.New("built-in providers cannot be removed")
	}
	a.providerMu.RLock()
	found := false
	for _, cp := range a.customProviders {
		if cp.ID == id {
			found = true
			break
		}
	}
	a.providerMu.RUnlock()
	if !found {
		return fmt.Errorf("custom provider %q not found", id)
	}
	oldSettings, err := a.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	oldCredential, oldCredentialPresent := a.credentialOverride(id)
	a.providerMu.RLock()
	oldKeys := cloneStringMap(a.llmKeys)
	oldProviders := append([]customProviderProfile(nil), a.customProviders...)
	a.providerMu.RUnlock()
	mutated := false
	committed := false
	defer func() {
		if committed || !mutated {
			return
		}
		rollbackErr := a.restoreProviderCredential(ctx, id, oldCredential, oldCredentialPresent)
		rollbackErr = errors.Join(rollbackErr, a.restoreProviderSettings(oldSettings, id, "llm.custom."))
		a.providerMu.Lock()
		a.llmKeys = oldKeys
		a.customProviders = oldProviders
		a.providerMu.Unlock()
		// registerLLM publishes only after a complete build. Rebuild the prior
		// snapshot so a failed delete cannot leave the durable and live
		// registries diverged.
		if rebuildErr := a.registerLLMUnlocked(); rebuildErr != nil {
			if rollbackErr == nil {
				rollbackErr = rebuildErr
			} else {
				rollbackErr = errors.Join(rollbackErr, rebuildErr)
			}
		}
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("custom provider delete rollback: %w", rollbackErr))
		}
	}()
	if err := a.store.DeleteSetting(ctx, "llm.custom."+id); err != nil {
		return err
	}
	mutated = true
	if err := a.deleteProviderCredential(ctx, id); err != nil {
		return err
	}
	a.providerMu.Lock()
	kept := a.customProviders[:0]
	for _, cp := range a.customProviders {
		if cp.ID != id {
			kept = append(kept, cp)
		}
	}
	a.customProviders = kept
	delete(a.llmKeys, id)
	a.providerMu.Unlock()
	if err := a.registerLLMUnlocked(); err != nil {
		return err
	}
	a.providerStateMu.Unlock()
	stateReleased = true
	committed = true
	if a.webserver != nil {
		a.webserver.NotifyNativeLLMAdaptersUpdated()
	}
	if a.compaction != nil {
		if err := a.registerCompaction(); err != nil {
			return err
		}
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
// rebuilds the selected LLM provider — no restart. It runs under the control
// lock and legacy turn lock (D5
// serial: no turn is in flight while the selection swaps) and registerLLM
// publishes the new pointer under llmMu, so the very next message (buildLoop
// re-wires every turn) talks to the new provider. The accepted selection is
// also stored in the durable settings table as the shared default; config.yaml
// remains the base configuration. Fail-closed: on error the previous selection
// is fully restored.
func (a *app) webSwitchModel(ctx context.Context, provider, model, effort string) error {
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	// Hold the publication barrier across both the config mutation and the
	// complete registry rebuild. Agent-backed turns take its read side when
	// resolving their runtime, so they see either the old pair or the new pair,
	// never a new provider id against an old registry.
	a.providerStateMu.Lock()
	if a.llmReg == nil {
		a.providerStateMu.Unlock()
		return fmt.Errorf("llm not registered")
	}
	if provider != "" {
		p, err := a.llmReg.Get(provider)
		if err != nil {
			a.providerStateMu.Unlock()
			return fmt.Errorf("unknown provider %q (registered: %s)", provider, llmProviderIDs(a.llmReg))
		}
		if !p.Available() {
			a.providerStateMu.Unlock()
			return fmt.Errorf("provider %q not available (missing %s)", provider, llmCredentialEnv(provider))
		}
	}
	a.providerMu.RLock()
	currentCfg := a.cfg.Clone()
	a.providerMu.RUnlock()
	target := provider
	if target == "" {
		target = currentCfg.LLM.Provider
	}
	candidateModel := model
	if candidateModel == "" {
		candidateModel = llmProviderModel(currentCfg, target)
	}
	if effort != "" {
		if err := validateModelCapabilityForSelection(a.modelCapabilityForRouteWithConfig(currentCfg, target, candidateModel), effort); err != nil {
			a.providerStateMu.Unlock()
			return err
		}
	}
	// Snapshot for rollback.
	oldProvider := currentCfg.LLM.Provider
	oldModel, oldOpenAI, oldAnthropic := currentCfg.Model, currentCfg.LLM.OpenAI.Model, currentCfg.LLM.Anthropic.Model
	oldEffort := currentCfg.ReasoningEffort
	a.providerMu.Lock()
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
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
		a.cfg.ReasoningEffort = effort
	default:
		a.providerMu.Unlock()
		a.providerStateMu.Unlock()
		return fmt.Errorf("unknown reasoning effort %q (want off|low|high|max)", effort)
	}
	a.providerMu.Unlock()
	if err := a.registerLLMUnlocked(); err != nil {
		// Restore the previous selection — never leave a half-applied switch.
		a.providerMu.Lock()
		a.cfg.LLM.Provider = oldProvider
		a.cfg.Model, a.cfg.LLM.OpenAI.Model, a.cfg.LLM.Anthropic.Model = oldModel, oldOpenAI, oldAnthropic
		a.cfg.ReasoningEffort = oldEffort
		a.providerMu.Unlock()
		a.providerStateMu.Unlock()
		return err
	}
	a.providerStateMu.Unlock()
	// Rebuild compaction on the new provider so auto-summaries follow the switch.
	if a.compaction != nil {
		_ = a.registerCompaction()
	}
	a.providerMu.RLock()
	acceptedCfg := a.cfg.Clone()
	a.providerMu.RUnlock()
	a.persistDefaultModelSelection(ctx, acceptedCfg.LLM.Provider, llmProviderModel(acceptedCfg, acceptedCfg.LLM.Provider), acceptedCfg.ReasoningEffort)
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
		mode := "one-shot"
		if c.Continuable {
			mode = "continuable"
		}
		out = append(out, map[string]any{"id": c.ID, "label": c.Label, "running": c.Running, "mode": mode, "has_children": false})
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
