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

// codeModeSection is the PTC preset's DSH-aligned direct-call rule. Shutu's
// current run_code substrate executes a shell program (rather than DSH's
// generated TypeScript SDK), so the prompt states that contract explicitly and
// prevents the model from inventing direct shell tool names or cwd semantics.
const codeModeSection = "## Code Mode (PTC)\n" +
	"`run_code` is the only tool you can call directly in Code Mode. Do not call `shell`, `sh`, `pwsh`, or any other native tool directly; if a programmatic operation is needed, put it in the `run_code` call.\n\n" +
	"`run_code` executes one non-interactive shell program in the current session workspace unless an explicit `cwd` is supplied. Its required arguments are `lang` (currently \"sh\") and `code`; `timeout` and `cwd` are optional. Use the command's output as the source of truth for the current directory. For a current-directory question, run `pwd`/`cd` in `run_code` and report that result without guessing from the process or sandbox directory.\n\n" +
	"Use `run_code` for batched operations and keep the program focused. A non-zero exit code or timeout is a normal tool result that should be inspected and, when safe, corrected in a follow-up call."

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
