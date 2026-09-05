# GAP-3 派发：workflow（多 agent 编排，JSON DAG）

> 标准模式缺口 ADR `docs/decisions/2026-08-20-standard-gaps.md`（D-GAP-2）。本文件是 **GAP-3** 契约：`internal/workflow` 声明式 DAG 编排 + `workflow_run` 工具 + config cap + cmd/sta 接线 + 测试。用户已拍板「JSON DAG 声明式编排」（Go 原生执行，无 JS 引擎；dsh 的 JS 脚本 hooks 用 JSON 依赖图等价表达）。

## 纪律

- 零新依赖、CGO-free；只动 internal/workflow（新建）、internal/session、internal/config、cmd/sta、config.yaml；gofmt；不改 loop。
- 默认关（D10）：`workflow.enabled` 默认 false；未启用不注册不白名单；**minimal 分支关闭**。
- 提交 1 个：`GAP-3: workflow JSON DAG 编排（internal/workflow + workflow_run 工具 + config + 接线）`

## 已知现状（实施时通读对应区域）

- 子代理 API：`internal/subagent` — `Runtime.Start(ctx, name, StartRequest) (*Run, error)`；`Run.Result(ctx) (Result, error)`；`Result{Output, StopReason}`；`StartRequest{Prompt, ParentSessionID, ...}`（service.go）。组合根持有 `a.subagents subagent.Runtime`。
- 工具/事件模式：照 internal/eval/tools.go 与 session eval/run 事件。
- `internal/config/config.go`：Config struct、applyDefaults 白名单 append、**Mode-1 minimal 分支**（加 `cfg.Workflow.Enabled = false`）。

## 变更清单（精确）

### 1. internal/workflow/workflow.go（新建，包 internal/workflow）
```go
// Package workflow orchestrates a declarative task DAG over subagents
// (D-GAP-2). The model submits tasks[] — each with an id, a prompt, and
// depends_on — and the engine topologically sorts, spawns ready tasks
// concurrently (bounded), feeds each dependent the bounded outputs of its
// dependencies, and returns a per-task report. No JS engine: the JSON DAG is
// the declarative form (用户拍板). The engine only orchestrates; the spawned
// subagents do the work.
package workflow
```
- 类型与接口：
```go
// Spawn is the subagent-launch capability (D2: 组合根注入闭包). It starts one
// fresh subagent with the given prompt and returns its terminal output text.
type Spawn func(ctx context.Context, prompt string) (string, error)

// Task is one DAG node.
type Task struct {
	ID       string   // unique node id
	Prompt   string   // the subagent task prompt
	DependsOn []string // prerequisite task IDs (may be empty)
}

// Spec is the full workflow request.
type Spec struct {
	Tasks []Task
}

// TaskStatus is one task's terminal outcome.
type TaskStatus string

const (
	StatusCompleted TaskStatus = "completed" // spawned and produced output
	StatusFailed    TaskStatus = "failed"    // spawn produced an error
)

// TaskReport is the bounded per-task result.
type TaskReport struct {
	ID     string
	Status TaskStatus
	Output string // bounded (≤4000 runes); "" on failure
	Error  string // bounded (≤2000 runes); "" on success
}

// Report is the workflow result, in dependency order (topological).
type Report struct {
	Tasks []TaskReport
}

// Engine runs DAGs.
type Engine struct {
	spawn        Spawn
	maxConcurrent int
}

// NewEngine returns an engine bound to a spawn capability with the given
// concurrency cap (<=0 → DefaultMaxConcurrent). A nil spawn is rejected.
func NewEngine(spawn Spawn, maxConcurrent int) (*Engine, error)
```
- 常量：
```go
// DefaultMaxConcurrent is the ready-task concurrency cap (D-GAP-2).
const DefaultMaxConcurrent = 4

// depOutputMax bounds the dependency-output summary fed to a dependent.
const depOutputMax = 2000 // runes per dependency
```
- 校验（fail-closed）：空 tasks → error；ID 重复 → error；`depends_on` 引用不存在的 ID → error；`Run` 前 Kahn 拓扑检测环 → `ErrCycle`。
- 执行逻辑（严格）：
  - 拓扑排序（Kahn）：indegree、邻接表；结果顺序 = 拓扑序（依赖在前）。环 → `ErrCycle`。
  - 并发执行：按拓扑层推进——每层（无未满足依赖的任务集合）用 `maxConcurrent` 并发 spawn（限流信号量）。依赖任务的输出摘要（`boundRunes(output, depOutputMax)`）按 `depends_on` 顺序附加到依赖者 prompt 末尾：
    ```
    \n\n（依赖任务输出摘要）
    <dep id>:
    <bounded output>
    ```
  - 任务 spawn error → TaskReport{Status: StatusFailed, Error: bound(error, 2000)}；其依赖者仍执行（prompt 注「依赖 <id> 失败」）。
  - ctx 取消：取消信号量等待/进行中的 spawn；返回 ctx.Err()（已完成的 TaskReport 保留在 Report 中——实施时决定：ctx 取消 → 返回 `Report{...已完成...}, ctx.Err()`，让调用方可部分恢复）。
  - `boundRunes(s, max)` 内部 helper（>max 截断 + "…"）。
  - `ErrCycle = errors.New("workflow: dependency cycle")`。

