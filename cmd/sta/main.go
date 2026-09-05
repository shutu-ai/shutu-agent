// Command sta is the Shutu Agent REPL (M1鈫扢3). It wires the thin core 鈥?llm,
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

	"github.com/shutu-ai/shutu-agent/internal/acp"
	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/code"
	"github.com/shutu-ai/shutu-agent/internal/compaction"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/credential"
	"github.com/shutu-ai/shutu-agent/internal/eval"
	"github.com/shutu-ai/shutu-agent/internal/extensionhost"
	"github.com/shutu-ai/shutu-agent/internal/fs"
	hookrunner "github.com/shutu-ai/shutu-agent/internal/hooks"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/jobs"
	"github.com/shutu-ai/shutu-agent/internal/lifecycle"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/mcp"
	"github.com/shutu-ai/shutu-agent/internal/meter"
	"github.com/shutu-ai/shutu-agent/internal/observability"
	"github.com/shutu-ai/shutu-agent/internal/plan"
	"github.com/shutu-ai/shutu-agent/internal/plugin"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/schedule"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/skill"
	"github.com/shutu-ai/shutu-agent/internal/spill"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/subagent"
	"github.com/shutu-ai/shutu-agent/internal/team"
	"github.com/shutu-ai/shutu-agent/internal/terminal"
	"github.com/shutu-ai/shutu-agent/internal/timecontext"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/web"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	webOnly := flag.Bool("web-only", false, "serve the web portal only, without the REPL (blocks until interrupted)")
	acpMode := flag.Bool("acp", false, "serve ACP JSON-RPC over stdin/stdout")
	sdkMode := flag.Bool("sdk", false, "serve the Shutu SDK JSON-RPC runtime over stdin/stdout")
	catalogManifestPath := flag.String("catalog-manifest", "", "write the canonical tool catalog manifest to this file ('-' for stdout) and exit")
	verifyCatalogManifestPath := flag.String("verify-catalog-manifest", "", "verify a tool catalog manifest against this runtime and exit")
	flag.Parse()
	if *acpMode && *sdkMode {
		fmt.Fprintln(os.Stderr, "sta: --acp and --sdk are mutually exclusive")
		os.Exit(2)
	}
	if *acpMode || *sdkMode {
		ignoreTransportBrokenPipe()
	}
	if *catalogManifestPath != "" && *verifyCatalogManifestPath != "" {
		fmt.Fprintln(os.Stderr, "sta: --catalog-manifest and --verify-catalog-manifest are mutually exclusive")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if err := enforceCrashDumpPolicy(cfg.Security.CrashDumpPolicy); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	cfg.WebServer.DistDir = resolveFrontendDist(*configPath, cfg.WebServer.DistDir)

	st, err := store.OpenSQLite(filepath.Join(cfg.DataDir, "pa.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	shutdown := lifecycle.New()
	if err := shutdown.Register("store", st.Close); err != nil {
		fmt.Fprintln(os.Stderr, "sta: shutdown:", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdown.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "sta: shutdown:", err)
		}
	}()

	// Runtime General-settings rows (durable in the SQLite settings table,
	// applied at startup; D-WEB2-D: config changes need a restart, no hot
	// reload). agent_preset overrides the mode preset (D-MODE), and
	// terminal_enabled the terminal switch 鈥?but a minimal preset keeps its
	// mandatory terminal (D-MODE-2). permission_preset is applied to the
	// execution whitelist after registration (see below).
	settings, err := st.GetSettings(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta: settings:", err)
		os.Exit(1)
	}
	if raw := settings["mcp.servers"]; raw != "" {
		var servers []config.McpServer
		if err := json.Unmarshal([]byte(raw), &servers); err == nil {
			cfg.Mcp.Servers = servers
		}
	}
	permissionPreset := settings["permission_preset"] // "" | "readonly" | "standard" | "full"
	if permissionPreset != "" {
		stored, _, _, _ := permissionBundle(permissionPreset)
		permissionPreset = stored
	}
	if v, ok := settings["agent_preset"]; ok &&
		(v == config.ModeMinimal || v == config.ModeStandard || v == config.ModeCode) {
		cfg.Mode = v
		config.ApplyModePreset(&cfg)
	}
	// DSH's model picker saves the accepted provider/model/effort as the
	// shared Agent default. Apply it before the provider registry is built so
	// new sessions and the Web model catalog see the same selection after a
	// restart. A session-specific selection still wins during turn setup.
	if selection, ok := parsePersistedModelSelection(settings[defaultModelSettingKey]); ok {
		applyModelSelectionToConfig(&cfg, selection)
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
	var runtimeApp *app
	sessionRoot := func() string {
		if runtimeApp != nil {
			return runtimeApp.sessionCWD()
		}
		dir := cfg.Workspace.DefaultDir
		if dir == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				dir = filepath.Join(home, "shutu")
				_ = os.MkdirAll(dir, 0o755)
			} else {
				dir, _ = os.Getwd()
			}
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		return filepath.Clean(dir)
	}
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if !config.Enabled(cfg.Fs.Enabled) {
		if err := reg.Register(tools.NewReadFileForRoot(sessionRoot)); err != nil {
			fmt.Fprintln(os.Stderr, "sta:", err)
			os.Exit(1)
		}
	}
	promptBuilder, err := buildPrompt(cfg.Mode, cfg.PromptsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	promptBuilder.SetTools(func() []llm.ToolSchema { return toolSpecsForMode(cfg.Mode, reg.VisibleSpecs()) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if err := shutdown.Register("signal", func() error { stop(); return nil }); err != nil {
		fmt.Fprintln(os.Stderr, "sta: shutdown:", err)
		os.Exit(1)
	}
	telemetry, err := observability.NewSessionTelemetryExporterFromEnvAt(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta: telemetry:", err)
		os.Exit(1)
	}

	app := &app{
		cfg:          cfg,
		configPath:   filepath.Clean(*configPath),
		store:        st,
		reg:          reg,
		prompt:       promptBuilder,
		agentPresets: newNativeAgentPresetStore(cfg.DataDir, cfg.PromptsDir, cfg.Mode),
		basePolicy:   pol,
		// M10 W1: the real-time event hub (ADR D-WEB2-B) exists for the whole
		// process lifetime so attachSink can broadcast every persisted event to
		// the web's SSE subscribers whenever the webserver is enabled.
		hub: NewEventHub(),
		// baseCtx = the process-lifetime signal ctx (see the field comment): the
		// persist sink and the web-only block live as long as the process.
		baseCtx:            ctx,
		agentRegistry:      agent.NewRegistry(),
		strictAgentRuntime: true,
		pluginRegistry:     plugin.NewRegistryWithTools(nil, reg),
		sessionAgents:      make(map[string]*agent.Handle),
		jobTraceSpans:      make(map[string]*observability.Span),
		usageMeter:         meter.New(),
		metrics:            observability.New(),
		tracer:             observability.NewTracer(4096),
		telemetry:          telemetry,
	}
	if telemetry != nil {
		if err := shutdown.Register("telemetry", func() error {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return telemetry.Shutdown(shutdownCtx)
		}); err != nil {
			fmt.Fprintln(os.Stderr, "sta: shutdown:", err)
			os.Exit(1)
		}
	}
	credentialBackend := store.CredentialRecordStore(st)
	credentialVault, err := credential.New(context.Background(), credentialBackend)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sta: credentials:", err)
		os.Exit(1)
	}
	app.credentials = credentialVault
	if cfg.TimeContext.EnabledWith(config.Enabled(cfg.Schedule.Enabled)) {
		timeContext, err := timecontext.New(timecontext.Config{
			TimeZone:          cfg.TimeContext.TimeZone,
			RefreshIntervalMS: cfg.TimeContext.RefreshIntervalMS,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "sta:", err)
			os.Exit(1)
		}
		app.timeContext = timeContext
	}
	if err := shutdown.Register("credentials", credentialVault.Close); err != nil {
		fmt.Fprintln(os.Stderr, "sta: shutdown:", err)
		os.Exit(1)
	}
	runtimeApp = app
	registerShutdown := func(name string, closeFn func() error) {
		if err := shutdown.Register(name, closeFn); err != nil {
			fmt.Fprintln(os.Stderr, "sta: shutdown:", err)
			os.Exit(1)
		}
	}
	// Plugin scopes close before Agent and tool consumers in LIFO shutdown.
	registerShutdown("plugins", func() error { return app.pluginRegistry.Close() })
	// M11: load the provider API-key overrides (llm.key.<id>) and custom
	// OpenAI-compatible provider declarations (llm.custom.<route>) from the
	// durable settings table before registerLLM builds the registry. A
	// configured key wins over the env var (閰嶇疆鍚庝互閰嶇疆鐨勪负鍑? user 2026-09).
	legacyCredentialKeys := map[string]string{}
	for k, v := range settings {
		if strings.HasPrefix(k, "llm.key.") {
			legacyCredentialKeys[strings.TrimPrefix(k, "llm.key.")] = v
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
	for provider, value := range legacyCredentialKeys {
		if strings.TrimSpace(value) == "" {
			if err := st.DeleteSetting(context.Background(), "llm.key."+provider); err != nil {
				fmt.Fprintln(os.Stderr, "sta: migrate empty credential:", err)
				os.Exit(1)
			}
			continue
		}
		reference := llmKeyEnv(provider)
		if !credentialVault.Has(reference) {
			if err := credentialVault.Set(context.Background(), reference, value); err != nil {
				fmt.Fprintln(os.Stderr, "sta: migrate credential:", err)
				os.Exit(1)
			}
		}
		if err := st.DeleteSetting(context.Background(), "llm.key."+provider); err != nil {
			fmt.Fprintln(os.Stderr, "sta: remove legacy credential setting:", err)
			os.Exit(1)
		}
	}
	app.llmKeys = map[string]string{}
	for _, provider := range builtinProviders {
		if key := providerKeyFromSnapshot(nil, provider.id); key != "" {
			app.llmKeys[provider.id] = key
		}
	}
	for _, provider := range app.customProviders {
		if value, err := credentialVault.Resolve(context.Background(), llmKeyEnv(provider.ID)); err == nil {
			app.llmKeys[provider.ID] = value
		}
	}
	// M8-2: registerLLM builds the provider registry and injects the selected
	// provider into a.llm 鈥?the single llm.LLM the loop, compaction, subagent
	if err := app.registerLLM(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	// M8-3: wire the image-attachment store 鈥?under <data_dir>/attachments/ 鈥?	// when llm.multimodal.enabled (榛樿鍏?D10). disabled 鈬?/attach unavailable.
	if err := app.registerAttachments(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	// to nothing itself; config.applyDefaults already whitelisted them when
	// M5a-2: wire the jobs seam 鈥?Local registry + the five job_* tools + the
	// D3 event sink 鈥?when jobs.enabled (榛樿鍏抽棴, D10). config.applyDefaults
	// already whitelisted the job_* names when jobs.enabled was true. The
	// deferred Close cancels and awaits every live background job at shutdown
	// so no goroutine leaks (lifecycle reversible, ADR 鍐崇瓥 鈶?.
	if err := app.registerJobs(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	// M5b-2: wire the subagent seam 鈥?spawn provider + Runtime + the four
	// subagent_* tools + the D3 event sink 鈥?when subagent.enabled (榛樿鍏抽棴,
	// D10). config.applyDefaults already whitelisted the subagent_* names when
	// subagent.enabled was true. The deferred Close cancels and awaits every
	// live child at shutdown so no background goroutine leaks (lifecycle
	// reversible, ADR 鍐崇瓥 鈶?.
	if err := app.registerSubagent(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.subagents != nil {
		registerShutdown("subagents", app.subagents.Close)
	}
	if err := app.registerTeam(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
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
		fmt.Fprintln(os.Stderr, "sta:", err)
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
		fmt.Fprintln(os.Stderr, "sta:", err)
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.compaction != nil {
		registerShutdown("compaction", func() error { app.closeCompactionEngines(); return nil })
	}
	// M5d-2: wire the skill seam 鈥?filesystem provider + Registry + the
	// skill_load tool + the "skill" pre-step catalog injector 鈥?when
	// skill.enabled (榛樿鍏抽棴, D10). config.applyDefaults already whitelisted
	// skill_load when skill.enabled was true. The deferred Close releases the
	// registry and its providers at shutdown (lifecycle reversible, ADR
	// 鍐崇瓥 鈶?.
	if err := app.registerSkills(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.skills != nil {
		registerShutdown("skills", app.skills.Close)
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.schedules != nil {
		registerShutdown("schedules", app.schedules.Close)
	}
	// M6b-2: wire the plan seam 鈥?in-memory Provider + Engine + the six
	// plan_* tools + the D3 event sink 鈥?when plan.enabled (榛樿鍏抽棴, D10).
	// config.applyDefaults already whitelisted the plan_* names when
	// plan.enabled was true. The deferred Close releases the provider and
	// rejects further operations at shutdown (lifecycle reversible, ADR
	// 鍐崇瓥 M6b). The plan tree is a planning model only 鈥?execution delegation
	// to subagents is deferred to M6c+.
	if err := app.registerPlans(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.plans != nil {
		registerShutdown("plans", func() error { app.closePlanEngines(); return nil })
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.spills != nil {
		registerShutdown("spills", app.spills.Close)
	}
	// M6e-2: wire the code seam 鈥?local subprocess Provider + Engine + the
	// run_code tool + the D3 event sink 鈥?when code.enabled (榛樿鍏抽棴, D10).
	// config.applyDefaults already whitelisted run_code when code.enabled was
	// true. registerCode runs before registerInteracts so the sensitive-tool
	// gate can wrap run_code too. The deferred Close releases the provider and
	// rejects further runs at shutdown (lifecycle reversible, ADR 鍐崇瓥 M6e).
	// run_code executes on the serial tool path (D5) 鈥?no background goroutine.
	if err := app.registerCode(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.code != nil {
		registerShutdown("code", app.code.Close)
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if len(app.mcpClients()) > 0 {
		registerShutdown("mcp", func() error {
			var first error
			clients := app.mcpClients()
			for _, c := range clients {
				if err := c.Close(); err != nil && first == nil {
					first = err
				}
			}
			return first
		})
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.fs != nil {
		registerShutdown("filesystem", app.fs.Close)
	}
	// D-GAP-1: wire the file-content-search seam 鈥?the grep/glob tools (dsh tool-fs-search contract) 鈥?when
	// fs_search.enabled (榛樿鍏?D10). config.applyDefaults already whitelisted
	// fs_search when fs_search.enabled was true. The tools are read-only and
	// holds no resources, so there is no deferred Close; the default search
	// root is the agent working directory (os.Getwd, like internal/code and
	// internal/skill). They execute on the serial tool path (D5).
	if err := app.registerFsSearch(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	// P2 session-query: wire five read-only history tools when enabled.
	if err := app.registerSessionQuery(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if err := app.loadNativeRuntimeSettings(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "sta: load native runtime settings:", err)
		os.Exit(1)
	}
	if err := app.registerLSP(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if err := app.registerHooks(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	registerShutdown("hooks", func() error { app.closeHooks(); return nil })
	// M7-2: wire the web seam 鈥?Engine + DeepSeek search provider (env key
	// only) + HTTP fetch provider + the two web_* tools 鈥?when web.enabled
	// (榛樿鍏抽棴, D10). config.applyDefaults already whitelisted web_search/
	// web_fetch when web.enabled was true. registerWeb runs before
	// registerInteracts so the sensitive-tool gate can wrap the web tools too.
	// The Engine holds no closable resources, so there is no deferred Close.
	// The canonical web search LLM request is logged by the provider's OnRequest (D3); the web_*
	// tools execute on the serial tool path (D5) 鈥?no background goroutine.
	if err := app.registerWeb(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	// M9/dsh: wire the pwsh seam 鈥?the fresh-process pwsh tool (dsh
	// tool-pwsh: one `pwsh -Command` process per call, no state between
	// calls) + the /term REPL over the M9 persistent session 鈥?when
	// terminal.enabled (榛樿鍏?D10). config.applyDefaults already whitelisted
	// pwsh when terminal.enabled was true. The deferred cleanup closes the
	// active /term session at shutdown so no child process leaks.
	if err := app.registerTerminal(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	registerShutdown("terminals", func() error {
		app.closeModelTerminalSessions()
		if app.termSess != nil {
			return app.termSess.Close()
		}
		return nil
	})
	if err := app.registerExtensions(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.extensions != nil {
		registerShutdown("extensions", func() error {
			return errors.Join(app.extensions.Close(), app.extensionEventLog.Close())
		})
	}
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.interacts != nil {
		registerShutdown("interactions", app.interacts.Close)
	}
	if config.Enabled(app.cfg.Plan.Enabled) {
		if err := app.reg.Register(exitPlanModeTool{app: app}); err != nil {
			fmt.Fprintln(os.Stderr, "sta:", err)
			os.Exit(1)
		}
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.evalEng != nil {
		registerShutdown("evaluation", func() error { app.evalEng.Close(); return nil })
	}
	// Provider generations may retain credentials for an in-flight request.
	// Register this disposer before title/Agent/jobs barriers so LIFO shutdown
	// drains all consumers first and wipes the current generation last among
	// model users.
	registerShutdown("llm-credentials", func() error {
		return app.closeProviderGenerations()
	})
	// M10a: wire the unified web portal (ADR 2026-08-20-m10-web-portal.md) 鈥?	// the bearer-authenticated net/http server over the read-only store
	// (sessions/events browsing + static vanilla-JS frontend) 鈥?when
	// web_server.enabled (榛樿鍏?D10, no listener at all). An empty token fails
	// closed at startup (no bare server). The deferred Close shuts the listener
	// at shutdown so no port lingers.
	// Register this defer after every Agent-dependent service has registered its
	// own cleanup. LIFO then quiesces Agents first, so no child turn can enter a
	// jobs/code/approval/plugin service after that service has been closed.
	registerShutdown("title-workers", func() error { app.waitTitleWorkers(); return nil })
	registerShutdown("agents", func() error {
		if err := app.agentRegistry.CloseAll(); err != nil {
			return err
		}
		return nil
	})
	// Jobs must drain before Agents close: settlement callbacks may still need
	// to publish a completion wake into the owning Agent inbox. The defer is
	// registered after the Agent cleanup so LIFO shutdown is gate -> scheduler
	// -> jobs -> agents -> remaining process services.
	registerShutdown("jobs", func() error {
		if app.jobs != nil {
			return app.jobs.Close()
		}
		return nil
	})
	if *catalogManifestPath != "" {
		if err := writeToolCatalogManifest(app.reg, *catalogManifestPath); err != nil {
			fmt.Fprintln(os.Stderr, "sta:", err)
			os.Exit(1)
		}
		return
	}
	if *verifyCatalogManifestPath != "" {
		if err := verifyToolCatalogManifest(app.reg, *verifyCatalogManifestPath); err != nil {
			fmt.Fprintln(os.Stderr, "sta:", err)
			os.Exit(1)
		}
		fmt.Println("tool catalog manifest verified")
		return
	}
	if *acpMode {
		// Admission is registered last so the coordinator closes it first.
		registerShutdown("admission", func() error { app.beginShutdown(); return nil })
		server := &acp.Server{
			Factory:      &acpFactory{app: app},
			In:           os.Stdin,
			Out:          os.Stdout,
			AgentName:    "shutu-agent",
			AgentVersion: "0.1",
		}
		if err := server.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "sta: acp:", err)
			os.Exit(1)
		}
		return
	}
	if *sdkMode {
		registerShutdown("admission", func() error { app.beginShutdown(); return nil })
		server := newSDKServer(app, os.Stdin, os.Stdout)
		if err := server.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "sta: sdk:", err)
			os.Exit(1)
		}
		return
	}
	if err := app.registerWebServer(); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	if app.webserver != nil {
		registerShutdown("webserver", app.webserver.Close)
	}
	if err := app.startup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
	app.startGoalScheduler(ctx)
	registerShutdown("goal-schedulers", func() error { app.closeGoalSchedulers(); return nil })
	// Admission is registered after every dependent resource. The coordinator
	// therefore rejects new work before any resource close barrier runs.
	registerShutdown("admission", func() error { app.beginShutdown(); return nil })
	// M10 W3: --web-only serves the portal without the REPL (dsh-style
	// standalone web). The signal ctx cancels on Ctrl+C/Ctrl+Break; the
	// deferred webserver.Close shuts the listener so no port lingers.
	if *webOnly {
		if app.webserver == nil {
			fmt.Fprintln(os.Stderr, "sta: --web-only requires web_server.enabled=true in config")
			os.Exit(1)
		}
		<-ctx.Done()
	} else {
		app.repl(ctx)
	}
}

// resolveFrontendDist makes a configured relative frontend path portable when
// sta is launched from outside the configuration directory. Absolute paths are
// preserved; an empty path remains empty so the web server can report the
// missing frontend only when the web portal is enabled.
func resolveFrontendDist(configPath, distDir string) string {
	if strings.TrimSpace(distDir) == "" || filepath.IsAbs(distDir) {
		return distDir
	}
	return filepath.Clean(filepath.Join(filepath.Dir(configPath), distDir))
}

// app holds the REPL's mutable session state.
type app struct {
	cfg          config.Config
	configPath   string
	store        store.Store
	reg          *tools.Registry
	prompt       *prompt.Builder
	agentPresets *nativeAgentPresetStore
	// basePolicy is the startup Execute policy (global mode + permission
	// preset). Per-session permission tiers swap a derived policy around the
	// legacy turn only; Agent-owned turns use a cloned registry.
	basePolicy tools.Policy
	// codeUnavailableReason is populated when code mode was requested but its
	// enforcing runtime could not be verified. The rest of the Agent remains
	// usable; code mode is rejected at session admission and never advertised.
	codeUnavailableReason string
	// promptByMode caches a per-mode system-prompt builder (Phase 2: 鎸変細璇?	// mode 閿佸畾). Populated lazily on first use for a non-global session mode.
	promptByMode map[string]*prompt.Builder
	llm          llm.LLM
	usageMeter   *meter.Meter
	metrics      *observability.Metrics
	tracer       *observability.Tracer
	telemetry    *observability.SessionTelemetryExporter
	// llmMu guards the llm/llmReg pointer swap during the live model switch
	// (POST /api/config/model, P5.1): the switch holds the legacy/control locks
	// (D5 serial, so no legacy turn is in flight) and takes the write lock; consumers read the selected
	// provider through currentLLM() (RLock). The zero value is ready.
	llmMu sync.RWMutex
	// providerMu guards the mutable provider configuration and credential
	// indexes. A model switch is serialized with the legacy turn, but status,
	// ACP capability discovery and background Agent creation can read the same
	// state concurrently; those readers must observe an immutable snapshot.
	providerMu sync.RWMutex
	// providerStateMu is the publication barrier for the selected provider and
	// its configuration. A live model switch builds a complete registry before
	// publishing it; Agent runtime assembly takes the read side so it cannot
	// observe new config with the previous registry during that rebuild.
	providerStateMu sync.RWMutex
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
	// credentials owns real provider secrets separately from generic runtime
	// settings. llmKeys is retained only as a process-local compatibility/cache
	// overlay for provider construction and is populated from this vault.
	credentials *credential.Vault
	// providerGeneration owns the lifetime of the published registry. A model
	// switch retires the previous generation, but credential-bearing adapters
	// are not closed until every in-flight stream releases its lease.
	providerGeneration *providerGeneration
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
	attachStore   *attachment.Store
	currentID     string
	log           *session.Log
	jobs          *jobs.Local             // nil when jobs disabled (D10)
	subagents     subagent.Runtime        // nil when subagent disabled (D10)
	subagentTools *subagent.SubagentTools // live browser-addressed child seam

	compaction        compaction.Engine // nil when compaction disabled (D10)
	compactionMu      sync.Mutex
	compactionEngines map[string]compaction.Engine
	timeContext       *timecontext.Service
	skills            skill.Registry // nil when skill disabled (D10)
	// skillManager is the web settings-page skill manager (dsh-skill-mcp-panel
	// 瀵归綈). It is created whenever the web server runs 鈥?independent of
	// skill.enabled 鈥?so the 鎶€鑳?settings page can list/enable/disable/delete/
	// add/migrate skill files even when the model-facing skill capability is off.
	skillManager *skill.Manager
	// titleMu guards titleDone, the per-process set of sessions whose
	// asynchronous model title has already been attempted (dsh-session-title
	// alignment): the model title fires at most once per session per process,
	// so a failed run never re-fires on every later turn.
	titleMu        sync.Mutex
	titleDone      map[string]bool
	titleWG        sync.WaitGroup
	schedules      schedule.Engine // nil when schedule disabled (D10)
	goalScheduler  *schedule.DurableScheduler
	scheduleMu     sync.Mutex
	goalSchedulers map[string]*schedule.DurableScheduler
	// scheduleClosed is the shutdown gate for lazy per-session scheduler
	// creation. Without it, a tool already admitted while shutdown begins could
	// recreate a scheduler after closeGoalSchedulers has drained the ticker.
	scheduleClosed       bool
	scheduleSessionMu    sync.Mutex
	scheduleSessionLocks map[string]*sync.Mutex
	scheduleWake         chan struct{}
	scheduleCancel       context.CancelFunc
	scheduleWG           sync.WaitGroup
	plans                plan.Engine // nil when plan disabled (D10)
	planMu               sync.Mutex
	planEngines          map[string]plan.Engine
	planPending          map[string]bool // session-scoped selections awaiting the next Agent boundary
	// nativeGoalMu serializes the plan-engine mutation followed by its durable
	// session-event append. It is intentionally independent of the legacy turn lock: an
	// addressed Web session must not wait for or mutate the REPL's current
	// session while still preserving CAS/event ordering for goal mutations.
	nativeGoalMu sync.Mutex
	// goalActivation is intentionally process-local. Durable goal state lives
	// in the session log; opening/resuming a session disarms automatic rounds
	// until an explicit human resume or a newly created goal arms it.
	goalActivationMu sync.Mutex
	goalActivation   map[string]bool
	spills           spill.Engine    // nil when spill disabled (D10)
	interacts        interact.Engine // nil when interact disabled (D10)
	teamMu           sync.Mutex
	teamBoards       map[string]*team.Board
	// teamDispatchMu protects the process-local delivery fences used by the
	// durable Team mailbox. The Lead journal is the ordering source; these
	// fences only prevent concurrent live injections from reordering that log or
	// dispatching the same queued message twice during recovery.
	teamDispatchMu       sync.Mutex
	teamDispatchTails    map[string]chan struct{}
	teamDispatchInFlight map[string]*teamDispatchResult
	jobWakeMu            sync.Mutex
	// jobTraceMu protects live background-job spans. The span is created
	// before the producer goroutine starts and ended by the single settlement
	// observer, so async work remains correlated after its tool returns.
	jobTraceMu    sync.Mutex
	jobTraceSpans map[string]*observability.Span
	// jobEventMu serializes the check-and-append boundary for job/* events. A
	// fast job can be observed by job_start at the same time its completion
	// observer runs; both paths must share one durable terminal projection.
	jobEventMu    sync.Mutex
	jobWakeCounts map[string]int
	// interactionSessions keeps the owner session for live Web approval
	// requests. DSH scopes interaction surfaces to the addressed conversation;
	// the engine itself remains process-wide for CLI compatibility.
	interactionMu       sync.RWMutex
	interactionSessions map[string]string
	interactionCallIDs  map[string]string
	// interactionResolveMu serializes answerers across CLI/Web. Resolving the
	// in-memory engine and appending its durable decision must be one ordered
	// state transition from the application's point of view; otherwise two
	// answerers can race and a failed append can leave live state ahead of the
	// transcript.
	interactionResolveMu sync.Mutex
	// agentRegistry owns the long-lived root Agent handles used by the native
	// and ACP bridges. Production composition sets strictAgentRuntime, so a
	// missing registry is fail-closed; lightweight compatibility/test apps may
	// leave both unset and use the legacy direct path.
	agentRegistry      *agent.Registry
	strictAgentRuntime bool
	// pluginRegistry owns plugin generations and generation-guarded tool
	// publication into reg. It is created for every production composition so
	// optional plugin hosts cannot bypass canonical ownership metadata.
	pluginRegistry *plugin.Registry
	agentMu        sync.Mutex
	sessionAgents  map[string]*agent.Handle
	// runtimeMu/runtimeLogs keep native session-owned log objects alive
	// independently of the REPL's current selection. Web/Agent turns must not
	// switch currentID merely to find their durable source of truth.
	runtimeMu               sync.Mutex
	runtimeLogs             map[string]*session.Log
	runtimeMaxTokensMu      sync.RWMutex
	runtimeMaxTokens        map[string]int
	nativeRuntimeSettingsMu sync.RWMutex
	nativeShell             tools.ShellSettings
	nativeAgentLoopMax      int
	nativeWebSearch         nativeWebSearchSettings
	code                    code.ProgramRuntime // nil when code disabled (D10)
	extensions              *extensionhost.Host // nil when extensions disabled
	extensionEventLog       *extensionEventLogger
	codeBindingPolicy       tools.Policy          // PTC nested tools during the active turn
	mcp                     []mcp.Client          // nil when mcp disabled (D10); one live bridged client per configured server
	mcpMu                   sync.RWMutex          // protects the live bridged-client slice for Web status and shutdown
	mcpByServer             map[string]mcp.Client // name-stable view; unlike mcp, it remains correct when an optional server is down
	mcpSyncMu               sync.Mutex            // serializes reconnect re-sync and generation replacement
	mcpToolNames            map[string][]string   // currently published bridged names by server
	fs                      fs.FileService        // nil when fs disabled (D10)
	web                     *web.Engine           // nil when web disabled (D10)
	hooks                   *hookrunner.Runner

	// webserver is the M10a unified web portal (ADR 2026-08-20-m10-web-portal.md);
	// nil when web_server disabled (D10).
	webserver *webserver.Server

	// sessionStateMu serializes only the compatibility REPL/currentID selection.
	// Addressed Agent sessions use their own handle and webSessionLocks, so
	// Web/ACP turns never use process-global currentID as their owner.
	sessionStateMu sync.Mutex
	// controlMu serializes the remaining REPL publication path. Agent turns
	// consume providerState snapshots and do not take it.
	controlMu sync.Mutex
	// lifecycleMu/lifecycleClosed form the application-wide admission gate.
	// Shutdown closes admission before dependent services are drained, so a
	// late Web/ACP request cannot recreate an Agent, scheduler or title worker
	// while the process is releasing those resources.
	lifecycleMu     sync.RWMutex
	lifecycleClosed bool
	// cancelMu + turnCancel let POST /api/sessions/{id}/stop abort the web turn
	// (dsh 鍋滄鎸夐挳) without holding the legacy turn lock: the web message handler registers its
	// cancellable context here, and the stop handler calls the stored cancel.
	cancelMu    sync.Mutex
	turnCancels map[string]context.CancelFunc
	// webQueueMu protects the process-local dsh-style queue. Queue contents are
	// intentionally ephemeral like the current Web turn; durable conversation
	// history remains in the session store.
	webQueueMu      sync.Mutex
	webQueue        map[string][]webQueueMessage
	webQueueRunning map[string]bool
	// webSessionMu serializes command-side mutations for one Agent-backed
	// session. It deliberately does not serialize different sessions and is
	// separate from sessionStateMu, which protects only REPL selection state.
	webSessionMu    sync.Mutex
	webSessionLocks map[string]*sync.Mutex
	webQueueSeq     uint64
	// runningSession is the session id whose turn is currently in flight, or ""
	// when idle. It is published by runTurn under sessionStateMu and read atomically by
	// the sidebar status provider, so the webserver always sees a consistent
	// "running" dot without touching a.currentID (which other handlers mutate).
	// dsh-session-status alignment.
	runningSession  atomic.Value
	runningMu       sync.Mutex
	runningSessions map[string]int
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

	// modelTerms are the dsh-compatible persistent terminal sessions. They are
	// separate from termSess, which remains the user-facing /term session.
	modelTermMu sync.Mutex
	modelTerms  map[string]*modelTerminalRecord
	// modelTermStopMu serializes owner disposal retries so a failed receipt can
	// be retried without racing another disposer into a duplicate stop edge.
	modelTermStopMu sync.Mutex

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
	// feedbackMu/id hold the harness-home anonymous identity used by the
	// feedback acknowledgement. The durable file is created lazily so merely
	// starting the app never creates an identity on disk.
	feedbackMu          sync.Mutex
	feedbackAnonymousID string
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
	if a.currentID == "" || a.log == nil || sessionHasUserContent(a.log.Events()) {
		return
	}
	if err := a.store.DeleteSession(ctx, a.currentID); err != nil {
		fmt.Fprintf(os.Stderr, "sta: prune blank session %q: %v\n", a.currentID, err)
	}
}

// sessionHasUserContent excludes log-only command lifecycle rows from the
// blank-session decision. Starting a command such as /new must not make an
// otherwise empty session durable merely because command/run was recorded.
func sessionHasUserContent(events []session.Event) bool {
	for _, event := range events {
		if event.Type == session.EventCommandRun || event.Type == session.EventCommandDone {
			continue
		}
		return true
	}
	return false
}

// newSession starts a fresh session with a random id.
func (a *app) newSession(ctx context.Context) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	// Session replacement swaps the process-wide log and its durable sink. It
	// must not race an active Web/REPL turn, whose loop holds the current log
	// pointer while worker callbacks may still be committing events.
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	id, err := store.GenerateReservedID(ctx, a.store, "session", newSessionID)
	if err != nil {
		return fmt.Errorf("sta: generate session id: %w", err)
	}
	// dsh: starting a fresh session discards an abandoned blank one.
	a.pruneBlankCurrent(ctx)
	created := time.Now().UTC()
	cwd := a.defaultWorkdir()
	createdAtomically := false
	if atomic, ok := a.store.(store.SessionCreateStore); ok {
		cfg := a.providerConfigSnapshot()
		if err := atomic.CreateSessionWithOptions(ctx, id, created, store.SessionCreateOptions{
			Header: store.SessionHeader{ID: id, CreatedAt: created, CWD: cwd, AgentPreset: cfg.Mode},
			Config: &store.SessionConfig{AgentPreset: cfg.Mode, Provider: cfg.LLM.Provider, Model: llmProviderModel(cfg, cfg.LLM.Provider), ReasoningEffort: cfg.ReasoningEffort},
		}, nil); err != nil {
			return err
		}
		createdAtomically = true
	} else {
		if err := a.store.CreateSession(ctx, id, created); err != nil {
			return err
		}
		if err := a.setSessionCWD(ctx, id, cwd); err != nil {
			return err
		}
	}
	a.currentID = id
	a.log = session.New()
	a.configureImageResolver(a.log)
	a.runtimeMu.Lock()
	if a.runtimeLogs == nil {
		a.runtimeLogs = make(map[string]*session.Log)
	}
	a.runtimeLogs[id] = a.log
	a.runtimeMu.Unlock()
	a.setGoalActivation(id, false)
	if err := a.restorePlans(); err != nil {
		return err
	}
	if err := a.restoreGoalScheduler(); err != nil {
		return err
	}
	// Compatibility stores may not have the atomic creation seam. Keep their
	// legacy header repair, while SQLite already committed the cwd above.
	if !createdAtomically {
		if err := a.setSessionCWD(ctx, id, a.sessionCWD()); err != nil {
			return err
		}
	}
	a.attachSink(ctx)
	a.bindSpillOwner()
	a.markSessionViewed(ctx, id)
	a.extensions.PublishSessionStarted(id)
	return nil
}

// resumeSession loads a session's full history from the store into a new log.
func (a *app) resumeSession(ctx context.Context, id string) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	return a.resumeSessionLocked(ctx, id)
}

func (a *app) resumeSessionLocked(ctx context.Context, id string) error {
	// Opening a session from another browser tab can arrive while a long turn is
	// streaming. Wait for that turn to settle before replacing a.log; otherwise
	// the old loop and the newly restored log can both append the same Seq to the
	// session store through different sinks.
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
	a.configureImageResolver(a.log)
	a.runtimeMu.Lock()
	if a.runtimeLogs == nil {
		a.runtimeLogs = make(map[string]*session.Log)
	}
	a.runtimeLogs[id] = a.log
	a.runtimeMu.Unlock()
	a.setGoalActivation(id, false)
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
	a.attachSinkFor(ctx, id, a.log)
}

func (a *app) attachSinkFor(_ context.Context, id string, log *session.Log) {
	if log == nil || a.store == nil {
		return
	}
	// Persist under the process-lifetime ctx. baseCtx is always set by main;
	// tests that build an app directly fall back to Background so a nil ctx can
	// never reach database/sql (which panics on a nil context).
	pctx := a.baseCtx
	if pctx == nil {
		pctx = context.Background()
	}
	log.SetSink(func(ev session.Event) error {
		return a.store.AppendEvents(pctx, id, []session.Event{ev})
	})
	log.SetObserver(func(ev session.Event) {
		if a.hub != nil {
			a.hub.Publish(id, ev)
		}
		if a.hooks != nil {
			a.hooks.Notify(id, ev)
		}
		if a.extensions != nil {
			a.extensions.PublishSessionEvent(id, ev)
		}
		if a.telemetry != nil {
			if ev.Type == session.EventFeedbackRecord {
				a.telemetry.ObserveSession(id, log.Events(), ev.Seq)
			} else {
				a.telemetry.Observe(id, ev)
			}
		}
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

// The loop's turn/step structure remains unchanged (D4).
func (a *app) newLoop() *loop.Loop {
	return a.buildLoop(
		func(delta string) { fmt.Print(delta) },
		func(err error) { fmt.Fprintln(os.Stderr, "\n[stream error]", err) },
		a.currentID, "", a.cfg.Model, a.cfg.ReasoningEffort, a.cfg.Mode, a.prompt,
	)
}

// newLoopWeb builds a Loop identical to newLoop except its stream hooks are
// silent (interactive=false, M10 W1): the web frontend renders the stream from
// the SSE event flow (each chunk is already persisted by the loop), so nothing
// may be printed to the REPL's stdout/stderr during a web turn.
func (a *app) newLoopWeb() *loop.Loop {
	return a.buildLoop(func(string) {}, func(error) {}, a.currentID, "", a.cfg.Model, a.cfg.ReasoningEffort, a.cfg.Mode, a.prompt)
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
			rt.sessionID, rt.provider, rt.model, rt.effort, rt.mode, rt.prompt,
		)
	}
	return a.buildLoop(func(string) {}, func(error) {}, rt.sessionID, rt.provider, rt.model, rt.effort, rt.mode, rt.prompt)
}

// buildLoop assembles a Loop bound to the current session log. onText/onError
// are the streaming hooks: the REPL prints them, the web path is silent.
// provider/model/effort override the globals when a per-session selection is
// active (dsh ModelSelection: the session owns provider+model+effort); an
// unknown provider id is fail-closed at turn dispatch. mode is the
// session's agent preset (standard | code | minimal): it owns the model-facing
// tool surface (loop.Config.ToolSpecs). pb overrides the system prompt when a
// per-session mode is active. effort is the thinking-effort selection ("" keeps
// the provider default).
func (a *app) buildLoop(onText func(string), onError func(error), sessionID, provider, model, effort, mode string, pb *prompt.Builder) *loop.Loop {
	return a.buildLoopBound(onText, onError, sessionID, provider, model, effort, mode, pb, a.log, a.reg)
}

func (a *app) buildLoopBound(onText func(string), onError func(error), sessionID, provider, model, effort, mode string, pb *prompt.Builder, log *session.Log, registry *tools.Registry) *loop.Loop {
	return a.buildLoopBoundWithProvider(onText, onError, sessionID, provider, model, effort, mode, pb, log, registry, nil)
}

// buildLoopBoundWithProvider optionally pins the provider instance selected
// while an Agent turn was assembled. The named route remains part of the
// request identity, but an in-flight turn must not re-read a newly published
// process-wide registry generation after a concurrent model switch.
func (a *app) buildLoopBoundWithProvider(onText func(string), onError func(error), sessionID, provider, model, effort, mode string, pb *prompt.Builder, log *session.Log, registry *tools.Registry, pinned llm.LLM) *loop.Loop {
	runtime := a.providerRuntimeSnapshot(provider)
	if pinned != nil {
		runtime.selected = pinned
		runtime.selectedID = provider
	}
	cfg := runtime.cfg
	provider = runtime.provider
	if provider == "" {
		provider = cfg.LLM.Provider
	}
	if model == "" {
		model = llmProviderModel(cfg, provider)
	}
	// Resolve the final route before deriving any capability-dependent loop
	// field. The previous order used the session's current model to derive
	// MaxTokens, then replaced capability with the explicitly requested route;
	// a caller switching routes during assembly could therefore send the old
	// model's output budget with the new model.
	capability := a.modelCapabilityForRoute(provider, model)
	maxTokens := effectiveModelOutputLimit(cfg.MaxTokens, capability.DefaultMaxTokens, capability.MaxTokens)
	a.runtimeMaxTokensMu.RLock()
	if configured := a.runtimeMaxTokens[sessionID]; configured > 0 {
		maxTokens = configured
	}
	a.runtimeMaxTokensMu.RUnlock()
	effort = effectiveModelReasoningEffort(capability, effort)
	reasoningBudgetTokens := 0
	if cfg.LLM.ThinkingBudgets != nil {
		reasoningBudgetTokens = cfg.LLM.ThinkingBudgets[strings.TrimSpace(effort)]
	}
	if pb == nil {
		pb = a.prompt
	}
	if mode == "" {
		mode = cfg.Mode
	}
	if mode == config.ModeCode {
		if registry == nil {
			registry = tools.New()
		}
		pb = pb.Clone().Add(prompt.Section{
			Name:  "code-mode-sdk",
			Order: 1001,
			Text:  codeModeSDKSection(registry.Specs()),
		})
	}
	ll := runtime.selected
	return loop.New(loop.Config{
		LLM: ll,
		// Request middleware is allowed to select an explicit provider. Resolve
		// that final route at the transport boundary so provider metadata,
		// credentials and provider-owned retry policy cannot diverge. An empty
		// provider keeps this turn's already-resolved route.
		ResolveRequestLLM: func(_ context.Context, request llm.ChatRequest) (llm.LLM, error) {
			requested := strings.TrimSpace(request.Provider)
			if requested == "" {
				if provider == "" {
					// Standalone embedders/tests may intentionally supply only an
					// LLM without a named registry route.
					return ll, nil
				}
				requested = provider
			}
			if pinned != nil && requested == provider {
				return pinned, nil
			}
			resolved := a.providerRuntimeSnapshot(requested)
			if resolved.selectedID == "" {
				return nil, fmt.Errorf("sta: llm provider %q is not registered", requested)
			}
			return resolved.selected, nil
		},
		Log:                      log,
		Tools:                    registry,
		ToolSpecs:                func() []llm.ToolSchema { return modelToolSpecs(capability, mode, registry.VisibleSpecs()) },
		Prompt:                   pb,
		Model:                    model,
		MaxTokens:                maxTokens,
		ContextWindow:            capability.ContextWindow,
		Provider:                 provider,
		ReasoningEffort:          effort,
		ReasoningBudgetTokens:    reasoningBudgetTokens,
		MaxParallelToolCallsFunc: func() int { return a.nativeAgentLoopMaxParallel() },
		// Bind the runtime snapshot to the session selected for this turn. The
		// loop's injector callback also receives userText, not a session id, so
		// capturing the id here avoids falling back to the process-global currentID.
		RuntimeContext: func(ctx context.Context, _ string) []llm.Message {
			return a.runtimeContextFor(ctx, sessionID)
		},
		TimeContext:      a.timeContext,
		RuntimeSessionID: sessionID,
		RuntimeAgentID:   sessionID,
		RuntimeEmit: func(typ string, data any) error {
			if log == nil {
				return errors.New("runtime event sink is unavailable")
			}
			_, err := a.appendRuntimeEvent(log, typ, data)
			return err
		},
		// M5c-2b: the "compaction" pre-step injector (auto token-pressure
		// compaction) is appended when compaction is enabled; it runs after the
		// (D4 — the turn/step structure is unchanged).
		PreStep:                a.preStepInjectorsForSession(sessionID, log),
		RecoverContextOverflow: func(ctx context.Context) bool { return a.recoverContextOverflowFor(ctx, log) },
		OnText:                 onText,
		OnError:                onError,
		OnUsage: func(request llm.ChatRequest, usage llm.TokenUsage) {
			if a.usageMeter != nil {
				a.usageMeter.RecordSuccessfulUsageAt(sessionID, request, usage, log)
			}
		},
		Metrics: a.metrics,
		Tracer:  a.tracer,
	})
}

// sessionRuntime is the resolved per-turn runtime for one session: the
// effective LLM provider, model, thinking effort, the mode preset and the
// system-prompt builder (by mode). All fields are "" / nil when the session
// falls back to the globals (dsh ModelSelection: provider+model+effort are one
// selection; the mode defaults to the deployment preset).
type sessionRuntime struct {
	sessionID string
	provider  string
	// selected is the provider generation captured while resolving this turn.
	// It prevents a concurrent registry publication from changing an in-flight
	// Agent request route; retirement still requires a separate usage lease.
	selected  llm.LLM
	model     string
	effort    string
	mode      string
	prompt    *prompt.Builder
	log       *session.Log
	registry  *tools.Registry
	cwd       string
	maxTokens int
}

// applySessionRuntime resolves one session's per-turn provider/model/effort /
// mode-prompt / permission tier (session override ?? global) and swaps the
// registry policy to the session's mode-projected whitelist. The compatibility
// helper remains only for focused composition tests; production turns use the
// strict Agent-owned path. The returned restore func reinstates the base policy.
// Fail-open: any store or builder
// error falls back to the globals.
func (a *app) applySessionRuntime(id string) (sessionRuntime, func()) {
	return a.applySessionRuntimeOn(id, a.log, a.reg)
}

// applySessionRuntimeE is the strict legacy-host assembly boundary. Even when
// a small compatibility host has not composed the Agent registry, it must
// return durable configuration and canonical catalog projection errors instead
// of silently substituting global runtime state.
func (a *app) applySessionRuntimeE(id string) (sessionRuntime, func(), error) {
	return a.applySessionRuntimeOnStrict(id, a.log, a.reg)
}

func (a *app) applySessionRuntimeOn(id string, log *session.Log, registry *tools.Registry) (sessionRuntime, func()) {
	rt, restore, err := a.applySessionRuntimeOnMode(id, log, registry, false)
	if err != nil {
		// The legacy REPL has no error return at this seam. Keep its historical
		// compatibility behavior explicit and isolated; Agent-owned turns use
		// applySessionRuntimeOnStrict below and never take this fallback.
		return sessionRuntime{sessionID: id, log: log, registry: registry}, func() {}
	}
	return rt, restore
}

// applySessionRuntimeOnStrict is the Agent-owned runtime assembly boundary.
// A durable configuration or approval-policy failure must not silently turn
// into a global-provider/global-permission turn after restart.
func (a *app) applySessionRuntimeOnStrict(id string, log *session.Log, registry *tools.Registry) (sessionRuntime, func(), error) {
	return a.applySessionRuntimeOnMode(id, log, registry, true)
}

func (a *app) applySessionRuntimeOnMode(id string, log *session.Log, registry *tools.Registry, strict bool) (sessionRuntime, func(), error) {
	cfgSnapshot := a.providerConfigSnapshot()
	provider := cfgSnapshot.LLM.Provider
	rt := sessionRuntime{
		sessionID: id,
		provider:  provider,
		model:     llmProviderModel(cfgSnapshot, provider),
		effort:    cfgSnapshot.ReasoningEffort,
		prompt:    a.prompt,
		log:       log,
		registry:  registry,
		cwd:       a.sessionCWDFor(id),
	}
	perm := ""
	mode := cfgSnapshot.Mode
	if mode == "" {
		mode = config.ModeStandard
	}
	if scs, ok := a.store.(store.SessionConfigStore); ok && id != "" {
		cfg, err := scs.GetSessionConfig(context.Background(), id)
		if err != nil && strict && !errors.Is(err, store.ErrNotFound) {
			return sessionRuntime{}, func() {}, fmt.Errorf("sta: load session runtime %q: %w", id, err)
		}
		if err == nil {
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
	// Approval policy is part of the session runtime, not merely a tool
	// whitelist. Reapply the persisted permission tier before any sensitive
	// call so a restored full-permission session cannot accidentally inherit the
	// process-wide ask policy (and vice versa).
	if a.interacts != nil && id != "" && perm != "" {
		if controller, ok := a.interacts.(interact.PolicyController); ok {
			_, _, _, approvalPolicy := permissionBundle(perm)
			if err := controller.SetSessionPolicy(id, interact.ApprovalPolicy(approvalPolicy)); err != nil && strict {
				return sessionRuntime{}, func() {}, fmt.Errorf("sta: set session approval policy %q: %w", id, err)
			}
		}
	}
	rt.mode = mode
	toolMode := mode
	if a.agentPresets != nil && !nativeAgentPresetKnown(mode) {
		toolMode = a.agentPresets.Mode(mode)
	}
	if toolMode == config.ModeCode && a.codeUnavailableReason != "" {
		return sessionRuntime{}, func() {}, fmt.Errorf("sta: code mode unavailable: %s", a.codeUnavailableReason)
	}
	// DSH persona sections resolve model and working-directory placeholders at
	// prompt render time. Clone per turn so one shared base builder remains safe
	// while sessions use different model/workspace selections.
	rt.prompt = rt.prompt.Clone().SetVariables(map[string]string{
		"model": rt.model,
		"cwd":   rt.cwd,
	})
	if log != nil {
		planActive, err := currentPlanModeActive(log)
		if err != nil {
			return sessionRuntime{}, func() {}, fmt.Errorf("sta: read plan mode for session %q: %w", id, err)
		}
		if planActive {
			rt.prompt = rt.prompt.Clone().Add(prompt.Section{Name: "plan-mode", Order: 900, Text: planModeSection})
		}
	}
	// Every turn projects the session's mode onto the full base whitelist and
	// swaps it in for the turn's duration (dsh: the executor honors the same
	// presentation mode; standard never executes run_code, PTC only run_code,
	// minimal only its fixed seam).
	if registry == nil {
		registry = tools.New()
	}
	base := a.basePolicy
	base.Enabled = modeToolWhitelist(toolMode, base.Enabled)
	base.Profile = toolMode
	pol, _ := a.sessionPolicyFromRegistry(base, perm, toolMode, registry)
	registry.SetPolicy(pol)
	if strict {
		if err := registry.ValidateProjection(toolMode, toolSpecsForMode(toolMode, registry.VisibleSpecs())); err != nil {
			return sessionRuntime{}, func() {}, fmt.Errorf("sta: validate canonical tool projection for session %q: %w", id, err)
		}
	}
	// PTC exposes only run_code to the model, but its TypeScript program may
	// dispatch the same tools available to the session. Keep that nested view
	// separate from the direct-call policy so the outer loop remains fail-closed.
	nested := a.basePolicy
	nested.Profile = config.ModeStandard
	nested.Enabled = modeToolWhitelist(config.ModeStandard, nested.Enabled)
	if perm == "readonly" {
		nested.Enabled = config.ReadOnlyTools()
	} else if perm == "full" {
		nested.Enabled = modeToolWhitelist(config.ModeStandard, registeredToolNames(registry))
	}
	if registry == a.reg {
		a.codeBindingPolicy = nested
	}
	selected := a.providerRuntimeSnapshotPinned(rt.provider)
	rt.selected = selected.selected
	return rt, func() {
		if registry == a.reg {
			registry.SetPolicy(a.basePolicy)
		}
		if selected.release != nil {
			selected.release()
		}
	}, nil
}

func (a *app) sessionPolicyFrom(base tools.Policy, perm, mode string) (tools.Policy, bool) {
	return a.sessionPolicyFromRegistry(base, perm, mode, a.reg)
}

func (a *app) sessionPolicyFromRegistry(base tools.Policy, perm, mode string, registry *tools.Registry) (tools.Policy, bool) {
	switch perm {
	case "readonly":
		base.Enabled = config.ReadOnlyTools()
		return base, true
	case "full":
		base.Enabled = modeToolWhitelist(mode, registeredToolNames(registry))
		return base, true
	default:
		return base, false
	}
}

func registeredToolNames(registry *tools.Registry) []string {
	if registry == nil {
		return nil
	}
	specs := registry.Specs()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// modeToolWhitelist projects the model-facing tool surface for a mode. Native
// standard keeps registered tools (including str_replace_editor), PTC exposes only run_code, and minimal keeps
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
// and caching it on first use. Called under the legacy turn lock. The minimal persona is
// self-contained (fixed text, no appended catalog); the wire tool surface is
// still mode-filtered through loop.Config.ToolSpecs.
func (a *app) promptFor(mode string) *prompt.Builder {
	cfg := a.providerConfigSnapshot()
	if mode == "" || mode == cfg.Mode {
		return a.prompt
	}
	if a.promptByMode == nil {
		a.promptByMode = map[string]*prompt.Builder{}
	}
	if b, ok := a.promptByMode[mode]; ok {
		return b
	}
	var b *prompt.Builder
	var err error
	if a.agentPresets != nil && !nativeAgentPresetKnown(mode) {
		b, err = a.agentPresets.Prompt(mode)
	} else {
		b, err = buildPrompt(mode, cfg.PromptsDir)
	}
	if err == nil {
		if mode != config.ModeMinimal {
			b.SetTools(func() []llm.ToolSchema { return toolSpecsForMode(mode, a.reg.VisibleSpecs()) })
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
	return a.runTurnFor(ctx, a.currentID, text, interactive)
}

// runTurnFor executes a turn through the session-owned Agent handle. There is
// deliberately no global-session fallback: a missing Agent registry is a
// composition error, not permission to route a request through legacy state.
func (a *app) runTurnFor(ctx context.Context, sessionID, text string, interactive bool) error {
	return a.runTurnForWithMeta(ctx, sessionID, text, interactive, webserver.PromptMeta{})
}

func (a *app) runTurnForWithMeta(ctx context.Context, sessionID, text string, interactive bool, meta webserver.PromptMeta) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if a.agentRegistry == nil {
		if a.strictAgentRuntime || a.reg == nil || a.log == nil {
			return errors.New("agent runtime is unavailable")
		}
		return a.runTurnForLegacy(ctx, sessionID, text, interactive)
	}
	handle, err := a.sessionAgent(sessionID)
	if err != nil {
		return err
	}
	metadata := map[string]string{}
	if meta.RPCID != "" {
		metadata["rpc_id"] = meta.RPCID
	}
	if meta.ClientTimeZone != "" {
		metadata["client_time_zone"] = meta.ClientTimeZone
	}
	if interactive {
		metadata["interactive"] = "true"
	}
	return handle.Run(ctx, text, metadata)
}

func (a *app) runTurnContentFor(ctx context.Context, sessionID string, content []llm.ContentBlock, interactive bool) error {
	return a.runTurnContentForWithMeta(ctx, sessionID, content, interactive, webserver.PromptMeta{})
}

func (a *app) runTurnContentForWithMeta(ctx context.Context, sessionID string, content []llm.ContentBlock, interactive bool, meta webserver.PromptMeta) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if a.agentRegistry == nil {
		if a.strictAgentRuntime || a.reg == nil || a.log == nil {
			return errors.New("agent runtime is unavailable")
		}
		return a.runTurnContentForLegacy(ctx, sessionID, content, interactive, meta)
	}
	handle, err := a.sessionAgent(sessionID)
	if err != nil {
		return err
	}
	metadata := map[string]string{}
	if meta.RPCID != "" {
		metadata["rpc_id"] = meta.RPCID
	}
	if meta.ClientTimeZone != "" {
		metadata["client_time_zone"] = meta.ClientTimeZone
	}
	if interactive {
		metadata["interactive"] = "true"
	}
	return handle.RunContent(ctx, content, metadata)
}

// runTurnForLegacy is retained only for small compatibility hosts and tests
// that intentionally do not compose the Agent registry. The production app
// sets strictAgentRuntime and never reaches this process-global path.
func (a *app) runTurnForLegacy(ctx context.Context, sessionID, text string, interactive bool) error {
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	if sessionID != "" && sessionID != a.currentID {
		if err := a.resumeSession(ctx, sessionID); err != nil {
			return err
		}
	}
	activeID := a.currentID
	defer a.runningSession.Store("")
	a.runningSession.Store(activeID)
	rt, restore, err := a.applySessionRuntimeE(activeID)
	if err != nil {
		return err
	}
	defer restore()
	return a.newLoopFor(rt, interactive).Run(ctx, text)
}

func (a *app) runTurnContentForLegacy(ctx context.Context, sessionID string, content []llm.ContentBlock, interactive bool, meta webserver.PromptMeta) error {
	a.sessionStateMu.Lock()
	defer a.sessionStateMu.Unlock()
	if sessionID != "" && sessionID != a.currentID {
		if err := a.resumeSession(ctx, sessionID); err != nil {
			return err
		}
	}
	activeID := a.currentID
	defer a.runningSession.Store("")
	a.runningSession.Store(activeID)
	rt, restore, err := a.applySessionRuntimeE(activeID)
	if err != nil {
		return err
	}
	defer restore()
	message := llm.Message{Role: llm.RoleUser, Content: append([]llm.ContentBlock(nil), content...)}
	message.SourceRPCID = meta.RPCID
	message.SourceClientTimeZone = meta.ClientTimeZone
	return a.newLoopFor(rt, interactive).RunMessages(ctx, []llm.Message{message})
}

// repl drives turns from stdin, handling the session commands.
func (a *app) repl(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("sta - Shutu Agent REPL. Type /help for the command table.")
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
				fmt.Fprintln(os.Stderr, "sta:", err)
			}
			continue
		}
		if err := a.runTurn(ctx, line, true); err != nil {
			fmt.Fprintln(os.Stderr, "\npa:", err)
		} else {
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
		fmt.Fprintln(os.Stderr, "sta:", err)
		os.Exit(1)
	}
}

// command dispatches the /-commands.
func (a *app) command(ctx context.Context, line string) (err error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return errors.New("empty command")
	}
	var commandID string
	commandLog := a.log
	if nativeCommandLifecycle(fields[0]) {
		commandID, err = appendCommandRun(commandLog, strings.TrimPrefix(fields[0], "/"), commandArgs(line, fields[0]))
		if err != nil {
			return err
		}
		defer func() {
			kind := "success"
			text := ""
			if err != nil {
				kind = "error"
				text = err.Error()
			}
			if appendErr := appendCommandDone(commandLog, commandID, kind, text); err == nil && appendErr != nil {
				err = appendErr
			}
		}()
	}
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
	case "/feedback":
		text := strings.TrimSpace(line[len(fields[0]):])
		result, err := a.webFeedback(ctx, text)
		if err != nil {
			return err
		}
		fmt.Println(result)
	case "/plan":
		return a.planCommand(ctx, strings.TrimSpace(line[len(fields[0]):]))
	default:
		return fmt.Errorf("unknown command %q (try /help)", fields[0])
	}
	return nil
}

// planCommand implements the host-side /plan entry for the native REPL. Plan
// mode is a durable session fact; an optional suffix is a separate ordinary
// user turn, matching the Web command and dsh's command contract.
func (a *app) planCommand(ctx context.Context, suffix string) error {
	if a.log == nil {
		return errors.New("no active session")
	}
	if a.agentRegistry != nil {
		loggedActive, err := currentPlanModeActive(a.log)
		if err != nil {
			return err
		}
		action, err := a.setPlanModeFor(ctx, a.currentID, a.log, suffix != "off")
		if err != nil {
			return err
		}
		if suffix == "off" {
			switch {
			case action == planModeQueued:
				fmt.Println("Leaving plan mode (applies from the next step).")
			case action == planModeCancelled:
				fmt.Println("Plan mode entry cancelled.")
			case action == planModeNoop && !loggedActive:
				fmt.Println("Plan mode is already inactive.")
			default:
				fmt.Println("Plan mode off.")
			}
			return nil
		}
		if suffix == "" {
			switch {
			case action == planModeQueued:
				fmt.Println("Entering plan mode (applies from the next step). Use /plan off to leave.")
			case action == planModeCommitted || action == planModeNoop && !loggedActive:
				fmt.Println("Plan mode on. Use /plan off to leave.")
			default:
				fmt.Println("Plan mode is already active. Use /plan off to leave.")
			}
			return nil
		}
		if action == planModeQueued {
			if steered, steerErr := a.steerPlanMessage(a.currentID, suffix); steerErr != nil {
				return steerErr
			} else if steered {
				return nil
			}
		}
		return a.runTurn(ctx, suffix, true)
	}
	active, err := currentPlanModeActive(a.log)
	if err != nil {
		return err
	}
	if suffix == "off" {
		if active {
			if _, err := a.log.Append(session.EventPlanMode, session.NewPlanMode(false)); err != nil {
				return err
			}
			fmt.Println("Plan mode off.")
			return nil
		}
		fmt.Println("Plan mode is already inactive.")
		return nil
	}
	if !active {
		if _, err := a.log.Append(session.EventPlanMode, session.NewPlanMode(true)); err != nil {
			return err
		}
		fmt.Println("Plan mode on. Use /plan off to leave.")
	} else if suffix == "" {
		fmt.Println("Plan mode is already active. Use /plan off to leave.")
	}
	if suffix == "" {
		return nil
	}
	// The exact argument "off" was handled above. Every other non-empty
	// suffix is the message sent in plan mode, without the /plan command text.
	if err := a.runTurn(ctx, suffix, true); err != nil {
		return err
	}
	a.spillAutoSpill(ctx)
	a.ensureSessionTitle(ctx, a.currentID)
	return a.runIdleGoal(ctx, true)
}

func (a *app) printHelp() {
	fmt.Println(`commands:
  /new              start a new session
  /list             list all sessions (most recently updated first)
  /resume <id>      resume an existing session by id
  /llm-status       LLM provider status (provider / model / modalities)
  /attach <path>    attach an image file as a multimodal user message (M8-3)
  /term <start|write|read|signal|stop>  persistent-shell terminal (M9)
  /eval-status       task-evaluation status (eval)
  /compact          compact the session now (fold old context into a summary)
  /compact region <start> <end>  compact only the given surface event range
	/feedback <text>  record feedback without sending an LLM turn
	/plan [message]    enter plan mode, optionally submit a message
	/plan off         leave plan mode
  /help             show this command table
  /exit             quit (alias: /quit)
  anything else     send to the agent as a message

startup:  sta [--config <path>]   config defaults to config.yaml`)
	fmt.Printf("llm: provider=%s model=%s modalities=%s\n",
		a.cfg.LLM.Provider, llmProviderModel(a.cfg, a.cfg.LLM.Provider), llmModalitiesValue(a.cfg))
	if a.multimodalEnabled() {
		fmt.Printf("multimodal: enabled (max_image_bytes=%d)\n", a.cfg.LLM.Multimodal.MaxImageBytes)
	} else {
		fmt.Println("multimodal: disabled (llm.multimodal.enabled=false)")
	}
	fmt.Printf("enabled tools: %s\n", strings.Join(a.cfg.Tools.Enabled, ", "))
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
		fmt.Printf("skills: enabled (description_max_chars=%d, body_max_chars=%d)\n",
			a.cfg.Skill.DescriptionMaxChars, a.cfg.Skill.BodyMaxChars)
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

// newSessionID returns an opaque collision-resistant session id. The durable
// SQLite primary key remains the authoritative uniqueness check.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "s-" + hex.EncodeToString(b[:]), nil
}
