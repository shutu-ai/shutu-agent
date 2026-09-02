// Package acp implements the small, automation-only ACP JSON-RPC surface.
//
// The transport is newline-delimited JSON. Stdout belongs exclusively to ACP
// frames; callers own diagnostics and should write them to stderr.
package acp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const ProtocolVersion = 1

type SessionFactory interface {
	NewSession(context.Context, string) (Session, error)
}

// ResumableSessionFactory is the durable-session extension. A server restart
// must not require the client to create a new transcript when the host can
// reopen the addressed session from its persistence backend.
type ResumableSessionFactory interface {
	SessionFactory
	ResumeSession(context.Context, string) (Session, error)
}

// CapabilityProvider lets a factory advertise only capabilities that are
// actually available in the current deployment. Unknown capabilities remain
// disabled; this keeps initialize fail-closed for partially configured hosts.
type CapabilityProvider interface {
	Capabilities(context.Context) map[string]bool
}

// ToolCatalogEntry is the wire-safe projection of one registered capability.
type ToolCatalogEntry struct {
	Name       string `json:"name"`
	Profile    string `json:"profile"`
	Provenance string `json:"provenance"`
	Generation uint64 `json:"generation"`
	Visible    bool   `json:"visible"`
}

// ToolCatalog carries a verifiable inventory revision. It is an optional ACP
// extension; absent when an embedder does not provide a registry-backed catalog.
type ToolCatalog struct {
	SchemaVersion int                `json:"schemaVersion"`
	Revision      uint64             `json:"revision"`
	Digest        string             `json:"digest"`
	Tools         []ToolCatalogEntry `json:"tools"`
}

// ToolCatalogProvider exposes the current canonical inventory and its revision.
// A provider error must fail initialize or session establishment; stale or
// incomplete inventory must not be presented as success.
type ToolCatalogProvider interface {
	ToolCatalog(context.Context) (ToolCatalog, error)
}

type Session interface {
	Prompt(context.Context, string, func(Update)) (StopReason, error)
	Cancel() error
	Close() error
}

// SessionIdentity is an optional extension for durable session factories.
// When implemented, session/new must expose this stable id on the ACP wire so
// a later session/resume or session/reconnect addresses the same transcript.
// Text-only embedders may omit it and retain the legacy server-generated id.
type SessionIdentity interface {
	Session
	SessionID() string
}

// ResumeMetadata lets a durable session expose persisted runtime facts without
// forcing every embedder to widen the JSON-RPC result shape.
type ResumeMetadata interface {
	ResumeMetadata() map[string]any
}

// ErrPromptInFlight reports that a durable runtime cannot be replaced because
// its addressed prompt is still active.
var ErrPromptInFlight = errors.New("ACP prompt is in flight")

// ErrSessionNotFound lets durable factories classify an unknown resume target
// as invalid request parameters rather than an opaque internal failure.
var ErrSessionNotFound = errors.New("ACP session not found")

// PermissionRequester is the ACP bridge used by a session when a tool needs
// a human decision. ACP models this as a server-initiated JSON-RPC request to
// the client; the request remains in flight until the matching response is
// received, cancelled, or the session context ends.
type PermissionRequester interface {
	RequestPermission(context.Context, PermissionRequest) (PermissionOutcome, error)
}

// PermissionSession is implemented by sessions that can delegate approval to
// the ACP client. It is optional so text-only embedders remain source
// compatible.
type PermissionSession interface {
	Session
	SetPermissionRequester(PermissionRequester)
}

type PermissionRequest struct {
	SessionID  string             `json:"sessionId"`
	ToolCallID string             `json:"toolCallId"`
	ToolName   string             `json:"toolName"`
	Reason     string             `json:"reason,omitempty"`
	Options    []PermissionOption `json:"options,omitempty"`
}

