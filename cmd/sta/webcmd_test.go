// webcmd_test.go — the web slash-command tests (dsh 输入条 "/" 对齐, ①③⑤):
// webCommand routes a "/" command to a handler, records the command lifecycle
// and a UI-only result (never model history), and never runs an LLM turn. The
// webMessage leading-"/" branch is asserted too.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

func webCommandResultText(t *testing.T, log *session.Log) string {
	t.Helper()
	var text string
	found := false
	for _, event := range log.Events() {
		if event.Type != session.EventWebCommandResult {
			continue
		}
		var result struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Data, &result); err != nil {
			t.Fatal(err)
		}
		text = result.Text
		found = true
	}
	if !found {
		t.Fatalf("session has no web/command-result: %+v", log.Events())
	}
	return text
}

// assistantText extracts the "text" field of an assistant/message payload.
func assistantText(t *testing.T, data any) string {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal assistant data: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal assistant data: %v", err)
	}
	if txt, ok := m["text"].(string); ok {
		return txt
	}
	return ""
}

// lastEvent returns the final event in the log.
func lastEvent(t *testing.T, log *session.Log) session.Event {
	t.Helper()
	evs := log.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type != session.EventCommandRun && evs[i].Type != session.EventCommandDone {
			return evs[i]
		}
	}
	t.Fatal("log contains only command lifecycle events")
	return session.Event{}
}

// TestWebCommandGoal verifies /goal routes to plan_goal without entering model
// history; the UI result is carried by web/command-result.
func TestWebCommandGoal(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	if err := a.webCommand(context.Background(), "/goal Ship 完成目标"); err != nil {
		t.Fatalf("webCommand: %v", err)
	}
	for _, typ := range []string{session.EventCommandRun, session.EventPlanCreate, session.EventWebCommandResult, session.EventCommandDone} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log missing %s after /goal", typ)
		}
	}
	text := webCommandResultText(t, a.log)
	if !strings.Contains(text, "goal-1") {
		t.Fatalf("/goal result = %q, want it to report the created goal id", text)
	}
}

func TestNativeCommandManagerReturnsCommittedWebLifecycleResult(t *testing.T) {
	a := makePlanApp(false)
	manager := nativeCommandManager{app: a}
	execution, matched, err := manager.Execute(context.Background(), a.currentID, "/help", nil)
	if err != nil || !matched {
		t.Fatalf("native command execute = %+v, matched=%v, err=%v", execution, matched, err)
	}
	if execution.CommandID == "" || !strings.HasPrefix(execution.CommandID, "shutu-cmd-") {
		t.Fatalf("native command id = %q, want committed shutu-cmd id", execution.CommandID)
	}
	if execution.Result.Kind != "success" || !strings.Contains(execution.Result.Text, "斜杠") {
		t.Fatalf("native command result = %+v, want successful help text", execution.Result)
	}
	if events := a.log.Events(); len(events) != 3 || events[0].Type != session.EventCommandRun || events[2].Type != session.EventCommandDone {
		t.Fatalf("native command events = %+v, want one committed run/result/done sequence", events)
	}
}

func TestWebCommandRejectsUndeclaredImagesBeforeModelTurn(t *testing.T) {
	a := makePlanApp(false)
	image := llm.ImageRef{ID: "image-1", MediaType: "image/png"}
	if err := a.webMessage(context.Background(), a.currentID, "/help", []llm.ImageRef{image}, webserver.PromptMeta{}); err == nil {
		t.Fatal("/help with an image was accepted")
	}
	if events := a.log.Events(); len(events) != 0 {
		t.Fatalf("image-rejected command wrote events = %+v", events)
	}
	commands, err := (nativeCommandManager{app: a}).List(context.Background(), a.currentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if command.Name == "help" && command.Images {
			t.Fatal("/help incorrectly advertises image support")
		}
		if command.Name == "plan" && !command.Images {
			t.Fatal("/plan does not advertise its image support")
		}
	}
}

