package webserver

// This file is the narrow DSH Connection wire adapter. The regular REST API
// remains the Shutu shell contract; these endpoints expose the same store and
// turn handlers as DSH's browser client-request/server-response transport.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	nativeSettingsDeepSeek     = "llm-deepseek"
	nativeSettingsPiAI         = "llm-pi-ai"
	nativeDirectoryMaxEntries  = 1000
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

func nativeSettingsSchemaFor(namespace string) map[string]any {
	if namespace != nativeSettingsPiAI {
		return cloneNativeSettingsValue(nativeSettingsSchema).(map[string]any)
	}
	// This is the serialized schemastery graph needed by the DSH model editor:
	// providers is a keyed object, each profile has an api field, and api is the
	// same four-protocol union the editor reads for custom routes.
	return map[string]any{
		"uid": 0,
		"refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{}}, "dict": map[string]any{"providers": 1}},
			"1": map[string]any{"uid": 1, "type": "dict", "meta": map[string]any{"default": map[string]any{}}, "inner": 2, "sKey": 3},
			"2": map[string]any{"uid": 2, "type": "object", "meta": map[string]any{"default": map[string]any{}}, "dict": map[string]any{
				"api": 4, "apiKeyEnv": 5, "baseURL": 6, "displayName": 7, "model": 8, "models": 9,
			}},
			"3":  map[string]any{"uid": 3, "type": "string", "meta": map[string]any{}},
			"4":  map[string]any{"uid": 4, "type": "union", "meta": map[string]any{}, "list": []any{10, 11, 12, 13}},
			"5":  map[string]any{"uid": 5, "type": "string", "meta": map[string]any{}},
			"6":  map[string]any{"uid": 6, "type": "string", "meta": map[string]any{}},
			"7":  map[string]any{"uid": 7, "type": "string", "meta": map[string]any{}},
			"8":  map[string]any{"uid": 8, "type": "string", "meta": map[string]any{}},
			"9":  map[string]any{"uid": 9, "type": "array", "meta": map[string]any{"default": []any{}}, "inner": 14},
			"10": map[string]any{"uid": 10, "type": "const", "meta": map[string]any{}, "value": "openai-completions"},
			"11": map[string]any{"uid": 11, "type": "const", "meta": map[string]any{}, "value": "openai-responses"},
			"12": map[string]any{"uid": 12, "type": "const", "meta": map[string]any{}, "value": "anthropic-messages"},
			"13": map[string]any{"uid": 13, "type": "const", "meta": map[string]any{}, "value": "google-generative-ai"},
			"14": map[string]any{"uid": 14, "type": "object", "meta": map[string]any{"default": map[string]any{}}, "dict": map[string]any{
				"id": 5, "name": 7, "contextWindow": 15, "maxTokens": 16,
			}},
			"15": map[string]any{"uid": 15, "type": "number", "meta": map[string]any{}},
			"16": map[string]any{"uid": 16, "type": "number", "meta": map[string]any{}},
		},
	}
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
	SessionID       string                 `json:"sessionId"`
	Title           string                 `json:"title,omitempty"`
	UpdatedAt       int64                  `json:"updatedAt"`
	Running         bool                   `json:"running"`
	Blank           bool                   `json:"blank"`
	ParentSessionID string                 `json:"parentSessionId,omitempty"`
	Origin          string                 `json:"origin,omitempty"`
	CWD             string                 `json:"cwd,omitempty"`
	AgentPreset     string                 `json:"agentPreset,omitempty"`
	Projections     *nativeProjectionBlock `json:"projections,omitempty"`
}

func nativeSessionLineage(events []session.Event) (parentSession, origin string, depth int) {
	for _, ev := range events {
		if ev.Type != session.EventSubagentStart {
			continue
		}
		data := nativeJSONObject(ev.Data)
		parentSession = nativeEventString(data, "parentSession", "parent_session")
		origin = "subagent"
		depth = nativeEventInt(data, "depth", 0)
		if depth < 0 {
			depth = 0
		}
		break
	}
	return parentSession, origin, depth
}

// nativeSessionHeader is the DSH session-header projection. The SQLite
// session metadata currently owns cwd/createdAt; optional lineage and preset
// fields remain omitted until their durable source is available.
type nativeSessionHeader struct {
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	CWD             string `json:"cwd,omitempty"`
	ParentSessionID string `json:"parentSession,omitempty"`
	SeedLength      int    `json:"seedLength,omitempty"`
	Origin          string `json:"origin,omitempty"`
	DelegationDepth int    `json:"delegationDepth,omitempty"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

type nativeSessionEvent struct {
	Seq             uint64          `json:"seq"`
	Type            string          `json:"type"`
	Time            int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	SourceEventSeqs []uint64        `json:"sourceEventSeqs,omitempty"`
	SurfaceOp       any             `json:"surfaceOp,omitempty"`
	Ignorable       bool            `json:"ignorable,omitempty"`
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

type nativeSessionSelectModelRequest struct {
	SessionID       string `json:"sessionId"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
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

type nativeProjectionFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Key       string `json:"key"`
	Value     any    `json:"value"`
	Seq       uint64 `json:"seq"`
}

type nativeSubscribedFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	LastSeq   int64  `json:"lastSeq"`
}

func (s *Server) registerNativeRoutes(mux *http.ServeMux) {
	for _, method := range []string{
		"host.describe", "host.listDirectory", "host.createDirectory", "host.pickDirectory",
		"session.list", "session.search", "session.create",
		"session.history", "session.rename", "session.prompt", "session.cancel", "session.attachment",
		"session.models", "session.selectModel",
		"workspace.list", "agentPreset.list", "agentPreset.select", "settings.describe",
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
	s.ensureNativeSettingsFromConfigLocked()
	namespaces := make([]any, 0, 3)
	for _, namespace := range []string{nativeSettingsOnboarding, nativeSettingsDeepSeek, nativeSettingsPiAI} {
		if document, ok := s.nativeSettings[namespace]; ok {
			namespaces = append(namespaces, s.nativeSettingsView(namespace, document))
		}
	}
	return nativeRPCSuccess(map[string]any{
		"writable":    true,
		"hasDocument": true,
		"namespaces":  namespaces,
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
	if !nativeSettingsNamespace(req.Namespace) {
		return nativeRPCFailure("not-found", "native settings namespace is not registered", map[string]any{"ns": req.Namespace})
	}

	s.nativeSettingsMu.Lock()
	defer s.nativeSettingsMu.Unlock()
	s.ensureNativeSettingsFromConfigLocked()
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
		"schema":   nativeSettingsSchemaFor(namespace),
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

func (s *Server) ensureNativeSettingsFromConfigLocked() {
	if s.nativeSettings == nil {
		s.nativeSettings = make(map[string]nativeSettingsDocument)
	}
	s.nativeSettings[nativeSettingsOnboarding] = s.nativeSettings[nativeSettingsOnboarding]
	if s.cfgFn == nil {
		return
	}
	configView := s.cfgFn()
	providers := nativeConfigProviderMaps(configView["providers"])
	if _, exists := s.nativeSettings[nativeSettingsDeepSeek]; !exists {
		profile := map[string]any{}
		for _, provider := range providers {
			if nativeString(provider["id"]) != "deepseek-official" {
				continue
			}
			nativeCopyProviderProfile(profile, provider)
			break
		}
		s.nativeSettings[nativeSettingsDeepSeek] = nativeSettingsDocument{Value: profile}
	}
	if _, exists := s.nativeSettings[nativeSettingsPiAI]; !exists {
		profiles := map[string]any{}
		for _, provider := range providers {
			id := nativeString(provider["id"])
			if id == "" || id == "deepseek-official" {
				continue
			}
			if !nativeBool(provider["configured"]) && !nativeBool(provider["available"]) && !nativeBool(provider["custom"]) {
				continue
			}
			profile := map[string]any{}
			nativeCopyProviderProfile(profile, provider)
			profiles[id] = profile
		}
		s.nativeSettings[nativeSettingsPiAI] = nativeSettingsDocument{
			Value: map[string]any{"providers": profiles},
		}
	}
}

func nativeSettingsNamespace(namespace string) bool {
	return namespace == nativeSettingsOnboarding || namespace == nativeSettingsDeepSeek || namespace == nativeSettingsPiAI
}

func nativeCopyProviderProfile(profile map[string]any, provider map[string]any) {
	if value := nativeString(provider["env_var"]); value != "" {
		profile["apiKeyEnv"] = value
	}
	if value := nativeString(provider["base_url"]); value != "" {
		profile["baseURL"] = value
	}
	if value := nativeString(provider["protocol"]); value != "" {
		profile["api"] = value
	}
	if value := nativeString(provider["model"]); value != "" {
		profile["model"] = value
	}
	if value := nativeString(provider["name"]); value != "" {
		profile["displayName"] = value
	}
	models := nativeProviderModels(provider)
	if len(models) > 0 {
		profile["models"] = models
	}
}

func nativeProviderModels(provider map[string]any) []any {
	if models := nativeConfigMaps(provider["models"]); len(models) > 0 {
		result := make([]any, 0, len(models))
		for _, model := range models {
			id := nativeString(model["id"])
			if id == "" {
				continue
			}
			entry := map[string]any{"id": id}
			if name := nativeString(model["name"]); name != "" {
				entry["name"] = name
			}
			if contextWindow := nativeNumber(model["context_window"]); contextWindow > 0 {
				entry["contextWindow"] = contextWindow
			}
			if maxTokens := nativeNumber(model["max_tokens"]); maxTokens > 0 {
				entry["maxTokens"] = maxTokens
			}
			result = append(result, entry)
		}
		return result
	}
	candidates := nativeStrings(provider["candidates"])
	result := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, map[string]any{"id": candidate})
	}
	return result
}

func nativeProviderReasoning(provider map[string]any, modelID string) map[string]any {
	encoded, err := json.Marshal(provider["reasoning"])
	if err != nil || string(encoded) == "null" {
		return nil
	}
	var catalog map[string]map[string]any
	if json.Unmarshal(encoded, &catalog) != nil {
		return nil
	}
	metadata, ok := catalog[modelID]
	if !ok {
		return nil
	}
	efforts := nativeConfigMaps(metadata["efforts"])
	resultEfforts := make([]any, 0, len(efforts))
	for _, effort := range efforts {
		id := nativeString(effort["id"])
		if id == "" {
			continue
		}
		name := nativeString(effort["name"])
		if name == "" {
			name = id
		}
		resultEfforts = append(resultEfforts, map[string]any{"id": id, "name": name})
	}
	if len(resultEfforts) == 0 {
		return nil
	}
	result := map[string]any{"efforts": resultEfforts}
	if effort := nativeString(metadata["default_effort"]); effort != "" {
		result["defaultEffort"] = effort
	}
	return result
}

func nativeConfigProviderMaps(value any) []map[string]any {
	return nativeConfigMaps(value)
}

func nativeConfigMaps(value any) []map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) == "null" {
		return nil
	}
	var decoded []map[string]any
	if json.Unmarshal(encoded, &decoded) != nil {
		return nil
	}
	return decoded
}

