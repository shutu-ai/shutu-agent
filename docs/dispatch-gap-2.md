# GAP-2 派发：ralph（fresh-agent 循环）

> 标准模式缺口 ADR `docs/decisions/2026-08-20-standard-gaps.md`（D-GAP-3）。本文件是 **GAP-2** 契约：`internal/ralph` fresh-agent 循环引擎 + `ralph` 工具 + config cap + cmd/pa 接线 + 测试。对齐 dsh `tool-ralph`。

## 纪律

- 零新依赖、CGO-free；只动 internal/ralph（新建）、internal/config、cmd/pa、config.yaml；gofmt；不改 loop。
- 默认关（D10）：`ralph.enabled` 默认 false；未启用不注册不白名单；**minimal 分支关闭**。
- 提交 1 个：`GAP-2: ralph fresh-agent 循环（internal/ralph + ralph 工具 + config + 接线）`

## 已知现状（实施时通读对应区域）

- 子代理 API：`internal/subagent` — `Runtime.Start(ctx, name, StartRequest) (*Run, error)`；`Run.Result(ctx) (Result, error)`；`Result{Output, StopReason}`；`StartRequest{Prompt, ParentSessionID, ...}`（service.go）。组合根持有 `a.subagents subagent.Runtime`（M5b-2 已接线）。
- 工具模式照 `internal/eval/tools.go`（EvalTools 持有 eng + onEvent）或 `internal/fs/tools.go`。D3 事件照 jobs/eval emit 模式。
- `internal/config/config.go`：Config struct、applyDefaults 白名单 append 模式、**Mode-1 minimal 分支**（需加 `cfg.Ralph.Enabled = false`）。
- 子代理父会话：`StartRequest.ParentSessionID` — 组合根把当前 session id 传给它（照 cmd/pa/subagent.go 现有 spawn 工具的传法）。

## 变更清单（精确）

### 1. internal/ralph/ralph.go（新建，包 internal/ralph）
```go
// Package ralph runs a bounded fresh-agent loop over an immutable objective
// (D-GAP-3, 对齐 dsh tool-ralph). Each round spawns a brand-new subagent with
// no session inheritance; only a bounded structured report crosses rounds, and
// the shared workspace (disk/project files) is the long-term memory. A worker
// reports completion, a concrete blocker, or round-limit exhaustion. The engine
// only orchestrates — the actual work happens inside the spawned subagents.
package ralph
```
- 类型与接口：
```go
// Spawn is the subagent-launch capability the engine depends on (D2: 组合根注
// 入闭包). It starts one fresh subagent with the given prompt and returns its
// terminal output text. Prompts and outputs are plain strings; the engine never
// sees subagent internals.
type Spawn func(ctx context.Context, prompt string) (string, error)

// Report is the bounded result of one loop run (D-GAP-3: 只携带摘要, 不携带
// 全量对话).
type Report struct {
	Objective   string
	MaxRounds   int
	Rounds      int      // rounds actually spawned
	Done        bool     // a worker reported DONE
	Blocked     bool     // a worker reported BLOCKED
	BlockReason string   // set when Blocked
	Final       string   // final deliverable (when Done)
	RoundBriefs []string // one bounded brief per round (progress or final)
}

// Engine runs the loop.
type Engine struct {
	spawn Spawn
}

// NewEngine returns an engine bound to a spawn capability. A nil spawn is
// rejected at use time (NewEngine errors).
func NewEngine(spawn Spawn) (*Engine, error)

// Run executes the fresh-agent loop up to maxRounds (<=0 → DefaultMaxRounds).
// It returns an error only for engine-level failures (spawn capability errors
// that are not worker reports); worker BLOCKED/DONE are normal outcomes.
func (e *Engine) Run(ctx context.Context, objective string, maxRounds int) (Report, error)
```
- 常量与协议：
```go
// DefaultMaxRounds is the loop cap when max_rounds is absent (D-GAP-3).
const DefaultMaxRounds = 3

// Round protocol markers (D-GAP-3): a worker's final reply starting with
// "DONE: " means the objective is met; "BLOCKED: " means an impassable
// concrete blocker; otherwise the reply is treated as a progress report and
// the loop continues.
const (
	MarkerDone    = "DONE: "
	MarkerBlocked = "BLOCKED: "
)
```
- worker prompt 构造（严格，实施照此文本）：
```go
// buildWorkerPrompt renders the round prompt: the immutable objective + the
// previous round's bounded brief (or "（无）" for round one). Fresh agents share
// no session, so only this text carries history across rounds; the workspace on
// disk is the shared long-term memory.
func buildWorkerPrompt(objective string, round int, prevBrief string) string
```
  prompt 内容（契约原文）：
