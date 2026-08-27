// Package config loads the runtime configuration from config.yaml (design.md
// §2). API keys are never part of configuration: they only ever come from the
// environment (design.md §6, Agent.md §5.6), so this file never contains them.
package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied to fields that are empty or absent in config.yaml.
const (
	DefaultModel      = "deepseek-v4-flash"
	DefaultBaseURL    = "" // empty => provider default (https://api.deepseek.com)
	DefaultDataDir    = "data"
	DefaultPromptsDir = "config/prompts"

	// M3 tool-execution defaults (design.md §5 / dispatch-m3): the default
	// whitelist holds only the read-only tools; the per-tool execute deadline
	// is 30s; tool output over 64KB is truncated and spilled.
	DefaultToolTimeout = 30 * time.Second
	DefaultOutputLimit = 64 * 1024
	// DefaultRunCommandTimeout is dsh bash's fresh-process default. It is a
	// per-tool override, so other tools retain the 30s global policy.
	DefaultRunCommandTimeout = 120 * time.Second

	// M5a jobs defaults (dispatch-m5a-2 §3): the per-owner active-job cap is
	// 10 when jobs.max_concurrent_jobs_per_owner is absent or non-positive
	// (mirrors dsh jobs-local and internal/jobs' own default).
	DefaultMaxConcurrentJobsPerOwner = 10

	// M5b subagent defaults (dispatch-m5b-2 §3): the delegation depth cap is 8
	// when subagent.max_depth is absent or non-positive, and the default
	// provider is "spawn" (the only provider shipped in M5b) when
	// subagent.default_provider is empty.
	DefaultSubagentMaxDepth = 8
	DefaultSubagentProvider = "spawn"

	// M5c compaction defaults (dispatch-m5c-2a §2 / dispatch-m5c-2 §2): the
	// token-pressure threshold is 32000 when compaction.token_threshold is
	// absent or non-positive, and the retained tail is 8 turns when
	// compaction.retain_turns is absent or non-positive. max_chars defaults to
	// 0, meaning the engine default (the wiring passes BasicEngine's default,
	// or 0 for the engine to fall back on).
	DefaultCompactionTokenThreshold = 32000
	DefaultCompactionRetainTurns    = 8
	// DefaultCompactionSummaryInputTokens bounds each model request used to
	// summarize a large shadowed prefix.
	DefaultCompactionSummaryInputTokens = 12000

	// M5d-2 skill defaults (dispatch-m5d-2 §2): the injected catalog is
	// bounded to 500 chars when skill.catalog_max_chars is absent or
	// non-positive, and skill returns at most 8000 chars of a skill body
	// when skill.body_max_chars is absent or non-positive (dispatch-m5d 约束:
	// 正文有长度上限防超长注入).
	DefaultSkillCatalogMaxChars = 500
	DefaultSkillBodyMaxChars    = 8000

	// M6a-2 schedule defaults (dispatch-m6a-2 §2): the serial pre-step clock
	// advances at the configured cadence; tick_interval defaults to 1m when
	// absent or non-positive. M6a-2 deliberately has no background ticker (D5)
	// — the loop's per-turn "schedule" pre-step injector calls Engine.Tick on
	// the serial path, and tick_interval is the documented cadence knob for
	// that advancement (reserved for a future gated advance; the value is
	// parsed and defaulted here regardless).
	DefaultScheduleTickInterval = time.Minute

	// M6e-2 code-sandbox defaults (dispatch-m6e-2 §2): the sandbox execution
	// deadline is 30s when code.timeout is absent or non-positive (mirrors the
	// local provider's own default), the per-stream output cap is 65536 bytes
	// when code.max_output is absent or non-positive (64KiB, the same bound as
	// the provider default and tools.output_limit), and sandbox_dir stays empty
	// meaning the provider default (<project>/.sandbox).
	DefaultCodeTimeout   = 30 * time.Second
	DefaultCodeMaxOutput = 64 * 1024

	// M7-2 web defaults (dispatch-m7-2 §5 / ADR 2026-08-20-m7-web-search.md):
	// the search result/query caps, the search and fetch timeouts, the fetch
	// output byte/char/url/redirect caps, the User-Agent, and the DeepSeek
	// search-provider parameters. web.enabled=false is the default (D10); when
	// enabled these are the values the composition root passes to the web seam.
	DefaultWebSearchMaxResults      = 8
	DefaultWebSearchMaxQueries      = 4
	DefaultWebSearchTimeoutMs       = 30000
	DefaultWebFetchTimeoutMs        = 30000
	DefaultWebFetchMaxOutputChars   = 200000
	DefaultWebFetchMaxResponseBytes = 2097152 // 2 MiB
	DefaultWebFetchMaxURLBytes      = 2048
	DefaultWebFetchMaxRedirects     = 5
	DefaultWebFetchUserAgent        = "shutu-agent/0.1 (M7)"
	DefaultWebDeepSeekBaseURL       = "https://api.deepseek.com/anthropic/v1"
	DefaultWebDeepSeekModel         = "deepseek-v4-flash"
	DefaultWebDeepSeekAPIVersion    = "2023-06-01"
	DefaultWebDeepSeekMaxTokens     = 4096
	DefaultWebDeepSeekMaxUses       = 5

	// M8-2 LLM-provider defaults (dispatch-m8-2 §5 / M8-2b §3): the default
	// provider is "deepseek-official" (regression: behavior identical to before M8-2);
	// openai defaults to base_url https://api.openai.com/v1 and model
	// gpt-4o-mini (both configurable); anthropic defaults to base_url
	// https://api.anthropic.com/v1 and model claude-sonnet-4-5 (both
	// configurable, consumed by M8-2b — these must stay in sync with the
	// internal/llm/anthropic package defaults).
	DefaultLLMProvider        = "deepseek-official"
	DefaultOpenAIBaseURL      = "https://api.openai.com/v1"
	DefaultOpenAIModel        = "gpt-4o-mini"
	DefaultAnthropicBaseURL   = "https://api.anthropic.com/v1"
	DefaultAnthropicModel     = "claude-sonnet-4-5"
	DefaultLLMMaxRetries      = 2
	DefaultLLMRetryBackoff    = 500 * time.Millisecond
	DefaultLLMRetryMaxBackoff = 8 * time.Second

	// M8-3 multimodal defaults (dispatch-m8-3 §3 / ADR
	// 2026-08-20-m8-message-model.md 决策 M8-3): multimodal is ON by default
	// (用户 2026-08-20 拍板「图片附件默认打开」,覆盖原 D10 默认关——显式 "enabled: false"
	// 仍可关); model_input_modalities defaults to "text" (the exact-model
	// capability declaration); a single image's raw-byte cap defaults to
	// 10 MiB (over-limit fails closed in internal/attachment); the per-request
	// image byte budget defaults to 20 MiB (M8-3b, over-budget images are
	// offloaded — oldest replaced by the placeholder — in the providers).
	DefaultModelInputModalities           = "text"
	DefaultMultimodalMaxImageBytes        = 10 * 1024 * 1024 // 10 MiB
	DefaultMultimodalMaxRequestImageBytes = 20 * 1024 * 1024 // 20 MiB

	// M9 terminal defaults (dispatch-m9-2 §2 / ADR 2026-08-20-m9-terminal.md):
	// the persistent-shell terminal is off by default (D10); the scrollback
	// caps, read pacing and the single-active-owner concurrency limit follow
	// the design defaults below.
	DefaultTerminalScrollbackMaxBytes = 65536
	DefaultTerminalScrollbackLines    = 2000
	DefaultTerminalReadIdleMS         = 500
	DefaultTerminalReadTimeoutMS      = 30000
	DefaultTerminalMaxConcurrent      = 1

	// M-Eval eval defaults (dispatch-eval-3a §1 / ADR 2026-08-20-eval-seam.md
	// D-EVAL-6): manual_fallback defaults to true (LLM undecided → human) and
	// the evaluation-history cap to 100 when absent or non-positive. The
	// capability itself is off by default (D10, bool 零值即关).
	DefaultEvalManualFallback = true
	DefaultEvalMaxRecords     = 100

	// GAP-3 workflow defaults (dispatch-gap-3 §5): the ready-task concurrency
	// cap is 4 when workflow.max_concurrent is absent or non-positive — kept in
	// sync with workflow.DefaultMaxConcurrent (the same D-GAP-2 cap).
	DefaultWorkflowMaxConcurrent   = 4
	DefaultWorkflowMaxTotalAgents  = 1000
	DefaultWorkflowMaxItemsPerCall = 4096
	DefaultWorkflowSyncTimeoutMS   = 5000
	DefaultRalphMaxRounds          = 256

	// Mode presets (ADR 2026-08-20-mode-presets.md D-MODE-1): the top-level
	// mode selects the agent's capability preset — minimal (极简: 固定 persona
	// + 持久 shell + 文件编辑), standard (标准: 全部已实现能力, 默认), code
	// (PTC: 标准 + 程序化操作 Code Mode 提示词段). An unknown value fails
	// closed at Load (like the LLM provider). 默认 standard ⇒ 现有默认行为零
	// 变化 (D10).
	DefaultMode  = "standard"
	ModeMinimal  = "minimal"
	ModeStandard = "standard"
	ModeCode     = "code"
)

