# Eval-3b 派发：cmd/pa 评测接线（registerEval + LLM judge 闭包 + 人工回退闭包 + /eval-status + 测试）

> 评测接缝 ADR `docs/decisions/2026-08-20-eval-seam.md` D-EVAL-2/3/7。本文件是 **Eval-3b**（收尾）契约：组合根接线——registerEval、LLM judge 闭包（用 `a.llm`）、人工回退闭包（用 `a.interacts`）、`/eval-status` 命令、printHelp、生命周期、接线测试 + 冒烟。前置：Eval-1/2/3a 全部已交付。

## 纪律

- 零新依赖、CGO-free；只动 cmd/pa（eval.go 新建 + main.go 接线 + eval_test.go 新建）；gofmt；不改 loop；默认关（D10）。
- 提交 1 个：`Eval-3b: cmd/pa 评测接线（registerEval + LLM judge + 人工回退 + /eval-status + 测试）`

## 已知 API（已交付，勿重复读）

- `internal/eval`：`NewEngine(opts EngineOpts{Evaluator, MaxRecords}) (Engine, error)`；`Engine.Evaluate(ctx, taskID, output, criteria) (EvalRecord, error)` / `List` / `Get` / `Close`；`EvalRecord{ID, TaskID, Criteria, Output, Verdict, Reason, EvaluatorKind, CreatedAt}`；`VerdictPass/Fail/Manual`；`CompositeEvaluator{Rule, LLM, Manual Evaluator; ManualFallback bool}`（三者都实现 `Evaluate(ctx, output, criteria) (Verdict, string, string, error)`）；`RuleEvaluator{}`；`LLMEvaluator{Judge JudgeFunc}`；`ManualEvaluator{Manual ManualFunc}`；`JudgeFunc func(ctx, output string, llmCriteria []string) (Verdict, string, error)`；`ManualFunc func(ctx, taskID, output string, manualCriteria []string) (Verdict, string, error)`；`NewEvalTools(eng Engine, onEvent) *EvalTools` + `Run()/Result()/List()`（各实现 tools.Tool）。
- `internal/interact`：`Engine.Request(ctx, prompt, toolName, args string) (Request, error)`；`Await(ctx, id) (Request, error)`；`StatusApproved/StatusRejected/StatusPending` 常量。
- `internal/llm`：`LLM.Stream(ctx, ChatRequest{Model, Messages}) (StreamReader, error)`；`Message{Role, Content []llm.ContentBlock}`；`llm.Text(s)`；`llm.RoleSystem/RoleUser`；`StreamTextDelta`；读取流照 compaction.basic.go summarize（for { ev, err := reader.Next(); io.EOF → break; ev.Kind == StreamTextDelta → 拼 }）。
- `cmd/pa/llm.go`：`llmProviderModel(cfg, id) string` 存在。
- main.go 注册序列：`registerInteracts()` 在 312 行（registerTerminal 294 之后）；`app.llm llm.LLM`（332 行）；`app.interacts interact.Engine`（338 附近，nil when interact disabled）；`a.log *session.Log`。

## 交付 1：cmd/pa/eval.go（package main）

### registerEval
```go
// registerEval wires the task-evaluation seam (ADR 2026-08-20-eval-seam.md)
// when eval.enabled (默认关 D10): it builds the CompositeEvaluator (rule
// assertions → LLM judge → human fallback, D-EVAL-3) over the app's LLM and
// interact engines, creates the eval Engine, registers the three eval_* tools,
// and wires the D3 event sink so eval/run lands in the active session log.
// config.applyDefaults already whitelisted the eval_* names when eval.enabled
// was true. The engine is in-memory (no persisted history) and its Close is
// idempotent. The loop's turn/step structure is untouched (D4): evaluation
// runs on the serial tool path.
func (a *app) registerEval() error {
	if !a.cfg.Eval.Enabled {
		return nil
	}
	manualFallback := true
	if a.cfg.Eval.ManualFallback != nil {
		manualFallback = *a.cfg.Eval.ManualFallback
	}
	composite := eval.CompositeEvaluator{
		Rule:           eval.RuleEvaluator{},
		LLM:            eval.LLMEvaluator{Judge: a.evalJudge()},
		Manual:         eval.ManualEvaluator{Manual: a.evalManual()},
		ManualFallback: manualFallback,
	}
	eng, err := eval.NewEngine(eval.EngineOpts{Evaluator: composite, MaxRecords: a.cfg.Eval.MaxRecords})
	if err != nil {
		return fmt.Errorf("pa: eval engine: %w", err)
	}
	a.evalEng = eng
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	et := eval.NewEvalTools(eng, onEvent)
	for _, t := range []tools.Tool{et.Run(), et.Result(), et.List()} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("pa: register %s: %w", t.Name(), err)
		}
	}
	return nil
}
```

