// promptmode.go — 模式预设的提示词组装 (ADR 2026-08-20-mode-presets.md
// D-MODE-3): standard 用 prompts_dir 目录加载 (现状); minimal 用固定 persona
// (对齐 dsh minimal complete:true — 单 section, 不追加其他提示文本);
// code (PTC) 在 standard 基础上注入「程序化操作 (Code Mode)」段, 提示模型
// 优先用 run_code 把多步操作写成一段程序一次执行 (D-MODE-4 诚实近似: 无 TS
// SDK, 行为层偏好). 工具目录由调用方 SetTools 安装, 这里不碰.
package main

import (
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/prompt"
)

// minimalPersona is the minimal preset's fixed persona (D-MODE-3): 固定、完整、
// 自包含, 不依赖 prompts_dir.
const minimalPersona = `You are a minimal personal agent (mode: minimal).

You operate with exactly two tool families and nothing else:
- PowerShell command execution: pwsh (each call runs in a fresh pwsh process — no state persists between calls; pass workdir instead of cd)
- file editing: read / write / list / edit

Do not attempt tools outside these. Keep responses brief and factual.`

// codeModeSection is the PTC preset's programmatic-operation section
// (D-MODE-4): 提示模型把可批量的多步操作合并进一次 run_code 程序.
const codeModeSection = `## 程序化操作（Code Mode）
当一次任务包含多个可批量的文件/命令操作时, 优先用 run_code 沙箱把它们写进
一段程序一次执行（如遍历文件批量处理、循环调用、组合读取+写入），而不是逐个
工具往返。仅当操作无法程序化、或单次操作依赖前一次的人工可观察结果时才逐个
调用工具。`

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