// defaultEnabledTools is the native standard-mode whitelist applied when
// tools.enabled is absent. PTC adds run_code only through its mode projection;
// minimal replaces the list with its fixed terminal/file seam.
var defaultEnabledTools = []string{"get_time", "read"}

// ReadOnlyTools returns the read-only execution whitelist (D10): the tools
// that are always safe to expose. The General-settings "permission" preset's
// readonly tier whitelists exactly these (the composition root applies it).
func ReadOnlyTools() []string { return append([]string(nil), defaultEnabledTools...) }

// MinimalTools returns the exact tool whitelist for the minimal session preset.
func MinimalTools() []string { return append([]string(nil), minimalEnabledTools...) }

// minimalEnabledTools is the minimal preset's exact execution whitelist (ADR
// 2026-08-20-mode-presets.md D-MODE-2): M1 基础只读 + 命令 shell (pwsh, dsh
// 对齐: 每次调用全新 pwsh 进程) + 文件编辑 (read/write/list/edit). 工具名须与
// 各包常量一致 (tools.go/fs.go).
var minimalEnabledTools = []string{
	platformShellToolName(), "str_replace_editor",
}

// Bool returns a pointer to b, for assigning an explicit *bool flag where the
// field's zero value must mean "absent" rather than a value (tests and the
// composition root).
func Bool(b bool) *bool { return &b }

// Enabled reports whether a capability flag is on. A nil flag (absent from
// config.yaml) means the default is on — the D10 "default off" posture is
// replaced by opt-out, matching dsh's shipped composition (dsh 出厂默认挂载核心
// 能力, 按需 opt-out). An explicit *bool carries the user's choice, so an
// "enabled: false" in config.yaml still disables the capability. External
// subagent providers and the web portal keep their own default-off posture
// (see those structs), so this helper is for the core capability switches.
func Enabled(b *bool) bool { return b == nil || *b }

// Config is the file-backed runtime configuration. Any field may be omitted in
// config.yaml; Load fills defaults for empty values, so callers never branch
// on field presence.
type Config struct {
	Model      string `yaml:"model"`       // chat model; default deepseek-v4-flash
	BaseURL    string `yaml:"base_url"`    // optional OpenAI-compatible base URL; empty means the provider default
	DataDir    string `yaml:"data_dir"`    // directory for pa.db (and runtime data); default "data"
	PromptsDir string `yaml:"prompts_dir"` // directory of prompt section files; default "config/prompts"
	// ReasoningEffort is the runtime thinking-effort selection (dsh 思考强度,
	// ModelSelect effort): "" | "off" | "low" | "high" | "max". Runtime-only
	// (like the live model switch) — it never enters config.yaml.
	ReasoningEffort string             `yaml:"-"`             // runtime selection; empty keeps provider default
	Tools           ToolsConfig        `yaml:"tools"`         // tool-execution policy (M3)
	Jobs            JobsConfig         `yaml:"jobs"`          // background-job policy (M5a)
	Subagent        SubagentConfig     `yaml:"subagent"`      // subagent policy (M5b)
	Compaction      CompactionConfig   `yaml:"compaction"`    // context-compaction policy (M5c)
	Skill           SkillConfig        `yaml:"skill"`         // skill policy (M5d)
	Schedule        ScheduleConfig     `yaml:"schedule"`      // schedule policy (M6a)
	Plan            PlanConfig         `yaml:"plan"`          // task-planning policy (M6b)
	Spill           SpillConfig        `yaml:"spill"`         // long-term-memory policy (M6c)
	Interact        InteractConfig     `yaml:"interact"`      // human-approval policy (M6d)
	Code            CodeConfig         `yaml:"code"`          // code-sandbox policy (M6e)
	Mcp             McpConfig          `yaml:"mcp"`           // MCP tool-ecosystem policy (M6f)
	Fs              FsConfig           `yaml:"fs"`            // safe-file-operation policy (M6f)
	Web             WebConfig          `yaml:"web"`           // web search/fetch policy (M7)
	LLM             LLMConfig          `yaml:"llm"`           // LLM provider selection (M8-2)
	Terminal        TerminalConfig     `yaml:"terminal"`      // persistent-shell terminal (M9)
	Eval            EvalConfig         `yaml:"eval"`          // task-evaluation seam (eval)
	Ralph           RalphConfig        `yaml:"ralph"`         // fresh-agent loop (D-GAP-3)
	Workflow        WorkflowConfig     `yaml:"workflow"`      // task-DAG orchestration (D-GAP-2)
	FsSearch        FsSearchConfig     `yaml:"fs_search"`     // file-content-search policy (D-GAP-1)
	SessionQuery    SessionQueryConfig `yaml:"session_query"` // read-only session history queries (P2)
	LSP             LSPConfig          `yaml:"lsp"`           // read-only language-server queries (P2)
	Hooks           HooksConfig        `yaml:"hooks"`         // metadata-only event hooks (P2)
	WebServer       WebServerConfig    `yaml:"web_server"`    // unified web portal (M10a)
	Workspace       WorkspaceConfig    `yaml:"workspace"`     // workspace/session cwd policy

	// Mode selects the agent capability preset (D-MODE-1): minimal | standard
	// | code; default standard. minimal is preset-first (D-MODE-6): 能力开关
	// 与白名单被覆盖, 用户显式开启的其余能力在 minimal 下被忽略.
	Mode string `yaml:"mode"`
}

// WorkspaceConfig controls the process-side directory used by sessions that
// are not attached to a directory-backed workspace. An empty value resolves
// to the agent process working directory, matching dsh's fallback cwd.
type WorkspaceConfig struct {
	DefaultDir string `yaml:"default_dir"`
}

// LLMConfig is the LLM provider-selection policy (dispatch-m8-2 §5 / ADR
// 2026-08-20-m8-message-model.md 决策 M8-2). Provider routes to one of the
// registered providers: deepseek (default) | openai | anthropic (M8-2b). An
// unknown value fails closed at startup (the composition root errors, no
// silent fallback). The per-provider parameters are still parsed to their
// defaults even when that provider is not selected, so the config can switch
// providers; credentials only ever come from the environment (纪律 6), never
// from this file.
type LLMConfig struct {
	// Provider is the selection route; empty defaults to "deepseek-official".
	Provider string `yaml:"provider"`
	// OpenAI carries the OpenAI-compatible provider parameters (base_url /
	// model); the API key is OPENAI_API_KEY from the environment.
	OpenAI OpenAIProviderConfig `yaml:"openai"`
	// Anthropic carries the Anthropic Messages provider parameters (base_url /
	// model); the API key is ANTHROPIC_API_KEY from the environment (M8-2b).
	Anthropic AnthropicProviderConfig `yaml:"anthropic"`
	Retry     RetryConfig             `yaml:"retry"`
	// ModelInputModalities is the exact-model capability declaration
	// (dispatch-m8-3 §3): "text" | "text,image". Empty defaults to "text".
	// /llm-status displays it as the modalities line.
	ModelInputModalities string `yaml:"model_input_modalities"`
	// Multimodal is the image-attachment policy (M8-3). Multimodal is off by
	// default (D10): when disabled the composition root creates no attachment
	// store, /attach is unavailable, and image blocks are never serialized.
	Multimodal MultimodalConfig `yaml:"multimodal"`
}

// RetryConfig is the shared request-level retry policy. Retries are attempted
// only before a stream has yielded data; partial streams are never replayed.
type RetryConfig struct {
	MaxRetries     int      `yaml:"max_retries"`
	InitialBackoff Duration `yaml:"initial_backoff"`
	MaxBackoff     Duration `yaml:"max_backoff"`
}

// TerminalConfig is the pwsh-tool + M9 /term REPL policy (ADR
// 2026-08-23-pwsh-dsh-alignment.md / dispatch-m9-2 §2). Enabled gates both the
// model-facing pwsh tool (dsh tool-pwsh: one fresh `pwsh -Command` process
// per call — the remaining fields do not apply to it) and the /term REPL's
// persistent session. The session subprocess inherits a scrubbed environment
// (credential-bearing variables are dropped, 纪律 6) — see
// internal/terminal/scrubbedEnv.
type TerminalConfig struct {
	Enabled               *bool    `yaml:"enabled"`                 // default on (dsh 对齐); *bool distinguishes absent
	ACPEnabled            *bool    `yaml:"acp_enabled"`             // default false; explicit opt-in for ACP shell tools
	Shell                 string   `yaml:"shell"`                   // default "" → platform default (cmd.exe / /bin/sh)
	Args                  []string `yaml:"args"`                    // extra shell args
	Workdir               string   `yaml:"workdir"`                 // default "" → inherit agent cwd
	ScrollbackMaxBytes    int      `yaml:"scrollback_max_bytes"`    // default 65536
	ScrollbackLines       int      `yaml:"scrollback_lines"`        // default 2000
	ReadIdleMS            int      `yaml:"read_idle_ms"`            // default 500
	ReadTimeoutMS         int      `yaml:"read_timeout_ms"`         // default 30000
	MaxConcurrentSessions int      `yaml:"max_concurrent_sessions"` // default 1 (single active owner, D5)
}