```
你是 github.com/shutu-ai/shutu-agent 的 fresh 工作代理。目标（不可变）：
<objective>

这是第 <round> 轮。上一轮进展摘要（第一轮为「无」）：
<prevBrief 或 "（无）">

你与前几轮没有共享会话——你只能看到上面的目标与上一轮摘要。工作区（磁盘/项目
文件）是跨轮共享的长期记忆，请基于它的当前状态继续推进。

最终回复的判定规则：
- 若你已达成目标：第一行以 "DONE: " 开头，随后给出最终交付摘要。
- 若你遇到无法逾越的具体阻塞（缺凭证、需要人工、外部依赖不可用等）：第一行以
  "BLOCKED: " 开头，随后给出原因。
- 否则：给出本轮进展报告（做了什么、发现什么、下一步），继续推进。
```
- 执行逻辑（严格）：
  - `maxRounds <= 0` → DefaultMaxRounds。
  - 每轮：`out, err := e.spawn(ctx, buildWorkerPrompt(...))`；err → 若 ctx.Err() 非 nil 返回 ctx.Err()，否则返回 err（引擎级失败）。
  - `trimmed := strings.TrimSpace(out)`；`HasPrefix(trimmed, MarkerDone)` → Done=true, Final=截断(剩余, 4000), append RoundBriefs=Final, 结束；`HasPrefix(trimmed, MarkerBlocked)` → Blocked=true, BlockReason=截断(剩余, 2000), append brief, 结束；否则 append 截断(整段, 4000) brief，继续下一轮。
  - 轮数到 maxRounds 未完成 → 正常结束（Done=false, Blocked=false）。
  - 有界截断 helper：`boundRunes(s string, max int) string`（>max 截断 + "…"，内部未导出）。
  - RoundBriefs 每轮最多一条，长度 ≤4000 runes。

### 2. internal/ralph/ralph_test.go
fakeSpawn（func 字段，脚本化返回序列）：
1. `TestRunDoneFirstRound`：spawn 返回 "DONE: 完成报告" → Done=true, Final 含 "完成报告", Rounds=1, RoundBriefs==1。
2. `TestRunBlocked`：返回 "BLOCKED: 缺凭证" → Blocked=true, BlockReason=="缺凭证"。
3. `TestRunProgressThenDone`：返回 ["进展一", "DONE: 最终"] → Rounds=2, Done=true, RoundBriefs==2（第一条是进展）。
4. `TestRunRoundLimit`：全部返回进展 → Rounds==3（默认上限）, Done=false。
5. `TestRunMaxRoundsParam`：maxRounds=5 + 全进展 → Rounds==5。
6. `TestRunSpawnError`：spawn 返回 error → Run 返回该 error。
7. `TestRunContextCancel`：预取消 ctx → 返回 ctx.Err()。
8. `TestBoundRunes`：>max 截断 + "…"。
9. `TestWorkerPromptContent`：buildWorkerPrompt 含 objective/轮次/prevBrief；首轮 prevBrief 为 "（无）"。
10. `TestNewEngineNilSpawn`：NewEngine(nil) → error。

### 3. internal/ralph/tools.go（新建）
- `const RalphToolName = "ralph"`；`RalphTool{eng *Engine, onEvent func(typ string, data any)}`；`NewRalphTool(eng, onEvent)`。
- tools.Tool 方法：
  - `Name()` → ralph；`Description()` → "对不可变目标运行多轮 fresh-agent 循环，返回最终报告（完成/阻塞/轮上限）"。
  - `Schema()`：
```json
{"type":"object","properties":{"objective":{"type":"string","minLength":1,"description":"immutable objective to drive the loop"},
 "max_rounds":{"type":"integer","minimum":1,"description":"loop cap (default 3)"}},
 "required":["objective"],"additionalProperties":false}
```
  - `Execute(ctx, args)`：unmarshal → objective 空拒绝 → `eng.Run(ctx, objective, maxRounds)` → 格式化报告文本（照契约格式）→ emit 事件。错误 → `fmt.Errorf("ralph: %w", err)`。
