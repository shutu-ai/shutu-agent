package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/acp"
	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/compaction"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/mcp"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/subagent"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

type acpPermissionAnswer struct {
	outcome acp.PermissionOutcome
	err     error
}

type acpPermissionCancelAnswer struct{}

func (acpPermissionCancelAnswer) RequestPermission(ctx context.Context, _ acp.PermissionRequest) (acp.PermissionOutcome, error) {
	<-ctx.Done()
	return acp.PermissionOutcome{}, ctx.Err()
}

func (a acpPermissionAnswer) RequestPermission(context.Context, acp.PermissionRequest) (acp.PermissionOutcome, error) {
	return a.outcome, a.err
}

func TestACPPermissionGateUsesSharedApprovalServiceContract(t *testing.T) {
	log := session.New()
	approval := interact.NewEngine(nil)
	defer approval.Close()
	s := &acpSession{id: "acp-approval", log: log, approval: approval}
	s.SetPermissionRequester(acpPermissionAnswer{outcome: acp.PermissionOutcome{OptionID: "allow-once"}})
	decision, err := s.acpPermissionGate([]string{"danger"})(context.Background(), tools.Execution{CallID: "call-1", Name: "danger", Arguments: map[string]any{"x": 1}})
	if err != nil || decision.Kind != "allow" {
		t.Fatalf("allowed gate = %+v, err=%v", decision, err)
	}
	events := log.Events()
	if len(events) != 2 || events[0].Type != session.EventApprovalAsked || events[1].Type != session.EventApprovalDecided {
		t.Fatalf("approval events = %#v", events)
	}
	var asked struct{ ID, CallID, ToolName string }
	var decided struct{ ID, CallID, Outcome string }
	if err := json.Unmarshal(events[0].Data, &asked); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1].Data, &decided); err != nil {
		t.Fatal(err)
	}
	if asked.ID != "req-1" || asked.CallID != "call-1" || asked.ToolName != "danger" || decided.ID != asked.ID || decided.CallID != asked.CallID || decided.Outcome != string(interact.StatusAllowedOnce) {
		t.Fatalf("approval correlation = asked=%+v decided=%+v", asked, decided)
	}

	denied := &acpSession{id: "acp-denied", log: session.New(), approval: interact.NewEngine(nil)}
	defer denied.approval.Close()
	denied.SetPermissionRequester(acpPermissionAnswer{outcome: acp.PermissionOutcome{OptionID: "reject-once"}})
	decision, err = denied.acpPermissionGate([]string{"danger"})(context.Background(), tools.Execution{CallID: "call-2", Name: "danger", Arguments: map[string]any{}})
	if err != nil || decision.Kind != "deny" {
		t.Fatalf("denied gate = %+v, err=%v", decision, err)
	}
	var rejected struct{ Outcome string }
	if err := json.Unmarshal(denied.log.Events()[1].Data, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Outcome != string(interact.StatusRejected) {
		t.Fatalf("rejected outcome = %q", rejected.Outcome)
	}
}

