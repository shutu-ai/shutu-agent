package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/code"
	"github.com/jabing/shutu-agent/internal/fs"
	"github.com/jabing/shutu-agent/internal/fssearch"
	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/ralph"
	"github.com/jabing/shutu-agent/internal/schedule"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/sessionquery"
	"github.com/jabing/shutu-agent/internal/skill"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/tools"
	"github.com/jabing/shutu-agent/internal/web"
	"github.com/jabing/shutu-agent/internal/workflow"
)

type contractSessionBackend struct{ store *store.SQLiteStore }

func (b contractSessionBackend) SearchSessions(ctx context.Context, query string) ([]store.SearchHit, error) {
	return b.store.SearchSessions(ctx, query)
}

func (b contractSessionBackend) LoadSession(ctx context.Context, id string) ([]session.Event, error) {
	return b.store.LoadSession(ctx, id)
}

func (b contractSessionBackend) ListSessions(ctx context.Context) ([]store.SessionMeta, error) {
	return b.store.ListSessions(ctx)
}

type contractCodeRuntime struct{}

func (contractCodeRuntime) RunProgram(context.Context, code.ProgramRequest) (code.ProgramResult, error) {
	return code.ProgramResult{}, context.Canceled
}

func (contractCodeRuntime) Close() error { return nil }

type richContractTool struct{}

func (richContractTool) Name() string        { return "rich_contract" }
func (richContractTool) Description() string { return "rich result contract fixture" }
func (richContractTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (richContractTool) OutputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t richContractTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}
func (richContractTool) ExecuteResult(context.Context, any) (tools.ToolResult, error) {
	return tools.ToolResult{
		Value:  map[string]any{"ok": true},
		Output: `{"ok":true}`,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{ID: "rich-image", MediaType: "image/png", Bytes: 3}},
			{Kind: "audio", Raw: json.RawMessage(`{"type":"audio","data":"AQ==","mimeType":"audio/wav","metadata":{"durationMs":7}}`)},
			{Kind: "resource", Raw: json.RawMessage(`{"type":"resource","uri":"file:///contract.md","metadata":{"version":2}}`)},
		},
		Meta: map[string]any{"kind": "contract", "ordered": true},
	}, nil
}

