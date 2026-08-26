package webserver

// This file is the narrow DSH Connection wire adapter. The regular REST API
// remains the Shutu shell contract; these endpoints expose the same store and
// turn handlers as DSH's browser client-request/server-response transport.

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

const (
	nativeRPCTypeRequest       = "client-request"
	nativeRPCTypeResponse      = "server-response"
	nativeRPCTypeServerRequest = "server-request"
	nativeMuxPath              = "/api/events.mux"
	nativeHostPath             = "/api/events.host"
	webSocketGUID              = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	nativeSettingsOnboarding   = "ui-onboarding"
)

var nativeSettingsSchema = map[string]any{
	"uid": 0,
	"refs": map[string]any{
		"0": map[string]any{"uid": 0, "type": "any", "meta": map[string]any{}},
	},
}

type nativeSettingsDocument struct {
	Value    map[string]any
	Revision int
}

type nativeSettingsPathOp struct {
	Op    string   `json:"op"`
	Path  []string `json:"path"`
	Value any      `json:"value"`
}

type nativeSettingsViewRequest struct {
	Namespace string                 `json:"ns"`
	Ops       []nativeSettingsPathOp `json:"ops"`
	Expected  *int                   `json:"expectedRevision"`
}

type nativeRPCRequest struct {
	Type    string          `json:"type"`
	RPCID   string          `json:"rpcId"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

type nativeRPCResponse struct {
	Type   string          `json:"type"`
	RPCID  string          `json:"rpcId"`
	Result nativeRPCResult `json:"result"`
}

type nativeRPCResult struct {
	OK    bool            `json:"ok"`
	Value any             `json:"value,omitempty"`
	Error *nativeRPCError `json:"error,omitempty"`
}

type nativeRPCError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type nativePromptPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
	Name      string `json:"name"`
}

type nativeSessionListItem struct {
	SessionID       string `json:"sessionId"`
	UpdatedAt       int64  `json:"updatedAt"`
	Running         bool   `json:"running"`
	Blank           bool   `json:"blank"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	Origin          string `json:"origin,omitempty"`
	CWD             string `json:"cwd,omitempty"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

type nativeSessionEvent struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

type nativeHistoryEntry struct {
	Event nativeSessionEvent `json:"event"`
}

type nativeHistoryRequest struct {
	SessionID   string  `json:"sessionId"`
	BeforeSeq   *uint64 `json:"beforeSeq"`
	MaxMessages int     `json:"maxMessages"`
}

type nativeSessionCreateRequest struct {
	WorkspaceID string `json:"workspaceId"`
	CWD         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	AgentPreset string `json:"agentPreset"`
}

type nativeSessionPromptRequest struct {
	SessionID      string             `json:"sessionId"`
	Mode           string             `json:"mode"`
	Content        []nativePromptPart `json:"content"`
	ClientTimeZone string             `json:"clientTimeZone"`
}

type nativeSessionRenameRequest struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

type nativeSessionIDRequest struct {
	SessionID string `json:"sessionId"`
}

type nativeSessionSearchRequest struct {
	Query string `json:"query"`
}

type nativeWorkspaceView struct {
	WorkspaceID string   `json:"workspaceId"`
	Title       string   `json:"title"`
	Path        string   `json:"path,omitempty"`
	SessionIDs  []string `json:"sessionIds"`
}

type nativeWorkspaceListValue struct {
	Items []nativeWorkspaceView `json:"items"`
}

type nativeEventEnvelope struct {
	Type    string `json:"type"`
	RPCID   string `json:"rpcId"`
	Method  string `json:"method"`
	Payload any    `json:"payload"`
}

type nativeSessionEventFrame struct {
	Type      string             `json:"type"`
	SessionID string             `json:"sessionId"`
	Event     nativeSessionEvent `json:"event"`
}

type nativeSubscribedFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	LastSeq   int64  `json:"lastSeq"`
}

func (s *Server) registerNativeRoutes(mux *http.ServeMux) {
	for _, method := range []string{
		"host.describe", "session.list", "session.search", "session.create",
		"session.history", "session.rename", "session.prompt", "session.cancel",
		"workspace.list", "agentPreset.list", "settings.describe",
		"settings.mutate", "credentials.describe", "dynamicCordisRunner/syncInspectManifest",
		"dynamicCordisRunner/inventory", "llm.providers", "llm.models",
	} {
		mux.Handle("POST /api/"+method, s.requireAuth(http.HandlerFunc(s.handleNativeRPC)))
	}
	mux.Handle("GET "+nativeMuxPath, s.requireAuth(http.HandlerFunc(s.handleNativeMuxWebSocket)))
	mux.Handle("GET "+nativeHostPath, s.requireAuth(http.HandlerFunc(s.handleNativeHostWebSocket)))
}

func (s *Server) handleNativeRPC(w http.ResponseWriter, r *http.Request) {
	var req nativeRPCRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "body is not JSON", http.StatusBadRequest)
		return
	}
	if req.Type != nativeRPCTypeRequest || strings.TrimSpace(req.RPCID) == "" || strings.TrimSpace(req.Method) == "" {
		http.Error(w, "invalid client-request message", http.StatusBadRequest)
		return
	}
	if expected := strings.TrimPrefix(r.URL.Path, "/api/"); req.Method != expected {
		s.writeNativeRPC(w, req.RPCID, nativeRPCFailure("bad-request", "method does not match endpoint", nil))
		return
	}
	result := s.dispatchNativeRPC(r, req.Method, req.Payload)
	s.writeNativeRPC(w, req.RPCID, result)
}

func (s *Server) writeNativeRPC(w http.ResponseWriter, rpcID string, result nativeRPCResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(nativeRPCResponse{Type: nativeRPCTypeResponse, RPCID: rpcID, Result: result})
}

func nativeRPCSuccess(value any) nativeRPCResult { return nativeRPCResult{OK: true, Value: value} }

func nativeRPCFailure(code, message string, details map[string]any) nativeRPCResult {
	return nativeRPCResult{OK: false, Error: &nativeRPCError{Code: code, Message: message, Details: details}}
}

func nativeDecode(raw json.RawMessage, value any) nativeRPCResult {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return nativeRPCFailure("bad-request", "payload is invalid JSON", map[string]any{"message": err.Error()})
	}
	return nativeRPCResult{}
}

func (s *Server) nativeSettingsDescribe() nativeRPCResult {
	s.nativeSettingsMu.Lock()
	defer s.nativeSettingsMu.Unlock()
	document := s.nativeSettings[nativeSettingsOnboarding]
	return nativeRPCSuccess(map[string]any{
		"writable":    true,
		"hasDocument": true,
		"namespaces":  []any{s.nativeSettingsView(nativeSettingsOnboarding, document)},
	})
}

func (s *Server) nativeSettingsMutate(raw json.RawMessage) nativeRPCResult {
	var req nativeSettingsViewRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.Namespace) == "" {
		return nativeRPCFailure("bad-request", "ns is required", nil)
	}
	if req.Namespace != nativeSettingsOnboarding {
		return nativeRPCFailure("not-found", "native settings namespace is not registered", map[string]any{"ns": req.Namespace})
	}

	s.nativeSettingsMu.Lock()
	defer s.nativeSettingsMu.Unlock()
	document := s.nativeSettings[req.Namespace]
	if document.Value == nil {
		document.Value = map[string]any{}
	}
	if req.Expected != nil && *req.Expected != document.Revision {
		return nativeRPCFailure("settings-conflict", "settings changed since it was read", map[string]any{
			"expectedRevision": *req.Expected,
			"actualRevision":   document.Revision,
		})
	}
	for _, op := range req.Ops {
		if err := applyNativeSettingsOp(&document.Value, op); err != nil {
			return nativeRPCFailure("settings-rejected", err.Error(), nil)
		}
	}
	if len(req.Ops) > 0 {
		document.Revision++
	}
	s.nativeSettings[req.Namespace] = document
	return nativeRPCSuccess(s.nativeSettingsView(req.Namespace, document))
}

func (s *Server) nativeSettingsView(namespace string, document nativeSettingsDocument) map[string]any {
	value := cloneNativeSettingsMap(document.Value)
	return map[string]any{
		"ns":       namespace,
		"schema":   cloneNativeSettingsValue(nativeSettingsSchema),
		"value":    value,
		"user":     cloneNativeSettingsMap(document.Value),
		"applies":  "live",
		"secrets":  []any{},
		"revision": document.Revision,
	}
}

func applyNativeSettingsOp(root *map[string]any, op nativeSettingsPathOp) error {
	if op.Op != "set" && op.Op != "unset" {
		return fmt.Errorf("unsupported settings operation %q", op.Op)
	}
	if len(op.Path) == 0 {
		if op.Op == "unset" {
			*root = map[string]any{}
			return nil
		}
		value, ok := op.Value.(map[string]any)
		if !ok {
			return errors.New("setting the section root requires an object")
		}
		*root = cloneNativeSettingsMap(value)
		return nil
	}
	current := *root
	for _, key := range op.Path[:len(op.Path)-1] {
		child, ok := current[key].(map[string]any)
		if !ok {
			child = map[string]any{}
			current[key] = child
		}
		current = child
	}
	leaf := op.Path[len(op.Path)-1]
	if op.Op == "unset" {
		delete(current, leaf)
	} else {
		current[leaf] = cloneNativeSettingsValue(op.Value)
	}
	return nil
}

func cloneNativeSettingsMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneNativeSettingsValue(value)
	}
	return cloned
}

func cloneNativeSettingsValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneNativeSettingsMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneNativeSettingsValue(item)
		}
		return cloned
	default:
		return value
	}
}

func (s *Server) dispatchNativeRPC(r *http.Request, method string, raw json.RawMessage) nativeRPCResult {
	switch method {
	case "host.describe":
		metas, err := s.store.ListSessions(r.Context())
		if err != nil {
			return nativeRPCFailure("internal", err.Error(), nil)
		}
		attached := 0
		for _, m := range metas {
			if s.statusFn != nil && s.statusFn(r.Context(), m).State != "" {
				attached++
			}
		}
		value := map[string]any{
			"version":          "shutu-agent",
			"cwd":              s.defaultWorkdir,
			"attachedSessions": attached,
			"home":             "",
			"canOpenPath":      false,
		}
		if cfg := s.cfgFn; cfg != nil {
			view := cfg()
			if provider, ok := view["provider"].(string); ok {
				value["provider"] = provider
			}
			if model, ok := view["model"].(string); ok {
				value["model"] = model
			}
		}
		return nativeRPCSuccess(value)
	case "session.list":
		return s.nativeSessionList(r)
	case "session.search":
		var req nativeSessionSearchRequest
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		query := strings.TrimSpace(req.Query)
		if query == "" {
			return nativeRPCFailure("bad-request", "query is required", nil)
		}
		hits, err := s.store.SearchSessions(r.Context(), query)
		if err != nil {
			return nativeRPCFailure("internal", err.Error(), nil)
		}
		items := make([]map[string]any, 0, len(hits))
		for _, hit := range hits {
			items = append(items, map[string]any{"sessionId": hit.SessionID, "snippet": hit.Snippet})
		}
		return nativeRPCSuccess(map[string]any{"items": items, "hasMore": false})
	case "session.history":
		return s.nativeSessionHistory(r, raw)
	case "session.create":
		return s.nativeSessionCreate(r, raw)
	case "session.rename":
		var req nativeSessionRenameRequest
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		title := session.NormalizeTitle(req.Title, session.TitleMaxBytes)
		if err := s.store.SetSessionTitle(r.Context(), req.SessionID, title, session.TitleSourceUser); err != nil {
			return nativeStoreFailure(err)
		}
		return nativeRPCSuccess(map[string]any{"title": title, "seq": 0})
	case "session.prompt":
		return s.nativeSessionPrompt(r, raw)
	case "session.cancel":
		var req nativeSessionIDRequest
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		if s.stopFn == nil {
			return nativeRPCFailure("not-supported", "turn stopper not wired", nil)
		}
		if err := s.stopFn(req.SessionID); err != nil {
			return nativeRPCFailure("cancel-failed", err.Error(), nil)
		}
		return nativeRPCSuccess(map[string]any{"accepted": true})
	case "workspace.list":
		return s.nativeWorkspaceList(r)
	case "agentPreset.list":
		// Shutu does not persist DSH Agent Preset documents yet. Returning the
		// protocol's empty catalog keeps the native settings surface usable and
		// makes the capability boundary explicit to the client.
		return nativeRPCSuccess(map[string]any{
			"presets": []any{}, "authorable": false, "hasDocument": false,
		})
	case "settings.describe":
		return s.nativeSettingsDescribe()
	case "settings.mutate":
		return s.nativeSettingsMutate(raw)
	case "credentials.describe":
		var req struct {
			Refs []string `json:"refs"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		credentials := make(map[string]any, len(req.Refs))
		for _, ref := range req.Refs {
			// Never return a secret or infer configuration from process
			// environment here. The DSH UI can still render the credential
			// boundary and will report it as unavailable/read-only.
			credentials[ref] = map[string]any{"configured": false, "writable": false}
		}
		return nativeRPCSuccess(map[string]any{"credentials": credentials})
	case "llm.providers":
		provider := "deepseek-official"
		displayName := "DeepSeek"
		if cfg := s.cfgFn; cfg != nil {
			view := cfg()
			if configured, ok := view["provider"].(string); ok && strings.TrimSpace(configured) != "" {
				provider = configured
				displayName = configured
			}
		}
		return nativeRPCSuccess(map[string]any{"providers": []any{
			map[string]any{
				"provider": provider, "displayName": displayName,
				"settingsNs": "llm-deepseek", "settingsPath": []string{},
				"active": true,
			},
		}})
	case "llm.models":
		// The native model catalog is intentionally empty until the live
		// provider adapter is exposed through the clean DSH RPC surface. The
		// response shape still lets Models render its empty/loading state.
		return nativeRPCSuccess(map[string]any{"groups": []any{}, "failures": []any{}})
	case "dynamicCordisRunner/syncInspectManifest":
		return nativeRPCSuccess(nil)
	case "dynamicCordisRunner/inventory":
		return nativeRPCSuccess([]any{})
	default:
		return nativeRPCFailure("not-supported", "native RPC method is not implemented", map[string]any{"method": method})
	}
}

