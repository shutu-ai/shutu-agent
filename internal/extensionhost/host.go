package extensionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/loop"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

var (
	ErrManifestInvalid  = errors.New("extension: invalid manifest")
	ErrIncompatible     = errors.New("extension: incompatible")
	ErrConnectionClosed = errors.New("extension: connection closed")
	ErrRestartExhausted = errors.New("extension: restart budget exhausted")
)

type Source struct {
	ManifestPath string
	Required     bool
	Grants       []string
}

type Config struct {
	AgentName             string
	AgentVersion          string
	Workspace             string
	StartupTimeout        time.Duration
	HealthTimeout         time.Duration
	ContextTimeout        time.Duration
	ShutdownTimeout       time.Duration
	EventTimeout          time.Duration
	EventQueueSize        int
	GlobalContextChars    int
	MaxContributionChars  int
	GlobalContextTokens   int
	MaxContributionTokens int
	Sources               []Source
	Grants                map[string][]string
	AllowedTools          map[string]struct{}
	Registry              *tools.Registry
	Observer              Observer
	OnWebContributions    func([]WebContribution)
}

type Event struct {
	ExtensionID   string    `json:"extensionId"`
	Capability    string    `json:"capability"`
	Method        string    `json:"method,omitempty"`
	DurationMS    int64     `json:"durationMs,omitempty"`
	Success       bool      `json:"success"`
	Timeout       bool      `json:"timeout,omitempty"`
	Error         string    `json:"error,omitempty"`
	ContextChars  int       `json:"contextChars,omitempty"`
	ContextTokens int       `json:"contextTokens,omitempty"`
	ToolName      string    `json:"toolName,omitempty"`
	EventType     string    `json:"eventType,omitempty"`
	Restarts      int       `json:"restarts,omitempty"`
	HealthReady   bool      `json:"healthReady,omitempty"`
	Delivered     bool      `json:"delivered,omitempty"`
	Queued        bool      `json:"queued,omitempty"`
	Dropped       bool      `json:"dropped,omitempty"`
	QueueDepth    int       `json:"queueDepth,omitempty"`
	At            time.Time `json:"at"`
}

type Observer func(Event)

type managedExtension struct {
	manifest       extension.Manifest
	source         Source
	connection     connection
	initialized    extension.InitializeResult
	grants         map[string]struct{}
	tools          []extension.ToolDefinition
	publishedTools []string
	restarts       int
	ready          atomic.Bool
	webURL         string
	eventQueue     chan queuedEvent
	eventDone      chan struct{}
	eventStop      sync.Once
	subscriptions  map[string]struct{}
}

type Host struct {
	config         Config
	mu             sync.RWMutex
	items          []*managedExtension
	closed         bool
	contextStateMu sync.Mutex
	contextStates  map[string]*contextCadenceState
}

func New(config Config) *Host {
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = 10 * time.Second
	}
	if config.HealthTimeout <= 0 {
		config.HealthTimeout = 3 * time.Second
	}
	if config.ContextTimeout <= 0 {
		config.ContextTimeout = 5 * time.Second
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 3 * time.Second
	}
	if config.EventTimeout <= 0 {
		config.EventTimeout = 2 * time.Second
	}
	if config.EventQueueSize <= 0 {
		config.EventQueueSize = 256
	}
	if config.GlobalContextChars <= 0 {
		config.GlobalContextChars = 4000
	}
	if config.MaxContributionChars <= 0 {
		config.MaxContributionChars = 2000
	}
	if config.GlobalContextTokens <= 0 {
		config.GlobalContextTokens = 1000
	}
	if config.MaxContributionTokens <= 0 {
		config.MaxContributionTokens = 500
	}
	if config.AgentName == "" {
		config.AgentName = "shutu-agent"
	}
	return &Host{config: config, contextStates: make(map[string]*contextCadenceState)}
}

func Discover(explicit []Source, root string) ([]Source, error) {
	sources := append([]Source(nil), explicit...)
	seen := make(map[string]struct{}, len(explicit))
	for _, source := range explicit {
		seen[source.ManifestPath] = struct{}{}
	}
	if root == "" {
		return sources, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sources, nil
		}
		return nil, fmt.Errorf("extension: read discovery directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "extension.yaml")
		if _, statErr := os.Stat(path); statErr == nil {
			if _, duplicate := seen[path]; duplicate {
				continue
			}
			seen[path] = struct{}{}
			sources = append(sources, Source{ManifestPath: path})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ManifestPath < sources[j].ManifestPath })
	return sources, nil
}