// EvalConfig is the task-evaluation policy (ADR 2026-08-20-eval-seam.md
// D-EVAL-6). The capability is default off (D10): when Enabled is false the
// composition root registers no eval_* tools and no /eval-status command.
// ManualFallback is a pointer so an absent YAML field (→ the default true) is
// distinguishable from an explicit false: the LLM-undecided (manual) verdict
// routes to a human by default, and an explicit false makes it fail closed.
// applyDefaults guarantees it is never nil, so the composition root reads it by
// dereference.
type EvalConfig struct {
	Enabled *bool `yaml:"enabled"` // default false (D10)
	// ManualFallback is nil (absent) → the default true: an undecided verdict
	// routes to a human; false (explicit) fails closed.
	ManualFallback *bool `yaml:"manual_fallback"` // default true
	// MaxRecords caps the evaluation history (oldest evicted); <= 0 means the
	// default 100.
	MaxRecords int `yaml:"max_records"` // default 100
}

// RalphConfig is the fresh-agent loop policy (ADR 2026-08-20-standard-gaps.md
// D-GAP-3 / dispatch-gap-2 §5). The capability is default off (D10): when
// Enabled is false the composition root registers no ralph tool. Enabling ralph
// also requires subagent (the loop spawns children through the subagent
// Runtime). minimal 模式同样关闭 (D-MODE-2).
type RalphConfig struct {
	Enabled *bool `yaml:"enabled"` // default false (D10)
	// MaxRounds is the deployment ceiling for one fresh-agent loop. <= 0 uses
	// dsh's default 256.
	MaxRounds int `yaml:"max_rounds"`
}

// WorkflowConfig is the dsh-compatible workflow policy. The capability is
// default on through Enabled(nil), matching the personal-agent/dsh opt-out
// posture; minimal mode still disables it. The Go DAG and external Node
// JavaScript runner both use the subagent Runtime for child agents.
type WorkflowConfig struct {
	// Enabled gates the whole capability: when false, no workflow_run tool is
	// registered or whitelisted.
	Enabled *bool `yaml:"enabled"` // default true (nil means enabled)
	// MaxConcurrent is the ready-task concurrency cap the engine applies
	// (D-GAP-2); <= 0 means the default 4.
	MaxConcurrent int `yaml:"max_concurrent"` // default 4
	// MaxTotalAgents and MaxItemsPerCall are the dsh script-runner backstops.
	MaxTotalAgents  int `yaml:"max_total_agents"`   // default 1000
	MaxItemsPerCall int `yaml:"max_items_per_call"` // default 4096
	SyncTimeoutMS   int `yaml:"sync_timeout_ms"`    // default 5000
}

// WebServerConfig is the unified web portal policy (ADR
// 2026-08-20-m10-web-portal.md D-WEB-7). The portal is off by default (D10):
// when Enabled is false the composition root never starts a listener. When
// enabled, Token is required (fail-closed: empty token refuses to start rather
// than serving bare); only its SHA-256 digest is retained by the server. minimal
// 模式同样关闭 (D-MODE-2).
type WebServerConfig struct {
	Enabled bool   `yaml:"enabled"` // default false (D10)
	Addr    string `yaml:"addr"`    // default 127.0.0.1:8080 (local-only personal portal)
	Token   string `yaml:"token"`   // required when enabled; plaintext only in this config
	// DistDir points to the React/Cordis SPA output. The composition root passes
	// it to the web server so frontend releases do not require a Go rebuild.
	DistDir string `yaml:"dist_dir"`
}

// MultimodalConfig is the image-attachment policy (dispatch-m8-3 §3 / ADR
// 2026-08-20-m8-message-model.md 决策 M8-3; 用户 2026-08-20 拍板「图片附件默认打开」,
// 覆盖该条 D10 默认关). When disabled the composition root creates no
// attachment store and /attach is unavailable. Images are stored as files under
// <data_dir>/attachments/ and only the ImageRef is logged (dsh 7078918 范式: 落库
// 只存引用，请求时才转 data URL — M8-3b serializes). The bytes never enter the
// session log or the config.
type MultimodalConfig struct {
	// Enabled gates the whole capability: false ⇒ /attach unavailable and
	// image blocks are never serialized. A pointer distinguishes "unset"
	// (defaults to true — the user chose default-on) from an explicit
	// "enabled: false". The minimal preset forces false (D-MODE-2).
	Enabled *bool `yaml:"enabled"`
	// MaxImageBytes is the single-image raw-byte cap applied by SaveImage;
	// <= 0 means the default 10 MiB (over-limit fails closed).
	MaxImageBytes int `yaml:"max_image_bytes"`
	// MaxRequestImageBytes is the per-request image byte budget
	// (dispatch-m8-3b §6): images whose cumulative bytes (in message-history
	// order) exceed it are offloaded at serialize time — the oldest images are
	// replaced by the placeholder text. <= 0 means the default 20 MiB (the
	// provider New applies the same fallback, 校验非负).
	MaxRequestImageBytes int `yaml:"max_request_image_bytes"`
}

// OpenAIProviderConfig is the OpenAI-compatible provider parameters
// (dispatch-m8-2 §5). BaseURL defaults to https://api.openai.com/v1, Model to
// gpt-4o-mini; the API key is OPENAI_API_KEY (env-only, 纪律 6).
type OpenAIProviderConfig struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

// AnthropicProviderConfig is the Anthropic Messages provider parameters
// (dispatch-m8-2b §3; ADR 2026-08-20-m8-message-model.md 决策 M8-2). BaseURL
// defaults to https://api.anthropic.com/v1, Model to claude-sonnet-4-5; the
// API key is ANTHROPIC_API_KEY (env-only, 纪律 6). The defaults must stay in
// sync with the internal/llm/anthropic package defaults.
type AnthropicProviderConfig struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

// JobsConfig is the background-job policy (dispatch-m5a-2 §3 / ADR
// 2026-08-18-m5-agent-core.md 决策 ①). Jobs are off by default (D10): when
// disabled the composition root neither initializes a registry nor registers
// or whitelists the job_* tools.
type JobsConfig struct {
	// Enabled gates the whole capability: when false, no registry is created
	// (jobs.NewLocal is never called) and the job_* tools are neither
	// registered nor whitelisted (D10).
	Enabled *bool `yaml:"enabled"`
	// MaxConcurrentJobsPerOwner caps the running+stopping jobs in one owner
	// bucket (and the shared unowned bucket); <= 0 means the default 10.
	MaxConcurrentJobsPerOwner int `yaml:"max_concurrent_jobs_per_owner"`
}

// SubagentConfig is the subagent policy (dispatch-m5b-2 §3 / ADR
// 2026-08-18-m5-agent-core.md 决策 ②). Subagents are off by default (D10): when
// disabled the composition root neither initializes a runtime nor registers or
// whitelists the subagent_* tools.
type SubagentConfig struct {
	// Enabled gates the whole capability: when false, no Runtime/SpawnProvider
	// is created and the subagent_* tools are neither registered nor
	// whitelisted (D10).
	Enabled *bool `yaml:"enabled"`
	// ACPEnabled is a second explicit opt-in: ACP sessions do not create a
	// subagent runtime unless this is true, even when the normal subagent
	// capability is enabled.
	ACPEnabled *bool `yaml:"acp_enabled"`
	// MaxDepth is the default delegation depth cap applied by subagent_spawn
	// when the model omits max_depth; <= 0 means the default 8.
	MaxDepth int `yaml:"max_depth"`
	// DefaultProvider is the provider subagent_spawn delegates to; empty means
	// the default "spawn" (the only provider shipped in M5b, so the tool
	// resolves to it regardless).
	DefaultProvider string `yaml:"default_provider"`
	// ExternalProviders declares optional external subagent backends
	// (D-GAP-4): keyed by provider name, each with an optional enable flag
	// (default false) and the CLI command (empty → per-name default:
	// codex→"codex", claude_code→"claude"). A provider is registered only
	// when Enabled is true; an enabled provider whose binary is missing fails
	// closed at Start. All default off (D10).
	ExternalProviders map[string]ExternalProviderConfig `yaml:"external_providers"`
}

// ExternalProviderConfig is one optional external subagent backend (D-GAP-4):
// an enable flag (default off, D10) and the one-shot CLI command. The provider
// is registered into the subagent Runtime only when Enabled is true; an
// enabled provider whose binary is missing fails closed at Start (no silent
// fallback to the local provider).
type ExternalProviderConfig struct {
	// Enabled gates this provider's registration (default false, D10 —
	// guardrail: an external binary whose CLI is missing fails closed at
	// Start, so external subagent providers stay opt-in, not default-on).
	Enabled bool `yaml:"enabled"`
	// Command is the CLI binary invoked for a one-shot prompt→stdout session;
	// empty means the per-name default filled by applyDefaults (codex→"codex",
	// claude_code→"claude"); any other key keeps an empty command and the
	// composition root looks the name up as-is (fail-closed at Start).
	Command string `yaml:"command"`
}