- 报告文本格式：
```
ralph: <objective 头 80 runes>…
  rounds: <N>/<max>
  outcome: done|blocked|round-limit
  final: <Final 或 BlockReason 或 "—">
  briefs:
    round 1: <brief…>
    ...
```
  （简化为：首行 outcome + rounds + final；briefs 每条一行。）
- 事件：`emit(session.EventRalphRun, session.NewRalphRun(objective, rounds, done, blocked))`（session 事件在变更 4）。
- tools_test.go：fake Engine（直接构造 ralph.Engine + fakeSpawn）经 NewRalphTool Execute 断言输出格式 + 空 objective 拒绝 + 事件 emit。

### 4. internal/session/session.go — ralph/run 事件（照 eval/run 模式）
- 常量 `EventRalphRun = "ralph/run"`（注释：log-only D3，只记元数据不落输出全文）。
- `ralphRunData{Objective string, Rounds int, Done bool, Blocked bool}` + `NewRalphRun(objective string, rounds int, done, blocked bool) any`。
- session_test.go：Append + 回读断言（照 TestEvalRunEventAppendsAndStaysOpaque）。

### 5. internal/config/config.go + config.yaml
- `RalphConfig{Enabled bool}` + `Config.Ralph RalphConfig \`yaml:"ralph"\``（Eval 后）。
- applyDefaults 白名单：`if cfg.Ralph.Enabled && !contains(...) { append "ralph" }`（照各 cap 风格）。
- **minimal 分支**：加 `cfg.Ralph.Enabled = false`。
- config.yaml：`ralph:` 段 + 注释（enabled 默认 false D10）。

### 6. cmd/pa/ralph.go（新建）+ main.go 接线 + ralph_test.go
- `registerRalph() error`（照 eval.go 模式）：
```go
// registerRalph wires the fresh-agent loop seam (D-GAP-3) when ralph.enabled
// (默认关 D10): it builds the ralph Engine over the subagent spawn capability
// and registers the ralph tool. config.applyDefaults already whitelisted the
// name. The spawn closure drives a.subagents.Start("spawn", …) with the current
// session id, awaits the run, and returns the child Output (D2 解耦: ralph 只
// 依赖字符串闭包, 不依赖 subagent 类型).
func (a *app) registerRalph() error {
	if !a.cfg.Ralph.Enabled {
		return nil
	}
	spawn := func(ctx context.Context, prompt string) (string, error) {
		run, err := a.subagents.Start(ctx, "spawn", subagent.StartRequest{Prompt: prompt, ParentSessionID: a.sessionID()})
		if err != nil {
			return "", err
		}
		res, err := run.Result(ctx)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	}
	eng, err := ralph.NewEngine(spawn)
	if err != nil { return err }
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil { fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err) }
	}
	if err := a.reg.Register(ralph.NewRalphTool(eng, onEvent)); err != nil {
		return fmt.Errorf("pa: register %s: %w", ralph.RalphToolName, err)
	}
	return nil
}
```
  （`a.sessionID()`：照 cmd/pa 里取当前 session id 的既有方式——实施时查现有 spawn 工具/loop 如何取 session id，用同一来源；无 session 上下文时 ParentSessionID="" 亦可。）
- main.go：import internal/ralph；`registerRalph()` 调用（registerSubagent 之后——依赖 a.subagents）；无 defer。
- cmd/pa/ralph_test.go：makeXxxApp + fake 子代理（照 cmd/pa/subagent_test.go 的 scriptedLLM 模式）：
  - `TestRegisterRalphDisabledRegistersNothing`（D10）。
  - `TestRegisterRalphEnabled`：enabled → 注册 ralph 工具 + 白名单含 ralph。
  - `TestRalphRunE2E`：fake LLM 让子代理输出 "DONE: 完成" → Execute `{"objective":"x"}` 含 "done" 与 "完成"；ralph/run 事件入 log。
  - `TestRalphRunBlocked`：fake LLM 输出 "BLOCKED: 缺凭证" → 含 "blocked"。

## 验证

`go build ./...` + `go test -count=1 ./internal/ralph/ ./internal/session/ ./internal/config/ ./cmd/pa/ -run 'Ralph|ralph' -v` 全 PASS 后提交；随后 `go test -count=1 ./...` 全绿确认。

## 环境

- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明（session id 取法）。不要贴代码。
