// webserver.go — the M10 unified web portal (ADR 2026-08-20-m10-web-portal.md
// D-WEB-1~7): a single net/http server carrying the dsh-style session/event
// browsing entry (M10a), the dashboard stats API (M10c) and later KB admin
// (M10b). The API is read-only (D-WEB-4): it never writes the session log.
// Every route sits behind the bearer-token middleware; the frontend is vanilla
// JS embedded into the binary (go:embed) — zero new dependencies.
package webserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

//go:embed static
var staticFS embed.FS

// maxSummary is the rune cap on the bounded per-event summary the events API
// exposes (防超大载荷 / 防泄露完整日志正文, D-WEB-4). Message bodies
// (user/message, assistant/message) are the text the frontend must display in
// full — dsh renders assistant markdown whole, so they are NOT truncated; the
// cap applies to tool outputs, reasoning, snippets and injected text.
const maxSummary = 200

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
	srv       *http.Server

	// M10 W1 interactive wiring (ADR D-WEB2-A/B/C): the optional handlers the
	// composition root injects after New. All three are nil until a Setter is
	// called; a nil handler makes its API answer 501.
	msgFn  func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error
	sessFn func(ctx context.Context, action, id string) (string, error)
	evSrc  func(sessionID string, sink func(session.Event)) func()

	// statusFn is the dsh-session-status alignment: it computes the live state
	// (warning/ongoing/done/idle + labels + running-subagent count) for one
	// session row, so the sidebar renders the status dot and the hover card
	// without the webserver knowing any runtime state. nil (the default) leaves
	// every row's status empty.
	statusFn func(ctx context.Context, m store.SessionMeta) SessionStatus

	// cfgFn is the M10 W2 config provider (ADR D-WEB2-D): it returns the
	// sanitized configuration view for GET /api/config. The redaction itself is
	// the composition root's job (cmd/pa's webConfig never exposes web_server.
	// token or any key); the webserver only forwards the provider's map. nil
	// (the default) makes the API answer 501.
	cfgFn func() map[string]any

	// contextWindowFn resolves the effective model's context window (used by
	// GET /api/sessions/{id}/context for the ContextMeter). It takes the session
	// id (for the per-session model override) and returns a token budget; 0
	// means the server falls back to its default.
	contextWindowFn func(sessionID string) int

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
	att *attachment.Store

	// P5.1 (模型选择实时生效, 用户 2026-08-20 拍板): the live model-switch
	// dispatcher for POST /api/config/model. It validates the provider/model/
	// reasoning-effort, rebuilds the selected LLM provider and answers the new
	// config state. nil (the default) makes the API answer 501.
	setModelFn func(ctx context.Context, provider, model, effort string) error

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

	// 技能设置页 (dsh-skill-mcp-panel 对齐): the skill-management dispatcher for
	// /api/config/skills. It lists every skill with scope/rel/disabled state,
	// reads full content, hot-enables/disables, deletes, adds, migrates between
	// the two scopes and persists display groups. nil (the default) makes every
	// /api/config/skills route answer 501.
	skillsFn func(ctx context.Context, action string, req SkillRequest) (map[string]any, error)
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
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
}

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

// SetSkillManager wires the skill-management API (POST /api/config/skills, the
// dsh-skill-mcp-panel 对齐 settings page). The composition root dispatches each
// action to the skill.Manager and returns a JSON-able result map. nil (the
// default) keeps every /api/config/skills route at 501.
func (s *Server) SetSkillManager(fn func(ctx context.Context, action string, req SkillRequest) (map[string]any, error)) {
	s.skillsFn = fn
}

// SetAttachmentStore wires the image-attachment store (P5): POST/GET
// /api/sessions/{id}/attachments and the images field of POST /api/sessions/
// {id}/message. Called by the composition root; nil (default) keeps the
// attachment APIs at 501.
func (s *Server) SetAttachmentStore(st *attachment.Store) { s.att = st }

