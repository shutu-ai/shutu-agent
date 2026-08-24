// webcmd_test.go — the web slash-command tests (dsh 输入条 "/" 对齐, ①③⑤):
// webCommand routes a "/" command to a handler, appends user/message +
// assistant/message (and the command's fact) to the session log, and never
// runs an LLM turn. The webMessage leading-"/" branch is asserted too.
package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

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
	if len(evs) == 0 {
		t.Fatal("log is empty")
	}
	return evs[len(evs)-1]
}

// TestWebCommandGoal verifies /goal routes to plan_goal: the log gains
// user/message, the plan/create fact and an assistant/message carrying the
// created-goal result.
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
	for _, typ := range []string{session.EventUserMessage, session.EventAssistantMessage, session.EventPlanCreate} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log missing %s after /goal", typ)
		}
	}
	text := assistantText(t, lastEvent(t, a.log).Data)
	if !strings.Contains(text, "goal-1") {
		t.Fatalf("/goal result = %q, want it to report the created goal id", text)
	}
}

// TestWebCommandHelp verifies /help returns the command table as the final
// assistant message (no plan/create, no LLM turn).
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
	if text := assistantText(t, lastEvent(t, a.log).Data); !strings.Contains(text, "可用的斜杠命令") {
		t.Fatalf("/help result = %q, want the command table", text)
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
	text := assistantText(t, lastEvent(t, a.log).Data)
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
	if text := assistantText(t, lastEvent(t, a.log).Data); !strings.Contains(text, "current preset full") {
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
	if text := assistantText(t, lastEvent(t, a.log).Data); !strings.Contains(text, "current preset readonly") {
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

// TestWebCommandUnknown verifies an unknown command answers with the /help
// guidance inside the assistant reply (the user message is still logged).
func TestWebCommandUnknown(t *testing.T) {
	a := makePlanApp(true)
	if err := a.webCommand(context.Background(), "/bogus"); err != nil {
		t.Fatalf("webCommand: %v", err)
	}
	if !hasEvent(a.log, session.EventUserMessage) {
		t.Fatal("the command text must be logged as user/message")
	}
	if text := assistantText(t, lastEvent(t, a.log).Data); !strings.Contains(text, "unknown command") {
		t.Fatalf("unknown result = %q, want /help guidance", text)
	}
}

// TestWebMessageSlashRouting verifies webMessage dispatches a leading "/" to
// webCommand: the log gains user/message + assistant/message and the LLM is
// never invoked (the command is not a turn).
func TestWebMessageSlashRouting(t *testing.T) {
	llm := &turnLLM{}
	a := makeTurnApp()
	a.llm = llm
	a.currentID = "s-a"
	if err := a.webMessage(context.Background(), "s-a", "/help", nil); err != nil {
		t.Fatalf("webMessage: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (a slash command must not run a turn)", llm.calls)
	}
	for _, typ := range []string{session.EventUserMessage, session.EventAssistantMessage} {
		if !hasEvent(a.log, typ) {
			t.Fatalf("log missing %s after /help via webMessage", typ)
		}
	}
}
