package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

func TestRegisterExtensionsComposesDiscoveryContextToolsAndApproval(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "extension.yaml")
	manifest := extension.Manifest{
		ID: "composed", Name: "Composed", Version: "0.1.0", ExtensionAPI: "1.0",
		Capabilities: extension.Capabilities{Tools: true, ContextProvider: true, Lifecycle: true, Health: true},
		Transport: extension.Transport{
			Type: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestRegisterExtensionsHelperProcess$", "--"},
			Env: []string{"GO_WANT_COMPOSED_EXTENSION=1"},
		},
		Tools: extension.ToolsContribution{Definitions: []extension.ToolDefinition{{
			Name: "write", Description: "write", Risk: extension.ToolRiskWrite,
			InputSchema: map[string]any{"type": "object"},
		}}},
		ContextProvider: extension.ContextProviderConfig{Enabled: true, Strategy: extension.ContextBeforeEveryModelCall},
		Health:          extension.HealthConfig{Enabled: true, TimeoutMS: 1000},
		Lifecycle:       extension.LifecycleConfig{Enabled: true, RestartPolicy: extension.RestartNever},
		Permissions:     []extension.Permission{{Name: "session.id"}, {Name: "user.input", Required: true}},
	}
	var manifestYAML strings.Builder
	if err := extension.WriteManifest(&manifestYAML, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifestYAML.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tools.New()
	policy := tools.Policy{Timeout: 5 * time.Second, Enabled: []string{PublicExtensionToolName("composed", "write")}}
	registry.SetPolicy(policy)
	app := &app{
		cfg: config.Config{DataDir: dir, Extensions: config.ExtensionsConfig{
			Enabled: true, StartupTimeoutMS: 5000, HealthTimeoutMS: 1000, ContextTimeoutMS: 1000,
			ShutdownTimeoutMS: 1000, GlobalContextChars: 4000, MaxContributionChars: 2000,
			GlobalContextTokens: 1000, MaxContributionTokens: 500,
			Sources: []config.ExtensionSourceConfig{{Manifest: manifestPath, Grants: []string{"session.id", "user.input"}}},
		}},
		configPath: filepath.Join(dir, "config.yaml"), reg: registry, basePolicy: policy,
	}
	if err := app.registerExtensions(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = app.extensions.Close()
		_ = app.extensionEventLog.Close()
	}()
	name := PublicExtensionToolName("composed", "write")
	if _, ok := registry.Registration(name); !ok {
		t.Fatalf("extension tool %q was not registered", name)
	}
	if !containsSensitive(app.cfg.Interact.SensitiveTools, name) {
		t.Fatalf("extension tool was not marked sensitive: %#v", app.cfg.Interact.SensitiveTools)
	}
	injectors := app.preStepInjectorsForSession("session", nil)
	var extensionInjector int = -1
	for index, injector := range injectors {
		if injector.Name == "extensions" {
			extensionInjector = index
		}
	}
	if extensionInjector < 0 {
		t.Fatalf("extension injectors = %#v", injectors)
	}
	ctx := runtimectx.WithCorrelation(context.Background(), runtimectx.Correlation{SessionID: "session"})
	messages, err := injectors[extensionInjector].InjectWithError(ctx, "composed question")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Text(), "composed question") {
		t.Fatalf("composed context = %#v", messages)
	}
	if _, err := registry.Execute(ctx, name, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func PublicExtensionToolName(extensionID, toolName string) string {
	return "ext__" + extensionID + "__" + toolName
}

func TestRegisterExtensionsHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_COMPOSED_EXTENSION") != "1" {
		return
	}
	manifest := extension.Manifest{
		ID: "composed", Name: "Composed", Version: "0.1.0", ExtensionAPI: "1.0", RequiredAgentAPI: "1.0",
		Capabilities: extension.Capabilities{Tools: true, ContextProvider: true, Lifecycle: true, Health: true},
	}
	server := extension.NewServer(extension.ServerCallbacks{
		Manifest: manifest,
		Health: func(context.Context) (extension.HealthResult, error) {
			return extension.HealthResult{Ready: true}, nil
		},
		ProvideContext: func(_ context.Context, request extension.ContextRequest) (extension.ContextResult, error) {
			return extension.ContextResult{Contributions: []extension.ContextContribution{{
				Source: "composed", Content: "composed evidence: " + request.UserInput, Truncatable: true,
			}}}, nil
		},
		CallTool: func(context.Context, extension.ToolCallRequest) (extension.ToolCallResult, error) {
			return extension.ToolCallResult{Value: "written"}, nil
		},
	})
	if err := server.Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}
