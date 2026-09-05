# GAP-4 派发：subagent 外部 provider 变体（codex / claude-code，可选默认关）

> 标准模式缺口 ADR `docs/decisions/2026-08-20-standard-gaps.md`（D-GAP-4）。本文件是 **GAP-4** 契约：`internal/subagent` 外部 provider + `subagent_spawn` 加 provider 字段 + config + cmd/sta 接线 + 测试。用户已拍板「可选、默认关、CLI 探测」。

## 纪律

- 零新依赖、CGO-free；只动 internal/subagent、internal/config、cmd/sta、config.yaml；gofmt；不改 loop。
- 默认关（D10）：外部 provider 未启用不注册；`subagent_spawn` 的 provider 字段缺省 `spawn`；未启用/未知 provider → fail-closed（不静默回退本地）。
- 提交 1 个：`GAP-4: subagent 外部 provider（codex/claude-code exec + provider 字段 + config + 接线）`

## 已知现状（实施时通读对应区域）

- `internal/subagent`：`Provider` 接口（Name/Capabilities/Start(ctx, StartRequest) (*Run, error)）；`Runtime.Start(ctx, name, req)` 按名委托并校验 capabilities；`Run{ID, Result func(ctx) (Result, error), Cancel func(reason) error}`；`Result{Output, StopReason}`；`childRun` 模式（spawn.go 55-134：id 自增、settle 首胜、done channel）；`withAcceptance(req.Prompt, req.AcceptanceCriteria)`（spawn.go:97，把验收标准注入 prompt——检查是否导出，未导出则外部 provider 内联同等逻辑）。
- `internal/subagent/tools.go`：`SubagentSpawnTool.Execute` 用 `defaultProviderName` 硬编码（tools.go:236）——需改为读 schema 的 provider 字段。`defaultProviderName` 常量（照查 spawn.go 或 service.go）。
- `internal/subagent/service.go`：Capabilities{OutputSchema, DepthLimit, ToolFilter, Persona}；哨兵 error 集合。
- `internal/config/config.go`：`SubagentConfig`（读现有字段：max_depth / default_provider / ...）；applyDefaults；Mode-1 minimal 分支（subagent 已关闭，无需加——但确认 `cfg.Subagent.Enabled = false` 已在 minimal 分支；外部 provider 开关随 subagent cap 一起被 minimal 关闭）。
- `cmd/sta/subagent.go`：registerSubagent（注册 SpawnProvider + SubagentTools + D3 事件）。

## 变更清单（精确）

### 1. internal/subagent/external.go（新建）
```go
// external.go — external subagent providers (D-GAP-4): codex / claude-code
// spawn the external CLI in a child process, bridge the prompt over stdin and
// the output from stdout, and report the exit as completed/error. The
// capability is optional and default off: the composition root registers a
// provider only for an enabled, configured external command; a missing binary
// fails closed at Start (no silent fallback to the local provider). The
// provider is honest about capabilities: it enforces none of the harness's
// depth/filter/persona semantics (the CLI owns its own behavior).
package subagent
```
- 类型：
```go
// ExternalProvider is one external-CLI subagent backend.
type ExternalProvider struct {
	name    string // provider name ("codex" / "claude-code" / config key)
	command string // CLI binary; LookPath'd at Start (fail-closed)
	args    []string // CLI arguments for the headless one-shot mode
}

// NewExternalProvider returns a provider for an external one-shot CLI. For the
// two known names the headless arguments are preset: codex → ["exec","--json"],
// claude-code → ["-p"]; any other name runs the binary bare (stdin=prompt,
// stdout=output).
func NewExternalProvider(name, command string) *ExternalProvider
```
- 行为（严格）：
  - `Name() string` → name。
  - `Capabilities() Capabilities` → `Capabilities{}`（全 false——诚实声明：外部 CLI 由各自行为决定，本 provider 不强制执行深度/工具过滤/persona）。
  - `Start(ctx, req) (*Run, error)`：
    - `req.Prompt == ""` → `ErrInvalidRequest`。
    - `ctx.Err()` 非 nil → 返回。
    - `exec.LookPath(command)` 失败 → error：`fmt.Errorf("%w: external provider %q: command %q not found", ErrInvalidRequest, name, command)`（fail-closed；不发本地回退）。
    - prompt 应用 acceptance 注入（`req.Prompt = withAcceptance(req.Prompt, req.AcceptanceCriteria)`，若该 helper 未导出则内联同等逻辑：空 criteria 跳过、非空则追加验收标准段）。
    - 构造 `exec.CommandContext(ctx, command, args...)`：`Stdin = strings.NewReader(req.Prompt)`；`Stdout`/`Stderr` 用 pipe（stderr 丢弃或小缓冲，不进 Result）。
    - id 自增：`fmt.Sprintf("%s-%d", name, n)`（provider 内互斥计数）。
    - 后台 goroutine：`cmd.Run()` 等待退出 → settle `Result{Output: stdout 全文, StopReason: exit==0 ? StopCompleted : StopError}`；`cmd.Start` 失败 → settle `Result{StopReason: StopError}`（Output 空）。
    - 返回 `Run{ID, Result: 阻塞 settle, Cancel: 进程 Kill（幂等）}`。
    - `ctx` 取消：CommandContext 在 ctx 取消时杀进程 → Run 返回 ctx 取消。
  - 不实现 `closer`/`childrenLister`（无跟踪 registry；进程由 Result await 回收，生命周期可逆——组合根 Close 时未 await 的进程由进程自然结束；文档诚实记录）。

