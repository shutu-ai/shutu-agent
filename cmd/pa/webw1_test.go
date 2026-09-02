// webw1_test.go — the M10 W1 composition-root tests (docs/dispatch-m10-web2.md
// §5): runTurn serialization (D5), webMessage dispatch with implicit resume,
// the eventHub publish/subscribe semantics (including the drop-slow-subscriber
// policy), and the registerWebServer injection assertion. The webserver-side
// API tests live in internal/webserver.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

// turnLLM is a scripted llm.LLM for the W1 turn tests: it records how many
// Stream calls are in flight at once (maxActive) so a test can assert that
// turnMu serializes, and returns a fixed single-step answer.
type turnLLM struct {
	mu        sync.Mutex
	calls     int
	active    int
	maxActive int
}

func (l *turnLLM) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	l.mu.Lock()
	l.calls++
	l.active++
	if l.active > l.maxActive {
		l.maxActive = l.active
	}
	l.mu.Unlock()
	time.Sleep(30 * time.Millisecond) // widen the overlap window so a race is visible
	l.mu.Lock()
	l.active--
	l.mu.Unlock()
	return &turnReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: "hello"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

type turnReader struct {
	events []llm.StreamEvent
	i      int
}

func (r *turnReader) Next() (llm.StreamEvent, error) {
	if r.i >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := r.events[r.i]
	r.i++
	return ev, nil
}

// disconnectLLM holds the model request open so the web turn test can cancel
// the originating HTTP context while the agent is still running.
type disconnectLLM struct {
	started chan context.Context
	release chan struct{}
}

