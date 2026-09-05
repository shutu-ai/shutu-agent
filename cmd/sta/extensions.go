package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/extensionhost"
	"github.com/shutu-ai/shutu-agent/internal/loop"
)

type extensionEventLogger struct {
	mu   sync.Mutex
	file *os.File
}

func newExtensionEventLog(dataDir string) (*extensionEventLogger, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("sta: extension telemetry directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dataDir, "extension_events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sta: extension telemetry log: %w", err)
	}
	return &extensionEventLogger{file: file}, nil
}

func (l *extensionEventLogger) Write(event extensionhost.Event) error {
	if l == nil {
		return nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *extensionEventLogger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// registerExtensions discovers independent Extension v1 processes, negotiates
// capabilities, and publishes their tools through the canonical registry. It
// runs before the approval gate so extension-declared risk cannot bypass it.
func (a *app) registerExtensions() error {
	cfg := a.cfg.Extensions
	if !cfg.Enabled {
		return nil
	}
	explicit := make([]extensionhost.Source, 0, len(cfg.Sources))
	configDir := filepath.Dir(a.configPath)
	for _, source := range cfg.Sources {
		if strings.TrimSpace(source.Manifest) == "" {
			return errors.New("sta: extension source manifest is required")
		}
		manifest := resolveConfigPath(configDir, source.Manifest)
		explicit = append(explicit, extensionhost.Source{ManifestPath: manifest, Required: source.Required, Grants: append([]string(nil), source.Grants...)})
	}
	directory := cfg.Directory
	if directory != "" && !filepath.IsAbs(directory) {
		directory = filepath.Clean(filepath.Join(configDir, directory))
	}
	sources, err := extensionhost.Discover(explicit, directory)
	if err != nil {
		return fmt.Errorf("sta: discover extensions: %w", err)
	}
	workspace := a.cfg.Workspace.DefaultDir
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	eventLog, err := newExtensionEventLog(a.cfg.DataDir)
	if err != nil {
		return err
	}
	host := extensionhost.New(extensionhost.Config{
		AgentName:             "shutu-agent",
		AgentVersion:          "0.1",
		Workspace:             workspace,
		StartupTimeout:        time.Duration(cfg.StartupTimeoutMS) * time.Millisecond,
		HealthTimeout:         time.Duration(cfg.HealthTimeoutMS) * time.Millisecond,
		ContextTimeout:        time.Duration(cfg.ContextTimeoutMS) * time.Millisecond,
		ShutdownTimeout:       time.Duration(cfg.ShutdownTimeoutMS) * time.Millisecond,
		GlobalContextChars:    cfg.GlobalContextChars,
		MaxContributionChars:  cfg.MaxContributionChars,
		GlobalContextTokens:   cfg.GlobalContextTokens,
		MaxContributionTokens: cfg.MaxContributionTokens,
		Sources:               sources,
		Grants:                cfg.Grants,
		Registry:              a.reg,
		AllowedTools:          allowedToolSet(a.basePolicy.Enabled),
		Observer: func(event extensionhost.Event) {
			var err error
			if !event.Success {
				err = errors.New(event.Error)
			}
			if a.metrics != nil {
				a.metrics.Extension(err)
			}
			_ = eventLog.Write(event)
		},
		OnWebContributions: func([]extensionhost.WebContribution) {
			if a.webserver != nil {
				a.webserver.SetExtensionRoutes(a.extensionRoutes())
			}
		},
	})
	if err := host.Start(context.Background()); err != nil {
		_ = host.Close()
		_ = eventLog.Close()
		return fmt.Errorf("sta: extensions: %w", err)
	}
	a.extensionEventLog = eventLog
	a.extensions = host
	sensitive := a.extensions.SensitiveTools()
	if len(sensitive) > 0 {
		a.cfg.Interact.SensitiveTools = append(a.cfg.Interact.SensitiveTools, sensitive...)
	}
	return nil
}

func allowedToolSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

func (a *app) extensionPreSteps() []loop.PreStepInjector {
	if a == nil || a.extensions == nil {
		return nil
	}
	return []loop.PreStepInjector{a.extensions.ContextInjector()}
}

func resolveConfigPath(configDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(configDir, path))
}