// CompactionConfig is the context-compaction policy (dispatch-m5c-2a §2 /
// dispatch-m5c-2 §2 / ADR 2026-08-18-m5-agent-core.md 决策 ③). Compaction is
// off by default (D10): when disabled the composition root neither registers
// subagent, enabling compaction whitelists no tools — compaction has no
// consumer tools (automatic triggering runs through the loop pre-step
// injector, manual through the /compact command, dispatch-m5c-2 §2).
type CompactionConfig struct {
	// Enabled gates the whole capability: when false, no compaction engine is
	// wired into the loop's PreStep and the /compact command reports the
	// capability as unavailable (D10).
	Enabled *bool `yaml:"enabled"`
	// TokenThreshold is the surface-token pressure threshold above which a
	// step auto-compacts; <= 0 means the default 32000.
	TokenThreshold int `yaml:"token_threshold"`
	// RetainTurns is the tail of recent turns the basic provider keeps
	// unshadowed; <= 0 means the default 8.
	RetainTurns int `yaml:"retain_turns"`
	// RetainTokens is the dsh-style token budget kept at the tail. When
	// positive it takes precedence over retain_turns; zero keeps legacy turn
	// based selection.
	RetainTokens int `yaml:"retain_tokens"`
	// MaxChars bounds the generated summary; <= 0 means the engine default
	// (the wiring passes BasicEngine's default, or 0 for the engine to fall
	// back on).
	MaxChars int `yaml:"max_chars"`
	// SummaryInputTokens bounds the conversation portion of each summarizer
	// request; <= 0 means the safe default. Oversized prefixes are reduced in
	// bounded intermediate chunks.
	SummaryInputTokens int `yaml:"summary_input_tokens"`
}

// SkillConfig is the skill policy (dispatch-m5d-2 §2 / ADR
// 2026-08-18-m5-agent-core.md 决策 ④). Skills are off by default (D10): when
// disabled the composition root neither creates a provider/registry nor
// registers or whitelists the skill tool, and no catalog injector is
// registered.
type SkillConfig struct {
	// Enabled gates the whole capability: when false, no skill provider/
	// registry is created and skill is neither registered nor whitelisted,
	// and no skill catalog pre-step injector is wired (D10).
	Enabled *bool `yaml:"enabled"`
	// Dirs are additional custom skill directories (source "custom", rank 300)
	// scanned by the filesystem provider, in order. Empty by default.
	Dirs []string `yaml:"dirs"`
	// CatalogMaxChars bounds the injected skill catalog (sorted name +
	// description) in chars; <= 0 means the default 500.
	CatalogMaxChars int `yaml:"catalog_max_chars"`
	// BodyMaxChars bounds the skill body skill returns to the model in
	// chars (Unicode-safe truncation, 防超长注入); <= 0 means the default 8000.
	BodyMaxChars int `yaml:"body_max_chars"`
}

// ScheduleConfig is the schedule policy (dispatch-m6a-2 §2 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6a). Schedules are off by default (D10):
// when disabled the composition root neither creates an Engine nor registers
// or whitelists the schedule_* tools, and no "schedule" pre-step injector is
// wired.
type ScheduleConfig struct {
	// Enabled gates the whole capability: when false, no Provider/Engine is
	// created and the schedule_* tools are neither registered nor whitelisted,
	// and no schedule pre-step injector is wired (D10).
	Enabled *bool `yaml:"enabled"`
	// TickInterval is the cadence of the serial schedule-clock advancement
	// (per-turn pre-step Engine.Tick). There is no background ticker in M6a-2
	// (D5); the value is parsed and defaulted here so a future gated advance
	// can consume it. <= 0 means the default 1m.
	TickInterval Duration `yaml:"tick_interval"`
}

// PlanConfig is the task-planning policy (dispatch-m6b-2 §2 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6b). Planning is off by default (D10): when
// disabled the composition root neither creates an Engine nor registers or
// whitelists the plan_* tools.
type PlanConfig struct {
	// Enabled gates the whole capability: when false, no Provider/Engine is
	// created and the plan_* tools are neither registered nor whitelisted
	// (D10).
	Enabled *bool `yaml:"enabled"`
}

// SpillConfig is the long-term-memory policy (dispatch-m6c-2 §2 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6c). Spill is off by default (D10): when
// disabled the composition root neither creates a Provider/Engine nor
// registers or whitelists the spill_* tools, and no auto-sedimentation path is
// wired. AutoSpill is a pointer so an absent YAML field (→ the default true) is
// distinguishable from an explicit false, which carries real meaning here:
// auto_spill: false keeps the spill_* tools usable while turning the automatic
// end-of-turn sedimentation off. Read it through AutoSpillValue, never
// directly.
type SpillConfig struct {
	// Enabled gates the whole capability: when false, no Provider/Engine is
	// created, the spill_* tools are neither registered nor whitelisted, and
	// no auto-sedimentation path is wired (D10).
	Enabled *bool `yaml:"enabled"`
	// AutoSpill toggles the end-of-turn auto-sedimentation writeback
	// (Engine.AutoSpill over the session event log): nil (absent) means true —
	// within an enabled spill the auto-sedimentation defaults on, matching the
	// config.yaml documentation. It only takes effect when Enabled is true.
	AutoSpill *bool `yaml:"auto_spill"`
}

// AutoSpillValue returns whether the end-of-turn auto-sedimentation runs
// (true by default within an enabled spill; false explicitly disables it).
func (s SpillConfig) AutoSpillValue() bool {
	if s.AutoSpill == nil {
		return true
	}
	return *s.AutoSpill
}

// InteractConfig is the human-approval policy (dispatch-m6d-2 §2 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6d). Interact is off by default (D10): when
// disabled the composition root neither creates an Engine nor registers or
// whitelists the interact_* tools, and no sensitive-tool gate is installed.
type InteractConfig struct {
	// Enabled gates the whole capability: when false, no Provider/Engine is
	// created, the interact_* tools are neither registered nor whitelisted,
	// and no sensitive-tool gate is installed (D10).
	Enabled *bool `yaml:"enabled"`
	// SensitiveTools names the tools whose execution must first pass a human
	// approval (the ADR 决策 M6d sensitive-tool gate: approved before the tool
	// runs, rejected returns a denial to the model). Empty means no gating —
	// an enabled interact still registers the interact_* tools but intercepts
	// nothing.
	SensitiveTools []string `yaml:"sensitive_tools"`
}

// CodeConfig is the code-sandbox policy (dispatch-m6e-2 §2 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6e). The code sandbox is off by default
// (D10): when disabled the composition root neither creates a local Provider /
// Engine nor registers or whitelists the run_code tool. It is controlled
// isolation, not strong isolation (process boundary + timeout + output quota +
// default no network; Windows has no network namespace — see the internal/code
// package comment for the exact boundary).
type CodeConfig struct {
	// Enabled gates the whole capability: when false, no local Provider/Engine
	// is created and run_code is neither registered nor whitelisted (D10).
	Enabled *bool `yaml:"enabled"`
	// Timeout is the sandbox execution deadline run_code applies when the model
	// omits the per-call timeout (and the outer per-tool deadline bound for
	// run_code, mirroring tools.run_command.timeout); <= 0 means the default 30s.
	Timeout Duration `yaml:"timeout"`
	// MaxOutput is the per-stream output cap of a sandbox run (the model cannot
	// override it); <= 0 means the default 65536 bytes.
	MaxOutput int `yaml:"max_output"`
	// SandboxDir is the sandbox working directory used when the model omits
	// cwd. Empty means the provider default (<project>/.sandbox).
	SandboxDir string `yaml:"sandbox_dir"`
	// AllowNetwork is a declarative network toggle: false (the default) means
	// the sandbox injects no network credentials — the v1 local provider always
	// scrubs credential-shaped environment entries regardless of this flag. It
	// is a recorded boundary, not strong isolation: denying network access at
	// the OS level is out of scope on Windows (no network namespace).
	AllowNetwork bool `yaml:"allow_network"`
}

// McpConfig is the MCP tool-ecosystem policy (dispatch-m6f-2 §2 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6f). MCP is off by default (D10): when
// disabled the composition root neither creates a Factory nor registers or
// whitelists the mcp_* tools, and no server is bridged. When enabled, mcp_list
// lists a configured server's tools and mcp_call invokes one by name (each a
// fresh stdio client per call, D5), and every tool each configured server
// advertises is bridged into the tool registry as mcp.<server>.<tool> with its
// input schema passed through, calling back into the server via tools/call.
type McpConfig struct {
	// Enabled gates the whole capability: when false, no mcp Factory is
	// created, the mcp_* tools are neither registered nor whitelisted, and no
	// server is bridged (D10).
	Enabled *bool `yaml:"enabled"`
	// ACPEnabled is a second explicit opt-in: MCP subprocesses are not started
	// for ACP sessions unless this is true, even when the REPL MCP capability
	// is enabled.
	ACPEnabled *bool `yaml:"acp_enabled"`
	// Servers are the configured MCP servers (stdio, newline-delimited
	// JSON-RPC). Each server's tools are bridged at startup with the
	// mcp.<server>.<tool> prefix.
	Servers []McpServer `yaml:"servers"`
}

// McpServer is one configured MCP server: a unique Name (used as the
// mcp_list/mcp_call selector and the mcp.<server>.<tool> bridge prefix) and a
// stdio command line (Cmd plus Args) the Factory spawns.
type McpServer struct {
	Name string   `yaml:"name"`
	Cmd  string   `yaml:"cmd"`
	Args []string `yaml:"args"`
}

// FsConfig is the safe-file-operation policy (dispatch-m6f-3 §3 / ADR
// 2026-08-19-m6-agent-full.md 决策 M6f). The fs capability is off by default
// (D10): when disabled the composition root neither creates a FileService nor
// registers or whitelists the fs_* tools. When enabled, every fs_* operation
// is constrained to the allowed root.
type FsConfig struct {
	// Enabled gates the whole capability: when false, no FileService is
	// created and the fs_* tools are neither registered nor whitelisted (D10).
	Enabled *bool `yaml:"enabled"`
	// Root is the allowed root every fs_* path must stay inside. Empty means
	// the default <project> (the process working directory), resolved by the
	// FileService constructor.
	Root string `yaml:"root"`
}