func (h *Host) Start(ctx context.Context) error {
	for _, source := range h.config.Sources {
		if err := h.startOne(ctx, source); err != nil {
			if source.Required {
				return err
			}
			h.observe(Event{ExtensionID: filepath.Base(filepath.Dir(source.ManifestPath)), Capability: "lifecycle", Method: "start", Success: false, Error: err.Error(), At: time.Now().UTC()})
		}
	}
	return nil
}

func (h *Host) startOne(ctx context.Context, source Source) error {
	data, err := os.ReadFile(source.ManifestPath)
	if err != nil {
		return fmt.Errorf("extension: read manifest: %w", err)
	}
	manifest, err := extension.ParseManifest(data)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrManifestInvalid, source.ManifestPath, err)
	}
	if !extension.CompatibleAgentAPI(extension.APIVersion, manifest.RequiredAgentAPI) {
		return fmt.Errorf("%w: %s requires agent API %s, host supports %s", ErrIncompatible, manifest.ID, manifest.RequiredAgentAPI, extension.APIVersion)
	}
	grantsList := append([]string(nil), source.Grants...)
	grantsList = append(grantsList, h.config.Grants[manifest.ID]...)
	grants, err := resolveGrants(manifest, grantsList)
	if err != nil {
		return err
	}
	conn, err := newConnection(manifest.Transport)
	if err != nil {
		return err
	}
	item, err := h.initialize(ctx, manifest, source, conn, grants)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := h.publishTools(item); err != nil {
		_ = h.removeTools(item)
		_ = conn.Close()
		return err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = h.removeTools(item)
		_ = conn.Close()
		return ErrConnectionClosed
	}
	h.items = append(h.items, item)
	h.mu.Unlock()
	h.notifyWebContributions()
	h.startEventDelivery(item)
	h.observe(Event{ExtensionID: manifest.ID, Capability: "lifecycle", Method: "start", Success: true, HealthReady: true, At: time.Now().UTC()})
	h.publishLifecycle(item, extension.EventExtensionStarted, nil)
	return nil
}

func (h *Host) notifyWebContributions() {
	if h.config.OnWebContributions == nil {
		return
	}
	callback := h.config.OnWebContributions
	defer func() { _ = recover() }()
	callback(h.WebContributions())
}

func resolveGrants(manifest extension.Manifest, configured []string) (map[string]struct{}, error) {
	configuredSet := make(map[string]struct{}, len(configured))
	for _, name := range configured {
		configuredSet[strings.TrimSpace(name)] = struct{}{}
	}
	granted := make(map[string]struct{})
	for _, permission := range manifest.Permissions {
		if _, ok := configuredSet[permission.Name]; ok {
			granted[permission.Name] = struct{}{}
			continue
		}
		if permission.Required {
			return nil, fmt.Errorf("extension: %s requires permission %q which was not granted", manifest.ID, permission.Name)
		}
	}
	return granted, nil
}