type PermissionOption struct {
	ID          string `json:"optionId"`
	Label       string `json:"name"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// PromptContentSession is the optional rich-prompt extension. The text-only
// Session interface is intentionally retained so existing embedders do not
// need to change in lockstep with ACP image support.
type PromptContentSession interface {
	Session
	PromptContent(context.Context, []PromptContentBlock, func(Update)) (StopReason, error)
}

type PromptContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// ResourceLinkText is the deterministic textual projection used when ACP
// carries a resource link. The harness does not fetch remote resources during
// prompt admission; the model receives an explicit reference instead.
func ResourceLinkText(name, uri string) string {
	nameJSON, _ := json.Marshal(name)
	uriJSON, _ := json.Marshal(uri)
	return "\n[resource_link name=" + string(nameJSON) + " uri=" + string(uriJSON) + "]\n"
}

type Update struct {
	// Text is the legacy shorthand for a text content block. Content is used
	// by rich-output sessions and is delivered verbatim as one ACP content
	// block, preserving the committed assistant block order.
	Text    string
	Content *PromptContentBlock
}

type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopCancelled StopReason = "cancelled"
)

type Server struct {
	Factory       SessionFactory
	In            io.Reader
	Out           io.Writer
	MaxFrameBytes int
	AgentName     string
	AgentVersion  string

	outMu            sync.Mutex
	sessionsMu       sync.Mutex
	sessions         map[string]Session
	nextID           int
	closed           bool
	workers          sync.WaitGroup
	promptMu         sync.Mutex
	promptActive     map[string]bool
	resumeMu         sync.Mutex
	permissionMu     sync.Mutex
	permissions      map[string]chan permissionReply
	nextPermissionID uint64
}

type permissionReply struct {
	outcome PermissionOutcome
	err     *rpcError
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func (s *Server) Run(ctx context.Context) error {
	if s.Factory == nil || s.In == nil || s.Out == nil {
		return errors.New("acp: factory, input and output are required")
	}
	if s.MaxFrameBytes <= 0 {
		s.MaxFrameBytes = 4 << 20
	}
	if s.AgentName == "" {
		s.AgentName = "shutu-agent"
	}
	if s.AgentVersion == "" {
		s.AgentVersion = "dev"
	}
	s.sessions = make(map[string]Session)
	s.permissions = make(map[string]chan permissionReply)
	s.promptActive = make(map[string]bool)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	scanner := bufio.NewScanner(s.In)
	scanner.Buffer(make([]byte, 64<<10), s.MaxFrameBytes)
	// Session creation/replacement is an admission barrier. A client may
	// pipeline session/prompt immediately after session/new on the same input
	// stream, while the factory may still be initializing the session. Prompt
	// handlers must not observe the transient "unknown session" state. The
	// barrier is limited to lifecycle admission; prompt/cancel requests remain
	// concurrent once the session exists.
	var lifecycleTail <-chan struct{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.JSONRPC != "2.0" {
			_ = s.writeResponse(response{JSONRPC: "2.0", ID: nullID(), Error: &rpcError{Code: -32600, Message: "invalid request"}})
			continue
		}
		if req.Method == "" {
			if !s.resolvePermissionResponse(req.ID, []byte(line)) {
				_ = s.writeResponse(response{JSONRPC: "2.0", ID: nullID(), Error: &rpcError{Code: -32600, Message: "invalid request"}})
			}
			continue
		}
		priorLifecycle := lifecycleTail
		var lifecycleDone chan struct{}
		if acpLifecycleAdmission(req.Method) {
			lifecycleDone = make(chan struct{})
			lifecycleTail = lifecycleDone
		}
		s.workers.Add(1)
		go func(req request, prior <-chan struct{}, done chan struct{}) {
			defer s.workers.Done()
			if prior != nil {
				<-prior
			}
			if done != nil {
				defer close(done)
			}
			s.handle(runCtx, req)
		}(req, priorLifecycle, lifecycleDone)
	}
	if err := scanner.Err(); err != nil {
		cancel()
		// A read-side transport failure is a connection teardown just like
		// clean EOF. Wake server-initiated permission requests before waiting
		// for prompt workers; otherwise a worker blocked on approval can keep
		// Run from returning forever.
		s.cancelPermissions(&rpcError{Code: -32800, Message: "ACP transport failed"})
		s.cancelSessions()
		s.workers.Wait()
		s.closeSessions()
		return fmt.Errorf("acp: read: %w", err)
	}
	cancel()
	s.cancelPermissions(&rpcError{Code: -32800, Message: "ACP server closed"})
	s.cancelSessions()
	s.workers.Wait()
	s.closeSessions()
	return nil
}

func acpLifecycleAdmission(method string) bool {
	switch method {
	case "session/new", "session/resume", "session/reconnect":
		return true
	default:
		return false
	}
}

func (s *Server) handle(ctx context.Context, req request) {
	result, err, notify := s.dispatch(ctx, req.Method, req.Params)
	if notify || len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}
	if err != nil {
		_ = s.writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: err})
		return
	}
	_ = s.writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, *rpcError, bool) {
	switch method {
	case "initialize":
		promptCapabilities := map[string]bool{"image": false, "audio": false, "embeddedContext": false}
		if provider, ok := s.Factory.(CapabilityProvider); ok {
			for name, enabled := range provider.Capabilities(ctx) {
				if _, known := promptCapabilities[name]; known {
					promptCapabilities[name] = enabled
				}
			}
		}
		result := map[string]any{
			"protocolVersion":   ProtocolVersion,
			"agentInfo":         map[string]string{"name": s.AgentName, "version": s.AgentVersion},
			"agentCapabilities": map[string]any{"promptCapabilities": promptCapabilities},
			"authMethods":       []any{},
		}
		catalog, err := s.toolCatalog(ctx)
		if err != nil {
			return nil, internalError(err), false
		}
		if catalog != nil {
			result["toolCatalog"] = catalog
		}
		return result, nil, false
	case "authenticate":
		return map[string]any{}, nil, false
	case "session/new":
		var p struct {
			CWD                   string   `json:"cwd"`
			AdditionalDirectories []string `json:"additionalDirectories"`
			MCPServers            []any    `json:"mcpServers"`
		}
		if err := decodeParams(raw, &p); err != nil || !isAbsoluteACPPath(p.CWD) {
			return nil, invalidParams("session/new requires an absolute cwd"), false
		}
		if len(p.AdditionalDirectories) != 0 || len(p.MCPServers) != 0 {
			return nil, invalidParams("additionalDirectories and mcpServers are unsupported"), false
		}
		// Capture before creating the runtime so a catalog failure cannot leak
		// an already-open session.
		catalog, catalogErr := s.toolCatalog(ctx)
		if catalogErr != nil {
			return nil, internalError(catalogErr), false
		}
		sess, err := s.Factory.NewSession(ctx, p.CWD)
		if err != nil {
			return nil, internalError(err), false
		}
		wireID := ""
		if identity, ok := sess.(SessionIdentity); ok {
			wireID = strings.TrimSpace(identity.SessionID())
		}
		s.sessionsMu.Lock()
		if s.closed {
			s.sessionsMu.Unlock()
			_ = sess.Close()
			return nil, internalError(errors.New("server is closed")), false
		}
		if wireID == "" {
			s.nextID++
			wireID = newSessionID(s.nextID)
		}
		id := wireID
		if _, exists := s.sessions[id]; exists {
			s.sessionsMu.Unlock()
			_ = sess.Close()
			return nil, invalidParams("session id is already open"), false
		}
		s.sessions[id] = sess
		s.sessionsMu.Unlock()
		if permissionSession, ok := sess.(PermissionSession); ok {
			permissionSession.SetPermissionRequester(permissionRequesterFunc{server: s, sessionID: id})
		}
		result := map[string]any{"sessionId": id}
		if catalog != nil {
			result["toolCatalog"] = catalog
		}
		return result, nil, false
	case "session/resume", "session/reconnect":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := decodeParams(raw, &p); err != nil || strings.TrimSpace(p.SessionID) == "" {
			return nil, invalidParams("session resume requires sessionId"), false
		}
		factory, ok := s.Factory.(ResumableSessionFactory)
		if !ok {
			return nil, invalidParams("session resume is unsupported"), false
		}
		// Resume builds a fresh runtime before replacing the old one. Serialize
		// replacement so two reconnecting clients cannot race the same durable
		// id and leave an orphan runtime behind.
		s.resumeMu.Lock()
		defer s.resumeMu.Unlock()
		catalog, catalogErr := s.toolCatalog(ctx)
		if catalogErr != nil {
			return nil, internalError(catalogErr), false
		}
		sess, err := factory.ResumeSession(ctx, p.SessionID)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return nil, invalidParams(fmt.Sprintf("session %q not found", p.SessionID)), false
			}
			return nil, internalError(err), false
		}
		old, busy, ok := s.replaceSession(p.SessionID, sess)
		if !ok {
			_ = sess.Close()
			if busy {
				return nil, &rpcError{Code: -32000, Message: ErrPromptInFlight.Error()}, false
			}
			return nil, internalError(errors.New("ACP server is closed")), false
		}
		if old != nil {
			_ = old.Close()
		}
		if permissionSession, ok := sess.(PermissionSession); ok {
			permissionSession.SetPermissionRequester(permissionRequesterFunc{server: s, sessionID: p.SessionID})
		}
		result := map[string]any{"sessionId": p.SessionID}
		if catalog != nil {
			result["toolCatalog"] = catalog
		}
		if metadata, ok := sess.(ResumeMetadata); ok {
			if values := metadata.ResumeMetadata(); len(values) != 0 {
				result["metadata"] = values
			}
		}
		return result, nil, false
	case "session/prompt":
		var p struct {
			SessionID string               `json:"sessionId"`
			Prompt    []PromptContentBlock `json:"prompt"`
		}
		if err := decodeParams(raw, &p); err != nil || p.SessionID == "" || len(p.Prompt) == 0 {
			return nil, invalidParams("session/prompt requires sessionId and prompt"), false
		}
		sess, busy, ok := s.beginPrompt(p.SessionID)
		if !ok {
			return nil, invalidParams("unknown session"), false
		}
		// ACP permits concurrent JSON-RPC requests, but one session has one
		// prompt turn. The reference rejects a second prompt while the first is
		// in flight; queueing it would silently create a second user turn after
		// the caller has already lost the cancellation/ordering boundary.
		if busy {
			return nil, invalidParams("a prompt is already in flight for this session"), false
		}
		defer s.endPrompt(p.SessionID)
		hasRich := false
		var text strings.Builder
		for _, block := range p.Prompt {
			switch block.Type {
			case "text":
				if block.Text == "" {
					return nil, invalidParams("text prompt blocks must be non-empty"), false
				}
				text.WriteString(block.Text)
			case "resource_link":
				if block.Name == "" || block.URI == "" {
					return nil, invalidParams("resource_link prompt blocks require name and uri"), false
				}
				text.WriteString(ResourceLinkText(block.Name, block.URI))
			case "image":
				hasRich = true
				provider, ok := s.Factory.(CapabilityProvider)
				if !ok || !provider.Capabilities(ctx)["image"] {
					return nil, invalidParams("image prompt capability is not available"), false
				}
				if !validImageMediaType(block.MimeType) {
					return nil, invalidParams("image mimeType must be image/png, image/jpeg, image/webp, or image/gif"), false
				}
				if !validCanonicalBase64(block.Data) {
					return nil, invalidParams("image data must be canonical base64"), false
				}
			default:
				return nil, invalidParams("unsupported prompt content type"), false
			}
		}
		if !hasRich && strings.TrimSpace(text.String()) == "" {
			return nil, invalidParams("prompt text is empty"), false
		}
		var emitMu sync.Mutex
		var emitErr error
		emit := func(u Update) {
			content := map[string]string{}
			if u.Content != nil {
				if u.Content.Type == "" {
					return
				}
				content["type"] = u.Content.Type
				switch u.Content.Type {
				case "text":
					content["text"] = u.Content.Text
				case "image":
					content["data"] = u.Content.Data
					content["mimeType"] = u.Content.MimeType
				default:
					return
				}
			} else {
				if strings.TrimSpace(u.Text) == "" {
					return
				}
				content["type"] = "text"
				content["text"] = u.Text
			}
			if err := s.writeNotification(notification{JSONRPC: "2.0", Method: "session/update", Params: map[string]any{
				"sessionId": p.SessionID,
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": content},
			}}); err != nil {
				// A committed assistant block that cannot reach the client is a
				// failed ACP operation. Keep the first transport error so the prompt
				// response cannot falsely report end_turn after losing output.
				emitMu.Lock()
				if emitErr == nil {
					emitErr = err
				}
				emitMu.Unlock()
				return
			}
		}
		var stop StopReason
		var err error
		if hasRich {
			rich, ok := sess.(PromptContentSession)
			if !ok {
				return nil, invalidParams("rich prompt content is unsupported"), false
			}
			stop, err = rich.PromptContent(ctx, p.Prompt, emit)
		} else {
			stop, err = sess.Prompt(ctx, text.String(), emit)
		}
		emitMu.Lock()
		writeErr := emitErr
		emitMu.Unlock()
		// ACP settlement precedence is cancellation, output-delivery failure,
		// then the underlying prompt failure. A client must not receive a less
		// actionable model error when its committed output was also lost.
		if writeErr != nil {
			return nil, internalError(fmt.Errorf("ACP session/update delivery failed: %w", writeErr)), false
		}
		if err != nil {
			return nil, internalError(err), false
		}
		if stop == "" {
			stop = StopEndTurn
		}
		return map[string]string{"stopReason": string(stop)}, nil, false
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := decodeParams(raw, &p); err != nil || p.SessionID == "" {
			return nil, invalidParams("session/cancel requires sessionId"), false
		}
		sess, ok := s.session(p.SessionID)
		if !ok {
			// ACP cancellation is deliberately idempotent. A client may race
			// disconnect/reconnect or send a stale cancellation after the
			// addressed session has already been disposed; that must not turn a
			// best-effort notification into a protocol error.
			return nil, nil, true
		}
		if err := sess.Cancel(); err != nil {
			return nil, internalError(err), false
		}
		return nil, nil, true
	case "session/request_permission":
		return nil, invalidParams("permission requests are unsupported"), false
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}, false
	}
}

// toolCatalog captures the optional provider inventory. nil means the
// extension is intentionally absent, not that the deployment has no tools.
func (s *Server) toolCatalog(ctx context.Context) (*ToolCatalog, error) {
	provider, ok := s.Factory.(ToolCatalogProvider)
	if !ok {
		return nil, nil
	}
	catalog, err := provider.ToolCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("tool catalog: %w", err)
	}
	if catalog.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported tool catalog schema version %d", catalog.SchemaVersion)
	}
	if strings.TrimSpace(catalog.Digest) == "" {
		return nil, errors.New("tool catalog digest is required")
	}
	return &catalog, nil
}

func (s *Server) replaceSession(id string, sess Session) (old Session, busy bool, ok bool) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.closed {
		return nil, false, false
	}
	if s.promptActive != nil && s.promptActive[id] {
		return nil, true, false
	}
	old = s.sessions[id]
	if s.sessions == nil {
		s.sessions = make(map[string]Session)
	}
	s.sessions[id] = sess
	return old, false, true
}

// isAbsoluteACPPath follows ACP's absolute-CWD requirement on every host.
// filepath.IsAbs is authoritative for the current OS; the drive/UNC forms
// are also accepted while running protocol tests on a different OS so a
// Windows client does not get a platform-dependent protocol error.
func isAbsoluteACPPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return value != ""
	}
	if strings.HasPrefix(value, `\\`) || strings.HasPrefix(value, "//") {
		return true
	}
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}

type permissionRequesterFunc struct {
	server    *Server
	sessionID string
}

func (p permissionRequesterFunc) RequestPermission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
	if req.SessionID == "" {
		req.SessionID = p.sessionID
	}
	return p.server.requestPermission(ctx, req)
}

func (s *Server) requestPermission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
	if err := ctx.Err(); err != nil {
		return PermissionOutcome{}, err
	}
	if req.SessionID == "" || req.ToolCallID == "" || req.ToolName == "" {
		return PermissionOutcome{}, errors.New("acp: permission request requires session, tool call id and tool name")
	}
	s.permissionMu.Lock()
	s.nextPermissionID++
	id := fmt.Sprintf("permission-%d", s.nextPermissionID)
	ch := make(chan permissionReply, 1)
	s.permissions[id] = ch
	s.permissionMu.Unlock()
	params := map[string]any{
		"sessionId": req.SessionID,
		"toolCall": map[string]any{
			"toolCallId": req.ToolCallID,
			"name":       req.ToolName,
		},
		"reason":  req.Reason,
		"options": req.Options,
	}
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "params": params}); err != nil {
		s.removePermission(id)
		return PermissionOutcome{}, err
	}
	defer s.removePermission(id)
	select {
	case <-ctx.Done():
		return PermissionOutcome{}, ctx.Err()
	case reply := <-ch:
		if reply.err != nil {
			return PermissionOutcome{}, errors.New(reply.err.Message)
		}
		if reply.outcome.Outcome == "" && reply.outcome.OptionID == "" {
			return PermissionOutcome{}, errors.New("acp: permission response is empty")
		}
		return reply.outcome, nil
	}
}

func (s *Server) removePermission(id string) {
	s.permissionMu.Lock()
	delete(s.permissions, id)
	s.permissionMu.Unlock()
}

func (s *Server) cancelPermissions(err *rpcError) {
	s.permissionMu.Lock()
	pending := make([]chan permissionReply, 0, len(s.permissions))
	for id, ch := range s.permissions {
		delete(s.permissions, id)
		pending = append(pending, ch)
	}
	s.permissionMu.Unlock()
	for _, ch := range pending {
		ch <- permissionReply{err: err}
	}
}

func (s *Server) resolvePermissionResponse(id json.RawMessage, raw []byte) bool {
	if len(id) == 0 || string(id) == "null" {
		return false
	}
	key := string(id)
	if id[0] == '"' {
		var decoded string
		if err := json.Unmarshal(id, &decoded); err != nil {
			return false
		}
		key = decoded
	}
	var incoming struct {
		Error  *rpcError       `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &incoming); err != nil || (incoming.Result == nil && incoming.Error == nil) {
		return false
	}
	reply := permissionReply{err: incoming.Error}
	if incoming.Error == nil {
		if err := decodePermissionOutcome(incoming.Result, &reply.outcome); err != nil {
			return false
		}
	}
	s.permissionMu.Lock()
	ch, ok := s.permissions[key]
	if ok {
		delete(s.permissions, key)
	}
	s.permissionMu.Unlock()
	if !ok {
		return false
	}
	ch <- reply
	return true
}

