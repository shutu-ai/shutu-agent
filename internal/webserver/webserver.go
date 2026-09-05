// webserver.go — the M10 unified web portal (ADR 2026-08-20-m10-web-portal.md
// D-WEB-1~7): a single net/http server carrying the dsh-style session/event
// (M10b). Data views are read-only (D-WEB-4); injected workspace actions
// (messages, session controls and approval decisions) are explicit exceptions.
// Every API route sits behind the bearer-token middleware; the frontend is vanilla
// React/Cordis dist is served from the configured filesystem directory.
package webserver

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/crashboundary"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/meter"
	"github.com/shutu-ai/shutu-agent/internal/profile"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

// maxSummary is the rune cap on the bounded per-event summary the events API
// exposes (防超大载荷 / 防泄露完整日志正文, D-WEB-4). Message bodies
// (user/message, assistant/message) are the text the frontend must display in
// full — dsh renders assistant markdown whole, so they are NOT truncated; the
// cap applies to tool outputs, reasoning, snippets and injected text.
const maxSummary = 200

// PromptMeta carries canonical DSH user-rpc provenance into the composition
// root. The native client sends ClientTimeZone; legacy REST callers leave it
// empty, and time-context then reports the browser zone unavailable.
type PromptMeta struct {
	RPCID          string `json:"rpcId,omitempty"`
	ClientTimeZone string `json:"clientTimeZone,omitempty"`
}

// Server is the M10 web portal: a net/http server over the read-only session
// store. Authentication is optional (D-WEB-2 change, user decision 2026-08-20):
// when token == "" every API route is open to the local machine (the
// 127.0.0.1 bind is the trust boundary, like dsh web); when a token is set the
// bearer middleware guards every /api route.
type Server struct {
	store     store.Store
	tokenHash [32]byte // sha256 of the configured token; the plaintext never survives New
	authOn    bool     // token != "" → bearer check enforced
	addr      string
	// frontendDir is the React/Cordis SPA dist root. The server never serves an
	// embedded or legacy frontend when this path is absent.
	frontendDir string
	// defaultWorkdir is the fallback cwd for ungrouped sessions and legacy
	// title-only workspaces. The composition root sets it from config; an empty
	// value falls back to the server process cwd.
	defaultWorkdir  string
	srv             *http.Server
	coreHandler     http.Handler
	extensionMu     sync.RWMutex
	extensionRoutes []ExtensionRoute
	closeMu         sync.Mutex
	closeDone       chan struct{}
	closed          bool

	// nativeSettings is the small settings document owned by the DSH native
	// adapter. It carries the onboarding acknowledgement and the agent-preset
	// default; the latter is mirrored into the persistent settings table by the
	// native settings bridge without coupling the native API to the legacy REST
	// settings surface.
	nativeSettingsMu            sync.Mutex
	nativeSettings              map[string]nativeSettingsDocument
	nativeSettingsLoaded        map[string]bool
	nativeSettingsSubscribersMu sync.Mutex
	nativeSettingsSubscribers   map[uint64]func(string, int)
	nativeSettingsSubscriberID  uint64
	// Canonical owner-event fanout for the configuration plane. These carry
	// references and topology facts only; credential values never enter them.
	nativeCredentialSubscribersMu sync.Mutex
	nativeCredentialSubscribers   map[uint64]func(string)
	nativeCredentialSubscriberID  uint64
	nativeLLMAdapterSubscribersMu sync.Mutex
	nativeLLMAdapterSubscribers   map[uint64]func()
	nativeLLMAdapterSubscriberID  uint64

	// M10 W1 interactive wiring (ADR D-WEB2-A/B/C): the optional handlers the
	// composition root injects after New. All three are nil until a Setter is
	// called; a nil handler makes its API answer 501.
	msgFn                      func(ctx context.Context, sessionID, text string, images []llm.ImageRef, meta PromptMeta) error
	sessFn                     func(ctx context.Context, action, id string) (string, error)
	nativeSessionCreateFn      func(ctx context.Context, spec NativeSessionCreateSpec) (NativeSessionCreateResult, error)
	nativeSessionCreateMu      sync.Mutex
	nativeSessionCreateFlights map[string]*nativeSessionCreateFlight
	evSrc                      func(sessionID string, sink func(session.Event)) func()
	queueListFn                func(ctx context.Context, sessionID string) ([]QueueItem, error)
	queueEnqueueFn             func(ctx context.Context, sessionID, text string, content []llm.ContentBlock, meta PromptMeta) (QueueItem, error)
	queueUpdateFn              func(ctx context.Context, sessionID, itemID, action string) error
	// nativeMuxSubscribers receives authoritative queue/job refreshes for one
	// subscribed session. The map is transport-only; providers remain owned by
	// the composition root.
	nativeMuxMu           sync.Mutex
	nativeMuxSubscribers  map[string]map[uint64]func()
	nativeMuxSessionAdded map[uint64]func(string)
	nativeMuxSubscriberID uint64
	// Hijacked native WebSocket connections are outside net/http's Shutdown
	// accounting. Track them explicitly so Close cannot leave a poller holding
	// the SQLite store (or a goroutine) after the HTTP listener is gone.
	nativeConnMu  sync.Mutex
	nativeConns   map[io.Closer]struct{}
	nativeClosing bool
	nativeDone    sync.WaitGroup
	// DSH serializes workspace create/rename/delete on one mutation chain so
	// concurrent path/title operations cannot publish duplicate or stale state.
	workspaceMutationMu sync.Mutex
	// nativeQueueUpdateFn accepts the DSH action vocabulary. The legacy queue
	// callback above intentionally remains text/action-only for the REST API;
	// this seam carries the native edit payload without weakening that API.
	nativeQueueUpdateFn          func(ctx context.Context, sessionID, itemID, action, text string) error
	nativeSubagentPromptFn       func(ctx context.Context, childSessionID string, content []llm.ContentBlock, meta PromptMeta) error
	nativeSubagentInterruptFn    func(childSessionID, reason string) error
	nativeGoalMutationFn         func(ctx context.Context, mutation NativeGoalMutation) (NativeGoalMutationResult, error)
	nativeCredentialSetFn        func(ctx context.Context, ref, value string) error
	nativeCredentialUnsetFn      func(ctx context.Context, ref string) error
	nativeSettingsOpenDocumentFn func(ctx context.Context) error
	nativeSettingsAppliedFn      func(ctx context.Context, namespace string, resolved map[string]any) error
	nativeAgentPresetManager     NativeAgentPresetManager
	nativeCommandManager         NativeCommandManager
	profileRegistry              *profile.Registry
	crashContracts               *crashboundary.Registry

	// statusFn is the dsh-session-status alignment: it computes the live state
	// (warning/ongoing/done/idle + labels + running-subagent count) for one
	// session row, so the sidebar renders the status dot and the hover card
	// without the webserver knowing any runtime state. nil (the default) leaves
	// every row's status empty.
	statusFn func(ctx context.Context, m store.SessionMeta) SessionStatus
	// liveAgentCountFn reports the number of Agents currently published by the
	// runtime registry. host.describe must count live Agent handles, not every
	// durable session row that happens to have a computed status.
	liveAgentCountFn func() int

	// cfgFn is the M10 W2 config provider (ADR D-WEB2-D): it returns the
	// sanitized configuration view for GET /api/config. The redaction itself is
	// the composition root's job (cmd/sta's webConfig never exposes web_server.
	// token or any key); the webserver only forwards the provider's map. nil
	// (the default) makes the API answer 501.
	cfgFn func() map[string]any

	// contextWindowFn resolves the effective model's context window (used by
	// GET /api/sessions/{id}/context for the ContextMeter). It takes the session
	// id (for the per-session model override) and returns a token budget; 0
	// means the server falls back to its default.
	contextWindowFn func(sessionID string) int
	// contextMeterFn is the same replay-aware meter used by compaction and
	// runtime telemetry. Keeping it injectable avoids a second Web-only token
	// heuristic that can disagree with replacement folding and provider usage.
	contextMeterFn func(sessionID string, events []session.Event) (meter.Measurement, error)

	// stateFn returns the durable per-session state projection (plan mode,
	// goals/plans and memory summary). The webserver only transports the
	// projection; the composition root owns replay and capability policy.
	stateFn func(ctx context.Context, sessionID string) (map[string]any, error)

	// stopFn cancels a running turn for a session (POST
	// /api/sessions/{id}/stop, dsh 停止按钮). nil makes the API answer 501.
	stopFn func(sessionID string) error

	// M10 W4 (ADR D-WEB2-H): optional read-only providers for the subagent and
	// background-job panels (GET /api/subagents, GET /api/jobs). Both are nil
	// until a Setter is called; a nil provider makes its API answer 501. Each
	// returns sanitized view maps (id/status/timestamps only — no prompts,
	// outputs or session content).
	subFn  func(ctx context.Context, sessionID string) ([]map[string]any, error)
	jobsFn func(ctx context.Context, sessionID string) ([]map[string]any, error)

	// P5 (ADR D-WEB2-I): the image-attachment store wired by the composition
	// root when multimodal is enabled. nil (the default) makes the attachment
	// APIs answer 501 and message bodies with images answer 400.
	att                     *attachment.Store
	nativeImageCapabilityFn func(context.Context, string) bool

	// P5.1 (模型选择实时生效, 用户 2026-08-20 拍板): the live model-switch
	// dispatcher for POST /api/config/model. It validates the provider/model/
	// reasoning-effort, rebuilds the selected LLM provider and answers the new
	// config state. nil (the default) makes the API answer 501.
	setModelFn func(ctx context.Context, provider, model, effort string) error
	// sessionModelValidateFn applies the same provider/route admission used by
	// native, ACP, SDK and CLI turn assembly before a durable session override
	// is accepted.
	sessionModelValidateFn func(ctx context.Context, sessionID, provider, model, effort string) error
	// nativeDefaultModelFn receives accepted native DSH model selections so the
	// composition root can persist the shared Agent default for later sessions.
	nativeDefaultModelFn func(ctx context.Context, provider, model, effort string)
	// nativeSessionRenameFn commits an accepted title through the composition
	// root's live session log and returns the durable event sequence. A nil
	// handler keeps the storage-only compatibility path for embedders that do
	// not own a live Agent runtime.
	nativeSessionRenameFn func(ctx context.Context, sessionID, title string) (int64, error)

	// M11 (增加提供方 / 增加自定义提供方, dsh-synced): the provider-management
	// dispatcher. setProviderFn handles POST /api/config/provider (save a
	// built-in key override or a custom provider profile, api_key empty removes
	// the override) and DELETE /api/config/provider (remove a custom provider).
	// nil (the default) makes those APIs answer 501.
	setProviderFn func(ctx context.Context, action string, edit ProviderEdit) error

	// M11-pi-ai (模型探测, dsh discovery 对齐): the model-discovery dispatcher
	// for POST /api/config/provider/discover. It asks an endpoint which models
	// it serves (获取可用模型). nil (the default) makes the API answer 501.
	setDiscoverFn func(ctx context.Context, request ProviderDiscover) ([]ProviderModel, error)
	mcpRefreshFn  func(ctx context.Context) ([]map[string]any, error)
	mcpManageFn   func(ctx context.Context, action string, edit MCPServerEdit) ([]map[string]any, error)

	// 技能设置页 (dsh-skill-mcp-panel 对齐): the skill-management dispatcher for
	// /api/config/skills. It lists every skill with scope/rel/disabled state,
	// reads full content, hot-enables/disables, deletes, adds, migrates between
	// the two scopes and persists display groups. nil (the default) makes every
	// /api/config/skills route answer 501.
	skillsFn func(ctx context.Context, action string, req SkillRequest) (map[string]any, error)
	// skillCatalogFn is the native DSH read-only view addressed by one
	// session's durable cwd. It is deliberately separate from skillsFn, which
	// owns the settings-page management surface and includes disabled/global
	// rows not exposed to the composer.
	skillCatalogFn func(ctx context.Context, cwd string) ([]SkillCatalogEntry, error)

	// interactionFn/resolverFn expose the live DSH-style approval surface. The
	// engine remains owned by cmd/sta; this server only transports request views
	// and a resolve command.
	interactionFn               func(ctx context.Context, sessionID string) ([]interact.Request, error)
	resolveInteractionFn        func(ctx context.Context, sessionID, id string, status interact.ApprovalStatus, answer string) error
	resolveInteractionSessionFn func(ctx context.Context, id string) (string, error)
	// DSH question cancellation responses intentionally carry no sessionId.
	// The native mux records the owning session when it publishes a pending
	// interaction so /api/respond can resolve a later cancellation.
	nativeInteractionMu       sync.RWMutex
	nativeInteractionSessions map[string]string
}

// ProviderEdit is the M11 provider-management payload shared by POST and DELETE
// /api/config/provider: id plus the optional custom-provider profile fields and
// an API-key override. For a built-in provider only api_key is honored (the
// profile fields stay config-driven); for a custom provider the profile is
// persisted with it. Protocol is the custom provider's wire protocol (M11-pi-ai
// 四协议); empty defaults to openai-completions. Models is the multi-model list
// (M11-pi-ai ModelListEditor 对齐); an empty list falls back to the single
// Model field.
type ProviderEdit struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	BaseURL  string          `json:"base_url"`
	Model    string          `json:"model"`
	APIKey   string          `json:"api_key"`
	Protocol string          `json:"protocol"`
	Models   []ProviderModel `json:"models"`
	Custom   bool            `json:"custom"`
}

// ProviderModel is one custom-provider model row (id + optional name /
// capacities), mirroring customModel on the composition side.
type ProviderModel struct {
	ID                     string             `json:"id"`
	Name                   string             `json:"name,omitempty"`
	Input                  []string           `json:"input,omitempty"`
	ReasoningEfforts       map[string]*string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string             `json:"default_reasoning_effort,omitempty"`
	DefaultMaxTokens       int                `json:"default_max_tokens,omitempty"`
	ContextWindow          int                `json:"context_window,omitempty"`
	MaxTokens              int                `json:"max_tokens,omitempty"`
	Reasoning              *bool              `json:"reasoning,omitempty"`
	Tools                  *bool              `json:"tools,omitempty"`
	Vision                 *bool              `json:"vision,omitempty"`
	Audio                  *bool              `json:"audio,omitempty"`
}

