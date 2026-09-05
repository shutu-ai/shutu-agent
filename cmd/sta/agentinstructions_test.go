package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/agentinstructions"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

func TestVisibleAgentInstructionsStateRebuildsDeltas(t *testing.T) {
	log := session.New()
	if state := visibleAgentInstructionsState(log); state != nil {
		t.Fatalf("empty state = %#v, want nil", state)
	}
	scope := agentinstructions.CandidateScope(".", "AGENTS.md")
	baseline := llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("first")},
		SourceKind: "agent-instructions", SourceForm: "instructions",
		SourceBaseline: true, SourceBaselineIdentity: "identity-1",
		SourceChanges: []agentinstructions.Change{{
			Action: "set", Scope: scope, Path: "AGENTS.md", Digest: "digest-1",
		}},
	}
	if _, err := log.Append(session.EventUserMessage, session.NewContextMessageFromLLM(baseline)); err != nil {
		t.Fatal(err)
	}
	state := visibleAgentInstructionsState(log)
	if state == nil || state.Identity != "identity-1" || state.Changes[scope].Digest != "digest-1" {
		t.Fatalf("baseline state = %#v", state)
	}

	delta := llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("updated")},
		SourceKind: "agent-instructions", SourceForm: "instructions",
		SourceChanges: []agentinstructions.Change{{
			Action: "replace", Scope: scope, Path: "AGENTS.md", Digest: "digest-2",
		}, {
			Action: "set", Scope: agentinstructions.CandidateScope("app", "CLAUDE.md"),
			Path: "app/CLAUDE.md", Digest: "digest-3",
		}},
	}
	if _, err := log.Append(session.EventUserMessage, session.NewContextMessageFromLLM(delta)); err != nil {
		t.Fatal(err)
	}
	state = visibleAgentInstructionsState(log)
	if state == nil || state.Changes[scope].Action != "replace" || state.Changes[scope].Digest != "digest-2" {
		t.Fatalf("reconciled state = %#v", state)
	}

	removal := llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("removed")},
		SourceKind: "agent-instructions", SourceForm: "instructions",
		SourceChanges: []agentinstructions.Change{{
			Action: "remove", Scope: scope, Path: "AGENTS.md",
		}},
	}
	if _, err := log.Append(session.EventUserMessage, session.NewContextMessageFromLLM(removal)); err != nil {
		t.Fatal(err)
	}
	state = visibleAgentInstructionsState(log)
	appScope := agentinstructions.CandidateScope("app", "CLAUDE.md")
	if state == nil || state.Identity != "identity-1" || len(state.Changes) != 1 ||
		state.Changes[appScope].Digest != "digest-3" {
		t.Fatalf("delta must not reset baseline state: %#v", state)
	}

	removal = llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("removed")},
		SourceKind: "agent-instructions", SourceForm: "instructions",
		SourceChanges: []agentinstructions.Change{{
			Action: "remove", Scope: appScope, Path: "app/CLAUDE.md",
		}},
	}
	if _, err := log.Append(session.EventUserMessage, session.NewContextMessageFromLLM(removal)); err != nil {
		t.Fatal(err)
	}
	state = visibleAgentInstructionsState(log)
	if state == nil || len(state.Changes) != 0 {
		t.Fatalf("post-removal state = %#v", state)
	}
}

func TestVisibleAgentInstructionsStateIgnoresShadowedBaseline(t *testing.T) {
	log := session.New()
	baseline := llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("first")},
		SourceKind: "agent-instructions", SourceForm: "instructions",
		SourceBaseline: true, SourceBaselineIdentity: "identity-1",
		SourceChanges: []agentinstructions.Change{{
			Action: "set", Scope: agentinstructions.CandidateScope(".", "AGENTS.md"),
			Path: "AGENTS.md", Digest: "digest-1",
		}},
	}
	event, err := log.Append(session.EventUserMessage, session.NewContextMessageFromLLM(baseline))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessageReplaceWithSources(
		"summary", int64(event.Seq), int64(event.Seq), []uint64{event.Seq},
	)); err != nil {
		t.Fatal(err)
	}
	if state := visibleAgentInstructionsState(log); state != nil {
		t.Fatalf("shadowed state = %#v, want nil", state)
	}
}

func TestAgentInstructionsInjectorProjectsSuccessfulFileTouches(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(workspace, ".git"), "")
	writeFile(filepath.Join(workspace, "AGENTS.md"), "root rules")
	enabled := true
	a := &app{cfg: config.Config{
		Workspace:         config.WorkspaceConfig{DefaultDir: workspace},
		AgentInstructions: config.AgentInstructionsConfig{Enabled: &enabled, Home: home},
	}}
	log := session.New()
	inject := a.agentInstructionsInjectorFor("touch-session", log)
	messages, err := inject.InjectWithError(context.Background(), "inspect")
	if err != nil || len(messages) != 1 || !messages[0].SourceBaseline {
		t.Fatalf("baseline messages = %#v, err=%v", messages, err)
	}

	args := `{"file_path":"pkg/deep/file.txt"}`
	if _, err := log.Append(session.EventToolCall, session.NewToolCall(
		1, 1, "touch-call", "read", args,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventToolResult, session.NewToolResultAt(
		1, 1, "touch-call", "read", "file contents", nil,
	)); err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(workspace, "pkg", "AGENTS.md"), "nested rules")
	writeFile(filepath.Join(workspace, "pkg", "deep", "file.txt"), "file contents")

	messages, err = inject.InjectWithError(context.Background(), "inspect")
	if err != nil || len(messages) != 1 {
		t.Fatalf("nested messages = %#v, err=%v", messages, err)
	}
	if messages[0].SourceBaseline {
		t.Fatalf("nested context = %#v, want non-baseline delta", messages[0])
	}
	changes, ok := messages[0].SourceChanges.([]agentinstructions.Change)
	if !ok || len(changes) != 1 || changes[0].Action != "set" ||
		changes[0].Path != filepath.ToSlash(filepath.Join("pkg", "AGENTS.md")) {
		t.Fatalf("nested source changes = %#v", messages[0].SourceChanges)
	}
	if !containsText(messages[0], "nested rules") {
		t.Fatalf("nested message = %#v", messages[0])
	}
}

func containsText(message llm.Message, want string) bool {
	return strings.Contains(message.Text(), want)
}