func decodePermissionOutcome(raw json.RawMessage, out *PermissionOutcome) error {
	var direct PermissionOutcome
	if err := json.Unmarshal(raw, &direct); err == nil && (direct.Outcome != "" || direct.OptionID != "") {
		*out = direct
		return nil
	}
	var nested struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil || (nested.Outcome.Outcome == "" && nested.Outcome.OptionID == "") {
		return errors.New("invalid permission outcome")
	}
	*out = PermissionOutcome{Outcome: nested.Outcome.Outcome, OptionID: nested.Outcome.OptionID}
	return nil
}

func (s *Server) session(id string) (Session, bool) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *Server) beginPrompt(id string) (Session, bool, bool) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false, false
	}
	if s.promptActive == nil {
		s.promptActive = make(map[string]bool)
	}
	if s.promptActive[id] {
		return sess, true, true
	}
	s.promptActive[id] = true
	return sess, false, true
}

func (s *Server) endPrompt(id string) {
	s.promptMu.Lock()
	delete(s.promptActive, id)
	s.promptMu.Unlock()
}

func (s *Server) closeSessions() {
	s.sessionsMu.Lock()
	if s.closed {
		s.sessionsMu.Unlock()
		return
	}
	s.closed = true
	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.Unlock()
	for _, sess := range sessions {
		_ = sess.Close()
	}
}

func (s *Server) cancelSessions() {
	s.sessionsMu.Lock()
	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.Unlock()
	for _, sess := range sessions {
		_ = sess.Cancel()
	}
}

func (s *Server) writeResponse(v response) error {
	return s.write(v)
}

func (s *Server) writeNotification(v notification) error {
	return s.write(v)
}

func (s *Server) write(v any) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := s.Out.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}

func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("missing params")
	}
	return json.Unmarshal(raw, dst)
}

func invalidParams(message string) *rpcError { return &rpcError{Code: -32602, Message: message} }
func internalError(err error) *rpcError {
	return &rpcError{Code: -32603, Message: "internal error", Data: err.Error()}
}
func nullID() json.RawMessage   { return json.RawMessage("null") }
func newSessionID(n int) string { return fmt.Sprintf("shutu-%d", n) }

var canonicalBase64 = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

func validCanonicalBase64(value string) bool {
	if value == "" || !canonicalBase64.MatchString(value) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	return err == nil && base64.StdEncoding.EncodeToString(decoded) == value
}

func validImageMediaType(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}