// MCPServerEdit is the sanitized Web settings payload for one stdio or
// Streamable HTTP MCP server. Configuration changes are persisted by the
// composition root and take effect after restart; refresh remains available
// for live diagnostics.
type MCPServerEdit struct {
	OriginalName string            `json:"original_name"`
	Name         string            `json:"name"`
	Transport    string            `json:"transport"`
	Cmd          string            `json:"cmd"`
	Args         []string          `json:"args"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	Env          map[string]string `json:"env"`
	// Pointer distinguishes an omitted legacy Web field from an explicit empty
	// string, so updating a server does not accidentally erase its working dir.
	Cwd               *string `json:"cwd"`
	ToolCallTimeoutMS *int    `json:"tool_call_timeout_ms"`
	// Pointer distinguishes an omitted legacy Web field from an explicit false
	// so updating a server does not accidentally erase its startup policy.
	FailOnStartupError *bool `json:"fail_on_startup_error"`
}

// QueueItem is one user message waiting behind the active turn. Placement is
// kept explicit so the wire shape can grow to match dsh's queued/steered
// states without changing the frontend contract.
type QueueItem struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	Placement string    `json:"placement"`
	// Content preserves rich prompt admission for the native DSH queue
	// projection. The legacy REST queue remains text-only and may leave it nil.
	Content []llm.ContentBlock `json:"-"`
}

// Stable queue admission failures returned by composition-root callbacks and
// mapped to DSH's session.updateQueue error codes. Keep these sentinel-wrapped
// so an implementation can add context without losing the wire classification.
var (
	// ErrQueueItemNotFound means the id is not pending in this live queue.
	ErrQueueItemNotFound = errors.New("queued item is no longer pending")
	// ErrSteerUnavailable means the pending item exists, but the current turn
	// no longer accepts a strict steer.
	ErrSteerUnavailable = errors.New("current turn no longer accepts steering")
)

// ProviderDiscover is the POST /api/config/provider/discover payload: the
// endpoint as the form currently shows it (base URL + protocol + a key typed
// but not yet saved), plus an optional directory route that answers from its
// own catalog.
type ProviderDiscover struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Protocol string `json:"protocol"`
	APIKey   string `json:"api_key"`
}

// SkillFile is one uploaded skill file for the add action: a path relative to
// the skill root plus its base64 content (aligned with the plugin's add wire).
type SkillFile struct {
	Path   string `json:"path"`
	Base64 string `json:"base64"`
}

// SkillRequest is the unified /api/config/skills payload. Every action reads
// only the fields it needs:
//   - list:        no fields
//   - content:     name + scope
//   - set_enabled: name + scope + enabled
//   - delete:      name + scope
//   - add:         kind + files + scope
//   - migrate:     name + from + to + mode
//   - group_save:  group_id + group_name + scope + names
//   - group_delete: group_id
//
// Scope is a string ("global" | "project") — the plugin's per-workspace scope
// object collapses to our two roots (有差异对齐, 显式记录).
type SkillRequest struct {
	Name      string      `json:"name"`
	Scope     string      `json:"scope"`
	From      string      `json:"from"`
	To        string      `json:"to"`
	Mode      string      `json:"mode"`
	Kind      string      `json:"kind"`
	Enabled   bool        `json:"enabled"`
	Files     []SkillFile `json:"files"`
	GroupID   string      `json:"group_id"`
	GroupName string      `json:"group_name"`
	Names     []string    `json:"names"`
}

// SkillCatalogEntry is the read-only, session-scoped skill projection used by
// DSH's slash picker. Provenance and filesystem paths intentionally stay
// host-side.
type SkillCatalogEntry struct {
	Name           string
	Description    string
	WhenToUse      string
	ModelInvocable bool
}

// SetSkillManager wires the skill-management API (POST /api/config/skills, the
// dsh-skill-mcp-panel 对齐 settings page). The composition root dispatches each
// action to the skill.Manager and returns a JSON-able result map. nil (the
// default) keeps every /api/config/skills route at 501.
func (s *Server) SetSkillManager(fn func(ctx context.Context, action string, req SkillRequest) (map[string]any, error)) {
	s.skillsFn = fn
}

// SetSkillCatalogProvider wires DSH's session-addressed skill.list contract.
// The host resolves the session cwd before invoking the provider; skill lookup
// never creates or resumes an Agent.
func (s *Server) SetSkillCatalogProvider(fn func(ctx context.Context, cwd string) ([]SkillCatalogEntry, error)) {
	s.skillCatalogFn = fn
}

// SetMCPManager wires the live MCP diagnostics/refresh action used by the
// runtime inventory page. It does not mutate configuration or secrets.
func (s *Server) SetMCPManager(fn func(ctx context.Context) ([]map[string]any, error)) {
	s.mcpRefreshFn = fn
}

// SetMCPConfigManager wires add/update/delete persistence for MCP servers.
func (s *Server) SetMCPConfigManager(fn func(ctx context.Context, action string, edit MCPServerEdit) ([]map[string]any, error)) {
	s.mcpManageFn = fn
}

// SetInteractionManager wires the live approval surface used by DSH Web.
func (s *Server) SetInteractionManager(
	list func(ctx context.Context, sessionID string) ([]interact.Request, error),
	resolve func(ctx context.Context, sessionID, id string, status interact.ApprovalStatus, answer string) error,
) {
	s.interactionFn = list
	s.resolveInteractionFn = resolve
}

// SetInteractionSessionResolver wires the durable owner lookup used when a
// native client cancels a question without echoing sessionId. The lookup is
// intentionally separate from the list/resolve callbacks so a reconnecting
// process can recover correlation without exposing the approval payload.
func (s *Server) SetInteractionSessionResolver(fn func(context.Context, string) (string, error)) {
	s.resolveInteractionSessionFn = fn
}

// SetAttachmentStore wires the image-attachment store (P5): POST/GET
// /api/sessions/{id}/attachments and the images field of POST /api/sessions/
// {id}/message. Called by the composition root; nil (default) keeps the
// attachment APIs at 501.
func (s *Server) SetAttachmentStore(st *attachment.Store) { s.att = st }

// SetNativeImageCapabilityResolver wires the session-specific image route
// check used by native rich prompts. The generic server only owns attachment
// storage; the composition root owns provider/model capability resolution.
func (s *Server) SetNativeImageCapabilityResolver(fn func(context.Context, string) bool) {
	s.nativeImageCapabilityFn = fn
}

// SetModelSwitcher wires the live model switch (POST /api/config/model, P5.1):
// the handler validates the provider/model/reasoning-effort and rebuilds the
// selected LLM provider. Called by the composition root; nil (default) keeps
// the API at 501.
func (s *Server) SetModelSwitcher(fn func(ctx context.Context, provider, model, effort string) error) {
	s.setModelFn = fn
}

// SetSessionModelValidator wires the shared runtime route-admission check for
// native session.selectModel. The composition root owns provider availability,
// effort policy and model-catalog semantics.
func (s *Server) SetSessionModelValidator(fn func(ctx context.Context, sessionID, provider, model, effort string) error) {
	s.sessionModelValidateFn = fn
}

// SetNativeDefaultModelSaver wires DSH's shared Agent-default model behavior
// for the native session.selectModel RPC. The current session remains owned by
// its session config; this callback only updates the default used by sessions
// created afterwards.
func (s *Server) SetNativeDefaultModelSaver(fn func(ctx context.Context, provider, model, effort string)) {
	s.nativeDefaultModelFn = fn
}

// SetNativeSessionRenamer wires the canonical session/title event boundary for
// native session.rename. The composition root owns live-session materialization
// and persistence; the webserver only transports the accepted title and seq.
func (s *Server) SetNativeSessionRenamer(fn func(ctx context.Context, sessionID, title string) (int64, error)) {
	s.nativeSessionRenameFn = fn
}

// SetProviderManager wires the M11 provider-management API (POST /api/config/
// provider to save an API-key override or a custom provider, DELETE
// /api/config/provider to remove a custom provider). Called by the composition
// root; nil (default) keeps those APIs at 501.
func (s *Server) SetProviderManager(fn func(ctx context.Context, action string, edit ProviderEdit) error) {
	s.setProviderFn = fn
}

// SetProviderDiscover wires the M11-pi-ai model discovery API (POST
// /api/config/provider/discover to ask an endpoint which models it serves for
// the 获取可用模型 action). Called by the composition root; nil (default) keeps
// the API at 501.
func (s *Server) SetProviderDiscover(fn func(ctx context.Context, request ProviderDiscover) ([]ProviderModel, error)) {
	s.setDiscoverFn = fn
}

// SetDefaultWorkdir sets the absolute directory used by ungrouped sessions
// and by workspaces created without an explicit path.
func (s *Server) SetDefaultWorkdir(dir string) { s.defaultWorkdir = dir }

// SetFrontendDist selects the React/Cordis SPA dist directory. The directory
// must contain index.html so a misconfigured production server fails during
// startup instead of exposing a different frontend.
func (s *Server) SetFrontendDist(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("webserver: frontend dist is required")
	}
	clean := filepath.Clean(dir)
	info, err := os.Stat(filepath.Join(clean, "index.html"))
	if err != nil {
		return fmt.Errorf("webserver: frontend index: %w", err)
	}
	if info.IsDir() {
		return errors.New("webserver: frontend index is a directory")
	}
	s.frontendDir = clean
	return nil
}

// empty opens the portal to the local machine (dsh-style, no login); a token
// turns on bearer auth and only its SHA-256 digest is retained. addr defaults
// to "127.0.0.1:8080".
func New(st store.Store, token, addr string) (*Server, error) {
	if st == nil {
		return nil, errors.New("webserver: store is required")
	}
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	s := &Server{
		store:                     st,
		tokenHash:                 sha256.Sum256([]byte(token)),
		authOn:                    token != "",
		addr:                      addr,
		nativeInteractionSessions: make(map[string]string),
		nativeConns:               make(map[io.Closer]struct{}),
		closeDone:                 make(chan struct{}),
		nativeSettings: map[string]nativeSettingsDocument{
			// Shutu is not the DSH developer-preview distribution. Mark the
			// DSH product welcome notice as acknowledged so the native UI opens
			// directly on the application instead of showing that declaration.
			nativeSettingsOnboarding: {Value: map[string]any{
				"welcomeNoticeVersion": nativeWelcomeNoticeVersion,
			}},
		},
		nativeSettingsLoaded: make(map[string]bool),
		profileRegistry:      profile.Local(),
		crashContracts:       crashboundary.Required(),
	}
	// The React shell (login view + frontend assets) is public so a fresh
	// browser can load the page and present the token form (D-WEB-2): it holds
	// no data. Every /api route sits behind the bearer middleware.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.Handle("GET /api/health", s.requireAuth(http.HandlerFunc(s.handleHealth)))
	mux.Handle("GET /api/stats", s.requireAuth(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /api/sessions", s.requireAuth(http.HandlerFunc(s.handleSessions)))
	mux.Handle("GET /api/sessions/{id}/events", s.requireAuth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /api/sessions/{id}/state", s.requireAuth(http.HandlerFunc(s.handleSessionState)))
	mux.Handle("GET /api/interactions", s.requireAuth(http.HandlerFunc(s.handleInteractions)))
	mux.Handle("POST /api/interactions/{id}/resolve", s.requireAuth(http.HandlerFunc(s.handleInteractionResolve)))
	// dsh /export compatibility: the browser downloads the current Session log
	// as a ZIP and keeps the command outside model history.
	mux.Handle("GET /api/session.export", s.requireAuth(http.HandlerFunc(s.handleSessionExport)))
	mux.Handle("HEAD /api/session.export", s.requireAuth(http.HandlerFunc(s.handleSessionExport)))
	mux.Handle("GET /api/sessions/{id}/feedback", s.requireAuth(http.HandlerFunc(s.handleFeedbackList)))
	mux.Handle("PUT /api/sessions/{id}/feedback/{seq}", s.requireAuth(http.HandlerFunc(s.handleFeedbackPut)))
	mux.Handle("DELETE /api/sessions/{id}/feedback/{seq}", s.requireAuth(http.HandlerFunc(s.handleFeedbackDelete)))
	// ContextMeter (dsh ContextMeter): the current session's estimated tokens.
	mux.Handle("GET /api/sessions/{id}/context", s.requireAuth(http.HandlerFunc(s.handleSessionContext)))
	// Stop a running turn (dsh 停止按钮).
	mux.Handle("POST /api/sessions/{id}/stop", s.requireAuth(http.HandlerFunc(s.handleTurnStop)))
	// M10 W1 interactive API (ADR D-WEB2): session new/resume, message dispatch
	// and the SSE event stream all sit behind the same bearer middleware.
	mux.Handle("POST /api/sessions", s.requireAuth(http.HandlerFunc(s.handleSessionCreate)))
	mux.Handle("POST /api/sessions/{id}/resume", s.requireAuth(http.HandlerFunc(s.handleSessionResume)))
	// P6.2: fork (clone into a new session), archive/unarchive, manual order.
	mux.Handle("POST /api/sessions/{id}/fork", s.requireAuth(http.HandlerFunc(s.handleSessionFork)))
	mux.Handle("POST /api/sessions/{id}/archive", s.requireAuth(http.HandlerFunc(s.handleSessionArchive)))
	mux.Handle("POST /api/sessions/{id}/unarchive", s.requireAuth(http.HandlerFunc(s.handleSessionUnarchive)))
	mux.Handle("PATCH /api/sessions/order", s.requireAuth(http.HandlerFunc(s.handleSessionsOrder)))
	mux.Handle("PATCH /api/sessions/flat-order", s.requireAuth(http.HandlerFunc(s.handleSessionsFlatOrder)))
	// P6.3 remote search across session bodies (dsh searchAcrossSessions).
	mux.Handle("GET /api/search", s.requireAuth(http.HandlerFunc(s.handleSearch)))
	// dsh file reference compatibility: browse/search the addressed session's
	// workspace and preview bounded text with line ranges.
	mux.Handle("GET /api/sessions/{id}/files", s.requireAuth(http.HandlerFunc(s.handleSessionFiles)))
	mux.Handle("GET /api/sessions/{id}/file", s.requireAuth(http.HandlerFunc(s.handleSessionFile)))
	// P6 workspace grouping (dsh grouped sidebar view): list, create, rename,
	// delete, order. The sessions list carries workspace_id so the sidebar groups.
	mux.Handle("GET /api/workspaces", s.requireAuth(http.HandlerFunc(s.handleWorkspaces)))
	mux.Handle("POST /api/workspaces", s.requireAuth(http.HandlerFunc(s.handleWorkspaceCreate)))
	mux.Handle("POST /api/workspaces/pick-directory", s.requireAuth(http.HandlerFunc(s.handleWorkspacePickDirectory)))
	mux.Handle("GET /api/workspaces/directories", s.requireAuth(http.HandlerFunc(s.handleWorkspaceDirectoryList)))
	mux.Handle("POST /api/workspaces/directories", s.requireAuth(http.HandlerFunc(s.handleWorkspaceDirectoryCreate)))
	mux.Handle("PATCH /api/workspaces/{id}", s.requireAuth(http.HandlerFunc(s.handleWorkspaceTitle)))
	mux.Handle("DELETE /api/workspaces/{id}", s.requireAuth(http.HandlerFunc(s.handleWorkspaceDelete)))
	mux.Handle("PATCH /api/workspaces/order", s.requireAuth(http.HandlerFunc(s.handleWorkspacesOrder)))
	mux.Handle("POST /api/sessions/{id}/message", s.requireAuth(http.HandlerFunc(s.handleMessage)))
	// dsh queue/steer compatibility: queued messages are session-scoped and
	// managed by the composition root, while this server only transports them.
	mux.Handle("GET /api/sessions/{id}/queue", s.requireAuth(http.HandlerFunc(s.handleQueueList)))
	mux.Handle("POST /api/sessions/{id}/queue", s.requireAuth(http.HandlerFunc(s.handleQueueEnqueue)))
	mux.Handle("PATCH /api/sessions/{id}/queue/{itemID}", s.requireAuth(http.HandlerFunc(s.handleQueueUpdate)))
	// M10 P2 (ADR D-WEB2-I): sidebar session management — rename (PATCH) and
	// delete (DELETE). PATCH body is {"title": "..."}; an empty title clears the
	// override back to first-user-message inference.
	mux.Handle("PATCH /api/sessions/{id}/title", s.requireAuth(http.HandlerFunc(s.handleSessionTitle)))
	mux.Handle("GET /api/sessions/{id}/config", s.requireAuth(http.HandlerFunc(s.handleSessionConfigGet)))
	mux.Handle("PATCH /api/sessions/{id}/config", s.requireAuth(http.HandlerFunc(s.handleSessionConfigPatch)))
	mux.Handle("DELETE /api/sessions/{id}", s.requireAuth(http.HandlerFunc(s.handleSessionDelete)))
	// M10 P5 (ADR D-WEB2-I): image attachments — multipart upload (POST) and
	// byte echo (GET). Both stay behind the same bearer middleware.
	mux.Handle("POST /api/sessions/{id}/attachments", s.requireAuth(http.HandlerFunc(s.handleAttachmentUpload)))
	mux.Handle("GET /api/sessions/{id}/attachments/{attID}", s.requireAuth(http.HandlerFunc(s.handleAttachmentGet)))
	// P5.1: live model switch (provider/model), answers the new config state.
	mux.Handle("POST /api/config/model", s.requireAuth(http.HandlerFunc(s.handleModelSwitch)))
	// M11: provider management — save an API-key override or a custom provider
	// profile (POST) / remove a custom provider (DELETE). Both persist to the
	// settings table and apply immediately (the registry is rebuilt).
	mux.Handle("POST /api/config/provider", s.requireAuth(http.HandlerFunc(s.handleProviderSave)))
	mux.Handle("DELETE /api/config/provider", s.requireAuth(http.HandlerFunc(s.handleProviderDelete)))
	mux.Handle("POST /api/config/provider/discover", s.requireAuth(http.HandlerFunc(s.handleProviderDiscover)))
	// 技能设置页 (dsh-skill-mcp-panel 对齐): the skill-management API. One route
	// carries every action (list/content/set_enabled/delete/add/migrate/group_save/
	// group_delete) selected by the "action" body field.
	mux.Handle("POST /api/config/skills", s.requireAuth(http.HandlerFunc(s.handleSkills)))
	mux.Handle("GET /api/config/skills", s.requireAuth(http.HandlerFunc(s.handleSkillsList)))
	mux.Handle("GET /api/settings", s.requireAuth(http.HandlerFunc(s.handleSettingsGet)))
	mux.Handle("PATCH /api/settings", s.requireAuth(http.HandlerFunc(s.handleSettingsPatch)))
	mux.Handle("GET /api/sessions/{id}/events/stream", s.requireAuth(http.HandlerFunc(s.handleEventStream)))
	// M10 W2 (ADR D-WEB2-D): the read-only sanitized config view.
	mux.Handle("GET /api/config", s.requireAuth(http.HandlerFunc(s.handleConfig)))
	mux.Handle("POST /api/config/mcp/refresh", s.requireAuth(http.HandlerFunc(s.handleMCPRefresh)))
	mux.Handle("POST /api/config/mcp", s.requireAuth(http.HandlerFunc(s.handleMCPManage)))
	// M10 W4 (ADR D-WEB2-H): the read-only subagent and background-job panels.
	mux.Handle("GET /api/subagents", s.requireAuth(http.HandlerFunc(s.handleSubagents)))
	mux.Handle("GET /api/jobs", s.requireAuth(http.HandlerFunc(s.handleJobs)))
	// DSH client-hmr opens this public development channel on every native
	// page. Shutu's production dist has no hot-rebuild publisher, but returning
	// a valid idle SSE stream prevents the native UI from treating the missing
	// optional channel as a transport failure.
	mux.Handle("GET /plugins/events", http.HandlerFunc(s.handlePluginEvents))
	// DSH native Connection transport: unary client-request RPC plus the two
	// downlink-only WebSocket streams. The existing REST routes remain intact.
	s.registerNativeRoutes(mux)
	s.coreHandler = mux
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

type ExtensionRoute struct {
	ExtensionID       string `json:"extensionId"`
	Title             string `json:"title"`
	Route             string `json:"route"`
	Icon              string `json:"icon,omitempty"`
	NavigationEnabled bool   `json:"navigationEnabled"`
	NavigationGroup   string `json:"navigationGroup"`
	Order             int    `json:"order,omitempty"`
	Ready             bool   `json:"ready"`
	ServiceURL        string `json:"-"`
}

// SetExtensionRoutes publishes generic reverse-proxied extension web apps.
// The shell owns routing and authentication; each extension owns its business
// UI and process. Passing nil clears the contribution.
func (s *Server) SetExtensionRoutes(routes []ExtensionRoute) {
	if s == nil || s.srv == nil {
		return
	}
	s.extensionMu.Lock()
	s.extensionRoutes = append([]ExtensionRoute(nil), routes...)
	s.extensionMu.Unlock()
	mux := http.NewServeMux()
	for _, route := range routes {
		segment := urlPathSegment(route.ExtensionID)
		if segment == "" || route.ServiceURL == "" {
			continue
		}
		mux.Handle("/extensions/"+segment+"/", s.requireAuth(http.HandlerFunc(s.handleExtensionReverseProxy)))
		mux.Handle("/extensions/"+segment, s.requireAuth(http.HandlerFunc(s.handleExtensionRedirect)))
	}
	mux.Handle("GET /api/extensions", s.requireAuth(http.HandlerFunc(s.handleExtensionList)))
	mux.Handle("/", s.coreHandler)
	s.srv.Handler = mux
}

func (s *Server) handlePluginEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "event stream unsupported", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, ": native hmr channel idle\n\n")
	flusher.Flush()
	<-r.Context().Done()
}

// Handler returns the authenticated HTTP handler (for httptest).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.addr }

// Serve blocks serving the portal until Close.
func (s *Server) Serve() error {
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close shuts the server down (idempotent).
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	s.closeMu.Lock()
	if s.closed {
		done := s.closeDone
		s.closeMu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	s.closed = true
	done := s.closeDone
	s.closeMu.Unlock()
	s.nativeConnMu.Lock()
	s.nativeClosing = true
	connections := make([]io.Closer, 0, len(s.nativeConns))
	for conn := range s.nativeConns {
		connections = append(connections, conn)
	}
	s.nativeConnMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	s.nativeDone.Wait()
	if done != nil {
		close(done)
	}
	return err
}

func (s *Server) trackNativeConnection(conn io.Closer) bool {
	if s == nil || conn == nil {
		return false
	}
	s.nativeConnMu.Lock()
	if s.nativeClosing {
		s.nativeConnMu.Unlock()
		_ = conn.Close()
		return false
	}
	s.nativeDone.Add(1)
	if s.nativeConns == nil {
		s.nativeConns = make(map[io.Closer]struct{})
	}
	s.nativeConns[conn] = struct{}{}
	s.nativeConnMu.Unlock()
	return true
}

func (s *Server) untrackNativeConnection(conn io.Closer) {
	if s == nil || conn == nil {
		return
	}
	s.nativeConnMu.Lock()
	delete(s.nativeConns, conn)
	s.nativeConnMu.Unlock()
	s.nativeDone.Done()
}

// SetMessageHandler wires the message dispatch API (POST
// /api/sessions/{id}/message). images carries the parsed image refs of the
// message (P5), nil/empty for text-only turns. Called by the composition root
// (cmd/sta) at registration time; nil (the default) makes the API answer 501.
func (s *Server) SetMessageHandler(fn func(ctx context.Context, sessionID, text string, images []llm.ImageRef, meta PromptMeta) error) {
	s.msgFn = fn
}

// SetQueueManager wires the dsh-style per-session queue. The composition root
// owns execution and cancellation; nil callbacks leave the queue API at 501.
func (s *Server) SetQueueManager(
	list func(ctx context.Context, sessionID string) ([]QueueItem, error),
	enqueue func(ctx context.Context, sessionID, text string, content []llm.ContentBlock, meta PromptMeta) (QueueItem, error),
	update func(ctx context.Context, sessionID, itemID, action string) error,
) {
	s.queueListFn = list
	s.queueEnqueueFn = enqueue
	s.queueUpdateFn = update
}

// subscribeNativeMux registers a refresh callback for one native session
// stream. It is intentionally private: native mux owns the DSH frame shape and
// only the webserver needs to coordinate queue/job snapshots with mutations.
func (s *Server) subscribeNativeMux(sessionID string, refresh func()) func() {
	s.nativeMuxMu.Lock()
	if s.nativeMuxSubscribers == nil {
		s.nativeMuxSubscribers = make(map[string]map[uint64]func())
	}
	s.nativeMuxSubscriberID++
	id := s.nativeMuxSubscriberID
	if s.nativeMuxSubscribers[sessionID] == nil {
		s.nativeMuxSubscribers[sessionID] = make(map[uint64]func())
	}
	s.nativeMuxSubscribers[sessionID][id] = refresh
	s.nativeMuxMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.nativeMuxMu.Lock()
			defer s.nativeMuxMu.Unlock()
			listeners := s.nativeMuxSubscribers[sessionID]
			delete(listeners, id)
			if len(listeners) == 0 {
				delete(s.nativeMuxSubscribers, sessionID)
			}
		})
	}
}

// notifyNativeMux asks every native subscriber for the latest authoritative
// control-plane snapshots after a queue mutation.
func (s *Server) notifyNativeMux(sessionID string) {
	s.nativeMuxMu.Lock()
	listeners := make([]func(), 0, len(s.nativeMuxSubscribers[sessionID]))
	for _, refresh := range s.nativeMuxSubscribers[sessionID] {
		listeners = append(listeners, refresh)
	}
	s.nativeMuxMu.Unlock()
	for _, refresh := range listeners {
		refresh()
	}
}

// subscribeNativeMuxSessionAdded registers a callback for sessions created
// after a native mux connection was established. DSH keeps the mux stream
// resident while the user creates sessions, so the new session must join its
// event subscription without waiting for a browser refresh.
func (s *Server) subscribeNativeMuxSessionAdded(added func(string)) func() {
	s.nativeMuxMu.Lock()
	if s.nativeMuxSessionAdded == nil {
		s.nativeMuxSessionAdded = make(map[uint64]func(string))
	}
	s.nativeMuxSubscriberID++
	id := s.nativeMuxSubscriberID
	s.nativeMuxSessionAdded[id] = added
	s.nativeMuxMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.nativeMuxMu.Lock()
			delete(s.nativeMuxSessionAdded, id)
			s.nativeMuxMu.Unlock()
		})
	}
}

// notifyNativeMuxSessionAdded asks every connected native mux stream to
// subscribe a newly created session immediately.
func (s *Server) notifyNativeMuxSessionAdded(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	s.nativeMuxMu.Lock()
	listeners := make([]func(string), 0, len(s.nativeMuxSessionAdded))
	for _, added := range s.nativeMuxSessionAdded {
		listeners = append(listeners, added)
	}
	s.nativeMuxMu.Unlock()
	for _, added := range listeners {
		added(sessionID)
	}
}

// SetNativeQueueUpdater wires DSH session.updateQueue. text is populated only
// for the edit action; remove and steer receive an empty string.
func (s *Server) SetNativeQueueUpdater(fn func(ctx context.Context, sessionID, itemID, action, text string) error) {
	s.nativeQueueUpdateFn = fn
}

// SetNativeSubagentManager wires the live child inbox and cancellation seams
// used by DSH's subagent.prompt and subagent.interrupt RPCs.
func (s *Server) SetNativeSubagentManager(
	prompt func(ctx context.Context, childSessionID string, content []llm.ContentBlock, meta PromptMeta) error,
	interrupt func(childSessionID, reason string) error,
) {
	s.nativeSubagentPromptFn = prompt
	s.nativeSubagentInterruptFn = interrupt
}

// NativeGoalMutation is the transport-neutral input for the DSH goal.*
// mutation family. Optional pointers preserve the distinction between an
// omitted edit field and an explicitly supplied value.
type NativeGoalMutation struct {
	Action        string
	SessionID     string
	GoalID        string
	Revision      int
	Objective     *string
	MaxGoalRounds *int
}

// NativeGoalMutationResult is the acknowledgement returned by the live goal
// manager. Non-clear mutations return the new compare-and-set reference.
type NativeGoalMutationResult struct {
	GoalID   string
	Revision int
	Cleared  bool
}

// NativeAgentPreset is one DSH agent-preset roster entry. The composition
// root owns how presets are stored and composed; the webserver only transports
// the sanitized native contract.
type NativeAgentPreset struct {
	ID          string
	Trust       string
	IsDefault   bool
	Name        string
	Description string
	Broken      string
}

type NativeAgentPresetCatalog struct {
	Presets     []NativeAgentPreset
	Authorable  bool
	HasDocument bool
}

type NativeAgentPresetDetails struct {
	AgentPreset string
	Trust       string
	Content     string
	Name        string
	Description string
}

type NativeAgentPresetDocument struct {
	Opened bool
	Path   string
}

// NativeAgentPresetManager wires the DSH authoring calls. IDs, trust and
// content are resolved by the composition root; no filesystem path crosses
// the browser request boundary.
type NativeAgentPresetManager interface {
	List(context.Context) (NativeAgentPresetCatalog, error)
	Read(context.Context, string) (NativeAgentPresetDetails, error)
	Copy(context.Context, string, string, string) (string, error)
	OpenDocument(context.Context, string) (NativeAgentPresetDocument, error)
	Remove(context.Context, string) error
}

// NativeCommand describes one host-side slash command in the DSH command
// directory. The command name never includes the leading slash.
type NativeCommand struct {
	Name        string
	Description string
	InputHint   string
	Images      bool
}

// NativeCommandResult is the settled result rendered by DSH's command flow.
// Kind is either "success" or "error"; successful results may omit Text.
type NativeCommandResult struct {
	Kind           string
	Text           string
	SourceEventSeq *uint64
}

// NativeCommandExecution pairs a command result with the lifecycle id used
// by the native command flow.
type NativeCommandExecution struct {
	CommandID string
	Result    NativeCommandResult
}

// NativeCommandManager wires DSH command discovery and execution to the
// composition root. The webserver owns only the transport contract.
type NativeCommandManager interface {
	List(context.Context, string) ([]NativeCommand, error)
	Execute(context.Context, string, string, []llm.ImageRef) (NativeCommandExecution, bool, error)
}

// SetNativeCommandManager wires DSH commands/list and commands/execute.
func (s *Server) SetNativeCommandManager(manager NativeCommandManager) {
	s.nativeCommandManager = manager
}

// SetProfileRegistry wires the fail-closed runtime deployment-profile authority.
// Local() is the production default, but embedders may replace the inventory
// without changing transport handlers.
func (s *Server) SetProfileRegistry(registry *profile.Registry) {
	if registry == nil {
		return
	}
	s.profileRegistry = registry
}

// SetCrashBoundaryRegistry replaces the machine-readable external side-effect
// crash contracts. Required() is the production authority.
func (s *Server) SetCrashBoundaryRegistry(registry *crashboundary.Registry) {
	if registry == nil {
		return
	}
	s.crashContracts = registry
}

// SetNativeGoalManager wires DSH's goal.create/edit/pause/resume/complete/clear
// RPCs to the composition root's durable goal engine. A nil manager leaves the
// native routes explicitly unsupported.
func (s *Server) SetNativeGoalManager(fn func(context.Context, NativeGoalMutation) (NativeGoalMutationResult, error)) {
	s.nativeGoalMutationFn = fn
}

// SetNativeCredentialManager wires the value-bearing credential mutations.
// The server never stores or returns credential values itself.
func (s *Server) SetNativeCredentialManager(
	set func(ctx context.Context, ref, value string) error,
	unset func(ctx context.Context, ref string) error,
) {
	s.nativeCredentialSetFn = set
	s.nativeCredentialUnsetFn = unset
}

// SetNativeSettingsDocumentOpener wires DSH settings.openDocument. The
// composition root owns the configured document path; the browser never sends
// a path, so this callback cannot be used to open an arbitrary host file.
func (s *Server) SetNativeSettingsDocumentOpener(fn func(context.Context) error) {
	s.nativeSettingsOpenDocumentFn = fn
}

// SetNativeSettingsApplier notifies the composition root after a native
// settings document changes. The callback receives the resolved base+user view
// so provider owners can rebuild live runtime state from the effective facts.
func (s *Server) SetNativeSettingsApplier(fn func(ctx context.Context, namespace string, resolved map[string]any) error) {
	s.nativeSettingsAppliedFn = fn
}

// SetNativeAgentPresetManager wires the DSH agent-preset authoring surface.
func (s *Server) SetNativeAgentPresetManager(manager NativeAgentPresetManager) {
	s.nativeAgentPresetManager = manager
}

// SetSessionManager wires the session new/resume API (POST /api/sessions and
// POST /api/sessions/{id}/resume). Called by the composition root; nil makes
// those APIs answer 501.
func (s *Server) SetSessionManager(fn func(ctx context.Context, action, id string) (string, error)) {
	s.sessFn = fn
}

// NativeSessionCreateSpec carries DSH session.create admission inputs. A
// non-empty SessionID adopts a durable session only when its cwd and explicit
// preset are compatible; an empty ID reserves a fresh identity.
type NativeSessionCreateSpec struct {
	SessionID            string
	CWD                  string
	WorkspaceID          string
	AgentPreset          string
	AgentPresetRequested bool
}

// NativeSessionCreateResult echoes the identity actually served and the preset
// actually running in it. The preset may differ from the request default when a
// named identity adopts an existing session.
type NativeSessionCreateResult struct {
	SessionID   string
	AgentPreset string
	CWD         string
}

// NativeSessionCreateError lets a runtime creator return DSH's stable create
// error domain (for example session-conflict or agent-preset-conflict) while
// the transport owns the RPC envelope.
type NativeSessionCreateError struct {
	Code    string
	Message string
	Details map[string]any
}

// nativeSessionCreateFlight deduplicates concurrent named create retries so
// only the first identity admission runs; followers await the same outcome.
type nativeSessionCreateFlight struct {
	done    chan struct{}
	result  NativeSessionCreateResult
	failure nativeRPCResult
	failed  bool
}

func (e *NativeSessionCreateError) Error() string { return e.Message }

func NewNativeSessionCreateError(code, message string, details map[string]any) *NativeSessionCreateError {
	return &NativeSessionCreateError{Code: code, Message: message, Details: details}
}

// SetNativeSessionCreator wires the DSH native create/adoption path. Unlike
// the legacy session manager, it honors the caller-requested identity and cwd.
func (s *Server) SetNativeSessionCreator(fn func(ctx context.Context, spec NativeSessionCreateSpec) (NativeSessionCreateResult, error)) {
	s.nativeSessionCreateFn = fn
}

// SetEventSource wires the real-time event stream (GET
// /api/sessions/{id}/events/stream): the source subscribes a session and calls
// sink for each new event; the returned func unsubscribes. Called by the
// composition root; nil makes the stream answer 501.
func (s *Server) SetEventSource(fn func(sessionID string, sink func(session.Event)) func()) {
	s.evSrc = fn
}

// SetConfigProvider wires the read-only config view (GET /api/config, M10 W2,
// ADR D-WEB2-D). The provider returns a sanitized map — cmd/sta's webConfig
// never includes web_server.token or any key — and the webserver forwards it
// verbatim. Called by the composition root; nil makes the API answer 501.
func (s *Server) SetConfigProvider(fn func() map[string]any) {
	s.cfgFn = fn
}

// SetContextWindow wires the ContextMeter's token budget for
// GET /api/sessions/{id}/context. fn takes the session id (so the per-session
// model override can be honored) and returns the effective model's context
// window; 0 makes the server fall back to its default. Called by the
// composition root; nil keeps the default budget.
func (s *Server) SetContextWindow(fn func(sessionID string) int) {
	s.contextWindowFn = fn
}

// SetContextMeter wires the replay-aware token measurement used by
// GET /api/sessions/{id}/context. A nil function retains the standalone
// compatibility estimator for embedders that do not own a meter.
func (s *Server) SetContextMeter(fn func(sessionID string, events []session.Event) (meter.Measurement, error)) {
	s.contextMeterFn = fn
}

// contextWindowForSession resolves the same effective capacity used by the
// generic context endpoint, for native DSH projection baselines and streams.
func (s *Server) contextWindowForSession(sessionID string) int {
	window := defaultContextWindow
	if s.contextWindowFn != nil {
		if resolved := s.contextWindowFn(sessionID); resolved > 0 {
			window = resolved
		}
	}
	return window
}

// SetSessionStateProvider wires the durable per-session state projection. A
// nil provider leaves the endpoint at 501, preserving the generic server's
// optional-composition behavior.
func (s *Server) SetSessionStateProvider(fn func(ctx context.Context, sessionID string) (map[string]any, error)) {
	s.stateFn = fn
}

// SetTurnStopper wires the running-turn cancel for POST
// /api/sessions/{id}/stop (dsh 停止按钮). Called by the composition root; nil
// keeps the API at 501.
func (s *Server) SetTurnStopper(fn func(sessionID string) error) {
	s.stopFn = fn
}

// SetSessionStatusProvider wires the live per-session status computation
// (dsh-session-status alignment): given one session's durable metadata it
// returns the dot state (warning/ongoing/done/idle) + ordered status entries so
// the sidebar renders the status dot and the hover card without the webserver
// knowing any runtime state. Called by the composition root; nil leaves every
// row's status empty.
func (s *Server) SetSessionStatusProvider(fn func(ctx context.Context, m store.SessionMeta) SessionStatus) {
	s.statusFn = fn
}

// SetLiveAgentCounter wires the read-only live Agent count for host.describe.
// Called by the composition root; nil leaves the count at zero.
func (s *Server) SetLiveAgentCounter(fn func() int) {
	s.liveAgentCountFn = fn
}

// SetSubagentProvider wires the read-only subagent panel (GET /api/subagents,
// M10 W4, ADR D-WEB2-H). The provider returns sanitized child-agent views
// (id/status/timestamps only). Called by the composition root; nil makes the
// API answer 501.
func (s *Server) SetSubagentProvider(fn func(ctx context.Context, sessionID string) ([]map[string]any, error)) {
	s.subFn = fn
}

// SetJobsProvider wires the read-only background-job panel (GET /api/jobs, M10
// W4, ADR D-WEB2-H). The provider returns sanitized job views (id/kind/status/
// timestamps only — no outputs). Called by the composition root; nil makes the
// API answer 501.
func (s *Server) SetJobsProvider(fn func(ctx context.Context, sessionID string) ([]map[string]any, error)) {
	s.jobsFn = fn
}

// InteractiveHandlers is a snapshot of the currently injected interactive
// wiring (M10 W1, ADR D-WEB2). The composition root reads it in its wiring
// tests; nil fields mean the corresponding API answers 501.
type InteractiveHandlers struct {
	Message   func(ctx context.Context, sessionID, text string, images []llm.ImageRef, meta PromptMeta) error
	Session   func(ctx context.Context, action, id string) (string, error)
	Event     func(sessionID string, sink func(session.Event)) func()
	Config    func() map[string]any
	Subagents func(ctx context.Context, sessionID string) ([]map[string]any, error)
	Jobs      func(ctx context.Context, sessionID string) ([]map[string]any, error)
	Model     func(ctx context.Context, provider, model, effort string) error // P5.1 live switch
}

// Handlers returns the current interactive wiring.
func (s *Server) Handlers() InteractiveHandlers {
	return InteractiveHandlers{
		Message: s.msgFn, Session: s.sessFn, Event: s.evSrc, Config: s.cfgFn,
		Subagents: s.subFn, Jobs: s.jobsFn, Model: s.setModelFn,
	}
}

// panicSafeWriter tracks whether a response has started so a deferred recover
// can decide if it may still write a 500 body (writing after the header was
// sent panics again). It also forwards Flush for the SSE stream.
type panicSafeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *panicSafeWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *panicSafeWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *panicSafeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requireAuth wraps an /api handler with the bearer-token check (D-WEB-2): the
// presented token's SHA-256 must match the stored digest under a constant-time
// compare. Only the API routes are gated; the React shell stays public so the
// login view can load (data never leaves the API). It also recovers a panicking
// handler into a JSON 500 (M10 W3 robustness): a crashed route must never
// answer a bare connection reset, and the panic + stack is logged for repair.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &panicSafeWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("sta: web handler panic: %v\n%s", rec, debug.Stack())
				if !sw.wrote {
					writeJSON(sw, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("internal error: %v", rec)})
				}
			}
		}()
		if !s.authOn {
			if !sameOriginMutation(r) {
				writeJSON(sw, http.StatusForbidden, map[string]any{"error": "cross-origin mutation rejected"})
				return
			}
			next.ServeHTTP(sw, r)
			return
		}
		if !sameOriginMutation(r) {
			writeJSON(sw, http.StatusForbidden, map[string]any{"error": "cross-origin mutation rejected"})
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeJSON(sw, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		sum := sha256.Sum256([]byte(strings.TrimPrefix(auth, prefix)))
		if subtle.ConstantTimeCompare(sum[:], s.tokenHash[:]) != 1 {
			writeJSON(sw, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(sw, r)
	})
}

// sameOriginMutation adds a browser-origin boundary for mutating API calls.
// Bearer auth is still the primary credential when configured; this check also
// protects the intentionally open localhost mode from cross-origin form/fetch
// writes. Requests without Origin/Referer remain valid for CLI/ACP/native
// clients, which do not carry browser provenance headers.
func sameOriginMutation(r *http.Request) bool {
	if r == nil || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if raw == "" {
		return true
	}
	if strings.EqualFold(raw, "null") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleIndex serves the configured React/Cordis single-page shell. In the
// ServeMux the pattern "GET /" matches every unmatched path, so the dist
// handler also owns client-side routes and static assets.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.frontendDir == "" {
		http.Error(w, "frontend dist not configured", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/favicon.ico" || strings.HasPrefix(r.URL.Path, "/static/") {
		http.NotFound(w, r)
		return
	}
	s.serveFrontend(w, r)
}

// serveFrontend serves the external SPA with a safe path boundary. Existing
// API routes win in ServeMux; all other GET paths are either a dist asset or
// an SPA fallback to index.html. This mirrors DSH's frontend-static behavior.
func (s *Server) serveFrontend(w http.ResponseWriter, r *http.Request) {
	root := filepath.Clean(s.frontendDir)
	index := filepath.Join(root, "index.html")
	cleanPath := path.Clean("/" + r.URL.Path)
	rel := strings.TrimPrefix(cleanPath, "/")
	target := index
	if rel != "" {
		target = filepath.Join(root, filepath.FromSlash(rel))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, target)
		return
	}
	if _, err := os.Stat(index); err != nil {
		http.Error(w, "frontend index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleConfig implements GET /api/config (M10 W2, ADR D-WEB2-D): it serves
// the injected config provider's sanitized map verbatim. The provider (cmd/sta's
// webConfig) is responsible for redaction — web_server.token and any keys are
// never included — so the API boundary never carries a plaintext secret. An
// unwired provider answers 501.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfgFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "config provider not wired"})
		return
	}
	writeJSON(w, http.StatusOK, s.cfgFn())
}

func (s *Server) handleMCPRefresh(w http.ResponseWriter, r *http.Request) {
	if s.mcpRefreshFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "mcp manager not wired"})
		return
	}
	servers, err := s.mcpRefreshFn(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (s *Server) handleMCPManage(w http.ResponseWriter, r *http.Request) {
	if s.mcpManageFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "mcp config manager not wired"})
		return
	}
	var body struct {
		Action string `json:"action"`
		MCPServerEdit
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	action := strings.TrimSpace(body.Action)
	if action != "add" && action != "update" && action != "delete" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action must be add, update, or delete"})
		return
	}
	servers, err := s.mcpManageFn(r.Context(), action, body.MCPServerEdit)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers, "restart_required": true})
}

// handleSettingsGet implements GET /api/settings (the General-settings rows:
// agent preset / permission preset / default terminal). Stored values come
// from the durable settings table; the *_current values come from the config
// view so the UI can show what is actually in effect after the next restart.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	stored, err := s.store.GetSettings(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	modeOptions := []string{"minimal", "standard"}
	if s.nativeCodeAvailable() {
		modeOptions = append(modeOptions, "code")
	}
	resp := map[string]any{
		"agent_preset":       stored["agent_preset"],
		"permission_preset":  stored["permission_preset"],
		"terminal_shell":     stored["terminal_shell"],
		"language":           stored["language"],
		"mode_options":       modeOptions,
		"permission_options": []string{"readonly", "standard", "full"},
		"terminal_options":   []string{"off", "powershell", "gitbash", "wsl"},
		"restart_required":   true,
	}
	if s.cfgFn != nil {
		if cfg := s.cfgFn(); cfg != nil {
			if m, ok := cfg["mode"].(string); ok {
				resp["mode_current"] = m
			}
			if t, ok := cfg["terminal_enabled"].(bool); ok {
				resp["terminal_current"] = t
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSettingsPatch implements PATCH /api/settings: it stores the changed
// General-settings rows (only non-empty fields are written, so a partial body
// updates just those rows). The composition root applies them at startup —
// they take effect after restart (D-WEB2-D: no runtime hot reload).
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentPreset      string `json:"agent_preset"`
		PermissionPreset string `json:"permission_preset"`
		TerminalShell    string `json:"terminal_shell"`
		Language         string `json:"language"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if body.AgentPreset != "" {
		if body.AgentPreset != "minimal" && body.AgentPreset != "standard" && body.AgentPreset != "code" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid agent_preset"})
			return
		}
		if body.AgentPreset == "code" && !s.nativeCodeAvailable() {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "agent_preset code is unavailable"})
			return
		}
		if err := s.store.SetSetting(r.Context(), "agent_preset", body.AgentPreset); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if body.PermissionPreset != "" {
		if body.PermissionPreset != "readonly" && body.PermissionPreset != "standard" && body.PermissionPreset != "full" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid permission_preset"})
			return
		}
		if err := s.store.SetSetting(r.Context(), "permission_preset", body.PermissionPreset); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if body.TerminalShell != "" {
		if body.TerminalShell != "off" && body.TerminalShell != "powershell" &&
			body.TerminalShell != "gitbash" && body.TerminalShell != "wsl" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid terminal_shell"})
			return
		}
		if err := s.store.SetSetting(r.Context(), "terminal_shell", body.TerminalShell); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if body.Language != "" {
		if body.Language != "zh" && body.Language != "en" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid language"})
			return
		}
		if err := s.store.SetSetting(r.Context(), "language", body.Language); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": true})
}

