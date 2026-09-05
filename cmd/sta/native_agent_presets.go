package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/webserver"
)

const (
	nativeAgentPresetPromptFile   = "prompt.md"
	nativeAgentPresetMetadataFile = "metadata.json"
)

type nativeAgentPresetMetadata struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Mode        string `json:"mode,omitempty"`
}

// nativeAgentPresetStore is the local authoring root for DSH presets. A user
// preset is a directory containing only a prompt and metadata; the wire API
// exposes ids, never these paths.
type nativeAgentPresetStore struct {
	root          string
	promptsDir    string
	defaultMu     sync.RWMutex
	defaultPreset string
}

func nativeAgentPresetKnown(id string) bool {
	return id == config.ModeMinimal || id == config.ModeStandard || id == config.ModeCode
}

func nativeAgentPresetIDValid(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for index := 0; index < len(id); index++ {
		char := id[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || char == '-' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func newNativeAgentPresetStore(dataDir, promptsDir, defaultPreset string) *nativeAgentPresetStore {
	return &nativeAgentPresetStore{
		root: filepath.Join(dataDir, "agent-presets"), promptsDir: promptsDir, defaultPreset: defaultPreset,
	}
}

func (p *nativeAgentPresetStore) List(context.Context) (webserver.NativeAgentPresetCatalog, error) {
	p.defaultMu.RLock()
	defaultPreset := p.defaultPreset
	p.defaultMu.RUnlock()
	entries := []webserver.NativeAgentPreset{
		{ID: config.ModeMinimal, Trust: "system", IsDefault: defaultPreset == config.ModeMinimal, Name: "Minimal", Description: "Basic read, shell, and file capabilities."},
		{ID: config.ModeStandard, Trust: "system", IsDefault: defaultPreset == config.ModeStandard, Name: "Standard", Description: "The standard Shutu capability set."},
		{ID: config.ModeCode, Trust: "system", IsDefault: defaultPreset == config.ModeCode, Name: "Code", Description: "Standard capabilities with programmatic Code Mode."},
	}
	items, err := os.ReadDir(p.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return webserver.NativeAgentPresetCatalog{}, fmt.Errorf("read agent preset root: %w", err)
	}
	userEntries := make([]webserver.NativeAgentPreset, 0, len(items))
	for _, item := range items {
		if !item.IsDir() || !nativeAgentPresetIDValid(item.Name()) || nativeAgentPresetKnown(item.Name()) {
			continue
		}
		entry := webserver.NativeAgentPreset{ID: item.Name(), Trust: "user"}
		metadata, metadataErr := p.readMetadata(item.Name())
		if metadataErr != nil {
			entry.Broken = metadataErr.Error()
		} else {
			entry.Name, entry.Description = metadata.Name, metadata.Description
		}
		userEntries = append(userEntries, entry)
	}
	sort.Slice(userEntries, func(i, j int) bool { return userEntries[i].ID < userEntries[j].ID })
	entries = append(entries, userEntries...)
	return webserver.NativeAgentPresetCatalog{
		Presets: entries, Authorable: true, HasDocument: nativeAgentPresetOpenSupported(),
	}, nil
}

// SetDefault updates the in-process roster after the native settings provider
// has persisted the deployment default. It is intentionally small: session
// creation reads this value, while existing sessions retain their preset.
func (p *nativeAgentPresetStore) SetDefault(id string) {
	p.defaultMu.Lock()
	p.defaultPreset = id
	p.defaultMu.Unlock()
}

func (p *nativeAgentPresetStore) Read(_ context.Context, id string) (webserver.NativeAgentPresetDetails, error) {
	if !nativeAgentPresetIDValid(id) {
		return webserver.NativeAgentPresetDetails{}, fmt.Errorf("invalid agent preset id %q", id)
	}
	if nativeAgentPresetKnown(id) {
		content, err := p.builtinContent(id)
		if err != nil {
			return webserver.NativeAgentPresetDetails{}, err
		}
		return webserver.NativeAgentPresetDetails{
			AgentPreset: id, Trust: "system", Content: content,
			Name: nativeAgentPresetDisplayName(id), Description: nativeAgentPresetDescription(id),
		}, nil
	}
	metadata, err := p.readMetadata(id)
	if err != nil {
		return webserver.NativeAgentPresetDetails{}, err
	}
	content, err := os.ReadFile(p.filePath(id, nativeAgentPresetPromptFile))
	if err != nil {
		return webserver.NativeAgentPresetDetails{}, fmt.Errorf("read agent preset %q: %w", id, err)
	}
	return webserver.NativeAgentPresetDetails{
		AgentPreset: id, Trust: "user", Content: string(content),
		Name: metadata.Name, Description: metadata.Description,
	}, nil
}

func (p *nativeAgentPresetStore) Copy(_ context.Context, from, id, name string) (string, error) {
	if !nativeAgentPresetIDValid(from) || !nativeAgentPresetIDValid(id) {
		return "", errors.New("from and agent preset ids must be valid")
	}
	if nativeAgentPresetKnown(id) {
		return "", fmt.Errorf("cannot overwrite system agent preset %q", id)
	}
	if _, err := os.Stat(p.filePath(id, nativeAgentPresetMetadataFile)); err == nil {
		return "", fmt.Errorf("agent preset %q already exists", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("check agent preset %q: %w", id, err)
	}
	source, err := p.Read(context.Background(), from)
	if err != nil {
		return "", fmt.Errorf("read source preset %q: %w", from, err)
	}
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		return "", fmt.Errorf("create agent preset root: %w", err)
	}
	temp, err := os.MkdirTemp(p.root, ".preset-")
	if err != nil {
		return "", fmt.Errorf("create agent preset staging directory: %w", err)
	}
	defer os.RemoveAll(temp)
	metadata := nativeAgentPresetMetadata{Name: strings.TrimSpace(name), Description: source.Description, Mode: source.AgentPreset}
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode agent preset metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temp, nativeAgentPresetPromptFile), []byte(source.Content), 0o600); err != nil {
		return "", fmt.Errorf("write agent preset prompt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temp, nativeAgentPresetMetadataFile), metadataBytes, 0o600); err != nil {
		return "", fmt.Errorf("write agent preset metadata: %w", err)
	}
	if err := os.Rename(temp, p.filePath(id, "")); err != nil {
		return "", fmt.Errorf("publish agent preset %q: %w", id, err)
	}
	return id, nil
}

func (p *nativeAgentPresetStore) OpenDocument(ctx context.Context, id string) (webserver.NativeAgentPresetDocument, error) {
	if !nativeAgentPresetIDValid(id) {
		return webserver.NativeAgentPresetDocument{}, errors.New("invalid agent preset id")
	}
	if nativeAgentPresetKnown(id) {
		return webserver.NativeAgentPresetDocument{}, fmt.Errorf("system agent preset %q cannot be edited", id)
	}
	if _, err := p.Read(ctx, id); err != nil {
		return webserver.NativeAgentPresetDocument{}, err
	}
	path := p.filePath(id, "")
	if !nativeAgentPresetOpenSupported() {
		return webserver.NativeAgentPresetDocument{Path: path}, nil
	}
	if err := webserver.OpenNativePath(ctx, path); err != nil {
		return webserver.NativeAgentPresetDocument{}, fmt.Errorf("open agent preset %q: %w", id, err)
	}
	return webserver.NativeAgentPresetDocument{Opened: true}, nil
}

func (p *nativeAgentPresetStore) Remove(_ context.Context, id string) error {
	if !nativeAgentPresetIDValid(id) {
		return errors.New("invalid agent preset id")
	}
	if nativeAgentPresetKnown(id) {
		return fmt.Errorf("system agent preset %q cannot be removed", id)
	}
	if _, err := p.readMetadata(id); err != nil {
		return err
	}
	if err := os.RemoveAll(p.filePath(id, "")); err != nil {
		return fmt.Errorf("remove agent preset %q: %w", id, err)
	}
	return nil
}

func (p *nativeAgentPresetStore) Prompt(id string) (*prompt.Builder, error) {
	details, err := p.Read(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if nativeAgentPresetKnown(id) {
		return buildPrompt(id, p.promptsDir)
	}
	return prompt.New(details.Content), nil
}

func (p *nativeAgentPresetStore) Mode(id string) string {
	if nativeAgentPresetKnown(id) {
		return id
	}
	metadata, err := p.readMetadata(id)
	if err != nil || !nativeAgentPresetKnown(metadata.Mode) {
		return config.ModeStandard
	}
	return metadata.Mode
}

func (p *nativeAgentPresetStore) builtinContent(id string) (string, error) {
	b, err := buildPrompt(id, p.promptsDir)
	if err != nil {
		return "", fmt.Errorf("build agent preset %q: %w", id, err)
	}
	return b.Build(), nil
}

func (p *nativeAgentPresetStore) readMetadata(id string) (nativeAgentPresetMetadata, error) {
	data, err := os.ReadFile(p.filePath(id, nativeAgentPresetMetadataFile))
	if err != nil {
		return nativeAgentPresetMetadata{}, fmt.Errorf("read agent preset metadata %q: %w", id, err)
	}
	var metadata nativeAgentPresetMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nativeAgentPresetMetadata{}, fmt.Errorf("parse agent preset metadata %q: %w", id, err)
	}
	if metadata.Mode != "" && !nativeAgentPresetKnown(metadata.Mode) {
		return nativeAgentPresetMetadata{}, fmt.Errorf("agent preset %q has invalid source mode", id)
	}
	return metadata, nil
}

func (p *nativeAgentPresetStore) filePath(id, name string) string {
	base := filepath.Join(p.root, id)
	if name == "" {
		return base
	}
	return filepath.Join(base, name)
}

func nativeAgentPresetOpenSupported() bool {
	switch runtime.GOOS {
	case "windows", "darwin", "linux", "freebsd", "openbsd", "netbsd":
		return true
	default:
		return false
	}
}

func nativeAgentPresetDisplayName(id string) string {
	switch id {
	case config.ModeMinimal:
		return "Minimal"
	case config.ModeCode:
		return "Code"
	default:
		return "Standard"
	}
}

func nativeAgentPresetDescription(id string) string {
	switch id {
	case config.ModeMinimal:
		return "Basic read, shell, and file capabilities."
	case config.ModeCode:
		return "Standard capabilities with programmatic Code Mode."
	default:
		return "The standard Shutu capability set."
	}
}