func nativeStoreFailure(err error) nativeRPCResult {
	if errors.Is(err, store.ErrNotFound) {
		return nativeRPCFailure("not-found", "session not found", nil)
	}
	return nativeRPCFailure("internal", err.Error(), nil)
}

func (s *Server) nativeSessionList(r *http.Request) nativeRPCResult {
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	items := make([]nativeSessionListItem, 0, len(metas))
	for _, m := range metas {
		if !m.ArchivedAt.IsZero() {
			continue
		}
		item := nativeSessionListItem{
			SessionID: m.ID, UpdatedAt: m.UpdatedAt.UnixMilli(), Running: false,
			Blank: m.EventCount == 0, CWD: m.CWD,
		}
		if s.statusFn != nil {
			item.Running = s.statusFn(r.Context(), m).State == "ongoing"
		}
		items = append(items, item)
	}
	return nativeRPCSuccess(map[string]any{"items": items})
}

func (s *Server) nativeSessionHistory(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeHistoryRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	limit := req.MaxMessages
	if limit <= 0 || limit > maxEventPageLimit {
		limit = defaultEventPageLimit
	}
	var before uint64
	if req.BeforeSeq != nil {
		before = *req.BeforeSeq
	}
	events, hasMore, err := s.store.LoadSessionPage(r.Context(), req.SessionID, before, 0, limit)
	if err != nil {
		return nativeStoreFailure(err)
	}
	entries := make([]nativeHistoryEntry, 0, len(events))
	for _, ev := range events {
		entries = append(entries, nativeHistoryEntry{Event: nativeEvent(ev)})
	}
	return nativeRPCSuccess(map[string]any{"events": entries, "hasMore": hasMore})
}