// StatusEntry is one session status the sidebar renders (dsh ui-workspace
// SessionStatus): the dot color category (warning/ongoing/done/idle) plus the
// localized label. The first entry is the primary status; a session can carry a
// secondary running-subagent entry after it.
type StatusEntry struct {
	State string `json:"state"`
	Label string `json:"label"`
}

// SessionStatus is the live status of one session row (dsh ui-workspace
// sessionStatuses): the primary dot State (warning/ongoing/done/idle) and the
// ordered statuses used for the dot, the screen-reader labels and the hover
// card. State is empty when the webserver has no status provider wired.
type SessionStatus struct {
	State    string        `json:"state,omitempty"`
	Statuses []StatusEntry `json:"statuses,omitempty"`
}

// M10 W4 (D-WEB2-H) adds the session-list fields the dsh-style sidebar needs:
// title (first user message, bounded) and blank (no events yet). P6 adds
// workspace_id for the grouped sidebar view; P6.2 adds archived and sort.
type sessionView struct {
	ID          string        `json:"id"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	EventCount  int           `json:"event_count"`
	Title       string        `json:"title,omitempty"`
	Blank       bool          `json:"blank"`
	WorkspaceID string        `json:"workspace_id,omitempty"`
	Archived    bool          `json:"archived,omitempty"`
	Sort        int           `json:"sort,omitempty"`
	FlatSort    int           `json:"flat_sort,omitempty"`
	Status      SessionStatus `json:"status,omitempty"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]sessionView, 0, len(metas))
	for _, m := range metas {
		// P6.2: archived sessions leave the active sidebar list (dsh archive).
		if !m.ArchivedAt.IsZero() {
			continue
		}
		v := sessionView{ID: m.ID, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, EventCount: m.EventCount, Blank: m.EventCount == 0, WorkspaceID: m.WorkspaceID, Sort: m.Sort, FlatSort: m.FlatSort}
		canonicalTitle := ""
		if s.statusFn != nil {
			v.Status = s.statusFn(r.Context(), m)
		}
		var events []session.Event
		if m.EventCount > 0 {
			if loaded, loadErr := s.store.LoadSession(r.Context(), m.ID); loadErr == nil {
				events = loaded
				canonical, projectionErr := projection.Build(events)
				if projectionErr != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "session projection failed: " + projectionErr.Error()})
					return
				}
				metadata := canonical.SessionList
				canonicalTitle = canonical.Title
				v.Blank = metadata.Blank
				// Keep Web's sidebar ordering/activity authority identical to the
				// native projection. SQLite's session row timestamp describes the
				// latest physical append, which can be a lifecycle/transport event;
				// dsh uses the latest eligible human prompt for this projection.
				if metadata.LastPromptAt != nil {
					promptAt := time.UnixMilli(*metadata.LastPromptAt).UTC()
					if promptAt.After(v.UpdatedAt) {
						v.UpdatedAt = promptAt
					}
				}
			}
		}
		if m.Title != "" {
			// Accepted title (fallback / LLM / user rename), normalized at
			// write; re-normalize defensively for legacy rows.
			v.Title = session.NormalizeTitle(m.Title, session.TitleMaxBytes)
		} else if canonicalTitle != "" {
			v.Title = canonicalTitle
		} else if len(events) > 0 {
			// The deterministic first-prompt fallback (dsh session-title):
			// first eligible words of the first user message, byte-bounded.
			if text := session.FirstEligibleUserText(events); text != "" {
				v.Title = session.FallbackTitle(text, session.TitleFallbackMaxWords, session.TitleFallbackMaxBytes)
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionTitle implements PATCH /api/sessions/{id}/title (P2 sidebar
// rename). The request body is {"title":"..."} (UTF-8, normalized and bounded
// to session.TitleMaxBytes). A title that normalizes to empty is rejected, and
// an accepted title is recorded with the user source, which pins it against
// future automatic revisions (dsh session-title rename semantics).
func (s *Server) handleSessionTitle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct{ Title string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	title := session.NormalizeTitle(body.Title, session.TitleMaxBytes)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title-invalid"})
		return
	}
	if s.nativeSessionRenameFn != nil {
		seq, err := s.nativeSessionRenameFn(r.Context(), id, title)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "title": title, "seq": seq})
		return
	}
	// Compatibility embedders without a live runtime cannot mint a log event.
	// Production cmd/sta wires nativeSessionRenameFn, so this path is deliberately
	// storage-only and must not be treated as the canonical runtime contract.
	if err := s.store.SetSessionTitle(r.Context(), id, title, session.TitleSourceUser); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "title": title})
}

