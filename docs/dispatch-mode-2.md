# Mode-2 派发：cmd/sta 模式接线（per-mode prompt 组装 + /mode 状态 + 测试）

> 模式预设接缝 ADR `docs/decisions/2026-08-20-mode-presets.md`（D-MODE-3/5）。本文件是 **Mode-2** 契约：cmd/sta 组合根按 `agent.mode` 组装提示词。前置：Mode-1 已合入（config.go 有 `Config.Mode` + 校验 + minimal 分支 + minimalEnabledTools）。

## 纪律

- 零新依赖、CGO-free；只动 cmd/sta（新建 promptmode.go + main.go 接线 + promptmode_test.go）；gofmt；不改 loop。
- standard 是默认 ⇒ 现有提示词行为零变化（`prompt.LoadDir(cfg.PromptsDir)`）。
- 提交 1 个：`Mode-2: cmd/sta 模式接线（per-mode prompt 组装 + minimal persona + code-mode 段 + 测试）`

## 已知现状（实施时通读对应区域）

- `cmd/sta/main.go:94-99`：当前 prompt 构造
  ```go
  promptBuilder, err := prompt.LoadDir(cfg.PromptsDir)
  if err != nil { fmt.Fprintln(os.Stderr, "pa:", err); os.Exit(1) }
  promptBuilder.SetTools(func() []llm.ToolSchema { return reg.Specs() })
  ```
- `app.prompt *prompt.Builder`（main.go:344）；`prompt.New(persona)` 单 section、`Builder.Add(Section{Name,Order,Text})`、`Builder.Build()`（internal/prompt/prompt.go，照 Mode-1 前已读）。
- printHelp 状态块（main.go 约 620-647，web 块结束 642-647，terminal 块后）。`case "/term"`（约 536）附近命令 switch。
- 测试模式：cmd/sta 各 *_test.go 的 makeXxxApp + captureStdout（compact_test.go 有既有 `captureStdout` helper）。

## 变更清单（精确）

### 1. cmd/sta/promptmode.go（新建）
```go
// promptmode.go — 模式预设的提示词组装 (ADR 2026-08-20-mode-presets.md
// D-MODE-3): standard 用 prompts_dir 目录加载 (现状); minimal 用固定 persona
// (对齐 dsh minimal complete:true — 单 section, 不追加其他提示文本);
// code (PTC) 在 standard 基础上注入「程序化操作 (Code Mode)」段, 提示模型
// 优先用 code_run 把多步操作写成一段程序一次执行 (D-MODE-4 诚实近似: 无 TS
// SDK, 行为层偏好). 工具目录由调用方 SetTools 安装, 这里不碰.
package main
```
- 常量：
```go
// minimalPersona is the minimal preset's fixed persona (D-MODE-3): 固定、完整、
// 自包含, 不依赖 prompts_dir.
const minimalPersona = `You are a minimal personal agent (mode: minimal).

You operate with exactly two tool families and nothing else:
- persistent shell: terminal_start / terminal_write / terminal_read / terminal_signal / terminal_stop
- file editing: fs_read / fs_write / fs_list

Do not attempt tools outside these. Keep responses brief and factual.`

// codeModeSection is the PTC preset's programmatic-operation section
// (D-MODE-4): 提示模型把可批量的多步操作合并进一次 code_run 程序.
const codeModeSection = `## 程序化操作（Code Mode）
当一次任务包含多个可批量的文件/命令操作时, 优先用 code_run 沙箱把它们写进
一段程序一次执行（如遍历文件批量处理、循环调用、组合读取+写入），而不是逐个
工具往返。仅当操作无法程序化、或单次操作依赖前一次的人工可观察结果时才逐个
调用工具。`

// buildPrompt assembles the per-mode system-prompt builder (D-MODE-3).
// standard (默认) → LoadDir(promptsDir); minimal → 固定 persona; code →
// LoadDir + 追加 code-mode 段. 返回的 Builder 尚未安装工具目录 (调用方 SetTools).
func buildPrompt(mode, promptsDir string) (*prompt.Builder, error)
```
- 实现（严格）：
  - `mode == config.ModeMinimal` → `return prompt.New(minimalPersona), nil`（不读目录）。
  - 否则 `LoadDir(promptsDir)`（err 原样返回）；若 `mode == config.ModeCode` → `b.Add(prompt.Section{Name: "code-mode", Order: 1000, Text: codeModeSection})`。
  - `mode == config.ModeStandard`（或空）→ 直接返回 LoadDir 结果。
  - （mode 合法性已由 config.Load 校验 fail-closed；这里不重复。）

### 2. cmd/sta/main.go 接线（两处）
- **prompt 构造**（94-99 处替换）：
```go
	promptBuilder, err := buildPrompt(cfg.Mode, cfg.PromptsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	promptBuilder.SetTools(func() []llm.ToolSchema { return reg.Specs() })
```
- **printHelp 状态块**：terminal 块后（或 web 块后）加：
```go
	fmt.Printf("mode: %s\n", cfg.Mode)
```
  位置：状态块里与其他 `a.cfg.X.Enabled` 行并列即可，实施时选一致位置。

### 3. cmd/sta/promptmode_test.go（新建）
1. `TestBuildPromptStandard`：mode=standard → Build() 含 prompts_dir 内容（写临时 dir 带一个 `10-persona.md`）且**不含** code-mode 段。
2. `TestBuildPromptMinimal`：mode=minimal → Build() == 固定 minimalPersona（忽略 prompts_dir——即使传一个不存在的 dir 也不报错）；不含 "Code Mode"。
3. `TestBuildPromptCode`：mode=code → Build() 含 prompts_dir persona + 含 codeModeSection 文本（"程序化操作"）且 code-mode section 在 persona 之后。
4. `TestBuildPromptCodeLoadDirError`：mode=code + promptsDir 指向一个权限拒绝/不可读路径（Windows 下可用指向文件的路径制造 ReadDir error）→ 返回 error（不吞）。

### 4. 主接线测试（cmd/sta，可并入 promptmode_test.go 或 main 相关）
- `TestModeWiring`（可选、轻量）：用既有 makeXxxApp 模式验证 `app.prompt` 按 mode 组装——minimal app 的 prompt Build() == minimalPersona；standard app == LoadDir 结果。若既有测试基建不便，则用 buildPrompt 单测覆盖（1-4 已足够），本项可省略。

## 验证

`go build ./...` + `go test -count=1 ./cmd/sta/ -run 'BuildPrompt|Mode|Prompt' -v` 全 PASS 后提交；随后 `go test -count=1 ./...` 全绿确认（默认 standard 不回归既有 prompt 测试）。

## 环境

- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明。不要贴代码。
