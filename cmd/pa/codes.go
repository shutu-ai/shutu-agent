// codes.go — the M6e-2 composition-root orchestration (dispatch-m6e-2 §4).
// This is where the code-sandbox capability seam is wired into the REPL:
// registerCode creates the TypeScript Code Mode runtime and registers the
// run_code tool when code.enabled (D10), and wires the D3 event sink so
// code/run is appended to the active session log. The wiring sits entirely in
// the tool registration layer — the loop's turn/step structure is untouched
// (D4) — and run_code execution is foreground and serial on the tool path (D5,
// no background goroutine). It must run before registerInteracts so the
// sensitive-tool gate can wrap run_code too.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/shutu-ai/shutu-agent/internal/code"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// Kept as a seam so startup tests can exercise the unsupported-runtime
// downgrade without depending on the host's Node installation.
var probeTypeScriptRuntimeStatus = code.TypeScriptRuntimeStatus

// registerCode creates the TypeScript runtime, registers the run_code tool and
// wires the D3 event sink when code.enabled. When code is
// disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerPlans/registerSpills/registerInteracts).
func (a *app) registerCode() error {
	if err := code.RecoverWindowsACLSandboxes(); err != nil {
		return fmt.Errorf("sta: recover Windows ACL sandbox state: %w", err)
	}
	if !config.Enabled(a.cfg.Code.Enabled) {
		return nil
	}
	if available, reason := probeTypeScriptRuntimeStatus(); !available {
		// Code Mode is an optional capability. An unverified sandbox must not
		// prevent the core Agent from starting, and its stale config whitelist
		// must not survive into any later session runtime.
		a.codeUnavailableReason = reason
		a.basePolicy.Enabled = withoutTool(a.basePolicy.Enabled, code.ToolRunName)
		if a.reg != nil {
			policy := a.reg.Policy()
			policy.Enabled = withoutTool(policy.Enabled, code.ToolRunName)
			a.reg.SetPolicy(policy)
		}
		return nil
	}
	runtime := code.NewTypeScriptRuntime()
	a.code = runtime
	// D3 event sink: code/run is appended to the active session log. The
	// callback only ever runs inside a run_code tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the other register* wiring.
	onEvent := func(typ string, data any) {
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "sta: "+typ+" event:", err)
		}
	}
	ct := code.NewCodeToolsWithRuntime(runtime, onEvent)
	ct.SetErrorSink(func(typ string, data any) error {
		if a.log == nil {
			return fmt.Errorf("sta: no session log for %s event", typ)
		}
		_, err := a.log.Append(typ, data)
		return err
	})
	ct.SetBinding(a.executeCodeBinding)
	ct.MaxParallelSubCalls = a.cfg.Code.MaxParallelSubCalls
	ct.IsConcurrencySafe = func(name string, args any) bool {
		return a.reg.IsConcurrencySafe(name, args)
	}
	// Config-derived sandbox policy knobs (code.timeout / code.max_output /
	// code.sandbox_dir). The tool stays decoupled from config (D2): the wiring
	// supplies the values after the seam constructor.
	ct.DefaultTimeout = a.cfg.Code.Timeout.Duration
	ct.DefaultComputeMS = a.cfg.Code.ComputeMS
	ct.DefaultMaxWallMS = a.cfg.Code.MaxWallMS
	ct.DefaultMaxOutput = a.cfg.Code.MaxOutput
	ct.MaxOldGenerationSizeMB = a.cfg.Code.MaxOldGenerationSizeMB
	ct.DefaultCwd = a.cfg.Code.SandboxDir
	ct.DefaultMode = code.SandboxWorkspaceWrite
	// DSH starts tools in the workspace attached to the current session. Keep
	// an explicit model-provided cwd as an override, but resolve the omitted cwd
	// against the active session at execution time rather than process cwd or a
	// single startup sandbox.
	ct.DefaultCwdFunc = func() string { return a.sessionCWD() }
	ct.DefaultCwdContextFunc = func(ctx context.Context) string {
		return a.sessionCWDFor(runtimectx.SessionID(ctx))
	}
	if err := a.reg.Register(ct.Run()); err != nil {
		return fmt.Errorf("sta: register run_code: %w", err)
	}
	return nil
}

func withoutTool(names []string, unwanted string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != unwanted {
			out = append(out, name)
		}
	}
	return out
}