func (l *disconnectLLM) Stream(ctx context.Context, _ llm.ChatRequest) (llm.StreamReader, error) {
	l.started <- ctx
	select {
	case <-l.release:
		return &turnReader{events: []llm.StreamEvent{
			{Kind: llm.StreamTextDelta, Text: "completed"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// makeTurnApp builds a minimal app able to run a real loop turn: only the
// fields newLoop touches (cfg.Model, llm, log, reg, prompt) are set; all the
// optional seams stay nil so preStepInjectors contributes nothing.
func makeTurnApp() *app {
	return &app{
		cfg:    config.Config{Model: "m"},
		llm:    &turnLLM{},
		reg:    tools.New(),
		prompt: prompt.New("You are a test agent."),
		log:    session.New(),
	}
}

// TestRunTurnSerial verifies D5 (M10 W1): concurrent runTurn calls share the
// global turnMu, so at most one loop Run (one LLM Stream) is in flight at any
// moment and every message still produces its own turn's events.
func TestRunTurnSerial(t *testing.T) {
	llm := &turnLLM{}
	a := makeTurnApp()
	a.llm = llm
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := a.runTurn(context.Background(), fmt.Sprintf("msg-%d", n), false); err != nil {
				t.Errorf("runTurn: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if llm.maxActive != 1 {
		t.Fatalf("max concurrent LLM streams = %d, want 1 (turnMu must serialize)", llm.maxActive)
	}
	if llm.calls != 5 {
		t.Fatalf("LLM calls = %d, want 5", llm.calls)
	}
	if n := len(a.log.Events()); n != 40 { // one durable runtime snapshot + 5 canonical turns
		t.Fatalf("log events = %d, want 40", n)
	}
}

// TestWebMessageRunsTurn verifies webMessage dispatches a turn on the current
// session: the log gains user/message, assistant/chunk and assistant/message.
func TestWebMessageRunsTurn(t *testing.T) {
	a := makeTurnApp()
	a.currentID = "s-a"
	if err := a.webMessage(context.Background(), "s-a", "hi", nil); err != nil {
		t.Fatalf("webMessage: %v", err)
	}
	for _, typ := range []string{session.EventUserMessage, session.EventAssistantChunk, session.EventAssistantMessage} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log missing %s after webMessage", typ)
		}
	}
}

// TestWebMessageSurvivesRequestDisconnect verifies the dsh lifecycle boundary:
// losing the browser/request connection does not cancel an active agent turn;
// only the explicit stop endpoint cancels its turn context.
func TestWebMessageSurvivesRequestDisconnect(t *testing.T) {
	model := &disconnectLLM{started: make(chan context.Context, 1), release: make(chan struct{})}
	a := makeTurnApp()
	a.llm = model
	a.baseCtx = context.Background()
	a.currentID = "s-a"

	requestCtx, disconnect := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.webMessage(requestCtx, "s-a", "continue", nil) }()

	var turnCtx context.Context
	select {
	case turnCtx = <-model.started:
	case <-time.After(time.Second):
		t.Fatal("web turn did not reach the model")
	}
	disconnect()
	select {
	case <-turnCtx.Done():
		t.Fatalf("turn context cancelled when request disconnected: %v", turnCtx.Err())
	case <-time.After(50 * time.Millisecond):
	}
	close(model.release)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("webMessage after request disconnect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("web turn did not settle")
	}
}

// TestWebMessageResumesOtherSession verifies webMessage implicitly resumes a
// target session that differs from the current one (D-WEB2-A): the turn runs on
// the resumed session (its store gains the events). The previous session is
// BLANK (no events) here, so dsh discards it when the user switches away — the
// abandoned empty row is pruned from the store (see the separate
// TestPruneBlankSession tests for a blank held on the hero).
func TestWebMessageResumesOtherSession(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"s-a", "s-other"} {
		if err := st.CreateSession(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	a := makeTurnApp()
	a.store = st
	a.currentID = "s-a"
	// Keep a.log consistent with currentID so the blank check (len(log)==0)
	// reflects the session the user is leaving — the app invariant is that
	// a.log always corresponds to a.currentID.
	a.log = session.New()
	if err := a.webMessage(ctx, "s-other", "hi", nil); err != nil {
		t.Fatalf("webMessage: %v", err)
	}
	if a.currentID != "s-other" {
		t.Fatalf("currentID = %q, want s-other (resumed)", a.currentID)
	}
	events, err := st.LoadSession(ctx, "s-other")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("the resumed session must hold the turn's events")
	}
	// The blank source session was discarded on switch (dsh).
	if _, err := st.LoadSession(ctx, "s-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("s-a err = %v, want ErrNotFound (blank session discarded on switch)", err)
	}
}

func TestWebSessionManagerAgentPathDoesNotSwitchREPLSelection(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	legacyLog := session.New()
	a := &app{
		store:         st,
		currentID:     "repl-session",
		log:           legacyLog,
		agentRegistry: agent.NewRegistry(),
		baseCtx:       context.Background(),
		cfg:           config.Config{Workspace: config.WorkspaceConfig{DefaultDir: t.TempDir()}},
	}
	defer a.agentRegistry.CloseAll()

	id, err := a.webSessionManager(context.Background(), "new", "")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || a.currentID != "repl-session" || a.log != legacyLog {
		t.Fatalf("agent web new changed REPL selection: id=%q current=%q logChanged=%t", id, a.currentID, a.log != legacyLog)
	}
	if _, err := a.webSessionManager(context.Background(), "resume", id); err != nil {
		t.Fatal(err)
	}
	if a.currentID != "repl-session" || a.log != legacyLog {
		t.Fatal("agent web resume changed REPL selection")
	}
}

// TestPruneBlankSessionOnSwitch verifies dsh's empty-session behavior: a session
// with no events that the user leaves by switching to another session is
// discarded from the store, while a session that has content is preserved.
func TestPruneBlankSessionOnSwitch(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"blank", "target", "real"} {
		if err := st.CreateSession(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	a := makeTurnApp()
	a.store = st

	// Leave a blank session (no events) → it is discarded.
	a.currentID = "blank"
	a.log = session.New()
	if err := a.resumeSession(ctx, "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadSession(ctx, "blank"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blank err = %v, want ErrNotFound (blank discarded on switch)", err)
	}

	// Leave a session with content → it is preserved.
	a.currentID = "real"
	a.log = session.New()
	if _, err := a.log.Append(session.EventUserMessage, session.NewUserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	if err := a.resumeSession(ctx, "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadSession(ctx, "real"); err != nil {
		t.Fatalf("real should be preserved: %v", err)
	}
}

// TestPruneBlankSessionOnNew verifies dsh's empty-session behavior when starting
// a fresh session: the abandoned blank current session is discarded.
func TestPruneBlankSessionOnNew(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "old-blank", time.Now()); err != nil {
		t.Fatal(err)
	}
	a := makeTurnApp()
	a.store = st
	a.currentID = "old-blank"
	a.log = session.New()
	if err := a.newSession(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadSession(ctx, "old-blank"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old-blank err = %v, want ErrNotFound (blank discarded on new)", err)
	}
}

// TestEventHubPublishSubscribe verifies the hub delivers an event to the
// subscribers of the session only, and that unsubscribe closes the channel.
func TestEventHubPublishSubscribe(t *testing.T) {
	h := NewEventHub()
	ch, unsub := h.Subscribe("s-1")
	h.Publish("s-1", session.Event{Seq: 1, Type: session.EventUserMessage})
	select {
	case got := <-ch:
		if got.Seq != 1 {
			t.Fatalf("got seq %d, want 1", got.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the published event")
	}
	// A different session id must not leak into this subscriber.
	select {
	case got := <-ch:
		t.Fatalf("received an event for the wrong session: %+v", got)
	default:
	}
	unsub()
	if _, ok := <-ch; ok {
		t.Fatal("channel must be closed after unsubscribe")
	}
}

func TestEventHubSubscribeAllPreservesSessionIdentity(t *testing.T) {
	h := NewEventHub()
	ch, unsub := h.SubscribeAll()
	defer unsub()
	h.Publish("s-2", session.Event{Seq: 4, Type: session.EventTurnEnd})
	select {
	case got := <-ch:
		if got.sessionID != "s-2" || got.event.Seq != 4 {
			t.Fatalf("all-subscriber delivery = %+v, want session identity and event", got)
		}
	case <-time.After(time.Second):
		t.Fatal("all-subscriber did not receive the published event")
	}
}

// TestEventHubDropsSlowSubscriber verifies the drop policy (dispatch-m10-web2
// §2): publishing to a subscriber whose buffer is full never blocks the caller.
func TestEventHubDropsSlowSubscriber(t *testing.T) {
	h := NewEventHub()
	_, unsub := h.Subscribe("s-1")
	defer unsub()
	for i := 0; i < eventHubBuffer; i++ {
		h.Publish("s-1", session.Event{Seq: uint64(i)})
	}
	start := time.Now()
	h.Publish("s-1", session.Event{Seq: 999})
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("publish to a full-buffer subscriber must not block")
	}
}

// TestRegisterWebServerInjectsHandlers verifies registerWebServer injects the
// message handler, session manager and event source into the webserver
// (dispatch-m10-web2 §2/§5): all three Handlers() fields must be non-nil.
func TestRegisterWebServerInjectsHandlers(t *testing.T) {
	a, st := makeWebServerApp(t, true, "tok")
	defer st.Close()
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	defer a.webserver.Close()
	h := a.webserver.Handlers()
	if h.Message == nil || h.Session == nil || h.Event == nil {
		t.Fatalf("injected handlers = %+v, want all three non-nil", h)
	}
}

// TestWebConfigRedacts verifies webConfig (M10 W2, ADR D-WEB2-D): the sanitized
// view never leaks the web_server token plaintext, the model/provider/mode and
// the capability gates are correct, the tool whitelist carries its count plus a
// bounded list, and registerWebServer wires the provider into the webserver
// (Handlers().Config non-nil).
func TestWebConfigProjectsCanonicalToolCatalog(t *testing.T) {
	a := &app{reg: tools.New()}
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	a.reg.SetPolicy(tools.Policy{Profile: config.ModeStandard, Enabled: []string{"get_time"}})

	names, manifest, err := a.webToolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.ValidateCatalogManifest(manifest); err != nil {
		t.Fatalf("web catalog manifest invalid: %v", err)
	}
	if len(names) != 1 || names[0] != "get_time" || manifest.Revision == 0 {
		t.Fatalf("web catalog = %v/%+v", names, manifest)
	}

	configView := a.webConfig()
	projected, ok := configView["tool_catalog"].(tools.CatalogManifest)
	if !ok {
		t.Fatalf("tool_catalog type = %T, want tools.CatalogManifest", configView["tool_catalog"])
	}
	if projected.Digest != manifest.Digest || projected.Revision != manifest.Revision || len(projected.Tools) != 1 {
		t.Fatalf("projected catalog = %+v, want manifest %+v", projected, manifest)
	}
	if got := configView["tools_enabled_count"]; got != 1 {
		t.Fatalf("tools_enabled_count = %v, want 1", got)
	}

	// A generation replacement must be observable by the next inventory read.
	if err := a.reg.Unregister("get_time"); err != nil {
		t.Fatal(err)
	}
	if err := a.reg.RegisterWithInfo(tools.GetTime{}, tools.RegistrationInfo{Owner: "reload", Plugin: "demo", Generation: manifest.Revision + 1}); err != nil {
		t.Fatal(err)
	}
	_, reloaded, err := a.webToolCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision <= manifest.Revision || reloaded.Digest == manifest.Digest {
		t.Fatalf("reload manifest = %+v, want advanced revision and digest", reloaded)
	}
}

func TestWebConfigRedacts(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<div id=\"react-root\">DSH</div>"), 0o644); err != nil {
		t.Fatalf("write frontend index: %v", err)
	}
	a := &app{
		cfg: config.Config{
			Model:   "deepseek-chat",
			BaseURL: "https://api.example.com/v1",
			Mode:    "standard",
			LLM: config.LLMConfig{
				Provider:   "openai",
				OpenAI:     config.OpenAIProviderConfig{Model: "gpt-4o"},
				Multimodal: config.MultimodalConfig{Enabled: boolPtr(true)},
			},
			Tools:      config.ToolsConfig{Enabled: []string{"get_time", "read", "web_search"}},
			Web:        config.WebConfig{Enabled: config.Bool(true)},
			Compaction: config.CompactionConfig{Enabled: config.Bool(true)},
			Eval:       config.EvalConfig{Enabled: config.Bool(false)},
			WebServer:  config.WebServerConfig{Enabled: true, Addr: "127.0.0.1:0", Token: "super-secret", DistDir: dist},
		},
		store: st,
	}
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	defer a.webserver.Close()

	v := a.webConfig()

	// Redaction: the token key is absent (webConfig omits it) or masked; the
	// plaintext never appears anywhere in the serialized view.
	if tok, ok := v["web_server.token"]; ok && tok != "***" {
		t.Fatalf("web_server.token = %v, want absent or \"***\"", tok)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "super-secret") {
		t.Fatal("webConfig leaks the token plaintext")
	}

	// Model / provider / mode / base_url. "model" is the active provider's
	// model (P5.1: llmProviderModel — openai's own field here).
	if v["model"] != "gpt-4o" || v["llm_provider"] != "openai" || v["mode"] != "standard" {
		t.Fatalf("model/provider/mode = %v/%v/%v, want gpt-4o/openai/standard",
			v["model"], v["llm_provider"], v["mode"])
	}
	if v["base_url"] != "https://api.example.com/v1" {
		t.Fatalf("base_url = %v, want https://api.example.com/v1", v["base_url"])
	}

	// Capability gates.
	for key, want := range map[string]bool{
		"web_enabled": true, "compaction_enabled": true,
		"multimodal_enabled": true, "eval_enabled": false, "jobs_enabled": true,
	} {
		if got, _ := v[key].(bool); got != want {
			t.Fatalf("%s = %v, want %v", key, v[key], want)
		}
	}

	// Web server address.
	if v["web_server_addr"] != "127.0.0.1:0" {
		t.Fatalf("web_server_addr = %v, want 127.0.0.1:0", v["web_server_addr"])
	}

	// The config provider is wired into the webserver.
	h := a.webserver.Handlers()
	if h.Config == nil {
		t.Fatal("registerWebServer must wire SetConfigProvider(a.webConfig)")
	}
	if got := h.Config(); got["model"] != "gpt-4o" {
		t.Fatalf("wired cfgFn returned model %v, want gpt-4o (active provider model)", got["model"])
	}
}

// TestWebSessionNewThenMessageAfterRequestCtxCancelled is the M10 W3 regression
// for the real-smoke catch: the persist sink must survive the request ctx that
// created/resumed the session. webSessionManager/webMessage run with an HTTP
// request ctx (r.Context()) that is cancelled the instant its handler returns;
// if the sink captured that ctx, every later append failed with "store: begin
// append: context canceled" and the web message returned 500. The sink persists
// under the process-level baseCtx (Background in tests), so a later message
// still lands.
func TestWebSessionNewThenMessageAfterRequestCtxCancelled(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := makeTurnApp()
	a.store = st
	a.baseCtx = context.Background() // the process-lifetime ctx (main sets it)
	a.currentID = ""

	// Simulate POST /api/sessions: a request-scoped ctx that is cancelled when
	// the handler returns (r.Context() semantics).
	reqCtx, cancel := context.WithCancel(context.Background())
	sid, err := a.webSessionManager(reqCtx, "new", "")
	cancel() // handler returned -> r.Context() is now cancelled
	if err != nil {
		t.Fatalf("webSessionManager new: %v", err)
	}

	// A later, unrelated request's message must still persist (the sink must
	// NOT be bound to the cancelled request ctx).
	if err := a.webMessage(context.Background(), sid, "hello", nil); err != nil {
		t.Fatalf("webMessage after request ctx cancelled: %v (persist sink must use baseCtx)", err)
	}

	evs, err := st.LoadSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{session.EventUserMessage, session.EventAssistantChunk, session.EventAssistantMessage} {
		found := false
		for _, ev := range evs {
			if ev.Type == typ {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("store missing %s after message (persist failed under cancelled ctx)", typ)
		}
	}
}

// TestWebSwitchModel covers the P5.1 live model switch (webSwitchModel): a
// model-only change rebuilds the provider on the same selection; a provider
// change swaps a.llm and updates the per-provider model; a reasoning-effort
// change applies live; an unknown provider or effort is rejected fail-closed
// (previous selection untouched).
func TestWebSwitchModel(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	mm := true
	a := &app{cfg: config.Config{
		Model: "deepseek-chat",
		LLM: config.LLMConfig{
			Provider:   "deepseek-official",
			OpenAI:     config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
			Multimodal: config.MultimodalConfig{Enabled: &mm, MaxImageBytes: 1 << 20},
		},
	}}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}

	// Model-only switch on deepseek.
	if err := a.webSwitchModel(context.Background(), "", "deepseek-reasoner", ""); err != nil {
		t.Fatalf("model switch: %v", err)
	}
	if a.cfg.Model != "deepseek-reasoner" {
		t.Fatalf("cfg.Model = %q, want deepseek-reasoner", a.cfg.Model)
	}

	// Reasoning-effort switch (dsh 思考强度) applies live; unknown effort fails
	// closed and leaves the previous selection.
	if err := a.webSwitchModel(context.Background(), "", "", "high"); err != nil {
		t.Fatalf("effort switch: %v", err)
	}
	if a.cfg.ReasoningEffort != "high" {
		t.Fatalf("cfg.ReasoningEffort = %q, want high", a.cfg.ReasoningEffort)
	}
	if err := a.webSwitchModel(context.Background(), "", "", "bogus"); err == nil {
		t.Fatal("unknown effort must error")
	}
	if a.cfg.ReasoningEffort != "high" {
		t.Fatalf("effort after failed switch = %q, want high (unchanged)", a.cfg.ReasoningEffort)
	}

	// Provider switch to openai, with its own model.
	if err := a.webSwitchModel(context.Background(), "openai", "gpt-4o-mini", ""); err != nil {
		t.Fatalf("provider switch: %v", err)
	}
	if a.cfg.LLM.Provider != "openai" || a.cfg.LLM.OpenAI.Model != "gpt-4o-mini" {
		t.Fatalf("cfg = provider %q openai.model %q, want openai/gpt-4o-mini", a.cfg.LLM.Provider, a.cfg.LLM.OpenAI.Model)
	}
	if a.currentLLM() == nil {
		t.Fatal("currentLLM must be non-nil after a successful switch")
	}

	// Unknown provider → fail-closed, selection untouched.
	if err := a.webSwitchModel(context.Background(), "nope", "", ""); err == nil {
		t.Fatal("unknown provider must error")
	}
	if a.cfg.LLM.Provider != "openai" {
		t.Fatalf("provider after failed switch = %q, want openai (unchanged)", a.cfg.LLM.Provider)
	}
}