### evalJudge（LLM judge 闭包）
```go
// evalJudgeSystemPrompt asks the judge model to return JSON only.
const evalJudgeSystemPrompt = "You are a rigorous evaluator. Given a deliverable and acceptance criteria, judge whether the deliverable satisfies them. Respond with JSON only: {\"verdict\": \"pass\"|\"fail\"|\"manual\", \"reason\": \"one-line justification\"}."

// judgeOutputMax bounds the deliverable head sent to the judge (D-EVAL-3).
const judgeOutputMax = 6000

// evalJudge adapts the app's LLM to the eval seam's JudgeFunc (D-EVAL-3). It
// sends a single non-streaming-style request (no tools ⇒ plain stream) and
// parses the model's JSON verdict, tolerantly mapping unrecognized output to
// manual.
func (a *app) evalJudge() eval.JudgeFunc {
	model := llmProviderModel(a.cfg, a.cfg.LLM.Provider)
	return func(ctx context.Context, output string, llmCriteria []string) (eval.Verdict, string, error) {
		head := runeHead(output, judgeOutputMax)
		user := "Deliverable:\n" + head + "\n\nAcceptance criteria to judge:\n" + strings.Join(llmCriteria, "\n")
		msgs := []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(evalJudgeSystemPrompt)}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(user)}},
		}
		reader, err := a.llm.Stream(ctx, llm.ChatRequest{Model: model, Messages: msgs})
		if err != nil {
			return eval.VerdictManual, "", err
		}
		var b strings.Builder
		for {
			ev, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return eval.VerdictManual, "", err
			}
			if ev.Kind == llm.StreamTextDelta {
				b.WriteString(ev.Text)
			}
		}
		var parsed struct {
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		}
		text := strings.TrimSpace(b.String())
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			// Tolerant fallback: scan for a verdict keyword.
			switch {
			case strings.Contains(text, `"fail"`):
				parsed.Verdict = "fail"
			case strings.Contains(text, `"pass"`):
				parsed.Verdict = "pass"
			default:
				parsed.Verdict = "manual"
			}
		}
		switch parsed.Verdict {
		case "pass":
			return eval.VerdictPass, parsed.Reason, nil
		case "fail":
			return eval.VerdictFail, parsed.Reason, nil
		default:
			return eval.VerdictManual, parsed.Reason, nil
		}
	}
}

// runeHead returns the first max runes of s (append "…" when cut).
func runeHead(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
```

### evalManual（人工回退闭包）
```go
// evalManual adapts the app's interact engine to the eval seam's ManualFunc
// (D-EVAL-7): an undecidable evaluation becomes an interact approval request
// (approved→pass, rejected→fail). When interact is disabled the fallback
// reports manual (undecided) rather than failing.
func (a *app) evalManual() eval.ManualFunc {
	return func(ctx context.Context, taskID, output string, manualCriteria []string) (eval.Verdict, string, error) {
		if a.interacts == nil {
			return eval.VerdictManual, "interact disabled: no human fallback", nil
		}
		promptText := "无法自动判定以下交付是否满足验收标准，请人工审批。\n"
		if taskID != "" {
			promptText += "任务：" + taskID + "\n"
		}
		promptText += "验收标准：\n" + strings.Join(manualCriteria, "\n") + "\n交付摘要：\n" + runeHead(output, 2000)
		req, err := a.interacts.Request(ctx, promptText, "eval_manual", runeHead(output, 2000))
		if err != nil {
			return eval.VerdictManual, "", err
		}
		res, err := a.interacts.Await(ctx, req.ID)
		if err != nil {
			return eval.VerdictManual, "", err
		}
		switch res.Status {
		case interact.StatusApproved:
			return eval.VerdictPass, "approved by human", nil
		case interact.StatusRejected:
			return eval.VerdictFail, "rejected by human", nil
		default:
			return eval.VerdictManual, "no human decision", nil
		}
	}
}
```

### evalStatus（/eval-status 命令）
```go
// evalStatus prints the eval seam configuration and history summary.
func (a *app) evalStatus() error {
	if a.evalEng == nil {
		fmt.Println("eval: disabled (eval.enabled=false)")
		return nil
	}
	recs, err := a.evalEng.List(context.Background())
	if err != nil {
		return err
	}
	manual := true
	if a.cfg.Eval.ManualFallback != nil {
		manual = *a.cfg.Eval.ManualFallback
	}
	fmt.Printf("eval: enabled (records=%d, max_records=%d, manual_fallback=%v)\n",
		len(recs), a.cfg.Eval.MaxRecords, manual)
	return nil
}
```