func (h *Host) initialize(ctx context.Context, manifest extension.Manifest, source Source, conn connection, grants map[string]struct{}) (*managedExtension, error) {
	startupCtx, cancel := context.WithTimeout(ctx, h.config.StartupTimeout)
	defer cancel()
	grantedNames := make([]string, 0, len(grants))
	for name := range grants {
		grantedNames = append(grantedNames, name)
	}
	sort.Strings(grantedNames)
	var result extension.InitializeResult
	request := extension.InitializeRequest{
		ProtocolVersion:       extension.ProtocolVersion,
		AgentAPIVersion:       extension.APIVersion,
		AgentName:             h.config.AgentName,
		AgentVersion:          h.config.AgentVersion,
		GrantedPermissions:    grantedNames,
		SupportedCapabilities: extension.Capabilities{Tools: true, ContextProvider: true, Lifecycle: true, Web: true, Health: true, Events: true},
		SupportedEventTypes:   extension.SupportedEventTypes,
	}
	if err := conn.Call(startupCtx, extension.MethodInitialize, request, &result); err != nil {
		return nil, fmt.Errorf("extension: initialize %s: %w", manifest.ID, err)
	}
	if result.ProtocolVersion != extension.ProtocolVersion {
		return nil, fmt.Errorf("%w: %s returned protocol %q, host supports %q", ErrIncompatible, manifest.ID, result.ProtocolVersion, extension.ProtocolVersion)
	}
	major, _, err := extension.VersionParts(result.ExtensionAPIVersion)
	if err != nil || major != 1 {
		return nil, fmt.Errorf("%w: %s returned invalid extension API %q", ErrIncompatible, manifest.ID, result.ExtensionAPIVersion)
	}
	if (manifest.Capabilities.Tools && !result.Capabilities.Tools) ||
		(manifest.Capabilities.ContextProvider && !result.Capabilities.ContextProvider) ||
		(manifest.Capabilities.Web && !result.Capabilities.Web) ||
		(manifest.Capabilities.Health && !result.Capabilities.Health) ||
		(manifest.Capabilities.Lifecycle && !result.Capabilities.Lifecycle) ||
		(len(manifest.Events.Subscribe) > 0 && !result.Capabilities.Events) {
		return nil, fmt.Errorf("%w: %s omitted a capability declared by its manifest", ErrIncompatible, manifest.ID)
	}
	result.Capabilities.Tools = result.Capabilities.Tools && manifest.Capabilities.Tools
	result.Capabilities.ContextProvider = result.Capabilities.ContextProvider && manifest.Capabilities.ContextProvider
	result.Capabilities.Web = result.Capabilities.Web && manifest.Capabilities.Web
	result.Capabilities.Health = result.Capabilities.Health && manifest.Capabilities.Health
	result.Capabilities.Lifecycle = result.Capabilities.Lifecycle && manifest.Capabilities.Lifecycle
	result.Capabilities.Events = result.Capabilities.Events && manifest.Capabilities.Events

	item := &managedExtension{manifest: manifest, source: source, connection: conn, initialized: result, grants: grants}
	if result.Capabilities.Events {
		item.subscriptions = make(map[string]struct{}, len(manifest.Events.Subscribe))
		for _, eventType := range manifest.Events.Subscribe {
			item.subscriptions[strings.TrimSpace(eventType)] = struct{}{}
		}
	}
	item.ready.Store(true)
	if manifest.Capabilities.Health {
		healthCtx, healthCancel := context.WithTimeout(ctx, timeout(time.Duration(manifest.Health.TimeoutMS)*time.Millisecond, h.config.HealthTimeout))
		defer healthCancel()
		var health extension.HealthResult
		callErr := conn.Call(healthCtx, extension.MethodHealth, nil, &health)
		if callErr != nil || !health.Ready {
			message := "extension is not ready"
			if callErr != nil {
				message = callErr.Error()
			} else if health.Detail != "" {
				message = health.Detail
			} else if health.Status != "" {
				message = health.Status
			}
			return nil, fmt.Errorf("extension: health %s: %s", manifest.ID, message)
		}
	}
	if result.Capabilities.Web {
		webURL := strings.TrimSpace(result.WebBaseURL)
		if webURL == "" {
			webURL = strings.TrimSpace(manifest.Web.ServiceURL)
		}
		parsed, parseErr := url.Parse(webURL)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return nil, fmt.Errorf("extension: invalid web service URL for %s", manifest.ID)
		}
		item.webURL = webURL
	}
	return item, nil
}

