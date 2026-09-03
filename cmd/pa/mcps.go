// mcps.go — the M6f-2 composition-root orchestration (dispatch-m6f-2 §4-5).
// This is where the MCP tool-ecosystem seam is wired into the REPL:
// registerMcps creates the stdio Factory, registers the mcp_list/mcp_call
// tools (internal/mcp, the dynamic name-addressed path), and — when mcp.enabled
// and servers are configured — starts a stdio client per server and bridges
// every advertised tool into the registry as mcp.<server>.<tool> with its input
// schema passed through (the static path). The wiring sits entirely in the tool
// registration layer — the loop's turn/step structure is untouched (D4) — and
// every client call is foreground and serial on the tool path (D5, no
// background goroutine). It must run before registerInteracts so the
// sensitive-tool gate can wrap the mcp tools too.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/mcp"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

// registerMcps creates the stdio Factory, registers the mcp_* tools and bridges
// every configured server's tools into the registry when mcp.enabled. When mcp
// is disabled it creates nothing and registers nothing (D10, mirrors
// registerJobs/registerPlans/…/registerCode).
func (a *app) registerMcps() error {
	if !config.Enabled(a.cfg.Mcp.Enabled) {
		return nil
	}
	seen := make(map[string]struct{}, len(a.cfg.Mcp.Servers))
	for _, srv := range a.cfg.Mcp.Servers {
		mapped := mcp.McpServer{Name: srv.Name, Transport: srv.Transport, Cmd: srv.Cmd, Args: srv.Args, URL: srv.URL, Headers: srv.Headers, Env: srv.Env, Cwd: srv.Cwd, ToolCallTimeout: time.Duration(srv.ToolCallTimeoutMS) * time.Millisecond}
		if err := mcp.ValidateServer(mapped); err != nil {
			return fmt.Errorf("sta: invalid mcp server %q: %w", srv.Name, err)
		}
		if _, exists := seen[srv.Name]; exists {
			return fmt.Errorf("sta: mcp server %q is configured more than once", srv.Name)
		}
		seen[srv.Name] = struct{}{}
	}
	f := a.mcpFactory
	if f == nil {
		f = mcp.NewStdioFactory()
	}
	// Register the dynamic selector path as well as the static bridged path.
	// The latter exposes the tools advertised at startup; mcp_list/mcp_call are
	// the name-addressed path and must remain available even when no server is
	// configured.
	mt := mcp.NewMcpTools(f, toMcpServers(a.cfg.Mcp.Servers), func(typ string, data any) {
		if a.log == nil {
			return
		}
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Printf("sta: %s event: %v\n", typ, err)
		}
	})
	mt.SetErrorSink(func(typ string, data any) error {
		if a.log == nil {
			return fmt.Errorf("sta: no session log for %s event", typ)
		}
		_, err := a.log.Append(typ, data)
		return err
	})
	mt.SetAttachmentStore(a.attachStore, a.cfg.LLM.Multimodal.MaxImageBytes)
	mt.SetImageAdmission(a.mcpImageAdmission)
	for _, tl := range []tools.Tool{mt.List(), mt.Call()} {
		if err := a.reg.RegisterWithInfo(tl, agenttools.RegistrationInfo{Owner: "mcp", Plugin: "builtin-mcp"}); err != nil {
			return fmt.Errorf("sta: register %s: %w", tl.Name(), err)
		}
	}
	// D3 event sink: mcp/list and mcp/call are appended to the active session
	// log. The callback only ever runs inside a mcp_* tool Execute — the serial
	// main-loop path (D5). a.log is read at call time, so a session switch
	// (/new, /resume) is honored the same way as the other register* wiring.
	ctx := context.Background()
	for _, srv := range a.cfg.Mcp.Servers {
		if err := a.bridgeMcpServer(ctx, f, srv); err != nil {
			if srv.FailOnStartupError {
				return err
			}
			// MCP is an optional external capability. Keep mcp_list/mcp_call
			// available for a later foreground recovery attempt, but do not
			// prevent the host from starting because one server is down. The
			// bridge already closed the failed client before returning.
			fmt.Printf("sta: MCP server %q unavailable at startup: %v\n", srv.Name, redactMCPError(err, srv))
		}
	}
	return nil
}