// executeCodeBinding is the host side of PTC's tools.<name>() bridge. The
// direct loop policy exposes only run_code, so nested calls use a cloned
// registry with the session's underlying capability policy. This preserves
// schema validation, approvals, timeouts, output bounds and tool-specific
// hooks without allowing a program to recursively invoke run_code.
func (a *app) executeCodeBinding(ctx context.Context, req code.ProgramBindingRequest) (any, error) {
	if req.Name == code.ToolRunName {
		return nil, fmt.Errorf("run_code cannot be called from inside another run_code program")
	}
	if a.reg == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	scoped := a.reg.Clone()
	// Agent-owned calls always carry a runtime identity. In that path the
	// nested policy is derived below from the immutable startup policy; reading
	// codeBindingPolicy here would race with applySessionRuntime's legacy
	// compatibility write while another session is starting a turn.
	policy := a.basePolicy
	if _, ok := runtimectx.Get(ctx); !ok {
		policy = a.codeBindingPolicy
	}
	// A Code Mode bridge is part of the enclosing Agent turn. Resolve its
	// registry owner and permission tier from that turn's runtime context so a
	// concurrent session can never inherit another session's policy or spill
	// namespace through the process-global compatibility field.
	if sessionID := runtimectx.SessionID(ctx); sessionID != "" {
		if log, logErr := a.sessionLogForAgent(ctx, sessionID); logErr == nil {
			scoped.SetOwner(tools.Owner{SessionID: sessionID, NextSeq: log.NextSeq})
		}
		cfg := a.providerConfigSnapshot()
		mode := cfg.Mode
		toolMode := mode
		if a.agentPresets != nil && !nativeAgentPresetKnown(toolMode) {
			toolMode = a.agentPresets.Mode(toolMode)
		}
		policy = a.basePolicy
		policy.Enabled = modeToolWhitelist(toolMode, policy.Enabled)
		if scs, ok := a.store.(store.SessionConfigStore); ok {
			if saved, savedErr := scs.GetSessionConfig(context.Background(), sessionID); savedErr == nil {
				if saved.AgentPreset != "" {
					mode = saved.AgentPreset
					toolMode = mode
					if a.agentPresets != nil && !nativeAgentPresetKnown(toolMode) {
						toolMode = a.agentPresets.Mode(toolMode)
					}
				}
				switch saved.Permission {
				case "readonly":
					policy.Enabled = config.ReadOnlyTools()
				case "full":
					policy.Enabled = modeToolWhitelist(toolMode, registeredToolNames(scoped))
				default:
					policy.Enabled = modeToolWhitelist(toolMode, policy.Enabled)
				}
			}
		}
	}
	if len(policy.Enabled) == 0 {
		policy = a.basePolicy
		policy.Enabled = modeToolWhitelist(config.ModeStandard, policy.Enabled)
	}
	scoped.SetPolicy(policy)
	result, err := scoped.ExecuteWithCallID(ctx, req.CallID, req.Name, req.Args)
	if err != nil {
		return nil, err
	}
	if result.IsError {
		message := result.Output
		if message == "" && result.Error != nil {
			message = result.Error.Name + ": " + result.Error.Code
		}
		if message == "" {
			message = "tool execution failed"
		}
		// A failed nested tool still may have deferred contexts (for example a
		// policy/error hook produced corrective context before rejecting it).
		// Return the rich envelope alongside the rejected promise so the Code
		// Mode bridge can forward those contexts to the outer durable result.
		if len(result.Content) > 0 || result.Meta != nil || len(result.AdditionalContexts) > 0 || len(result.AdditionalContextMessages) > 0 {
			return code.ProgramBindingResult{
				Value:                     result.Value,
				Content:                   codeContentBlocks(result.Content),
				Meta:                      result.Meta,
				AdditionalContexts:        append([]string(nil), result.AdditionalContexts...),
				AdditionalContextMessages: cloneCodeContextMessages(result.AdditionalContextMessages),
				ConcludesTurn:             false,
			}, fmt.Errorf("%s", message)
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(result.Content) > 0 || result.Meta != nil || len(result.AdditionalContexts) > 0 || len(result.AdditionalContextMessages) > 0 || result.ConcludesTurn {
		return code.ProgramBindingResult{
			Value:                     result.Value,
			Content:                   codeContentBlocks(result.Content),
			Meta:                      result.Meta,
			AdditionalContexts:        append([]string(nil), result.AdditionalContexts...),
			AdditionalContextMessages: cloneCodeContextMessages(result.AdditionalContextMessages),
			ConcludesTurn:             result.ConcludesTurn,
		}, nil
	}
	return result.Value, nil
}

func cloneCodeContextMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = append([]llm.ContentBlock(nil), message.Content...)
		out[i].ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
	}
	return out
}

// codeContentBlocks converts provider-neutral content into the plain JSON
// shape used by Code Mode's durable nested-dispatch event. Image paths are not
// exposed: attachment IDs are the replay-stable identity.
func codeContentBlocks(blocks []llm.ContentBlock) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		// ContentBlock deliberately preserves forward-compatible wire kinds in
		// Raw (for example audio/resource blocks not yet modeled by the Go
		// union). Do not project those blocks through the known-field fallback:
		// doing so silently drops provider metadata and makes the durable nested
		// dispatch differ from what the tool actually returned.
		if len(block.Raw) > 0 {
			var raw map[string]any
			if err := json.Unmarshal(block.Raw, &raw); err == nil && raw != nil {
				if _, ok := raw["type"]; ok {
					out = append(out, raw)
					continue
				}
			}
		}
		item := map[string]any{"type": string(block.Kind)}
		switch block.Kind {
		case llm.BlockText, llm.BlockReasoning:
			item["text"] = block.Text
		case llm.BlockImage:
			item["attachmentId"] = block.Image.ID
			item["mediaType"] = block.Image.MediaType
			if block.Image.Bytes > 0 {
				item["bytes"] = block.Image.Bytes
			}
			if block.Image.Width > 0 {
				item["width"] = block.Image.Width
			}
			if block.Image.Height > 0 {
				item["height"] = block.Image.Height
			}
		default:
			if block.Text != "" {
				item["text"] = block.Text
			}
			if block.CallID != "" {
				item["callId"] = block.CallID
			}
			if block.Name != "" {
				item["name"] = block.Name
			}
			if len(block.Blocks) > 0 {
				item["content"] = codeContentBlocks(block.Blocks)
			}
			if block.IsError {
				item["isError"] = true
			}
		}
		out = append(out, item)
	}
	return out
}