// handleSessionDelete implements DELETE /api/sessions/{id} (P2 sidebar delete).
// It removes the session and all of its events from the durable store.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// statsView is the /api/stats aggregate (D-WEB-5: a read-only in-memory rollup
// of the session log, never persisted). last_active is the newest event time,
// zero when the store holds no events.
type statsView struct {
	SessionsTotal   int            `json:"sessions_total"`
	EventsTotal     int            `json:"events_total"`
	LastActive      time.Time      `json:"last_active"`
	EventTypeCounts map[string]int `json:"event_type_counts"`
	ToolCalls       int            `json:"tool_calls"`
}

// handleStats aggregates every session's events into the dashboard view. It is
// deliberately O(all events): ListSessions then one LoadSession per session,
// summing the type counts, tool/result calls and the newest event time. Fine
// for a personal portal; a huge log would want paging/denormalization, which
// M10 accepts not adding (dispatch-m10 §M10c, 诚实记录限制).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	st := statsView{SessionsTotal: len(metas), EventTypeCounts: map[string]int{}}
	for _, m := range metas {
		events, err := s.store.LoadSession(r.Context(), m.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, ev := range events {
			st.EventsTotal++
			st.EventTypeCounts[ev.Type]++
			if ev.Type == "tool/result" {
				st.ToolCalls++
			}
			if ev.At.After(st.LastActive) {
				st.LastActive = ev.At
			}
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// maxEventImages bounds how many image refs the events API exposes per message
// (P5, aligned with the frontend default 10).
const maxEventImages = 10

// imageView is one image reference in an event's images list (P5).
type imageView struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// eventView is one event's bounded public summary (D-WEB-4: data is never
// exposed wholesale). M10 W4 (D-WEB2-H) adds the fields the dsh-style message
// stream needs: the assistant's reasoning chain (思维链), the tool-card title
// and its bounded output. P5 adds the image refs carried by user/assistant
// messages (bytes never leave the attachment store; the browser fetches them
// through the authorized echo endpoint).
type eventView struct {
	Details           map[string]any `json:"details,omitempty"`
	Command           string         `json:"command,omitempty"` // browser-side web command action
	Seq               uint64         `json:"seq"`
	Type              string         `json:"type"`
	Version           int            `json:"version"`
	Time              time.Time      `json:"time"`
	Summary           string         `json:"summary"`
	ContextMessage    bool           `json:"context_message,omitempty"` // model-only runtime/skill context; never a user bubble
	ContextSource     string         `json:"context_source,omitempty"`  // dsh source label for model-only context
	CompactionID      string         `json:"compaction_id,omitempty"`
	CompactionSummary string         `json:"compaction_summary,omitempty"`
	CompactionItems   int            `json:"compaction_items,omitempty"`
	CompactionTokens  int            `json:"compaction_tokens,omitempty"`
	CompactionError   string         `json:"compaction_error,omitempty"`
	CompactionMarker  bool           `json:"compaction_marker,omitempty"`
	Reasoning         string         `json:"reasoning,omitempty"`   // assistant/message 的思维链（有界）
	ToolName          string         `json:"tool_name,omitempty"`   // tool/start、tool/result、tool/error 的工具名
	ToolOutput        string         `json:"tool_output,omitempty"` // tool/result 的有界输出
	ToolArgs          string         `json:"tool_args,omitempty"`   // 该调用名对应的工具入参（来自 assistant 的 toolCall；只读展示用）
	CallID            string         `json:"call_id,omitempty"`     // 工具调用关联 id（tool/start → tool/result/error 配对）
	Images            []imageView    `json:"images,omitempty"`      // P5: 该消息携带的图片引用
}

// eventPageView is the cursor envelope consumed by the React/Cordis client.
type eventPageView struct {
	Events        []eventView `json:"events"`
	HasMore       bool        `json:"has_more"`
	NextBeforeSeq uint64      `json:"next_before_seq,omitempty"`
	NextAfterSeq  uint64      `json:"next_after_seq,omitempty"`
	FirstSeq      uint64      `json:"first_seq,omitempty"`
	LastSeq       uint64      `json:"last_seq,omitempty"`
}

// toEventView builds the public view for one event (bounded summary + the W4
// extra fields + the P5 image refs).
func toEventView(ev session.Event) eventView {
	contextMessage := isInternalContextMessage(ev)
	contextSource := internalContextSource(ev)
	summary := summarize(ev)
	if contextMessage {
		// Context is sent to the model, but must not cross the Web conversation
		// surface as a raw user message. Keep only the dsh source label for the
		// context-injection row; never expose the injected prompt body.
		summary = "上下文注入 " + contextSource
	}
	v := eventView{Seq: ev.Seq, Type: ev.Type, Version: ev.Version, Time: ev.At, Summary: summary, ContextMessage: contextMessage, ContextSource: contextSource}
	v.CompactionID, v.CompactionSummary, v.CompactionItems, v.CompactionTokens, v.CompactionError, v.CompactionMarker = compactionFields(ev)
	v.Reasoning, v.ToolName, v.ToolOutput = extraFields(ev)
	v.Images = extractImages(ev)
	v.CallID = callIDOf(ev)
	v.Command = commandOf(ev)
	v.Details = eventDetails(ev)
	return v
}

// isInternalContextMessage identifies durable user/message rows that are
// model-facing projections rather than human-authored conversation turns.
// Older logs do not carry a source discriminator, so the stable dsh-compatible
// framing is used as a backwards-compatible wire classification.
func isInternalContextMessage(ev session.Event) bool {
	if ev.Type != session.EventUserMessage {
		return false
	}
	var d struct {
		Text    string `json:"text"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Source *messageSource `json:"source"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return false
	}
	if d.Source != nil && (d.Source.Kind == "skill-catalog" ||
		d.Source.Kind == "skill-invocation" ||
		d.Source.Kind == "agent-instructions" ||
		d.Source.Kind == "session-reference" ||
		d.Source.Kind == "subagent-settled" ||
		d.Source.Kind == "subagent-report" ||
		(d.Source.Kind == "plugin" && d.Source.Plugin == "@shutu-ai/system-prompt")) {
		return true
	}
	text := d.Text
	if text == "" {
		for _, block := range d.Content {
			text += block.Text
		}
	}
	return strings.HasPrefix(text, "<system-reminder>\n") ||
		strings.HasPrefix(text, "Current runtime context.") ||
		strings.HasPrefix(text, "Background subagent ") ||
		strings.HasPrefix(text, "<skill_content name=\"")
}

type messageSource struct {
	Kind   string `json:"kind"`
	Plugin string `json:"plugin,omitempty"`
}

func internalContextSource(ev session.Event) string {
	if ev.Type != session.EventUserMessage {
		return ""
	}
	var d struct {
		Text    string `json:"text"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Source *messageSource `json:"source"`
	}
	if json.Unmarshal(ev.Data, &d) == nil && d.Source != nil {
		if d.Source.Kind == "skill-catalog" {
			return "skill-catalog"
		}
		if d.Source.Kind == "skill-invocation" {
			return "skill-invocation"
		}
		if d.Source.Kind == "agent-instructions" {
			return "agent-instructions"
		}
		if d.Source.Kind == "session-reference" {
			return "session-reference"
		}
		if d.Source.Kind == "subagent-settled" || d.Source.Kind == "subagent-report" {
			return d.Source.Kind
		}
		if d.Source.Kind == "plugin" && d.Source.Plugin != "" {
			return d.Source.Plugin
		}
	}
	// Backwards-compatible source attribution for logs written before source
	// metadata was added.
	text := d.Text
	if text == "" {
		for _, block := range d.Content {
			text += block.Text
		}
	}
	switch {
	case strings.HasPrefix(text, "Current runtime context."):
		return "@shutu-ai/system-prompt"
	case strings.HasPrefix(text, "<system-reminder>\n"):
		return "skill-catalog"
	case strings.HasPrefix(text, "Background subagent "):
		return "subagent-settled"
	case strings.HasPrefix(text, "<skill_content name=\""):
		return "skill-invocation"
	default:
		return ""
	}
}

// compactionFields exposes the bounded, display-oriented compaction
// projection. The replacement user/message is model history machinery, while
// the browser renders it as dsh's dedicated context row.
func compactionFields(ev session.Event) (id, summary string, items, tokens int, errText string, marker bool) {
	switch ev.Type {
	case session.EventUserMessage:
		var d struct {
			SurfaceOp *struct {
				Op string `json:"op"`
			} `json:"surfaceOp"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.SurfaceOp != nil && d.SurfaceOp.Op == "replace" {
			return "", summarize(ev), 0, 0, "", true
		}
	case session.EventCompactionSummary:
		var d struct {
			CompactionID   string  `json:"compactionId"`
			Summary        string  `json:"summary"`
			ShadowedSeqs   []int64 `json:"shadowedSeqs"`
			ShadowedTokens int     `json:"shadowedTokens"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return d.CompactionID, boundRunes(d.Summary, maxSummary), len(d.ShadowedSeqs), d.ShadowedTokens, "", false
		}
	case session.EventCompactionEnd:
		var d struct {
			CompactionID   string   `json:"compactionId"`
			ShadowedRange  [2]int64 `json:"shadowedRange"`
			ShadowedTokens int      `json:"shadowedTokens"`
			Error          string   `json:"error"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			if d.ShadowedRange[1] >= d.ShadowedRange[0] && d.ShadowedRange[0] > 0 {
				items = int(d.ShadowedRange[1] - d.ShadowedRange[0] + 1)
			}
			return d.CompactionID, "", items, d.ShadowedTokens, boundRunes(d.Error, maxSummary), false
		}
	}
	return "", "", 0, 0, "", false
}

func commandOf(ev session.Event) string {
	if ev.Type != session.EventWebCommandResult && ev.Type != session.EventCommandRun {
		return ""
	}
	var data struct {
		Command string `json:"command"`
		Name    string `json:"name"`
	}
	if json.Unmarshal(ev.Data, &data) != nil {
		return ""
	}
	if ev.Type == session.EventCommandRun {
		return data.Name
	}
	return data.Command
}

// extractImages pulls the image refs out of a user/assistant message's content
// blocks (only ref metadata — the bytes live in the attachment store). Unknown
// payloads yield nil; the frontend hides history images when absent.
type imageRefWire struct {
	ID           string `json:"ID"`
	AttachmentID string `json:"attachmentId"`
	MediaType    string `json:"mediaType"`
	LegacyType   string `json:"MediaType"`
	Bytes        int64  `json:"bytes"`
	LegacyBytes  int64  `json:"Bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Path         string `json:"Path"`
}

func extractImages(ev session.Event) []imageView {
	if ev.Type != "user/message" && ev.Type != "assistant/message" {
		return nil
	}
	var d struct {
		Content []struct {
			Type       string        `json:"type"`
			Kind       string        `json:"Kind"`
			Attachment *imageRefWire `json:"attachment"`
			Image      *imageRefWire `json:"Image"`
		} `json:"content"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return nil
	}
	var out []imageView
	for _, b := range d.Content {
		if b.Type != "image" && b.Kind != string(llm.BlockImage) {
			continue
		}
		ref := b.Attachment
		if ref == nil {
			ref = b.Image
		}
		if ref == nil {
			continue
		}
		id := ref.ID
		if id == "" {
			id = ref.AttachmentID
		}
		if ref.MediaType == "" {
			ref.MediaType = ref.LegacyType
		}
		if ref.Bytes == 0 {
			ref.Bytes = ref.LegacyBytes
		}
		out = append(out, imageView{
			ID: id, MediaType: ref.MediaType,
			Width: ref.Width, Height: ref.Height,
		})
		if len(out) >= maxEventImages {
			break
		}
	}
	return out
}

// extraFields extracts the W4 per-type fields from an event's Data blob by
// unmarshalling only the leaf JSON keys the known types carry. Unknown types
// yield empty strings (前端忽略)。
func extraFields(ev session.Event) (reasoning, toolName, toolOutput string) {
	switch ev.Type {
	case "assistant/message":
		var d struct {
			Reasoning string `json:"reasoning"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			reasoning = boundRunes(d.Reasoning, maxSummary)
		}
	case "assistant/reasoning":
		// One streamed reasoning delta: the frontend accumulates these into
		// the in-place thinking card (before the tool calls of the step).
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			reasoning = d.Text
		}
	case "tool/call", "tool/start":
		// One dispatched tool call (dsh running row): name rides tool_name,
		// args ride tool_args; no summary text.
		var d struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			toolName = d.Name
		}
	case "tool/result":
		var d struct {
			Name   string `json:"name"`
			Output string `json:"output"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			toolName = d.Name
			toolOutput = boundRunes(d.Output, maxSummary)
		}
	case "tool/error":
		var d struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			toolName = d.Name
			toolOutput = boundRunes(d.Error, maxSummary)
		}
	}
	return reasoning, toolName, toolOutput
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	before, after, limit, err := parseEventPageQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	events, hasMore, err := s.loadEventWindow(r.Context(), id, before, after, limit)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]eventView, 0, len(events))
	argsByCall := collectToolArgs(events)
	for _, ev := range events {
		v := toEventView(ev)
		if callID := callIDOf(ev); callID != "" {
			// Read-only detail field: the tool's own input, for the details panel.
			v.ToolArgs = argsByCall[callID]
		}
		out = append(out, v)
	}
	page := eventPageView{Events: out, HasMore: hasMore}
	if len(out) > 0 {
		page.FirstSeq = out[0].Seq
		page.LastSeq = out[len(out)-1].Seq
		if hasMore {
			if after != 0 {
				page.NextAfterSeq = page.LastSeq
			} else {
				page.NextBeforeSeq = page.FirstSeq
			}
		}
	}
	writeJSON(w, http.StatusOK, page)
}