// bridgeMcpServer starts one configured server's stdio client (Factory.New +
// Start + ListTools), then registers each advertised tool as a first-class
// registry tool named mcp.<server>.<tool> with its input schema passed through;
// executing such a tool calls back into the server via tools/call
// (dispatch-m6f-2 §4). The client is kept in a.mcp so main's deferred Close
// terminates every bridged server at shutdown. A server that fails to start or
// list fails startup loudly (the whole registerMcps errors) — mcp is opt-in
// (D10), so a misconfigured server is a real configuration error, not a silent
// degradation.
func (a *app) bridgeMcpServer(ctx context.Context, f mcp.Factory, srv config.McpServer) error {
	server := mcp.McpServer{Name: srv.Name, Transport: srv.Transport, Cmd: srv.Cmd, Args: srv.Args, URL: srv.URL, Headers: srv.Headers, Env: srv.Env, Cwd: srv.Cwd, ToolCallTimeout: time.Duration(srv.ToolCallTimeoutMS) * time.Millisecond}
	client, err := mcp.NewClientForServer(ctx, f, server)
	if err != nil {
		return fmt.Errorf("sta: create mcp server %q: %w", srv.Name, err)
	}
	// stdio transports expose a connection-loss signal, so promote them to a
	// supervised client. A failed tools/call is never replayed here because its
	// side effect may have committed before the transport reported the error.
	if _, ok := client.(mcp.ConnectionLostHandler); ok {
		reconnect := mcp.DefaultReconnectOptions
		reconnect.Enabled = config.Enabled(a.cfg.Mcp.ReconnectEnabled)
		reconnect.InitialDelay = time.Duration(a.cfg.Mcp.ReconnectInitialDelayMS) * time.Millisecond
		reconnect.MaxDelay = time.Duration(a.cfg.Mcp.ReconnectMaxDelayMS) * time.Millisecond
		reconnect.MaxAttempts = a.cfg.Mcp.ReconnectMaxAttempts
		// MCP SDK clients bind protocol state to one transport generation. Use a
		// fresh client after loss so stale requests, callbacks and negotiated
		// session state cannot bleed into the replacement generation.
		initial := client
		client = mcp.NewReconnectingClientWithFactory(initial, func(reconnectCtx context.Context) (mcp.Client, error) {
			return mcp.NewClientForServer(reconnectCtx, f, server)
		}, reconnect)
	}
	if reconnecting, ok := client.(mcp.ReconnectedHandler); ok {
		reconnecting.SetReconnectedHandler(func() {
			if err := a.resyncMCPServer(context.Background(), srv.Name, client); err != nil {
				fmt.Printf("sta: MCP tool-list refresh after reconnect %q: %v\n", srv.Name, redactMCPError(err, srv))
			}
		})
	}
	if exhausted, ok := client.(mcp.ReconnectExhaustedHandler); ok {
		exhausted.SetReconnectExhaustedHandler(func() {
			a.removeMCPServerTools(srv.Name)
		})
	}
	if err := client.Start(ctx); err != nil {
		if srv.FailOnStartupError || !a.adoptRecoveringMCPLifecycle(srv.Name, client) {
			_ = client.Close()
		}
		return fmt.Errorf("sta: start mcp server %q: %w", srv.Name, err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		if srv.FailOnStartupError || !a.adoptRecoveringMCPLifecycle(srv.Name, client) {
			_ = client.Close()
		}
		return fmt.Errorf("sta: list tools of mcp server %q: %w", srv.Name, err)
	}
	if a.mcpToolNames == nil {
		a.mcpToolNames = make(map[string][]string)
	}
	names := make(map[string]string, len(tools))
	for _, tl := range tools {
		name := mcp.PublicToolName(srv.Name, tl.Name)
		if previous, exists := names[name]; exists {
			_ = client.Close()
			return fmt.Errorf("sta: mcp server %q advertised tools %q and %q under the same public name %q", srv.Name, previous, tl.Name, name)
		}
		names[name] = tl.Name
	}
	// Publish one complete generation or none. In particular, a registry
	// conflict in a later tool must not leave an earlier tool from this server
	// visible while the client itself remains live.
	published := make([]string, 0, len(tools))
	for _, tl := range tools {
		name := mcp.PublicToolName(srv.Name, tl.Name)
		bt := bridgedMcpTool{
			client:         client,
			name:           name,
			tool:           tl.Name,
			desc:           tl.Description,
			schema:         normalizeSchema(tl.InputSchema),
			output:         normalizeOutputSchema(tl.OutputSchema),
			taskSupport:    tl.TaskSupport,
			attachments:    a.attachStore,
			maxImageBytes:  a.cfg.LLM.Multimodal.MaxImageBytes,
			imageAdmission: a.mcpImageAdmission,
			onEvent: func(typ string, data any) error {
				if a.log == nil {
					return errors.New("sta: no session log for MCP event")
				}
				_, err := a.log.Append(typ, data)
				return err
			},
		}
		if err := a.reg.RegisterWithInfo(bt, mcpToolRegistration(srv.Name)); err != nil {
			for _, registered := range published {
				_ = a.reg.Unregister(registered)
			}
			delete(a.mcpToolNames, srv.Name)
			_ = client.Close()
			return fmt.Errorf("sta: register bridged mcp tool %q: %w", name, err)
		}
		// Bridged names are dynamic — config.applyDefaults cannot whitelist
		// them (their names are only known at runtime), so the whitelist is
		// extended here as each tool is registered (dispatch-m6f-2 §4).
		a.reg.Allow(name)
		a.mcpToolNames[srv.Name] = append(a.mcpToolNames[srv.Name], name)
		published = append(published, name)
	}
	a.mcpMu.Lock()
	a.mcp = append(a.mcp, client)
	if a.mcpByServer == nil {
		a.mcpByServer = make(map[string]mcp.Client)
	}
	a.mcpByServer[srv.Name] = client
	a.mcpMu.Unlock()
	if notifier, ok := client.(mcp.ToolListChangedHandler); ok {
		notifier.SetToolListChangedHandler(func() {
			if err := a.resyncMCPServer(context.Background(), srv.Name, client); err != nil {
				fmt.Printf("sta: MCP tool-list refresh %q: %v\n", srv.Name, redactMCPError(err, srv))
			}
		})
	}
	return nil
}

// adoptRecoveringMCPLifecycle records a supervised client whose initial
// discovery failed but whose bounded retry remains active. Without this
// ownership edge, recovery could publish tools into a runtime that shutdown
// cannot drain.
func (a *app) adoptRecoveringMCPLifecycle(server string, client mcp.Client) bool {
	reconnecting, ok := client.(*mcp.ReconnectingClient)
	if !ok || !reconnecting.RecoveryPending() {
		return false
	}
	if a.mcpToolNames == nil {
		a.mcpToolNames = make(map[string][]string)
	}
	a.mcpSyncMu.Lock()
	if a.mcpToolNames == nil {
		a.mcpToolNames = make(map[string][]string)
	}
	a.mcpSyncMu.Unlock()
	a.mcpMu.Lock()
	a.mcp = append(a.mcp, client)
	if a.mcpByServer == nil {
		a.mcpByServer = make(map[string]mcp.Client)
	}
	a.mcpByServer[server] = client
	a.mcpMu.Unlock()
	return true
}

func (a *app) mcpClients() []mcp.Client {
	a.mcpMu.RLock()
	defer a.mcpMu.RUnlock()
	return append([]mcp.Client(nil), a.mcp...)
}

func (a *app) mcpClientForServer(name string) (mcp.Client, bool) {
	a.mcpMu.RLock()
	defer a.mcpMu.RUnlock()
	client, ok := a.mcpByServer[name]
	return client, ok
}

// resyncMCPServer replaces the published tool generation after a recovered
// transport. Discovery is repeated so stale schemas and removed tools do not
// survive a reconnect.
func (a *app) resyncMCPServer(ctx context.Context, server string, client mcp.Client) error {
	a.mcpSyncMu.Lock()
	defer a.mcpSyncMu.Unlock()
	tools, err := client.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("re-sync MCP server %q: %w", server, err)
	}
	// Validate the complete advertised generation before mutating the
	// registry. Public-name normalization can make two distinct server tool
	// names collide, and a later registry conflict must not withdraw the live
	// previous generation while the transport is still usable.
	publicNames := make(map[string]string, len(tools))
	for _, tl := range tools {
		name := mcp.PublicToolName(server, tl.Name)
		if previous, exists := publicNames[name]; exists {
			return fmt.Errorf("MCP server %q advertised tools %q and %q under the same public name %q", server, previous, tl.Name, name)
		}
		publicNames[name] = tl.Name
	}

	oldNames := append([]string(nil), a.mcpToolNames[server]...)
	oldTools := make(map[string]struct {
		tool agenttools.Tool
		info agenttools.RegistrationInfo
	}, len(oldNames))
	maxGeneration := uint64(0)
	for _, name := range oldNames {
		tool, info, ok := a.reg.RegistrationTool(name)
		if !ok {
			continue
		}
		oldTools[name] = struct {
			tool agenttools.Tool
			info agenttools.RegistrationInfo
		}{tool: tool, info: info}
		if info.Generation > maxGeneration {
			maxGeneration = info.Generation
		}
	}
	// Replacement generations must advance past every old registration. New
	// names use the same generation when possible, making the published list a
	// coherent registry generation rather than a sequence of unrelated writes.
	newGeneration := uint64(0)
	if maxGeneration > 0 {
		newGeneration = maxGeneration + 1
	}
	registration := mcpToolRegistration(server)
	if newGeneration > 0 {
		registration.Generation = newGeneration
	}

	newNames := make([]string, 0, len(tools))
	registered := make([]string, 0, len(tools))
	replaced := make([]string, 0, len(tools))
	rollback := func() {
		for _, name := range registered {
			_ = a.reg.Unregister(name)
		}
		for _, name := range replaced {
			if previous, ok := oldTools[name]; ok {
				_ = a.reg.RestoreWithInfo(previous.tool, previous.info)
			}
		}
	}
	for _, tl := range tools {
		name := mcp.PublicToolName(server, tl.Name)
		tool := bridgedMcpTool{
			client:         client,
			name:           name,
			tool:           tl.Name,
			desc:           tl.Description,
			schema:         normalizeSchema(tl.InputSchema),
			output:         normalizeOutputSchema(tl.OutputSchema),
			taskSupport:    tl.TaskSupport,
			attachments:    a.attachStore,
			maxImageBytes:  a.cfg.LLM.Multimodal.MaxImageBytes,
			imageAdmission: a.mcpImageAdmission,
			onEvent: func(typ string, data any) error {
				if a.log == nil {
					return errors.New("sta: no session log for MCP event")
				}
				_, err := a.log.Append(typ, data)
				return err
			},
		}
		if _, exists := oldTools[name]; exists {
			if err := a.reg.ReplaceWithInfo(tool, registration); err != nil {
				rollback()
				return fmt.Errorf("publish MCP server %q tool generation: %w", server, err)
			}
			replaced = append(replaced, name)
		} else if err := a.reg.RegisterWithInfo(tool, registration); err != nil {
			rollback()
			return fmt.Errorf("publish MCP server %q tool generation: %w", server, err)
		} else {
			registered = append(registered, name)
		}
		newNames = append(newNames, name)
	}
	newNameSet := make(map[string]struct{}, len(newNames))
	for _, name := range newNames {
		newNameSet[name] = struct{}{}
	}
	for _, name := range oldNames {
		if _, stillPublished := newNameSet[name]; !stillPublished {
			_ = a.reg.Unregister(name)
		}
	}
	for _, name := range newNames {
		a.reg.Allow(name)
	}
	a.mcpToolNames[server] = newNames
	return nil
}

