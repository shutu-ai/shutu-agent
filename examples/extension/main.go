package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("demo-extension: start web listener: %v", err)
	}
	webServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>Demo Extension</title><h1>Demo Extension</h1><p>This page is served by an independent extension process.</p>")
	})}
	go func() { _ = webServer.Serve(listener) }()
	defer func() { _ = webServer.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	manifest := extension.Manifest{
		ID: "demo", Name: "Demo Extension", Version: "0.1.0", ExtensionAPI: "1.0",
		Description: "A generic example that contributes context, a tool, health and a web page.",
		Capabilities: extension.Capabilities{
			Tools: true, ContextProvider: true, Lifecycle: true, Web: true, Health: true, Events: true,
		},
		Transport: extension.Transport{Type: "stdio", Command: "demo-extension"},
		Tools: extension.ToolsContribution{Definitions: []extension.ToolDefinition{{
			Name: "echo", Description: "Echo text supplied by the model",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required": []any{"text"},
			},
			OutputSchema: map[string]any{"type": "string"}, Risk: extension.ToolRiskRead,
		}}},
		ContextProvider: extension.ContextProviderConfig{
			Enabled: true, Strategy: extension.ContextBeforeEveryModelCall, TimeoutMS: 2000, MaxChars: 1200, Priority: 10,
		},
		Web:       extension.WebContribution{Enabled: true, Route: "/extensions/demo/", Title: "Demo", ServiceURL: "http://" + listener.Addr().String() + "/"},
		Health:    extension.HealthConfig{Enabled: true, TimeoutMS: 1000},
		Lifecycle: extension.LifecycleConfig{Enabled: true, StartupTimeoutMS: 5000, ShutdownTimeoutMS: 2000, RestartPolicy: extension.RestartOnFailure, MaxRestarts: 3},
		Permissions: []extension.Permission{
			{Name: "session.id"}, {Name: "session.turn"}, {Name: "session.step"},
			{Name: "workspace.path"}, {Name: "user.input", Required: true},
		},
		ConfigurationSpec: map[string]any{
			"type": "object", "properties": map[string]any{"exampleSetting": map[string]any{"type": "string"}},
		},
	}
	server := extension.NewServer(extension.ServerCallbacks{
		Manifest:    manifest,
		WebBaseURL:  func() string { return "http://" + listener.Addr().String() + "/" },
		Restartable: true,
		Health: func(context.Context) (extension.HealthResult, error) {
			return extension.HealthResult{Ready: true, Status: "ready"}, nil
		},
		ProvideContext: func(_ context.Context, request extension.ContextRequest) (extension.ContextResult, error) {
			query := strings.TrimSpace(request.UserInput)
			if query == "" {
				return extension.ContextResult{}, nil
			}
			return extension.ContextResult{Contributions: []extension.ContextContribution{{
				Source: "demo", Content: "Demo extension evidence for: " + query,
				Priority: 10, EstimatedTokens: len(query)/4 + 8, Truncatable: true,
			}}}, nil
		},
		CallTool: func(_ context.Context, request extension.ToolCallRequest) (extension.ToolCallResult, error) {
			return extension.ToolCallResult{Value: request.Arguments["text"]}, nil
		},
		OnEvent: func(context.Context, extension.Event) error { return nil },
	})
	if err := server.Run(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		log.Fatalf("demo-extension: %v", err)
	}
}