// TestRequiredToolNegativeContractMatrix closes the production-object half of
// A4.4. Every model-facing required tool is instantiated through its owning
// package and driven through the same public Registry boundaries. The invalid
// arguments never enter a tool body: the matrix proves denial is stable, schema
// admission is tool-owned, pre-execute policy remains ordered after D7, and
// every rejection emits exactly one terminal tools/result observation.
func TestRequiredToolNegativeContractMatrix(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "contract.db")
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fsTools := fs.NewFsTools(fs.NewLocalFS(t.TempDir()), nil)
	webTools := web.NewWebTools(web.NewEngine(), web.Options{}, nil)
	query := sessionquery.NewToolsWithConfigContext(
		contractSessionBackend{store: st},
		func(context.Context) string { return "contract-owner" },
		10,
		0,
	)
	interactEngine := interact.NewEngine(nil)
	defer interactEngine.Close()
	interacts := interact.NewInteractToolsWithSessionAndErrorSink(interactEngine, nil, nil, func() string {
		return "contract-owner"
	})
	planTools := plan.NewDSHTools(plan.NewEngine(plan.NewMemProvider()), nil)
	jobRegistry := jobs.NewLocal(jobs.LocalOpts{})
	defer jobRegistry.Close()
	jobTools := jobs.NewJobToolsWithContext(jobRegistry, func(context.Context) string {
		return "contract-owner"
	}, func(context.Context) string { return t.TempDir() }, nil)
	subagents := subagent.NewSubagentToolsWithContinuableContext(subagent.NewRuntime(), 2, func(context.Context) string {
		return "contract-owner"
	}, nil, true)
	codeTools := code.NewCodeToolsWithRuntime(contractCodeRuntime{}, nil)
	skillRegistry := skill.NewRegistry()
	skillTools := skill.NewSkillTools(skillRegistry, 1024, nil)
	scheduleTools := schedule.NewScheduleTools(schedule.NewEngine(schedule.NewMemProvider()), nil)
	mcpTools := mcp.NewMcpTools(nil, nil, nil)
	spawn := func(context.Context, string) (string, error) { return "", context.Canceled }
	ralphEngine, err := ralph.NewEngineWithLimit(spawn, 1)
	if err != nil {
		t.Fatal(err)
	}
	workflowEngine, err := workflow.NewEngine(spawn, 1)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		args  json.RawMessage
		valid json.RawMessage
	}{
		{"get_time", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"read", json.RawMessage(`{}`), json.RawMessage(`{"file_path":"contract"}`)},
		{"write", json.RawMessage(`{}`), json.RawMessage(`{"file_path":"contract","content":"x"}`)},
		{"edit", json.RawMessage(`{}`), json.RawMessage(`{"file_path":"contract","old_string":"a","new_string":"b"}`)},
		{"str_replace_editor", json.RawMessage(`{}`), json.RawMessage(`{"command":"view","path":"contract"}`)},
		{"pwsh", json.RawMessage(`{}`), json.RawMessage(`{"command":"Write-Output contract","description":"contract"}`)},
		{"terminal_open", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{"type":"shell"}`)},
		{"terminal_list", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"terminal_read", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{"sessionId":"contract"}`)},
		{"terminal_send", json.RawMessage(`{}`), json.RawMessage(`{"sessionId":"contract","text":"exit"}`)},
		{"terminal_signal", json.RawMessage(`{}`), json.RawMessage(`{"sessionId":"contract","signal":"SIGINT"}`)},
		{"terminal_close", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{"sessionId":"contract"}`)},
		{"glob", json.RawMessage(`{}`), json.RawMessage(`{"pattern":"*.go"}`)},
		{"grep", json.RawMessage(`{}`), json.RawMessage(`{"pattern":"contract"}`)},
		{"read_image", json.RawMessage(`{}`), json.RawMessage(`{"file_path":"contract.png"}`)},
		{"skill", json.RawMessage(`{}`), json.RawMessage(`{"name":"contract"}`)},
		{"web_search", json.RawMessage(`{}`), json.RawMessage(`{"queries":["contract"]}`)},
		{"web_fetch", json.RawMessage(`{}`), json.RawMessage(`{"url":"https://example.invalid/contract"}`)},
		{"schedule_create", json.RawMessage(`{}`), json.RawMessage(`{"kind":"interval","spec":"1h","payload":"contract"}`)},
		{"schedule_delete", json.RawMessage(`{}`), json.RawMessage(`{"id":"contract"}`)},
		{"schedule_list", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"create_goal", json.RawMessage(`{}`), json.RawMessage(`{"objective":"contract"}`)},
		{"get_goal", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"update_goal", json.RawMessage(`{}`), json.RawMessage(`{"goal_id":"contract","revision":1,"action":"complete"}`)},
		{"todo_write", json.RawMessage(`{}`), json.RawMessage(`{"todos":[]}`)},
		{"exit_plan_mode", json.RawMessage(`{}`), json.RawMessage(`{"plan":"# Contract"}`)},
		{"ask_user_question", json.RawMessage(`{}`), json.RawMessage(`{"questions":[{"id":"q","question":"contract"}]}`)},
		{"subagent", json.RawMessage(`{}`), json.RawMessage(`{"description":"contract","prompt":"contract"}`)},
		{"subagent_fork", json.RawMessage(`{}`), json.RawMessage(`{"description":"contract","prompt":"contract"}`)},
		{"spawn_teammate", json.RawMessage(`{}`), json.RawMessage(`{"name":"contract","description":"contract","prompt":"contract"}`)},
		{"list_agents", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"wait_agent", json.RawMessage(`{"timeout_ms":1}`), json.RawMessage(`{}`)},
		{"interrupt_agent", json.RawMessage(`{}`), json.RawMessage(`{"agent_id":"contract"}`)},
		{"send_message", json.RawMessage(`{}`), json.RawMessage(`{"target":"contract","message":"contract"}`)},
		{"followup_task", json.RawMessage(`{}`), json.RawMessage(`{"target":"contract","message":"contract"}`)},
		{"report", json.RawMessage(`{}`), json.RawMessage(`{"output":"contract"}`)},
		{"job_output", json.RawMessage(`{}`), json.RawMessage(`{"job_id":"contract"}`)},
		{"job_kill", json.RawMessage(`{}`), json.RawMessage(`{"job_id":"contract"}`)},
		{"job_list", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"ralph", json.RawMessage(`{}`), json.RawMessage(`{"objective":"contract"}`)},
		{"workflow", json.RawMessage(`{}`), json.RawMessage(`{"meta":{"name":"contract","description":"contract"},"script":"await log('contract')"}`)},
		{"session_search", json.RawMessage(`{}`), json.RawMessage(`{"query":"contract"}`)},
		{"session_trace", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{}`)},
		{"session_event_read", json.RawMessage(`{}`), json.RawMessage(`{"seq":1}`)},
		{"session_event_search", json.RawMessage(`{}`), json.RawMessage(`{"query":"contract"}`)},
		{"session_event_trace", json.RawMessage(`{}`), json.RawMessage(`{"seq":1}`)},
		{"mcp_list", json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{"server":"contract"}`)},
		{"mcp_call", json.RawMessage(`{}`), json.RawMessage(`{"server":"contract","tool":"contract"}`)},
		{"run_code", json.RawMessage(`{}`), json.RawMessage(`{"code":"return 'contract'","description":"contract"}`)},
	}

	toolsByName := map[string]tools.Tool{
		tools.GetTime{}.Name():            tools.GetTime{},
		fsTools.Read().Name():             fsTools.Read(),
		fsTools.Write().Name():            fsTools.Write(),
		fsTools.Edit().Name():             fsTools.Edit(),
		fsTools.StrReplaceEditor().Name(): fsTools.StrReplaceEditor(),
		fsTools.ReadImage().Name():        fsTools.ReadImage(),
		"pwsh":                            tools.NewPwsh(tools.PwshOpts{}),
		"terminal_open":                   modelTerminalTool{kind: "terminal_open"},
		"terminal_list":                   modelTerminalTool{kind: "terminal_list"},
		"terminal_read":                   modelTerminalTool{kind: "terminal_read"},
		"terminal_send":                   modelTerminalTool{kind: "terminal_send"},
		"terminal_signal":                 modelTerminalTool{kind: "terminal_signal"},
		"terminal_close":                  modelTerminalTool{kind: "terminal_close"},
		"glob":                            fssearch.NewGlobToolForCWD(func() string { return t.TempDir() }),
		"grep":                            fssearch.NewGrepToolForCWD(func() string { return t.TempDir() }),
		"skill":                           skillTools.Load(),
		"web_search":                      webTools.Search(),
		"web_fetch":                       webTools.Fetch(),
		"schedule_create":                 scheduleTools.Create(),
		"schedule_delete":                 scheduleTools.Delete(),
		"schedule_list":                   scheduleTools.List(),
		"create_goal":                     planTools.CreateGoal(),
		"get_goal":                        planTools.GetGoal(),
		"update_goal":                     planTools.UpdateGoal(),
		"todo_write":                      planTools.TodoWrite(),
		"exit_plan_mode":                  exitPlanModeTool{},
		"ask_user_question":               interacts.AskUserQuestion(),
		"subagent":                        subagents.Spawn(),
		"subagent_fork":                   subagents.Fork(),
		"spawn_teammate":                  subagents.SpawnTeammate(),
		"list_agents":                     subagents.ListAgents(),
		"wait_agent":                      subagents.WaitAgent(),
		"interrupt_agent":                 subagents.Interrupt(),
		"send_message":                    subagents.DshSend(),
		"followup_task":                   subagents.FollowupTask(),
		"report":                          subagents.Report(),
		"job_output":                      jobTools.DshOutput(),
		"job_kill":                        jobTools.DshKill(),
		"job_list":                        jobTools.DshList(),
		"ralph":                           ralph.NewRalphTool(ralphEngine, nil),
		"workflow":                        workflow.NewWorkflowRunTool(workflowEngine, nil),
		"session_search":                  query.Search(),
		"session_trace":                   query.Trace(),
		"session_event_read":              query.Read(),
		"session_event_search":            query.EventSearch(),
		"session_event_trace":             query.EventTrace(),
		"mcp_list":                        mcpTools.List(),
		"mcp_call":                        mcpTools.Call(),
		"run_code":                        codeTools.Run(),
	}
	boundaryCalls := make(map[string]*atomic.Bool, len(toolsByName))
	for name, tool := range toolsByName {
		called := &atomic.Bool{}
		boundaryCalls[name] = called
		toolsByName[name] = boundaryContractTool{Tool: tool, called: called}
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := toolsByName[tc.name]
			if !ok {
				t.Fatalf("required production tool %q was not assembled", tc.name)
			}
			if got := tool.Name(); got != tc.name {
				t.Fatalf("tool name = %q, want %q", got, tc.name)
			}

			registry := tools.New()
			if err := registry.RegisterWithInfo(tool, tools.RegistrationInfo{
				Owner: "tool-contract", Plugin: "matrix", SessionID: "contract-owner",
			}); err != nil {
				t.Fatal(err)
			}
			var observed atomic.Int32
			var replayResult tools.ToolResult
			registry.AddResultHook(func(execution tools.Execution, result tools.ToolResult) {
				observed.Add(1)
				if !result.IsError || result.Error == nil {
					t.Errorf("observed rejection = %+v, want structured terminal error", result)
				}
				if strings.HasPrefix(execution.CallID, "approval:") {
					replayResult = result
				}
			})

			registry.SetPolicy(tools.Policy{Enabled: nil})
			if _, err := registry.ExecuteWithCallID(ctx, "disabled:"+tc.name, tc.name, tc.args); tools.ErrorInfoOf(err).Code != tools.CodeToolDenied {
				t.Fatalf("disabled tool error = %+v, want %s", err, tools.CodeToolDenied)
			}
			if got := observed.Load(); got != 1 {
				t.Fatalf("disabled observer count = %d, want 1", got)
			}

			registry.SetPolicy(tools.Policy{Enabled: []string{tc.name}})
			var preHookEntered atomic.Bool
			registry.AddPreExecuteHook(func(context.Context, tools.Execution) (tools.PreToolDecision, error) {
				preHookEntered.Store(true)
				return tools.PreToolDecision{Kind: "deny", Reason: "contract denial"}, nil
			})
			if _, err := registry.ExecuteWithCallID(ctx, "invalid:"+tc.name, tc.name, tc.args); tools.ErrorInfoOf(err).Code != tools.CodeInvalidArgs {
				t.Fatalf("invalid arguments error = %+v, want %s", err, tools.CodeInvalidArgs)
			}
			if preHookEntered.Load() {
				t.Fatal("invalid arguments reached the pre-execute policy")
			}
			if got := observed.Load(); got != 2 {
				t.Fatalf("invalid-args observer count = %d, want 2", got)
			}

			if _, err := registry.ExecuteWithCallID(ctx, "approval:"+tc.name, tc.name, tc.valid); tools.ErrorInfoOf(err).Code != tools.CodeToolDenied {
				t.Fatalf("approval denial error = %+v, want %s", err, tools.CodeToolDenied)
			}
			if !preHookEntered.Load() {
				t.Fatal("approval-boundary pre-hook was not reached")
			}
			if got := observed.Load(); got != 3 {
				t.Fatalf("terminal observer count = %d, want one result per rejected call", got)
			}

			// The normalized rejection is replay evidence, not merely a live Go
			// error. Append the canonical tool lifecycle, restore it through the
			// same event vocabulary used by SQLite/JSONL replay, and require the
			// stable code and terminal output to survive.
			durable := session.New()
			appends := []struct {
				typ     string
				payload any
			}{
				{session.EventTurnStart, session.NewTurnStartAt(1)},
				{session.EventStepStart, session.NewStepStartAt(1, 1)},
				{session.EventToolCall, session.NewToolCall(1, 1, "approval:"+tc.name, tc.name, string(tc.valid))},
			}
			for _, appendCase := range appends {
				if _, err := durable.Append(appendCase.typ, appendCase.payload); err != nil {
					t.Fatalf("append %s: %v", appendCase.typ, err)
				}
			}
			sourceSeq := durable.Events()[len(durable.Events())-1].Seq
			if _, err := durable.Append(session.EventToolResult, session.NewToolErrorResultAtCodeWithSourceMeta(
				1, 1, "approval:"+tc.name, tc.name, replayResult.Output, nil, replayResult.Error.Code, sourceSeq, replayResult.Meta,
			)); err != nil {
				t.Fatal(err)
			}
			if _, err := durable.Append(session.EventStepEnd, session.NewStepEndAt(1, 1, "error", "")); err != nil {
				t.Fatal(err)
			}
			if _, err := durable.Append(session.EventTurnEnd, session.NewTurnEndAt(1, "error", "")); err != nil {
				t.Fatal(err)
			}
			replayed := session.New()
			if err := replayed.Restore(durable.Events()); err != nil {
				t.Fatal(err)
			}
			durableID := "tool-contract:" + tc.name
			if err := st.CreateSession(ctx, durableID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvents(ctx, durableID, durable.Events()); err != nil {
				t.Fatal(err)
			}
			loaded, err := st.LoadSession(ctx, durableID)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != len(durable.Events()) {
				t.Fatalf("SQLite durable event count = %d, want %d", len(loaded), len(durable.Events()))
			}
			for i := range loaded {
				if loaded[i].Type != durable.Events()[i].Type || string(loaded[i].Data) != string(durable.Events()[i].Data) {
					t.Fatalf("SQLite durable event %d = %s/%s, want %s/%s", loaded[i].Seq, loaded[i].Type, loaded[i].Data, durable.Events()[i].Type, durable.Events()[i].Data)
				}
			}
			var replayData struct {
				CallID string `json:"callId"`
				Output string `json:"output"`
				Error  struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			resultEvent := loaded[3]
			if resultEvent.Type != session.EventToolResult {
				t.Fatalf("SQLite replay event = %s, want tool/result", resultEvent.Type)
			}
			if err := json.Unmarshal(resultEvent.Data, &replayData); err != nil {
				t.Fatal(err)
			}
			if replayData.CallID != "approval:"+tc.name || replayData.Output != replayResult.Output || replayData.Error.Code != replayResult.Error.Code {
				t.Fatalf("SQLite replayed rejection = %+v, want output/code from %+v", replayData, replayResult)
			}
			if boundaryCalls[tc.name].Load() {
				t.Fatalf("rejected %q reached the production tool body", tc.name)
			}
		})
	}
}

// boundaryContractTool delegates every execution form while recording whether
// the production tool body was reached. The negative matrix uses this to prove
// denials stop at the registry boundary for both legacy Execute tools and rich
// ExecuteResult tools.
type boundaryContractTool struct {
	tools.Tool
	called *atomic.Bool
}

func (t boundaryContractTool) Execute(ctx context.Context, args any) (string, error) {
	t.called.Store(true)
	return t.Tool.Execute(ctx, args)
}

func (t boundaryContractTool) ExecuteResult(ctx context.Context, args any) (tools.ToolResult, error) {
	t.called.Store(true)
	if executor, ok := t.Tool.(interface {
		ExecuteResult(context.Context, any) (tools.ToolResult, error)
	}); ok {
		return executor.ExecuteResult(ctx, args)
	}
	output, err := t.Tool.Execute(ctx, args)
	return tools.ToolResult{Output: output}, err
}

// TestRichToolResultPreservesBlocksThroughDurableReplay covers the second A4.4
// acceptance clause at the canonical boundary: registry rich output keeps its
// ordered block types and metadata, the durable tool/result event does not
// collapse image/audio/resource blocks to text, and a fresh replay derives the
// same provider-neutral content. Transport-specific adaptations retain their
// dedicated ACP/SDK/MCP regression suites.
func TestRichToolResultPreservesBlocksThroughDurableReplay(t *testing.T) {
	registry := tools.New()
	if err := registry.Register(richContractTool{}); err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(tools.Policy{Enabled: []string{"rich_contract"}})
	result, err := registry.Execute(context.Background(), "rich_contract", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 3 {
		t.Fatalf("registry rich result = %+v err=%v, want three ordered blocks", result, err)
	}
	if result.Content[0].Kind != llm.BlockImage || result.Content[0].Image.ID != "rich-image" {
		t.Fatalf("registry image block = %+v", result.Content[0])
	}
	audio := string(result.Content[1].Raw)
	resource := string(result.Content[2].Raw)
	if result.Content[1].Kind != "audio" || !strings.Contains(audio, `"durationMs":7`) {
		t.Fatalf("registry audio block = %q/%q", result.Content[1].Kind, audio)
	}
	if result.Content[2].Kind != "resource" || !strings.Contains(resource, `"version":2`) {
		t.Fatalf("registry resource block = %q/%q", result.Content[2].Kind, resource)
	}

	source := session.New()
	if _, err := source.Append(session.EventUserMessage, session.NewUserMessage("rich")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(session.EventTurnStart, session.NewTurnStartAt(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(session.EventStepStart, session.NewStepStartAt(1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(session.EventToolCall, session.NewToolCall(1, 1, "rich-call", "rich_contract", `{}`)); err != nil {
		t.Fatal(err)
	}
	callSeq := source.Events()[len(source.Events())-1].Seq
	if _, err := source.Append(session.EventToolResult, session.NewToolResultWithContentAtSourceMeta(
		1, 1, "rich-call", "rich_contract", result.Output, result.Content, callSeq, result.Meta,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(session.EventStepEnd, session.NewStepEndAt(1, 1, "stop", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(session.EventTurnEnd, session.NewTurnEndAt(1, "stop", "")); err != nil {
		t.Fatal(err)
	}
	replayed := session.New()
	if err := replayed.Restore(source.Events()); err != nil {
		t.Fatal(err)
	}
	history := replayed.DeriveHistory()
	var replayedResult llm.Message
	for _, message := range history {
		if message.Role == llm.RoleTool {
			replayedResult = message
			break
		}
	}
	if replayedResult.Role != llm.RoleTool || len(replayedResult.Content) != 3 {
		t.Fatalf("replayed rich tool history = %+v, want one tool result with three blocks", history)
	}
	if replayedResult.Content[0].Kind != llm.BlockImage || replayedResult.Content[0].Image.ID != "rich-image" {
		t.Fatalf("replayed image block = %+v", replayedResult.Content[0])
	}
	if replayedResult.Content[1].Kind != "audio" || !strings.Contains(string(replayedResult.Content[1].Raw), `"durationMs":7`) {
		t.Fatalf("replayed audio block = %+v", replayedResult.Content[1])
	}
	if replayedResult.Content[2].Kind != "resource" || !strings.Contains(string(replayedResult.Content[2].Raw), `"version":2`) {
		t.Fatalf("replayed resource block = %+v", replayedResult.Content[2])
	}
}