// removeMCPServerTools withdraws the last published generation after the
// reconnect supervisor reaches its terminal failure state. The registry must
// not keep stale MCP tools callable after the transport has stopped retrying.
func (a *app) removeMCPServerTools(server string) {
	a.mcpSyncMu.Lock()
	defer a.mcpSyncMu.Unlock()
	for _, name := range a.mcpToolNames[server] {
		_ = a.reg.Unregister(name)
	}
	a.mcpToolNames[server] = nil
}

// mcpToolRegistration identifies an externally owned MCP capability at the
// registry boundary. The server name is part of the owner so two server
// generations cannot be mistaken for the same provenance after reconnect.
func mcpToolRegistration(server string) agenttools.RegistrationInfo {
	return agenttools.RegistrationInfo{
		Owner:  "mcp:" + server,
		Plugin: "mcp",
	}
}

// bridgedMcpTool is one MCP server tool registered by registerMcps as
// mcp.<server>.<tool> (dispatch-m6f-2 §4). It implements the tools.Tool method
// set structurally (Go structural typing) — cmd/pa is the composition root, so
// importing internal/tools here is fine. Executing it calls back into the live
// server client via tools/call with the model's arguments passed through
// verbatim; the registry's D7 gate validates those arguments against the
// server's own input schema, which is passed through to Schema().
type bridgedMcpTool struct {
	client         mcp.Client
	name           string
	tool           string
	desc           string
	schema         map[string]any
	output         map[string]any
	taskSupport    string
	attachments    *attachment.Store
	maxImageBytes  int
	imageAdmission func(context.Context) error
	onEvent        func(string, any) error
}