### 2. internal/workflow/workflow_test.go
fakeSpawn（func 字段；可记录并发峰值——用原子计数 + channel 探针）：
1. `TestRunLinear`：A→B→C 依赖链 → Report 顺序 A,B,C，B 的 prompt 含 A 输出摘要，C 含 B。
2. `TestRunFanOut`：A→{B,C} → B、C 并发（各含 A 摘要）。
3. `TestRunIndependentParallel`：A、B 无依赖 → 两任务并发（fakeSpawn 并发探针断言峰值 ≥2）。
4. `TestRunCycle`：A→B→A → ErrCycle。
5. `TestRunUnknownDep`：depends_on 引用不存在 ID → error。
6. `TestRunDuplicateID` → error。
7. `TestRunTaskFailure`：B spawn error → B StatusFailed + Error；依赖 B 的 C 仍完成且 prompt 注「依赖 B 失败」。
8. `TestRunContextCancel`：预取消 → ctx.Err()（已完成任务保留在 Report）。
9. `TestMaxConcurrentCap`：maxConcurrent=1 → 并发峰值 ≤1。
10. `TestNewEngineNilSpawn` → error。
11. `TestBoundRunes`。

### 3. internal/workflow/tools.go（新建）
- `const WorkflowRunToolName = "workflow_run"`；`WorkflowRunTool{eng *Engine, onEvent}`；`NewWorkflowRunTool(eng, onEvent)`。
- tools.Tool：
  - `Name()` → workflow_run；`Description()` → "提交任务 DAG，并发编排多个子代理（依赖在先后执行），返回逐任务结果"。
  - `Schema()`：
```json
{"type":"object","properties":{"tasks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","minLength":1},"prompt":{"type":"string","minLength":1},"depends_on":{"type":"array","items":{"type":"string"}}},"required":["id","prompt"],"additionalProperties":false},"minItems":1,"description":"task DAG nodes (unique ids; depends_on lists prerequisite ids)"}},
 "required":["tasks"],"additionalProperties":false}
```
  - `Execute(ctx, args)`：unmarshal → `eng.Run(ctx, Spec{Tasks})` → 格式化逐任务报告：
```
workflow_run: <N> tasks
  <id>: completed|failed
    output: <bound 400>
    error: <…>
```
    每任务输出/错误有界展示（400 runes）；事件 emit。错误 → `fmt.Errorf("workflow_run: %w", err)`（ErrCycle 等直接透传）。
  - 事件：`emit(session.EventWorkflowRun, session.NewWorkflowRun(total, completed, failed))`。
- tools_test.go：fakeSpawn 构造 Engine → Execute 断言格式 + 环 → error + 空 tasks 拒绝 + 事件 emit。

### 4. internal/session/session.go — workflow/run 事件
- 常量 `EventWorkflowRun = "workflow/run"`（注释：log-only D3，只记元数据不落输出全文）。
- `workflowRunData{Total, Completed, Failed int}` + `NewWorkflowRun(total, completed, failed int) any`。
- session_test.go：Append + 回读断言。

### 5. internal/config/config.go + config.yaml
- `WorkflowConfig{Enabled bool, MaxConcurrent int}` + `Config.Workflow WorkflowConfig \`yaml:"workflow"\``（Ralph 后）。
- 默认：MaxConcurrent <=0 → DefaultWorkflowMaxConcurrent（=4，常量与 workflow.DefaultMaxConcurrent 同步；实施时用 `if cfg.Workflow.MaxConcurrent <= 0 { cfg.Workflow.MaxConcurrent = 4 }`）。
- applyDefaults 白名单：`if cfg.Workflow.Enabled && !contains(...) { append "workflow_run" }`。
- **minimal 分支**：加 `cfg.Workflow.Enabled = false`。
- config.yaml：`workflow:` 段（enabled 默认 false D10、max_concurrent 默认 4）。

### 6. cmd/sta/workflow.go（新建）+ main.go 接线 + workflow_test.go
- `registerWorkflow() error`（照 eval.go 模式）：
```go
// registerWorkflow wires the task-DAG orchestration seam (D-GAP-2) when
// workflow.enabled (默认关 D10): Engine over the subagent spawn capability +
// workflow_run tool. config.applyDefaults already whitelisted the name.
func (a *app) registerWorkflow() error {
	if !a.cfg.Workflow.Enabled {
		return nil
	}
	spawn := func(ctx context.Context, prompt string) (string, error) {
		run, err := a.subagents.Start(ctx, "spawn", subagent.StartRequest{Prompt: prompt, ParentSessionID: a.sessionID()})
		if err != nil { return "", err }
		res, err := run.Result(ctx)
		if err != nil { return "", err }
		return res.Output, nil
	}
	eng, err := workflow.NewEngine(spawn, a.cfg.Workflow.MaxConcurrent)
	if err != nil { return err }
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil { fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err) }
	}
	if err := a.reg.Register(workflow.NewWorkflowRunTool(eng, onEvent)); err != nil {
		return fmt.Errorf("pa: register %s: %w", workflow.WorkflowRunToolName, err)
	}
	return nil
}
```
- main.go：import internal/workflow；`registerWorkflow()` 调用（registerSubagent 之后）；无 defer。
- cmd/sta/workflow_test.go：
  - `TestRegisterWorkflowDisabledRegistersNothing`（D10）。
  - `TestRegisterWorkflowEnabled`：enabled → 注册 + 白名单含 workflow_run。
  - `TestWorkflowRunE2E`：fake LLM 两任务（无依赖并发）→ Execute 含两个 completed；workflow/run 事件入 log（total=2）。
  - `TestWorkflowRunCycleE2E`：A→B→A → Execute 返回含 "cycle" 的 error。

## 验证

`go build ./...` + `go test -count=1 ./internal/workflow/ ./internal/session/ ./internal/config/ ./cmd/sta/ -run 'Workflow|workflow' -v` 全 PASS 后提交；随后 `go test -count=1 ./...` 全绿确认。

## 环境

- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明（session id 取法、并发实现方式）。不要贴代码。