// FsSearchConfig is the file-content-search policy (D-GAP-1, 对齐 dsh
// tool-fs-search). The capability is default off (D10): when Enabled is false
// the composition root registers no grep/glob tool. minimal 模式同样关闭
// (D-MODE-2).
type FsSearchConfig struct {
	Enabled *bool `yaml:"enabled"` // default false (D10)
}

// SessionQueryConfig is the read-only dsh-aligned session history query
// surface. It is opt-in because the local implementation remains deliberately
// narrower than dsh's live/persisted dual-source provider.
type SessionQueryConfig struct {
	Enabled    bool `yaml:"enabled"`
	MaxResults int  `yaml:"max_results"` // <= 0 means the default 20
}

// LSPConfig is the read-only language-server consumer (P2). It is explicit
// opt-in because starting a configured executable is an external process
// boundary; no server is started when Enabled is false.
type LSPConfig struct {
	Enabled          bool              `yaml:"enabled"`
	Command          string            `yaml:"command"`
	Args             []string          `yaml:"args"`
	Extensions       map[string]string `yaml:"extensions"`
	TimeoutMS        int               `yaml:"timeout_ms"`
	MaxLocations     int               `yaml:"max_locations"`
	MaxResultChars   int               `yaml:"max_result_chars"`
	MaxDocumentBytes int               `yaml:"max_document_bytes"`
}

// HooksConfig enables one metadata-only executable observer for selected
// committed session event types. Hook payloads exclude event data, message
// text, tool arguments, and tool results.
type HooksConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Command    string   `yaml:"command"`
	Args       []string `yaml:"args"`
	Events     []string `yaml:"events"`
	TimeoutMS  int      `yaml:"timeout_ms"`
	WorkingDir string   `yaml:"working_dir"`
}

// WebConfig 是联网能力策略（ADR 2026-08-20-m7-web-search.md / dispatch-m7-2 §5）。
// 默认关（D10）：disabled 时组合根不创建 Engine、不注册/白名单 web_* 工具。
// web.enabled 同时开搜索与抓取——不设独立的 search_enabled/fetch_enabled 开关
// （dispatch-m7-2 §6 决策：按 dsh {search:true, fetch:true} 语义简化为总开关）。
type WebConfig struct {
	// Enabled gates the whole capability: when false, the composition root
	// creates no Engine, registers no web_* tool, and whitelists nothing (D10).
	Enabled *bool `yaml:"enabled"`

	// SearchMaxResults is the source cap for a single search and for the
	// merged multi-query result; <= 0 means the default 8.
	SearchMaxResults int `yaml:"search_max_results"`
	// SearchMaxQueries is the query-count cap for one web_search call (the
	// schema maxItems); <= 0 means the default 4.
	SearchMaxQueries int `yaml:"search_max_queries"`
	// SearchTimeoutMs is the outer budget for one web_search call (all queries
	// share it); <= 0 means the default 30000.
	SearchTimeoutMs int `yaml:"search_timeout_ms"`
	// DeepSeek carries the DeepSeek search-provider parameters (API key stays
	// in the environment — DEEPSEEK_API_KEY only, design.md §6).
	DeepSeek DeepSeekWebConfig `yaml:"deepseek"`

	// FetchTimeoutMs is the outer budget for one web_fetch call; <= 0 means
	// the default 30000.
	FetchTimeoutMs int `yaml:"fetch_timeout_ms"`
	// FetchMaxOutputChars caps the model-facing web_fetch body in chars
	// (truncated with a notice); <= 0 means the default 200000.
	FetchMaxOutputChars int `yaml:"fetch_max_output_chars"`
	// FetchMaxResponseBytes caps the fetched response body in bytes; <= 0
	// means the default 2097152 (2 MiB).
	FetchMaxResponseBytes int `yaml:"fetch_max_response_bytes"`
	// FetchMaxURLBytes caps the request URL length; <= 0 means the default 2048.
	FetchMaxURLBytes int `yaml:"fetch_max_url_bytes"`
	// FetchMaxRedirects caps same-origin redirect hops; <= 0 means the default 5.
	FetchMaxRedirects int `yaml:"fetch_max_redirects"`
	// FetchUserAgent is the User-Agent header; empty means the default.
	FetchUserAgent string `yaml:"fetch_user_agent"`
}

// DeepSeekWebConfig 是 DeepSeek 搜索 provider 的参数（M7-1/M7-2；API key 只在
// 环境变量 DEEPSEEK_API_KEY）。默认值见 DefaultWebDeepSeek* 常量。
type DeepSeekWebConfig struct {
	// BaseURL 是 Anthropic 兼容 Messages API 基址（/messages 附加）。
	BaseURL string `yaml:"base_url"` // 默认 https://api.deepseek.com/anthropic/v1
	// Model 是搜索请求的模型。
	Model string `yaml:"model"` // 默认 deepseek-v4-flash
	// APIVersion 是 anthropic-version 头。
	APIVersion string `yaml:"api_version"` // 默认 2023-06-01
	// MaxTokens 是搜索请求的 max_tokens。
	MaxTokens int `yaml:"max_tokens"` // 默认 4096
	// MaxUses 是 web_search server tool 单请求最大使用次数。
	MaxUses int `yaml:"max_uses"` // 默认 5
}

// pointers so an absent YAML field (→ the default) is distinguishable from an
// explicit 0/false, which carries real meaning here: recall_limit 0 disables
// proactive recall and catalog false suppresses the system-prompt catalog.
// Read them through RecallLimitValue / CatalogValue, never directly.

// ToolsConfig is the M3 tool-execution policy: the whitelist, the per-tool
// execute deadline, the output limit, and the optional run_command policy.
type ToolsConfig struct {
	// Enabled is the tool whitelist (design.md §5): only these names may
	// execute. Absent/empty defaults to the read-only pair.
	Enabled []string `yaml:"enabled"`
	// Timeout is the per-tool execute deadline (default 30s). Every Execute
	// is wrapped in context.WithTimeout.
	Timeout Duration `yaml:"timeout"`
	// OutputLimit caps the model-facing tool result in bytes (default 64KB).
	// Oversized output is truncated and the full text is spilled to
	// data/spill/<session>-<seq>.txt.
	OutputLimit int `yaml:"output_limit"`
	// RunCommand is the policy for the sole execution-class tool (default
	// disabled; design.md §5 / D10 落地).
	RunCommand RunCommandConfig `yaml:"run_command"`
}

// RunCommandConfig is the run_command tool policy. The tool is registered and
// usable only when Enabled is true (default off); its timeout defaults to the
// dsh bash value of 120s and overrides the global tools.timeout; Workdir fixes
// the working directory of every command.
type RunCommandConfig struct {
	Enabled bool     `yaml:"enabled"`
	Timeout Duration `yaml:"timeout"` // 0/absent => dsh bash default 120s
	Workdir string   `yaml:"workdir"` // fixed cwd; empty => the agent's own cwd
}

// Duration unmarshals a YAML scalar like "30s" into a time.Duration. An empty
// or absent value yields the zero duration.
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses a Go duration string ("30s", "1m", ...). An empty
// string is accepted as the zero duration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("config: duration must be a string like \"30s\": %w", err)
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

// Load reads configuration from path. A missing file is not an error: the
// returned Config holds the defaults. A present-but-invalid file is an error.
func Load(path string) (Config, error) {
	cfg := Config{
		Model:      DefaultModel,
		BaseURL:    DefaultBaseURL,
		DataDir:    DefaultDataDir,
		PromptsDir: DefaultPromptsDir,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			applyDefaults(&cfg)
			return cfg, nil
		}
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	applyDefaults(&cfg)
	// Mode fails closed on unknown values, like the LLM provider route
	// (D-MODE-1): never silently fall back.
	switch cfg.Mode {
	case ModeMinimal, ModeStandard, ModeCode:
	default:
		return Config{}, fmt.Errorf("config: invalid mode %q (want minimal|standard|code)", cfg.Mode)
	}
	// BaseURL intentionally keeps an empty value to mean "provider default".
	return cfg, nil
}