func (h *Host) publishTools(item *managedExtension) error {
	if h.config.Registry == nil || !item.initialized.Capabilities.Tools {
		return nil
	}
	definitions := append([]extension.ToolDefinition(nil), item.manifest.Tools.Definitions...)
	if len(item.initialized.Tools) > 0 {
		definitions = append([]extension.ToolDefinition(nil), item.initialized.Tools...)
	}
	seen := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if strings.TrimSpace(definition.Name) == "" {
			return fmt.Errorf("%w: %s returned a tool without a name", ErrManifestInvalid, item.manifest.ID)
		}
		if !toolNameAllowed(definition.Name) {
			return fmt.Errorf("%w: %s returned invalid tool name %q", ErrManifestInvalid, item.manifest.ID, definition.Name)
		}
		if !extension.ValidToolRisk(definition.Risk) {
			return fmt.Errorf("%w: %s returned invalid tool risk %q", ErrManifestInvalid, item.manifest.ID, definition.Risk)
		}
		public := PublicToolName(item.manifest.ID, definition.Name)
		if _, duplicate := seen[public]; duplicate {
			return fmt.Errorf("%w: %s returned duplicate tool %q", ErrManifestInvalid, item.manifest.ID, definition.Name)
		}
		seen[public] = struct{}{}
		if err := h.config.Registry.RegisterWithInfo(extensionTool{host: h, item: item, definition: definition, publicName: public}, tools.RegistrationInfo{Owner: "extension:" + item.manifest.ID, Plugin: item.manifest.ID, Provenance: "extension"}); err != nil {
			return fmt.Errorf("extension: register tool %s: %w", public, err)
		}
		if definition.Risk == extension.ToolRiskRead && !definition.RequiresApproval {
			h.config.Registry.Allow(public)
		} else if _, explicitlyAllowed := h.config.AllowedTools[public]; explicitlyAllowed {
			h.config.Registry.Allow(public)
		}
		item.tools = append(item.tools, definition)
		item.publishedTools = append(item.publishedTools, public)
	}
	return nil
}

func (h *Host) removeTools(item *managedExtension) error {
	if h.config.Registry == nil {
		return nil
	}
	var first error
	for _, name := range item.publishedTools {
		if err := h.config.Registry.Unregister(name); err != nil && first == nil {
			first = err
		}
	}
	item.publishedTools = nil
	return first
}

func PublicToolName(extensionID, toolName string) string {
	return "ext__" + strings.ToLower(extensionID) + "__" + toolName
}

func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	items := append([]*managedExtension(nil), h.items...)
	h.items = nil
	h.mu.Unlock()
	var first error
	for _, item := range items {
		h.publishLifecycle(item, extension.EventExtensionStopped, map[string]any{"reason": "shutdown"})
		h.stopEventDelivery(item)
		ctx, cancel := context.WithTimeout(context.Background(), h.config.ShutdownTimeout)
		_ = item.connection.Call(ctx, extension.MethodShutdown, nil, nil)
		cancel()
		if err := item.connection.Close(); err != nil && first == nil {
			first = err
		}
		if err := h.removeTools(item); err != nil && first == nil {
			first = err
		}
		item.ready.Store(false)
	}
	h.notifyWebContributions()
	return first
}

func (h *Host) Snapshot() []extension.Manifest {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]extension.Manifest, 0, len(h.items))
	for _, item := range h.items {
		out = append(out, item.manifest)
	}
	return out
}

func (h *Host) SensitiveTools() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var names []string
	for _, item := range h.items {
		for _, tool := range item.tools {
			if tool.Risk != extension.ToolRiskRead || tool.RequiresApproval {
				names = append(names, PublicToolName(item.manifest.ID, tool.Name))
			}
		}
	}
	return names
}

type WebContribution struct {
	ExtensionID       string
	Title             string
	Route             string
	Icon              string
	NavigationEnabled bool
	NavigationGroup   string
	Order             int
	ServiceURL        string
	Ready             bool
}

