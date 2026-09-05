// Package extension defines Shutu Agent's public Extension Contract v1.
// The package intentionally has no Shutu internal imports: independently
// released extensions can compile against these DTOs without reaching into
// the Agent's implementation packages.
package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// ProtocolVersion is the wire protocol identifier. It changes only with an
	// incompatible framing/method contract.
	ProtocolVersion = "shutu-extension/1"
	// APIVersion is the current v1 minor revision exposed by this SDK.
	APIVersion = "1.0"
)

var (
	extensionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{1,63}$`)
	toolNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

// Manifest is the local, versioned description of one extension deployment.
// Tool schemas and declarations may also be returned by initialize; the
// manifest remains the immutable identity, permission request, and risk record.
type Manifest struct {
	ID                string                `json:"id" yaml:"id"`
	Name              string                `json:"name" yaml:"name"`
	Version           string                `json:"version" yaml:"version"`
	Description       string                `json:"description,omitempty" yaml:"description"`
	ExtensionAPI      string                `json:"extensionApi" yaml:"extension_api"`
	RequiredAgentAPI  string                `json:"requiredAgentApi,omitempty" yaml:"required_agent_api"`
	Capabilities      Capabilities          `json:"capabilities" yaml:"capabilities"`
	Transport         Transport             `json:"transport" yaml:"transport"`
	Tools             ToolsContribution     `json:"tools" yaml:"tools"`
	ContextProvider   ContextProviderConfig `json:"contextProvider" yaml:"context_provider"`
	Web               WebContribution       `json:"web" yaml:"web"`
	Events            EventSubscription     `json:"events" yaml:"events"`
	Health            HealthConfig          `json:"health" yaml:"health"`
	Lifecycle         LifecycleConfig       `json:"lifecycle" yaml:"lifecycle"`
	Permissions       []Permission          `json:"permissions,omitempty" yaml:"permissions"`
	ConfigurationSpec map[string]any        `json:"configurationSchema,omitempty" yaml:"configuration_schema"`
}

type Capabilities struct {
	Tools           bool `json:"tools" yaml:"tools"`
	ContextProvider bool `json:"contextProvider" yaml:"context_provider"`
	Lifecycle       bool `json:"lifecycle" yaml:"lifecycle"`
	Web             bool `json:"web" yaml:"web"`
	Health          bool `json:"health" yaml:"health"`
	Events          bool `json:"events" yaml:"events"`
}

type Transport struct {
	// Type is "stdio" for an Agent-managed child process or "http" for an
	// independently started local service.
	Type    string   `json:"type" yaml:"type"`
	Command string   `json:"command,omitempty" yaml:"command"`
	Args    []string `json:"args,omitempty" yaml:"args"`
	Env     []string `json:"env,omitempty" yaml:"env"`
	Workdir string   `json:"workdir,omitempty" yaml:"workdir"`
	// Endpoint is required for type=http and is an Extension v1 JSON-RPC endpoint.
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint"`
}

type ToolsContribution struct {
	Definitions []ToolDefinition `json:"definitions,omitempty" yaml:"definitions"`
}

type ToolDefinition struct {
	Name         string         `json:"name" yaml:"name"`
	Description  string         `json:"description" yaml:"description"`
	InputSchema  map[string]any `json:"inputSchema" yaml:"input_schema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty" yaml:"output_schema"`
	Risk         ToolRisk       `json:"risk" yaml:"risk"`
	// RequiresApproval is an extension request. The Agent's configured approval
	// policy remains authoritative and can always require more scrutiny.
	RequiresApproval bool `json:"requiresApproval,omitempty" yaml:"requires_approval"`
}

type ToolRisk string

const (
	ToolRiskRead               ToolRisk = "read"
	ToolRiskWrite              ToolRisk = "write"
	ToolRiskDestructive        ToolRisk = "destructive"
	ToolRiskExternalSideEffect ToolRisk = "external_side_effect"
	ToolRiskPrivileged         ToolRisk = "privileged"
)

type ContextProviderConfig struct {
	Enabled     bool              `json:"enabled" yaml:"enabled"`
	Strategy    ContextStrategy   `json:"strategy" yaml:"strategy"`
	Required    bool              `json:"required" yaml:"required"`
	TimeoutMS   int               `json:"timeoutMs,omitempty" yaml:"timeout_ms"`
	MaxChars    int               `json:"maxChars,omitempty" yaml:"max_chars"`
	Priority    int               `json:"priority,omitempty" yaml:"priority"`
	Description string            `json:"description,omitempty" yaml:"description"`
	Metadata    map[string]string `json:"metadata,omitempty" yaml:"metadata"`
}

type ContextStrategy string

const (
	ContextOncePerTurn          ContextStrategy = "once_per_turn"
	ContextBeforeEveryModelCall ContextStrategy = "before_every_model_call"
	ContextOnUserInputChange    ContextStrategy = "on_user_input_change"
	ContextAfterToolResult      ContextStrategy = "after_tool_result"
	ContextManual               ContextStrategy = "manual"
)