## 交付 2：main.go 接线（四处）

1. **app 字段**（`web *web.Engine` 或 terminal 字段后加）：
```go
	// evalEng is the task-evaluation engine (ADR 2026-08-20-eval-seam.md);
	// nil when eval disabled (D10).
	evalEng eval.Engine
```
（imports 加 `github.com/shutu-ai/shutu-agent/internal/eval`）
2. **注册调用**（`registerInteracts()` 块之后加）：
```go
	// eval: wire the task-evaluation seam — the CompositeEvaluator (rule → LLM
	// judge → human fallback) over a.llm/a.interacts + the three eval_* tools +
	// the /eval-status command + the D3 event sink — when eval.enabled (默认关
	// D10). config.applyDefaults already whitelisted the eval_* names when
	// eval.enabled was true. The engine is in-memory; Close is idempotent.
	if err := app.registerEval(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.evalEng != nil {
		defer app.evalEng.Close()
	}
```
3. **command switch**（`/term` case 后加）：
```go
	case "/eval-status":
		return a.evalStatus()
```
4. **printHelp**：命令表 `/term ...` 行后加 `  /eval-status       task-evaluation status (eval)`；状态块 terminal 块后加：
```go
	if a.cfg.Eval.Enabled {
		fmt.Printf("eval: enabled (max_records=%d)\n", a.cfg.Eval.MaxRecords)
	} else {
		fmt.Println("eval: disabled (eval.enabled=false)")
	}
```

## 交付 3：cmd/pa/eval_test.go（接线测试 + 冒烟）

模式照 jobs_test.go（makeXxxApp + 白名单 policy）。fakeLLM 照 subagent spawn_test 的 scriptedLLM（实现 llm.LLM.Stream，返回固定事件或捕获 req）。fakeInteract 实现 interact.Engine（Request 记录 + 预置 Approved/Rejected 状态 + Await 立即返回）。

用例：
1. `TestRegisterEvalDisabledRegistersNothing`：makeEvalApp(false) → registerEval()；evalEng nil；无 eval_* 工具注册（D10）。
2. `TestRegisterEvalEnabledRegistersTools`：makeEvalApp(true)（cfg Eval{Enabled:true, MaxRecords:10, ManualFallback:&true} + llm: fakeLLM + interacts: fakeInteract）→ registerEval()；3 个 eval_* 在 Specs。
3. `TestEvalRunRulePass`（冒烟，无 LLM 调用）：registerEval 后经 eval_run 工具 Execute `{task_id:"t-1", output:"报告已产出", criteria:["contains:报告"]}` → 返回含 "pass"；eval/run 事件入 log（EventEvalRun 类型 + payload verdict=="pass"）。
4. `TestEvalRunRuleFail`：criteria `["contains:不存在的内容"]` → 返回含 "fail"。
5. `TestEvalRunLLMJudge`：fakeLLM 返回 JSON `{"verdict":"pass","reason":"ok"}` → criteria `["llm:结论合理"]` → eval_run 返回含 "pass" 且 fakeLLM.calls 数为 1（LLM 被调用）。
6. `TestEvalRunManual`：criteria `["manual:人工确认"]` + fakeInteract 预置 Approved → eval_run 返回含 "pass"（approved→pass）且 fakeInteract 收到 toolName=="eval_manual"；预置 Rejected → fail。
7. `TestEvalStatus`：/eval-status handler（evalStatus()）enabled 打印含 "enabled"、disabled 打印含 "disabled"（capture stdout 或直接调 evalStatus 断言不 error + 状态字段——可改用 `app.evalEng != nil` 断言 + 文本用 buffer 捕获）。
8. `TestEvalResultList`：eval_run 两次后 eval_list 返回含两条记录（含 "eval eval-2" 最前）；eval_result 按 id 返回单条。

## 纪律

- fakeLLM/fakeInteract 只实现所需方法（llm.LLM 只有 Stream；interact.Engine 有 Request/Resolve/Await/List/Close——fake 全实现，Request 返回预置状态、Await 立即返回）。
- 测试断言宽松（Contains），不放宽到必过；flaky 先重跑 2 次再修。
- `go build ./...` + `go test -count=1 ./cmd/pa/ -run 'Eval|RegisterEval' -v` 全 PASS 后提交；随后 `go test -count=1 ./...` 全绿确认。

## 环境

- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明。不要贴代码。