// applyDefaults fills every field that is empty or absent so callers never
// branch on field presence. It runs on both the missing-file and parsed paths.
func applyDefaults(cfg *Config) {
	if cfg.Mode == "" {
		cfg.Mode = DefaultMode
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir
	}
	if cfg.PromptsDir == "" {
		cfg.PromptsDir = DefaultPromptsDir
	}
	if len(cfg.Tools.Enabled) == 0 {
		cfg.Tools.Enabled = append([]string(nil), defaultEnabledTools...)
	}
	if cfg.Tools.Timeout.Duration <= 0 {
		cfg.Tools.Timeout.Duration = DefaultToolTimeout
	}
	if cfg.Tools.OutputLimit <= 0 {
		cfg.Tools.OutputLimit = DefaultOutputLimit
	}
	if cfg.Tools.RunCommand.Timeout.Duration <= 0 {
		cfg.Tools.RunCommand.Timeout.Duration = DefaultRunCommandTimeout
	}
	// Enabling run_command makes it whitelisted too, so the single
	// tools.run_command.enabled switch is what turns the execution tool on
	// (design.md §5 / D10).
	if cfg.Tools.RunCommand.Enabled && !contains(cfg.Tools.Enabled, "bash") {
		cfg.Tools.Enabled = append(cfg.Tools.Enabled, "bash")
	}
	// Enabling jobs whitelists its five consumer tools as well, so the single
	// jobs.enabled switch turns the whole capability (registry + tools + event
	if Enabled(cfg.Jobs.Enabled) {
		for _, name := range jobsToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	// M5a jobs defaults: off by default; the per-owner active-job cap is 10.
	if cfg.Jobs.MaxConcurrentJobsPerOwner <= 0 {
		cfg.Jobs.MaxConcurrentJobsPerOwner = DefaultMaxConcurrentJobsPerOwner
	}
	// Enabling subagent whitelists its four consumer tools as well, so the
	// single subagent.enabled switch turns the whole capability (runtime +
	// provider + tools + event logging) on; default off (D10, dispatch-m5b-2
	if Enabled(cfg.Subagent.Enabled) {
		for _, name := range subagentToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Subagent.ACPEnabled == nil {
		cfg.Subagent.ACPEnabled = Bool(false)
	}
	// M5b subagent defaults: off by default; the delegation depth cap is 8;
	// the default provider is "spawn".
	if cfg.Subagent.MaxDepth <= 0 {
		cfg.Subagent.MaxDepth = DefaultSubagentMaxDepth
	}
	if cfg.Subagent.DefaultProvider == "" {
		cfg.Subagent.DefaultProvider = DefaultSubagentProvider
	}
	// D-GAP-4 external subagent providers: all default off (D10); an empty
	// command falls back to the per-name default (codex→"codex",
	// claude_code→"claude"). Any other key keeps an empty command and the
	// composition root looks the name up as-is (fail-closed at Start when the
	// binary is missing). Registration itself is gated by Enabled plus the
	// subagent master switch — registerSubagent returns early when subagent is
	// disabled, so the minimal preset's Subagent.Enabled=false also disables
	// every external provider (D-MODE-2).
	for name, ep := range cfg.Subagent.ExternalProviders {
		if ep.Command == "" {
			switch name {
			case "codex":
				ep.Command = "codex"
			case "claude_code":
				ep.Command = "claude"
			}
		}
		cfg.Subagent.ExternalProviders[name] = ep
	}
	// M5c compaction defaults: off by default (D10); the token-pressure
	// threshold is 32000; the retained tail is 8 turns; max_chars 0 means the
	// engine default. Compaction deliberately whitelists no tools — it has none
	// (automatic triggering runs through the loop pre-step injector, manual
	// through the /compact command, dispatch-m5c-2a §2). Non-positive
	// thresholds/retain are clamped to the defaults (校验非负: a negative
	// configured value can never survive to the wiring).
	if cfg.Compaction.TokenThreshold <= 0 {
		cfg.Compaction.TokenThreshold = DefaultCompactionTokenThreshold
	}
	if cfg.Compaction.RetainTurns <= 0 {
		cfg.Compaction.RetainTurns = DefaultCompactionRetainTurns
	}
	if cfg.Compaction.SummaryInputTokens <= 0 {
		cfg.Compaction.SummaryInputTokens = DefaultCompactionSummaryInputTokens
	}
	// M5d-2 skill defaults: the sample config enables this capability; the catalog is bounded to
	// 500 chars and the returned skill body to 8000 chars. Enabling skill
	// whitelists its single consumer tool skill, so the one
	// skill.enabled switch turns the whole capability (provider + registry +
	// bounds are clamped to the defaults (校验非负: a negative configured value
	// can never survive to the wiring).
	if Enabled(cfg.Skill.Enabled) {
		for _, name := range skillToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Skill.CatalogMaxChars <= 0 {
		cfg.Skill.CatalogMaxChars = DefaultSkillCatalogMaxChars
	}
	if cfg.Skill.BodyMaxChars <= 0 {
		cfg.Skill.BodyMaxChars = DefaultSkillBodyMaxChars
	}
	// M6a-2 schedule defaults: off by default (D10); the serial clock cadence
	// is 1m. Enabling schedule whitelists its three consumer tools, so the one
	// schedule.enabled switch turns the whole capability (Provider + Engine +
	// tools + pre-step trigger + fire event/job wiring) on (mirrors
	// (校验非负: a negative configured value can never survive to the wiring).
	if Enabled(cfg.Schedule.Enabled) {
		for _, name := range scheduleToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Schedule.TickInterval.Duration <= 0 {
		cfg.Schedule.TickInterval.Duration = DefaultScheduleTickInterval
	}
	// M6b-2 plan defaults: off by default (D10). Enabling plan whitelists its
	// six consumer tools, so the one plan.enabled switch turns the whole
	// capability (Provider + Engine + tools + event logging) on (mirrors
	if Enabled(cfg.Plan.Enabled) {
		for _, name := range append(append([]string{}, goalToolNames...), todoToolNames...) {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	// M6d-2 interact defaults: off by default (D10). Enabling interact
	// whitelists its two consumer tools, so the one interact.enabled switch
	// turns the whole capability (Provider + Engine + tools + event logging +
	// plan/spill). sensitive_tools is left verbatim: empty means the gate is
	// not installed even when enabled (no gating by default).
	if Enabled(cfg.Interact.Enabled) {
		for _, name := range interactToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	// M6e-2 code defaults: off by default (D10); the sandbox timeout is 30s,
	// the per-stream output cap 65536 bytes, and sandbox_dir empty (the
	// provider default <project>/.sandbox). Enabling code whitelists its single
	// consumer tool run_code, so the one code.enabled switch turns the whole
	// capability (Provider + Engine + tool + event logging) on (mirrors
	// are clamped to the defaults (校验非负: a negative configured value can
	// never survive to the wiring). allow_network stays verbatim: false by
	// default (declarative no-network boundary).
	if Enabled(cfg.Code.Enabled) {
		for _, name := range codeToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Code.Timeout.Duration <= 0 {
		cfg.Code.Timeout.Duration = DefaultCodeTimeout
	}
	if cfg.Code.MaxOutput <= 0 {
		cfg.Code.MaxOutput = DefaultCodeMaxOutput
	}
	// M6f-2 mcp defaults: off by default (D10). Enabling mcp whitelists its two
	// consumer tools mcp_list and mcp_call, so the one mcp.enabled switch turns
	// the whole capability (Factory + mcp_* tools + server bridging + event
	// code). Bridged server tools (mcp.<server>.<tool>) cannot be whitelisted
	// here — their names are only known at runtime — so the composition root
	// whitelists each one as it is registered.
	if Enabled(cfg.Mcp.Enabled) {
		for _, name := range mcpToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Mcp.ACPEnabled == nil {
		cfg.Mcp.ACPEnabled = Bool(false)
	}
	// M6f-3 fs defaults: enabled by default to match dsh base; root empty means the default
	// <project> (the process working directory), resolved by the FileService
	// constructor — there is nothing to default here. Enabling fs whitelists
	// its three consumer tools, so the one fs.enabled switch turns the whole
	// capability (FileService + fs_* tools + event logging) on (mirrors
	if cfg.Fs.Enabled == nil {
		cfg.Fs.Enabled = Bool(true)
	}
	if Enabled(cfg.Fs.Enabled) {
		for _, name := range fsToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
		if cfg.Mode != ModeMinimal && Enabled(cfg.LLM.Multimodal.Enabled) && !contains(cfg.Tools.Enabled, "read_image") {
			cfg.Tools.Enabled = append(cfg.Tools.Enabled, "read_image")
		}
	}
	// M7-2 web defaults: off by default (D10); the search/query caps, timeouts,
	// fetch bounds and DeepSeek provider parameters fall back to the defaults.
	// Enabling web whitelists its two consumer tools web_search and web_fetch,
	// so the one web.enabled switch turns the whole capability (Engine +
	// spill/interact/code/mcp/fs). web.enabled is the single switch for both
	// search and fetch (no search_enabled/fetch_enabled split, dispatch-m7-2
	// §6). Non-positive bounds are clamped to the defaults (校验非负: a negative
	// configured value can never survive to the wiring).
	if Enabled(cfg.Web.Enabled) {
		for _, name := range webToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Web.SearchMaxResults <= 0 {
		cfg.Web.SearchMaxResults = DefaultWebSearchMaxResults
	}
	if cfg.Web.SearchMaxQueries <= 0 {
		cfg.Web.SearchMaxQueries = DefaultWebSearchMaxQueries
	}
	if cfg.Web.SearchTimeoutMs <= 0 {
		cfg.Web.SearchTimeoutMs = DefaultWebSearchTimeoutMs
	}
	if cfg.Web.FetchTimeoutMs <= 0 {
		cfg.Web.FetchTimeoutMs = DefaultWebFetchTimeoutMs
	}
	if cfg.Web.FetchMaxOutputChars <= 0 {
		cfg.Web.FetchMaxOutputChars = DefaultWebFetchMaxOutputChars
	}
	if cfg.Web.FetchMaxResponseBytes <= 0 {
		cfg.Web.FetchMaxResponseBytes = DefaultWebFetchMaxResponseBytes
	}
	if cfg.Web.FetchMaxURLBytes <= 0 {
		cfg.Web.FetchMaxURLBytes = DefaultWebFetchMaxURLBytes
	}
	if cfg.Web.FetchMaxRedirects <= 0 {
		cfg.Web.FetchMaxRedirects = DefaultWebFetchMaxRedirects
	}
	if cfg.Web.FetchUserAgent == "" {
		cfg.Web.FetchUserAgent = DefaultWebFetchUserAgent
	}
	if cfg.Web.DeepSeek.BaseURL == "" {
		cfg.Web.DeepSeek.BaseURL = DefaultWebDeepSeekBaseURL
	}
	if cfg.Web.DeepSeek.Model == "" {
		cfg.Web.DeepSeek.Model = DefaultWebDeepSeekModel
	}
	if cfg.Web.DeepSeek.APIVersion == "" {
		cfg.Web.DeepSeek.APIVersion = DefaultWebDeepSeekAPIVersion
	}
	if cfg.Web.DeepSeek.MaxTokens <= 0 {
		cfg.Web.DeepSeek.MaxTokens = DefaultWebDeepSeekMaxTokens
	}
	if cfg.Web.DeepSeek.MaxUses <= 0 {
		cfg.Web.DeepSeek.MaxUses = DefaultWebDeepSeekMaxUses
	}
	// M8-2 LLM defaults (dispatch-m8-2 §5): provider 空 → deepseek; the openai
	// and anthropic parameter fields fall back to their defaults so the config
	// can switch providers. The top-level model/base_url stay as the deepseek
	// default configuration (compatible existing config.yaml, not migrated).
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = DefaultLLMProvider
	}
	if cfg.LLM.OpenAI.BaseURL == "" {
		cfg.LLM.OpenAI.BaseURL = DefaultOpenAIBaseURL
	}
	if cfg.LLM.OpenAI.Model == "" {
		cfg.LLM.OpenAI.Model = DefaultOpenAIModel
	}
	if cfg.LLM.Anthropic.BaseURL == "" {
		cfg.LLM.Anthropic.BaseURL = DefaultAnthropicBaseURL
	}
	if cfg.LLM.Anthropic.Model == "" {
		cfg.LLM.Anthropic.Model = DefaultAnthropicModel
	}
	if cfg.LLM.Retry.MaxRetries <= 0 {
		cfg.LLM.Retry.MaxRetries = DefaultLLMMaxRetries
	}
	if cfg.LLM.Retry.InitialBackoff.Duration <= 0 {
		cfg.LLM.Retry.InitialBackoff.Duration = DefaultLLMRetryBackoff
	}
	if cfg.LLM.Retry.MaxBackoff.Duration <= 0 {
		cfg.LLM.Retry.MaxBackoff.Duration = DefaultLLMRetryMaxBackoff
	}
	// M8-3 multimodal defaults (dispatch-m8-3 §3): model_input_modalities 缺省
	// "text"；multimodal.max_image_bytes 缺省 10MiB（非正值钳到默认，校验非负: 负值
	// 永远不会到达接线层）。multimodal.enabled 缺省 true（用户 2026-08-20 拍板「图片附件
	// 默认打开」：*bool 区分「未设置 → 默认开」与显式 "enabled: false" → 关；minimal
	// 预设强制关 D-MODE-2）。
	if cfg.LLM.ModelInputModalities == "" {
		cfg.LLM.ModelInputModalities = DefaultModelInputModalities
	}
	if cfg.LLM.Multimodal.Enabled == nil {
		t := true
		cfg.LLM.Multimodal.Enabled = &t
	}
	if cfg.LLM.Multimodal.MaxImageBytes <= 0 {
		cfg.LLM.Multimodal.MaxImageBytes = DefaultMultimodalMaxImageBytes
	}
	if cfg.LLM.Multimodal.MaxRequestImageBytes <= 0 {
		cfg.LLM.Multimodal.MaxRequestImageBytes = DefaultMultimodalMaxRequestImageBytes
	}
	if cfg.Terminal.ScrollbackMaxBytes <= 0 {
		cfg.Terminal.ScrollbackMaxBytes = DefaultTerminalScrollbackMaxBytes
	}
	if cfg.Terminal.ScrollbackLines <= 0 {
		cfg.Terminal.ScrollbackLines = DefaultTerminalScrollbackLines
	}
	if cfg.Terminal.ReadIdleMS <= 0 {
		cfg.Terminal.ReadIdleMS = DefaultTerminalReadIdleMS
	}
	if cfg.Terminal.ReadTimeoutMS <= 0 {
		cfg.Terminal.ReadTimeoutMS = DefaultTerminalReadTimeoutMS
	}
	if cfg.Terminal.MaxConcurrentSessions <= 0 {
		cfg.Terminal.MaxConcurrentSessions = DefaultTerminalMaxConcurrent
	}
	if cfg.Terminal.Enabled == nil {
		cfg.Terminal.Enabled = Bool(true)
	}
	if cfg.Terminal.ACPEnabled == nil {
		cfg.Terminal.ACPEnabled = Bool(false)
	}
	// Enabling terminal whitelists its single consumer tool (pwsh), so the
	// one terminal.enabled switch turns the whole capability on (the
	// fresh-process pwsh tool + the /term REPL); default on (dsh 对齐 opt-out,
	// dispatch-m9-2 §2 — mirrors run_command).
	if Enabled(cfg.Terminal.Enabled) {
		for _, name := range terminalToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	// M-Eval eval defaults (dispatch-eval-3a §1 / ADR 2026-08-20-eval-seam.md
	// D-EVAL-6): manual_fallback is a pointer so an absent YAML field (→ the
	// default true) is distinguishable from an explicit false (which means LLM
	// undecided → fail); the history cap is 100; enabled defaults off (D10,
	// bool 零值即关). Enabling eval whitelists its three consumer tools, so the
	// single eval.enabled switch turns the whole capability on (mirrors
	// terminal).
	if cfg.Eval.ManualFallback == nil {
		v := true
		cfg.Eval.ManualFallback = &v
	}
	if cfg.Eval.MaxRecords <= 0 {
		cfg.Eval.MaxRecords = DefaultEvalMaxRecords
	}
	// D-GAP-1 fs-search defaults: enabled by default to match dsh base. Enabling fs_search
	// whitelists its two consumer tools grep and glob, so the one fs_search.
	// enabled switch turns the whole capability (search engine + tools) on
	// fs/web/terminal/eval).
	if cfg.FsSearch.Enabled == nil {
		cfg.FsSearch.Enabled = Bool(true)
	}
	if Enabled(cfg.FsSearch.Enabled) {
		for _, name := range fsSearchToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	// GAP-2 ralph defaults: the sample config enables it. Enabling ralph whitelists its
	// single consumer tool ralph, so the one ralph.enabled switch turns the
	// subagent/skill/schedule/plan/spill/interact/code/mcp/fs/web/terminal/eval/
	// fs_search).
	if Enabled(cfg.Ralph.Enabled) {
		for _, name := range ralphToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Ralph.MaxRounds <= 0 || cfg.Ralph.MaxRounds > DefaultRalphMaxRounds {
		cfg.Ralph.MaxRounds = DefaultRalphMaxRounds
	}
	// GAP-3 workflow defaults: the sample config enables it; the ready-task concurrency
	// cap is 4. Enabling workflow whitelists its single consumer tool
	// workflow_run, so the one workflow.enabled switch turns the whole
	// skill/schedule/plan/spill/interact/code/mcp/fs/web/terminal/eval/
	// fs_search/ralph). Non-positive caps are clamped to the default (校验非负).
	if Enabled(cfg.Workflow.Enabled) {
		for _, name := range workflowToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.Workflow.MaxConcurrent <= 0 {
		cfg.Workflow.MaxConcurrent = DefaultWorkflowMaxConcurrent
	}
	if cfg.Workflow.MaxTotalAgents <= 0 {
		cfg.Workflow.MaxTotalAgents = DefaultWorkflowMaxTotalAgents
	}
	if cfg.Workflow.MaxItemsPerCall <= 0 {
		cfg.Workflow.MaxItemsPerCall = DefaultWorkflowMaxItemsPerCall
	}
	if cfg.Workflow.SyncTimeoutMS <= 0 {
		cfg.Workflow.SyncTimeoutMS = DefaultWorkflowSyncTimeoutMS
	}
	// P2 session-query defaults: opt-in until the local store grows dsh's
	// workspace/parent authorization metadata. Enabling it whitelists all five
	// read-only consumers; the query package owns their schemas and bounds.
	if cfg.SessionQuery.Enabled {
		for _, name := range sessionQueryToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	if cfg.SessionQuery.MaxResults <= 0 || cfg.SessionQuery.MaxResults > 100 {
		cfg.SessionQuery.MaxResults = 20
	}
	if cfg.LSP.Enabled {
		for _, name := range lspToolNames {
			if !contains(cfg.Tools.Enabled, name) {
				cfg.Tools.Enabled = append(cfg.Tools.Enabled, name)
			}
		}
	}
	// P2 LSP defaults: explicit opt-in because it starts a configured stdio
	// language-server process. The default route keeps the common Go setup
	// useful while command selection remains user-owned.
	if cfg.LSP.TimeoutMS <= 0 {
		cfg.LSP.TimeoutMS = 60000
	}
	if cfg.LSP.MaxLocations <= 0 {
		cfg.LSP.MaxLocations = 100
	}
	if cfg.LSP.MaxResultChars <= 0 {
		cfg.LSP.MaxResultChars = 16000
	}
	if cfg.LSP.MaxDocumentBytes <= 0 {
		cfg.LSP.MaxDocumentBytes = 4 << 20
	}
	if len(cfg.LSP.Extensions) == 0 {
		cfg.LSP.Extensions = map[string]string{".go": "go"}
	}
	if cfg.Hooks.TimeoutMS <= 0 {
		cfg.Hooks.TimeoutMS = 10000
	}
	// M10a web portal defaults (ADR 2026-08-20-m10-web-portal.md): addr defaults
	// to the local-only personal portal; token is left for the composition root
	// to fail closed on when enabled.
	if cfg.WebServer.Addr == "" {
		cfg.WebServer.Addr = "127.0.0.1:8080"
	}
	// D-MODE-2 (ADR 2026-08-20-mode-presets.md): minimal 模式是预设优先 ——
	// 只保留持久 shell + 文件编辑 + M1 基础只读；其余能力 cap 全关、白名单
	// 整体重置为 minimal 集合。register* 的 D10 门读这些 Enabled, 因此注册面
	// 与白名单面自动收敛。standard/code 不触碰 (现状). 必须放在所有既有
	// append 之后, 否则后续 append 会把用户开启的其余工具加回白名单.
	ApplyModePreset(cfg)
	if cfg.Mode == ModeCode && !contains(cfg.Tools.Enabled, "run_code") {
		cfg.Tools.Enabled = append(cfg.Tools.Enabled, "run_code")
	}
}

// ApplyModePreset applies the D-MODE mode preset to cfg (ADR
// 2026-08-20-mode-presets.md). Minimal is preset-first and resets every
// capability switch plus the whole execution whitelist. Standard and PTC are
// selected per session; the composition root keeps their registered capability
// set available and the session runtime narrows the visible/executable tools.
func ApplyModePreset(cfg *Config) {
	if cfg.Mode == ModeCode {
		cfg.Code.Enabled = Bool(true)
		return
	}
	if cfg.Mode != ModeMinimal {
		return
	}
	cfg.Terminal.Enabled = Bool(true) // minimal 只保留持久 shell + 文件编辑 (D-MODE-2)
	cfg.Fs.Enabled = Bool(true)
	cfg.FsSearch.Enabled = Bool(false) // minimal 不含搜索 (D-MODE-2)
	cfg.SessionQuery.Enabled = false   // minimal 不含历史查询 (P2)
	cfg.Ralph.Enabled = Bool(false)    // minimal 不含 fresh-agent 循环 (D-MODE-2)
	cfg.Workflow.Enabled = Bool(false) // minimal 不含 workflow DAG 编排 (D-MODE-2)
	cfg.WebServer.Enabled = false      // minimal 不含 web 门户 (D-MODE-2)
	cfg.Jobs.Enabled = Bool(false)
	cfg.Subagent.Enabled = Bool(false)
	cfg.Compaction.Enabled = Bool(false)
	cfg.Skill.Enabled = Bool(false)
	cfg.Schedule.Enabled = Bool(false)
	cfg.Plan.Enabled = Bool(false)
	cfg.Spill.Enabled = Bool(false)
	cfg.Interact.Enabled = Bool(false)
	cfg.Code.Enabled = Bool(false)
	cfg.LSP.Enabled = false
	cfg.Hooks.Enabled = false
	cfg.Mcp.Enabled = Bool(false)
	cfg.Web.Enabled = Bool(false)
	cfg.Eval.Enabled = Bool(false)
	{
		v := false
		cfg.LLM.Multimodal.Enabled = &v // minimal 无多模态 (D-MODE-2)
	}
	cfg.Tools.RunCommand.Enabled = false
	cfg.Tools.Enabled = append([]string(nil), minimalEnabledTools...)
}

// jobsToolNames are the background-job consumer tools (dispatch-m5a-2 §2),
// including dsh's canonical output/kill/list projections. They are registered
// and whitelisted only when jobs is enabled; keeping the names here makes the
// "jobs.enabled ⇒ 工具自动白名单" rule a single, tested fact shared by
// applyDefaults and the composition root.
var jobsToolNames = []string{"job_output", "job_kill", "job_list"}

// subagentToolNames are the subagent consumer tools (dispatch-m5b-2 §2). They
// are registered and whitelisted only when subagent is enabled; keeping the
// names here makes the "subagent.enabled ⇒ 工具自动白名单" rule a single, tested
// fact shared by applyDefaults and the composition root.
var subagentToolNames = []string{"subagent", "subagent_fork", "send_message", "interrupt_agent"}

// skillToolNames are the skill consumer tools (dispatch-m5d-2 §2). skill
// is registered and whitelisted only when skill is enabled; keeping the name
// here makes the "skill.enabled ⇒ 工具自动白名单" rule a single, tested fact
// shared by applyDefaults and the composition root.
var skillToolNames = []string{"skill"}

// scheduleToolNames are the schedule consumer tools (dispatch-m6a-2 §3). They
// are registered and whitelisted only when schedule is enabled; keeping the
// names here makes the "schedule.enabled ⇒ 工具自动白名单" rule a single, tested
// fact shared by applyDefaults and the composition root.
var scheduleToolNames = []string{"schedule_create", "schedule_list", "schedule_delete"}

// goalToolNames and todoToolNames are the DSH goal/todo consumer tools.
var goalToolNames = []string{"get_goal", "create_goal", "update_goal"}
var todoToolNames = []string{"todo_write"}

// interactToolNames are the human-approval consumer tools (dispatch-m6d-2 §3).
// They are registered and whitelisted only when interact is enabled; keeping
// the names here makes the "interact.enabled ⇒ 工具自动白名单" rule a single,
// tested fact shared by applyDefaults and the composition root.
var interactToolNames = []string{"interact_ask", "ask_user_question", "interact_status"}

// codeToolNames are the code-sandbox consumer tools (dispatch-m6e-2 §2).
// run_code is registered and whitelisted only when code is enabled; keeping the
// name here makes the "code.enabled ⇒ 工具自动白名单" rule a single, tested fact
// shared by applyDefaults and the composition root.
var codeToolNames = []string{"run_code"}

// mcpToolNames are the MCP consumer tools (dispatch-m6f-2 §2). mcp_list and
// mcp_call are registered and whitelisted only when mcp is enabled; keeping the
// names here makes the "mcp.enabled ⇒ 工具自动白名单" rule a single, tested fact
// shared by applyDefaults and the composition root. Bridged server tools
// (mcp.<server>.<tool>) are dynamic and are whitelisted by the composition root
// as they are registered.
var mcpToolNames = []string{}

// fsToolNames are the safe-file-operation consumer tools (dispatch-m6f-3 §3).
// They are registered and whitelisted only when fs is enabled; keeping the
// names here makes the "fs.enabled ⇒ 工具自动白名单" rule a single, tested fact
// shared by applyDefaults and the composition root.
var fsToolNames = []string{"write", "edit"}

// fsSearchToolNames are the file-content-search consumer tools (D-GAP-1, 对齐
// dsh tool-fs-search). They are registered and whitelisted only when
// fs_search is enabled; keeping the names here makes the
// "fs_search.enabled ⇒ 工具自动白名单" rule a single, tested fact shared by
// applyDefaults and the composition root.
var fsSearchToolNames = []string{"grep", "glob"}

// sessionQueryToolNames are the five dsh-aligned read-only history consumers.
var sessionQueryToolNames = []string{"session_search", "session_event_search", "session_trace", "session_event_trace", "session_event_read"}

// lspToolNames is the single read-only language-server consumer.
var lspToolNames = []string{"lsp"}

// ralphToolNames are the fresh-agent-loop consumer tools (D-GAP-3). ralph is
// registered and whitelisted only when ralph is enabled; keeping the name here
// makes the "ralph.enabled ⇒ 工具自动白名单" rule a single, tested fact shared by
// applyDefaults and the composition root.
var ralphToolNames = []string{"ralph"}

// workflowToolNames are the task-DAG orchestration consumer tools (D-GAP-2).
// workflow is registered and whitelisted only when workflow is enabled;
// keeping the name here makes the "workflow.enabled ⇒ 工具自动白名单" rule a
// single, tested fact shared by applyDefaults and the composition root.
var workflowToolNames = []string{"workflow"}

// webToolNames are the web consumer tools (dispatch-m7-2 §5). They are
// registered and whitelisted only when web is enabled; keeping the names here
// makes the "web.enabled ⇒ 工具自动白名单" rule a single, tested fact shared by
// applyDefaults and the composition root.
var webToolNames = []string{"web_search", "web_fetch"}

// terminalToolNames are the pwsh consumer tools (ADR
// 2026-08-23-pwsh-dsh-alignment.md / dispatch-m9-2 §4). They are registered
// and whitelisted only when terminal is enabled; keeping the names here makes
// the "terminal.enabled ⇒ 工具自动白名单" rule a single, tested fact shared by
// applyDefaults and the composition root.
var terminalToolNames = append([]string{platformShellToolName()},
	"terminal_open", "terminal_list", "terminal_read", "terminal_send", "terminal_signal", "terminal_close",
)

func platformShellToolName() string {
	if runtime.GOOS == "windows" {
		return "pwsh"
	}
	return "bash"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