const (
	defaultEventPageLimit = 100
	maxEventPageLimit     = 500
)

func parseEventPageQuery(r *http.Request) (before, after uint64, limit int, err error) {
	query := r.URL.Query()
	limit = defaultEventPageLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxEventPageLimit {
			return 0, 0, 0, fmt.Errorf("limit must be between 1 and %d", maxEventPageLimit)
		}
	}
	if raw := strings.TrimSpace(query.Get("before_seq")); raw != "" {
		before, err = strconv.ParseUint(raw, 10, 64)
		if err != nil || before == 0 {
			return 0, 0, 0, fmt.Errorf("before_seq must be a positive integer")
		}
	}
	if raw := strings.TrimSpace(query.Get("after_seq")); raw != "" {
		after, err = strconv.ParseUint(raw, 10, 64)
		if err != nil || after == 0 {
			return 0, 0, 0, fmt.Errorf("after_seq must be a positive integer")
		}
	}
	if before != 0 && after != 0 {
		return 0, 0, 0, fmt.Errorf("before_seq and after_seq are mutually exclusive")
	}
	return before, after, limit, nil
}

func (s *Server) loadEventWindow(ctx context.Context, id string, before, after uint64, limit int) ([]session.Event, bool, error) {
	return s.store.LoadSessionPage(ctx, id, before, after, limit)
}

// handleSessionExport serves a portable ZIP containing the selected session's
// append-only events. includeDescendants follows the persisted subagent
// lineage and writes each descendant under its own stable archive directory,
// matching the DSH export contract without making the browser understand the
// storage layout.
func (s *Server) handleSessionExport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sessionId is required"})
		return
	}
	includeDescendants, err := exportIncludeDescendants(r.URL.Query().Get("includeDescendants"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ids, err := s.sessionExportIDs(r.Context(), id, includeDescendants)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	attachments := make(map[string]exportAttachmentRef)
	for index, sessionID := range ids {
		events, loadErr := s.store.LoadSession(r.Context(), sessionID)
		if loadErr != nil {
			_ = zw.Close()
			if errors.Is(loadErr, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": loadErr.Error()})
			return
		}
		entryPath := path.Join("session", "events.jsonl")
		if index > 0 {
			entryPath = path.Join("subagents", safeExportSessionID(sessionID), "events.jsonl")
		}
		entry, createErr := zw.Create(entryPath)
		if createErr != nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": createErr.Error()})
			return
		}
		if writeErr := writeSessionExportEvents(entry, events); writeErr != nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": writeErr.Error()})
			return
		}
		for _, ev := range events {
			collectExportAttachmentRefs(ev.Data, attachments)
		}
	}
	attachmentIDs := make([]string, 0, len(attachments))
	for attachmentID := range attachments {
		attachmentIDs = append(attachmentIDs, attachmentID)
	}
	sort.Strings(attachmentIDs)
	for _, attachmentID := range attachmentIDs {
		if s.att == nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "session export attachment store is unavailable"})
			return
		}
		ref, readErr := s.att.GetByID(attachmentID)
		if readErr != nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": readErr.Error()})
			return
		}
		data, readErr := s.att.Read(ref)
		if readErr != nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": readErr.Error()})
			return
		}
		extension := exportAttachmentExtension(ref.MediaType)
		if extension == "" {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "session export attachment has unsupported media type"})
			return
		}
		entry, createErr := zw.Create(path.Join("media", safeExportSessionID(attachmentID)+"."+extension))
		if createErr != nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": createErr.Error()})
			return
		}
		if _, writeErr := entry.Write(data); writeErr != nil {
			_ = zw.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": writeErr.Error()})
			return
		}
	}
	if err := zw.Close(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	filename := "dsh-session-" + safeExportSessionID(id) + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf.Bytes())
}

type exportAttachmentRef struct {
	mediaType string
}

func collectExportAttachmentRefs(raw json.RawMessage, refs map[string]exportAttachmentRef) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return
	}
	var visit func(any)
	visit = func(value any) {
		switch typed := value.(type) {
		case []any:
			for _, child := range typed {
				visit(child)
			}
		case map[string]any:
			if typed["type"] == "image" {
				if attachment, ok := typed["attachment"].(map[string]any); ok {
					id, _ := attachment["attachmentId"].(string)
					if id == "" {
						id, _ = attachment["attachment_id"].(string)
					}
					mediaType, _ := attachment["mediaType"].(string)
					if mediaType == "" {
						mediaType, _ = attachment["media_type"].(string)
					}
					if id != "" {
						refs[id] = exportAttachmentRef{mediaType: mediaType}
					}
				}
			}
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
}

func exportAttachmentExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

func exportIncludeDescendants(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("includeDescendants must be a boolean")
	}
	return value, nil
}