func (t bridgedMcpTool) Name() string { return t.name }

// The bridge forwards the execution context through tools/call and reconnect
// start; cancellation is not treated as a retryable connection failure.
func (bridgedMcpTool) CancellationAware() bool  { return true }
func (t bridgedMcpTool) Description() string    { return t.desc }
func (t bridgedMcpTool) Schema() map[string]any { return t.schema }
func (t bridgedMcpTool) OutputSchema() map[string]any {
	structured := map[string]any{}
	required := []string{"content"}
	if t.output != nil {
		structured = t.output
		required = append(required, "structuredContent")
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"content": map[string]any{"type": "array", "items": map[string]any{}}, "structuredContent": structured},
		"required":             required,
		"additionalProperties": false,
	}
}

func (t bridgedMcpTool) Execute(ctx context.Context, args any) (string, error) {
	result, err := t.ExecuteResult(ctx, args)
	return result.Output, err
}

func (t bridgedMcpTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	if t.taskSupport == "required" {
		return agenttools.ToolResult{}, fmt.Errorf("%s: MCP task-based execution is not supported", t.name)
	}
	var a map[string]any
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	res, err := callMCPWithReconnect(ctx, t.client, t.tool, a)
	if err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if err := t.emitEvent(ctx, session.EventMcpCall, session.NewMcpCall(t.tool, res.IsError)); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("%s: persist event: %w", t.name, err)
	}
	value := map[string]any{"content": res.Content}
	if res.StructuredContentSet {
		value["structuredContent"] = res.StructuredContent
	}
	contentStore := t.attachments
	contentAdmission := t.imageAdmission
	if res.IsError {
		// isError is a completed MCP call, but failed results must not persist
		// untrusted images into the attachment store.
		contentStore = nil
		contentAdmission = nil
	}
	content := projectMcpContentWithAdmission(ctx, t.name, res.Content, contentStore, t.maxImageBytes, contentAdmission)
	if res.IsError {
		// MCP tool-level failures are successful JSON-RPC calls but failed tool
		// results. Match the reference bridge and let ToolRuntime persist an
		// isError result instead of converting it into a transport exception.
		return agenttools.ToolResult{
			Output:  mcp.FormatCallResult(res),
			Content: content,
			IsError: true,
			Error:   &agenttools.ErrorInfo{Name: "MCPToolError", Code: "MCP_TOOL_ERROR"},
		}, nil
	}
	// Same rendering as mcp_call (mcp.FormatCallResult), so a server tool's
	// result reads identically through either path.
	return agenttools.ToolResult{Value: value, Output: mcp.FormatCallResult(res), Content: content}, nil
}

