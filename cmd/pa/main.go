// Command pa is the personal agent REPL (M1鈫扢3). It wires the thin core 鈥?llm,
// session, tools, prompt, loop 鈥?plus the durable store (design.md D8) and
// drives turns from stdin. Sessions persist to data_dir/pa.db and are resumed
// across restarts; /new, /list and /resume manage multiple sessions. M3 adds
// the tool-execution safety policy (whitelist, timeout, output truncation/spill)
// and --config. The DeepSeek API key is read from the DEEPSEEK_API_KEY
// environment variable only.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/acp"
	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/code"
	"github.com/jabing/shutu-agent/internal/compaction"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/eval"
	"github.com/jabing/shutu-agent/internal/fs"
	hookrunner "github.com/jabing/shutu-agent/internal/hooks"
	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/kb"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/loop"
	"github.com/jabing/shutu-agent/internal/mcp"
	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/prompt"
	"github.com/jabing/shutu-agent/internal/schedule"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/skill"
	"github.com/jabing/shutu-agent/internal/spill"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/subagent"
	"github.com/jabing/shutu-agent/internal/terminal"
	"github.com/jabing/shutu-agent/internal/tools"
	"github.com/jabing/shutu-agent/internal/web"
	"github.com/jabing/shutu-agent/internal/webserver"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	webOnly := flag.Bool("web-only", false, "serve the web portal only, without the REPL (blocks until interrupted)")
	acpMode := flag.Bool("acp", false, "serve ACP JSON-RPC over stdin/stdout")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}

	st, err := store.OpenSQLite(filepath.Join(cfg.DataDir, "pa.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	defer st.Close()

	// Runtime General-settings rows (durable in the SQLite settings table,
	// applied at startup; D-WEB2-D: config changes need a restart, no hot
	// reload). agent_preset overrides the mode preset (D-MODE), and
	// terminal_enabled the terminal switch 鈥?but a minimal preset keeps its
	// mandatory terminal (D-MODE-2). permission_preset is applied to the
	// execution whitelist after registration (see below).
	settings, err := st.GetSettings(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "pa: settings:", err)
		os.Exit(1)
	}
	permissionPreset := settings["permission_preset"] // "" | "readonly" | "standard" | "full"
	if v, ok := settings["agent_preset"]; ok &&
		(v == config.ModeMinimal || v == config.ModeStandard || v == config.ModeCode) {
		cfg.Mode = v
		config.ApplyModePreset(&cfg)
	}
	// The General-settings "default terminal" row picks the shell (dsh
	// Powershell / Git Bash / WSL). Any non-"off" choice enables the terminal
	// and maps to the platform shell executable; minimal keeps its forced
	// terminal (D-MODE-2) but still honors the chosen shell. A legacy
	// terminal_enabled on/off value is honored for databases written before
	// the shell row existed.
	if v, ok := settings["terminal_shell"]; ok {
		switch v {
		case "off":
			if cfg.Mode != config.ModeMinimal {
				cfg.Terminal.Enabled = config.Bool(false)
			}
		case "powershell":
			cfg.Terminal.Enabled = config.Bool(true)
			cfg.Terminal.Shell = "powershell.exe"
		case "gitbash":
			cfg.Terminal.Enabled = config.Bool(true)
			cfg.Terminal.Shell = "bash.exe"
		case "wsl":
			cfg.Terminal.Enabled = config.Bool(true)
			cfg.Terminal.Shell = "wsl.exe"
		}
	} else if v, ok := settings["terminal_enabled"]; ok && cfg.Mode != config.ModeMinimal {
		cfg.Terminal.Enabled = config.Bool(v == "true")
	}

	// M3: the Execute pipeline's safety policy 鈥?whitelist, deadline, output
	// cap with spill to <data_dir>/spill (design.md 搂5).
	reg := tools.New()
	pol := tools.PolicyFromConfig(cfg.Tools, cfg.DataDir)
	// The base policy keeps the FULL registered whitelist (dsh: the deployment
	// composition). Per-session agent presets (standard / PTC / minimal) are
	// projected onto it by applySessionRuntime before every turn, so
	// projecting here would destroy the base other sessions project from.
	// M6e-2: code.timeout is the outer per-tool deadline bound for run_code
	// (mirrors tools.run_command.timeout) 鈥?the config value, after
	// applyDefaults, is authoritative for sandbox runs.
	pol.CodeRun.Timeout = cfg.Code.Timeout.Duration
	reg.SetPolicy(pol)
	// The permission preset's readonly tier narrows the whitelist to the
	// read-only tools (whitelist semantics: a name not listed is rejected).
	if permissionPreset == "readonly" {
		pol.Enabled = config.ReadOnlyTools()
		reg.SetPolicy(pol)
	}
	// The read-only built-ins are always registered; the whitelist gates their
	// execution. The execution-class tool is registered only when enabled
	// (榛樿鍏抽棴, D10).
	if err := reg.Register(tools.GetTime{}); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if !config.Enabled(cfg.Fs.Enabled) {
		if err := reg.Register(tools.NewReadFile(cfg.Fs.Root)); err != nil {
			fmt.Fprintln(os.Stderr, "pa:", err)
			os.Exit(1)
		}
	}
	if cfg.Tools.RunCommand.Enabled {
		if err := reg.Register(tools.NewRunCommand(cfg.Tools.RunCommand.Workdir)); err != nil {
			fmt.Fprintln(os.Stderr, "pa:", err)
			os.Exit(1)
		}
	}

	promptBuilder, err := buildPrompt(cfg.Mode, cfg.PromptsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	promptBuilder.SetTools(func() []llm.ToolSchema { return toolSpecsForMode(cfg.Mode, reg.Specs()) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	app := &app{
		cfg:        cfg,
		store:      st,
		reg:        reg,
		prompt:     promptBuilder,
		basePolicy: pol,
		// M10 W1: the real-time event hub (ADR D-WEB2-B) exists for the whole
		// process lifetime so attachSink can broadcast every persisted event to
		// the web's SSE subscribers whenever the webserver is enabled.
		hub: NewEventHub(),
		// baseCtx = the process-lifetime signal ctx (see the field comment): the
		// persist sink and the web-only block live as long as the process.
		baseCtx: ctx,
	}
	// M11: load the provider API-key overrides (llm.key.<id>) and custom
	// OpenAI-compatible provider declarations (llm.custom.<route>) from the
	// durable settings table before registerLLM builds the registry. A
	// configured key wins over the env var (閰嶇疆鍚庝互閰嶇疆鐨勪负鍑? user 2026-09).
	for k, v := range settings {
		if strings.HasPrefix(k, "llm.key.") {
			if app.llmKeys == nil {
				app.llmKeys = map[string]string{}
			}
			app.llmKeys[strings.TrimPrefix(k, "llm.key.")] = v
		} else if strings.HasPrefix(k, "llm.custom.") {
			var cp customProviderProfile
			if json.Unmarshal([]byte(v), &cp) == nil && cp.ID != "" && cp.Name != "" {
				app.customProviders = append(app.customProviders, cp)
			}
		} else if strings.HasPrefix(k, "llm.profile.") {
			var bp builtinProviderProfile
			if json.Unmarshal([]byte(v), &bp) == nil {
				if app.builtinProfiles == nil {
					app.builtinProfiles = map[string]builtinProviderProfile{}
				}
				app.builtinProfiles[strings.TrimPrefix(k, "llm.profile.")] = bp
			}
		}
	}
	// M8-2: registerLLM builds the provider registry and injects the selected
	// provider into a.llm 鈥?the single llm.LLM the loop, compaction, subagent
	// and kb extraction all consume (D2). It must run before registerSubagent /
	// registerCompaction / registerKB, which read a.llm at wiring time.
	if err := app.registerLLM(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// M8-3: wire the image-attachment store 鈥?under <data_dir>/attachments/ 鈥?	// when llm.multimodal.enabled (榛樿鍏?D10). disabled 鈬?/attach unavailable.
	if err := app.registerAttachments(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// M4b: wire the knowledge base seam 鈥?provider + kb_* tools + catalog 鈥?	// when kb.enabled (榛樿鍏抽棴, D10). kb.registerKB appends the kb_* tool names
	// to nothing itself; config.applyDefaults already whitelisted them when
	// kb.enabled was true.
	if err := app.registerKB(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.kb != nil {
		defer app.kb.Close()
	}
	// M5a-2: wire the jobs seam 鈥?Local registry + the five job_* tools + the
	// D3 event sink 鈥?when jobs.enabled (榛樿鍏抽棴, D10). config.applyDefaults
	// already whitelisted the job_* names when jobs.enabled was true. The
	// deferred Close cancels and awaits every live background job at shutdown
	// so no goroutine leaks (lifecycle reversible, ADR 鍐崇瓥 鈶?.
	if err := app.registerJobs(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.jobs != nil {
		defer app.jobs.Close()
	}
	// M5b-2: wire the subagent seam 鈥?spawn provider + Runtime + the four
	// subagent_* tools + the D3 event sink 鈥?when subagent.enabled (榛樿鍏抽棴,
	// D10). config.applyDefaults already whitelisted the subagent_* names when
	// subagent.enabled was true. The deferred Close cancels and awaits every
	// live child at shutdown so no background goroutine leaks (lifecycle
	// reversible, ADR 鍐崇瓥 鈶?.
	if err := app.registerSubagent(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.subagents != nil {
		defer app.subagents.Close()
	}
	// GAP-2: wire the ralph fresh-agent loop seam (ADR
	// 2026-08-20-standard-gaps.md D-GAP-3, 瀵归綈 dsh tool-ralph) 鈥?the ralph
	// tool 鈥?when ralph.enabled (榛樿鍏?D10). config.applyDefaults already
	// whitelisted ralph when ralph.enabled was true. registerRalph runs after
	// registerSubagent because its spawn closure depends on a.subagents (the
	// subagent Runtime); it holds no closable resources, so there is no deferred
	// Close. Each round spawns a fresh child and blocks until it settles on the
	// serial tool path (D5); the loop's turn/step structure is untouched (D4).
	if err := app.registerRalph(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// GAP-3: wire the task-DAG orchestration seam (ADR
	// 2026-08-20-standard-gaps.md D-GAP-2, 鐢ㄦ埛鎷嶆澘 JSON DAG 澹版槑寮忕紪鎺? 鈥?the
	// workflow_run tool 鈥?when workflow.enabled (榛樿鍏?D10). config.applyDefaults
	// already whitelisted workflow_run when workflow.enabled was true.
	// registerWorkflow runs after registerSubagent because its spawn closure
	// depends on a.subagents (the subagent Runtime); it holds no closable
	// resources, so there is no deferred Close. Each task spawns a child and
	// blocks until it settles on the serial tool path (D5); the loop's
	// turn/step structure is untouched (D4).
	if err := app.registerWorkflow(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// M5c-2b: wire the compaction seam 鈥?BasicEngine for the /compact command
	// and the loop "compaction" pre-step injector 鈥?when compaction.enabled
	// (榛樿鍏抽棴, D10). Compaction whitelists no consumer tools (it has none:
	// automatic triggering runs through the loop pre-step injector, manual
	// through the /compact command), so config.applyDefaults already handled
	// the whole gate. The engine shares the caller-owned LLM and holds no
	// closable resources, so there is no deferred Close.
	if err := app.registerCompaction(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// M5d-2: wire the skill seam 鈥?filesystem provider + Registry + the
	// skill_load tool + the "skill" pre-step catalog injector 鈥?when
	// skill.enabled (榛樿鍏抽棴, D10). config.applyDefaults already whitelisted
	// skill_load when skill.enabled was true. The deferred Close releases the
	// registry and its providers at shutdown (lifecycle reversible, ADR
	// 鍐崇瓥 鈶?.
	if err := app.registerSkills(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.skills != nil {
		defer app.skills.Close()
	}
	// M6a-2: wire the schedule seam 鈥?in-memory Provider + Engine + the three
	// schedule_* tools + the D3 event sink + the "schedule" pre-step injector
	// 鈥?when schedule.enabled (榛樿鍏抽棴, D10). config.applyDefaults already
	// whitelisted the schedule_* names when schedule.enabled was true. The
	// deferred Close releases the provider and rejects further operations at
	// shutdown (lifecycle reversible, ADR 鍐崇瓥 M6a). There is no background
	// ticker: the loop's per-turn "schedule" pre-step injector advances the
	// clock on the serial path (D5).
	if err := app.registerSchedules(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.schedules != nil {
		defer app.schedules.Close()
	}
	// M6b-2: wire the plan seam 鈥?in-memory Provider + Engine + the six
	// plan_* tools + the D3 event sink 鈥?when plan.enabled (榛樿鍏抽棴, D10).
	// config.applyDefaults already whitelisted the plan_* names when
	// plan.enabled was true. The deferred Close releases the provider and
	// rejects further operations at shutdown (lifecycle reversible, ADR
	// 鍐崇瓥 M6b). The plan tree is a planning model only 鈥?execution delegation
	// to subagents is deferred to M6c+.
	if err := app.registerPlans(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.plans != nil {
		defer app.plans.Close()
	}
	// M6c-2: wire the spill seam 鈥?in-memory Provider + Engine + the four
	// spill_* tools + the D3 event sink + the turn-completion auto-sedimentation
	// hook 鈥?when spill.enabled (榛樿鍏抽棴, D10). config.applyDefaults already
	// whitelisted the spill_* names when spill.enabled was true. The deferred
	// Close releases the provider and rejects further operations at shutdown
	// (lifecycle reversible, ADR 鍐崇瓥 M6c). AutoSpill runs on the serial
	// turn-completion path (after each completed turn in the REPL, D5); there
	// is no background goroutine.
	if err := app.registerSpills(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.spills != nil {
		defer app.spills.Close()
	}
	// M6e-2: wire the code seam 鈥?local subprocess Provider + Engine + the
	// run_code tool + the D3 event sink 鈥?when code.enabled (榛樿鍏抽棴, D10).
	// config.applyDefaults already whitelisted run_code when code.enabled was
	// true. registerCode runs before registerInteracts so the sensitive-tool
	// gate can wrap run_code too. The deferred Close releases the provider and
	// rejects further runs at shutdown (lifecycle reversible, ADR 鍐崇瓥 M6e).
	// run_code executes on the serial tool path (D5) 鈥?no background goroutine.
	if err := app.registerCode(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.code != nil {
		defer app.code.Close()
	}
	// M6f-2: wire the MCP tool-ecosystem seam 鈥?stdio Factory + the
	// mcp_list/mcp_call tools + per-server tool bridging (mcp.<server>.<tool>)
	// + the D3 event sink 鈥?when mcp.enabled (榛樿鍏抽棴, D10). config.
	// applyDefaults already whitelisted mcp_list/mcp_call when mcp.enabled was
	// true; bridged names are whitelisted as each server tool is registered.
	// registerMcps runs before registerInteracts so the sensitive-tool gate can
	// wrap the mcp tools too. The deferred Close terminates every bridged
	// server at shutdown (lifecycle reversible, ADR 鍐崇瓥 M6f). Bridging and the
	// mcp_* tools execute on the serial tool path (D5) 鈥?no background
	// goroutine.
	if err := app.registerMcps(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if len(app.mcp) > 0 {
		defer func() {
			for _, c := range app.mcp {
				_ = c.Close()
			}
		}()
	}
	// M6f-3: wire the safe-file-operation seam 鈥?local FileService (root =
	// fs.root, defaulting to <project>) + the three fs_* tools + the D3 event
	// sink 鈥?when fs.enabled (榛樿鍏抽棴, D10). config.applyDefaults already
	// whitelisted the fs_* names when fs.enabled was true. registerFs runs
	// before registerInteracts so the sensitive-tool gate can wrap the fs tools
	// too. The deferred Close marks the service closed (idempotent, no OS
	// resources) at shutdown (lifecycle reversible, ADR 鍐崇瓥 M6f). The fs_*
	// tools execute on the serial tool path (D5) 鈥?no background goroutine.
	if err := app.registerFs(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.fs != nil {
		defer app.fs.Close()
	}
	// D-GAP-1: wire the file-content-search seam 鈥?the grep/glob tools (dsh tool-fs-search contract) 鈥?when
	// fs_search.enabled (榛樿鍏?D10). config.applyDefaults already whitelisted
	// fs_search when fs_search.enabled was true. The tools are read-only and
	// holds no resources, so there is no deferred Close; the default search
	// root is the agent working directory (os.Getwd, like internal/code and
	// internal/skill). They execute on the serial tool path (D5).
	if err := app.registerFsSearch(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// P2 session-query: wire five read-only history tools when enabled.
	if err := app.registerSessionQuery(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if err := app.registerLSP(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if err := app.registerHooks(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	defer app.closeHooks()
	// M7-2: wire the web seam 鈥?Engine + DeepSeek search provider (env key
	// only) + HTTP fetch provider + the two web_* tools 鈥?when web.enabled
	// (榛樿鍏抽棴, D10). config.applyDefaults already whitelisted web_search/
	// web_fetch when web.enabled was true. registerWeb runs before
	// registerInteracts so the sensitive-tool gate can wrap the web tools too.
	// The Engine holds no closable resources, so there is no deferred Close.
	// web/search-request is logged by the provider's OnRequest (D3); the web_*
	// tools execute on the serial tool path (D5) 鈥?no background goroutine.
	if err := app.registerWeb(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	// M9/dsh: wire the pwsh seam 鈥?the fresh-process pwsh tool (dsh
	// tool-pwsh: one `pwsh -Command` process per call, no state between
	// calls) + the /term REPL over the M9 persistent session 鈥?when
	// terminal.enabled (榛樿鍏?D10). config.applyDefaults already whitelisted
	// pwsh when terminal.enabled was true. The deferred cleanup closes the
	// active /term session at shutdown so no child process leaks.
	if err := app.registerTerminal(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	defer func() {
		if app.termSess != nil {
			app.termSess.Close()
		}
	}()
	// M6d-2: wire the interact seam 鈥?in-memory Provider + Engine + the two
	// interact_* tools + the D3 event sink + the sensitive-tool gate 鈥?when
	// interact.enabled (榛樿鍏抽棴, D10). config.applyDefaults already whitelisted
	// the interact_* names when interact.enabled was true. registerInteracts
	// must run after every other register* so the sensitive-tool gate can wrap
	// the full registered tool set. The deferred Close releases the provider
	// and rejects further operations at shutdown (lifecycle reversible, ADR
	// 鍐崇瓥 M6d). The gate reads the user's y/n answer on the CLI serial path
	// (D5) 鈥?no background goroutine.
	if err := app.registerInteracts(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.interacts != nil {
		defer app.interacts.Close()
	}
	// The permission preset's "full" tier opens the whitelist to every
	// registered (and therefore enabled) tool, applied only after all
	// register* calls so reg.Specs() is complete (bridged MCP tools included).
	if permissionPreset == "full" {
		var all []string
		for _, s := range reg.Specs() {
			all = append(all, s.Name)
		}
		pol.Enabled = all
		reg.SetPolicy(pol)
	}
	// eval: wire the task-evaluation seam 鈥?the CompositeEvaluator (rule 鈫?LLM
	// judge 鈫?human fallback) over a.llm/a.interacts + the three eval_* tools +
	// the /eval-status command + the D3 event sink 鈥?when eval.enabled (榛樿鍏?	// D10). config.applyDefaults already whitelisted the eval_* names when
	// eval.enabled was true. The engine is in-memory; Close is idempotent.
	if err := app.registerEval(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.evalEng != nil {
		defer app.evalEng.Close()
	}
	// M10a: wire the unified web portal (ADR 2026-08-20-m10-web-portal.md) 鈥?	// the bearer-authenticated net/http server over the read-only store
	// (sessions/events browsing + static vanilla-JS frontend) 鈥?when
	// web_server.enabled (榛樿鍏?D10, no listener at all). An empty token fails
	// closed at startup (no bare server). The deferred Close shuts the listener
	// at shutdown so no port lingers.
	if *acpMode {
		server := &acp.Server{
			Factory:      &acpFactory{app: app},
			In:           os.Stdin,
			Out:          os.Stdout,
			AgentName:    "shutu-agent",
			AgentVersion: "0.1",
		}
		if err := server.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "pa: acp:", err)
			os.Exit(1)
		}
		return
	}
	if err := app.registerWebServer(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	if app.webserver != nil {
		defer app.webserver.Close()
	}
	if err := app.startup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
	app.startGoalScheduler(ctx)
	defer func() {
		if app.goalScheduler != nil {
			_ = app.goalScheduler.Close()
		}
	}()
	// M10 W3: --web-only serves the portal without the REPL (dsh-style
	// standalone web). The signal ctx cancels on Ctrl+C/Ctrl+Break; the
	// deferred webserver.Close shuts the listener so no port lingers.
	if *webOnly {
		if app.webserver == nil {
			fmt.Fprintln(os.Stderr, "pa: --web-only requires web_server.enabled=true in config")
			os.Exit(1)
		}
		<-ctx.Done()
	} else {
		app.repl(ctx)
	}
}

// app holds the REPL's mutable session state.
type app struct {
	cfg    config.Config
	store  store.Store
	reg    *tools.Registry
	prompt *prompt.Builder
	// basePolicy is the startup Execute policy (global mode + permission
	// preset). Per-session permission tiers swap a derived policy around a turn
	// (turnMu serializes turns) and restore basePolicy afterwards.
	basePolicy tools.Policy
	// promptByMode caches a per-mode system-prompt builder (Phase 2: 鎸変細璇?	// mode 閿佸畾). Populated lazily on first use for a non-global session mode.
	promptByMode map[string]*prompt.Builder
	llm          llm.LLM
	// llmMu guards the llm/llmReg pointer swap during the live model switch
	// (POST /api/config/model, P5.1): the switch holds turnMu (D5 serial, so no
	// turn is in flight) and takes the write lock; consumers read the selected
	// provider through currentLLM() (RLock). The zero value is ready.
	llmMu sync.RWMutex
	// baseCtx is the process-lifetime context (the main signal context). It is
	// the ctx the persist sink uses (attachSink), decoupled from any HTTP
	// request ctx: webSessionManager/webMessage pass r.Context() into
	// newSession/resumeSession, whose handler returns and cancels it 鈥?had the
	// sink captured that, every later append would fail with "context canceled"
	// (M10 W3 real-smoke catch). It also governs the web-only <-ctx.Done().
	baseCtx context.Context
	// llmReg is the M8-2 provider registry (dispatch-m8-2 搂6): registerLLM
	// builds it and injects the selected provider into llm; /llm-status reads
	// it. Non-nil only after registerLLM succeeds.
	llmReg *llm.Registry
	// llmKeys is the M11 provider API-key override map (settings rows
	// llm.key.<providerId>), loaded at startup and updated by the Model-settings
	// page's save endpoint. A configured key wins over the env var (閰嶇疆鍚庝互閰嶇疆鐨?	// 涓哄噯, user 2026-09); providerKey consults it first. nil 鈬?env-only.
	llmKeys map[string]string
	// customProviders is the M11 custom OpenAI-compatible provider declarations
	// (settings rows llm.custom.<route> = JSON customProviderProfile), loaded at
	// startup and updated by the provider-save endpoint. registerLLM registers
	// each under its route.
	customProviders []customProviderProfile
	// builtinProfiles is the per-built-in-provider override map (settings rows
	// llm.profile.<id> = JSON builtinProviderProfile, dsh ProviderEditor
	// 鑷畾涔夎缃?瀵归綈): base URL / model / model-list overrides for the
	// config-driven built-ins (deepseek-official). Loaded at startup and updated
	// by the provider-save endpoint; registerLLM applies them over config.yaml.
	builtinProfiles map[string]builtinProviderProfile
	// attachStore is the M8-3 image-attachment store (dispatch-m8-3 搂4): created
	// by registerAttachments only when llm.multimodal.enabled; nil when disabled
	// (D10) 鈥?/attach then errors.
	attachStore *attachment.Store
	kb          kb.KB // nil when kb disabled (D10)

	currentID string
	log       *session.Log
	jobs      *jobs.Local      // nil when jobs disabled (D10)
	subagents subagent.Runtime // nil when subagent disabled (D10)

	compaction compaction.Engine // nil when compaction disabled (D10)
	skills     skill.Registry    // nil when skill disabled (D10)
	// skillCatalogVersion is the digest of the last skill/catalog event logged
	// (dsh digest semantics): the catalog event fires once per catalog, and
	// again only when the catalog changes — never every turn.
	skillCatalogVersion string
	// skillManager is the web settings-page skill manager (dsh-skill-mcp-panel
	// 瀵归綈). It is created whenever the web server runs 鈥?independent of
	// skill.enabled 鈥?so the 鎶€鑳?settings page can list/enable/disable/delete/
	// add/migrate skill files even when the model-facing skill capability is off.
	skillManager *skill.Manager
	// titleMu guards titleDone, the per-process set of sessions whose
	// asynchronous model title has already been attempted (dsh-session-title
	// alignment): the model title fires at most once per session per process,
	// so a failed run never re-fires on every later turn.
	titleMu       sync.Mutex
	titleDone     map[string]bool
	schedules     schedule.Engine // nil when schedule disabled (D10)
	goalScheduler *schedule.DurableScheduler
	scheduleRunMu sync.Mutex
	scheduleWake  chan struct{}
	plans         plan.Engine     // nil when plan disabled (D10)
	spills        spill.Engine    // nil when spill disabled (D10)
	interacts     interact.Engine // nil when interact disabled (D10)
	code          code.Engine     // nil when code disabled (D10)
	mcp           []mcp.Client    // nil when mcp disabled (D10); one live bridged client per configured server
	fs            fs.FileService  // nil when fs disabled (D10)
	web           *web.Engine     // nil when web disabled (D10)
	hooks         *hookrunner.Runner

	// webserver is the M10a unified web portal (ADR 2026-08-20-m10-web-portal.md);
	// nil when web_server disabled (D10).
	webserver *webserver.Server

	// turnMu serializes every turn (D5): the REPL and the web message API share
	// one loop, so at most one Run executes at any moment (M10 W1, D-WEB2-A).
	turnMu sync.Mutex
	// cancelMu + turnCancel let POST /api/sessions/{id}/stop abort the web turn
	// (dsh 鍋滄鎸夐挳) without holding turnMu: the web message handler registers its
	// cancellable context here, and the stop handler calls the stored cancel.
	cancelMu   sync.Mutex
	turnCancel context.CancelFunc
	// runningSession is the session id whose turn is currently in flight, or ""
	// when idle. It is published by runTurn under turnMu and read atomically by
	// the sidebar status provider, so the webserver always sees a consistent
	// "running" dot without touching a.currentID (which other handlers mutate).
	// dsh-session-status alignment.
	runningSession atomic.Value
	// hub is the M10 W1 real-time event broadcaster (ADR D-WEB2-B): attachSink
	// publishes each persisted event of the current session, and the web's SSE
	// streams subscribe per session id. Always created in main; attachSink also
	// guards nil so tests constructing bare apps stay safe.
	hub *eventHub

	// evalEng is the task-evaluation engine (ADR 2026-08-20-eval-seam.md);
	// nil when eval disabled (D10).
	evalEng eval.Engine

	// M9 persistent terminal (dispatch-m9-2): the single active session
	// backing the /term REPL command. Nil when terminal disabled (D10) or no
	// session started; termOwner fences access to the starting session. The
	// model-facing pwsh tool is a fresh process per call and never touches
	// this session.
	termSess  *terminal.Session
	termOwner string

	// approveInput feeds the sensitive-tool gate's y/n read (nil => os.Stdin).
	// It exists so the wiring tests can inject canned approval answers; in the
	// REPL the gate reads the user's answer directly from the terminal on the
	// serial path (D5).
	approveInput io.Reader

	// mcpFactory builds MCP clients for the mcp_* tools and the server bridge;
	// nil uses mcp.NewStdioFactory(). It exists so the wiring tests can inject
	// a fake factory pointed at an in-memory fake server.
	mcpFactory mcp.Factory

	// skillProjectRoot / skillUserHome override the filesystem skill provider's
	// project/user roots when non-empty; empty uses the provider defaults (the
	// working directory and the user home). They exist so the wiring tests can
	// pin deterministic roots.
	skillProjectRoot string
	skillUserHome    string
}

// startup resumes the most recently updated session, or starts a fresh one.
func (a *app) startup(ctx context.Context) error {
	sessions, err := a.store.ListSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		if err := a.newSession(ctx); err != nil {
			return err
		}
		fmt.Printf("started new session %s\n", a.currentID)
		return nil
	}
	// ListSessions returns most recently updated first.
	last := sessions[0]
	if err := a.resumeSession(ctx, last.ID); err != nil {
		return err
	}
	fmt.Printf("resumed session %s (%d events)\n", a.currentID, len(a.log.Events()))
	return nil
}

// pruneBlankCurrent removes the current session from the store when it holds no
// events (nothing submitted). dsh discards a blank session once the user leaves
// it 鈥?a new-session hero never accumulates empty rows in the sidebar. It is a
// no-op when there is no current session or it already has content. Best-effort:
// a blank session has no durable value to lose, so a delete failure is logged
// rather than failing the switch.
func (a *app) pruneBlankCurrent(ctx context.Context) {
	if a.currentID == "" || a.log == nil || len(a.log.Events()) != 0 {
		return
	}
	if err := a.store.DeleteSession(ctx, a.currentID); err != nil {
		fmt.Fprintf(os.Stderr, "pa: prune blank session %q: %v\n", a.currentID, err)
	}
}

// newSession starts a fresh session with a random id.
func (a *app) newSession(ctx context.Context) error {
	id, err := newSessionID()
	if err != nil {
		return fmt.Errorf("pa: generate session id: %w", err)
	}
	// dsh: starting a fresh session discards an abandoned blank one.
	a.pruneBlankCurrent(ctx)
	if err := a.store.CreateSession(ctx, id, time.Now().UTC()); err != nil {
		return err
	}
	a.currentID = id
	a.log = session.New()
	if err := a.restorePlans(); err != nil {
		return err
	}
	if err := a.restoreGoalScheduler(); err != nil {
		return err
	}
	a.attachSink(ctx)
	a.bindSpillOwner()
	a.markSessionViewed(ctx, id)
	return nil
}

// resumeSession loads a session's full history from the store into a new log.
func (a *app) resumeSession(ctx context.Context, id string) error {
	events, err := a.store.LoadSession(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no such session %q (see /list)", id)
		}
		return err
	}
	// dsh: switching from a blank session to another one discards the empty row.
	// Guarded here (after a successful load) so a failed switch never deletes
	// the session the user is still on.
	if id != a.currentID {
		a.pruneBlankCurrent(ctx)
	}
	a.currentID = id
	a.log = session.New()
	if err := a.log.Restore(events); err != nil {
		return err
	}
	if err := a.restoreGoalScheduler(); err != nil {
		return err
	}
	if err := a.restorePlans(); err != nil {
		return err
	}
	a.attachSink(ctx)
	a.bindSpillOwner()
	// Opening a session clears its finished-but-unviewed reminder (dsh
	// status.completed is cleared by opening).
	a.markSessionViewed(ctx, id)
	return nil
}

// attachSink forwards every appended event to the durable store for the
// current session (D8: append-on-write, replay at startup) and 鈥?when the
// real-time hub exists 鈥?broadcasts it to the session's SSE subscribers
// (M10 W1, ADR D-WEB2-B). Publish is non-blocking and never fails, so the
// store's error semantics are unchanged: a store error still rolls the event
// back out of the log.
// attachSink forwards every appended event to the durable store for the
// current session (D8: append-on-write, replay at startup). The persist ctx is
// a.baseCtx 鈥?the process-lifetime context, NOT the caller's request ctx 鈥?// because the sink outlives any single HTTP request (M10 W3): a request ctx
// is cancelled when its handler returns, which would fail every later append
// with "context canceled" (real-smoke catch). The passed ctx is ignored for
// persistence (kept in the signature for the call sites).
func (a *app) attachSink(ctx context.Context) {
	id := a.currentID
	// Persist under the process-lifetime ctx. baseCtx is always set by main;
	// tests that build an app directly fall back to Background so a nil ctx can
	// never reach database/sql (which panics on a nil context).
	pctx := a.baseCtx
	if pctx == nil {
		pctx = context.Background()
	}
	a.log.SetSink(func(ev session.Event) error {
		if err := a.store.AppendEvents(pctx, id, []session.Event{ev}); err != nil {
			return err
		}
		if a.hub != nil {
			a.hub.Publish(id, ev)
		}
		if a.hooks != nil {
			a.hooks.Notify(id, ev)
		}
		return nil
	})
}

// bindSpillOwner points the tool registry's spill naming at the active
// session. The next-seq closure is pinned to the current log, so a spill is
// named <session>-<seq>.txt with the exact seq of the tool/result event that
// will carry the locator (M3). Called on every session switch.
func (a *app) bindSpillOwner() {
	log := a.log
	a.reg.SetOwner(tools.Owner{
		SessionID: a.currentID,
		NextSeq:   func() uint64 { return log.NextSeq() },
	})
}

// newLoop builds a Loop bound to the current session log. The Recall hook is
// the M4b proactive-recall extension point (dispatch-m4b 搂2): it runs the
// per-turn recall orchestration in cmd/pa; the loop's turn/step structure is
// unchanged (D4).
func (a *app) newLoop() *loop.Loop {
	return a.buildLoop(
		func(delta string) { fmt.Print(delta) },
		func(err error) { fmt.Fprintln(os.Stderr, "\n[stream error]", err) },
		"", a.cfg.Model, a.cfg.ReasoningEffort, a.cfg.Mode, a.prompt,
	)
}

// newLoopWeb builds a Loop identical to newLoop except its stream hooks are
// silent (interactive=false, M10 W1): the web frontend renders the stream from
// the SSE event flow (each chunk is already persisted by the loop), so nothing
// may be printed to the REPL's stdout/stderr during a web turn.
func (a *app) newLoopWeb() *loop.Loop {
	return a.buildLoop(func(string) {}, func(error) {}, "", a.cfg.Model, a.cfg.ReasoningEffort, a.cfg.Mode, a.prompt)
}

// newLoopFor builds a Loop bound to the current session log using the resolved
// per-session runtime (Phase 2: 按会话 model/mode; dsh ModelSelection 对齐:
// the session's provider override routes its turns, and its mode preset owns
// the model-facing tool surface). interactive selects the REPL or silent stream
// hooks.
func (a *app) newLoopFor(rt sessionRuntime, interactive bool) *loop.Loop {
	if interactive {
		return a.buildLoop(
			func(delta string) { fmt.Print(delta) },
			func(err error) { fmt.Fprintln(os.Stderr, "\n[stream error]", err) },
			rt.provider, rt.model, rt.effort, rt.mode, rt.prompt,
		)
	}
	return a.buildLoop(func(string) {}, func(error) {}, rt.provider, rt.model, rt.effort, rt.mode, rt.prompt)
}

// buildLoop assembles a Loop bound to the current session log. onText/onError
// are the streaming hooks: the REPL prints them, the web path is silent.
// provider/model/effort override the globals when a per-session selection is
// active (dsh ModelSelection: the session owns provider+model+effort); an
// unknown provider id falls back to the global LLM (fail-open). mode is the
// session's agent preset (standard | code | minimal): it owns the model-facing
// tool surface (loop.Config.ToolSpecs). pb overrides the system prompt when a
// per-session mode is active. effort is the thinking-effort selection ("" keeps
// the provider default).
func (a *app) buildLoop(onText func(string), onError func(error), provider, model, effort, mode string, pb *prompt.Builder) *loop.Loop {
	if provider == "" {
		provider = a.cfg.LLM.Provider
	}
	if model == "" {
		model = a.cfg.Model
	}
	if pb == nil {
		pb = a.prompt
	}
	if mode == "" {
		mode = a.cfg.Mode
	}
	ll := a.llmFor(provider)
	return loop.New(loop.Config{
		LLM:             ll,
		Log:             a.log,
		Tools:           a.reg,
		ToolSpecs:       func() []llm.ToolSchema { return toolSpecsForMode(mode, a.reg.Specs()) },
		Prompt:          pb,
		Model:           model,
		Provider:        provider,
		ReasoningEffort: effort,
		Recall:          a.recall,
		// M5c-2b: the "compaction" pre-step injector (auto token-pressure
		// compaction) is appended when compaction is enabled; it runs after the
		// M4b recall hook, inside the loop's existing pre-step extension point
		// (D4 — the turn/step structure is unchanged).
		PreStep: a.preStepInjectors(),
		OnText:  onText,
		OnError: onError,
	})
}

// sessionRuntime is the resolved per-turn runtime for one session: the
// effective LLM provider, model, thinking effort, the mode preset and the
// system-prompt builder (by mode). All fields are "" / nil when the session
// falls back to the globals (dsh ModelSelection: provider+model+effort are one
// selection; the mode defaults to the deployment preset).
type sessionRuntime struct {
	provider string
	model    string
	effort   string
	mode     string
	prompt   *prompt.Builder
}

// applySessionRuntime resolves one session's per-turn provider/model/effort /
// mode-prompt / permission tier (session override ?? global) and swaps the
// registry policy to the session's mode-projected whitelist. runTurn holds
// turnMu while it runs, so the swap is serialized with the turn; the returned
// restore func reinstates the base policy. Fail-open: any store or builder
// error falls back to the globals.
func (a *app) applySessionRuntime(id string) (sessionRuntime, func()) {
	rt := sessionRuntime{model: a.cfg.Model, effort: a.cfg.ReasoningEffort, prompt: a.prompt}
	perm := ""
	mode := a.cfg.Mode
	if scs, ok := a.store.(store.SessionConfigStore); ok && id != "" {
		if cfg, err := scs.GetSessionConfig(context.Background(), id); err == nil {
			if cfg.Provider != "" {
				rt.provider = cfg.Provider
			}
			if cfg.Model != "" {
				rt.model = cfg.Model
			}
			if cfg.ReasoningEffort != "" {
				rt.effort = cfg.ReasoningEffort
			}
			if cfg.AgentPreset != "" {
				mode = cfg.AgentPreset // 会话创建时锁定的模式 (dsh agent preset)
				rt.prompt = a.promptFor(mode)
			}
			perm = cfg.Permission
		}
	}
	rt.mode = mode
	if a.log != nil && session.FoldPlanMode(a.log.Events()) {
		rt.prompt = rt.prompt.Clone().Add(prompt.Section{Name: "plan-mode", Order: 900, Text: planModeSection})
	}
	// Every turn projects the session's mode onto the full base whitelist and
	// swaps it in for the turn's duration (dsh: the executor honors the same
	// presentation mode; standard never executes run_code, PTC only run_code,
	// minimal only its fixed seam).
	base := a.basePolicy
	base.Enabled = modeToolWhitelist(mode, base.Enabled)
	pol, _ := a.sessionPolicyFrom(base, perm, mode)
	a.reg.SetPolicy(pol)
	return rt, func() { a.reg.SetPolicy(a.basePolicy) }
}

func (a *app) sessionPolicyFrom(base tools.Policy, perm, mode string) (tools.Policy, bool) {
	switch perm {
	case "readonly":
		base.Enabled = config.ReadOnlyTools()
		return base, true
	case "full":
		base.Enabled = modeToolWhitelist(mode, a.allRegisteredToolNames())
		return base, true
	default:
		return base, false
	}
}

// modeToolWhitelist projects the model-facing tool surface for a mode. Native
// standard keeps registered tools, PTC exposes only run_code, and minimal keeps
// its fixed terminal/file seam. The executor receives the same projection, so a
// session cannot call a tool hidden from its model surface.
func modeToolWhitelist(mode string, enabled []string) []string {
	switch mode {
	case config.ModeCode:
		return []string{"run_code"}
	case config.ModeMinimal:
		return config.MinimalTools()
	default:
		out := make([]string, 0, len(enabled))
		for _, name := range enabled {
			if name != "run_code" {
				out = append(out, name)
			}
		}
		return out
	}
}

// toolSpecsForMode projects the model-facing tool schemas for a mode: PTC
// sends only run_code, minimal only its fixed seam, standard every registered
// tool except run_code. Both the wire tools array (loop.Config.ToolSpecs) and
// the prompt catalog must agree on this projection, so the model can never
// call a tool its mode hides.
func toolSpecsForMode(mode string, specs []llm.ToolSchema) []llm.ToolSchema {
	var allowed []string
	switch mode {
	case config.ModeCode:
		allowed = []string{"run_code"}
	case config.ModeMinimal:
		allowed = config.MinimalTools()
	default:
		allowed = make([]string, 0, len(specs))
		for _, spec := range specs {
			if spec.Name != "run_code" {
				allowed = append(allowed, spec.Name)
			}
		}
	}
	out := make([]llm.ToolSchema, 0, len(allowed))
	for _, spec := range specs {
		if containsString(allowed, spec.Name) {
			out = append(out, spec)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// allRegisteredToolNames returns the names of every tool currently registered
// in the registry (the "full" permission tier's whitelist).
func (a *app) allRegisteredToolNames() []string {
	specs := a.reg.Specs()
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}

// promptFor returns the system-prompt builder for a non-global mode, building
// and caching it on first use. Called under turnMu. The minimal persona is
// self-contained (fixed text, no appended catalog); the wire tool surface is
// still mode-filtered through loop.Config.ToolSpecs.
func (a *app) promptFor(mode string) *prompt.Builder {
	if mode == "" || mode == a.cfg.Mode {
		return a.prompt
	}
	if a.promptByMode == nil {
		a.promptByMode = map[string]*prompt.Builder{}
	}
	if b, ok := a.promptByMode[mode]; ok {
		return b
	}
	if b, err := buildPrompt(mode, a.cfg.PromptsDir); err == nil {
		if mode != config.ModeMinimal {
			b.SetTools(func() []llm.ToolSchema { return toolSpecsForMode(mode, a.reg.Specs()) })
		}
		a.promptByMode[mode] = b
		return b
	}
	return a.prompt
}

// runTurn executes one turn under the global serial lock (D5: REPL and web
// share one loop; at most one Run at a time). interactive=false suppresses the
// stdout stream (the web renders from the SSE event stream instead 鈥?chunk 宸?// 钀藉簱).
func (a *app) runTurn(ctx context.Context, text string, interactive bool) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	// Publish the running session so the sidebar status dot reflects the live
	// turn; cleared when the turn settles (the deferred store runs before the
	// unlock, so a concurrent list read sees the fully-settled session).
	defer a.runningSession.Store("")
	a.runningSession.Store(a.currentID)
	// Phase 2: resolve the session's per-turn model / mode prompt / permission
	// tier and swap the registry policy for the duration of the turn.
	rt, restore := a.applySessionRuntime(a.currentID)
	defer restore()
	if interactive {
		return a.newLoopFor(rt, true).Run(ctx, text)
	}
	return a.newLoopFor(rt, false).Run(ctx, text)
}

// repl drives turns from stdin, handling the session commands.
func (a *app) repl(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("pa 鈥?personal agent REPL. Type /help for the command table.")
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		if strings.HasPrefix(line, "/") {
			if err := a.command(ctx, line); err != nil {
				fmt.Fprintln(os.Stderr, "pa:", err)
			}
			continue
		}
		if err := a.runTurn(ctx, line, true); err != nil {
			fmt.Fprintln(os.Stderr, "\npa:", err)
		} else {
			// M4c: post-answer extraction writeback, orchestrated by the
			// composition root outside the loop (D4). Fail-open by contract:
			// extractTurn never returns an error and never affects the next
			// answer.
			a.extractTurn(ctx, line)
			// M6c-2: post-turn auto-sedimentation, orchestrated by the
			// composition root outside the loop (D4). It runs once per
			// completed turn on the serial REPL path (D5) and never duplicates:
			// the AutoSpill policy is idempotent by content hash and this is
			// the only invocation point. Fail-open by contract.
			a.spillAutoSpill(ctx)
			// session-title alignment (dsh): after the first eligible message,
			// materialize the deterministic fallback and schedule the
			// asynchronous model title.
			a.ensureSessionTitle(ctx, a.currentID)
			// Goal driver idle/followup: once the user-facing turn and its
			// post-turn hooks settle, continue the newest active goal in the
			// same session. runIdleGoal re-enters only through runTurn, never
			// from inside a plan tool execution.
			if err := a.runIdleGoal(ctx, true); err != nil {
				fmt.Fprintln(os.Stderr, "\npa: goal:", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "pa:", err)
		os.Exit(1)
	}
}

// command dispatches the /-commands.
func (a *app) command(ctx context.Context, line string) error {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/help":
		a.printHelp()
	case "/new":
		if err := a.newSession(ctx); err != nil {
			return err
		}
		fmt.Printf("new session %s\n", a.currentID)
	case "/list":
		return a.listSessions(ctx)
	case "/resume":
		if len(fields) < 2 {
			return fmt.Errorf("usage: /resume <id>")
		}
		if err := a.resumeSession(ctx, fields[1]); err != nil {
			return err
		}
		fmt.Printf("resumed session %s (%d events)\n", a.currentID, len(a.log.Events()))
	case "/kb-status":
		return a.kbStatus(ctx)
	case "/kb-reindex":
		return a.kbReindex(ctx)
	case "/llm-status":
		return a.llmStatus()
	case "/attach":
		return a.attachCommand(ctx, fields[1:])
	case "/term":
		return a.termCommand(ctx, fields[1:])
	case "/eval-status":
		return a.evalStatus()
	case "/compact":
		return a.compactCommand(ctx, fields[1:])
	default:
		return fmt.Errorf("unknown command %q (try /help)", fields[0])
	}
	return nil
}

// printHelp prints the complete command table (M3 CLI 瀹屽杽; M4b adds kb).
func (a *app) printHelp() {
	fmt.Println(`commands:
  /new              start a new session
  /list             list all sessions (most recently updated first)
  /resume <id>      resume an existing session by id
  /kb-status        knowledge-base status (entries / db size / recent writes)
  /kb-reindex       rebuild the knowledge-base FTS index
  /llm-status       LLM provider status (provider / model / modalities)
  /attach <path>    attach an image file as a multimodal user message (M8-3)
  /term <start|write|read|signal|stop>  persistent-shell terminal (M9)
  /eval-status       task-evaluation status (eval)
  /compact          compact the session now (fold old context into a summary)
  /compact region <start> <end>  compact only the given surface event range
  /help             show this command table
  /exit             quit (alias: /quit)
  anything else     send to the agent as a message

startup:  pa [--config <path>]   config defaults to config.yaml`)
	fmt.Printf("llm: provider=%s model=%s modalities=%s\n",
		a.cfg.LLM.Provider, llmProviderModel(a.cfg, a.cfg.LLM.Provider), llmModalitiesValue(a.cfg))
	if a.multimodalEnabled() {
		fmt.Printf("multimodal: enabled (max_image_bytes=%d)\n", a.cfg.LLM.Multimodal.MaxImageBytes)
	} else {
		fmt.Println("multimodal: disabled (llm.multimodal.enabled=false)")
	}
	fmt.Printf("enabled tools: %s\n", strings.Join(a.cfg.Tools.Enabled, ", "))
	if config.Enabled(a.cfg.KB.Enabled) {
		fmt.Printf("knowledge base: enabled (db=%s, recall_limit=%d, catalog=%v)\n",
			a.cfg.KB.DBPath, a.cfg.KB.RecallLimitValue(), a.cfg.KB.CatalogValue())
	} else {
		fmt.Println("knowledge base: disabled (kb.enabled=false)")
	}
	if config.Enabled(a.cfg.Jobs.Enabled) {
		fmt.Printf("jobs: enabled (max_concurrent_jobs_per_owner=%d)\n", a.cfg.Jobs.MaxConcurrentJobsPerOwner)
	} else {
		fmt.Println("jobs: disabled (jobs.enabled=false)")
	}
	if config.Enabled(a.cfg.Compaction.Enabled) {
		fmt.Printf("compaction: enabled (token_threshold=%d, retain_turns=%d)\n",
			a.cfg.Compaction.TokenThreshold, a.cfg.Compaction.RetainTurns)
	} else {
		fmt.Println("compaction: disabled (compaction.enabled=false)")
	}
	if config.Enabled(a.cfg.Skill.Enabled) {
		fmt.Printf("skills: enabled (catalog_max_chars=%d, body_max_chars=%d)\n",
			a.cfg.Skill.CatalogMaxChars, a.cfg.Skill.BodyMaxChars)
	} else {
		fmt.Println("skills: disabled (skill.enabled=false)")
	}
	if config.Enabled(a.cfg.Schedule.Enabled) {
		fmt.Printf("schedules: enabled (tick_interval=%s)\n", a.cfg.Schedule.TickInterval.Duration)
	} else {
		fmt.Println("schedules: disabled (schedule.enabled=false)")
	}
	if config.Enabled(a.cfg.Plan.Enabled) {
		fmt.Println("plans: enabled (goal 鈫?plan 鈫?todo planning tree)")
	} else {
		fmt.Println("plans: disabled (plan.enabled=false)")
	}
	if config.Enabled(a.cfg.Spill.Enabled) {
		fmt.Printf("spills: enabled (auto_spill=%v)\n", a.cfg.Spill.AutoSpillValue())
	} else {
		fmt.Println("spills: disabled (spill.enabled=false)")
	}
	if config.Enabled(a.cfg.Interact.Enabled) {
		if len(a.cfg.Interact.SensitiveTools) > 0 {
			fmt.Printf("interact: enabled (sensitive_tools=%s)\n", strings.Join(a.cfg.Interact.SensitiveTools, ", "))
		} else {
			fmt.Println("interact: enabled (no sensitive_tools 鈥?interact_* tools only, no gating)")
		}
	} else {
		fmt.Println("interact: disabled (interact.enabled=false)")
	}
	if config.Enabled(a.cfg.Code.Enabled) {
		fmt.Printf("code sandbox: enabled (timeout=%s, max_output=%d, sandbox_dir=%q, allow_network=%v)\n",
			a.cfg.Code.Timeout.Duration, a.cfg.Code.MaxOutput, a.cfg.Code.SandboxDir, a.cfg.Code.AllowNetwork)
	} else {
		fmt.Println("code sandbox: disabled (code.enabled=false)")
	}
	if config.Enabled(a.cfg.Mcp.Enabled) {
		if len(a.cfg.Mcp.Servers) > 0 {
			fmt.Printf("mcp: enabled (servers: %s)\n", mcpServerNames(a.cfg.Mcp.Servers))
		} else {
			fmt.Println("mcp: enabled (no servers 鈥?mcp_list/mcp_call only)")
		}
	} else {
		fmt.Println("mcp: disabled (mcp.enabled=false)")
	}
	if config.Enabled(a.cfg.Fs.Enabled) {
		if a.fs != nil {
			fmt.Printf("fs: enabled (root=%s)\n", a.fs.Root())
		} else {
			fmt.Println("fs: enabled (root=<project>)")
		}
	} else {
		fmt.Println("fs: disabled (fs.enabled=false)")
	}
	if config.Enabled(a.cfg.Web.Enabled) {
		fmt.Printf("web: enabled (search_max_results=%d, search_max_queries=%d)\n",
			a.cfg.Web.SearchMaxResults, a.cfg.Web.SearchMaxQueries)
	} else {
		fmt.Println("web: disabled (web.enabled=false)")
	}
	if a.cfg.WebServer.Enabled {
		fmt.Printf("web portal: enabled (%s)\n", a.cfg.WebServer.Addr)
	} else {
		fmt.Println("web portal: disabled (web_server.enabled=false)")
	}
	if config.Enabled(a.cfg.Terminal.Enabled) {
		fmt.Printf("terminal: enabled (shell=%q, idle=%dms, timeout=%dms)\n",
			a.cfg.Terminal.Shell, a.cfg.Terminal.ReadIdleMS, a.cfg.Terminal.ReadTimeoutMS)
	} else {
		fmt.Println("terminal: disabled (terminal.enabled=false)")
	}
	fmt.Printf("mode: %s\n", a.cfg.Mode)
	if config.Enabled(a.cfg.Eval.Enabled) {
		fmt.Printf("eval: enabled (max_records=%d)\n", a.cfg.Eval.MaxRecords)
	} else {
		fmt.Println("eval: disabled (eval.enabled=false)")
	}
}

func (a *app) listSessions(ctx context.Context) error {
	sessions, err := a.store.ListSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions yet 鈥?type /new to start one")
		return nil
	}
	for _, s := range sessions {
		marker := " "
		if s.ID == a.currentID {
			marker = "*"
		}
		fmt.Printf("%s %s  created=%s  updated=%s  events=%d\n",
			marker, s.ID,
			s.CreatedAt.Local().Format(time.RFC3339),
			s.UpdatedAt.Local().Format(time.RFC3339),
			s.EventCount)
	}
	return nil
}

// newSessionID returns a short random session id (e.g. "s-1a2b3c4d").
func newSessionID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "s-" + hex.EncodeToString(b[:]), nil
}