// SetModelSwitcher wires the live model switch (POST /api/config/model, P5.1):
// the handler validates the provider/model/reasoning-effort and rebuilds the
// selected LLM provider. Called by the composition root; nil (default) keeps
// the API at 501.
func (s *Server) SetModelSwitcher(fn func(ctx context.Context, provider, model, effort string) error) {
	s.setModelFn = fn
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
		store:     st,
		tokenHash: sha256.Sum256([]byte(token)),
		authOn:    token != "",
		addr:      addr,
	}
	// The static shell (login view + frontend assets) is public so a fresh
	// browser can load the page and present the token form (D-WEB-2): it holds
	// no data. Every /api route sits behind the bearer middleware.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /static/{file...}", s.handleStatic)
	mux.Handle("GET /api/health", s.requireAuth(http.HandlerFunc(s.handleHealth)))
	mux.Handle("GET /api/stats", s.requireAuth(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /api/kb", s.requireAuth(http.HandlerFunc(s.handleKBStub)))
	mux.Handle("GET /api/kb/{rest...}", s.requireAuth(http.HandlerFunc(s.handleKBStub)))
	mux.Handle("GET /api/sessions", s.requireAuth(http.HandlerFunc(s.handleSessions)))
	mux.Handle("GET /api/sessions/{id}/events", s.requireAuth(http.HandlerFunc(s.handleEvents)))
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
	// P6 workspace grouping (dsh grouped sidebar view): list, create, rename,
	// delete, order. The sessions list carries workspace_id so the sidebar groups.
	mux.Handle("GET /api/workspaces", s.requireAuth(http.HandlerFunc(s.handleWorkspaces)))
	mux.Handle("POST /api/workspaces", s.requireAuth(http.HandlerFunc(s.handleWorkspaceCreate)))
	mux.Handle("PATCH /api/workspaces/{id}", s.requireAuth(http.HandlerFunc(s.handleWorkspaceTitle)))
	mux.Handle("DELETE /api/workspaces/{id}", s.requireAuth(http.HandlerFunc(s.handleWorkspaceDelete)))
	mux.Handle("PATCH /api/workspaces/order", s.requireAuth(http.HandlerFunc(s.handleWorkspacesOrder)))
	mux.Handle("POST /api/sessions/{id}/message", s.requireAuth(http.HandlerFunc(s.handleMessage)))
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
	// M10 W4 (ADR D-WEB2-H): the read-only subagent and background-job panels.
	mux.Handle("GET /api/subagents", s.requireAuth(http.HandlerFunc(s.handleSubagents)))
	mux.Handle("GET /api/jobs", s.requireAuth(http.HandlerFunc(s.handleJobs)))
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// SetMessageHandler wires the message dispatch API (POST
// /api/sessions/{id}/message). images carries the parsed image refs of the
// message (P5), nil/empty for text-only turns. Called by the composition root
// (cmd/pa) at registration time; nil (the default) makes the API answer 501.
func (s *Server) SetMessageHandler(fn func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error) {
	s.msgFn = fn
}

// SetSessionManager wires the session new/resume API (POST /api/sessions and
// POST /api/sessions/{id}/resume). Called by the composition root; nil makes
// those APIs answer 501.
func (s *Server) SetSessionManager(fn func(ctx context.Context, action, id string) (string, error)) {
	s.sessFn = fn
}

// SetEventSource wires the real-time event stream (GET
// /api/sessions/{id}/events/stream): the source subscribes a session and calls
// sink for each new event; the returned func unsubscribes. Called by the
// composition root; nil makes the stream answer 501.
func (s *Server) SetEventSource(fn func(sessionID string, sink func(session.Event)) func()) {
	s.evSrc = fn
}

// SetConfigProvider wires the read-only config view (GET /api/config, M10 W2,
// ADR D-WEB2-D). The provider returns a sanitized map — cmd/pa's webConfig
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
	Message   func(ctx context.Context, sessionID, text string, images []llm.ImageRef) error
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
// compare. Only the API routes are gated; the static shell stays public so the
// login view can load (data never leaves the API). It also recovers a panicking
// handler into a JSON 500 (M10 W3 robustness): a crashed route must never
// answer a bare connection reset, and the panic + stack is logged for repair.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &panicSafeWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("pa: web handler panic: %v\n%s", rec, debug.Stack())
				if !sw.wrote {
					writeJSON(sw, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("internal error: %v", rec)})
				}
			}
		}()
		if !s.authOn {
			next.ServeHTTP(sw, r)
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

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleFavicon serves the browser favicon: the user's black brand logo
// (big_logo_1.png). index.html declares it via <link rel="icon">; this route
// catches direct /favicon.ico requests so they never 404 (dsh: favicon = the
// brand mark).
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/big_logo_1.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// handleIndex serves the embedded single-page shell. In the ServeMux the
// pattern "GET /" matches every unmatched path, so a strict path check keeps
// unknown routes a 404 rather than serving the shell.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// handleStatic serves embedded static assets under /static/ (StripPrefix
// removes the route prefix so the FileServer resolves inside the static dir).
// no-cache: every reload revalidates the embedded assets, so a rebuilt binary
// (new app.js/style.css) is picked up by a plain refresh instead of the
// browser's heuristic cache.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "static missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.StripPrefix("/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleConfig implements GET /api/config (M10 W2, ADR D-WEB2-D): it serves
// the injected config provider's sanitized map verbatim. The provider (cmd/pa's
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
	resp := map[string]any{
		"agent_preset":       stored["agent_preset"],
		"permission_preset":  stored["permission_preset"],
		"terminal_shell":     stored["terminal_shell"],
		"mode_options":       []string{"minimal", "standard", "code"},
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
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if body.AgentPreset != "" {
		if body.AgentPreset != "minimal" && body.AgentPreset != "standard" && body.AgentPreset != "code" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid agent_preset"})
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
		if s.statusFn != nil {
			v.Status = s.statusFn(r.Context(), m)
		}
		if m.Title != "" {
			// Accepted title (fallback / LLM / user rename), normalized at
			// write; re-normalize defensively for legacy rows.
			v.Title = session.NormalizeTitle(m.Title, session.TitleMaxBytes)
		} else if m.EventCount > 0 {
			// The deterministic first-prompt fallback (dsh session-title):
			// first eligible words of the first user message, byte-bounded.
			if evs, err := s.store.LoadSession(r.Context(), m.ID); err == nil {
				for _, ev := range evs {
					if ev.Type == "user/message" {
						var d struct{ Text string }
						if json.Unmarshal(ev.Data, &d) == nil && strings.TrimSpace(d.Text) != "" {
							v.Title = session.FallbackTitle(d.Text, session.TitleFallbackMaxWords, session.TitleFallbackMaxBytes)
							break
						}
					}
				}
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSessionTitle implements PATCH /api/sessions/{id}/title (P2 sidebar
// rename). The request body is {"title":"..."} (UTF-8, normalized and bounded
// to session.TitleMaxBytes); an empty title clears the override back to
// inference. A non-empty title is recorded with the user source, which pins it
// against future automatic revisions (dsh session-title rename semantics).
func (s *Server) handleSessionTitle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct{ Title string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	title := session.NormalizeTitle(body.Title, session.TitleMaxBytes)
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

// handleKBStub is the M10b KB-admin placeholder (ADR D-WEB-6): the /api/kb/*
// routes return 501 until the KB 全量 (content layer) lands and the real admin
// data/API is mounted — the shell exists so the frontend can navigate, the
// backend is honestly "not implemented".
func (s *Server) handleKBStub(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "KB admin not implemented (KB 全量后挂)"})
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
	Seq        uint64      `json:"seq"`
	Type       string      `json:"type"`
	Time       time.Time   `json:"time"`
	Summary    string      `json:"summary"`
	Reasoning  string      `json:"reasoning,omitempty"`   // assistant/message 的思维链（有界）
	ToolName   string      `json:"tool_name,omitempty"`   // tool/start、tool/result、tool/error 的工具名
	ToolOutput string      `json:"tool_output,omitempty"` // tool/result 的有界输出
	ToolArgs   string      `json:"tool_args,omitempty"`   // 该调用名对应的工具入参（来自 assistant 的 toolCall；只读展示用）
	CallID     string      `json:"call_id,omitempty"`     // 工具调用关联 id（tool/start → tool/result/error 配对）
	Images     []imageView `json:"images,omitempty"`      // P5: 该消息携带的图片引用
}

// toEventView builds the public view for one event (bounded summary + the W4
// extra fields + the P5 image refs).
func toEventView(ev session.Event) eventView {
	v := eventView{Seq: ev.Seq, Type: ev.Type, Time: ev.At, Summary: summarize(ev)}
	v.Reasoning, v.ToolName, v.ToolOutput = extraFields(ev)
	v.Images = extractImages(ev)
	v.CallID = callIDOf(ev)
	return v
}

// extractImages pulls the image refs out of a user/assistant message's content
// blocks (only ref metadata — the bytes live in the attachment store). Unknown
// payloads yield nil; the frontend hides history images when absent.
func extractImages(ev session.Event) []imageView {
	if ev.Type != "user/message" && ev.Type != "assistant/message" {
		return nil
	}
	var d struct {
		Content []llm.ContentBlock `json:"content"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return nil
	}
	var out []imageView
	for _, b := range d.Content {
		if b.Kind != llm.BlockImage {
			continue
		}
		out = append(out, imageView{
			ID: b.Image.ID, MediaType: b.Image.MediaType,
			Width: b.Image.Width, Height: b.Image.Height,
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
	case "tool/start":
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
	events, err := s.store.LoadSession(r.Context(), id)
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
	writeJSON(w, http.StatusOK, out)
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
	case "tool/start", "tool/result", "tool/error":
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
	id, err := s.sessFn(r.Context(), "new", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if body.WorkspaceID != "" {
		if err := s.store.SetSessionWorkspace(r.Context(), id, body.WorkspaceID); err != nil {
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
	// Carry the source title and workspace membership over to the clone.
	srcTitle, srcTitleSource, srcWorkspace := "", "", ""
	if all, err := s.store.ListSessions(r.Context()); err == nil {
		for _, m := range all {
			if m.ID == id {
				srcTitle, srcTitleSource, srcWorkspace = m.Title, m.TitleSource, m.WorkspaceID
				break
			}
		}
	}
	forkID, err := newSessionID()
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
	// The fork inherits the source's per-session overrides (dsh fork: the
	// clone keeps the parent's agent preset / model / permission), so the
	// diverging copy starts from the same mode.
	if scs, ok := s.store.(store.SessionConfigStore); ok {
		if cfg, err := scs.GetSessionConfig(r.Context(), id); err == nil {
			_ = scs.SetSessionConfig(r.Context(), forkID, cfg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": forkID})
}

// newSessionID returns a short random session id (e.g. "s-1a2b3c4d").
func newSessionID() (string, error) {
	var b [4]byte
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
		out = append(out, workspaceView{ID: m.ID, Title: m.Title, SessionIDs: ids, CreatedAt: created})
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

// handleWorkspaceCreate implements POST /api/workspaces {"title":...} (P6).
func (s *Server) handleWorkspaceCreate(w http.ResponseWriter, r *http.Request) {
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
	id, err := newWorkspaceID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.CreateWorkspace(r.Context(), id, title); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "title": title})
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
	if err := s.store.DeleteWorkspace(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "workspace not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
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
	if err := s.msgFn(r.Context(), id, body.Text, images); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maxWebImageBytes caps a single uploaded image via the web portal (P5). The
// frontend enforces the same default (10MB); the backend fails closed so the
// portal never writes a giant file even if the client lies.
const maxWebImageBytes = 10 << 20

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
// real-time event flow (M10 W1, ADR D-WEB2-B): it first replays the session's
// stored events as frames (snapshot), then subscribes the injected event source
// and forwards every new event as a frame. Each frame is
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
	events, err := s.store.LoadSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
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
	argsByCall := make(map[string]string)
	for _, ev := range events {
		collectToolArgsInto(argsByCall, ev)
		writeSSEEvent(w, ev, argsByCall)
	}
	fl.Flush()
	unsub := s.evSrc(id, func(ev session.Event) {
		collectToolArgsInto(argsByCall, ev)
		writeSSEEvent(w, ev, argsByCall)
		fl.Flush()
	})
	defer unsub()
	<-r.Context().Done()
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
	// Rough token proxy (len/4, mirroring compaction.defaultTokenEstimate): the
	// body of every event - messages, tool-call names/arguments, injected text.
	used := 0
	for _, ev := range events {
		used += len(ev.Data) / 4
	}
	window := defaultContextWindow
	if s.contextWindowFn != nil {
		if w := s.contextWindowFn(id); w > 0 {
			window = w
		}
	}
	percent := 0.0
	if window > 0 {
		percent = float64(used) / float64(window)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"used_tokens":    used,
		"context_window": window,
		"percent":        percent,
	})
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

// summarize extracts a bounded, safe one-line summary for an event by
// unmarshalling only the leaf fields the known types carry (未知类型 → ""; 前端
// 忽略空 summary). The raw Data blob is never exposed. Message bodies are the
// exception to the bound: user/assistant message text returns in full because
// the frontend displays it whole (dsh behavior).
func summarize(ev session.Event) string {
	switch ev.Type {
	case session.EventWebCommandResult:
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return d.Text
		}
	case "user/message":
		var d struct{ Text string }
		if json.Unmarshal(ev.Data, &d) == nil {
			return d.Text
		}
	case "assistant/message":
		var d struct{ Text string }
		if json.Unmarshal(ev.Data, &d) == nil {
			return d.Text
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
	case "tool/start":
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
	case "kb/recall":
		// dsh 上下文注入: 跨会话召回卡片 (query + hit count).
		var d struct {
			Query string `json:"query"`
			Hits  []struct {
				Title string `json:"title"`
			} `json:"hits"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			s := "跨会话召回: " + boundRunes(d.Query, maxSummary)
			if len(d.Hits) > 0 {
				s += fmt.Sprintf(" (%d 条)", len(d.Hits))
			}
			return s
		}
	case "skill/catalog":
		var d struct {
			EntryCount int `json:"entryCount"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			return fmt.Sprintf("上下文注入: 技能目录 (%d 个技能)", d.EntryCount)
		}
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