func (s *Server) nativeSessionCreate(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.sessFn == nil {
		return nativeRPCFailure("not-supported", "session manager not wired", nil)
	}
	var req nativeSessionCreateRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	id, err := s.sessFn(r.Context(), "new", req.SessionID)
	if err != nil {
		return nativeRPCFailure("session-create-failed", err.Error(), nil)
	}
	if req.WorkspaceID != "" {
		if err := s.store.SetSessionWorkspace(r.Context(), id, req.WorkspaceID); err != nil {
			return nativeRPCFailure("workspace-attach-failed", err.Error(), nil)
		}
		if err := s.syncSessionCWD(r.Context(), id, req.WorkspaceID); err != nil {
			return nativeRPCFailure("workspace-attach-failed", err.Error(), nil)
		}
	}
	if req.CWD != "" {
		if headers, ok := s.store.(store.SessionHeaderStore); ok {
			if err := headers.SetSessionCWD(r.Context(), id, req.CWD); err != nil {
				return nativeRPCFailure("session-create-failed", err.Error(), nil)
			}
		}
	}
	return nativeRPCSuccess(map[string]any{"sessionId": id})
}

func (s *Server) nativeSessionPrompt(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionPromptRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if s.msgFn == nil {
		return nativeRPCFailure("not-supported", "message handler not wired", nil)
	}
	if req.Mode != "queue" && req.Mode != "steer" {
		return nativeRPCFailure("bad-request", "mode must be queue or steer", nil)
	}
	var text string
	for _, part := range req.Content {
		switch part.Type {
		case "text":
			text += part.Text
		case "image":
			return nativeRPCFailure("not-supported", "native image prompt is not wired", nil)
		default:
			return nativeRPCFailure("bad-request", "unsupported prompt content type", nil)
		}
	}
	if strings.TrimSpace(text) == "" {
		return nativeRPCFailure("bad-request", "text content is required", nil)
	}
	if err := s.msgFn(r.Context(), req.SessionID, text, []llm.ImageRef{}); err != nil {
		return nativeRPCFailure("prompt-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"accepted": true})
}

func (s *Server) nativeWorkspaceList(r *http.Request) nativeRPCResult {
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	byWorkspace := make(map[string][]string)
	for _, m := range metas {
		if m.ArchivedAt.IsZero() && m.WorkspaceID != "" {
			byWorkspace[m.WorkspaceID] = append(byWorkspace[m.WorkspaceID], m.ID)
		}
	}
	items := make([]nativeWorkspaceView, 0, len(workspaces))
	for _, ws := range workspaces {
		ids := append([]string(nil), byWorkspace[ws.ID]...)
		sort.Strings(ids)
		items = append(items, nativeWorkspaceView{WorkspaceID: ws.ID, Title: ws.Title, Path: ws.Path, SessionIDs: ids})
	}
	return nativeRPCSuccess(nativeWorkspaceListValue{Items: items})
}

func nativeEvent(ev session.Event) nativeSessionEvent {
	data := ev.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return nativeSessionEvent{Seq: ev.Seq, Type: ev.Type, Time: ev.At.UnixMilli(), Data: data}
}

func (s *Server) handleNativeMuxWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.evSrc == nil {
		http.Error(w, "event source not wired", http.StatusNotImplemented)
		return
	}
	conn, reader, err := upgradeNativeWebSocket(w, r)
	if err != nil {
		return
	}
	ctx, cancel := contextWithConnection(r)
	defer cancel()
	defer conn.Close()
	var writes sync.Mutex
	write := func(payload any) error {
		method := ""
		switch frame := payload.(type) {
		case nativeSubscribedFrame:
			method = frame.Type
		case nativeSessionEventFrame:
			method = frame.Type
		}
		body, err := json.Marshal(nativeEventEnvelope{
			Type: nativeRPCTypeServerRequest, RPCID: nativeRPCID(), Method: method, Payload: payload,
		})
		if err != nil {
			return err
		}
		writes.Lock()
		defer writes.Unlock()
		return writeNativeWebSocketText(conn, body)
	}
	metas, err := s.store.ListSessions(ctx)
	if err != nil {
		return
	}
	unsubs := make([]func(), 0, len(metas))
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}()
	for _, meta := range metas {
		if !meta.ArchivedAt.IsZero() {
			continue
		}
		events, loadErr := s.store.LoadSession(ctx, meta.ID)
		if loadErr != nil {
			continue
		}
		lastSeq := int64(-1)
		if len(events) > 0 {
			lastSeq = int64(events[len(events)-1].Seq)
		}
		if err := write(nativeSubscribedFrame{Type: "session/subscribed", SessionID: meta.ID, LastSeq: lastSeq}); err != nil {
			return
		}
		unsubs = append(unsubs, s.evSrc(meta.ID, func(ev session.Event) {
			_ = write(nativeSessionEventFrame{Type: "session/event", SessionID: meta.ID, Event: nativeEvent(ev)})
		}))
	}
	go drainNativeWebSocket(reader, cancel)
	<-ctx.Done()
}