func TestWebCommandUsesRuntimeSessionLog(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.closePlanEngines()
	a.agentRegistry = agent.NewRegistry()
	defer a.agentRegistry.CloseAll()
	a.basePolicy = planPolicy()
	a.prompt = makeTurnApp().prompt
	// The global registry is deliberately unusable. An Agent-backed Web
	// command must derive a scoped policy from basePolicy rather than execute
	// directly against this process-global view.
	a.reg.SetPolicy(tools.Policy{})
	legacy := session.New()
	target := session.New()
	a.currentID = "legacy"
	a.log = legacy
	a.runtimeMu.Lock()
	a.runtimeLogs = map[string]*session.Log{"target": target}
	a.runtimeMu.Unlock()
	ctx := runtimectx.With(context.Background(), runtimectx.Runtime{
		SessionID: "target",
		Emit: func(typ string, data any) error {
			_, err := target.Append(typ, data)
			return err
		},
	})
	if err := a.webCommand(ctx, "/goal Ship target"); err != nil {
		t.Fatalf("webCommand: %v", err)
	}
	for _, typ := range []string{session.EventCommandRun, session.EventPlanCreate, session.EventWebCommandResult, session.EventCommandDone} {
		if !hasEvent(target, typ) {
			t.Fatalf("target log missing %s: %+v", typ, target.Events())
		}
	}
	if len(legacy.Events()) != 0 {
		t.Fatalf("runtime command leaked into legacy log: %+v", legacy.Events())
	}
}

// TestWebCommandHelp verifies /help returns the command table as a UI-only
// result (no plan/create, no LLM turn).
func TestWebCommandHelp(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	if err := a.webCommand(context.Background(), "/help"); err != nil {
		t.Fatalf("webCommand: %v", err)
	}
	if hasEvent(a.log, session.EventPlanCreate) {
		t.Fatal("/help must not run plan_goal")
	}
	text := webCommandResultText(t, a.log)
	if !strings.Contains(text, "可用的斜杠命令") || !strings.Contains(text, "/export") {
		t.Fatalf("/help result = %q, want the command table", text)
	}
	if strings.Contains(text, "其他文本") {
		t.Fatalf("/help contains the removed fallback row: %q", text)
	}
}