func (t bridgedMcpTool) emitEvent(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok {
		if runtime.Emit == nil {
			return errors.New("MCP runtime event sink is unavailable")
		}
		return runtime.Emit(typ, data)
	}
	if t.onEvent != nil {
		return t.onEvent(typ, data)
	}
	return nil
}

// callMCPWithReconnect preserves the reference MCP call boundary: one
// tools/call request produces one client call. The reconnect supervisor may
// recover the connection in the background, but this function never replays a
// request whose commit status is unknown.
func callMCPWithReconnect(ctx context.Context, client mcp.Client, name string, args map[string]any) (mcp.CallResult, error) {
	if client == nil {
		return mcp.CallResult{}, errors.New("MCP client is unavailable")
	}
	return client.Call(ctx, name, args)
}

// projectMcpContent preserves DSH's rich result boundary: text remains text,
// supported MCP image blocks are durably stored as ImageRef values, and
// unsupported or unadmitted blocks become explicit diagnostic text while the
// canonical Value above still retains the raw protocol content.
func projectMcpContent(toolName string, items []any, store *attachment.Store, maxImageBytes int) []llm.ContentBlock {
	return projectMcpContentWithAdmission(context.Background(), toolName, items, store, maxImageBytes, nil)
}

func projectMcpContentWithAdmission(ctx context.Context, toolName string, items []any, store *attachment.Store, maxImageBytes int, admission func(context.Context) error) []llm.ContentBlock {
	imageInputs := make([]attachment.ImageInput, 0)
	imageIndexes := make([]int, 0)
	imageReasons := make(map[int]string)
	for index, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := obj["type"].(string); typ != "image" {
			continue
		}
		mime, _ := obj["mimeType"].(string)
		data, _ := obj["data"].(string)
		if attachment.MediaTypeForExtension("."+strings.TrimPrefix(mime, "image/")) == "" {
			imageReasons[index] = "the declared media type is not PNG, JPEG, WebP, or GIF"
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil || data == "" || base64.StdEncoding.EncodeToString(decoded) != data {
			imageReasons[index] = "the image data is not canonical base64"
			continue
		}
		imageIndexes = append(imageIndexes, index)
		imageInputs = append(imageInputs, attachment.ImageInput{MediaType: mime, Data: decoded})
	}
	imageRefs := make(map[int]llm.ImageRef)
	if len(imageReasons) == 0 && len(imageInputs) > 0 {
		reason := ""
		if store == nil {
			reason = "no attachment store is mounted"
		} else if admission != nil {
			if err := admission(ctx); err != nil {
				reason = err.Error()
			}
		}
		if reason == "" {
			refs, err := store.SaveImages(imageInputs, maxImageBytes)
			if err != nil {
				reason = fmt.Sprintf("image admission rejected the result: %v", err)
			} else {
				for i, index := range imageIndexes {
					imageRefs[index] = refs[i]
				}
			}
		}
		if reason != "" {
			for _, index := range imageIndexes {
				imageReasons[index] = reason
			}
		}
	}
	blocks := make([]llm.ContentBlock, 0, len(items))
	for index, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			blocks = append(blocks, llm.Text("[unsupported MCP content block: expected an object]"))
			continue
		}
		typ, _ := obj["type"].(string)
		switch typ {
		case "text":
			text, _ := obj["text"].(string)
			blocks = append(blocks, llm.Text(text))
		case "image":
			mime, _ := obj["mimeType"].(string)
			if ref, ok := imageRefs[index]; ok {
				blocks = append(blocks, llm.ContentBlock{Kind: llm.BlockImage, Image: ref})
			} else {
				reason := imageReasons[index]
				if reason == "" {
					reason = "another image in the same result was invalid or was not admitted"
				}
				blocks = append(blocks, llm.Text(fmt.Sprintf("[image unavailable: %s; %s; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type"), reason)))
			}
		case "resource_link":
			name, _ := obj["name"].(string)
			uri, _ := obj["uri"].(string)
			if name == "" || uri == "" {
				blocks = append(blocks, llm.Text("[resource link unavailable: the MCP block is missing its name or URI]"))
			} else {
				blocks = append(blocks, llm.Text(fmt.Sprintf("Resource link: %s (%s)", name, uri)))
			}
		case "audio":
			mime, _ := obj["mimeType"].(string)
			blocks = append(blocks, llm.Text(fmt.Sprintf("[audio result unsupported: %s; raw audio data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type"))))
		case "resource":
			blocks = append(blocks, llm.Text("[embedded resource unsupported; raw resource data remains available to programmatic callers]"))
		default:
			blocks = append(blocks, llm.Text(fmt.Sprintf("[unsupported MCP content type: %s]", typ)))
		}
	}
	if len(blocks) == 0 {
		return []llm.ContentBlock{llm.Text(fmt.Sprintf("(%s returned no model-visible content)", toolName))}
	}
	return blocks
}