func (s *Server) handleNativeHostWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, reader, err := upgradeNativeWebSocket(w, r)
	if err != nil {
		return
	}
	ctx, cancel := contextWithConnection(r)
	defer cancel()
	defer conn.Close()
	go drainNativeWebSocket(reader, cancel)
	<-ctx.Done()
}

func contextWithConnection(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithCancel(r.Context())
}

func nativeRPCID() string {
	return fmt.Sprintf("shutu-%d", time.Now().UnixNano())
}

func upgradeNativeWebSocket(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "upgrade required", http.StatusUpgradeRequired)
		return nil, nil, errors.New("websocket upgrade headers missing")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return nil, nil, errors.New("websocket key missing")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return nil, nil, errors.New("hijacking unsupported")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	hash := sha1.Sum([]byte(key + webSocketGUID))
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(hash[:]) + "\r\n\r\n"
	if _, err := rw.WriteString(response); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw.Reader, nil
}

func headerHasToken(value, want string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), want) {
			return true
		}
	}
	return false
}

// Hijack keeps the authentication/panic wrapper transparent to the native
// WebSocket route. The standard net/http server exposes Hijacker only through
// the concrete ResponseWriter, so wrapping it without forwarding this method
// would make every authenticated upgrade fail as unsupported.
func (w *panicSafeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("webserver: websocket hijacking unsupported")
	}
	w.wrote = true
	return hijacker.Hijack()
}

func writeNativeWebSocketText(conn net.Conn, payload []byte) error {
	header := []byte{0x81}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(len(payload)))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(len(payload)))
	}
	_, err := conn.Write(append(header, payload...))
	return err
}

func drainNativeWebSocket(reader *bufio.Reader, cancel context.CancelFunc) {
	defer cancel()
	for {
		first, err := reader.ReadByte()
		if err != nil {
			return
		}
		second, err := reader.ReadByte()
		if err != nil {
			return
		}
		masked := second&0x80 != 0
		length := int64(second & 0x7f)
		if length == 126 {
			var n uint16
			if binary.Read(reader, binary.BigEndian, &n) != nil {
				return
			}
			length = int64(n)
		} else if length == 127 {
			var n uint64
			if binary.Read(reader, binary.BigEndian, &n) != nil || n > uint64(4<<20) {
				return
			}
			length = int64(n)
		}
		if length < 0 || length > 4<<20 {
			return
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(reader, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		opcode := first & 0x0f
		if opcode == 8 {
			return
		}
		// The native carrier is downlink-only. Ignore text/binary client data;
		// accepting ping keeps browser/proxy idle checks healthy.
		if opcode == 9 { /* pong is best-effort and the read side owns closure */
		}
	}
}