func TestWebCommandCatalogAppendsUserSkillsAfterCommands(t *testing.T) {
	a, proj := skillFixture(t, true)
	writeSkill(t, filepath.Join(proj, ".dsh", "skills"), "review-bash", "review bash scripts", "body")
	private := filepath.Join(proj, ".dsh", "skills", "private.md")
	if err := os.WriteFile(private, []byte("---\ndescription: private\nuser-invocable: false\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.registerSkills(); err != nil {
		t.Fatalf("registerSkills: %v", err)
	}
	defer a.skills.Close()

	catalog := a.webCommandCatalog()
	commands := []string{"help", "status", "compact", "permission", "feedback", "goal", "plan", "export"}
	if len(catalog) <= len(commands) {
		t.Fatalf("catalog = %#v, want commands plus user skill", catalog)
	}
	for i, want := range commands {
		if catalog[i]["name"] != want {
			t.Fatalf("catalog[%d] = %#v, want built-in command %q", i, catalog[i], want)
		}
	}
	if catalog[len(commands)]["name"] != "review-bash" || catalog[len(commands)]["hint"] != "Skill: review bash scripts" {
		t.Fatalf("skill catalog entry = %#v, want review-bash after commands", catalog[len(commands)])
	}
	help := a.webHelp()
	if !strings.Contains(help, "/review-bash") || !strings.Contains(help, "review bash scripts") {
		t.Fatalf("/help = %q, want the discovered skill", help)
	}
	if strings.Contains(help, "/private") {
		t.Fatalf("/help exposed a non-user-invocable skill: %q", help)
	}
	for _, item := range catalog {
		if item["name"] == "private" {
			t.Fatal("user-invocable:false skill must not be offered as a slash entry")
		}
	}
}

func TestWebCommandFeedbackMatchesDSH(t *testing.T) {
	a := makePlanApp(true)
	a.currentID = "s-feedback"
	if err := a.webCommand(context.Background(), "/feedback  /plan felt SLOW\n\ttwice today "); err != nil {
		t.Fatalf("webCommand feedback: %v", err)
	}
	evs := a.log.Events()
	if len(evs) != 4 || evs[0].Type != session.EventCommandRun || evs[1].Type != session.EventFeedbackRecord || evs[2].Type != session.EventWebCommandResult || evs[3].Type != session.EventCommandDone {
		t.Fatalf("feedback events = %+v, want command/run + feedback/record + web/command-result + command/done", evs)
	}
	var feedback struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(evs[1].Data, &feedback); err != nil {
		t.Fatal(err)
	}
	if feedback.Text != "/plan felt SLOW\n\ttwice today" {
		t.Fatalf("feedback text = %q, want surrounding whitespace trimmed only", feedback.Text)
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(evs[2].Data, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Text, "Feedback recorded for session s-feedback\nAnonymous user: anonymous-") || !strings.HasSuffix(result.Text, ". Session sharing is not configured.") {
		t.Fatalf("feedback result = %q", result.Text)
	}
	var commandRun struct {
		Args string `json:"args"`
	}
	if err := json.Unmarshal(evs[0].Data, &commandRun); err != nil {
		t.Fatal(err)
	}
	if commandRun.Args != "" {
		t.Fatalf("feedback command/run leaked input args = %q", commandRun.Args)
	}
	if got := a.log.DeriveHistory(); len(got) != 0 {
		t.Fatalf("feedback entered model history: %+v", got)
	}

	if err := a.webCommand(context.Background(), "/feedback   "); err != nil {
		t.Fatalf("empty feedback command: %v", err)
	}
	if countEvent(a.log, session.EventFeedbackRecord) != 1 {
		t.Fatalf("empty feedback created a record; events=%+v", a.log.Events())
	}
	last := lastEvent(t, a.log)
	if err := json.Unmarshal(last.Data, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Feedback text is required. Usage: /feedback <text>") {
		t.Fatalf("empty feedback result = %q", result.Text)
	}
}

func TestWebCommandPlanModeMatchesDSH(t *testing.T) {
	a := makePlanApp(true)
	a.currentID = "s-plan-mode"
	if err := a.webCommand(context.Background(), "/plan"); err != nil {
		t.Fatalf("/plan: %v", err)
	}
	if !session.FoldPlanMode(a.log.Events()) {
		t.Fatal("/plan did not activate plan mode")
	}
	if hasEvent(a.log, session.EventUserMessage) || hasEvent(a.log, session.EventAssistantMessage) {
		t.Fatal("/plan polluted model history")
	}
	if err := a.webCommand(context.Background(), "/plan off"); err != nil {
		t.Fatalf("/plan off: %v", err)
	}
	if session.FoldPlanMode(a.log.Events()) {
		t.Fatal("/plan off did not deactivate plan mode")
	}
}

func TestWebMessagePlanModeSubmitsOnlySuffix(t *testing.T) {
	llm := &turnLLM{}
	a := makeTurnApp()
	a.llm = llm
	a.currentID = "s-plan-turn"
	if err := a.webMessage(context.Background(), "s-plan-turn", "/plan design the change", nil, webserver.PromptMeta{}); err != nil {
		t.Fatalf("webMessage /plan: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1", llm.calls)
	}
	history := a.log.DeriveHistory()
	if len(history) == 0 || history[0].Text() != "design the change" {
		t.Fatalf("plan message history = %+v, want only the suffix", history)
	}
	if !session.FoldPlanMode(a.log.Events()) {
		t.Fatal("plan mode was not persisted")
	}
}

func TestWebGoalLifecycleMatchesDSH(t *testing.T) {
	a := makePlanApp(true)
	a.reg.SetPolicy(planPolicy())
	if err := a.registerPlans(); err != nil {
		t.Fatalf("registerPlans: %v", err)
	}
	defer a.plans.Close()
	ctx := context.Background()
	for _, command := range []string{"/goal Ship release", "/goal pause"} {
		if err := a.webCommand(ctx, command); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	goals, err := a.plans.List(ctx)
	if err != nil || len(goals) != 1 || string(goals[0].Status) != "paused" {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("paused goals = %+v", goals)
	}
	if err := a.webCommand(ctx, "/goal edit Ship updated release"); err != nil {
		t.Fatalf("/goal edit: %v", err)
	}
	if err := a.webCommand(ctx, "/goal resume"); err != nil {
		t.Fatalf("/goal resume: %v", err)
	}
	if err := a.webCommand(ctx, "/goal clear"); err != nil {
		t.Fatalf("/goal clear: %v", err)
	}
	goals, err = a.plans.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 0 {
		t.Fatalf("goals after clear = %+v", goals)
	}
	for _, typ := range []string{session.EventPlanCreate, session.EventPlanUpdate, session.EventPlanStatus, session.EventPlanDelete} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("goal lifecycle missing %s", typ)
		}
	}
}

// TestWebCommandCompact verifies /compact uses the manual compaction engine
// and returns its report as the Web assistant message.
func TestWebCommandCompact(t *testing.T) {
	a := makeCompactApp(true)
	a.log = threeTurnLog(t)
	a.compaction = basicEngine(nil, &compactStubLLM{text: "S"})
	if err := a.webCommand(context.Background(), "/compact"); err != nil {
		t.Fatalf("webCommand: %v", err)
	}
	text := webCommandResultText(t, a.log)
	if !strings.Contains(text, "compacted") || !strings.Contains(text, "summary: S") {
		t.Fatalf("/compact result = %q, want compaction report", text)
	}
	if countEvent(a.log, session.EventCompactionStart) != 1 || countEvent(a.log, session.EventCompactionEnd) != 1 {
		t.Fatalf("/compact event counts = start %d, end %d, want one each",
			countEvent(a.log, session.EventCompactionStart), countEvent(a.log, session.EventCompactionEnd))
	}
}

// TestWebCommandPermission verifies /permission reads and persists the active
// session's permission override.
func TestWebCommandPermission(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "permission.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "s-permission", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "permission_preset", "full"); err != nil {
		t.Fatal(err)
	}
	a := makePlanApp(true)
	a.store = st
	a.currentID = "s-permission"
	if err := a.webCommand(ctx, "/permission"); err != nil {
		t.Fatalf("read global permission: %v", err)
	}
	if text := webCommandResultText(t, a.log); !strings.Contains(text, "current preset full") {
		t.Fatalf("global permission query = %q, want full", text)
	}
	if err := a.webCommand(ctx, "/permission readonly"); err != nil {
		t.Fatalf("set permission: %v", err)
	}
	cfg, err := st.GetSessionConfig(ctx, a.currentID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Permission != "readonly" {
		t.Fatalf("session permission = %q, want readonly", cfg.Permission)
	}
	if err := a.webCommand(ctx, "/permission"); err != nil {
		t.Fatalf("read permission: %v", err)
	}
	if text := webCommandResultText(t, a.log); !strings.Contains(text, "current preset readonly") {
		t.Fatalf("permission query = %q, want readonly", text)
	}
}

func TestWebCommandCatalogIncludesPermission(t *testing.T) {
	a := &app{}
	for _, command := range a.webCommandCatalog() {
		if command["name"] == "permission" {
			if !strings.Contains(command["hint"], "readonly") {
				t.Fatalf("permission hint = %q, want preset choices", command["hint"])
			}
			return
		}
	}
	t.Fatal("backend command catalog is missing permission")
}

func TestWebCommandCatalogIncludesFeedback(t *testing.T) {
	a := &app{}
	for _, command := range a.webCommandCatalog() {
		if command["name"] == "feedback" {
			if command["hint"] != "Record feedback: /feedback <text>" {
				t.Fatalf("feedback hint = %q", command["hint"])
			}
			return
		}
	}
	t.Fatal("backend command catalog is missing feedback")
}

func TestWebCommandExportMatchesDSH(t *testing.T) {
	a := makePlanApp(true)
	a.currentID = "s-export"
	if err := a.webCommand(context.Background(), "/export"); err != nil {
		t.Fatalf("/export: %v", err)
	}
	evs := a.log.Events()
	if len(evs) != 3 || evs[0].Type != session.EventCommandRun || evs[1].Type != session.EventWebCommandResult || evs[2].Type != session.EventCommandDone {
		t.Fatalf("export events = %+v, want command/run + web/command-result + command/done", evs)
	}
	var result struct {
		Text    string `json:"text"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(evs[1].Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Text != "Session log download requested." || result.Command != "export" {
		t.Fatalf("export result = %+v", result)
	}
	if len(a.log.DeriveHistory()) != 0 {
		t.Fatal("/export entered model history")
	}
	if err := a.webCommand(context.Background(), "/export output.zip"); err != nil {
		t.Fatalf("/export path: %v", err)
	}
	if countEvent(a.log, session.EventUserMessage) != 0 {
		t.Fatal("/export path entered model history")
	}
}

// TestWebCommandUnknown verifies an unknown slash command is rejected before
// it can enter model history, matching the reference command executor.
func TestWebCommandUnknown(t *testing.T) {
	a := makePlanApp(true)
	if err := a.webCommand(context.Background(), "/bogus"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("webCommand error = %v, want unknown-command rejection", err)
	}
	if len(a.log.Events()) != 0 {
		t.Fatalf("unknown command polluted session history: %+v", a.log.Events())
	}
}

// TestWebMessageSlashRouting verifies webMessage dispatches a leading "/" to
// webCommand without entering model history or starting an LLM turn.
func TestWebMessageSlashRouting(t *testing.T) {
	llm := &turnLLM{}
	a := makeTurnApp()
	a.llm = llm
	a.currentID = "s-a"
	if err := a.webMessage(context.Background(), "s-a", "/help", nil, webserver.PromptMeta{}); err != nil {
		t.Fatalf("webMessage: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (a slash command must not run a turn)", llm.calls)
	}
	for _, typ := range []string{session.EventCommandRun, session.EventWebCommandResult, session.EventCommandDone} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log missing %s after /help via webMessage", typ)
		}
	}
}