func safeExportSessionID(id string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

func writeSessionExportEvents(w io.Writer, events []session.Event) error {
	for _, ev := range events {
		row := struct {
			Seq     uint64          `json:"seq"`
			Type    string          `json:"type"`
			Version int             `json:"version"`
			At      time.Time       `json:"at"`
			Data    json.RawMessage `json:"data"`
		}{ev.Seq, ev.Type, ev.Version, ev.At, ev.Data}
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// sessionExportIDs returns the root first, then descendants in stable ID
// order. Lineage is read from the durable subagent/start event rather than
// inferred from titles or workspace membership.
func (s *Server) sessionExportIDs(ctx context.Context, root string, includeDescendants bool) ([]string, error) {
	if _, err := s.store.GetSessionMeta(ctx, root); err != nil {
		return nil, err
	}
	if !includeDescendants {
		return []string{root}, nil
	}
	metas, err := s.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	children := make(map[string][]string)
	for _, meta := range metas {
		if meta.ID == root {
			continue
		}
		events, loadErr := s.store.LoadSession(ctx, meta.ID)
		if loadErr != nil {
			return nil, loadErr
		}
		parent, _, _ := nativeSessionLineage(events)
		if parent != "" {
			children[parent] = append(children[parent], meta.ID)
		}
	}
	for parent := range children {
		sort.Strings(children[parent])
	}
	ids := []string{root}
	seen := map[string]bool{root: true}
	var appendDescendants func(string)
	appendDescendants = func(parent string) {
		for _, child := range children[parent] {
			if seen[child] {
				continue
			}
			seen[child] = true
			ids = append(ids, child)
			appendDescendants(child)
		}
	}
	appendDescendants(root)
	return ids, nil
}

func (s *Server) feedbackStore(w http.ResponseWriter) (store.MessageFeedbackStore, bool) {
	fs, ok := s.store.(store.MessageFeedbackStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "feedback persistence unavailable"})
		return nil, false
	}
	return fs, true
}

func (s *Server) handleFeedbackList(w http.ResponseWriter, r *http.Request) {
	fs, ok := s.feedbackStore(w)
	if !ok {
		return
	}
	items, err := fs.ListMessageFeedback(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleFeedbackPut(w http.ResponseWriter, r *http.Request) {
	fs, ok := s.feedbackStore(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid feedback sequence"})
		return
	}
	var body struct {
		Rating string `json:"rating"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if body.Rating != "positive" && body.Rating != "negative" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "rating must be positive or negative"})
		return
	}
	if len([]byte(body.Note)) > store.MaxMessageFeedbackNoteBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "feedback note is too large"})
		return
	}
	if err := s.requireFeedbackTarget(r.Context(), id, seq); err != nil {
		s.writeFeedbackError(w, err)
		return
	}
	item, err := fs.PutMessageFeedback(r.Context(), id, seq, body.Rating, body.Note)
	if err != nil {
		s.writeFeedbackError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleFeedbackDelete(w http.ResponseWriter, r *http.Request) {
	fs, ok := s.feedbackStore(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid feedback sequence"})
		return
	}
	if err := fs.DeleteMessageFeedback(r.Context(), id, seq); err != nil {
		s.writeFeedbackError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) requireFeedbackTarget(ctx context.Context, sessionID string, seq uint64) error {
	events, err := s.store.LoadSession(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Seq == seq && ev.Type == session.EventAssistantMessage {
			return nil
		}
	}
	return fmt.Errorf("feedback target %d not found", seq)
}

func (s *Server) writeFeedbackError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if strings.HasPrefix(err.Error(), "feedback target ") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

// callIDOf returns the correlation id of a tool/start, tool/result or
// tool/error event; empty otherwise. It unmarshals only the leaf callId key.
func callIDOf(ev session.Event) string {
	switch ev.Type {
	case "tool/call", "tool/start", "tool/result", "tool/error":
		var d struct {
			CallID string `json:"callId"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return d.CallID
		}
	}
	return ""
}

// collectToolArgs builds the callID → args map from every assistant/message
// event's toolCalls, so the details panel can show a tool's input alongside its
// output without changing the session log format (the args are the model's own
// tool call, already durable in the assistant event).
func collectToolArgs(events []session.Event) map[string]string {
	args := make(map[string]string)
	for _, ev := range events {
		collectToolArgsInto(args, ev)
	}
	return args
}

// collectToolArgsInto folds one assistant/message event's toolCalls into a
// running callID → args map (the live-stream counterpart of collectToolArgs).
func collectToolArgsInto(args map[string]string, ev session.Event) {
	if ev.Type != "assistant/message" {
		return
	}
	var d struct {
		ToolCalls []struct {
			ID        string
			Arguments string
		} `json:"toolCalls"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return
	}
	for _, tc := range d.ToolCalls {
		if tc.ID != "" {
			args[tc.ID] = tc.Arguments
		}
	}
}

// handleSessionCreate implements POST /api/sessions (M10 W1, ADR D-WEB2-C):
// it asks the injected session manager to start a fresh session and returns
// its id. An unwired manager answers 501. P6 adds an optional {"workspace_id"}
// so a session can be created directly into a sidebar group. Phase 2 adds the
// optional per-session {"agent_preset","model","permission"} overrides, which
// are stored on the session (mode is locked from then on).
func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if s.sessFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "session manager not wired"})
		return
	}
	var body struct {
		WorkspaceID string `json:"workspace_id"`
		AgentPreset string `json:"agent_preset"`
		Model       string `json:"model"`
		Permission  string `json:"permission"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body) // optional
	if body.AgentPreset != "" && !validMode(body.AgentPreset) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown agent_preset: " + body.AgentPreset})
		return
	}
	if body.Permission != "" && !validPermission(body.Permission) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown permission: " + body.Permission})
		return
	}
	// dsh connectWorkspace reuses an existing, unarchived blank session that
	// already belongs to the selected workspace. This prevents repeated clicks
	// on a workspace row or picker from creating hidden duplicate sessions.
	id := ""
	var err error
	if body.WorkspaceID != "" {
		metas, listErr := s.store.ListSessions(r.Context())
		if listErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": listErr.Error()})
			return
		}
		for _, m := range metas {
			if m.ID == "" || m.WorkspaceID != body.WorkspaceID || m.EventCount != 0 || !m.ArchivedAt.IsZero() {
				continue
			}
			// A persisted cwd is authoritative when available. Legacy test
			// doubles and old rows may not have one, so membership remains the
			// fallback for those records.
			if m.CWD != "" {
				workspaceCWD, cwdErr := s.workspaceWorkdir(r.Context(), body.WorkspaceID)
				if cwdErr != nil || !sameWorkspacePath(m.CWD, workspaceCWD) {
					continue
				}
			}
			id = m.ID
			break
		}
	}
	if id == "" {
		id, err = s.sessFn(r.Context(), "new", "")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if body.WorkspaceID != "" {
		if err := s.store.SetSessionWorkspace(r.Context(), id, body.WorkspaceID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if body.WorkspaceID != "" {
		if err := s.syncSessionCWD(r.Context(), id, body.WorkspaceID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	cfg := store.SessionConfig{AgentPreset: body.AgentPreset, Model: body.Model, Permission: body.Permission}
	if cfg.AgentPreset != "" || cfg.Model != "" || cfg.Permission != "" {
		if scs, ok := s.store.(store.SessionConfigStore); ok {
			if err := scs.SetSessionConfig(r.Context(), id, cfg); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "workspace_id": body.WorkspaceID,
		"agent_preset": body.AgentPreset, "model": body.Model, "permission": body.Permission,
	})
}

// sessionDefaultWorkdir returns the configured fallback, resolving it once at
// the API boundary so persisted session headers are always absolute. With no
// override it creates and uses <user-home>/shutu.
func (s *Server) sessionDefaultWorkdir() (string, error) {
	dir := strings.TrimSpace(s.defaultWorkdir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", fmt.Errorf("webserver: resolve user home: %w", err)
		}
		dir = filepath.Join(home, "shutu")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("webserver: create default workdir %q: %w", dir, err)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("webserver: resolve workdir: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("webserver: workdir %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("webserver: workdir %q is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

// workspaceWorkdir resolves a workspace id to its persisted directory. Legacy
// title-only workspaces intentionally use the same default as ungrouped.
func (s *Server) workspaceWorkdir(ctx context.Context, workspaceID string) (string, error) {
	if workspaceID == "" {
		return s.sessionDefaultWorkdir()
	}
	workspaces, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	for _, ws := range workspaces {
		if ws.ID != workspaceID {
			continue
		}
		if strings.TrimSpace(ws.Path) == "" {
			return s.sessionDefaultWorkdir()
		}
		abs, err := filepath.Abs(ws.Path)
		if err != nil {
			return "", fmt.Errorf("webserver: resolve workspace %q path: %w", workspaceID, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("webserver: workspace %q path %q: %w", workspaceID, abs, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("webserver: workspace %q path %q is not a directory", workspaceID, abs)
		}
		return filepath.Clean(abs), nil
	}
	return "", fmt.Errorf("workspace not found: %s", workspaceID)
}

// syncSessionCWD keeps the durable session header aligned with its workspace,
// mirroring dsh's invariant that tools resolve against session.header.cwd.
func (s *Server) syncSessionCWD(ctx context.Context, sessionID, workspaceID string) error {
	hs, ok := s.store.(store.SessionHeaderStore)
	if !ok {
		return nil
	}
	cwd, err := s.workspaceWorkdir(ctx, workspaceID)
	if err != nil {
		return err
	}
	return hs.SetSessionCWD(ctx, sessionID, cwd)
}

// sameWorkspacePath compares persisted and resolved workspace directories in
// the platform form used by the file system. Windows paths are case-insensitive
// while POSIX paths preserve case.
func sameWorkspacePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// handleSessionConfigGet implements GET /api/sessions/{id}/config: the raw
// per-session overrides (empty values mean "fall back to the global config").
func (s *Server) handleSessionConfigGet(w http.ResponseWriter, r *http.Request) {
	scs, ok := s.store.(store.SessionConfigStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "per-session config not supported"})
		return
	}
	cfg, err := scs.GetSessionConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, configView(cfg))
}

// handleSessionConfigPatch implements PATCH /api/sessions/{id}/config: it
// rewrites provider, model, reasoning effort and permission only (the mode is
// locked at creation). The provider+model pair is the dsh ModelSelection of
// the session — the runtime routes the session's turns through that provider.
// It returns the updated overrides.
func (s *Server) handleSessionConfigPatch(w http.ResponseWriter, r *http.Request) {
	scs, ok := s.store.(store.SessionConfigStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "per-session config not supported"})
		return
	}
	var body struct {
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		Permission      string `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.Permission != "" && !validPermission(body.Permission) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown permission: " + body.Permission})
		return
	}
	// dsh ModelSelect 思考强度: "" clears back to the provider default; the
	// levels mirror the deepseek wire (off|low|high|max).
	if !validEffort(body.ReasoningEffort) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown reasoning_effort: " + body.ReasoningEffort})
		return
	}
	if err := scs.UpdateSessionConfig(r.Context(), r.PathValue("id"), body.Provider, body.Model, body.ReasoningEffort, body.Permission); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	cfg, err := scs.GetSessionConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, configView(cfg))
}

// configView is the JSON shape of per-session overrides.
func configView(cfg store.SessionConfig) map[string]any {
	return map[string]any{
		"agent_preset":     cfg.AgentPreset,
		"provider":         cfg.Provider,
		"model":            cfg.Model,
		"reasoning_effort": cfg.ReasoningEffort,
		"permission":       cfg.Permission,
	}
}

// validMode reports whether m is a known mode preset id.
func validMode(m string) bool { return m == "minimal" || m == "standard" || m == "code" }

// validPermission reports whether p is a known permission tier id.
func validPermission(p string) bool { return p == "readonly" || p == "standard" || p == "full" }

// validEffort reports whether e is a known reasoning-effort level ("" clears
// the selection back to the provider default; dsh ModelSelect 思考强度).
func validEffort(e string) bool {
	return e == "" || e == "off" || e == "low" || e == "high" || e == "max"
}

// handleSessionResume implements POST /api/sessions/{id}/resume: it asks the
// injected session manager to resume the session and returns its id.
func (s *Server) handleSessionResume(w http.ResponseWriter, r *http.Request) {
	if s.sessFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "session manager not wired"})
		return
	}
	id := r.PathValue("id")
	newID, err := s.sessFn(r.Context(), "resume", id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": newID})
}

// handleSessionFork implements POST /api/sessions/{id}/fork (P6.2, dsh fork):
// the session's full event log is cloned into a brand-new session in the same
// workspace, so the user gets a diverging copy. The clone keeps the title.
func (s *Server) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	events, err := s.store.LoadSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Production SQLite exposes a transaction that reads the parent revision,
	// closed seed and all inherited metadata before publishing the child. Keep
	// the older sequence below only for lightweight Store implementations that
	// do not provide this capability; otherwise Create→Append→SetMetadata would
	// expose a partially forked session after a crash.
	if atomic, ok := s.store.(store.SessionForkStore); ok {
		boundary := uint64(0)
		if len(events) > 0 {
			boundary = events[len(events)-1].Seq
		}
		forkID, err := store.GenerateReservedID(r.Context(), s.store, "session", newSessionID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if err := atomic.ForkSessionWithOptions(r.Context(), id, forkID, boundary, store.SessionForkOptions{
			InheritParentMetadata: true,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.notifyNativeMuxSessionAdded(forkID)
		writeJSON(w, http.StatusOK, map[string]any{"id": forkID})
		return
	}
	// Carry the source title and workspace membership over to the clone.
	srcTitle, srcTitleSource, srcWorkspace, srcCWD := "", "", "", ""
	if all, err := s.store.ListSessions(r.Context()); err == nil {
		for _, m := range all {
			if m.ID == id {
				srcTitle, srcTitleSource, srcWorkspace, srcCWD = m.Title, m.TitleSource, m.WorkspaceID, m.CWD
				break
			}
		}
	}
	forkID, err := store.GenerateReservedID(r.Context(), s.store, "session", newSessionID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.CreateSession(r.Context(), forkID, time.Now().UTC()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Clone the log with re-sequenced events (the store appends in order).
	cloned := make([]session.Event, len(events))
	for i, e := range events {
		c := e
		c.Seq = uint64(i + 1)
		cloned[i] = c
	}
	if err := s.store.AppendEvents(r.Context(), forkID, cloned); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if srcTitle != "" {
		_ = s.store.SetSessionTitle(r.Context(), forkID, srcTitle, srcTitleSource)
	}
	if srcWorkspace != "" {
		_ = s.store.SetSessionWorkspace(r.Context(), forkID, srcWorkspace)
	}
	if hs, ok := s.store.(store.SessionHeaderStore); ok {
		if srcCWD == "" {
			if resolved, resolveErr := s.workspaceWorkdir(r.Context(), srcWorkspace); resolveErr == nil {
				srcCWD = resolved
			}
		}
		if srcCWD != "" {
			_ = hs.SetSessionCWD(r.Context(), forkID, srcCWD)
		}
	}
	// The fork inherits the source's per-session overrides (dsh fork: the
	// clone keeps the parent's agent preset / model / permission), so the
	// diverging copy starts from the same mode.
	if scs, ok := s.store.(store.SessionConfigStore); ok {
		if cfg, err := scs.GetSessionConfig(r.Context(), id); err == nil {
			_ = scs.SetSessionConfig(r.Context(), forkID, cfg)
		}
	}
	s.notifyNativeMuxSessionAdded(forkID)
	writeJSON(w, http.StatusOK, map[string]any{"id": forkID})
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

// handleSessionArchive implements POST /api/sessions/{id}/archive (P6.2):
// the session leaves the active sidebar tree; the log is preserved.
func (s *Server) handleSessionArchive(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, true)
}

// handleSessionUnarchive implements POST /api/sessions/{id}/unarchive.
func (s *Server) handleSessionUnarchive(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, false)
}

func (s *Server) setArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	id := r.PathValue("id")
	if err := s.store.ArchiveSession(r.Context(), id, archived); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "archived": archived})
}

// handleSessionsOrder implements PATCH /api/sessions/order
// {"workspace_id":..., "session_ids":[...]} (P6.2 drag & drop): every listed
// session moves into the target workspace (empty = ungrouped) and takes the
// manual sort order 0..n-1, so the grouped tree follows the drop.
func (s *Server) handleSessionsOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkspaceID string   `json:"workspace_id"`
		SessionIDs  []string `json:"session_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	if err := s.store.ReorderSessions(r.Context(), body.WorkspaceID, body.SessionIDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, id := range body.SessionIDs {
		if err := s.syncSessionCWD(r.Context(), id, body.WorkspaceID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSessionsFlatOrder implements PATCH /api/sessions/flat-order
// {"ids":[...]} (P6.3 flat-view drag): flat_sort is rewritten 0..n-1.
func (s *Server) handleSessionsFlatOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	if err := s.store.ReorderSessionsFlat(r.Context(), body.IDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// searchHitView is the owned search result the frontend renders as a preview
// row (title + snippet + time).
type searchHitView struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Snippet   string    `json:"snippet"`
}

type sessionFileView struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mod_time,omitempty"`
}

func (s *Server) sessionWorkdir(ctx context.Context, sessionID string) (string, error) {
	meta, err := s.store.GetSessionMeta(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(meta.CWD) != "" {
		abs, err := filepath.Abs(meta.CWD)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("session workdir is unavailable")
		}
		return filepath.Clean(abs), nil
	}
	return s.workspaceWorkdir(ctx, meta.WorkspaceID)
}

func safeSessionPath(root, relative string) (string, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	relative = filepath.Clean(strings.TrimSpace(relative))
	if relative == "." || relative == "" {
		return root, "", nil
	}
	if filepath.IsAbs(relative) {
		return "", "", errors.New("absolute file paths are not allowed")
	}
	target := filepath.Clean(filepath.Join(root, relative))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("file path escapes workspace")
	}
	return target, filepath.ToSlash(rel), nil
}

func (s *Server) handleSessionFiles(w http.ResponseWriter, r *http.Request) {
	root, err := s.sessionWorkdir(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	relDir := r.URL.Query().Get("path")
	target, rel, err := safeSessionPath(root, relDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	entries := make([]sessionFileView, 0, 64)
	if query != "" {
		needle := strings.ToLower(query)
		walkErr := filepath.WalkDir(root, func(pathName string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if len(entries) >= 200 || d.IsDir() {
				return nil
			}
			relPath, relErr := filepath.Rel(root, pathName)
			if relErr != nil || !strings.Contains(strings.ToLower(filepath.ToSlash(relPath)), needle) {
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			entries = append(entries, sessionFileView{Name: filepath.Base(pathName), Path: filepath.ToSlash(relPath), Size: info.Size(), ModTime: info.ModTime()})
			return nil
		})
		if walkErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": walkErr.Error()})
			return
		}
	} else {
		children, readErr := os.ReadDir(target)
		if readErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": readErr.Error()})
			return
		}
		for _, child := range children {
			if strings.HasPrefix(child.Name(), ".") {
				continue
			}
			info, infoErr := child.Info()
			if infoErr != nil {
				continue
			}
			childRel := filepath.ToSlash(filepath.Join(rel, child.Name()))
			entries = append(entries, sessionFileView{Name: child.Name(), Path: childRel, Dir: child.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Dir != entries[j].Dir {
				return entries[i].Dir
			}
			return entries[i].Name < entries[j].Name
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"root": root, "path": rel, "entries": entries})
}

func (s *Server) handleSessionFile(w http.ResponseWriter, r *http.Request) {
	root, err := s.sessionWorkdir(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	target, rel, err := safeSessionPath(root, r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a relative file path is required"})
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "file not found"})
		return
	}
	if info.Size() > 512<<10 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "file preview is limited to 512 KiB"})
		return
	}
	data, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	lines := strings.Split(string(data), "\n")
	start := 1
	end := len(lines)
	if raw := r.URL.Query().Get("start"); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil && parsed > 0 {
			start = parsed
		}
	}
	if raw := r.URL.Query().Get("end"); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil && parsed >= start {
			end = parsed
		}
	}
	if start > len(lines) {
		start = len(lines)
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": rel, "content": strings.Join(lines[start-1:end], "\n"),
		"start_line": start, "end_line": end, "total_lines": len(lines),
	})
}

// handleSearch implements GET /api/search?q=... (P6.3): sessions whose event
// bodies contain the query, each with the first matching snippet.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []searchHitView{}})
		return
	}
	hits, err := s.store.SearchSessions(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]searchHitView, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHitView{ID: h.SessionID, Title: h.Title, UpdatedAt: h.UpdatedAt, Snippet: boundRunes(h.Snippet, maxSummary)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": out})
}

// handleWorkspacesOrder implements PATCH /api/workspaces/order {"ids":[...]}
// (P6.2 drag & drop of workspace rows): sort is rewritten 0..n-1.
func (s *Server) handleWorkspacesOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	if err := s.store.ReorderWorkspaces(r.Context(), body.IDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maxWorkspaceTitle is the rune cap on workspace titles (P6).
const maxWorkspaceTitle = 60

// workspaceView is one workspace plus its session ids, ordered by recent
// activity (the sidebar groups them under the workspace header).
type workspaceView struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Path       string   `json:"path,omitempty"`
	SessionIDs []string `json:"session_ids"`
	CreatedAt  int64    `json:"created_at"`
}

// handleWorkspaces implements GET /api/workspaces (P6): every workspace with
// its session ids (recently updated first) plus the ungrouped session ids, so
// the sidebar can render the dsh-style grouped tree without re-deriving
// membership.
func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	metaByID := map[string]store.SessionMeta{}
	byWorkspace := map[string][]string{}
	var ungrouped []string
	// Archived sessions leave every group (dsh archive) — they stay in the DB
	// for future restore, but the active tree ignores them.
	for _, m := range metas {
		if !m.ArchivedAt.IsZero() {
			continue
		}
		metaByID[m.ID] = m
		if m.WorkspaceID == "" {
			ungrouped = append(ungrouped, m.ID)
		} else {
			byWorkspace[m.WorkspaceID] = append(byWorkspace[m.WorkspaceID], m.ID)
		}
	}
	// A group's session order follows the manual drag (Sort asc, then recent
	// activity). ReorderSessions rewrites the whole dragged group, so an
	// untouched group (all Sort 0) degrades to updated_at DESC here.
	orderGroup := func(ids []string) {
		sort.SliceStable(ids, func(i, j int) bool {
			a, b := metaByID[ids[i]], metaByID[ids[j]]
			if a.Sort != b.Sort {
				return a.Sort < b.Sort
			}
			return a.UpdatedAt.After(b.UpdatedAt)
		})
	}
	orderGroup(ungrouped)
	out := make([]workspaceView, 0, len(ws))
	for _, m := range ws {
		ids := byWorkspace[m.ID]
		orderGroup(ids)
		if ids == nil {
			ids = []string{} // always a JSON array, never null
		}
		created := int64(0)
		if !m.CreatedAt.IsZero() {
			created = m.CreatedAt.UnixMilli()
		}
		out = append(out, workspaceView{ID: m.ID, Title: m.Title, Path: m.Path, SessionIDs: ids, CreatedAt: created})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out, "ungrouped_ids": ungrouped})
}