### 2. internal/subagent/external_test.go
- fake 外部命令用「当前测试二进制 + env 标志」模式（标准 Go 跨平台 helper）：
  - `TestExternalHelperProcess`：`if os.Getenv("GO_WANT_EXTERNAL_HELPER") != "1" { return }` 分支——读全部 stdin → 写回 stdout（模拟 CLI 回显 prompt）；供主测试 `exec.Command(os.Args[0], "-test.run=TestExternalHelperProcess")` + env 注入。
- 用例：
  1. `TestExternalProviderEcho`：NewExternalProvider("fake", 当前测试二进制 + helper 参数) → Start → Result.Output == 注入的 prompt 全文、StopReason == StopCompleted。
  2. `TestExternalProviderMissingBinary`：command 一个不存在的路径 → Start 返回 error 且含 "not found"（fail-closed）。
  3. `TestExternalProviderEmptyPrompt` → ErrInvalidRequest。
  4. `TestExternalProviderArgsPreset`：name=codex → 参数含 "exec"；name=claude-code → 参数含 "-p"（检查 internal args 字段——用同包测试直接读字段）。
  5. `TestExternalProviderCancel`：预取消 ctx → Start 返回 ctx.Err()（或 Result 返回 ctx 取消——实施时断言其一，行为：CommandContext 取消）。
  6. `TestExternalProviderAcceptanceInjected`：AcceptanceCriteria 非空 → prompt 注入验收标准段（Start 后无法直接看 prompt——用 echo 返回的 Output 断言含 "验收标准"）。

### 3. internal/subagent/tools.go — subagent_spawn 加 provider 字段
- Schema properties 加：
```go
"provider": map[string]any{
	"type":        "string",
	"enum":        []string{"spawn", "codex", "claude-code"},
	"description": "subagent provider: spawn (default, local) | codex | claude-code (external CLI; must be enabled in config)",
},
```
- Execute：struct 加 `Provider string \`json:"provider"\``；`provider := a.Provider; if provider == "" { provider = defaultProviderName }`；`t.t.rt.Start(ctx, provider, ...)`（替换硬编码 defaultProviderName）；`register(...)` 的 provider 字段用实际 provider；emit/输出文本用实际 provider。
- 未知/未启用 provider：Runtime.Start 对未注册 name 返回 `ErrUnknownProvider` → Execute 透传（`fmt.Errorf("subagent_spawn: %w", err)`）——fail-closed，无回退。
- tools_test.go：加 `TestSpawnToolProviderField`——`provider: "spawn"` 显式传等同缺省；`provider: "codex"`（未注册）→ error 含 "unknown provider"（用不含外部 provider 的测试 Runtime）。

### 4. internal/config/config.go + config.yaml
- `SubagentConfig` 加字段（读现有结构后追加）：
```go
	// ExternalProviders declares optional external subagent backends
	// (D-GAP-4): keyed by provider name, each with an optional enable flag
	// (default false) and the CLI command (empty → per-name default:
	// codex→"codex", claude_code→"claude"). A provider is registered only
	// when Enabled is true; an enabled provider whose binary is missing fails
	// closed at Start. All default off (D10).
	ExternalProviders map[string]ExternalProviderConfig `yaml:"external_providers"`
```
- `ExternalProviderConfig{Enabled bool, Command string}`。
- applyDefaults：遍历 ExternalProviders，`Command == ""` 时按 key 填默认（codex→"codex"、claude_code→"claude"；其他 key 留空则组合根按 name 原样 LookPath）。
- config.yaml `subagent:` 段加：
```yaml
    # 外部子代理后端 (可选, 默认关 D10): codex → `codex exec --json`,
    # claude_code → `claude -p`. command 缺省即同名命令. CLI 不存在时
    # subagent_spawn provider=codex 报错 (fail-closed, 不回退本地).
    external_providers:
      codex:
        enabled: false
        command: codex
      claude_code:
        enabled: false
        command: claude
```
- config_test.go：`TestExternalProviderDefaults`——配置 codex 空 command → applyDefaults 后 "codex"；未启用默认 false。

### 5. cmd/sta/subagent.go 接线
- `registerSubagent` 内（注册 SpawnProvider 后、或单独 loop）：遍历 `a.cfg.Subagent.ExternalProviders`，`Enabled` 的 → `a.subagents.RegisterProvider(subagent.NewExternalProvider(name, command))`；`RegisterProvider` 失败（重复名）→ 返回 error（fail-closed）。command 取 `cfg.Command`（applyDefaults 已填默认）。
- main.go：无需新调用（并入 registerSubagent）。
- cmd/sta/subagent_test.go（或新 external 接线测试）：
  - `TestRegisterSubagentExternalProviders`：enabled codex + command 指向 fake helper → RegisterProvider 后 `ListProviders()` 含 "codex"。
  - `TestRegisterSubagentExternalDisabled`：disabled → ListProviders 不含（D10）。

## 验证

`go build ./...` + `go test -count=1 ./internal/subagent/ ./internal/config/ ./cmd/sta/ -run 'External|Provider|Spawn|Subagent' -v` 全 PASS 后提交；随后 `go test -count=1 ./...` 全绿确认。

## 环境

- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明（fake 命令实现、defaultProviderName 位置、withAcceptance 是否导出）。不要贴代码。