func projectMcpImage(ctx context.Context, obj map[string]any, store *attachment.Store, maxImageBytes int, admission func(context.Context) error) llm.ContentBlock {
	mime, _ := obj["mimeType"].(string)
	data, _ := obj["data"].(string)
	if store == nil {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; no attachment store is mounted; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type")))
	}
	if admission != nil {
		if err := admission(ctx); err != nil {
			return llm.Text(fmt.Sprintf("[image unavailable: %s; %v; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type"), err))
		}
	}
	if attachment.MediaTypeForExtension("."+strings.TrimPrefix(mime, "image/")) == "" {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; the declared media type is not PNG, JPEG, WebP, or GIF; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type")))
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || data == "" || base64.StdEncoding.EncodeToString(decoded) != data {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; the image data is not canonical base64; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type")))
	}
	ref, err := store.SaveImage(mime, decoded, maxImageBytes)
	if err != nil {
		return llm.Text(fmt.Sprintf("[image unavailable: %s; image admission rejected the result: %v; raw image data remains available to programmatic callers]", fallbackMcpString(mime, "unknown media type"), err))
	}
	return llm.ContentBlock{Kind: llm.BlockImage, Image: ref}
}

// mcpImageAdmission is the application-level counterpart of the ACP image
// route check. MCP results are untrusted input: storing an image as model
// context is allowed only when the addressed session currently has an image
// capable route and multimodal storage is enabled. A refusal remains a
// model-visible diagnostic while the canonical MCP value retains raw content
// for programmatic callers.
func (a *app) mcpImageAdmission(ctx context.Context) error {
	if a == nil || !a.multimodalEnabled() || a.attachStore == nil {
		return errors.New("image input is not enabled for this session")
	}
	if !a.llmSupportsImagesForSession(a.runtimeSessionID(ctx)) {
		return errors.New("the current model route does not declare image input")
	}
	return nil
}