func TestACPPermissionGateTreatsRogueAndCancelledAnswerersAsUnavailableOrCancelled(t *testing.T) {
	rogueLog := session.New()
	rogueApproval := interact.NewEngine(nil)
	defer rogueApproval.Close()
	rogue := &acpSession{id: "acp-rogue", log: rogueLog, approval: rogueApproval}
	rogue.SetPermissionRequester(acpPermissionAnswer{outcome: acp.PermissionOutcome{Outcome: "grant-forever"}})
	decision, err := rogue.acpPermissionGate([]string{"danger"})(context.Background(), tools.Execution{CallID: "rogue-call", Name: "danger"})
	if err != nil || decision.Kind != "deny" {
		t.Fatalf("rogue answerer decision = %+v, err=%v", decision, err)
	}
	var rogueDecision struct{ Outcome string }
	if err := json.Unmarshal(rogueLog.Events()[1].Data, &rogueDecision); err != nil {
		t.Fatal(err)
	}
	if rogueDecision.Outcome != string(interact.StatusUnavailable) {
		t.Fatalf("rogue answerer outcome = %q, want unavailable", rogueDecision.Outcome)
	}

	cancelLog := session.New()
	cancelApproval := interact.NewEngine(nil)
	defer cancelApproval.Close()
	cancelled := &acpSession{id: "acp-cancelled", log: cancelLog, approval: cancelApproval}
	cancelled.SetPermissionRequester(acpPermissionCancelAnswer{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var got tools.PreToolDecision
	var gotErr error
	go func() {
		got, gotErr = cancelled.acpPermissionGate([]string{"danger"})(ctx, tools.Execution{CallID: "cancel-call", Name: "danger"})
		close(done)
	}()
	deadline := time.After(time.Second)
	for len(cancelLog.Events()) < 1 {
		select {
		case <-deadline:
			t.Fatal("cancelled approval was not created")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled permission gate did not settle")
	}
	if gotErr != nil || got.Kind != "deny" {
		t.Fatalf("cancelled answerer decision = %+v, err=%v", got, gotErr)
	}
	var cancelledDecision struct{ Outcome string }
	if err := json.Unmarshal(cancelLog.Events()[1].Data, &cancelledDecision); err != nil {
		t.Fatal(err)
	}
	if cancelledDecision.Outcome != string(interact.StatusCanceled) {
		t.Fatalf("cancelled answerer outcome = %q, want cancelled", cancelledDecision.Outcome)
	}
}

type acpSummaryLLM struct{}

func (acpSummaryLLM) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return &acpSummaryReader{}, nil
}

type acpSummaryReader struct{ done bool }

func (r *acpSummaryReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	r.done = true
	return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: "summary"}, nil
}

type acpImageLLM struct{ acpSummaryLLM }

func (acpImageLLM) ID() string           { return "acp-image-gw" }
func (acpImageLLM) Available() bool      { return true }
func (acpImageLLM) SupportsImages() bool { return true }

func TestACPTextOnlyPromptDoesNotRequireImageCapability(t *testing.T) {
	model := &acpSummaryLLM{}
	a := &app{
		cfg: config.Config{Model: "text-model"},
		llm: model,
	}
	s := &acpSession{
		app:      a,
		id:       "acp-text-only",
		log:      session.New(),
		registry: tools.New(),
		prompt:   prompt.New("You are helpful."),
		model:    "text-model",
	}
	if _, err := s.PromptContent(context.Background(), []acp.PromptContentBlock{{Type: "text", Text: "plain text"}}, nil); err != nil {
		t.Fatalf("text-only PromptContent: %v", err)
	}
}

func TestACPContentCancellationDuringAdmissionDoesNotQueuePrompt(t *testing.T) {
	s := &acpSession{log: session.New()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stop, err := s.PromptContent(ctx, []acp.PromptContentBlock{{Type: "text", Text: "must not queue"}}, nil)
	if err != nil || stop != acp.StopCancelled {
		t.Fatalf("cancelled admission = stop=%q err=%v, want cancelled without error", stop, err)
	}
	if len(s.log.Events()) != 0 {
		t.Fatalf("cancelled admission wrote %d events", len(s.log.Events()))
	}
}

func TestACPImageAdmissionValidatesTheWholeBatchBeforeWriting(t *testing.T) {
	root := t.TempDir()
	attachments, err := attachment.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	a := &app{
		cfg: config.Config{Model: "test-model", LLM: config.LLMConfig{
			Provider:             "",
			ModelInputModalities: "text,image",
			Multimodal:           config.MultimodalConfig{Enabled: &enabled, MaxImageBytes: 1024},
		}},
		llm:         acpImageLLM{},
		attachStore: attachments,
		llmReg:      llm.NewRegistry(),
	}
	if err := a.llmReg.Register(acpImageLLM{}); err != nil {
		t.Fatal(err)
	}
	a.customProviders = []customProviderProfile{{
		ID: "acp-image-gw", Model: "image-model",
		Models: []customModel{{ID: "image-model", Vision: catalogBool(true)}},
	}}
	s := &acpSession{
		app: a, id: "acp-image-batch", log: session.New(),
		provider: "acp-image-gw", model: "image-model",
	}
	_, err = s.PromptContent(context.Background(), []acp.PromptContentBlock{
		{Type: "image", Data: "AQ==", MimeType: "image/png"},
		{Type: "image", Data: "not base64", MimeType: "image/png"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("malformed image batch err = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("malformed batch wrote %d attachment(s), want none", len(entries))
	}
}

func TestACPCommittedRichOutputPreservesOrderAndPreflightsAttachments(t *testing.T) {
	root := t.TempDir()
	attachments, err := attachment.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := attachments.SaveImage("image/png", attachTestPNG, 1024)
	if err != nil {
		t.Fatal(err)
	}
	log := session.New()
	_, err = log.Append(session.EventAssistantMessage, map[string]any{
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "before"},
			map[string]any{"type": "image", "attachment": map[string]any{
				"attachmentId": ref.ID, "mediaType": ref.MediaType, "bytes": ref.Bytes,
			}},
			map[string]any{"type": "text", "text": "after"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &acpSession{app: &app{attachStore: attachments}, log: log}
	var updates []acp.Update
	if err := s.emitCommittedAssistantContent(0, func(update acp.Update) { updates = append(updates, update) }); err != nil {
		t.Fatalf("rich output delivery: %v", err)
	}
	if len(updates) != 3 || updates[0].Content == nil || updates[0].Content.Text != "before" ||
		updates[1].Content == nil || updates[1].Content.Type != "image" || updates[1].Content.Data != base64.StdEncoding.EncodeToString(attachTestPNG) ||
		updates[2].Content == nil || updates[2].Content.Text != "after" {
		t.Fatalf("rich output updates = %#v", updates)
	}

	missingLog := session.New()
	_, err = missingLog.Append(session.EventAssistantMessage, map[string]any{
		"message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "must not leak"},
			map[string]any{"type": "image", "attachment": map[string]any{"attachmentId": "missing"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.log = missingLog
	updates = nil
	if err := s.emitCommittedAssistantContent(0, func(update acp.Update) { updates = append(updates, update) }); err == nil || len(updates) != 0 {
		t.Fatalf("missing image delivery err=%v updates=%#v, want atomic failure", err, updates)
	}
}

type acpDynamicExtensionTool struct{}

func (acpDynamicExtensionTool) Name() string        { return "plugin_load" }
func (acpDynamicExtensionTool) Description() string { return "test-only dynamic extension" }
func (acpDynamicExtensionTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (acpDynamicExtensionTool) OutputSchema() map[string]any { return map[string]any{"type": "string"} }
func (acpDynamicExtensionTool) Execute(context.Context, any) (string, error) {
	return "unexpected", nil
}

func TestACPFactoryCreatesIndependentCWDAndLogs(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "marker.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "marker.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "pa.db")
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &app{
		cfg:        config.Config{Mode: config.ModeMinimal},
		store:      st,
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	factory := &acpFactory{app: a}
	firstSession, err := factory.NewSession(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := factory.NewSession(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	one := firstSession.(*acpSession)
	two := secondSession.(*acpSession)
	if one.terminal != nil || two.terminal != nil {
		t.Fatal("ACP terminal must remain disabled without explicit terminal.acp_enabled")
	}
	if one.id == two.id || one.log == two.log || one.registry == two.registry {
		t.Fatal("ACP sessions must not share identity, log, or registry")
	}
	got, err := one.registry.Execute(context.Background(), "str_replace_editor", []byte(`{"command":"view","path":"`+filepath.ToSlash(filepath.Join(first, "marker.txt"))+`"}`))
	if err != nil || !strings.Contains(got.Output, "     1  one") {
		t.Fatalf("first cwd read = %q, err=%v, result=%+v", got.Output, err, got)
	}
	got, err = two.registry.Execute(context.Background(), "str_replace_editor", []byte(`{"command":"view","path":"`+filepath.ToSlash(filepath.Join(second, "marker.txt"))+`"}`))
	if err != nil || !strings.Contains(got.Output, "     1  two") {
		t.Fatalf("second cwd read = %q, err=%v", got.Output, err)
	}
	metas, err := st.ListSessions(context.Background())
	if err != nil || len(metas) != 2 {
		t.Fatalf("session metadata count = %d, err=%v", len(metas), err)
	}
	seen := map[string]bool{}
	for _, meta := range metas {
		seen[meta.CWD] = true
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("session cwds = %#v", seen)
	}
}

func TestACPFactoryResumeRestoresDurableIdentityCWDHistoryAndCursor(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(filepath.Join(root, "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app{
		cfg:        config.Config{Mode: config.ModeMinimal},
		store:      st,
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	factory := &acpFactory{app: a}
	created, err := factory.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	createdSession := created.(*acpSession)
	if _, err := createdSession.log.Append(session.EventUserMessage, session.NewUserMessage("durable ACP prompt")); err != nil {
		t.Fatal(err)
	}
	id := createdSession.SessionID()
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := factory.ResumeSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumedSession := resumed.(*acpSession)
	if resumedSession.SessionID() != id || resumedSession.cwd != cwd {
		t.Fatalf("resumed identity/cwd = (%q, %q), want (%q, %q)", resumedSession.SessionID(), resumedSession.cwd, id, cwd)
	}
	events := resumedSession.log.Events()
	if len(events) != 1 || events[0].Type != session.EventUserMessage {
		t.Fatalf("resumed durable events = %#v", events)
	}
	metadata := resumedSession.ResumeMetadata()
	if metadata["durable"] != true || metadata["cwd"] != cwd || metadata["eventCursor"] != uint64(events[0].Seq) {
		t.Fatalf("resume metadata = %#v", metadata)
	}
}

func TestACPFactoryResumeRejectsProjectionInvalidDurableEvent(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(filepath.Join(root, "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app{
		cfg:        config.Config{Mode: config.ModeMinimal},
		store:      st,
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	factory := &acpFactory{app: a}
	created, err := factory.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	createdSession := created.(*acpSession)
	if _, err := createdSession.log.Append(session.EventPermissionPreset, session.NewPermissionPreset("read-only")); err != nil {
		t.Fatal(err)
	}
	// The session log only requires a key; the shared projection is the
	// stricter control-state authority and must fail reconnect on an invalid
	// downstream fact rather than resuming with an invented cursor.
	if _, err := createdSession.log.Append(session.EventSandboxMode, map[string]any{}); err != nil {
		t.Fatalf("append projection-invalid sandbox event: %v", err)
	}
	id := createdSession.SessionID()
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.ResumeSession(context.Background(), id)
	if err == nil {
		_ = resumed.Close()
		t.Fatal("resume accepted a projection-invalid durable session")
	}
	if !strings.Contains(err.Error(), "missing mode") {
		t.Fatalf("resume error = %v, want shared projection missing-mode failure", err)
	}
}

func TestACPFactorySharesAppApprovalServiceBySession(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "pa.db")
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	approval := interact.NewEngine(nil)
	defer approval.Close()
	a := &app{
		cfg:        config.Config{Mode: config.ModeMinimal},
		store:      st,
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
		interacts:  approval,
	}
	factory := &acpFactory{app: a}
	sessionValue, err := factory.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	s := sessionValue.(*acpSession)
	if s.approval != approval || !s.sharedApproval {
		t.Fatalf("ACP approval service = %p shared=%v, want app service/shared", s.approval, s.sharedApproval)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := approval.RequestForSession(context.Background(), s.id, "still open", "danger", "{}"); err != nil {
		t.Fatalf("closing ACP session closed shared approval service: %v", err)
	}
}

func TestACPFactoryResumeExposesMetadataAndUnknownIDClassification(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "pa.db")
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: config.Config{
			Model: "test-model",
			Mode:  config.ModeMinimal,
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
		store:      st,
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	factory := &acpFactory{app: a}
	first, err := factory.NewSession(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	live := first.(*acpSession)
	if _, err := live.log.Append(session.EventUserMessage, session.NewUserMessage("persisted")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	resumedValue, err := factory.ResumeSession(context.Background(), live.id)
	if err != nil {
		t.Fatal(err)
	}
	resumed := resumedValue.(*acpSession)
	metadata := resumed.ResumeMetadata()
	if metadata["durable"] != true || metadata["provider"] != "deepseek-official" ||
		metadata["model"] != "test-model" || metadata["mode"] != config.ModeMinimal ||
		metadata["eventCursor"] != uint64(1) {
		t.Fatalf("resume metadata = %#v", metadata)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = factory.ResumeSession(context.Background(), "does-not-exist")
	if !errors.Is(err, acp.ErrSessionNotFound) {
		t.Fatalf("unknown resume error = %v, want acp.ErrSessionNotFound", err)
	}
}

func TestACPEmitsOnlyCommittedAssistantMessage(t *testing.T) {
	log := session.New()
	s := &acpSession{log: log}
	var updates []string
	stop, err := s.runPrompt(context.Background(), func(update acp.Update) {
		updates = append(updates, update.Text)
	}, func(context.Context) error {
		if _, err := log.Append(session.EventAssistantChunk, map[string]any{"text": "partial"}); err != nil {
			return err
		}
		_, err := log.Append(session.EventAssistantMessage, session.NewAssistantMessage("committed", nil, "stop"))
		return err
	})
	if err != nil || string(stop) != "end_turn" {
		t.Fatalf("runPrompt stop=%q err=%v", stop, err)
	}
	if len(updates) != 1 || updates[0] != "committed" {
		t.Fatalf("updates = %#v, want only committed assistant text", updates)
	}
}

func TestACPRegistryDoesNotInheritRuntimeExtensionsOrProfiles(t *testing.T) {
	a := &app{
		cfg:        config.Config{Mode: config.ModeStandard},
		basePolicy: tools.DefaultPolicy(),
		reg:        tools.New(),
		customProviders: []customProviderProfile{{
			ID: "runtime-profile", Name: "runtime profile",
		}},
		builtinProfiles: map[string]builtinProviderProfile{
			"deepseek-official": {BaseURL: "https://profile.example.invalid", Model: "profile-model"},
		},
	}
	if err := a.reg.Register(acpDynamicExtensionTool{}); err != nil {
		t.Fatal(err)
	}
	registry, err := acpRegistry(a, "acp-test", t.TempDir(), session.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range registry.Specs() {
		if spec.Name == "plugin_load" || spec.Name == "runtime-profile" || spec.Name == "deepseek-official" {
			t.Fatalf("ACP registry inherited global runtime extension/profile %q", spec.Name)
		}
	}
}

func TestACPExplicitTerminalOwnsLifecycleAndTools(t *testing.T) {
	root := t.TempDir()
	log := session.New()
	a := &app{
		cfg: config.Config{
			Terminal: config.TerminalConfig{
				Enabled:    config.Bool(true),
				ACPEnabled: config.Bool(true),
			},
		},
		basePolicy: tools.DefaultPolicy(),
	}
	service := newACPCTerminal(a.cfg.Terminal, "acp-test", root, log)
	registry, err := acpRegistry(a, "acp-test", root, log, service, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), acpTerminalStart, []byte(`{}`)); err != nil {
		t.Fatalf("terminal_start: %v", err)
	}
	if _, err := registry.Execute(context.Background(), acpTerminalStop, []byte(`{}`)); err != nil {
		t.Fatalf("terminal_stop: %v", err)
	}
	types := map[string]int{}
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	if types[session.EventTerminalStart] != 1 || types[session.EventTerminalStop] != 1 {
		t.Fatalf("terminal events = %#v", types)
	}
}

type acpFakeMCPFactory struct{ client *acpFakeMCPClient }

func (f acpFakeMCPFactory) New(context.Context, string, []string) (mcp.Client, error) {
	return f.client, nil
}

type acpFakeMCPClient struct {
	mu     sync.Mutex
	closed bool
}

func (c *acpFakeMCPClient) Start(context.Context) error { return nil }
func (c *acpFakeMCPClient) ListTools(context.Context) ([]mcp.Tool, error) {
	return []mcp.Tool{{Name: "lookup", Description: "lookup data", InputSchema: map[string]any{"type": "object"}}}, nil
}
func (c *acpFakeMCPClient) Call(context.Context, string, map[string]any) (mcp.CallResult, error) {
	return mcp.CallResult{Content: []any{map[string]any{"type": "text", "text": "ok"}}}, nil
}
func (c *acpFakeMCPClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func TestACPExplicitMCPOwnsClientAndRegistry(t *testing.T) {
	root := t.TempDir()
	log := session.New()
	fake := &acpFakeMCPClient{}
	a := &app{
		cfg: config.Config{
			Mcp: config.McpConfig{
				Enabled:    config.Bool(true),
				ACPEnabled: config.Bool(true),
				Servers:    []config.McpServer{{Name: "demo", Cmd: "fake"}},
			},
		},
		basePolicy: tools.DefaultPolicy(),
		mcpFactory: acpFakeMCPFactory{client: fake},
	}
	service, err := newACPMCP(context.Background(), a, "acp-test", log)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := acpRegistry(a, "acp-test", root, log, nil, service)
	if err != nil {
		t.Fatal(err)
	}
	got, err := registry.Execute(context.Background(), "mcp__demo__lookup", []byte(`{}`))
	if err != nil || got.Output != "ok" {
		t.Fatalf("MCP advertised tool = %q, err=%v", got.Output, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	closed := fake.closed
	fake.mu.Unlock()
	if !closed {
		t.Fatal("ACP MCP client was not closed with its session")
	}
	types := map[string]int{}
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	if types[session.EventMcpCall] != 1 || types[session.EventMcpList] != 0 {
		t.Fatalf("MCP events = %#v", types)
	}
}

func TestACPExplicitSubagentOwnsRuntimeAndTools(t *testing.T) {
	log := session.New()
	a := &app{
		cfg: config.Config{
			Model: "test-model",
			Subagent: config.SubagentConfig{
				Enabled:    config.Bool(true),
				ACPEnabled: config.Bool(true),
				MaxDepth:   8,
			},
		},
		basePolicy: tools.DefaultPolicy(),
		llm:        acpSummaryLLM{},
	}
	registry, err := acpRegistry(a, "acp-test", t.TempDir(), log, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pb := prompt.New("You are an ACP subagent.")
	pb.SetTools(func() []llm.ToolSchema { return registry.Specs() })
	rt, bundle, err := newACPSubagent(a, "acp-test", log, registry, pb)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerACPSubagentTools(registry, bundle); err != nil {
		_ = rt.Close()
		t.Fatal(err)
	}
	if providers := rt.ListProviders(); len(providers) != 2 || !containsStr(providers, "spawn") || !containsStr(providers, "fork") {
		t.Fatalf("ACP providers = %v, want isolated spawn and fork providers", providers)
	}
	visible := make(map[string]bool)
	for _, spec := range registry.VisibleSpecs() {
		visible[spec.Name] = true
	}
	if !visible[subagent.ToolForkName] {
		_ = rt.Close()
		t.Fatalf("ACP registry does not expose %q: %v", subagent.ToolForkName, visible)
	}
	if _, err := registry.Execute(context.Background(), subagent.ToolSpawnName, []byte(`{"description":"summary","prompt":"summarize","run_in_background":false}`)); err != nil {
		_ = rt.Close()
		t.Fatalf("ACP subagent_spawn: %v", err)
	}
	children, err := rt.ListChildren(context.Background(), "acp-test")
	if err != nil || len(children) != 1 {
		_ = rt.Close()
		t.Fatalf("ACP children = %v, err=%v", children, err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Start(context.Background(), "spawn", subagent.StartRequest{Prompt: "after close"}); err == nil {
		t.Fatal("ACP subagent runtime accepted Start after Close")
	}
	types := map[string]int{}
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	if types[session.EventSubagentStart] != 1 {
		t.Fatalf("ACP subagent events = %#v", types)
	}
}

func TestACPCompactionUsesSessionLog(t *testing.T) {
	log := session.New()
	for _, item := range []struct {
		typ  string
		data any
	}{
		{session.EventUserMessage, session.NewUserMessage("old question")},
		{session.EventAssistantMessage, session.NewAssistantMessage("old answer", nil, "stop")},
		{session.EventUserMessage, session.NewUserMessage("recent question")},
		{session.EventAssistantMessage, session.NewAssistantMessage("recent answer", nil, "stop")},
	} {
		if _, err := log.Append(item.typ, item.data); err != nil {
			t.Fatal(err)
		}
	}
	s := &acpSession{
		app: &app{cfg: config.Config{Compaction: config.CompactionConfig{TokenThreshold: 1}}},
		log: log,
		compaction: compaction.NewBasic(compaction.BasicOpts{
			LLM:            acpSummaryLLM{},
			TokenThreshold: 1,
			RetainTurns:    1,
		}),
	}
	if got := s.compactionPreStep()(context.Background(), ""); len(got) != 1 {
		t.Fatalf("compaction injections = %d, want 1", len(got))
	}
	types := make(map[string]int)
	for _, ev := range log.Events() {
		types[ev.Type]++
	}
	for _, typ := range []string{session.EventCompactionStart, session.EventCompactionSummary, session.EventCompactionEnd} {
		if types[typ] != 1 {
			t.Fatalf("%s count = %d, want 1", typ, types[typ])
		}
	}
	if len(log.DeriveHistory()) == 0 {
		t.Fatal("compaction removed the entire model-visible history")
	}
}

func TestACPImageInputValidation(t *testing.T) {
	if !modelAcceptsImages("text,image") || modelAcceptsImages("text,no-image") {
		t.Fatal("model image modality parsing is not exact")
	}
	decoded, err := decodeCanonicalBase64("aGk=")
	if err != nil || string(decoded) != "hi" {
		t.Fatalf("canonical base64 decode = %q, %v", decoded, err)
	}
	for _, value := range []string{"aGk", "aGk=\n", "-___"} {
		if _, err := decodeCanonicalBase64(value); err == nil {
			t.Fatalf("non-canonical base64 %q was accepted", value)
		}
	}
	if acpImageMediaType("image/tiff") || !acpImageMediaType("image/webp") {
		t.Fatal("ACP image media-type allowlist is incorrect")
	}
}