// newWorkspaceID returns a short random workspace id (e.g. "w-1a2b3c4d").
func newWorkspaceID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "w-" + hex.EncodeToString(b[:]), nil
}

// handleWorkspaceCreate implements POST /api/workspaces {"title","path"}.
func (s *Server) handleWorkspaceCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		Path  string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	title := strings.TrimSpace(boundRunes(body.Title, maxWorkspaceTitle))
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}
	id, err := newWorkspaceID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	workspacePath := strings.TrimSpace(body.Path)
	if workspacePath == "" {
		workspacePath, err = s.sessionDefaultWorkdir()
	} else {
		workspacePath, err = filepath.Abs(workspacePath)
		if err == nil {
			info, statErr := os.Stat(workspacePath)
			if statErr != nil {
				err = fmt.Errorf("workspace path %q: %w", workspacePath, statErr)
			} else if !info.IsDir() {
				err = fmt.Errorf("workspace path %q is not a directory", workspacePath)
			}
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if ps, ok := s.store.(store.WorkspacePathStore); ok {
		err = ps.CreateWorkspaceWithPath(r.Context(), id, title, filepath.Clean(workspacePath))
	} else {
		err = s.store.CreateWorkspace(r.Context(), id, title)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "title": title, "path": workspacePath})
}

// handleWorkspacePickDirectory opens a native directory chooser on the host
// running the Web server. A browser upload control cannot expose the host's
// absolute path, while dsh's directory flow returns that path to the server.
func (s *Server) handleWorkspacePickDirectory(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "native directory picker is available on Windows only; enter the path manually"})
		return
	}
	const script = `Add-Type -AssemblyName System.Windows.Forms; $d=New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description='Select workspace directory'; $d.ShowNewFolderButton=$true; if($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK){[Console]::Write($d.SelectedPath)}`
	cmd := exec.CommandContext(r.Context(), "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		if r.Context().Err() != nil {
			writeJSON(w, http.StatusRequestTimeout, map[string]any{"error": "directory picker canceled"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("open directory picker: %v", err)})
		return
	}
	selected := strings.TrimSpace(string(out))
	if selected == "" {
		writeJSON(w, http.StatusOK, map[string]any{"path": ""})
		return
	}
	abs, err := filepath.Abs(selected)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": filepath.Clean(abs)})
}

type workspaceDirectoryEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Hidden bool   `json:"hidden"`
}

type workspaceDirectoryListing struct {
	Path      string                    `json:"path"`
	Home      string                    `json:"home"`
	Crumbs    []workspaceDirectoryEntry `json:"crumbs"`
	Entries   []workspaceDirectoryEntry `json:"entries"`
	ReadError string                    `json:"read_error,omitempty"`
	Truncated bool                      `json:"truncated"`
}

// handleWorkspaceDirectoryList implements the dsh directory-browser read
// used by the workspace picker. The browser receives absolute paths from the
// server and never joins path segments in JavaScript.
func (s *Server) handleWorkspaceDirectoryList(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		var err error
		// Start at the same directory used by ungrouped sessions and workspaces
		// without an explicit path. Enumerating the user's home root is not
		// reliable on Windows (it may be ACL-protected), while ~/shutu is the
		// application-owned default and is created by sessionDefaultWorkdir.
		path, err = s.sessionDefaultWorkdir()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "resolve default workspace directory: " + err.Error()})
			return
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "resolve directory: " + err.Error()})
		return
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("directory %q: %v", abs, err)})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("directory %q is not a directory", abs)})
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsPermission(err) {
			// The current directory is still a valid selection even when the
			// account cannot enumerate its children. Keep the path usable so the
			// user can choose it or type a child path manually.
			home, _ := os.UserHomeDir()
			writeJSON(w, http.StatusOK, workspaceDirectoryListing{
				Path: abs, Home: home, Crumbs: workspaceDirectoryCrumbs(abs),
				ReadError: fmt.Sprintf("无法列出子目录：%v；仍可选择当前目录或输入完整路径", err),
			})
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": fmt.Sprintf("read directory %q: %v", abs, err)})
		return
	}
	children := make([]workspaceDirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		childInfo, infoErr := entry.Info()
		if infoErr != nil || !childInfo.IsDir() {
			continue
		}
		name := entry.Name()
		children = append(children, workspaceDirectoryEntry{
			Name: name, Path: filepath.Join(abs, name), Hidden: strings.HasPrefix(name, "."),
		})
	}
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, workspaceDirectoryListing{
		Path: abs, Home: home, Crumbs: workspaceDirectoryCrumbs(abs), Entries: children,
	})
}

// handleWorkspaceDirectoryCreate implements the dsh directory-browser
// New-folder action. Creation is deliberately one level deep beneath the
// directory currently shown by the picker.
func (s *Server) handleWorkspaceDirectoryCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	parent, err := filepath.Abs(strings.TrimSpace(body.Path))
	if err != nil || strings.TrimSpace(body.Path) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent directory is required"})
		return
	}
	parent = filepath.Clean(parent)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("parent directory %q is unavailable", parent)})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "folder name must be one path segment"})
		return
	}
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0o755); err != nil {
		status := http.StatusInternalServerError
		if os.IsExist(err) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": fmt.Sprintf("create directory %q: %v", target, err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": filepath.Clean(target)})
}

func workspaceDirectoryCrumbs(path string) []workspaceDirectoryEntry {
	var reversed []workspaceDirectoryEntry
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		name := filepath.Base(current)
		if parent := filepath.Dir(current); parent == current {
			name = current
		}
		reversed = append(reversed, workspaceDirectoryEntry{Name: name, Path: current})
		if filepath.Dir(current) == current {
			break
		}
	}
	crumbs := make([]workspaceDirectoryEntry, len(reversed))
	for i := range reversed {
		crumbs[len(reversed)-1-i] = reversed[i]
	}
	return crumbs
}

// handleWorkspaceTitle implements PATCH /api/workspaces/{id} {"title":...}.
func (s *Server) handleWorkspaceTitle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct{ Title string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	title := strings.TrimSpace(boundRunes(body.Title, maxWorkspaceTitle))
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}
	if err := s.store.SetWorkspaceTitle(r.Context(), id, title); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "workspace not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "title": title})
}