func (h *Host) WebContributions() []WebContribution {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []WebContribution
	for _, item := range h.items {
		if !item.initialized.Capabilities.Web || item.webURL == "" || !webNavigationEnabled(item.manifest.Web) {
			continue
		}
		out = append(out, WebContribution{
			ExtensionID: item.manifest.ID, Title: item.manifest.Web.Title, Route: publicWebRoute(item.manifest),
			Icon: item.manifest.Web.Icon, NavigationEnabled: webNavigationEnabled(item.manifest.Web),
			NavigationGroup: webNavigationGroup(item.manifest.Web), Order: item.manifest.Web.Order,
			ServiceURL: item.webURL, Ready: item.ready.Load(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NavigationGroup != out[j].NavigationGroup {
			return out[i].NavigationGroup < out[j].NavigationGroup
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].ExtensionID < out[j].ExtensionID
	})
	return out
}

func webNavigationEnabled(contribution extension.WebContribution) bool {
	return contribution.NavigationEnabled == nil || *contribution.NavigationEnabled
}

func webNavigationGroup(contribution extension.WebContribution) string {
	if strings.TrimSpace(contribution.NavigationGroup) == "" {
		return "Extensions"
	}
	return strings.TrimSpace(contribution.NavigationGroup)
}

func publicWebRoute(manifest extension.Manifest) string {
	return "/extensions/" + strings.ToLower(manifest.ID) + "/"
}

func (h *Host) observe(event Event) {
	if h.config.Observer == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Error = llm.RedactDiagnostic(event.Error)
	if runes := []rune(event.Error); len(runes) > 300 {
		event.Error = string(runes[:300])
	}
	observer := h.config.Observer
	defer func() { _ = recover() }()
	observer(event)
}

type contextCadenceState struct {
	lastTurn   string
	lastInput  string
	lastStepID string
}

func (h *Host) ContextInjector() loop.PreStepInjector {
	return loop.PreStepInjector{
		Name: "extensions",
		InjectWithError: func(ctx context.Context, userText string) ([]llm.Message, error) {
			state := h.contextStateFor(ctx)
			contributions, err := h.ProvideContext(ctx, userText, state)
			if err != nil {
				return nil, err
			}
			messages := make([]llm.Message, 0, len(contributions))
			for _, contribution := range contributions {
				messages = append(messages, llm.Message{
					Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(contribution.Content)},
					SourceKind: "extension", SourcePlugin: contribution.Source,
				})
			}
			return messages, nil
		},
		Deduplicate: true,
	}
}

func (h *Host) contextStateFor(ctx context.Context) *contextCadenceState {
	correlation, _ := runtimectx.CorrelationOf(ctx)
	if correlation.SessionID == "" {
		return &contextCadenceState{}
	}
	h.contextStateMu.Lock()
	defer h.contextStateMu.Unlock()
	state := h.contextStates[correlation.SessionID]
	if state == nil {
		state = &contextCadenceState{}
		h.contextStates[correlation.SessionID] = state
	}
	return state
}

// RefreshContext is the generic manual trigger. It bypasses cadence only for
// providers that explicitly declare the manual strategy; other strategies
// retain their normal deduplication semantics.
func (h *Host) RefreshContext(ctx context.Context, userText string) ([]extension.ContextContribution, error) {
	state := h.contextStateFor(ctx)
	return h.provideContext(ctx, userText, state, true)
}

func (h *Host) ProvideContext(ctx context.Context, userText string, state *contextCadenceState) ([]extension.ContextContribution, error) {
	return h.provideContext(ctx, userText, state, false)
}

func (h *Host) provideContext(ctx context.Context, userText string, state *contextCadenceState, includeManual bool) ([]extension.ContextContribution, error) {
	if state == nil {
		state = h.contextStateFor(ctx)
	}
	h.mu.RLock()
	items := append([]*managedExtension(nil), h.items...)
	h.mu.RUnlock()
	correlation, _ := runtimectx.CorrelationOf(ctx)
	inputIdentity := normalizedInput(userText)
	h.publishContextRequested(ctx, userText)
	var all []extension.ContextContribution
	var requiredErr error
	for _, item := range items {
		if !item.initialized.Capabilities.ContextProvider || !item.manifest.ContextProvider.Enabled {
			continue
		}
		strategy := item.manifest.ContextProvider.Strategy
		if strategy == extension.ContextManual && !includeManual {
			continue
		}
		if strategy == extension.ContextOncePerTurn && correlation.TurnID != "" && state.lastTurn == correlation.TurnID {
			continue
		}
		if strategy == extension.ContextOnUserInputChange && state.lastInput == inputIdentity {
			continue
		}
		sameTurn := correlation.TurnID != "" && state.lastTurn == correlation.TurnID
		if strategy == extension.ContextAfterToolResult && (!sameTurn || state.lastStepID == "" || correlation.StepID == state.lastStepID) {
			continue
		}
		request := extension.ContextRequest{Metadata: map[string]string{}}
		if granted(item, "session.id") {
			request.SessionID = correlation.SessionID
		}
		if granted(item, "session.turn") {
			request.TurnID = correlation.TurnID
		}
		if granted(item, "session.step") {
			request.StepID = correlation.StepID
		}
		if granted(item, "workspace.path") {
			request.Workspace = h.config.Workspace
		}
		if granted(item, "user.input") {
			request.UserInput = userText
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout(time.Duration(item.manifest.ContextProvider.TimeoutMS)*time.Millisecond, h.config.ContextTimeout))
		started := time.Now()
		var result extension.ContextResult
		err := item.connection.Call(callCtx, extension.MethodProvideContext, request, &result)
		cancel()
		event := Event{ExtensionID: item.manifest.ID, Capability: "context", Method: extension.MethodProvideContext, DurationMS: time.Since(started).Milliseconds(), At: started.UTC()}
		if err != nil {
			event.Error = err.Error()
			event.Timeout = errors.Is(err, context.DeadlineExceeded)
			h.observe(event)
			h.recoverAfterCall(item.manifest.ID, err)
			if item.manifest.ContextProvider.Required {
				requiredErr = err
				break
			}
			continue
		}
		bounded := make([]extension.ContextContribution, 0, len(result.Contributions))
		for _, contribution := range result.Contributions {
			limit := h.config.MaxContributionChars
			if item.manifest.ContextProvider.MaxChars > 0 && item.manifest.ContextProvider.MaxChars < limit {
				limit = item.manifest.ContextProvider.MaxChars
			}
			contribution.Content = fitContribution(contribution, h.config.MaxContributionTokens, limit)
			contribution.Source = strings.TrimSpace(contribution.Source)
			if contribution.Source == "" {
				contribution.Source = item.manifest.ID
			}
			if contribution.Content != "" {
				bounded = append(bounded, contribution)
			}
		}
		event.Success = true
		for _, contribution := range bounded {
			event.ContextChars += len(contribution.Content)
			event.ContextTokens += estimateTokens(contribution)
		}
		h.observe(event)
		all = append(all, bounded...)
	}
	if state != nil && !includeManual {
		state.lastTurn = correlation.TurnID
		state.lastInput = inputIdentity
		state.lastStepID = correlation.StepID
	}
	if requiredErr != nil {
		return nil, fmt.Errorf("extension: required context provider failed: %w", requiredErr)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Priority != all[j].Priority {
			return all[i].Priority > all[j].Priority
		}
		return all[i].Source < all[j].Source
	})
	all = deduplicateContributions(all)
	all = truncateContributions(all, h.config.GlobalContextChars, h.config.GlobalContextTokens)
	h.publishContextInjected(ctx, all)
	return all, nil
}

