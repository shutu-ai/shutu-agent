// promptmode.go — 模式预设的提示词组装 (ADR 2026-08-20-mode-presets.md
// D-MODE-3): standard 用 prompts_dir 目录加载 (现状); minimal 用固定 persona
// (对齐 dsh minimal complete:true — 单 section, 不追加其他提示文本);
// code (PTC) 在 standard 基础上注入 DSH Code Mode 的调用边界和工作目录规则。
// 工具目录由调用方 SetTools 安装, 这里不碰.
package main

import (
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/prompt"
)

// minimalPersona is the minimal preset's fixed persona (D-MODE-3): 固定、完整、
// 自包含, 不依赖 prompts_dir.
const minimalPersona = `You are a helpful software engineer assistant.`

// codeModeSection is the PTC preset's DSH-aligned direct-call rule. Native
// tools are exposed to the model through the dynamic TypeScript SDK section;
// only run_code remains a direct model tool.
const codeModeSection = "## Code Mode (PTC)\n" +
	"`run_code` is the only tool you can call directly in Code Mode. Do not call `shell`, `sh`, `pwsh`, or any other native tool directly; if a programmatic operation is needed, put it in the `run_code` call.\n\n" +
	"`run_code` executes the body of one async TypeScript function in the current session workspace. Its required arguments are `code` and `description`; top-level `await` and `return` are supported. Use `await tools.name(args)` to call the host tools declared below. For a current-directory question, call the declared `bash` or `read` tool from the program and report its result rather than guessing from the runtime process.\n\n" +
	"Use one focused TypeScript program for batched operations. Tool-call failures reject their individual promises and can be handled with `try/catch`; a program exception, timeout, cancellation, or invalid result is reported as the run_code tool failure."

const planModeSection = `## Plan mode
The user has entered planning mode. Focus on understanding the request, exploring the
available context, identifying constraints, and proposing a complete ordered plan.
Do not perform irreversible execution merely because a plan is requested. Keep the
plan concrete, with assumptions and verification steps.`

// buildPrompt assembles the per-mode system-prompt builder (D-MODE-3).
// standard (默认) → LoadDir(promptsDir); minimal → 固定 persona; code →
// LoadDir + 追加 code-mode 段. 返回的 Builder 尚未安装工具目录 (调用方 SetTools).
func buildPrompt(mode, promptsDir string) (*prompt.Builder, error) {
	if mode == config.ModeMinimal {
		return prompt.New(minimalPersona), nil
	}
	b, err := prompt.LoadDir(promptsDir)
	if err != nil {
		return nil, err
	}
	if mode == config.ModeCode {
		b.Add(prompt.Section{Name: "code-mode", Order: 1000, Text: codeModeSection})
	}
	return b, nil
}