type WebContribution struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Route   string `json:"route,omitempty" yaml:"route"`
	Title   string `json:"title,omitempty" yaml:"title"`
	// Icon is presentation metadata only. Agents must provide a generic
	// fallback because older manifests may omit it.
	Icon string `json:"icon,omitempty" yaml:"icon"`
	// NavigationEnabled is optional. Nil preserves v1.0 behavior: every Web
	// contribution is navigable.
	NavigationEnabled *bool  `json:"navigationEnabled,omitempty" yaml:"navigation_enabled"`
	NavigationGroup   string `json:"navigationGroup,omitempty" yaml:"navigation_group"`
	Order             int    `json:"order,omitempty" yaml:"order"`
	// ServiceURL may be omitted for a stdio extension; initialize can return the
	// actual ephemeral URL after it starts its local listener.
	ServiceURL string `json:"serviceUrl,omitempty" yaml:"service_url"`
}

// EventSubscription is allow-list only. A capability declaration without
// event types receives nothing; the Agent never performs an unfiltered fanout.
type EventSubscription struct {
	Subscribe []string `json:"subscribe,omitempty" yaml:"subscribe"`
}

type HealthConfig struct {
	Enabled   bool `json:"enabled" yaml:"enabled"`
	TimeoutMS int  `json:"timeoutMs,omitempty" yaml:"timeout_ms"`
}

type LifecycleConfig struct {
	Enabled           bool          `json:"enabled" yaml:"enabled"`
	StartupTimeoutMS  int           `json:"startupTimeoutMs,omitempty" yaml:"startup_timeout_ms"`
	ShutdownTimeoutMS int           `json:"shutdownTimeoutMs,omitempty" yaml:"shutdown_timeout_ms"`
	RestartPolicy     RestartPolicy `json:"restartPolicy" yaml:"restart_policy"`
	MaxRestarts       int           `json:"maxRestarts,omitempty" yaml:"max_restarts"`
}

type RestartPolicy string

const (
	RestartNever     RestartPolicy = "never"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartAlways    RestartPolicy = "always"
)

type Permission struct {
	Name     string `json:"name" yaml:"name"`
	Reason   string `json:"reason,omitempty" yaml:"reason"`
	Required bool   `json:"required,omitempty" yaml:"required"`
}

// ParseManifest reads YAML or JSON. Unknown fields are rejected so a manifest
// intended for a newer minor API is not silently downgraded.
func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		if errors.Is(err, io.EOF) {
			return Manifest{}, errors.New("extension: manifest is empty")
		}
		return Manifest{}, fmt.Errorf("extension: parse manifest: %w", err)
	}
	return manifest, manifest.Validate()
}

func ParseManifestJSON(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("extension: parse manifest: %w", err)
	}
	return manifest, manifest.Validate()
}