func granted(item *managedExtension, permission string) bool {
	_, ok := item.grants[permission]
	return ok
}

func deduplicateContributions(input []extension.ContextContribution) []extension.ContextContribution {
	seen := make(map[string]struct{}, len(input))
	out := make([]extension.ContextContribution, 0, len(input))
	for _, contribution := range input {
		key := contribution.Source + "\x00" + contribution.Content
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, contribution)
	}
	return out
}

func estimateTokens(contribution extension.ContextContribution) int {
	estimate := contribution.EstimatedTokens
	if byteEstimate := (len(contribution.Content) + 3) / 4; byteEstimate > estimate {
		estimate = byteEstimate
	}
	if runeEstimate := (utf8.RuneCountInString(contribution.Content) + 1) / 2; runeEstimate > estimate {
		estimate = runeEstimate
	}
	return estimate
}

func fitContribution(contribution extension.ContextContribution, maxTokens, maxChars int) string {
	estimate := estimateTokens(contribution)
	if estimate <= maxTokens && len(contribution.Content) <= maxChars {
		return contribution.Content
	}
	if !contribution.Truncatable {
		return ""
	}
	limit := maxChars
	if tokenChars := maxTokens * 4; tokenChars < limit {
		limit = tokenChars
	}
	content := truncateUTF8(contribution.Content, limit, true)
	for estimate > maxTokens && len(content) > 0 {
		target := len(content) * maxTokens / estimate
		if target >= len(content) {
			target = len(content) - 1
		}
		content = truncateFixed(content, target)
		estimate = estimateTokens(extension.ContextContribution{Content: content, EstimatedTokens: contribution.EstimatedTokens})
	}
	return content
}