func nativeStrings(value any) []string {
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) == "null" {
		return nil
	}
	var decoded []string
	if json.Unmarshal(encoded, &decoded) != nil {
		return nil
	}
	return decoded
}

func nativeString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func nativeBool(value any) bool {
	result, _ := value.(bool)
	return result
}

func nativeNumber(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case float64:
		return int(number)
	default:
		return 0
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
	case "host.listDirectory":
		var req struct {
			Path string `json:"path"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeHostListDirectory(req.Path)
	case "host.createDirectory":
		var req struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeHostCreateDirectory(req.Path, req.Name)
	case "host.pickDirectory":
		return s.nativeHostPickDirectory(r)
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
	case "session.attachment":
		return s.nativeSessionAttachment(r, raw)
	case "session.models":
		return s.nativeSessionModels(r, raw)
	case "session.selectModel":
		return s.nativeSessionSelectModel(r, raw)
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
		return s.nativeAgentPresetList()
	case "agentPreset.select":
		return s.nativeAgentPresetSelect(r, raw)
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
		providers := []map[string]any{}
		if s.cfgFn != nil {
			providers = nativeConfigProviderMaps(s.cfgFn()["providers"])
		}
		for _, ref := range req.Refs {
			configured := false
			for _, provider := range providers {
				if nativeString(provider["env_var"]) == ref {
					configured = nativeBool(provider["configured"])
					break
				}
			}
			// Never return a secret. The configured bit is a non-sensitive
			// capability hint already exposed by the existing sanitized config
			// view, allowing DSH to render the correct provider row state.
			credentials[ref] = map[string]any{"configured": configured, "writable": true}
		}
		return nativeRPCSuccess(map[string]any{"credentials": credentials})
	case "llm.providers":
		providers := []map[string]any{}
		if s.cfgFn != nil {
			providers = nativeConfigProviderMaps(s.cfgFn()["providers"])
		}
		if len(providers) == 0 {
			providers = []map[string]any{{
				"id": "deepseek-official", "name": "DeepSeek", "registered": true, "available": true,
			}}
		}
		entries := make([]any, 0, len(providers))
		for _, provider := range providers {
			id := nativeString(provider["id"])
			if id == "" {
				continue
			}
			settingsNS := nativeSettingsPiAI
			settingsPath := []string{"providers", id}
			if id == "deepseek-official" {
				settingsNS = nativeSettingsDeepSeek
				settingsPath = []string{}
			}
			entry := map[string]any{
				"provider": id, "displayName": nativeString(provider["name"]),
				"settingsNs": settingsNS, "settingsPath": settingsPath,
				"active": nativeBool(provider["available"]),
			}
			if entry["displayName"] == "" {
				entry["displayName"] = id
			}
			if nativeBool(provider["custom"]) {
				entry["declared"] = true
			}
			entries = append(entries, entry)
		}
		return nativeRPCSuccess(map[string]any{"providers": entries})
	case "llm.models":
		return s.nativeLLMModels()
	case "dynamicCordisRunner/syncInspectManifest":
		return nativeRPCSuccess(nil)
	case "dynamicCordisRunner/inventory":
		return nativeRPCSuccess([]any{})
	default:
		return nativeRPCFailure("not-supported", "native RPC method is not implemented", map[string]any{"method": method})
	}
}

func (s *Server) nativeHostListDirectory(rawPath string) nativeRPCResult {
	path, err := s.nativeDirectoryPath(rawPath, true)
	if err != nil {
		return nativeRPCFailure("directory-unreadable", err.Error(), map[string]any{"path": strings.TrimSpace(rawPath)})
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("path is not a directory")
		}
		return nativeRPCFailure("directory-unreadable", fmt.Sprintf("cannot list %q: %v", path, err), map[string]any{"path": path})
	}

	// Readdirnames keeps the scan memory bounded even when a host directory has
	// a very large number of children. Retain maxEntries+1 sorted candidates so
	// the extra row proves that the returned level was truncated.
	directory, err := os.Open(path)
	if err != nil {
		return nativeRPCFailure("directory-unreadable", fmt.Sprintf("cannot list %q: %v", path, err), map[string]any{"path": path})
	}
	defer directory.Close()
	entries := make([]workspaceDirectoryEntry, 0, nativeDirectoryMaxEntries+1)
	truncated := false
	for {
		names, readErr := directory.Readdirnames(1)
		for _, name := range names {
			childPath := filepath.Join(path, name)
			childInfo, infoErr := os.Lstat(childPath)
			if infoErr != nil || (!childInfo.IsDir() && childInfo.Mode()&os.ModeSymlink == 0) {
				continue
			}
			if childInfo.Mode()&os.ModeSymlink != 0 {
				targetInfo, targetErr := os.Stat(childPath)
				if targetErr != nil || !targetInfo.IsDir() {
					continue
				}
			}
			candidate := workspaceDirectoryEntry{
				Name: name, Path: childPath, Hidden: strings.HasPrefix(name, "."),
			}
			if nativeInsertDirectoryEntry(&entries, candidate, nativeDirectoryMaxEntries+1) {
				truncated = true
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nativeRPCFailure("directory-unreadable", fmt.Sprintf("cannot list %q: %v", path, readErr), map[string]any{"path": path})
		}
	}
	if len(entries) > nativeDirectoryMaxEntries {
		entries = entries[:nativeDirectoryMaxEntries]
		truncated = true
	}
	home, _ := os.UserHomeDir()
	return nativeRPCSuccess(workspaceDirectoryListing{
		Path: path, Home: home, Crumbs: workspaceDirectoryCrumbs(path), Entries: entries, Truncated: truncated,
	})
}

func nativeInsertDirectoryEntry(entries *[]workspaceDirectoryEntry, candidate workspaceDirectoryEntry, keep int) bool {
	rows := *entries
	index := sort.Search(len(rows), func(index int) bool { return rows[index].Name >= candidate.Name })
	if len(rows) == keep && index == len(rows) {
		return true
	}
	rows = append(rows, workspaceDirectoryEntry{})
	copy(rows[index+1:], rows[index:])
	rows[index] = candidate
	if len(rows) > keep {
		*entries = rows[:keep]
		return true
	}
	*entries = rows
	return false
}

func (s *Server) nativeHostCreateDirectory(rawPath, name string) nativeRPCResult {
	parent, err := s.nativeDirectoryPath(rawPath, false)
	if err != nil {
		return nativeRPCFailure("directory-create-failed", err.Error(), map[string]any{"path": strings.TrimSpace(rawPath)})
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("path is not a directory")
		}
		return nativeRPCFailure("directory-create-failed", fmt.Sprintf("parent directory %q is unavailable: %v", parent, err), map[string]any{"path": parent})
	}
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		target := filepath.Join(parent, name)
		return nativeRPCFailure("directory-create-failed", fmt.Sprintf("%q is not a single path segment", name), map[string]any{"path": target})
	}
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0o755); err != nil {
		if os.IsExist(err) {
			return nativeRPCFailure("directory-exists", fmt.Sprintf("%q already exists", target), map[string]any{"path": target})
		}
		return nativeRPCFailure("directory-create-failed", fmt.Sprintf("cannot create %q: %v", target, err), map[string]any{"path": target})
	}
	return nativeRPCSuccess(map[string]any{"path": filepath.Clean(target)})
}

func (s *Server) nativeDirectoryPath(rawPath string, useDefault bool) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" && useDefault {
		path, err := s.sessionDefaultWorkdir()
		if err != nil {
			return "", fmt.Errorf("resolve default directory: %w", err)
		}
		return filepath.Clean(path), nil
	}
	if path == "" {
		return "", fmt.Errorf("directory path is required")
	}
	if !filepath.IsAbs(path) || (runtime.GOOS == "windows" && filepath.VolumeName(path) == "") {
		return "", fmt.Errorf("%q is not a fully qualified path", path)
	}
	return filepath.Clean(path), nil
}

func (s *Server) nativeHostPickDirectory(r *http.Request) nativeRPCResult {
	if runtime.GOOS != "windows" {
		return nativeRPCFailure("directory-picker-unavailable", "native directory picker is unavailable on this host", map[string]any{"capability": "browse"})
	}
	const script = `Add-Type -AssemblyName System.Windows.Forms; $d=New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description='Select workspace directory'; $d.ShowNewFolderButton=$true; if($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK){[Console]::Write($d.SelectedPath)}`
	cmd := exec.CommandContext(r.Context(), "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	output, err := cmd.Output()
	if err != nil {
		if r.Context().Err() != nil {
			return nativeRPCFailure("cancelled", "directory picker canceled", nil)
		}
		return nativeRPCFailure("internal", fmt.Sprintf("open directory picker: %v", err), nil)
	}
	selected := strings.TrimSpace(string(output))
	if selected == "" {
		return nativeRPCSuccess(map[string]any{"path": nil})
	}
	path, err := s.nativeDirectoryPath(selected, false)
	if err != nil {
		return nativeRPCFailure("directory-picker-unavailable", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"path": path})
}

func nativeStoreFailure(err error) nativeRPCResult {
	if errors.Is(err, store.ErrNotFound) {
		return nativeRPCFailure("not-found", "session not found", nil)
	}
	return nativeRPCFailure("internal", err.Error(), nil)
}

func (s *Server) nativeLLMModels() nativeRPCResult {
	providers := []map[string]any{}
	if s.cfgFn != nil {
		providers = nativeConfigProviderMaps(s.cfgFn()["providers"])
	}
	groups := make([]any, 0, len(providers))
	for _, provider := range providers {
		id := nativeString(provider["id"])
		models := nativeProviderModels(provider)
		if id == "" || len(models) == 0 {
			continue
		}
		items := make([]any, 0, len(models))
		for _, raw := range models {
			model, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			modelID := nativeString(model["id"])
			if modelID == "" {
				continue
			}
			entry := map[string]any{"id": modelID, "name": modelID}
			if name := nativeString(model["name"]); name != "" {
				entry["name"] = name
			}
			if reasoning := nativeProviderReasoning(provider, modelID); reasoning != nil {
				entry["reasoning"] = reasoning
			}
			items = append(items, entry)
		}
		if len(items) == 0 {
			continue
		}
		name := nativeString(provider["name"])
		if name == "" {
			name = id
		}
		groups = append(groups, map[string]any{"id": id, "name": name, "models": items})
	}
	return nativeRPCSuccess(map[string]any{"groups": groups, "failures": []any{}})
}

func (s *Server) nativeAgentPresetList() nativeRPCResult {
	defaultPreset := "standard"
	if s.cfgFn != nil {
		if mode := nativeString(s.cfgFn()["mode"]); nativeAgentPresetKnown(mode) {
			defaultPreset = mode
		}
	}
	presets := []any{
		map[string]any{
			"id": "minimal", "trust": "system", "isDefault": defaultPreset == "minimal",
			"name": "Minimal", "description": "基础只读能力、Shell 与文件编辑",
		},
		map[string]any{
			"id": "standard", "trust": "system", "isDefault": defaultPreset == "standard",
			"name": "Standard", "description": "标准 Shutu 能力集合",
		},
		map[string]any{
			"id": "code", "trust": "system", "isDefault": defaultPreset == "code",
			"name": "Code", "description": "标准能力加程序化 Code Mode",
		},
	}
	return nativeRPCSuccess(map[string]any{
		"presets": presets, "authorable": false, "hasDocument": false,
	})
}

func nativeAgentPresetKnown(preset string) bool {
	switch preset {
	case "minimal", "standard", "code":
		return true
	default:
		return false
	}
}

func (s *Server) nativeAgentPresetSelect(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req struct {
		SessionID   string `json:"sessionId"`
		AgentPreset string `json:"agentPreset"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.AgentPreset = strings.TrimSpace(req.AgentPreset)
	if req.SessionID == "" || req.AgentPreset == "" {
		return nativeRPCFailure("bad-request", "sessionId and agentPreset are required", nil)
	}
	if !nativeAgentPresetKnown(req.AgentPreset) {
		return nativeRPCFailure("agent-preset-invalid", "agent preset is not available", map[string]any{"agentPreset": req.AgentPreset})
	}
	configs, ok := s.store.(store.SessionConfigStore)
	if !ok {
		return nativeRPCFailure("not-supported", "session configuration store is not wired", nil)
	}
	events, err := s.store.LoadSession(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	if len(events) != 0 {
		return nativeRPCFailure("agent-preset-locked", "agent preset can only change on a blank session", map[string]any{"sessionId": req.SessionID})
	}
	config, err := configs.GetSessionConfig(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	config.AgentPreset = req.AgentPreset
	if err := configs.SetSessionConfig(r.Context(), req.SessionID, config); err != nil {
		return nativeRPCFailure("agent-preset-select-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"agentPreset": req.AgentPreset})
}

func (s *Server) nativeSessionModels(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionIDRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	if _, err := s.store.GetSessionMeta(r.Context(), req.SessionID); err != nil {
		return nativeStoreFailure(err)
	}
	catalog := s.nativeLLMModels()
	if !catalog.OK {
		return catalog
	}
	value, ok := catalog.Value.(map[string]any)
	if !ok {
		return nativeRPCFailure("internal", "model catalog has invalid shape", nil)
	}
	provider, model := "", ""
	effort := ""
	if s.cfgFn != nil {
		view := s.cfgFn()
		provider = nativeString(view["provider"])
		model = nativeString(view["model"])
	}
	if configs, ok := s.store.(store.SessionConfigStore); ok {
		if config, configErr := configs.GetSessionConfig(r.Context(), req.SessionID); configErr == nil {
			if config.Provider != "" {
				provider = config.Provider
			}
			if config.Model != "" {
				model = config.Model
			}
			effort = config.ReasoningEffort
		}
	}
	current := map[string]any{"provider": provider, "model": model}
	if effort != "" {
		current["reasoningEffort"] = effort
	}
	value["current"] = current
	value["routable"] = provider != "" && model != ""
	return nativeRPCSuccess(value)
}

func (s *Server) nativeSessionSelectModel(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionSelectModelRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	req.ReasoningEffort = strings.TrimSpace(req.ReasoningEffort)
	if req.SessionID == "" || req.Provider == "" || req.Model == "" {
		return nativeRPCFailure("bad-request", "sessionId, provider and model are required", nil)
	}
	if _, err := s.store.GetSessionMeta(r.Context(), req.SessionID); err != nil {
		return nativeStoreFailure(err)
	}
	configs, ok := s.store.(store.SessionConfigStore)
	if !ok {
		return nativeRPCFailure("not-supported", "session configuration store is not wired", nil)
	}
	config, err := configs.GetSessionConfig(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	config.Provider = req.Provider
	config.Model = req.Model
	config.ReasoningEffort = req.ReasoningEffort
	if err := configs.UpdateSessionConfig(r.Context(), req.SessionID, config.Provider, config.Model, config.ReasoningEffort, config.Permission); err != nil {
		return nativeRPCFailure("model-select-failed", err.Error(), nil)
	}
	selected := map[string]any{"provider": req.Provider, "model": req.Model}
	if req.ReasoningEffort != "" {
		selected["reasoningEffort"] = req.ReasoningEffort
	}
	return nativeRPCSuccess(map[string]any{"selected": selected})
}

func (s *Server) nativeSessionAttachment(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req struct {
		SessionID    string `json:"sessionId"`
		AttachmentID string `json:"attachmentId"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.AttachmentID) == "" {
		return nativeRPCFailure("bad-request", "sessionId and attachmentId are required", nil)
	}
	if s.att == nil {
		return nativeRPCFailure("not-supported", "attachment store not wired", nil)
	}
	events, err := s.store.LoadSession(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	if !nativeSessionReferencesAttachment(events, req.AttachmentID) {
		return nativeRPCFailure("attachment-error", "Image is not referenced by this session.", map[string]any{
			"reason": "ATTACHMENT_NOT_REFERENCED",
		})
	}
	ref, err := s.att.GetByID(req.AttachmentID)
	if err != nil {
		return nativeRPCFailure("attachment-error", "Unable to find image attachment.", map[string]any{
			"reason": "ATTACHMENT_NOT_FOUND",
		})
	}
	data, err := s.att.Read(ref)
	if err != nil {
		return nativeRPCFailure("attachment-error", "Unable to read image attachment.", map[string]any{
			"reason": "ATTACHMENT_READ_FAILED",
		})
	}
	return nativeRPCSuccess(map[string]any{
		"attachment": nativeAttachmentRefValue(ref, data),
		"data":       base64.StdEncoding.EncodeToString(data),
	})
}

func nativeSessionReferencesAttachment(events []session.Event, attachmentID string) bool {
	for _, event := range events {
		if nativeValueReferencesAttachment(nativeJSONObject(event.Data), attachmentID) {
			return true
		}
	}
	return false
}

func nativeValueReferencesAttachment(value any, attachmentID string) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if nativeValueReferencesAttachment(item, attachmentID) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if normalized == "attachmentid" {
				if nativeStringValue(item) == attachmentID {
					return true
				}
			}
			if normalized == "image" || normalized == "attachment" {
				if object, ok := item.(map[string]any); ok && nativeAttachmentID(object) == attachmentID {
					return true
				}
			}
			if nativeValueReferencesAttachment(item, attachmentID) {
				return true
			}
		}
	}
	return false
}

func nativeAttachmentID(object map[string]any) string {
	for _, key := range []string{"attachmentId", "attachment_id", "id", "ID"} {
		if value := nativeEventString(object, key); value != "" {
			return value
		}
	}
	return ""
}

func nativeStringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func nativeAttachmentRefValue(ref llm.ImageRef, data []byte) map[string]any {
	width, height := ref.Width, ref.Height
	if width <= 0 || height <= 0 {
		if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			width, height = config.Width, config.Height
		}
	}
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}
	return map[string]any{
		"attachmentId": ref.ID,
		"mediaType":    ref.MediaType,
		"bytes":        len(data),
		"width":        width,
		"height":       height,
	}
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
			SessionID: m.ID, Title: m.Title, UpdatedAt: m.UpdatedAt.UnixMilli(), Running: false,
			Blank: m.EventCount == 0, CWD: m.CWD,
		}
		if events, loadErr := s.store.LoadSession(r.Context(), m.ID); loadErr == nil {
			cursor := newNativeProjectionCursor()
			for _, ev := range events {
				cursor.project(m.ID, ev)
			}
			metadata := cursor.list
			item.Blank = metadata.blank
			item.ParentSessionID, item.Origin, _ = nativeSessionLineage(events)
			if configs, ok := s.store.(store.SessionConfigStore); ok {
				if config, configErr := configs.GetSessionConfig(r.Context(), m.ID); configErr == nil {
					item.AgentPreset = config.AgentPreset
				}
			}
			item.UpdatedAt = m.CreatedAt.UnixMilli()
			if metadata.lastPromptAt != nil && *metadata.lastPromptAt > item.UpdatedAt {
				item.UpdatedAt = *metadata.lastPromptAt
			}
			permission := ""
			if configs, ok := s.store.(store.SessionConfigStore); ok {
				if config, configErr := configs.GetSessionConfig(r.Context(), m.ID); configErr == nil {
					permission = config.Permission
				}
			}
			lastSeq := int64(-1)
			if len(events) > 0 {
				lastSeq = int64(events[len(events)-1].Seq)
			}
			baseline := cursor.projectionBlock(m.Title, lastSeq, permission)
			if limits := s.nativeImageLimitsProjection(); limits != nil {
				baseline.Values["imageLimits"] = limits
			}
			item.Projections = &baseline
		}
		if s.statusFn != nil {
			item.Running = s.statusFn(r.Context(), m).State == "ongoing"
		}
		items = append(items, item)
	}
	return nativeRPCSuccess(map[string]any{"items": items})
}

func (s *Server) nativeImageLimitsProjection() map[string]any {
	if s.att == nil {
		return nil
	}
	return map[string]any{
		"maxImageBytes":        maxWebImageBytes,
		"maxImagesPerMessage":  20,
		"maxMessageImageBytes": 100 * 1024 * 1024,
		"maxImagePixels":       40_000_000,
		"maxImageDimension":    2000,
		"mediaTypes":           []any{"image/png", "image/jpeg", "image/webp", "image/gif"},
	}
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
	events, err := s.store.LoadSession(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	meta, err := s.store.GetSessionMeta(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	entries := make([]nativeHistoryEntry, 0, len(events))
	projection := newNativeProjectionCursor()
	projected := make([]nativeSessionEvent, 0, len(events))
	for _, ev := range events {
		projected = append(projected, projection.project(req.SessionID, ev))
	}
	start, end := nativeHistoryPageBounds(projected, req.BeforeSeq, limit)
	for _, event := range projected[start:end] {
		entries = append(entries, nativeHistoryEntry{Event: event})
	}
	header := nativeSessionHeader{
		Version:   0,
		ID:        meta.ID,
		CreatedAt: meta.CreatedAt.UnixMilli(),
		CWD:       meta.CWD,
	}
	header.ParentSessionID, header.Origin, header.DelegationDepth = nativeSessionLineage(events)
	permission := ""
	if configs, ok := s.store.(store.SessionConfigStore); ok {
		if config, configErr := configs.GetSessionConfig(r.Context(), req.SessionID); configErr == nil {
			header.AgentPreset = config.AgentPreset
			permission = config.Permission
		}
	}
	value := map[string]any{
		"header":  header,
		"events":  entries,
		"hasMore": start > 0,
		"surface": projection.surfaceSnapshot(),
	}
	if req.BeforeSeq == nil || *req.BeforeSeq == 0 {
		lastSeq := int64(-1)
		if len(events) > 0 {
			lastSeq = int64(events[len(events)-1].Seq)
		}
		baseline := projection.projectionBlock(meta.Title, lastSeq, permission)
		if limits := s.nativeImageLimitsProjection(); limits != nil {
			baseline.Values["imageLimits"] = limits
		}
		value["projections"] = baseline
	}
	return nativeRPCSuccess(value)
}

// nativeHistoryPageBounds mirrors DSH's message-boundary history contract.
// The projection must be seeded by the complete ordered log before this
// window is selected: a page may begin with an assistant chunk, a tool result,
// or a compaction replacement whose turn/surface owner lives on an earlier
// page. Only append-origin user/assistant messages consume the page budget;
// replacement copies remain in the contiguous raw range but do not count.
// Keeping the bounds calculation separate also makes the sequence cursor
// semantics explicit (beforeSeq is exclusive; zero selects the tail).
func nativeHistoryPageBounds(events []nativeSessionEvent, before *uint64, maxMessages int) (start, end int) {
	end = len(events)
	if before != nil && *before != 0 {
		end = sort.Search(len(events), func(index int) bool {
			return events[index].Seq >= *before
		})
	}
	if end < 0 {
		end = 0
	}
	if end > len(events) {
		end = len(events)
	}
	if maxMessages <= 0 {
		maxMessages = defaultEventPageLimit
	}
	cut := uint64(0)
	for index, messages := end-1, 0; index >= 0; index-- {
		event := events[index]
		if !nativeIsAppendSurfaceMessage(event) {
			continue
		}
		messages++
		groupStart := event.Seq
		for _, source := range event.SourceEventSeqs {
			if source < groupStart {
				groupStart = source
			}
		}
		if messages >= maxMessages {
			cut = groupStart
			break
		}
	}
	if cut == 0 {
		return 0, end
	}
	start = sort.Search(end, func(index int) bool {
		return events[index].Seq >= cut
	})
	return start, end
}

func nativeIsAppendSurfaceMessage(event nativeSessionEvent) bool {
	if event.Type != session.EventUserMessage && event.Type != session.EventAssistantMessage {
		return false
	}
	return event.SurfaceOp == "append"
}

func (s *Server) nativeSessionCreate(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.sessFn == nil {
		return nativeRPCFailure("not-supported", "session manager not wired", nil)
	}
	var req nativeSessionCreateRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.AgentPreset = strings.TrimSpace(req.AgentPreset)
	if req.AgentPreset != "" && !nativeAgentPresetKnown(req.AgentPreset) {
		return nativeRPCFailure("agent-preset-invalid", "agent preset is not available", map[string]any{"agentPreset": req.AgentPreset})
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
	if req.AgentPreset != "" {
		configs, ok := s.store.(store.SessionConfigStore)
		if !ok {
			return nativeRPCFailure("session-create-failed", "session configuration store is not wired", nil)
		}
		if err := configs.SetSessionConfig(r.Context(), id, store.SessionConfig{AgentPreset: req.AgentPreset}); err != nil {
			return nativeRPCFailure("session-create-failed", err.Error(), nil)
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
		case nativeProjectionFrame:
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
		projection := newNativeProjectionCursor()
		var projectionMu sync.Mutex
		lastSeq := int64(-1)
		if len(events) > 0 {
			lastSeq = int64(events[len(events)-1].Seq)
		}
		for _, ev := range events {
			projection.project(meta.ID, ev)
		}
		if err := write(nativeSubscribedFrame{Type: "session/subscribed", SessionID: meta.ID, LastSeq: lastSeq}); err != nil {
			return
		}
		unsubs = append(unsubs, s.evSrc(meta.ID, func(ev session.Event) {
			projectionMu.Lock()
			projected := projection.project(meta.ID, ev)
			changes := projection.projectionChanges()
			projectionMu.Unlock()
			_ = write(nativeSessionEventFrame{Type: "session/event", SessionID: meta.ID, Event: projected})
			for key, value := range changes {
				_ = write(nativeProjectionFrame{Type: "session/projection", SessionID: meta.ID, Key: key, Value: value, Seq: ev.Seq})
			}
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