func (m Manifest) Validate() error {
	if !extensionIDPattern.MatchString(m.ID) {
		return fmt.Errorf("extension: invalid id %q", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("extension: name is required")
	}
	if !validVersion(m.Version) {
		return fmt.Errorf("extension: invalid semantic version %q", m.Version)
	}
	major, _, err := VersionParts(m.ExtensionAPI)
	if err != nil || major != 1 {
		return fmt.Errorf("extension: extension_api must be v1-compatible, got %q", m.ExtensionAPI)
	}
	if strings.TrimSpace(m.RequiredAgentAPI) != "" {
		if _, _, err := VersionParts(m.RequiredAgentAPI); err != nil {
			return fmt.Errorf("extension: invalid required_agent_api %q", m.RequiredAgentAPI)
		}
	}
	switch strings.ToLower(strings.TrimSpace(m.Transport.Type)) {
	case "stdio":
		if strings.TrimSpace(m.Transport.Command) == "" {
			return errors.New("extension: stdio transport command is required")
		}
		if err := validateEnvironment(m.Transport.Env); err != nil {
			return err
		}
	case "http":
		if strings.TrimSpace(m.Transport.Endpoint) == "" {
			return errors.New("extension: http transport endpoint is required")
		}
	default:
		return fmt.Errorf("extension: unsupported transport %q", m.Transport.Type)
	}
	if m.ContextProvider.Enabled && !ValidContextStrategy(m.ContextProvider.Strategy) {
		return fmt.Errorf("extension: unsupported context strategy %q", m.ContextProvider.Strategy)
	}
	if m.ContextProvider.Enabled && !m.Capabilities.ContextProvider {
		return errors.New("extension: context_provider.enabled requires the context_provider capability")
	}
	if len(m.Tools.Definitions) > 0 && !m.Capabilities.Tools {
		return errors.New("extension: tool definitions require the tools capability")
	}
	if m.Web.Enabled && strings.TrimSpace(m.Web.Route) == "" {
		return errors.New("extension: web route is required")
	}
	if len([]rune(m.Web.Icon)) > 32 {
		return errors.New("extension: web icon must contain at most 32 runes")
	}
	if len([]rune(m.Web.NavigationGroup)) > 32 {
		return errors.New("extension: web navigation group must contain at most 32 runes")
	}
	if m.Web.Enabled && !m.Capabilities.Web {
		return errors.New("extension: web.enabled requires the web capability")
	}
	if len(m.Events.Subscribe) > 0 && !m.Capabilities.Events {
		return errors.New("extension: event subscriptions require the events capability")
	}
	seenEvents := make(map[string]struct{}, len(m.Events.Subscribe))
	for _, eventType := range m.Events.Subscribe {
		eventType = strings.TrimSpace(eventType)
		if !ValidEventType(eventType) {
			return fmt.Errorf("extension: unsupported event subscription %q", eventType)
		}
		if _, duplicate := seenEvents[eventType]; duplicate {
			return fmt.Errorf("extension: duplicate event subscription %q", eventType)
		}
		seenEvents[eventType] = struct{}{}
	}
	if m.Health.Enabled && !m.Capabilities.Health {
		return errors.New("extension: health.enabled requires the health capability")
	}
	if m.Lifecycle.Enabled && !m.Capabilities.Lifecycle {
		return errors.New("extension: lifecycle.enabled requires the lifecycle capability")
	}
	if m.Lifecycle.RestartPolicy == "" {
		m.Lifecycle.RestartPolicy = RestartNever
	}
	if !validRestartPolicy(m.Lifecycle.RestartPolicy) {
		return fmt.Errorf("extension: unsupported restart policy %q", m.Lifecycle.RestartPolicy)
	}
	seen := make(map[string]struct{}, len(m.Tools.Definitions))
	for _, tool := range m.Tools.Definitions {
		if !toolNamePattern.MatchString(tool.Name) {
			return fmt.Errorf("extension: invalid tool name %q", tool.Name)
		}
		if _, exists := seen[tool.Name]; exists {
			return fmt.Errorf("extension: duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		if !ValidToolRisk(tool.Risk) {
			return fmt.Errorf("extension: unsupported tool risk %q", tool.Risk)
		}
	}
	seenPermission := make(map[string]struct{}, len(m.Permissions))
	for _, permission := range m.Permissions {
		if strings.TrimSpace(permission.Name) == "" {
			return errors.New("extension: permission name is required")
		}
		if _, exists := seenPermission[permission.Name]; exists {
			return fmt.Errorf("extension: duplicate permission %q", permission.Name)
		}
		seenPermission[permission.Name] = struct{}{}
	}
	return nil
}

func validVersion(value string) bool {
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	_, _, err := VersionParts(value)
	return err == nil
}

func validRestartPolicy(policy RestartPolicy) bool {
	switch policy {
	case RestartNever, RestartOnFailure, RestartAlways:
		return true
	default:
		return false
	}
}

func ValidContextStrategy(strategy ContextStrategy) bool {
	switch strategy {
	case ContextOncePerTurn, ContextBeforeEveryModelCall, ContextOnUserInputChange, ContextAfterToolResult, ContextManual:
		return true
	default:
		return false
	}
}

func ValidToolRisk(risk ToolRisk) bool {
	switch risk {
	case ToolRiskRead, ToolRiskWrite, ToolRiskDestructive, ToolRiskExternalSideEffect, ToolRiskPrivileged:
		return true
	default:
		return false
	}
}

func validateEnvironment(entries []string) error {
	for _, entry := range entries {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.ContainsAny(parts[0], " \t\r\n") {
			return fmt.Errorf("extension: invalid transport environment entry %q", entry)
		}
		if sensitiveEnvironmentName(parts[0]) {
			return fmt.Errorf("extension: credential-shaped transport environment %q is not allowed", parts[0])
		}
	}
	return nil
}

func sensitiveEnvironmentName(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	return strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.Contains(name, "PASSWORD") ||
		strings.Contains(name, "CREDENTIAL") || strings.Contains(name, "API_KEY") || strings.Contains(name, "AUTH") ||
		strings.HasSuffix(name, "_KEY")
}

func VersionParts(value string) (major, minor int, err error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return 0, 0, fmt.Errorf("invalid version %q", value)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return 0, 0, fmt.Errorf("invalid major version %q", value)
	}
	if len(parts) > 1 {
		minor, err = strconv.Atoi(parts[1])
		if err != nil || minor < 0 {
			return 0, 0, fmt.Errorf("invalid minor version %q", value)
		}
	}
	return major, minor, nil
}

// CompatibleAgentAPI reports whether the Agent's API satisfies a manifest's
// required range. V1 is major-exact and minor-backward-compatible.
func CompatibleAgentAPI(agentAPI, requiredAPI string) bool {
	if strings.TrimSpace(requiredAPI) == "" {
		return true
	}
	agentMajor, agentMinor, err := VersionParts(agentAPI)
	if err != nil {
		return false
	}
	requiredMajor, requiredMinor, requiredErr := VersionParts(requiredAPI)
	if requiredErr != nil {
		return false
	}
	return agentMajor == requiredMajor && agentMinor >= requiredMinor
}