func fallbackMcpString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// normalizeSchema adapts a server-provided inputSchema for registration in the
// tools registry (dispatch-m6f-2 §4: schema passthrough): the nil schema
// becomes an empty object (the registry compiles it as accept-anything) and a
// schema missing "type" is assumed to be an object (MCP tool arguments are
// always a JSON object), so the D7 gate rejects non-object arguments cleanly.
// Everything else is passed through verbatim.
func normalizeSchema(s map[string]any) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out
}

func normalizeOutputSchema(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// toMcpServers maps the configured config.McpServer entries onto the mcp
// package's own McpServer shape (identical fields), keeping the seam decoupled
// from config (D2).
func toMcpServers(servers []config.McpServer) []mcp.McpServer {
	out := make([]mcp.McpServer, 0, len(servers))
	for _, s := range servers {
		out = append(out, mcp.McpServer{Name: s.Name, Transport: s.Transport, Cmd: s.Cmd, Args: s.Args, URL: s.URL, Headers: s.Headers, Env: s.Env, Cwd: s.Cwd, ToolCallTimeout: time.Duration(s.ToolCallTimeoutMS) * time.Millisecond})
	}
	return out
}

// mcpServerNames returns the configured server names for the /help status line.
func mcpServerNames(servers []config.McpServer) string {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}