// handleWorkspaceDelete implements DELETE /api/workspaces/{id}: the workspace
// is removed and its sessions return to the ungrouped bucket (store-owned).
func (s *Server) handleWorkspaceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var moved []string
	if metas, err := s.store.ListSessions(r.Context()); err == nil {
		for _, m := range metas {
			if m.WorkspaceID == id {
				moved = append(moved, m.ID)
			}
		}
	}
	if err := s.store.DeleteWorkspace(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "workspace not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, sessionID := range moved {
		if err := s.syncSessionCWD(r.Context(), sessionID, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleMessage implements POST /api/sessions/{id}/message (M10 W1, ADR
// D-WEB2-A): it dispatches one user message to the injected handler, which runs
// the turn (the streaming process arrives on the SSE stream). The response 200
// {"ok":true} means the Run has completed. P5 extends the body with an optional
// images list (attachment ids → ImageRef, resolved through the attachment
// store). An empty text without images answers 400; an unwired handler answers
// 501.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if s.msgFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "message handler not wired"})
		return
	}
	id := r.PathValue("id")
	var body struct {
		Text   string   `json:"text"`
		Images []string `json:"images"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.Text) == "" && len(body.Images) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text is required"})
		return
	}
	var images []llm.ImageRef
	if len(body.Images) > 0 {
		if s.att == nil {
			writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "images not supported"})
			return
		}
		for _, imgID := range body.Images {
			ref, err := s.att.GetByID(imgID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image " + imgID + " not found"})
				return
			}
			images = append(images, ref)
		}
	}
	if err := s.msgFn(r.Context(), id, body.Text, images, PromptMeta{}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleQueueList implements GET /api/sessions/{id}/queue. The queue is
// process-owned by cmd/sta, so an unwired generic server answers 501.
func (s *Server) handleQueueList(w http.ResponseWriter, r *http.Request) {
	if s.queueListFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "queue manager not wired"})
		return
	}
	items, err := s.queueListFn(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if items == nil {
		items = []QueueItem{}
	}
	writeJSON(w, http.StatusOK, items)
}

// handleQueueEnqueue implements POST /api/sessions/{id}/queue. The active
// turn keeps running; the composition root drains this item after completion.
func (s *Server) handleQueueEnqueue(w http.ResponseWriter, r *http.Request) {
	if s.queueEnqueueFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "queue manager not wired"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text is required"})
		return
	}
	item, err := s.queueEnqueueFn(r.Context(), r.PathValue("id"), body.Text, []llm.ContentBlock{llm.Text(body.Text)}, PromptMeta{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.notifyNativeMux(r.PathValue("id"))
	writeJSON(w, http.StatusAccepted, item)
}

// handleQueueUpdate implements PATCH /api/sessions/{id}/queue/{itemID}.
// Supported actions mirror the useful dsh controls: move_first, delete and
// steer (cancel the active turn, then promote the item).
func (s *Server) handleQueueUpdate(w http.ResponseWriter, r *http.Request) {
	if s.queueUpdateFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "queue manager not wired"})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	action := strings.TrimSpace(body.Action)
	if action != "move_first" && action != "delete" && action != "steer" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action must be move_first, delete, or steer"})
		return
	}
	if err := s.queueUpdateFn(r.Context(), r.PathValue("id"), r.PathValue("itemID"), action); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.notifyNativeMux(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maxWebImageBytes caps a single uploaded image via the web portal and native
// prompt admission. DSH's default limit is 5 MiB; the backend still verifies
// every image so an oversized or lying browser payload is admitted nowhere.
const maxWebImageBytes = 5 << 20

// attachmentView is the POST /api/sessions/{id}/attachments response.
type attachmentView struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// handleModelSwitch implements POST /api/config/model (P5.1, 模型选择实时生效):
// it dispatches a live provider/model/reasoning-effort change to the injected
// switcher, which rebuilds the selected LLM provider (no restart). An unwired
// switcher answers 501; an empty body (neither provider nor model) answers 400;
// a rejected switch (unknown provider / missing key / bad model or effort)
// answers 400 and keeps the previous selection. On success it returns the
// refreshed config view so the frontend re-renders the settings panel.
func (s *Server) handleModelSwitch(w http.ResponseWriter, r *http.Request) {
	if s.setModelFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "model switcher not wired"})
		return
	}
	var body struct {
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.Provider) == "" && strings.TrimSpace(body.Model) == "" && strings.TrimSpace(body.ReasoningEffort) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider or model is required"})
		return
	}
	if err := s.setModelFn(r.Context(), strings.TrimSpace(body.Provider), strings.TrimSpace(body.Model), strings.TrimSpace(body.ReasoningEffort)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProviderSave implements POST /api/config/provider (M11, 增加提供方 /
// 增加自定义提供方): a built-in provider edit only carries an API-key override
// (custom:false, api_key empty removes the override back to the env var); a
// custom provider edit (custom:true) carries the full OpenAI-compatible
// profile (id/name/base_url/model) plus an optional key override. An unwired
// manager answers 501; a rejected edit (unknown id / invalid custom route /
// missing profile fields) answers 400 and leaves the registry untouched.
func (s *Server) handleProviderSave(w http.ResponseWriter, r *http.Request) {
	if s.setProviderFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "provider manager not wired"})
		return
	}
	var body ProviderEdit
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider id is required"})
		return
	}
	if err := s.setProviderFn(r.Context(), "save", body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProviderDelete implements DELETE /api/config/provider (M11): removes a
// custom provider declaration and its key override. Built-in providers are
// rejected by the manager. An unwired manager answers 501.
func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	if s.setProviderFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "provider manager not wired"})
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider id is required"})
		return
	}
	if err := s.setProviderFn(r.Context(), "delete", ProviderEdit{ID: body.ID}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProviderDiscover implements POST /api/config/provider/discover
// (M11-pi-ai, 获取可用模型): it asks the endpoint the form currently shows which
// models it advertises and returns candidate rows for the user to pick from.
// The reply is candidate metadata only — never written behind the user. An
// unwired discoverer answers 501.
// handleSkillsList implements GET /api/config/skills — the skill-page boot
// fetch (list + groups in one round trip). It answers 501 when the skill
// manager is not wired, else the dispatcher's JSON view.
func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	if s.skillsFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "skill management not wired"})
		return
	}
	result, err := s.skillsFn(r.Context(), "list", SkillRequest{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSkills implements POST /api/config/skills — every skill-page action
// (content/set_enabled/delete/add/migrate/group_save/group_delete), selected by
// the "action" body field. It answers 501 when the skill manager is not wired.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if s.skillsFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "skill management not wired"})
		return
	}
	var body struct {
		Action string `json:"action"`
		SkillRequest
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if body.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action required"})
		return
	}
	result, err := s.skillsFn(r.Context(), body.Action, body.SkillRequest)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleProviderDiscover(w http.ResponseWriter, r *http.Request) {
	if s.setDiscoverFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "provider discovery not wired"})
		return
	}
	var body ProviderDiscover
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	models, err := s.setDiscoverFn(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if models == nil {
		models = []ProviderModel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
func (s *Server) handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if s.att == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "attachment store not wired"})
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.LoadSession(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad multipart form"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file field required"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxWebImageBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read file failed"})
		return
	}
	// Media type: prefer the filename extension, fall back to content sniffing.
	mediaType := attachment.MediaTypeForExtension(strings.ToLower(filepath.Ext(header.Filename)))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	ref, err := s.att.SaveImage(mediaType, data, maxWebImageBytes)
	if err != nil {
		switch {
		case errors.Is(err, attachment.ErrUnsupportedType):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported type"})
		case errors.Is(err, attachment.ErrEmptyData):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty"})
		case errors.Is(err, attachment.ErrTooLarge):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "too large"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusCreated, attachmentView{
		ID: ref.ID, MediaType: ref.MediaType, Bytes: ref.Bytes,
		Width: ref.Width, Height: ref.Height,
	})
}

// handleAttachmentGet implements GET /api/sessions/{id}/attachments/{attID}
// (P5): it echoes the stored image bytes with their Content-Type for the
// browser <img> / lightbox. 404 when the session or attachment is unknown.
func (s *Server) handleAttachmentGet(w http.ResponseWriter, r *http.Request) {
	if s.att == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "attachment store not wired"})
		return
	}
	if _, err := s.store.LoadSession(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	ref, err := s.att.GetByID(r.PathValue("attID"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "attachment not found"})
		return
	}
	data, err := s.att.Read(ref)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "attachment not readable"})
		return
	}
	w.Header().Set("Content-Type", ref.MediaType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleEventStream implements GET /api/sessions/{id}/events/stream — the SSE
// real-time event flow (M10 W1, ADR D-WEB2-B): it subscribes the injected event
// source before replaying the session's stored events as frames (snapshot),
// queues concurrent events during replay, and then forwards every new event as
// a frame. Each frame is
// `id: <seq>\ndata: {seq,type,time,summary}\n\n` and is flushed immediately
// (http.Flusher). The handler returns when the request context is cancelled
// (client disconnect), unsubscribing the event source. It does not use
// writeJSON once the stream has started.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.evSrc == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "event source not wired"})
		return
	}
	resumeSeq, err := streamResumeSeq(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	allEvents, err := s.store.LoadSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	events := allEvents
	if resumeSeq != 0 {
		start := 0
		for start < len(events) && events[start].Seq <= resumeSeq {
			start++
		}
		events = events[start:]
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n")
	argsByCall := collectToolArgs(allEvents)
	pending := make(chan session.Event, 128)
	unsub := s.evSrc(id, func(ev session.Event) {
		select {
		case pending <- ev:
		case <-r.Context().Done():
		}
	})
	defer unsub()
	lastSeq := resumeSeq
	for _, ev := range events {
		if ev.Seq <= lastSeq {
			continue
		}
		writeSSEEvent(w, ev, argsByCall)
		lastSeq = ev.Seq
	}
	fl.Flush()
	// The live hub is deliberately non-blocking. If a slow client overflows
	// its in-memory queue, the next observed event (or this reconciliation
	// tick) repairs the durable gap before anything is written out of order.
	reconcile := time.NewTicker(2 * time.Second)
	defer reconcile.Stop()
	for {
		select {
		case ev := <-pending:
			if ev.Seq <= lastSeq {
				continue // subscription raced the snapshot; do not duplicate it
			}
			if ev.Seq > lastSeq+1 {
				var catchErr error
				lastSeq, catchErr = s.repairEventStream(r.Context(), id, lastSeq, argsByCall, func(missing session.Event) {
					writeSSEEvent(w, missing, argsByCall)
					fl.Flush()
				})
				if catchErr != nil {
					return // the client retries from the last durable SSE id
				}
			}
			if ev.Seq <= lastSeq {
				continue // repair may have included the queued event
			}
			if ev.Seq != lastSeq+1 {
				return // never publish an unverifiable sequence gap
			}
			collectToolArgsInto(argsByCall, ev)
			writeSSEEvent(w, ev, argsByCall)
			lastSeq = ev.Seq
			fl.Flush()
		case <-reconcile.C:
			var catchErr error
			lastSeq, catchErr = s.repairEventStream(r.Context(), id, lastSeq, argsByCall, func(missing session.Event) {
				writeSSEEvent(w, missing, argsByCall)
				fl.Flush()
			})
			if catchErr != nil {
				return
			}
		case <-r.Context().Done():
			// A publisher can enqueue an event immediately before the client
			// disconnects. Drain already-buffered events once so cancellation
			// does not make the stream lose a frame.
			for {
				select {
				case ev := <-pending:
					if ev.Seq <= lastSeq {
						continue
					}
					collectToolArgsInto(argsByCall, ev)
					writeSSEEvent(w, ev, argsByCall)
					lastSeq = ev.Seq
				default:
					fl.Flush()
					return
				}
			}
		}
	}
}

const streamRepairPageSize = 256

// repairEventStream fills the suffix after lastSeq from durable storage. The
// hub is an optimization for latency, not the source of truth: this method is
// what makes reconnects and slow subscribers lossless even when the hub drops
// an in-memory notification. It writes only contiguous events and returns the
// new cursor, so callers can safely deduplicate a notification that was also
// present in the repaired page.
func (s *Server) repairEventStream(ctx context.Context, sessionID string, lastSeq uint64, argsByCall map[string]string, emit func(session.Event)) (uint64, error) {
	for {
		events, more, err := s.store.LoadSessionPage(ctx, sessionID, 0, lastSeq, streamRepairPageSize)
		if err != nil {
			return lastSeq, err
		}
		for _, ev := range events {
			if ev.Seq <= lastSeq {
				continue
			}
			if ev.Seq != lastSeq+1 {
				return lastSeq, fmt.Errorf("web: durable event gap for %s at seq %d (got %d)", sessionID, lastSeq+1, ev.Seq)
			}
			collectToolArgsInto(argsByCall, ev)
			emit(ev)
			lastSeq = ev.Seq
		}
		if !more {
			return lastSeq, nil
		}
	}
}

func streamResumeSeq(r *http.Request) (uint64, error) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Last-Event-ID must be a positive integer")
	}
	return seq, nil
}

// handleSessionContext implements GET /api/sessions/{id}/context (dsh
// ContextMeter): it estimates the current session's model-visible token use from
// its stored events and pairs it with the effective model's context window (the
// composition root's contextWindowFn honors the per-session model override).
func (s *Server) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	events, err := s.store.LoadSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Context occupancy follows dsh and the compaction engine: count the
	// model-visible surface after replaying surfaceOp.replace markers. The
	// append-only log deliberately retains shadowed events for audit/replay, so
	// summing every raw event here would make /compact appear to do nothing.
	measurement := meter.Measurement{}
	var measureErr error
	if s.contextMeterFn != nil {
		measurement, measureErr = s.contextMeterFn(id, events)
	}
	used := measurement.TotalTokens
	if s.contextMeterFn == nil || measureErr != nil {
		used = estimateContextTokens(events)
	}
	window := s.contextWindowForSession(id)
	percent := 0.0
	if window > 0 {
		percent = float64(used) / float64(window)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"used_tokens":               used,
		"context_window":            window,
		"percent":                   percent,
		"baseline":                  measurement.Baseline,
		"baseline_estimated_tokens": measurement.BaselineEstimatedTokens,
		"baseline_usage_tokens":     measurement.BaselineUsageTokens,
		"surface_delta_tokens":      measurement.SurfaceDeltaTokens,
		"surface_tokens":            measurement.SurfaceTokens,
		"completion_ledger":         measurement.Completion,
		"log_revision":              measurement.LogRevision,
	})
}

// handleSessionState exposes state rebuilt from the target session's durable
// event log. It is intentionally separate from /events: clients can refresh
// status/projections without re-rendering the conversation transcript.
func (s *Server) handleSessionState(w http.ResponseWriter, r *http.Request) {
	if s.stateFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "session state provider not wired"})
		return
	}
	state, err := s.stateFn(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// estimateContextTokens mirrors compaction.defaultTokenEstimate over the
// shared projection's model-visible history. Raw-event fallback is
// intentionally fail-open for an old or damaged log: the meter should still
// provide a useful conservative value instead of making the whole context
// endpoint fail.
func estimateContextTokens(events []session.Event) int {
	snapshot, err := projection.Build(events)
	if err != nil {
		used := 0
		for _, ev := range events {
			used += len(ev.Data) / 4
		}
		return used
	}
	used := 0
	for _, message := range snapshot.History {
		used += len(message.Text()) / 4
		for _, call := range message.ToolCalls {
			used += (len(call.Name) + len(call.Arguments)) / 4
		}
	}
	return used
}

// defaultContextWindow is the fallback token budget when the wired
// contextWindowFn cannot resolve the model (dsh llm-deepseek
// DEFAULT_CONTEXT_WINDOW: 1,000,000).
const defaultContextWindow = 1000000

// handleTurnStop implements POST /api/sessions/{id}/stop (dsh 停止按钮): it asks
// the composition root to cancel the session's running turn (if any).
func (s *Server) handleTurnStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.stopFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "turn stopper not wired"})
		return
	}
	if err := s.stopFn(id); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeSSEEvent writes one SSE frame for an event and returns. Writes to a
// disconnected client fail silently (the handler exits on context cancellation).
func writeSSEEvent(w http.ResponseWriter, ev session.Event, argsByCall map[string]string) {
	v := toEventView(ev)
	if callID := callIDOf(ev); callID != "" {
		v.ToolArgs = argsByCall[callID]
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
}

// handleSubagents implements GET /api/subagents (M10 W4, ADR D-WEB2-H): the
// read-only panel for active sub-agents. An unwired provider answers 501; the
// provider's sanitized views are forwarded verbatim.
func (s *Server) handleSubagents(w http.ResponseWriter, r *http.Request) {
	if s.subFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "subagent provider not wired"})
		return
	}
	items, err := s.subFn(r.Context(), r.URL.Query().Get("session_id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagents": items})
}

// handleJobs implements GET /api/jobs (M10 W4, ADR D-WEB2-H): the read-only
// panel for background jobs. An unwired provider answers 501; the provider's
// sanitized views are forwarded verbatim.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.jobsFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "jobs provider not wired"})
		return
	}
	items, err := s.jobsFn(r.Context(), r.URL.Query().Get("session_id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

// handleInteractions exposes the live approval queue used by the DSH Web
// approval composer. Prompt and args are already bounded by interact.Engine;
// the response is still shaped explicitly so the engine's internal type never
// becomes a wire contract.
func (s *Server) handleInteractions(w http.ResponseWriter, r *http.Request) {
	if s.interactionFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "interaction manager not wired"})
		return
	}
	items, err := s.interactionFn(r.Context(), r.URL.Query().Get("session_id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	views := make([]map[string]any, 0, len(items))
	for _, item := range items {
		view := map[string]any{
			"id": item.ID, "prompt": item.Prompt, "tool_name": item.ToolName,
			"args": item.Args, "status": string(item.Status), "created_at": item.CreatedAt,
		}
		if len(item.Questions) > 0 {
			view["questions"] = item.Questions
		}
		if item.ResolvedAt != nil {
			view["resolved_at"] = item.ResolvedAt
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"interactions": views})
}

// handleInteractionResolve applies one browser decision to a pending request.
func (s *Server) handleInteractionResolve(w http.ResponseWriter, r *http.Request) {
	if s.resolveInteractionFn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "interaction resolver not wired"})
		return
	}
	var body struct {
		Status string `json:"status"`
		Answer string `json:"answer,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	status := interact.ApprovalStatus(body.Status)
	if status != interact.StatusApproved && status != interact.StatusRejected && status != interact.StatusCanceled {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status must be approved, rejected, or cancelled"})
		return
	}
	if err := s.resolveInteractionFn(r.Context(), r.URL.Query().Get("session_id"), r.PathValue("id"), status, body.Answer); err != nil {
		if errors.Is(err, interact.ErrUnknownRequest) || errors.Is(err, interact.ErrAlreadyResolved) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": r.PathValue("id"), "status": string(status)})
}

// eventDetails exposes only allow-listed, bounded fields for DSH lifecycle
// cards. Raw event payloads never cross this boundary: tool arguments, model
// context and other opaque data remain private.
func eventDetails(ev session.Event) map[string]any {
	if ev.Type == session.EventLLMRequestStart || ev.Type == session.EventLLMRequestEnd || ev.Type == session.EventLLMRetry || ev.Type == session.EventLLMRetryStarted {
		return llmRequestDetails(ev)
	}
	allowed := map[string][]string{
		session.EventCommandRun:             {"commandId", "name", "args", "source"},
		session.EventCommandDone:            {"commandId", "kind", "text", "sourceEventSeq"},
		session.EventPlanCreate:             {"scope", "id", "title", "goalId", "status"},
		session.EventPlanUpdate:             {"scope", "id", "title", "objective"},
		session.EventPlanDelete:             {"scope", "id"},
		session.EventPlanStatus:             {"scope", "id", "status", "reason"},
		session.EventPlanMode:               {"active"},
		session.EventGoalRoundStart:         {"goalId", "round"},
		session.EventGoalRoundEnd:           {"goalId", "round", "status", "error"},
		session.EventInteractRequest:        {"id", "toolName"},
		session.EventInteractResolve:        {"id", "approved"},
		session.EventInteractCancel:         {"id"},
		session.EventInteractDeny:           {"id"},
		session.EventInteractStatus:         {"id", "status"},
		session.EventApprovalAsked:          {"id", "toolName", "reason", "questions"},
		session.EventApprovalDecided:        {"id", "outcome", "answer"},
		session.EventWorkflowRun:            {"total", "completed", "failed"},
		session.EventWorkflowStart:          {"runId", "label"},
		session.EventWorkflowPhase:          {"title", "phase"},
		session.EventWorkflowLog:            {"message"},
		session.EventWorkflowAgentStart:     {"seq", "label", "phase"},
		session.EventWorkflowAgentEnd:       {"seq", "label", "outcome", "child_id"},
		session.EventWorkflowEnd:            {"stop_reason", "agents_started", "error"},
		session.EventToolWorkflowRunStart:   {"runId", "name"},
		session.EventToolWorkflowAgentStart: {"runId", "seq", "label", "phase", "childId"},
		session.EventToolWorkflowAgentEnd:   {"runId", "seq", "outcome"},
		session.EventToolWorkflowRunEnd:     {"runId", "stopReason"},
		session.EventRalphRun:               {"objective", "rounds", "done", "blocked"},
		session.EventEvalRun:                {"id", "taskId", "verdict", "reason", "evaluatorKind", "criteriaCount"},
		session.EventCodeRun:                {"lang", "exitCode", "timedOut", "truncated"},
		session.EventCodeDispatchStart:      {"rootCallId", "parentCallId", "subCallId", "name", "arguments"},
		session.EventCodeDispatch:           {"rootCallId", "parentCallId", "subCallId", "name", "arguments", "isError", "content"},
		session.EventFsRead:                 {"path", "size"},
		session.EventFsWrite:                {"path"},
		session.EventFsList:                 {"dir", "count"},
		session.EventTerminalStart:          {"id", "owner"},
		session.EventTerminalStop:           {"id", "reason"},
		session.EventScheduleCreate:         {"id", "kind", "spec"},
		session.EventScheduleList:           {"count"},
		session.EventScheduleDelete:         {"id"},
		session.EventScheduleFire:           {"id", "action"},
		session.EventSpillWrite:             {"id", "content"},
		session.EventSpillRecall:            {"query", "count"},
		session.EventSpillList:              {"count"},
		session.EventSpillDelete:            {"id"},
	}
	keys, ok := allowed[ev.Type]
	if !ok {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal(ev.Data, &raw) != nil {
		return nil
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		value, exists := raw[key]
		if !exists {
			continue
		}
		switch typed := value.(type) {
		case string:
			out[key] = boundRunes(typed, maxSummary)
		case bool, float64:
			out[key] = typed
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// llmRequestDetails is an allow-listed projection for the trajectory details
// drawer. It exposes provider/model/attempt/token usage without forwarding the
// opaque request payload or credentials.
func llmRequestDetails(ev session.Event) map[string]any {
	var raw struct {
		RequestID  string          `json:"requestId"`
		RetryID    string          `json:"retryId"`
		Turn       int             `json:"turn"`
		Step       int             `json:"step"`
		Provider   string          `json:"provider"`
		Model      string          `json:"model"`
		Effort     string          `json:"reasoningEffort"`
		Status     string          `json:"status"`
		Error      string          `json:"error"`
		Attempt    int             `json:"attempt"`
		Retry      int             `json:"retry"`
		MaxRetries int             `json:"maxRetries"`
		DelayMS    int64           `json:"delayMs"`
		Attempts   int             `json:"attempts"`
		Usage      *llm.TokenUsage `json:"usage"`
		Messages   []struct {
			Role       string `json:"role"`
			Text       string `json:"text"`
			Reasoning  string `json:"reasoning"`
			ToolCallID string `json:"toolCallId"`
			ToolCalls  []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"toolCalls"`
			Images int `json:"images"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"tools"`
	}
	if json.Unmarshal(ev.Data, &raw) != nil {
		return nil
	}
	out := make(map[string]any, 16)
	if raw.RequestID != "" {
		out["request_id"] = raw.RequestID
	}
	if raw.RetryID != "" {
		out["retry_id"] = raw.RetryID
	}
	if raw.Turn != 0 {
		out["turn"] = raw.Turn
	}
	if raw.Step != 0 {
		out["step"] = raw.Step
	}
	if raw.Provider != "" {
		out["provider"] = raw.Provider
	}
	if raw.Model != "" {
		out["model"] = raw.Model
	}
	if raw.Effort != "" {
		out["reasoning_effort"] = raw.Effort
	}
	if raw.Status != "" {
		out["status"] = raw.Status
	}
	if raw.Error != "" {
		out["error"] = boundRunes(raw.Error, maxSummary)
	}
	if raw.Attempt != 0 {
		out["attempt"] = raw.Attempt
	}
	if raw.Retry != 0 {
		out["retry"] = raw.Retry
	}
	if raw.MaxRetries != 0 {
		out["max_retries"] = raw.MaxRetries
	}
	if raw.DelayMS != 0 {
		out["delay_ms"] = raw.DelayMS
	}
	if raw.Attempts != 0 {
		out["attempts"] = raw.Attempts
	}
	if raw.Usage != nil && !raw.Usage.Empty() {
		out["usage"] = map[string]any{
			"input_tokens":        raw.Usage.InputTokens,
			"output_tokens":       raw.Usage.OutputTokens,
			"total_tokens":        raw.Usage.TotalTokens,
			"reasoning_tokens":    raw.Usage.ReasoningTokens,
			"cached_input_tokens": raw.Usage.CachedInputTokens,
		}
	}
	if len(raw.Messages) > 0 {
		const maxMessages = 100
		messages := raw.Messages
		if len(messages) > maxMessages {
			messages = messages[:maxMessages]
			out["messages_truncated"] = true
		}
		projected := make([]map[string]any, 0, len(messages))
		for _, message := range messages {
			item := map[string]any{"role": message.Role}
			if message.Text != "" {
				item["text"] = boundRunes(message.Text, maxSummary)
			}
			if message.Reasoning != "" {
				item["reasoning"] = boundRunes(message.Reasoning, maxSummary)
			}
			if message.ToolCallID != "" {
				item["tool_call_id"] = message.ToolCallID
			}
			if message.Images > 0 {
				item["images"] = message.Images
			}
			if len(message.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					calls = append(calls, map[string]any{"id": call.ID, "name": call.Name, "arguments": boundRunes(call.Arguments, maxSummary)})
				}
				item["tool_calls"] = calls
			}
			projected = append(projected, item)
		}
		out["messages"] = projected
	}
	if len(raw.Tools) > 0 {
		const maxTools = 100
		tools := raw.Tools
		if len(tools) > maxTools {
			tools = tools[:maxTools]
			out["tools_truncated"] = true
		}
		projected := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			item := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				item["description"] = boundRunes(tool.Description, maxSummary)
			}
			if tool.Parameters != nil {
				item["parameters"] = tool.Parameters
			}
			projected = append(projected, item)
		}
		out["tools"] = projected
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// summarize extracts a bounded, safe one-line summary for an event by
// unmarshalling only the leaf fields the known types carry (未知类型 → ""; 前端
// 忽略空 summary). The raw Data blob is never exposed. Message bodies are the
// exception to the bound: user/assistant message text returns in full because
// the frontend displays it whole (dsh behavior).
func summarize(ev session.Event) string {
	switch ev.Type {
	case session.EventCommandRun:
		var d struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return "/" + d.Name
		}
	case session.EventCommandDone:
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return boundRunes(d.Text, maxSummary)
		}
	case session.EventWebCommandResult:
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return d.Text
		}
	case "user/message":
		var d struct {
			Text    string `json:"text"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			if d.Text != "" {
				return d.Text
			}
			var text string
			for _, block := range d.Content {
				text += block.Text
			}
			return text
		}
	case "assistant/message":
		var d struct {
			Text    string `json:"text"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			if d.Text != "" {
				return d.Text
			}
			var text string
			for _, block := range d.Message.Content {
				if block.Type == "text" {
					text += block.Text
				}
			}
			return text
		}
	case "tool/result":
		var d struct {
			CallID string
			Name   string
			Output string
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return "tool " + d.Name + " → " + boundRunes(d.Output, maxSummary)
		}
	case "tool/call", "tool/start":
		var d struct {
			CallID string
			Name   string
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return "tool " + d.Name + " …"
		}
	case "tool/error":
		var d struct {
			CallID string
			Name   string
			Err    string
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return "tool " + d.Name + " error → " + boundRunes(d.Err, maxSummary)
		}
	case "skill/catalog":
		return "上下文注入 skill-catalog"
	case "compaction/summary":
		var d struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Summary != "" {
			return "上下文压缩: " + boundRunes(d.Summary, maxSummary)
		}
	}
	return ""
}

// boundRunes truncates s to at most max runes, appending "…" when cut.
func boundRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}