func truncateContributions(input []extension.ContextContribution, charBudget, tokenBudget int) []extension.ContextContribution {
	remainingChars, remainingTokens := charBudget, tokenBudget
	for i := range input {
		if remainingChars <= 0 || remainingTokens <= 0 {
			input[i].Content = ""
			continue
		}
		estimate := estimateTokens(input[i])
		if estimate > remainingTokens {
			if !input[i].Truncatable {
				input[i].Content = ""
				continue
			}
			target := len(input[i].Content) * remainingTokens / estimate
			input[i].Content = truncateFixed(input[i].Content, target)
			estimate = estimateTokens(input[i])
			if estimate > remainingTokens {
				input[i].Content = ""
				continue
			}
		}
		input[i].Content = truncateFixed(input[i].Content, remainingChars)
		if estimateTokens(input[i]) > remainingTokens {
			input[i].Content = ""
			continue
		}
		remainingChars -= len(input[i].Content)
		remainingTokens -= estimateTokens(input[i])
	}
	out := make([]extension.ContextContribution, 0, len(input))
	for _, contribution := range input {
		if contribution.Content != "" {
			out = append(out, contribution)
		}
	}
	return out
}

func truncateFixed(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8Boundary(value[cut]) {
		cut--
	}
	return value[:cut]
}

func truncateUTF8(value string, limit int, truncatable bool) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if !truncatable {
		return ""
	}
	const notice = "\n[extension context truncated by agent budget]"
	limit -= len(notice)
	if limit <= 0 {
		return ""
	}
	cut := limit
	for cut > 0 && !utf8Boundary(value[cut]) {
		cut--
	}
	return value[:cut] + notice
}

func utf8Boundary(char byte) bool {
	return char < 0x80 || char&0xC0 != 0x80
}

func toolNameAllowed(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for index, char := range name {
		alphanumeric := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
		if index == 0 && !alphanumeric {
			return false
		}
		if !alphanumeric && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

type extensionTool struct {
	host       *Host
	item       *managedExtension
	definition extension.ToolDefinition
	publicName string
}

func (t extensionTool) Name() string                 { return t.publicName }
func (t extensionTool) Description() string          { return t.definition.Description }
func (t extensionTool) Schema() map[string]any       { return cloneSchema(t.definition.InputSchema) }
func (t extensionTool) OutputSchema() map[string]any { return cloneSchema(t.definition.OutputSchema) }

func (t extensionTool) Execute(ctx context.Context, args any) (string, error) {
	arguments, ok := args.(map[string]any)
	if !ok {
		return "", fmt.Errorf("extension tool %s requires an object argument", t.publicName)
	}
	correlation, _ := runtimectx.CorrelationOf(ctx)
	request := extension.ToolCallRequest{Name: t.definition.Name, Arguments: arguments, CallID: tools.CallIDFromContext(ctx)}
	if granted(t.item, "session.id") {
		request.SessionID = correlation.SessionID
	}
	started := time.Now()
	var result extension.ToolCallResult
	err := t.item.connection.Call(ctx, extension.MethodCallTool, request, &result)
	event := Event{ExtensionID: t.item.manifest.ID, Capability: "tool", Method: extension.MethodCallTool, ToolName: t.publicName, DurationMS: time.Since(started).Milliseconds(), At: started.UTC()}
	if err != nil {
		event.Error = err.Error()
		event.Timeout = errors.Is(err, context.DeadlineExceeded)
		t.host.observe(event)
		t.host.recoverAfterCall(t.item.manifest.ID, err)
		return "", err
	}
	event.Success = result.Error == ""
	event.Error = result.Error
	t.host.observe(event)
	if result.Error != "" {
		return "", errors.New(result.Error)
	}
	if text, ok := result.Value.(string); ok {
		return text, nil
	}
	encoded, err := marshalJSON(result.Value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func cloneSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{}
	}
	encoded, err := marshalJSON(schema)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if json.Unmarshal(encoded, &out) != nil {
		return map[string]any{}
	}
	return out
}
