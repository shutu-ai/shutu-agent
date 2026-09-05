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
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/interact"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/pathsecure"
	"github.com/shutu-ai/shutu-agent/internal/profile"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/sessionreference"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

var canonicalIANAPromptTimeZone = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)
var nativeLocalePattern = regexp.MustCompile(`^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{1,8})*$`)

const (
	nativeSessionSearchMaxQueryUTF16     = 500
	nativeSessionSearchResultLimit       = 20
	nativeSessionSearchSnippetMaxRunes   = 240
	nativeSessionSearchProviderCallLimit = 100
)

func loadCanonicalPromptTimeZone(value string) (*time.Location, error) {
	if value == "UTC" {
		return time.UTC, nil
	}
	if !canonicalIANAPromptTimeZone.MatchString(value) {
		return nil, fmt.Errorf("browser time zone must be canonical UTC or IANA Area/Location: %q", value)
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return nil, fmt.Errorf("browser time zone is unsupported: %q", value)
	}
	return location, nil
}

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
	nativeSettingsAgentPresets = "agent-presets"
	nativeSettingsTheme        = "ui-theme"
	nativeSettingsLocale       = "locale"
	nativeSettingsConversation = "ui-conversation"
	nativeSettingsPermission   = "permission"
	nativeSettingsShell        = "shell"
	nativeSettingsAgentLoop    = "agent-loop"
	nativeSettingsDefaultModel = "agent-default-model"
	nativeSettingsWebSearch    = "web-search-deepseek"
	nativeWelcomeNoticeVersion = "2026-08-13.1"
	nativeDirectoryMaxEntries  = 1000
	nativeSessionListTailLimit = 256
)

var nativeSettingsSchema = map[string]any{
	"uid": 0,
	"refs": map[string]any{
		"0": map[string]any{"uid": 0, "type": "any", "meta": map[string]any{}},
	},
}

type nativeSettingsDocument struct {
	Value    map[string]any `json:"value"`
	Revision int            `json:"revision"`
}

type nativeRemoteEventFrame struct {
	Type  string `json:"type"`
	Event string `json:"event"`
	Args  []any  `json:"args"`
}

func nativeSettingsKey(namespace string) string { return "native.settings." + namespace }

func nativeSettingsSchemaFor(namespace string) map[string]any {
	switch namespace {
	case nativeSettingsTheme:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{"preference": "system", "fontSize": 14}}, "dict": map[string]any{"preference": 1, "fontSize": 5}},
			"1": map[string]any{"uid": 1, "type": "union", "meta": map[string]any{"default": "system"}, "list": []any{2, 3, 4}},
			"2": map[string]any{"uid": 2, "type": "const", "meta": map[string]any{}, "value": "light"},
			"3": map[string]any{"uid": 3, "type": "const", "meta": map[string]any{}, "value": "dark"},
			"4": map[string]any{"uid": 4, "type": "const", "meta": map[string]any{}, "value": "system"},
			"5": map[string]any{"uid": 5, "type": "number", "meta": map[string]any{"default": 14, "min": 12, "max": 17, "step": 1}},
		}}
	case nativeSettingsLocale:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{}}, "dict": map[string]any{"preference": 1}},
			"1": map[string]any{"uid": 1, "type": "string", "meta": map[string]any{"pattern": map[string]any{"source": "^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{1,8})*$", "flags": "u"}}},
		}}
	case nativeSettingsConversation:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{"busyEnter": "queue"}}, "dict": map[string]any{"busyEnter": 1}},
			"1": map[string]any{"uid": 1, "type": "union", "meta": map[string]any{"default": "queue"}, "list": []any{2, 3}},
			"2": map[string]any{"uid": 2, "type": "const", "meta": map[string]any{}, "value": "queue"},
			"3": map[string]any{"uid": 3, "type": "const", "meta": map[string]any{}, "value": "steer"},
		}}
	case nativeSettingsPermission:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{"defaultPreset": "standard"}}, "dict": map[string]any{"defaultPreset": 1}},
			"1": map[string]any{"uid": 1, "type": "union", "meta": map[string]any{"required": true, "default": "standard"}, "list": []any{2, 3, 4}},
			"2": map[string]any{"uid": 2, "type": "const", "meta": map[string]any{}, "value": "readonly"},
			"3": map[string]any{"uid": 3, "type": "const", "meta": map[string]any{}, "value": "standard"},
			"4": map[string]any{"uid": 4, "type": "const", "meta": map[string]any{}, "value": "full"},
		}}
	case nativeSettingsShell:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{
				"timeoutMs": 120000, "maxTimeoutMs": 600000, "maxOutputBytes": 64000,
				"maxSpillBytes": 67108864, "graceMs": 3000,
			}}, "dict": map[string]any{
				"cwd": 1, "timeoutMs": 2, "maxTimeoutMs": 3, "maxOutputBytes": 4,
				"maxSpillBytes": 5, "graceMs": 6, "pwshPath": 1,
			}},
			"1": map[string]any{"uid": 1, "type": "string", "meta": map[string]any{}},
			"2": map[string]any{"uid": 2, "type": "number", "meta": map[string]any{"default": 120000, "min": 1}},
			"3": map[string]any{"uid": 3, "type": "number", "meta": map[string]any{"default": 600000, "min": 1}},
			"4": map[string]any{"uid": 4, "type": "number", "meta": map[string]any{"default": 64000, "min": 1}},
			"5": map[string]any{"uid": 5, "type": "number", "meta": map[string]any{"default": 67108864, "min": 1}},
			"6": map[string]any{"uid": 6, "type": "number", "meta": map[string]any{"default": 3000, "min": 1, "max": 2147483647}},
		}}
	case nativeSettingsAgentLoop:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{"maxParallelToolCalls": 10}}, "dict": map[string]any{"maxParallelToolCalls": 1}},
			"1": map[string]any{"uid": 1, "type": "number", "meta": map[string]any{"default": 10, "min": 1, "step": 1}},
		}}
	case nativeSettingsDefaultModel:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"required": true}, "dict": map[string]any{"provider": 1, "model": 1, "reasoningEffort": 1}},
			"1": map[string]any{"uid": 1, "type": "string", "meta": map[string]any{"required": true}},
			"2": map[string]any{"uid": 2, "type": "string", "meta": map[string]any{}},
		}}
	case nativeSettingsWebSearch:
		return map[string]any{"uid": 0, "refs": map[string]any{
			"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{
				"apiKeyEnv": "DEEPSEEK_API_KEY", "model": "deepseek-v4-flash",
				"apiVersion": "2023-06-01", "maxTokens": 4096, "maxUses": 5,
			}}, "dict": map[string]any{
				"apiKey": 1, "apiKeyEnv": 1, "baseURL": 1, "model": 1,
				"apiVersion": 1, "maxTokens": 2, "maxUses": 3,
			}},
			"1": map[string]any{"uid": 1, "type": "string", "meta": map[string]any{}},
			"2": map[string]any{"uid": 2, "type": "number", "meta": map[string]any{"default": 4096, "min": 1, "step": 1}},
			"3": map[string]any{"uid": 3, "type": "number", "meta": map[string]any{"default": 5, "min": 1, "step": 1}},
		}}
	}
	if namespace == nativeSettingsAgentPresets {
		return map[string]any{
			"uid": 0,
			"refs": map[string]any{
				"0": map[string]any{"uid": 0, "type": "object", "meta": map[string]any{"default": map[string]any{"default": "standard"}}, "dict": map[string]any{"default": 1}},
				"1": map[string]any{"uid": 1, "type": "string", "meta": map[string]any{}},
			},
		}
	}
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
				"id": 5, "name": 7, "contextWindow": 15, "maxTokens": 16, "defaultMaxTokens": 17,
			}},
			"15": map[string]any{"uid": 15, "type": "number", "meta": map[string]any{}},
			"16": map[string]any{"uid": 16, "type": "number", "meta": map[string]any{}},
			"17": map[string]any{"uid": 17, "type": "number", "meta": map[string]any{}},
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
	Details map[string]any `json:"details"`
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

type nativeMessageFeedbackListRequest struct {
	SessionID string `json:"sessionId"`
}

type nativeMessageFeedbackPutRequest struct {
	SessionID string  `json:"sessionId"`
	MessageID string  `json:"messageId"`
	Rating    string  `json:"rating"`
	Note      *string `json:"note"`
	IfVersion *string `json:"ifVersion"`
}

type nativeMessageFeedbackDeleteRequest struct {
	SessionID string `json:"sessionId"`
	MessageID string `json:"messageId"`
	IfVersion string `json:"ifVersion"`
}

type nativeCommandRequest struct {
	SessionID string
	Line      string
	Images    []llm.ImageRef
}

// nativeCommandRequestFromPayload accepts the generated DSH connection shape
// ({args:{agentId,...}}) as well as the compact array form used by older DSH
// browser bundles ({args:[sessionId,line,images]}). Keeping this normalization
// at the wire boundary lets the command manager remain transport-neutral.
func nativeCommandRequestFromPayload(raw json.RawMessage) (nativeCommandRequest, error) {
	var direct struct {
		SessionID string          `json:"sessionId"`
		AgentID   string          `json:"agentId"`
		Line      string          `json:"line"`
		Images    []llm.ImageRef  `json:"images"`
		Args      json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &direct); err != nil {
		return nativeCommandRequest{}, fmt.Errorf("payload is invalid JSON: %w", err)
	}
	sessionID := strings.TrimSpace(direct.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(direct.AgentID)
	}
	if len(direct.Args) > 0 && string(direct.Args) != "null" {
		if direct.Args[0] == '{' {
			var args struct {
				SessionID string         `json:"sessionId"`
				AgentID   string         `json:"agentId"`
				Line      string         `json:"line"`
				Images    []llm.ImageRef `json:"images"`
			}
			if err := json.Unmarshal(direct.Args, &args); err != nil {
				return nativeCommandRequest{}, fmt.Errorf("command args are invalid: %w", err)
			}
			if sessionID == "" {
				sessionID = strings.TrimSpace(args.SessionID)
				if sessionID == "" {
					sessionID = strings.TrimSpace(args.AgentID)
				}
			}
			if direct.Line == "" {
				direct.Line = args.Line
			}
			if len(direct.Images) == 0 {
				direct.Images = args.Images
			}
		} else if direct.Args[0] == '[' {
			var args []json.RawMessage
			if err := json.Unmarshal(direct.Args, &args); err != nil {
				return nativeCommandRequest{}, fmt.Errorf("command args are invalid: %w", err)
			}
			if len(args) > 0 && sessionID == "" {
				_ = json.Unmarshal(args[0], &sessionID)
			}
			if len(args) > 1 && direct.Line == "" {
				_ = json.Unmarshal(args[1], &direct.Line)
			}
			if len(args) > 2 && len(direct.Images) == 0 {
				_ = json.Unmarshal(args[2], &direct.Images)
			}
		}
	}
	if sessionID == "" {
		return nativeCommandRequest{}, errors.New("agentId or sessionId is required")
	}
	return nativeCommandRequest{SessionID: sessionID, Line: direct.Line, Images: direct.Images}, nil
}

type nativeSessionSelectModelRequest struct {
	SessionID       string `json:"sessionId"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

type nativeSessionForkRequest struct {
	SessionID string  `json:"sessionId"`
	AtSeq     *uint64 `json:"atSeq"`
}

type nativeSessionUpdateQueueRequest struct {
	SessionID string `json:"sessionId"`
	ItemID    string `json:"itemId"`
	Action    struct {
		Kind    string             `json:"kind"`
		Content []nativePromptPart `json:"content"`
	} `json:"action"`
}

type nativeSubagentHistoryRequest struct {
	ParentSessionID string  `json:"parentSessionId"`
	ChildSessionID  string  `json:"childSessionId"`
	Mode            string  `json:"mode"`
	BeforeSeq       *uint64 `json:"beforeSeq"`
	MaxMessages     int     `json:"maxMessages"`
}

type nativeSubagentPromptRequest struct {
	ParentSessionID string             `json:"parentSessionId"`
	ChildSessionID  string             `json:"childSessionId"`
	Mode            string             `json:"mode"`
	Content         []nativePromptPart `json:"content"`
	ClientTimeZone  string             `json:"clientTimeZone"`
}

type nativeSubagentInterruptRequest struct {
	ParentSessionID string `json:"parentSessionId"`
	ChildSessionID  string `json:"childSessionId"`
	Mode            string `json:"mode"`
}

type nativeGoalRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

type nativeGoalMutationRequest struct {
	SessionID     string         `json:"sessionId"`
	Ref           *nativeGoalRef `json:"ref"`
	Objective     *string        `json:"objective"`
	MaxGoalRounds *int           `json:"maxGoalRounds"`
}

type nativeSessionSearchRequest struct {
	Query string `json:"query"`
}

// truncateUnicodeCodePoints mirrors DSH's code-point-safe response bound. The
// wire schema re-checks this bound, so truncation never appends an ellipsis.
func truncateUnicodeCodePoints(value string, maximum int) string {
	if maximum < 0 {
		return ""
	}
	count := 0
	for index := range value {
		if count == maximum {
			return value[:index]
		}
		count++
	}
	return value
}

// nativeSessionSearch enforces the fixed DSH sidebar-search bounds. Search is
// restricted to visible sessions and to messages on their current surface, so
// hidden compaction sources and tool telemetry do not enter the Web sidebar.
func (s *Server) nativeSessionSearch(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionSearchRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nativeRPCFailure("bad-request", "query is required", nil)
	}
	if strings.ContainsRune(query, 0) {
		return nativeRPCFailure("bad-request", "query must not contain NUL", nil)
	}
	if len(utf16.Encode([]rune(query))) > nativeSessionSearchMaxQueryUTF16 {
		return nativeRPCFailure("bad-request", fmt.Sprintf("query must contain at most %d characters", nativeSessionSearchMaxQueryUTF16), nil)
	}

	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	visible := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		if meta.ArchivedAt.IsZero() {
			visible[meta.ID] = struct{}{}
		}
	}
	items := make([]map[string]any, 0, nativeSessionSearchResultLimit)
	accepted := make(map[string]struct{})
	addHit := func(hit store.SearchHit) error {
		if _, ok := visible[hit.SessionID]; !ok {
			return nil
		}
		if _, ok := accepted[hit.SessionID]; ok {
			return nil
		}
		snippet, ok, err := s.currentSessionMessageMatch(r.Context(), hit.SessionID, query)
		if err != nil || !ok {
			return err
		}
		accepted[hit.SessionID] = struct{}{}
		items = append(items, map[string]any{
			"sessionId": hit.SessionID,
			"snippet":   truncateUnicodeCodePoints(snippet, nativeSessionSearchSnippetMaxRunes),
		})
		return nil
	}

	hasMore := false
	pager, canPage := s.store.(store.SessionSearchPager)
	if !canPage {
		hits, err := s.store.SearchSessions(r.Context(), query)
		if err != nil {
			return nativeRPCFailure("internal", err.Error(), nil)
		}
		for _, hit := range hits {
			if len(items) > nativeSessionSearchResultLimit {
				hasMore = true
				break
			}
			if err := addHit(hit); err != nil {
				return nativeRPCFailure("internal", err.Error(), nil)
			}
		}
	} else {
		offset, providerCalls := 0, 0
		for len(items) <= nativeSessionSearchResultLimit {
			if providerCalls >= nativeSessionSearchProviderCallLimit {
				return nativeRPCFailure("internal", fmt.Sprintf("session search provider exceeded the %d-call work budget", nativeSessionSearchProviderCallLimit), nil)
			}
			providerCalls++
			page, more, err := pager.SearchSessionsPage(r.Context(), query, offset, nativeSessionSearchResultLimit)
			if err != nil {
				return nativeRPCFailure("internal", err.Error(), nil)
			}
			for _, hit := range page {
				if len(items) > nativeSessionSearchResultLimit {
					break
				}
				if err := addHit(hit); err != nil {
					return nativeRPCFailure("internal", err.Error(), nil)
				}
			}
			offset += len(page)
			if len(items) > nativeSessionSearchResultLimit || !more {
				break
			}
		}
	}
	if len(items) > nativeSessionSearchResultLimit {
		hasMore = true
		items = items[:nativeSessionSearchResultLimit]
	}
	return nativeRPCSuccess(map[string]any{"items": items, "hasMore": hasMore})
}

// currentSessionMessageMatch rebuilds the authoritative projection and matches
// only current user/assistant text. It intentionally filters the raw store hit
// again because the SQLite prefilter spans every durable event.
func (s *Server) currentSessionMessageMatch(ctx context.Context, sessionID, query string) (string, bool, error) {
	events, err := s.store.LoadSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	surfaces, err := projection.ClassifyEventSurfaces(events)
	if err != nil {
		return "", false, err
	}
	needle := strings.ToLower(query)
	for _, event := range events {
		if event.Type != session.EventUserMessage && event.Type != session.EventAssistantMessage {
			continue
		}
		if surfaces[event.Seq] != projection.SurfaceCurrent {
			continue
		}
		message, ok := session.DeriveEventMessage(event)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(message.Text()), needle) {
			return message.Text(), true, nil
		}
	}
	return "", false, nil
}

type nativeWorkspaceView struct {
	WorkspaceID string   `json:"workspaceId"`
	Path        string   `json:"path"`
	Title       string   `json:"title"`
	SessionIDs  []string `json:"sessionIds"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

type nativeWorkspaceListValue struct {
	Items              []nativeWorkspaceView `json:"items"`
	ArchivedSessionIDs []string              `json:"archivedSessionIds"`
}

type nativeEventEnvelope struct {
	Type    string `json:"type"`
	RPCID   string `json:"rpcId"`
	Method  string `json:"method"`
	Payload any    `json:"payload"`
}

type nativeClientResponse struct {
	Type   string          `json:"type"`
	RPCID  string          `json:"rpcId"`
	Result nativeRPCResult `json:"result"`
}

type nativeRPCReceipt struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
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
		"commands/list", "commands/execute",
		"messageFeedback/list", "messageFeedback/put", "messageFeedback/delete",
		"fileReferences/list",
		"sessionReferenceResolver/candidates",
		"pluginInventory/list",
		"session.list", "session.search", "session.create",
		"session.history", "session.rename", "session.prompt", "session.cancel", "session.attachment",
		"session.models", "session.selectModel", "session.fork", "session.updateQueue",
		"workspace.list", "workspace.create", "workspace.rename", "workspace.delete",
		"workspace.insertBefore", "workspace.insertSessionBefore", "workspace.archiveSession",
		"agentPreset.list", "agentPreset.select", "agentPreset.read", "agentPreset.copy", "agentPreset.openDocument", "agentPreset.remove", "settings.describe",
		"settings.openDocument", "settings.mutate", "settings.update", "settings.replace", "credentials.describe", "credentials.set", "credentials.unset", "runtime.capabilities", "runtime.capability", "dynamicCordisRunner/syncInspectManifest",
		"host.openPath",
		"dynamicCordisRunner/inventory", "runtime.profiles", "runtime.profile", "runtime.crash-contracts", "runtime.crash-contract", "llm.providers", "llm.models", "llm.discoverModels", "skill.list",
		"subagent.list", "subagent.history", "subagent.prompt", "subagent.interrupt",
		"goal.create", "goal.edit", "goal.pause", "goal.resume", "goal.complete", "goal.clear",
		"goals/create", "goals/edit", "goals/pause", "goals/resume", "goals/complete", "goals/clear",
	} {
		mux.Handle("POST /api/"+method, s.requireAuth(http.HandlerFunc(s.handleNativeRPC)))
	}
	mux.Handle("GET "+nativeMuxPath, s.requireAuth(http.HandlerFunc(s.handleNativeMuxWebSocket)))
	mux.Handle("GET "+nativeHostPath, s.requireAuth(http.HandlerFunc(s.handleNativeHostWebSocket)))
	mux.Handle("POST /api/respond", s.requireAuth(http.HandlerFunc(s.handleNativeRespond)))
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

// handleNativeRespond is the DSH client-response carrier. Approval and
// question answers are correlated by the server-request rpcId; unlike unary
// RPCs this endpoint returns a small carrier receipt and the final outcome is
// broadcast on the mux stream.
func (s *Server) handleNativeRespond(w http.ResponseWriter, r *http.Request) {
	var response nativeClientResponse
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&response); err != nil ||
		response.Type != "client-response" || strings.TrimSpace(response.RPCID) == "" {
		_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "bad-response"})
		return
	}
	if s.resolveInteractionFn == nil {
		_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "not-pending"})
		return
	}
	interactionID := strings.TrimSpace(response.RPCID)
	if !response.Result.OK {
		// The DSH QuestionComposer dismiss action uses a cancelled error
		// envelope without a value/sessionId. Recover the session from the
		// native mux correlation established when the request was published.
		if response.Result.Error == nil || response.Result.Error.Code != "cancelled" {
			_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "not-pending"})
			return
		}
		sessionID := s.nativeInteractionSession(interactionID)
		if sessionID == "" && s.resolveInteractionSessionFn != nil {
			resolvedSession, resolveErr := s.resolveInteractionSessionFn(r.Context(), interactionID)
			if resolveErr == nil {
				sessionID = strings.TrimSpace(resolvedSession)
			}
		}
		if sessionID == "" {
			_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "not-pending"})
			return
		}
		if err := s.resolveInteractionFn(r.Context(), sessionID, interactionID, interact.StatusCanceled, ""); err != nil {
			reason := "not-pending"
			if !errors.Is(err, interact.ErrUnknownRequest) && !errors.Is(err, interact.ErrAlreadyResolved) {
				reason = "resolve-failed"
			}
			_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: reason})
			return
		}
		s.forgetNativeInteraction(interactionID)
		_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: true})
		return
	}
	value, ok := response.Result.Value.(map[string]any)
	if !ok {
		_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "bad-response"})
		return
	}
	sessionID := nativeString(value["sessionId"])
	if sessionID == "" {
		_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "bad-response"})
		return
	}
	status := interact.StatusApproved
	answer := ""
	if approvalID := nativeString(value["approvalId"]); approvalID != "" {
		if approvalID != interactionID {
			_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "bad-response"})
			return
		}
		switch nativeString(value["outcome"]) {
		case "allowed-once":
			status = interact.StatusApproved
		case "rejected":
			status = interact.StatusRejected
		default:
			_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "bad-response"})
			return
		}
	} else {
		// Question requests use the server-request rpcId as their logical
		// request id and carry the structured answer in result.value.answer.
		encoded, err := json.Marshal(value["answer"])
		if err != nil || string(encoded) == "null" {
			_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: "bad-response"})
			return
		}
		answer = string(encoded)
	}
	if err := s.resolveInteractionFn(r.Context(), sessionID, interactionID, status, answer); err != nil {
		reason := "not-pending"
		if !errors.Is(err, interact.ErrUnknownRequest) && !errors.Is(err, interact.ErrAlreadyResolved) {
			reason = "resolve-failed"
		}
		_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: false, Reason: reason})
		return
	}
	s.forgetNativeInteraction(interactionID)
	_ = json.NewEncoder(w).Encode(nativeRPCReceipt{Accepted: true})
}

func (s *Server) rememberNativeInteraction(id, sessionID string) {
	id, sessionID = strings.TrimSpace(id), strings.TrimSpace(sessionID)
	if id == "" || sessionID == "" {
		return
	}
	s.nativeInteractionMu.Lock()
	if s.nativeInteractionSessions == nil {
		s.nativeInteractionSessions = make(map[string]string)
	}
	s.nativeInteractionSessions[id] = sessionID
	s.nativeInteractionMu.Unlock()
}

func (s *Server) nativeInteractionSession(id string) string {
	s.nativeInteractionMu.RLock()
	sessionID := s.nativeInteractionSessions[strings.TrimSpace(id)]
	s.nativeInteractionMu.RUnlock()
	return sessionID
}

func (s *Server) forgetNativeInteraction(id string) {
	s.nativeInteractionMu.Lock()
	delete(s.nativeInteractionSessions, strings.TrimSpace(id))
	s.nativeInteractionMu.Unlock()
}

func nativeRPCSuccess(value any) nativeRPCResult { return nativeRPCResult{OK: true, Value: value} }

func nativeRPCFailure(code, message string, details map[string]any) nativeRPCResult {
	if details == nil {
		details = map[string]any{}
	}
	return nativeRPCResult{OK: false, Error: &nativeRPCError{Code: code, Message: message, Details: details}}
}

// nativeAttachmentFailure maps durable image admission failures to the stable
// attachment protocol shape. The reason lets the client distinguish model
// capability from caller-correctable image and batch policy failures.
func nativeAttachmentFailure(err error) nativeRPCResult {
	reason := "ATTACHMENT_WRITE_FAILED"
	switch {
	case errors.Is(err, attachment.ErrTooManyImages):
		reason = "TOO_MANY_IMAGES"
	case errors.Is(err, attachment.ErrBatchTooLarge):
		reason = "IMAGES_TOO_LARGE"
	case errors.Is(err, attachment.ErrUnsupportedType):
		reason = "UNSUPPORTED_IMAGE_TYPE"
	case errors.Is(err, attachment.ErrTypeMismatch):
		reason = "IMAGE_TYPE_MISMATCH"
	case errors.Is(err, attachment.ErrTooLarge):
		reason = "IMAGE_TOO_LARGE"
	case errors.Is(err, attachment.ErrEmptyData), errors.Is(err, attachment.ErrInvalidImage),
		errors.Is(err, attachment.ErrDimensionTooLarge), errors.Is(err, attachment.ErrTooManyPixels):
		reason = "INVALID_IMAGE"
	}
	return nativeRPCFailure("attachment-error", err.Error(), map[string]any{"reason": reason})
}

func nativeImageCapabilityFailure() nativeRPCResult {
	return nativeRPCFailure("attachment-error", "image prompt capability is unavailable for this session", map[string]any{
		"reason": "MODEL_DOES_NOT_SUPPORT_IMAGES",
	})
}

func nativeSubagentOwnershipFailure(sessionID string) nativeRPCResult {
	return nativeRPCFailure("agent-busy", fmt.Sprintf("session %q is owned by subagent routing", sessionID), map[string]any{
		"reason": "use subagent delivery for this child session",
	})
}

func nativePromptTimeZoneFailure(value string) nativeRPCResult {
	return nativeRPCFailure("invalid-time-zone",
		"clientTimeZone must be UTC or a valid IANA Area/Location name",
		map[string]any{"value": value})
}

// nativeSubagentOwnershipGate mirrors DSH's generic session resolver: session
// control-plane mutations resolve only ordinary sessions, while identities
// reserved for subagent delivery remain callable through the subagent API.
func (s *Server) nativeSubagentOwnershipGate(ctx context.Context, sessionID string) (nativeRPCResult, bool) {
	headers, ok := s.store.(store.SessionLineageStore)
	if !ok {
		return nativeRPCResult{}, true
	}
	header, err := headers.GetSessionHeader(ctx, sessionID)
	if err != nil || header.Origin != "subagent" {
		return nativeRPCResult{}, true
	}
	return nativeSubagentOwnershipFailure(sessionID), false
}

// nativeDecodeImageBase64 applies DSH's canonical browser-image Base64 rule.
// Decoding permissive variants would accept input the DSH client cannot round
// trip and could turn the same upload into different durable bytes.
func nativeDecodeImageBase64(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("image data is empty")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("image upload is not canonical base64: %v", err)
	}
	if base64.StdEncoding.EncodeToString(data) != encoded {
		return nil, errors.New("image upload is not canonical base64")
	}
	return data, nil
}

func nativeImageBase64Failure(err error) nativeRPCResult {
	return nativeRPCFailure("attachment-error", err.Error(), map[string]any{
		"reason": "INVALID_IMAGE_BASE64",
	})
}

func (s *Server) profileUnavailable(id string) nativeRPCResult {
	descriptor, err := s.profileRegistry.Get(id)
	if err != nil {
		return nativeRPCFailure("profile-unknown", err.Error(), map[string]any{"profile": id})
	}
	return nativeRPCFailure("profile-unsupported", "runtime profile is not available", map[string]any{
		"profile": descriptor,
	})
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

// nativeDecodeRemoteRequest unwraps the generated Typert Remote shape:
// {args: [request]}. Keeping this at the wire boundary allows the native
// handlers to expose the same request objects as DSH without coupling them to
// the browser's generated client details.
func nativeDecodeRemoteRequest(raw json.RawMessage, value any) error {
	var envelope struct {
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if len(envelope.Args) == 0 || string(envelope.Args) == "null" {
		return json.Unmarshal(raw, value)
	}
	if envelope.Args[0] == '[' {
		var args []json.RawMessage
		if err := json.Unmarshal(envelope.Args, &args); err != nil {
			return err
		}
		if len(args) == 0 {
			return errors.New("remote request args are empty")
		}
		return json.Unmarshal(args[0], value)
	}
	return json.Unmarshal(envelope.Args, value)
}

func nativeRemoteArguments(raw json.RawMessage) ([]json.RawMessage, error) {
	var envelope struct {
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Args) == 0 || string(envelope.Args) == "null" {
		return nil, errors.New("remote request args are missing")
	}
	if envelope.Args[0] == '[' {
		var args []json.RawMessage
		if err := json.Unmarshal(envelope.Args, &args); err != nil {
			return nil, err
		}
		return args, nil
	}
	return []json.RawMessage{envelope.Args}, nil
}

func nativeRemoteSessionID(raw json.RawMessage) (sessionID, cwd string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &sessionID); err != nil {
			return "", "", err
		}
		return strings.TrimSpace(sessionID), "", nil
	}
	var agent struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(trimmed, &agent); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(agent.ID) != "" {
		sessionID = agent.ID
	} else {
		sessionID = agent.SessionID
	}
	return strings.TrimSpace(sessionID), strings.TrimSpace(agent.CWD), nil
}

func (s *Server) nativeFileReferencesList(r *http.Request, raw json.RawMessage) nativeRPCResult {
	args, err := nativeRemoteArguments(raw)
	if err != nil || len(args) < 2 {
		return nativeRPCFailure("bad-request", "file reference request requires agent and query", nil)
	}
	sessionID, agentCWD, err := nativeRemoteSessionID(args[0])
	if err != nil {
		return nativeRPCFailure("bad-request", "file reference agent is invalid", nil)
	}
	var query string
	if err := json.Unmarshal(args[1], &query); err != nil {
		return nativeRPCFailure("bad-request", "file reference query is invalid", nil)
	}
	root := agentCWD
	if sessionID != "" {
		if meta, metaErr := s.store.GetSessionMeta(r.Context(), sessionID); metaErr == nil {
			root = meta.CWD
		}
	}
	if root == "" {
		root, err = s.sessionDefaultWorkdir()
		if err != nil {
			return nativeRPCSuccess([]any{})
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nativeRPCSuccess([]any{})
	}
	query = strings.ReplaceAll(strings.TrimSpace(query), `\`, "/")
	query = strings.TrimPrefix(query, "@")
	slash := strings.LastIndexByte(query, '/')
	displayDirectory, fragment := "", query
	if slash >= 0 {
		displayDirectory, fragment = query[:slash+1], query[slash+1:]
	}
	if !strings.HasPrefix(fragment, ".") && strings.Contains(displayDirectory, "/.") {
		return nativeRPCSuccess([]any{})
	}
	directory := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(displayDirectory, "/")))
	rel, err := filepath.Rel(root, directory)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nativeRPCSuccess([]any{})
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nativeRPCSuccess([]any{})
	}
	fragmentLower := strings.ToLower(fragment)
	values := make([]map[string]any, 0, 20)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(fragment, ".") {
			continue
		}
		if name == ".git" || name == "node_modules" {
			continue
		}
		if fragmentLower != "" && !strings.Contains(strings.ToLower(name), fragmentLower) {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if !entry.Type().IsRegular() {
			continue
		}
		values = append(values, map[string]any{"path": displayDirectory + name, "kind": kind})
		if len(values) >= 20 {
			break
		}
	}
	sort.SliceStable(values, func(i, j int) bool {
		left, _ := values[i]["path"].(string)
		right, _ := values[j]["path"].(string)
		return strings.ToLower(left) < strings.ToLower(right)
	})
	return nativeRPCSuccess(values)
}

func nativeSessionReferenceMention(sessionID, label string) string {
	encoded, _ := json.Marshal(sessionID)
	uri := base64.RawURLEncoding.EncodeToString(encoded)
	label = strings.NewReplacer("\\", "\\\\", "]", "\\]").Replace(label)
	return fmt.Sprintf("@[%s](shutu-session:%s)", label, uri)
}

func (s *Server) nativeSessionReferenceCandidates(r *http.Request, raw json.RawMessage) nativeRPCResult {
	args, err := nativeRemoteArguments(raw)
	if err != nil || len(args) < 2 {
		return nativeRPCFailure("bad-request", "session reference request requires agent and query", nil)
	}
	sessionID, _, err := nativeRemoteSessionID(args[0])
	if err != nil {
		return nativeRPCFailure("bad-request", "session reference agent is invalid", nil)
	}
	var query string
	if err := json.Unmarshal(args[1], &query); err != nil {
		return nativeRPCFailure("bad-request", "session reference query is invalid", nil)
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("session-reference-failed", err.Error(), nil)
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	type referenceCandidate struct {
		meta  store.SessionMeta
		index int
	}
	records := make([]referenceCandidate, 0, len(metas))
	for index, meta := range metas {
		if meta.ID == sessionID {
			continue
		}
		label := strings.TrimSpace(meta.Title)
		if label == "" {
			label = meta.ID
		}
		searchable := strings.ToLower(strings.Join([]string{meta.ID, label, meta.CWD}, "\n"))
		if needle != "" && !strings.Contains(searchable, needle) {
			continue
		}
		records = append(records, referenceCandidate{meta: meta, index: index})
	}
	// DSH ranks discovery by working-directory affinity, then by durable
	// catalog order. Keep that behavior when mentions select source sessions.
	targetCWD := ""
	if current, err := s.store.GetSessionMeta(r.Context(), sessionID); err == nil {
		targetCWD = current.CWD
	}
	sort.SliceStable(records, func(left, right int) bool {
		leftRank := sessionReferenceCandidateRank(records[left].meta.CWD, targetCWD)
		rightRank := sessionReferenceCandidateRank(records[right].meta.CWD, targetCWD)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return records[left].index < records[right].index
	})
	values := make([]map[string]any, 0, min(len(records), sessionreference.DefaultCandidateLimit))
	for _, record := range records {
		label := strings.TrimSpace(record.meta.Title)
		if label == "" {
			label = record.meta.ID
		}
		item := map[string]any{
			"sessionId": record.meta.ID,
			"label":     label,
			"createdAt": record.meta.CreatedAt.UnixMilli(),
			"mention":   nativeSessionReferenceMention(record.meta.ID, label),
		}
		if record.meta.CWD != "" {
			item["cwd"] = record.meta.CWD
		}
		values = append(values, item)
		if len(values) >= sessionreference.DefaultCandidateLimit {
			break
		}
	}
	return nativeRPCSuccess(values)
}

func sessionReferenceCandidateRank(candidateCWD, targetCWD string) int {
	if candidateCWD == "" {
		return 1
	}
	if candidateCWD == targetCWD {
		return 0
	}
	return 2
}

func nativePluginInventory() nativeRPCResult {
	packages := []string{
		"connection", "hmr", "locale", "runtime", "ui-agent-preset", "ui-attachment",
		"ui-brand-official", "ui-commands", "ui-conversation", "ui-deliverables",
		"ui-directory-picker-browse", "ui-goal", "ui-input-trigger", "ui-jobs",
		"ui-layout", "ui-message-feedback", "ui-model-selection", "ui-permission-presets",
		"ui-plan", "ui-reference", "ui-renderer", "ui-settings", "ui-settings-general",
		"ui-settings-models", "ui-settings-plugin-inventory", "ui-settings-plugins",
		"ui-sidebar", "ui-skill", "ui-subagent", "ui-theme", "ui-tool", "ui-trajectory",
		"ui-user-questions", "ui-workflow-run", "ui-workspace",
	}
	entries := make([]any, 0, len(packages)+4)
	for _, name := range packages {
		id := "@shutu-ai/client-" + name
		entries = append(entries, map[string]any{"entryId": id, "moduleName": id, "enabled": true, "fiberPhase": "active"})
	}
	for _, item := range []struct{ id, module string }{
		{"@shutu-ai/typert-registry", "@shutu-ai/typert-registry"},
		{"@shutu-ai/cordis-client-runner", "@shutu-ai/cordis-client-runner"},
		{"@shutu-ai/ui-cordis", "@shutu-ai/ui-cordis"},
		{"@shutu-ai/session-log-export", "@shutu-ai/session-log-export"},
	} {
		entries = append(entries, map[string]any{"entryId": item.id, "moduleName": item.module, "enabled": true, "fiberPhase": "active"})
	}
	return nativeRPCSuccess(map[string]any{"entries": entries})
}

func (s *Server) nativeSettingsDescribe() nativeRPCResult {
	s.nativeSettingsMu.Lock()
	defer s.nativeSettingsMu.Unlock()
	s.ensureNativeSettingsFromConfigLocked()
	namespaces := make([]any, 0, 12)
	for _, namespace := range nativeSettingsNamespaces() {
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

func (s *Server) nativeSettingsMutate(ctx context.Context, raw json.RawMessage) nativeRPCResult {
	var req nativeSettingsViewRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	return s.nativeSettingsApply(ctx, req.Namespace, req.Ops, req.Expected)
}

func (s *Server) nativeSettingsApply(ctx context.Context, namespace string, ops []nativeSettingsPathOp, expected *int) nativeRPCResult {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nativeRPCFailure("bad-request", "ns is required", nil)
	}
	if namespace == nativeSettingsAgentPresets {
		return s.nativeAgentPresetSettingsApply(ctx, ops, expected)
	}
	if !nativeSettingsNamespace(namespace) {
		return nativeRPCFailure("not-found", "native settings namespace is not registered", map[string]any{"ns": namespace})
	}

	s.nativeSettingsMu.Lock()
	defer s.nativeSettingsMu.Unlock()
	s.ensureNativeSettingsFromConfigLocked()
	document := s.nativeSettings[namespace]
	if document.Value == nil {
		document.Value = map[string]any{}
	}
	if expected != nil && *expected != document.Revision {
		return nativeRPCFailure("settings-conflict", "settings changed since it was read", map[string]any{
			"ns":       namespace,
			"expected": *expected,
			"actual":   document.Revision,
		})
	}
	for _, op := range ops {
		if err := applyNativeSettingsOp(&document.Value, op); err != nil {
			return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": namespace})
		}
	}
	if base := s.nativeSettingsBaseLocked(namespace); len(base) > 0 {
		resolved := cloneNativeSettingsMap(base)
		mergeNativeSettingsMap(resolved, document.Value)
		if err := validateNativeSettingsValue(namespace, resolved); err != nil {
			return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": namespace})
		}
	} else if err := validateNativeSettingsValue(namespace, document.Value); err != nil {
		return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": namespace})
	}
	if len(ops) > 0 {
		document.Revision++
		encoded, err := json.Marshal(document)
		if err != nil {
			return nativeRPCFailure("settings-rejected", "settings document is not serializable", map[string]any{"ns": namespace})
		}
		if err := s.store.SetSetting(ctx, nativeSettingsKey(namespace), string(encoded)); err != nil {
			return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": namespace})
		}
	}
	s.nativeSettings[namespace] = document
	view := s.nativeSettingsView(namespace, document)
	if apply := s.nativeSettingsAppliedFn; apply != nil {
		if err := apply(ctx, namespace, cloneNativeSettingsMap(view["value"].(map[string]any))); err != nil {
			return nativeRPCFailure("settings-apply-failed", err.Error(), map[string]any{"ns": namespace})
		}
	}
	s.notifyNativeSettingsDocumentUpdated(namespace, document.Revision)
	return nativeRPCSuccess(view)
}

func (s *Server) nativeSettingsView(namespace string, document nativeSettingsDocument) map[string]any {
	base := s.nativeSettingsBaseLocked(namespace)
	user := cloneNativeSettingsMap(document.Value)
	value := cloneNativeSettingsMap(base)
	mergeNativeSettingsMap(value, user)
	view := map[string]any{
		"ns":       namespace,
		"schema":   nativeSettingsSchemaFor(namespace),
		"value":    value,
		"user":     cloneNativeSettingsMap(document.Value),
		"applies":  "live",
		"secrets":  []any{},
		"revision": document.Revision,
	}
	if namespace == nativeSettingsAgentPresets {
		base := map[string]any{"default": s.nativeAgentPresetBaseDefault()}
		resolved := cloneNativeSettingsMap(document.Value)
		if _, ok := resolved["default"]; !ok {
			resolved["default"] = base["default"]
		}
		view["base"] = base
		view["value"] = resolved
		view["user"] = cloneNativeSettingsMap(document.Value)
	}
	if namespace == nativeSettingsTheme {
		if base == nil {
			base = map[string]any{"preference": "system", "fontSize": 14}
		}
	}
	if namespace == nativeSettingsConversation {
		if base == nil {
			base = map[string]any{"busyEnter": "queue"}
		}
	}
	if namespace == nativeSettingsPermission {
		if base == nil {
			base = map[string]any{"defaultPreset": "standard"}
		}
	}
	if namespace == nativeSettingsAgentLoop {
		if base == nil {
			base = map[string]any{"maxParallelToolCalls": 10}
		}
	}
	if namespace == nativeSettingsShell {
		if base == nil {
			base = map[string]any{
				"timeoutMs": 120000, "maxTimeoutMs": 600000,
				"maxOutputBytes": 64000, "maxSpillBytes": 67108864, "graceMs": 3000,
			}
		}
	}
	if namespace == nativeSettingsWebSearch {
		if base == nil {
			base = map[string]any{
				"apiKeyEnv": "DEEPSEEK_API_KEY", "model": "deepseek-v4-flash",
				"apiVersion": "2023-06-01", "maxTokens": 4096, "maxUses": 5,
			}
		}
		delete(base, "apiKey")
	}
	if namespace == nativeSettingsWebSearch {
		delete(value, "apiKey")
		delete(user, "apiKey")
		view["value"] = value
		view["user"] = user
	}
	if len(base) > 0 {
		view["base"] = cloneNativeSettingsMap(base)
		view["value"] = value
		view["user"] = user
	}
	return view
}

// nativeSettingsBaseLocked projects the composition/config-owned layer for a
// native namespace. The persisted native-settings row remains the user layer,
// so deployment defaults are removable/resettable rather than masquerading as
// user overrides (DSH settings base semantics).
func (s *Server) nativeSettingsBaseLocked(namespace string) map[string]any {
	switch namespace {
	case nativeSettingsTheme:
		return map[string]any{"preference": "system", "fontSize": 14}
	case nativeSettingsConversation:
		return map[string]any{"busyEnter": "queue"}
	case nativeSettingsPermission:
		return map[string]any{"defaultPreset": "standard"}
	case nativeSettingsAgentLoop:
		return map[string]any{"maxParallelToolCalls": 10}
	case nativeSettingsShell:
		return map[string]any{
			"timeoutMs": 120000, "maxTimeoutMs": 600000,
			"maxOutputBytes": 64000, "maxSpillBytes": 67108864, "graceMs": 3000,
		}
	case nativeSettingsWebSearch:
		return map[string]any{
			"apiKeyEnv": "DEEPSEEK_API_KEY", "model": "deepseek-v4-flash",
			"apiVersion": "2023-06-01", "maxTokens": 4096, "maxUses": 5,
		}
	case nativeSettingsDefaultModel:
		if s.cfgFn == nil {
			return nil
		}
		view := s.cfgFn()
		provider := nativeString(view["llm_provider"])
		if provider == "" {
			provider = nativeString(view["provider"])
		}
		model := nativeString(view["model"])
		effort := nativeString(view["reasoning_effort"])
		if provider == "" || model == "" {
			return nil
		}
		base := map[string]any{"provider": provider, "model": model}
		if effort != "" {
			base["reasoningEffort"] = effort
		}
		return base
	}
	if namespace != nativeSettingsDeepSeek && namespace != nativeSettingsPiAI {
		return nil
	}
	if s.cfgFn == nil {
		return nil
	}
	providers := nativeConfigProviderMaps(s.cfgFn()["providers"])
	if namespace == nativeSettingsDeepSeek {
		for _, provider := range providers {
			if nativeString(provider["id"]) != "deepseek-official" {
				continue
			}
			profile := map[string]any{}
			nativeCopyProviderProfile(profile, provider)
			return profile
		}
		return nil
	}
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
	return map[string]any{"providers": profiles}
}

func mergeNativeSettingsMap(base, user map[string]any) {
	for key, value := range user {
		userMap, userIsMap := value.(map[string]any)
		baseMap, baseIsMap := base[key].(map[string]any)
		if userIsMap && baseIsMap {
			mergeNativeSettingsMap(baseMap, userMap)
			continue
		}
		base[key] = cloneNativeSettingsValue(value)
	}
}

// nativeAgentPresetSettingsApply is the DSH settings-provider bridge for the
// agent-presets namespace. The browser writes only the deployment default;
// the durable setting remains the single source used when the process starts.
func (s *Server) nativeAgentPresetSettingsApply(ctx context.Context, ops []nativeSettingsPathOp, expected *int) nativeRPCResult {
	s.nativeSettingsMu.Lock()
	defer s.nativeSettingsMu.Unlock()
	s.ensureNativeSettingsFromConfigLocked()
	document := s.nativeSettings[nativeSettingsAgentPresets]
	if document.Value == nil {
		document.Value = map[string]any{}
	}
	if expected != nil && *expected != document.Revision {
		return nativeRPCFailure("settings-conflict", "settings changed since it was read", map[string]any{
			"ns": nativeSettingsAgentPresets, "expected": *expected, "actual": document.Revision,
		})
	}
	next := cloneNativeSettingsMap(document.Value)
	for _, op := range ops {
		if err := applyNativeSettingsOp(&next, op); err != nil {
			return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": nativeSettingsAgentPresets})
		}
	}
	rawDefault, hasUserDefault := next["default"]
	defaultPreset := ""
	if hasUserDefault {
		var ok bool
		defaultPreset, ok = rawDefault.(string)
		if !ok || strings.TrimSpace(defaultPreset) == "" {
			return nativeRPCFailure("settings-rejected", "agent-presets.default must be a non-empty preset id", map[string]any{"ns": nativeSettingsAgentPresets})
		}
		defaultPreset = strings.TrimSpace(defaultPreset)
		if !s.nativeAgentPresetAvailable(ctx, defaultPreset) {
			return nativeRPCFailure("agent-preset-invalid", "agent preset is not available", map[string]any{
				"agentPreset": defaultPreset, "reason": "preset is not available",
			})
		}
	}
	if !hasUserDefault {
		// Unsetting the user default deliberately falls through to the
		// deployment's base preset; this is DSH's remove/reset path.
		delete(next, "default")
	}
	if hasUserDefault {
		next["default"] = defaultPreset
	} else {
		delete(next, "default")
	}
	document.Value = next
	if document.Value == nil {
		document.Value = map[string]any{}
	}
	// Unsetting the user default restores the deployment base for the legacy
	// scalar used by session creation; setting an explicit default uses it.
	effectivePreset := defaultPreset
	if !hasUserDefault {
		effectivePreset = s.nativeAgentPresetBaseDefault()
	}
	var encoded []byte
	if len(ops) > 0 {
		document.Revision++
		var err error
		encoded, err = json.Marshal(document)
		if err != nil {
			return nativeRPCFailure("settings-rejected", "settings document is not serializable", map[string]any{"ns": nativeSettingsAgentPresets})
		}
	}
	// agent_preset and the namespaced document are two rows behind a single
	// logical mutation. Store the prior scalar so a failure of the second write
	// cannot leave startup reading a different preset than settings.describe.
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": nativeSettingsAgentPresets})
	}
	previousPreset := settings["agent_preset"]
	if err := s.store.SetSetting(ctx, "agent_preset", effectivePreset); err != nil {
		return nativeRPCFailure("settings-rejected", err.Error(), map[string]any{"ns": nativeSettingsAgentPresets})
	}
	if len(ops) > 0 {
		if err := s.store.SetSetting(ctx, nativeSettingsKey(nativeSettingsAgentPresets), string(encoded)); err != nil {
			data := map[string]any{"ns": nativeSettingsAgentPresets}
			if rollbackErr := s.store.SetSetting(ctx, "agent_preset", previousPreset); rollbackErr != nil {
				data["rollbackError"] = rollbackErr.Error()
			}
			return nativeRPCFailure("settings-rejected", err.Error(), data)
		}
	}
	if setter, ok := s.nativeAgentPresetManager.(interface{ SetDefault(string) }); ok {
		setter.SetDefault(effectivePreset)
	}
	s.nativeSettings[nativeSettingsAgentPresets] = document
	s.notifyNativeSettingsDocumentUpdated(nativeSettingsAgentPresets, document.Revision)
	return nativeRPCSuccess(s.nativeSettingsView(nativeSettingsAgentPresets, document))
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
	if s.nativeSettingsLoaded == nil {
		s.nativeSettingsLoaded = make(map[string]bool)
	}
	// Native settings are durable user documents, not a process-local cache.
	// Restore each known namespace before applying config-derived defaults. A
	// malformed optional row is ignored so it cannot take down the host's other
	// settings capabilities.
	if len(s.nativeSettingsLoaded) == 0 {
		if settings, err := s.store.GetSettings(context.Background()); err == nil {
			for _, namespace := range nativeSettingsNamespaces() {
				s.nativeSettingsLoaded[namespace] = true
				var document nativeSettingsDocument
				raw := strings.TrimSpace(settings[nativeSettingsKey(namespace)])
				if raw == "" || json.Unmarshal([]byte(raw), &document) != nil || document.Revision < 0 || document.Value == nil {
					continue
				}
				s.nativeSettings[namespace] = nativeSettingsDocument{Value: cloneNativeSettingsMap(document.Value), Revision: document.Revision}
			}
		}
	}
	s.nativeSettings[nativeSettingsOnboarding] = s.nativeSettings[nativeSettingsOnboarding]
	for _, namespace := range []string{
		nativeSettingsTheme, nativeSettingsLocale, nativeSettingsConversation,
		nativeSettingsPermission, nativeSettingsShell, nativeSettingsAgentLoop,
		nativeSettingsDefaultModel, nativeSettingsWebSearch,
	} {
		if _, exists := s.nativeSettings[namespace]; !exists {
			s.nativeSettings[namespace] = nativeSettingsDocument{Value: map[string]any{}}
		}
	}
	if s.cfgFn == nil {
		return
	}
	// Config-derived llm profiles belong to the base layer computed in
	// nativeSettingsBaseLocked. Persisted native-settings documents start as an
	// empty user layer; copying configuration into them would make deployment
	// defaults look like user overrides.
	if _, exists := s.nativeSettings[nativeSettingsDeepSeek]; !exists {
		s.nativeSettings[nativeSettingsDeepSeek] = nativeSettingsDocument{Value: map[string]any{}}
	}
	if _, exists := s.nativeSettings[nativeSettingsPiAI]; !exists {
		s.nativeSettings[nativeSettingsPiAI] = nativeSettingsDocument{Value: map[string]any{}}
	}
	if _, exists := s.nativeSettings[nativeSettingsAgentPresets]; !exists {
		s.nativeSettings[nativeSettingsAgentPresets] = nativeSettingsDocument{
			// The user layer starts empty. The deployment default is supplied
			// by the base layer in settings views, matching DSH's settings
			// composition semantics rather than copying base into user state.
			Value: map[string]any{},
		}
	}
}

func (s *Server) nativeAgentPresetBaseDefault() string {
	if s.cfgFn != nil {
		if mode := nativeString(s.cfgFn()["mode"]); nativeAgentPresetKnown(mode) {
			return mode
		}
	}
	return "standard"
}

func nativeSettingsNamespace(namespace string) bool {
	for _, known := range nativeSettingsNamespaces() {
		if namespace == known {
			return true
		}
	}
	return false
}

func nativeSettingsNamespaces() []string {
	return []string{
		nativeSettingsOnboarding, nativeSettingsDeepSeek, nativeSettingsPiAI,
		nativeSettingsAgentPresets, nativeSettingsTheme, nativeSettingsLocale,
		nativeSettingsConversation, nativeSettingsPermission, nativeSettingsShell,
		nativeSettingsAgentLoop, nativeSettingsDefaultModel, nativeSettingsWebSearch,
	}
}

func nativeSettingsStringList(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

func validateNativeSettingsObject(namespace string, value map[string]any, allowed []string) error {
	for key := range value {
		known := false
		for _, candidate := range allowed {
			if key == candidate {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("%s.%s is not a supported setting", namespace, key)
		}
	}
	return nil
}

func validateNativeSettingsNumber(namespace, key string, value any, min, max float64, integer bool) (float64, error) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	default:
		return 0, fmt.Errorf("%s.%s must be a number", namespace, key)
	}
	if integer && number != float64(int64(number)) {
		return 0, fmt.Errorf("%s.%s must be an integer", namespace, key)
	}
	if number < min || (max > 0 && number > max) {
		return 0, fmt.Errorf("%s.%s is outside the permitted range", namespace, key)
	}
	return number, nil
}

func validateNativeSettingsEnum(namespace, key string, value any, allowed ...string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s must be a string", namespace, key)
	}
	for _, candidate := range allowed {
		if text == candidate {
			return text, nil
		}
	}
	return "", fmt.Errorf("%s.%s has an unsupported value", namespace, key)
}

func validateNativeSettingsValue(namespace string, value any) error {
	root, ok := value.(map[string]any)
	if !ok || root == nil {
		return fmt.Errorf("%s settings must be an object", namespace)
	}
	switch namespace {
	case nativeSettingsTheme:
		if err := validateNativeSettingsObject(namespace, root, []string{"preference", "fontSize"}); err != nil {
			return err
		}
		if raw, exists := root["preference"]; exists {
			if _, err := validateNativeSettingsEnum(namespace, "preference", raw, "light", "dark", "system"); err != nil {
				return err
			}
		}
		if raw, exists := root["fontSize"]; exists {
			if _, err := validateNativeSettingsNumber(namespace, "fontSize", raw, 12, 17, true); err != nil {
				return err
			}
		}
	case nativeSettingsLocale:
		if err := validateNativeSettingsObject(namespace, root, []string{"preference"}); err != nil {
			return err
		}
		if raw, exists := root["preference"]; exists {
			text, ok := raw.(string)
			if !ok || !nativeLocalePattern.MatchString(text) {
				return fmt.Errorf("%s.preference must be a locale id", namespace)
			}
		}
	case nativeSettingsConversation:
		if err := validateNativeSettingsObject(namespace, root, []string{"busyEnter"}); err != nil {
			return err
		}
		if raw, exists := root["busyEnter"]; exists {
			if _, err := validateNativeSettingsEnum(namespace, "busyEnter", raw, "queue", "steer"); err != nil {
				return err
			}
		}
	case nativeSettingsPermission:
		if err := validateNativeSettingsObject(namespace, root, []string{"defaultPreset"}); err != nil {
			return err
		}
		if _, err := validateNativeSettingsEnum(namespace, "defaultPreset", root["defaultPreset"], "readonly", "standard", "full"); err != nil {
			return err
		}
	case nativeSettingsShell:
		if err := validateNativeSettingsObject(namespace, root, []string{
			"cwd", "timeoutMs", "maxTimeoutMs", "maxOutputBytes", "maxSpillBytes", "graceMs", "pwshPath",
		}); err != nil {
			return err
		}
		for _, key := range []string{"cwd", "pwshPath"} {
			if raw, exists := root[key]; exists && nativeString(raw) == "" {
				return fmt.Errorf("%s.%s must be a non-empty string", namespace, key)
			}
		}
		for _, key := range []string{"timeoutMs", "maxTimeoutMs", "maxOutputBytes", "maxSpillBytes"} {
			if raw, exists := root[key]; exists {
				if _, err := validateNativeSettingsNumber(namespace, key, raw, 1, 0, true); err != nil {
					return err
				}
			}
		}
		if raw, exists := root["graceMs"]; exists {
			if _, err := validateNativeSettingsNumber(namespace, "graceMs", raw, 1, 2147483647, true); err != nil {
				return err
			}
		}
	case nativeSettingsAgentLoop:
		if err := validateNativeSettingsObject(namespace, root, []string{"maxParallelToolCalls"}); err != nil {
			return err
		}
		if _, err := validateNativeSettingsNumber(namespace, "maxParallelToolCalls", root["maxParallelToolCalls"], 1, 0, true); err != nil {
			return err
		}
	case nativeSettingsDefaultModel:
		if err := validateNativeSettingsObject(namespace, root, []string{"provider", "model", "reasoningEffort"}); err != nil {
			return err
		}
		for _, key := range []string{"provider", "model"} {
			if nativeString(root[key]) == "" {
				return fmt.Errorf("%s.%s is required", namespace, key)
			}
		}
	case nativeSettingsWebSearch:
		if err := validateNativeSettingsObject(namespace, root, []string{
			"apiKey", "apiKeyEnv", "baseURL", "model", "apiVersion", "maxTokens", "maxUses",
		}); err != nil {
			return err
		}
		for _, key := range []string{"apiKey", "apiKeyEnv", "baseURL", "model", "apiVersion"} {
			if raw, exists := root[key]; exists && nativeString(raw) == "" {
				return fmt.Errorf("%s.%s must be a non-empty string", namespace, key)
			}
		}
		for _, key := range []string{"maxTokens", "maxUses"} {
			if raw, exists := root[key]; exists {
				if _, err := validateNativeSettingsNumber(namespace, key, raw, 1, 0, true); err != nil {
					return err
				}
			}
		}
	default:
		return nil
	}
	return nil
}

func (s *Server) subscribeNativeSettingsDocumentUpdated(callback func(namespace string, revision int)) func() {
	s.nativeSettingsSubscribersMu.Lock()
	defer s.nativeSettingsSubscribersMu.Unlock()
	if s.nativeSettingsSubscribers == nil {
		s.nativeSettingsSubscribers = make(map[uint64]func(string, int))
	}
	s.nativeSettingsSubscriberID++
	id := s.nativeSettingsSubscriberID
	s.nativeSettingsSubscribers[id] = callback
	return func() {
		s.nativeSettingsSubscribersMu.Lock()
		defer s.nativeSettingsSubscribersMu.Unlock()
		delete(s.nativeSettingsSubscribers, id)
	}
}

func (s *Server) notifyNativeSettingsDocumentUpdated(namespace string, revision int) {
	s.nativeSettingsSubscribersMu.Lock()
	callbacks := make([]func(string, int), 0, len(s.nativeSettingsSubscribers))
	for _, callback := range s.nativeSettingsSubscribers {
		callbacks = append(callbacks, callback)
	}
	s.nativeSettingsSubscribersMu.Unlock()
	for _, callback := range callbacks {
		callback(namespace, revision)
	}
}

// subscribeNativeCredentialUpdated registers a host/remote-event sink for the
// canonical credentials/updated owner event. The payload is the reference name
// only.
func (s *Server) subscribeNativeCredentialUpdated(callback func(ref string)) func() {
	s.nativeCredentialSubscribersMu.Lock()
	defer s.nativeCredentialSubscribersMu.Unlock()
	if s.nativeCredentialSubscribers == nil {
		s.nativeCredentialSubscribers = make(map[uint64]func(string))
	}
	s.nativeCredentialSubscriberID++
	id := s.nativeCredentialSubscriberID
	s.nativeCredentialSubscribers[id] = callback
	return func() {
		s.nativeCredentialSubscribersMu.Lock()
		defer s.nativeCredentialSubscribersMu.Unlock()
		delete(s.nativeCredentialSubscribers, id)
	}
}

// NotifyNativeCredentialUpdated is called by the composition root after a
// provider-managed credential source commits. It is intentionally value-free.
func (s *Server) NotifyNativeCredentialUpdated(ref string) {
	s.nativeCredentialSubscribersMu.Lock()
	callbacks := make([]func(string), 0, len(s.nativeCredentialSubscribers))
	for _, callback := range s.nativeCredentialSubscribers {
		callbacks = append(callbacks, callback)
	}
	s.nativeCredentialSubscribersMu.Unlock()
	for _, callback := range callbacks {
		callback(ref)
	}
}

// subscribeNativeLLMAdaptersUpdated registers a host/remote-event sink for the
// canonical payload-free LLM topology invalidation event.
func (s *Server) subscribeNativeLLMAdaptersUpdated(callback func()) func() {
	s.nativeLLMAdapterSubscribersMu.Lock()
	defer s.nativeLLMAdapterSubscribersMu.Unlock()
	if s.nativeLLMAdapterSubscribers == nil {
		s.nativeLLMAdapterSubscribers = make(map[uint64]func())
	}
	s.nativeLLMAdapterSubscriberID++
	id := s.nativeLLMAdapterSubscriberID
	s.nativeLLMAdapterSubscribers[id] = callback
	return func() {
		s.nativeLLMAdapterSubscribersMu.Lock()
		defer s.nativeLLMAdapterSubscribersMu.Unlock()
		delete(s.nativeLLMAdapterSubscribers, id)
	}
}

// NotifyNativeLLMAdaptersUpdated is called after a provider registry topology
// is committed, so connected configuration surfaces re-read provider views.
func (s *Server) NotifyNativeLLMAdaptersUpdated() {
	s.nativeLLMAdapterSubscribersMu.Lock()
	callbacks := make([]func(), 0, len(s.nativeLLMAdapterSubscribers))
	for _, callback := range s.nativeLLMAdapterSubscribers {
		callbacks = append(callbacks, callback)
	}
	s.nativeLLMAdapterSubscribersMu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
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
			if defaultMaxTokens := nativeNumber(model["default_max_tokens"]); defaultMaxTokens > 0 {
				entry["defaultMaxTokens"] = defaultMaxTokens
			}
			if value := nativeBoolPtr(model["reasoning"]); value != nil {
				entry["reasoning"] = *value
			}
			if value := nativeBoolPtr(model["tools"]); value != nil {
				entry["tools"] = *value
			}
			if value := nativeBoolPtr(model["vision"]); value != nil {
				entry["vision"] = *value
			}
			if input := nativeStrings(model["input"]); len(input) > 0 {
				entry["input"] = input
			}
			if efforts, exists := model["reasoning_efforts"]; exists && efforts != nil {
				entry["reasoningEfforts"] = efforts
			}
			if effort := nativeString(model["default_reasoning_effort"]); effort != "" {
				entry["defaultEffort"] = effort
			}
			if value := nativeBoolPtr(model["audio"]); value != nil {
				entry["audio"] = *value
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

func nativeBoolPtr(value any) *bool {
	if typed, ok := value.(bool); ok {
		return &typed
	}
	return nil
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
		attached := 0
		if s.liveAgentCountFn != nil {
			attached = s.liveAgentCountFn()
		}
		value := map[string]any{
			"version":          "shutu-agent",
			"cwd":              s.defaultWorkdir,
			"attachedSessions": attached,
			"home":             nativeHomeDirectory(),
			"canOpenPath":      nativeCanOpenPath(),
		}
		if cfg := s.cfgFn; cfg != nil {
			view := cfg()
			provider := nativeString(view["llm_provider"])
			if provider == "" {
				provider = nativeString(view["provider"])
			}
			if provider != "" {
				value["provider"] = provider
			}
			if model := nativeString(view["model"]); model != "" {
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
	case "host.openPath":
		var req struct {
			Path string `json:"path"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeHostOpenPath(r, req.Path)
	case "host.pickDirectory":
		return s.nativeHostPickDirectory(r)
	case "commands/list":
		if s.nativeCommandManager == nil {
			return nativeRPCFailure("not-supported", "command manager not wired", nil)
		}
		req, err := nativeCommandRequestFromPayload(raw)
		if err != nil {
			return nativeRPCFailure("bad-request", err.Error(), nil)
		}
		commands, err := s.nativeCommandManager.List(r.Context(), req.SessionID)
		if err != nil {
			return nativeRPCFailure("command-list-failed", err.Error(), nil)
		}
		items := make([]map[string]any, 0, len(commands))
		for _, command := range commands {
			name := strings.TrimSpace(command.Name)
			description := strings.TrimSpace(command.Description)
			if name == "" || description == "" {
				continue
			}
			item := map[string]any{"name": name, "description": description}
			if hint := strings.TrimSpace(command.InputHint); hint != "" {
				input := map[string]any{"hint": hint}
				if command.Images {
					input["images"] = true
				}
				item["input"] = input
			}
			items = append(items, item)
		}
		return nativeRPCSuccess(items)
	case "commands/execute":
		if s.nativeCommandManager == nil {
			return nativeRPCFailure("not-supported", "command manager not wired", nil)
		}
		req, err := nativeCommandRequestFromPayload(raw)
		if err != nil {
			return nativeRPCFailure("bad-request", err.Error(), nil)
		}
		execution, matched, err := s.nativeCommandManager.Execute(r.Context(), req.SessionID, req.Line, req.Images)
		if err != nil {
			return nativeRPCFailure("command-execute-failed", err.Error(), nil)
		}
		if !matched {
			return nativeRPCSuccess(nil)
		}
		result := map[string]any{"kind": execution.Result.Kind}
		if execution.Result.Text != "" {
			result["text"] = execution.Result.Text
		}
		if execution.Result.SourceEventSeq != nil {
			result["sourceEventSeq"] = *execution.Result.SourceEventSeq
		}
		return nativeRPCSuccess(map[string]any{
			"commandId": execution.CommandID,
			"result":    result,
		})
	case "messageFeedback/list":
		return s.nativeMessageFeedbackList(r, raw)
	case "messageFeedback/put":
		return s.nativeMessageFeedbackPut(r, raw)
	case "messageFeedback/delete":
		return s.nativeMessageFeedbackDelete(r, raw)
	case "fileReferences/list":
		return s.nativeFileReferencesList(r, raw)
	case "sessionReferenceResolver/candidates":
		return s.nativeSessionReferenceCandidates(r, raw)
	case "pluginInventory/list":
		return nativePluginInventory()
	case "session.list":
		return s.nativeSessionList(r)
	case "session.search":
		return s.nativeSessionSearch(r, raw)
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
		if title == "" {
			return nativeRPCFailure("title-invalid", "session title must contain visible characters", map[string]any{"sessionId": req.SessionID})
		}
		if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
			return failure
		}
		if s.nativeSessionRenameFn != nil {
			seq, err := s.nativeSessionRenameFn(r.Context(), req.SessionID, title)
			if err != nil {
				return nativeStoreFailure(err)
			}
			return nativeRPCSuccess(map[string]any{"title": title, "seq": seq})
		}
		if err := s.store.SetSessionTitle(r.Context(), req.SessionID, title, session.TitleSourceUser); err != nil {
			return nativeStoreFailure(err)
		}
		// Compatibility embedders without a live runtime cannot mint a log
		// event. Production cmd/sta always wires nativeSessionRenameFn above;
		// retain the old metadata-only path only for those embedders.
		return nativeRPCSuccess(map[string]any{"title": title, "seq": 0})
	case "session.prompt":
		return s.nativeSessionPrompt(r, raw)
	case "session.attachment":
		return s.nativeSessionAttachment(r, raw)
	case "session.models":
		return s.nativeSessionModels(r, raw)
	case "session.selectModel":
		return s.nativeSessionSelectModel(r, raw)
	case "session.fork":
		return s.nativeSessionFork(r, raw)
	case "session.updateQueue":
		return s.nativeSessionUpdateQueue(r, raw)
	case "session.cancel":
		var req nativeSessionIDRequest
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		req.SessionID = strings.TrimSpace(req.SessionID)
		if req.SessionID == "" {
			return nativeRPCFailure("bad-request", "sessionId is required", nil)
		}
		if s.stopFn == nil {
			return nativeRPCFailure("not-supported", "turn stopper not wired", nil)
		}
		// Generic session.cancel is the ordinary-session stop boundary. Child
		// identities reserved for subagent delivery are fenced first; a header
		// lookup keeps the fast running-turn cancellation path bounded.
		if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
			return failure
		}
		// Cancellation is a control-plane operation. Ask the in-memory turn
		// owner first: validating a running 100k-event session through SQLite can
		// wait behind the append writer and make the stop button appear frozen.
		// Only the idle/no-turn path needs the durable existence check below.
		if err := s.stopFn(req.SessionID); err != nil {
			if !strings.Contains(err.Error(), "no turn running") {
				return nativeRPCFailure("cancel-failed", err.Error(), nil)
			}
			if _, metaErr := s.store.GetSessionMeta(r.Context(), req.SessionID); metaErr != nil {
				if errors.Is(metaErr, store.ErrNotFound) {
					return nativeRPCFailure("session-not-found", "session not found", map[string]any{"sessionId": req.SessionID})
				}
				return nativeStoreFailure(metaErr)
			}
		}
		return nativeRPCSuccess(map[string]any{"accepted": true})
	case "workspace.list":
		return s.nativeWorkspaceList(r)
	case "workspace.create":
		var req struct {
			Path string `json:"path"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeWorkspaceCreate(r, req.Path)
	case "workspace.rename":
		var req struct {
			WorkspaceID string `json:"workspaceId"`
			Title       string `json:"title"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeWorkspaceRename(r, req.WorkspaceID, req.Title)
	case "workspace.delete":
		var req struct {
			WorkspaceID string `json:"workspaceId"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeWorkspaceDelete(r, req.WorkspaceID)
	case "workspace.insertBefore":
		var req struct {
			WorkspaceID     string `json:"workspaceId"`
			BeforeWorkspace string `json:"beforeWorkspaceId"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeWorkspaceInsertBefore(r, req.WorkspaceID, req.BeforeWorkspace)
	case "workspace.insertSessionBefore":
		var req struct {
			WorkspaceID   string `json:"workspaceId"`
			SessionID     string `json:"sessionId"`
			BeforeSession string `json:"beforeSessionId"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeWorkspaceInsertSessionBefore(r, req.WorkspaceID, req.SessionID, req.BeforeSession)
	case "workspace.archiveSession":
		var req nativeSessionIDRequest
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeWorkspaceArchiveSession(r, req.SessionID)
	case "agentPreset.list":
		return s.nativeAgentPresetList(r)
	case "agentPreset.select":
		return s.nativeAgentPresetSelect(r, raw)
	case "agentPreset.read":
		return s.nativeAgentPresetRead(r, raw)
	case "agentPreset.copy":
		return s.nativeAgentPresetCopy(r, raw)
	case "agentPreset.openDocument":
		return s.nativeAgentPresetOpenDocument(r, raw)
	case "agentPreset.remove":
		return s.nativeAgentPresetRemove(r, raw)
	case "settings.describe":
		return s.nativeSettingsDescribe()
	case "settings.openDocument":
		return s.nativeSettingsOpenDocument(r, raw)
	case "settings.mutate":
		return s.nativeSettingsMutate(r.Context(), raw)
	case "settings.update":
		var req struct {
			Namespace string         `json:"ns"`
			Patch     map[string]any `json:"patch"`
			Expected  *int           `json:"expectedRevision"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		keys := make([]string, 0, len(req.Patch))
		for key := range req.Patch {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ops := make([]nativeSettingsPathOp, 0, len(keys))
		for _, key := range keys {
			ops = append(ops, nativeSettingsPathOp{Op: "set", Path: []string{key}, Value: req.Patch[key]})
		}
		return s.nativeSettingsApply(r.Context(), req.Namespace, ops, req.Expected)
	case "settings.replace":
		var req struct {
			Namespace string         `json:"ns"`
			Section   map[string]any `json:"section"`
			Expected  *int           `json:"expectedRevision"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		return s.nativeSettingsApply(r.Context(), req.Namespace, []nativeSettingsPathOp{{Op: "set", Value: req.Section}}, req.Expected)
	case "credentials.describe":
		var req struct {
			Refs []string `json:"refs"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		if len(req.Refs) > 64 {
			return nativeRPCFailure("bad-request", "refs must contain at most 64 entries", nil)
		}
		credentials := make(map[string]any, len(req.Refs))
		providers := []map[string]any{}
		if s.cfgFn != nil {
			providers = nativeConfigProviderMaps(s.cfgFn()["providers"])
		}
		for _, ref := range req.Refs {
			if !nativeCredentialRefValid(ref) {
				return nativeRPCFailure("bad-request", "credential ref must be a valid environment variable name", map[string]any{"ref": ref})
			}
			configured := false
			source := ""
			writable := true
			if value := os.Getenv(ref); value != "" {
				configured = true
				source = "env"
				writable = false
			}
			for _, provider := range providers {
				if nativeString(provider["env_var"]) == ref {
					if !configured {
						configured = nativeBool(provider["configured"])
						if configured {
							source = "file"
						}
					}
					break
				}
			}
			// Never return a secret. An environment value is a read-only layer;
			// advertising it as writable would make a credentials.set appear to
			// succeed while the effective value remains unchanged.
			view := map[string]any{"configured": configured, "writable": writable}
			if source != "" {
				view["source"] = source
			}
			credentials[ref] = view
		}
		return nativeRPCSuccess(map[string]any{"credentials": credentials})
	case "credentials.set":
		var req struct {
			Ref   string `json:"ref"`
			Value string `json:"value"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		req.Ref = strings.TrimSpace(req.Ref)
		if !nativeCredentialRefValid(req.Ref) || req.Value == "" {
			return nativeRPCFailure("bad-request", "ref must be a valid credential name and value is required", nil)
		}
		if s.nativeCredentialSetFn == nil {
			return nativeRPCFailure("not-supported", "credential set handler not wired", nil)
		}
		if err := s.nativeCredentialSetFn(r.Context(), req.Ref, req.Value); err != nil {
			return nativeRPCFailure("credential-rejected", err.Error(), map[string]any{"ref": req.Ref})
		}
		return nativeRPCSuccess(map[string]any{})
	case "credentials.unset":
		var req struct {
			Ref string `json:"ref"`
		}
		if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
			return failure
		}
		req.Ref = strings.TrimSpace(req.Ref)
		if !nativeCredentialRefValid(req.Ref) {
			return nativeRPCFailure("bad-request", "ref must be a valid credential name", nil)
		}
		if s.nativeCredentialUnsetFn == nil {
			return nativeRPCFailure("not-supported", "credential unset handler not wired", nil)
		}
		if err := s.nativeCredentialUnsetFn(r.Context(), req.Ref); err != nil {
			return nativeRPCFailure("credential-rejected", err.Error(), map[string]any{"ref": req.Ref})
		}
		return nativeRPCSuccess(map[string]any{})
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
	case "llm.discoverModels":
		return s.nativeLLMDiscoverModels(r, raw)
	case "skill.list":
		return s.nativeSkillList(r, raw)
	case "subagent.list":
		return s.nativeSubagentList(r, raw)
	case "subagent.history":
		return s.nativeSubagentHistory(r, raw)
	case "subagent.prompt":
		return s.nativeSubagentPrompt(r, raw)
	case "subagent.interrupt":
		return s.nativeSubagentInterrupt(r, raw)
	case "goal.create", "goal.edit", "goal.pause", "goal.resume", "goal.complete", "goal.clear":
		return s.nativeGoalMutation(r, method, raw)
	case "goals/create", "goals/edit", "goals/pause", "goals/resume", "goals/complete", "goals/clear":
		return s.nativeGoalRemoteMutation(r, method, raw)
	case "dynamicCordisRunner/syncInspectManifest":
		return s.profileUnavailable(profile.IDCordisDynamicRunner)
	case "dynamicCordisRunner/inventory":
		return s.profileUnavailable(profile.IDCordisDynamicRunner)
	case "runtime.profiles":
		return nativeRPCSuccess(map[string]any{
			"default":  profile.IDStorageSQLite,
			"profiles": s.profileRegistry.List(),
		})
	case "runtime.profile":
		var request struct {
			ID string `json:"id"`
		}
		if failure := nativeDecode(raw, &request); !failure.OK && failure.Error != nil {
			return failure
		}
		descriptor, err := s.profileRegistry.Get(request.ID)
		if err != nil {
			return nativeRPCFailure("profile-unknown", err.Error(), map[string]any{"profile": request.ID})
		}
		if err := s.profileRegistry.Use(request.ID); err != nil {
			return nativeRPCFailure("profile-unsupported", err.Error(), map[string]any{"profile": descriptor})
		}
		return nativeRPCSuccess(descriptor)
	case "runtime.capabilities":
		return nativeRPCSuccess(map[string]any{
			"capabilities": profile.Classifications(),
			"count":        len(profile.CapabilityIDs),
		})
	case "runtime.capability":
		var request struct {
			ID string `json:"id"`
		}
		if failure := nativeDecode(raw, &request); !failure.OK && failure.Error != nil {
			return failure
		}
		capability, err := profile.GetCapability(request.ID)
		if err != nil {
			return nativeRPCFailure("capability-unknown", err.Error(), map[string]any{"capability": request.ID})
		}
		if capability.State != profile.StateAvailable {
			return nativeRPCFailure("capability-unsupported", capability.Reason, map[string]any{"capability": capability})
		}
		return nativeRPCSuccess(capability)
	case "runtime.crash-contracts":
		return nativeRPCSuccess(map[string]any{
			"contracts": s.crashContracts.List(),
		})
	case "runtime.crash-contract":
		var request struct {
			ID string `json:"id"`
		}
		if failure := nativeDecode(raw, &request); !failure.OK && failure.Error != nil {
			return failure
		}
		contract, err := s.crashContracts.Get(request.ID)
		if err != nil {
			return nativeRPCFailure("crash-boundary-unknown", err.Error(), map[string]any{"boundary": request.ID})
		}
		return nativeRPCSuccess(contract)
	default:
		return nativeRPCFailure("not-supported", "native RPC method is not implemented", map[string]any{"method": method})
	}
}

func (s *Server) nativeSettingsOpenDocument(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req map[string]any
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if s.nativeSettingsOpenDocumentFn == nil {
		return nativeRPCFailure("not-supported", "settings document opener not wired", nil)
	}
	if err := s.nativeSettingsOpenDocumentFn(r.Context()); err != nil {
		return nativeRPCFailure("open-settings-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"opened": true})
}

func nativeHomeDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Clean(home)
}

func nativeCanOpenPath() bool {
	switch runtime.GOOS {
	case "windows", "darwin", "linux", "freebsd", "openbsd", "netbsd":
		return true
	default:
		return false
	}
}

func nativeFeedbackItem(sessionID string, item store.MessageFeedback) map[string]any {
	value := map[string]any{
		"messageId": nativeMessageID(sessionID, item.Seq),
		"rating":    item.Rating,
		"version":   nativeFeedbackVersion(item),
		"createdAt": item.CreatedAt.UnixMilli(),
		"updatedAt": item.UpdatedAt.UnixMilli(),
	}
	if item.Note != "" {
		value["note"] = item.Note
	}
	return value
}

func nativeFeedbackVersion(item store.MessageFeedback) string {
	// The existing sidecar predates DSH's opaque version column. Derive a
	// stable equality token from the durable row and include the mutable fields
	// so every material replacement invalidates an older browser observation.
	noteHash := sha1.Sum([]byte(item.Note))
	return fmt.Sprintf("shutu-feedback:%d:%d:%s:%x", item.UpdatedAt.UnixNano(), item.Seq, item.Rating, noteHash[:6])
}

func nativeFeedbackSeq(events []session.Event, sessionID, messageID string) (uint64, bool) {
	for _, event := range events {
		if event.Type == session.EventAssistantMessage && nativeMessageID(sessionID, event.Seq) == messageID {
			return event.Seq, true
		}
	}
	return 0, false
}

func nativeFeedbackRejected(errorValue map[string]any) any {
	return map[string]any{"ok": false, "error": errorValue}
}

func nativeFeedbackSuccess(value any) any {
	return map[string]any{"ok": true, "value": value}
}

func (s *Server) nativeMessageFeedbackList(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeMessageFeedbackListRequest
	if err := nativeDecodeRemoteRequest(raw, &req); err != nil {
		return nativeRPCFailure("bad-request", "feedback request is invalid", map[string]any{"message": err.Error()})
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	feedback, ok := s.store.(store.MessageFeedbackStore)
	if !ok {
		return nativeRPCFailure("not-supported", "message feedback store not wired", nil)
	}
	items, err := feedback.ListMessageFeedback(r.Context(), req.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "session-not-found", "sessionId": req.SessionID}))
		}
		return nativeRPCFailure("feedback-list-failed", err.Error(), nil)
	}
	events, err := s.store.LoadSession(r.Context(), req.SessionID)
	if err != nil {
		return nativeRPCFailure("feedback-list-failed", err.Error(), nil)
	}
	values := make([]any, 0, len(items))
	for _, item := range items {
		if _, found := nativeFeedbackSeq(events, req.SessionID, nativeMessageID(req.SessionID, item.Seq)); found {
			values = append(values, nativeFeedbackItem(req.SessionID, item))
		}
	}
	return nativeRPCSuccess(nativeFeedbackSuccess(map[string]any{"items": values}))
}

func (s *Server) nativeMessageFeedbackPut(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeMessageFeedbackPutRequest
	if err := nativeDecodeRemoteRequest(raw, &req); err != nil {
		return nativeRPCFailure("bad-request", "feedback request is invalid", map[string]any{"message": err.Error()})
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.MessageID = strings.TrimSpace(req.MessageID)
	if req.SessionID == "" || req.MessageID == "" {
		return nativeRPCFailure("bad-request", "sessionId and messageId are required", nil)
	}
	if req.Rating != "positive" && req.Rating != "negative" {
		return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "invalid-rating"}))
	}
	if req.Note != nil {
		if strings.TrimSpace(*req.Note) == "" {
			return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "note-blank"}))
		}
		if len([]byte(*req.Note)) > store.MaxMessageFeedbackNoteBytes {
			return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "note-too-large", "maxBytes": store.MaxMessageFeedbackNoteBytes, "actualBytes": len([]byte(*req.Note))}))
		}
	}
	feedback, ok := s.store.(store.MessageFeedbackStore)
	if !ok {
		return nativeRPCFailure("not-supported", "message feedback store not wired", nil)
	}
	events, err := s.store.LoadSession(r.Context(), req.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "session-not-found", "sessionId": req.SessionID}))
		}
		return nativeRPCFailure("feedback-put-failed", err.Error(), nil)
	}
	seq, found := nativeFeedbackSeq(events, req.SessionID, req.MessageID)
	if !found {
		return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "target-not-found", "sessionId": req.SessionID, "messageId": req.MessageID}))
	}
	current, exists, err := feedback.GetMessageFeedback(r.Context(), req.SessionID, seq)
	if err != nil {
		return nativeRPCFailure("feedback-put-failed", err.Error(), nil)
	}
	if (exists && req.IfVersion == nil) || (exists && req.IfVersion != nil && *req.IfVersion != nativeFeedbackVersion(current)) || (!exists && req.IfVersion != nil) {
		var currentValue any
		if exists {
			currentValue = nativeFeedbackItem(req.SessionID, current)
		}
		return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "version-conflict", "current": currentValue}))
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	updated, err := feedback.PutMessageFeedback(r.Context(), req.SessionID, seq, req.Rating, note)
	if err != nil {
		return nativeRPCFailure("feedback-put-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(nativeFeedbackSuccess(nativeFeedbackItem(req.SessionID, updated)))
}

func (s *Server) nativeMessageFeedbackDelete(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeMessageFeedbackDeleteRequest
	if err := nativeDecodeRemoteRequest(raw, &req); err != nil {
		return nativeRPCFailure("bad-request", "feedback request is invalid", map[string]any{"message": err.Error()})
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.MessageID = strings.TrimSpace(req.MessageID)
	if req.SessionID == "" || req.MessageID == "" {
		return nativeRPCFailure("bad-request", "sessionId and messageId are required", nil)
	}
	feedback, ok := s.store.(store.MessageFeedbackStore)
	if !ok {
		return nativeRPCFailure("not-supported", "message feedback store not wired", nil)
	}
	events, err := s.store.LoadSession(r.Context(), req.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "session-not-found", "sessionId": req.SessionID}))
		}
		return nativeRPCFailure("feedback-delete-failed", err.Error(), nil)
	}
	seq, found := nativeFeedbackSeq(events, req.SessionID, req.MessageID)
	if !found {
		return nativeRPCSuccess(nativeFeedbackSuccess(map[string]any{"absent": true}))
	}
	current, exists, err := feedback.GetMessageFeedback(r.Context(), req.SessionID, seq)
	if err != nil {
		return nativeRPCFailure("feedback-delete-failed", err.Error(), nil)
	}
	if exists && req.IfVersion != nativeFeedbackVersion(current) {
		return nativeRPCSuccess(nativeFeedbackRejected(map[string]any{"code": "version-conflict", "current": nativeFeedbackItem(req.SessionID, current)}))
	}
	if err := feedback.DeleteMessageFeedback(r.Context(), req.SessionID, seq); err != nil {
		return nativeRPCFailure("feedback-delete-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(nativeFeedbackSuccess(map[string]any{"absent": true}))
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

func (s *Server) nativeHostOpenPath(r *http.Request, rawPath string) nativeRPCResult {
	path, err := s.nativeDirectoryPath(rawPath, false)
	if err != nil {
		return nativeRPCFailure("directory-unreadable", err.Error(), map[string]any{"path": strings.TrimSpace(rawPath)})
	}
	if _, err := os.Stat(path); err != nil {
		return nativeRPCFailure("directory-unreadable", fmt.Sprintf("cannot open %q: %v", path, err), map[string]any{"path": path})
	}
	if err := OpenNativePath(r.Context(), path); err != nil {
		return nativeRPCFailure("open-path-failed", fmt.Sprintf("cannot open %q: %v", path, err), map[string]any{"path": path})
	}
	// The desktop opener owns the child process. Waiting here would turn a
	// successful hand-off into a long-running RPC and would block the native UI.
	return nativeRPCSuccess(map[string]any{"opened": true})
}

// OpenNativePath hands an existing file or directory to the platform opener.
// It is exported for composition-root callbacks such as settings.openDocument;
// callers must resolve any user-facing identifier to a concrete path before
// invoking it.
func OpenNativePath(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || (runtime.GOOS == "windows" && filepath.VolumeName(path) == "") {
		return fmt.Errorf("%q is not a fully qualified path", path)
	}
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "explorer.exe", path)
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", path)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", path)
	}
	return cmd.Start()
}

func nativeCredentialRefValid(ref string) bool {
	if ref == "" || len(ref) > 256 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
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
		// The settings directory intentionally contains dormant providers so
		// users can configure them. The session model picker is different: it
		// must only expose routes that are configured and usable now.
		if !nativeBool(provider["configured"]) || !nativeBool(provider["available"]) {
			continue
		}
		id := nativeString(provider["id"])
		models := nativeConfiguredProviderModels(provider)
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
			} else if value := nativeBoolPtr(model["reasoning"]); value != nil {
				entry["reasoning"] = *value
			}
			for _, key := range []string{"contextWindow", "maxTokens", "defaultMaxTokens", "tools", "vision", "audio", "input", "reasoningEfforts", "defaultEffort"} {
				if value, exists := model[key]; exists {
					entry[key] = value
				}
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

// nativeConfiguredProviderModels is the model source for llm.models and
// session.models. Provider candidates are discovery/catalog suggestions for
// settings and must never become selectable session routes by themselves.
func nativeConfiguredProviderModels(provider map[string]any) []any {
	if models := nativeConfigMaps(provider["catalog_models"]); len(models) > 0 {
		result := make([]any, 0, len(models))
		for _, model := range models {
			result = append(result, model)
		}
		return result
	}
	if models := nativeProviderModels(provider); len(models) > 0 {
		if nativeConfigMaps(provider["models"]) != nil {
			return models
		}
	}
	model := nativeString(provider["model"])
	if model == "" {
		return nil
	}
	return []any{map[string]any{"id": model}}
}

func (s *Server) nativeLLMDiscoverModels(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.setDiscoverFn == nil {
		return nativeRPCFailure("not-supported", "provider discovery not wired", nil)
	}
	var req struct {
		SettingsNS string `json:"settingsNs"`
		Provider   string `json:"provider"`
		BaseURL    string `json:"baseURL"`
		API        string `json:"api"`
		APIKey     string `json:"apiKey"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.SettingsNS) == "" {
		return nativeRPCFailure("bad-request", "settingsNs is required", nil)
	}
	models, err := s.setDiscoverFn(r.Context(), ProviderDiscover{
		Provider: strings.TrimSpace(req.Provider),
		BaseURL:  strings.TrimSpace(req.BaseURL),
		Protocol: strings.TrimSpace(req.API),
		APIKey:   req.APIKey,
	})
	if err != nil {
		return nativeRPCFailure("model-discovery-failed", err.Error(), map[string]any{
			"settingsNs": req.SettingsNS,
			"baseURL":    strings.TrimSpace(req.BaseURL),
		})
	}
	entries := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		entry := map[string]any{"id": id}
		if name := strings.TrimSpace(model.Name); name != "" {
			entry["name"] = name
		}
		if model.ContextWindow > 0 {
			entry["contextWindow"] = model.ContextWindow
		}
		if model.MaxTokens > 0 {
			entry["maxTokens"] = model.MaxTokens
		}
		if model.DefaultMaxTokens > 0 {
			entry["defaultMaxTokens"] = model.DefaultMaxTokens
		}
		if model.Reasoning != nil {
			entry["reasoning"] = *model.Reasoning
		}
		if model.ReasoningEfforts != nil {
			entry["reasoningEfforts"] = model.ReasoningEfforts
		}
		if model.DefaultReasoningEffort != "" {
			entry["defaultEffort"] = model.DefaultReasoningEffort
		}
		if model.Tools != nil {
			entry["tools"] = *model.Tools
		}
		if model.Vision != nil {
			entry["vision"] = *model.Vision
		}
		if len(model.Input) > 0 {
			entry["input"] = append([]string(nil), model.Input...)
		}
		if model.Audio != nil {
			entry["audio"] = *model.Audio
		}
		entries = append(entries, entry)
	}
	return nativeRPCSuccess(map[string]any{"models": entries})
}

func (s *Server) nativeSkillList(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.skillCatalogFn == nil && s.skillsFn == nil {
		return nativeRPCFailure("not-supported", "skill registry not wired", nil)
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	if catalog := s.skillCatalogFn; catalog != nil {
		meta, err := s.store.GetSessionMeta(r.Context(), req.SessionID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nativeRPCFailure(
					"session-not-found",
					fmt.Sprintf("session %q not found (not attached)", req.SessionID),
					map[string]any{"sessionId": req.SessionID},
				)
			}
			return nativeRPCFailure("internal", err.Error(), map[string]any{"sessionId": req.SessionID})
		}
		if strings.TrimSpace(meta.CWD) == "" {
			return nativeRPCFailure(
				"internal",
				fmt.Sprintf("session %q has no project cwd", req.SessionID),
				map[string]any{"sessionId": req.SessionID},
			)
		}
		entries, err := catalog(r.Context(), meta.CWD)
		if err != nil {
			return nativeRPCFailure(
				"internal",
				fmt.Sprintf("skill listing failed: %v", err),
				map[string]any{"sessionId": req.SessionID},
			)
		}
		value := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			row := map[string]any{
				"name":           entry.Name,
				"description":    entry.Description,
				"modelInvocable": entry.ModelInvocable,
			}
			if entry.WhenToUse != "" {
				row["whenToUse"] = entry.WhenToUse
			}
			value = append(value, row)
		}
		sort.SliceStable(value, func(left, right int) bool {
			return nativeString(value[left]["name"]) < nativeString(value[right]["name"])
		})
		return nativeRPCSuccess(map[string]any{"skills": value})
	}
	result, err := s.skillsFn(r.Context(), "list", SkillRequest{})
	if err != nil {
		return nativeRPCFailure("skill-list-failed", err.Error(), map[string]any{"sessionId": req.SessionID})
	}
	rows, _ := result["skills"].([]map[string]any)
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if !nativeBool(row["user_invocable"]) {
			continue
		}
		name := nativeString(row["name"])
		if name == "" {
			continue
		}
		entry := map[string]any{
			"name":           name,
			"description":    nativeString(row["description"]),
			"modelInvocable": nativeBool(row["model_invocable"]),
		}
		if whenToUse := nativeString(row["when_to_use"]); whenToUse != "" {
			entry["whenToUse"] = whenToUse
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return nativeString(entries[left]["name"]) < nativeString(entries[right]["name"])
	})
	return nativeRPCSuccess(map[string]any{"skills": entries})
}

func (s *Server) nativeSubagentList(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.subFn == nil {
		return nativeRPCFailure("not-supported", "subagent registry not wired", nil)
	}
	var req struct {
		ParentSessionID string `json:"parentSessionId"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.ParentSessionID = strings.TrimSpace(req.ParentSessionID)
	if req.ParentSessionID == "" {
		return nativeRPCFailure("bad-request", "parentSessionId is required", nil)
	}
	if _, err := s.store.GetSessionMeta(r.Context(), req.ParentSessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("subagent-parent-not-found", "parent session not found", map[string]any{"parentSessionId": req.ParentSessionID})
		}
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	rows, err := s.subFn(r.Context(), req.ParentSessionID)
	if err != nil {
		return nativeRPCFailure("subagent-list-failed", err.Error(), map[string]any{"parentSessionId": req.ParentSessionID})
	}
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		id := nativeString(row["id"])
		if id == "" {
			continue
		}
		mode := nativeString(row["mode"])
		if mode != "continuable" {
			mode = "one-shot"
		}
		entry := map[string]any{
			"kind":        "child",
			"id":          id,
			"mode":        mode,
			"activity":    map[bool]string{true: "running", false: "inactive"}[nativeBool(row["running"])],
			"hasChildren": nativeBool(row["has_children"]),
		}
		if label := nativeString(row["label"]); label != "" {
			entry["label"] = label
		} else if mode == "continuable" {
			entry["label"] = id
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return nativeString(entries[left]["id"]) < nativeString(entries[right]["id"])
	})
	return nativeRPCSuccess(map[string]any{"entries": entries, "parentAvailable": true})
}

func (s *Server) nativeSubagentHistory(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSubagentHistoryRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.ParentSessionID = strings.TrimSpace(req.ParentSessionID)
	req.ChildSessionID = strings.TrimSpace(req.ChildSessionID)
	req.Mode = strings.TrimSpace(req.Mode)
	if req.ParentSessionID == "" || req.ChildSessionID == "" || req.Mode == "" {
		return nativeRPCFailure("bad-request", "parentSessionId, childSessionId, and mode are required", nil)
	}
	if req.Mode != "one-shot" && req.Mode != "continuable" {
		return nativeRPCFailure("bad-request", "mode must be one-shot or continuable", nil)
	}
	events, err := s.store.LoadSession(r.Context(), req.ChildSessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("subagent-not-found", "subagent session not found", map[string]any{"childSessionId": req.ChildSessionID})
		}
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	parent, _, _ := nativeSessionLineage(events)
	if parent != req.ParentSessionID {
		return nativeRPCFailure("subagent-unauthorized", "subagent does not belong to the requested parent", map[string]any{
			"parentSessionId": req.ParentSessionID,
			"childSessionId":  req.ChildSessionID,
		})
	}
	childRaw, err := json.Marshal(nativeHistoryRequest{
		SessionID: req.ChildSessionID, BeforeSeq: req.BeforeSeq, MaxMessages: req.MaxMessages,
	})
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	return s.nativeSessionHistory(r, childRaw)
}

func (s *Server) authorizeNativeSubagent(ctx context.Context, parentID, childID, mode string) (nativeRPCResult, bool) {
	if parentID == "" || childID == "" || mode == "" {
		return nativeRPCFailure("bad-request", "parentSessionId, childSessionId, and mode are required", nil), false
	}
	if mode != "continuable" {
		return nativeRPCFailure("bad-request", "mode must be continuable for this operation", nil), false
	}
	if _, err := s.store.LoadSession(ctx, parentID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("subagent-parent-not-found", "parent session not found", map[string]any{"parentSessionId": parentID}), false
		}
		return nativeRPCFailure("internal", err.Error(), nil), false
	}
	events, err := s.store.LoadSession(ctx, childID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("subagent-not-found", "subagent session not found", map[string]any{"childSessionId": childID}), false
		}
		return nativeRPCFailure("internal", err.Error(), nil), false
	}
	parent, _, _ := nativeSessionLineage(events)
	if parent != parentID {
		return nativeRPCFailure("subagent-unauthorized", "subagent does not belong to the requested parent", map[string]any{
			"parentSessionId": parentID, "childSessionId": childID,
		}), false
	}
	return nativeRPCResult{}, true
}

func (s *Server) nativeSubagentPrompt(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSubagentPromptRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.ParentSessionID = strings.TrimSpace(req.ParentSessionID)
	req.ChildSessionID = strings.TrimSpace(req.ChildSessionID)
	req.Mode = strings.TrimSpace(req.Mode)
	// DSH validates the canonical browser time zone before authorization or
	// any durable subagent work. Keep malformed request metadata fail-fast.
	if strings.TrimSpace(req.ClientTimeZone) != "" {
		if _, err := loadCanonicalPromptTimeZone(req.ClientTimeZone); err != nil {
			return nativePromptTimeZoneFailure(req.ClientTimeZone)
		}
	}
	if failure, ok := s.authorizeNativeSubagent(r.Context(), req.ParentSessionID, req.ChildSessionID, req.Mode); !ok {
		return failure
	}
	content := make([]llm.ContentBlock, 0, len(req.Content))
	var imageInputs []attachment.ImageInput
	var imageIndexes []int
	for _, part := range req.Content {
		switch part.Type {
		case "text":
			content = append(content, llm.Text(part.Text))
		case "image":
			if s.att == nil {
				return nativeRPCFailure("not-supported", "native subagent image prompt is not wired", nil)
			}
			encoded := strings.TrimSpace(part.Data)
			if strings.HasPrefix(encoded, "data:") {
				separator := strings.IndexByte(encoded, ',')
				if separator < 0 {
					return nativeRPCFailure("bad-request", "image data URL is invalid", nil)
				}
				encoded = encoded[separator+1:]
			}
			data, err := nativeDecodeImageBase64(encoded)
			if err != nil {
				return nativeImageBase64Failure(err)
			}
			imageInputs = append(imageInputs, attachment.ImageInput{
				MediaType: strings.TrimSpace(part.MediaType),
				Data:      data,
				Name:      strings.TrimSpace(part.Name),
			})
			imageIndexes = append(imageIndexes, len(content))
			content = append(content, llm.ContentBlock{Kind: llm.BlockImage})
		default:
			return nativeRPCFailure("bad-request", "unsupported subagent prompt content type", nil)
		}
	}
	if len(content) == 0 {
		return nativeRPCFailure("bad-request", "subagent prompt content is required", nil)
	}
	if len(imageInputs) > 0 && s.nativeImageCapabilityFn != nil && !s.nativeImageCapabilityFn(r.Context(), req.ChildSessionID) {
		return nativeImageCapabilityFailure()
	}
	if len(imageInputs) > 0 {
		refs, err := s.att.SaveImages(imageInputs, maxWebImageBytes)
		if err != nil {
			return nativeAttachmentFailure(err)
		}
		for index, ref := range refs {
			content[imageIndexes[index]].Image = ref
		}
	}
	if s.nativeSubagentPromptFn == nil {
		return nativeRPCFailure("not-supported", "subagent prompt handler not wired", nil)
	}
	meta := PromptMeta{RPCID: nativeRPCID(), ClientTimeZone: strings.TrimSpace(req.ClientTimeZone)}
	if err := s.nativeSubagentPromptFn(r.Context(), req.ChildSessionID, content, meta); err != nil {
		return nativeRPCFailure("subagent-prompt-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"messageId": meta.RPCID})
}

func (s *Server) nativeSubagentInterrupt(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSubagentInterruptRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.ParentSessionID = strings.TrimSpace(req.ParentSessionID)
	req.ChildSessionID = strings.TrimSpace(req.ChildSessionID)
	req.Mode = strings.TrimSpace(req.Mode)
	if failure, ok := s.authorizeNativeSubagent(r.Context(), req.ParentSessionID, req.ChildSessionID, req.Mode); !ok {
		return failure
	}
	if s.nativeSubagentInterruptFn == nil {
		return nativeRPCFailure("not-supported", "subagent interrupt handler not wired", nil)
	}
	if err := s.nativeSubagentInterruptFn(req.ChildSessionID, "native subagent interrupt"); err != nil {
		return nativeRPCFailure("subagent-interrupt-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"accepted": true})
}

func nativeGoalRemotePayload(raw json.RawMessage, operation string) (json.RawMessage, error) {
	args, err := nativeRemoteArguments(raw)
	if err != nil || len(args) < 2 {
		return nil, errors.New("goal request requires agent and operation arguments")
	}
	sessionID, _, err := nativeRemoteSessionID(args[0])
	if err != nil || sessionID == "" {
		return nil, errors.New("goal agent id is required")
	}
	payload := map[string]any{"sessionId": sessionID}
	if operation == "create" {
		var request struct {
			Objective     *string `json:"objective"`
			MaxGoalRounds *int    `json:"maxGoalRounds"`
		}
		if err := json.Unmarshal(args[1], &request); err != nil {
			return nil, err
		}
		payload["objective"] = request.Objective
		payload["maxGoalRounds"] = request.MaxGoalRounds
	} else {
		var ref struct {
			ID       string `json:"id"`
			Revision int    `json:"revision"`
		}
		if err := json.Unmarshal(args[1], &ref); err != nil {
			return nil, err
		}
		payload["ref"] = ref
		if operation == "edit" {
			var request struct {
				Objective     *string `json:"objective"`
				MaxGoalRounds *int    `json:"maxGoalRounds"`
			}
			if len(args) < 3 || json.Unmarshal(args[2], &request) != nil {
				return nil, errors.New("goal edit request is invalid")
			}
			payload["objective"] = request.Objective
			payload["maxGoalRounds"] = request.MaxGoalRounds
		}
	}
	return json.Marshal(payload)
}

func (s *Server) nativeGoalRemoteMutation(r *http.Request, method string, raw json.RawMessage) nativeRPCResult {
	operation := strings.TrimPrefix(method, "goals/")
	payload, err := nativeGoalRemotePayload(raw, operation)
	if err != nil {
		return nativeRPCFailure("bad-request", err.Error(), nil)
	}
	result := s.nativeGoalMutation(r, "goal."+operation, payload)
	if operation != "clear" || !result.OK {
		return result
	}
	var value map[string]any
	encoded, _ := json.Marshal(result.Value)
	if json.Unmarshal(encoded, &value) == nil {
		if ref, ok := value["ref"]; ok {
			return nativeRPCSuccess(ref)
		}
	}
	return result
}

func (s *Server) nativeGoalMutation(r *http.Request, method string, raw json.RawMessage) nativeRPCResult {
	var req nativeGoalMutationRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	mutation := NativeGoalMutation{Action: method, SessionID: req.SessionID}
	if req.Objective != nil {
		objective := strings.TrimSpace(*req.Objective)
		req.Objective = &objective
		mutation.Objective = req.Objective
	}
	if req.MaxGoalRounds != nil {
		if *req.MaxGoalRounds <= 0 {
			return nativeRPCFailure("bad-request", "maxGoalRounds must be positive", nil)
		}
		mutation.MaxGoalRounds = req.MaxGoalRounds
	}
	if method == "goal.create" {
		if req.Objective == nil || *req.Objective == "" {
			return nativeRPCFailure("bad-request", "objective is required", nil)
		}
	} else {
		if req.Ref == nil || strings.TrimSpace(req.Ref.ID) == "" || req.Ref.Revision < 1 {
			return nativeRPCFailure("bad-request", "ref.id and positive ref.revision are required", nil)
		}
		if method == "goal.edit" && req.Objective == nil && req.MaxGoalRounds == nil {
			return nativeRPCFailure("bad-request", "goal.edit requires objective or maxGoalRounds", nil)
		}
		mutation.GoalID = strings.TrimSpace(req.Ref.ID)
		mutation.Revision = req.Ref.Revision
	}
	if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
		return failure
	}
	if s.nativeGoalMutationFn == nil {
		return nativeRPCFailure("not-supported", "goal mutation handler not wired", nil)
	}
	result, err := s.nativeGoalMutationFn(r.Context(), mutation)
	if err != nil {
		return nativeRPCFailure("goal-mutation-failed", err.Error(), nil)
	}
	if method == "goal.clear" {
		if !result.Cleared {
			return nativeRPCFailure("goal-mutation-failed", "goal clear handler did not confirm clearing", nil)
		}
		value := map[string]any{"cleared": true}
		if result.GoalID != "" && result.Revision > 0 {
			value["ref"] = map[string]any{"id": result.GoalID, "revision": result.Revision}
		}
		return nativeRPCSuccess(value)
	}
	if strings.TrimSpace(result.GoalID) == "" || result.Revision < 1 {
		return nativeRPCFailure("goal-mutation-failed", "goal mutation handler returned an invalid reference", nil)
	}
	return nativeRPCSuccess(map[string]any{"ref": map[string]any{
		"id": result.GoalID, "revision": result.Revision,
	}})
}

func (s *Server) nativeAgentPresetList(r *http.Request) nativeRPCResult {
	if s.nativeAgentPresetManager != nil {
		catalog, err := s.nativeAgentPresetManager.List(r.Context())
		if err != nil {
			return nativeRPCFailure("agent-preset-list-failed", err.Error(), nil)
		}
		presets := make([]any, 0, len(catalog.Presets))
		for _, preset := range catalog.Presets {
			entry := map[string]any{
				"id": preset.ID, "trust": preset.Trust, "isDefault": preset.IsDefault,
			}
			if preset.Name != "" {
				entry["name"] = preset.Name
			}
			if preset.Description != "" {
				entry["description"] = preset.Description
			}
			if preset.Broken != "" {
				entry["broken"] = preset.Broken
			}
			presets = append(presets, entry)
		}
		return nativeRPCSuccess(map[string]any{
			"presets": presets, "authorable": catalog.Authorable, "hasDocument": catalog.HasDocument,
		})
	}
	defaultPreset := "standard"
	if s.cfgFn != nil {
		if mode := nativeString(s.cfgFn()["mode"]); nativeAgentPresetKnown(mode) && s.nativeAgentPresetAvailable(r.Context(), mode) {
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
	}
	if s.nativeCodeAvailable() {
		presets = append(presets, map[string]any{
			"id": "code", "trust": "system", "isDefault": defaultPreset == "code",
			"name": "Code", "description": "标准能力加程序化 Code Mode",
		})
	}
	return nativeRPCSuccess(map[string]any{
		"presets": presets, "authorable": false, "hasDocument": false,
	})
}

func (s *Server) nativeAgentPresetAvailable(ctx context.Context, preset string) bool {
	if preset == "code" && !s.nativeCodeAvailable() {
		return false
	}
	if nativeAgentPresetKnown(preset) {
		return true
	}
	if s.nativeAgentPresetManager == nil {
		return false
	}
	catalog, err := s.nativeAgentPresetManager.List(ctx)
	if err != nil {
		return false
	}
	for _, entry := range catalog.Presets {
		if entry.ID == preset && entry.Broken == "" {
			return true
		}
	}
	return false
}

func (s *Server) nativeAgentPresetDefault(ctx context.Context) string {
	if s.nativeAgentPresetManager != nil {
		if catalog, err := s.nativeAgentPresetManager.List(ctx); err == nil {
			for _, entry := range catalog.Presets {
				if entry.IsDefault && entry.Broken == "" {
					return entry.ID
				}
			}
			for _, entry := range catalog.Presets {
				if entry.Broken == "" && entry.ID != "" {
					return entry.ID
				}
			}
		}
	}
	if s.cfgFn != nil {
		if mode := nativeString(s.cfgFn()["mode"]); nativeAgentPresetKnown(mode) && s.nativeAgentPresetAvailable(ctx, mode) {
			return mode
		}
	}
	return "standard"
}

// nativeCodeAvailable is derived from the composition root's live capability
// view. A configured code preference is not enough: the external runtime and
// run_code registration must have succeeded before native clients may select
// or advertise the code preset. Legacy embedders without a config provider
// retain the historical optimistic catalog.
func (s *Server) nativeCodeAvailable() bool {
	if s.cfgFn == nil {
		return true
	}
	cfg := s.cfgFn()
	if value, ok := cfg["code_available"]; ok {
		return nativeBool(value)
	}
	if value, ok := cfg["code_enabled"]; ok {
		return nativeBool(value)
	}
	return true
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
	if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
		return failure
	}
	if !s.nativeAgentPresetAvailable(r.Context(), req.AgentPreset) {
		return nativeRPCFailure("agent-preset-invalid", "agent preset is not available", map[string]any{
			"agentPreset": req.AgentPreset, "reason": "preset is not available",
		})
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
		return nativeRPCFailure("agent-preset-locked", "agent preset can only change on a blank session", map[string]any{
			"sessionId": req.SessionID, "agentPreset": req.AgentPreset,
		})
	}
	config, err := configs.GetSessionConfig(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	config.AgentPreset = req.AgentPreset
	if err := configs.SetSessionConfig(r.Context(), req.SessionID, config); err != nil {
		return nativeRPCFailure("internal", "failed to persist selected agent preset: "+err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"agentPreset": req.AgentPreset})
}

func nativeAgentPresetIDValid(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for index, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (index > 0 && char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) nativeAgentPresetRead(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.nativeAgentPresetManager == nil {
		return nativeRPCFailure("not-supported", "agent preset authoring is not wired", nil)
	}
	var req struct {
		AgentPreset string `json:"agentPreset"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.AgentPreset = strings.TrimSpace(req.AgentPreset)
	if !nativeAgentPresetIDValid(req.AgentPreset) {
		return nativeRPCFailure("bad-request", "agentPreset must be a valid id", nil)
	}
	details, err := s.nativeAgentPresetManager.Read(r.Context(), req.AgentPreset)
	if err != nil {
		return nativeRPCFailure("agent-preset-read-failed", err.Error(), map[string]any{"agentPreset": req.AgentPreset})
	}
	value := map[string]any{"agentPreset": details.AgentPreset, "trust": details.Trust, "content": details.Content}
	if details.Name != "" {
		value["name"] = details.Name
	}
	if details.Description != "" {
		value["description"] = details.Description
	}
	return nativeRPCSuccess(value)
}

func (s *Server) nativeAgentPresetCopy(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.nativeAgentPresetManager == nil {
		return nativeRPCFailure("not-supported", "agent preset authoring is not wired", nil)
	}
	var req struct {
		From        string `json:"from"`
		AgentPreset string `json:"agentPreset"`
		Name        string `json:"name"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.From, req.AgentPreset, req.Name = strings.TrimSpace(req.From), strings.TrimSpace(req.AgentPreset), strings.TrimSpace(req.Name)
	if !nativeAgentPresetIDValid(req.From) || !nativeAgentPresetIDValid(req.AgentPreset) {
		return nativeRPCFailure("bad-request", "from and agentPreset must be valid ids", nil)
	}
	if len(req.Name) > 256 {
		return nativeRPCFailure("bad-request", "name is too long", nil)
	}
	id, err := s.nativeAgentPresetManager.Copy(r.Context(), req.From, req.AgentPreset, req.Name)
	if err != nil {
		return nativeRPCFailure("agent-preset-copy-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"agentPreset": id})
}

func (s *Server) nativeAgentPresetOpenDocument(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.nativeAgentPresetManager == nil {
		return nativeRPCFailure("not-supported", "agent preset authoring is not wired", nil)
	}
	var req struct {
		AgentPreset string `json:"agentPreset"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.AgentPreset = strings.TrimSpace(req.AgentPreset)
	if !nativeAgentPresetIDValid(req.AgentPreset) {
		return nativeRPCFailure("bad-request", "agentPreset must be a valid id", nil)
	}
	document, err := s.nativeAgentPresetManager.OpenDocument(r.Context(), req.AgentPreset)
	if err != nil {
		return nativeRPCFailure("agent-preset-open-failed", err.Error(), nil)
	}
	value := map[string]any{"opened": document.Opened}
	if !document.Opened {
		value["path"] = document.Path
	}
	return nativeRPCSuccess(value)
}

func (s *Server) nativeAgentPresetRemove(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.nativeAgentPresetManager == nil {
		return nativeRPCFailure("not-supported", "agent preset authoring is not wired", nil)
	}
	var req struct {
		AgentPreset string `json:"agentPreset"`
	}
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.AgentPreset = strings.TrimSpace(req.AgentPreset)
	if !nativeAgentPresetIDValid(req.AgentPreset) {
		return nativeRPCFailure("bad-request", "agentPreset must be a valid id", nil)
	}
	if err := s.nativeAgentPresetManager.Remove(r.Context(), req.AgentPreset); err != nil {
		return nativeRPCFailure("agent-preset-remove-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{})
}

func (s *Server) nativeSessionModels(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionIDRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
		return failure
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
		provider = nativeString(view["llm_provider"])
		if provider == "" {
			provider = nativeString(view["provider"])
		}
		model = nativeString(view["model"])
		effort = nativeString(view["reasoning_effort"])
		if effort == "" {
			effort = nativeString(view["reasoningEffort"])
		}
	}
	if configs, ok := s.store.(store.SessionConfigStore); ok {
		if config, configErr := configs.GetSessionConfig(r.Context(), req.SessionID); configErr == nil {
			if config.Provider != "" {
				provider = config.Provider
			}
			if config.Model != "" {
				model = config.Model
			}
			if config.ReasoningEffort != "" {
				effort = config.ReasoningEffort
			}
		}
	}
	current := map[string]any{"provider": provider, "model": model}
	if effort != "" {
		current["reasoningEffort"] = effort
	}
	routable := provider != "" && model != ""
	if routable && s.sessionModelValidateFn != nil {
		if err := s.sessionModelValidateFn(r.Context(), req.SessionID, provider, model, effort); err != nil {
			routable = false
			current["routeError"] = err.Error()
		}
	}
	value["current"] = current
	value["routable"] = routable
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
	if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
		return failure
	}
	if _, err := s.store.GetSessionMeta(r.Context(), req.SessionID); err != nil {
		return nativeStoreFailure(err)
	}
	if s.sessionModelValidateFn != nil {
		if err := s.sessionModelValidateFn(r.Context(), req.SessionID, req.Provider, req.Model, req.ReasoningEffort); err != nil {
			code := "model-select-failed"
			switch {
			case errors.Is(err, llm.ErrProviderUnavailable):
				code = "provider-unavailable"
			case errors.Is(err, llm.ErrCapabilityUnavailable):
				code = "model-unavailable"
			case strings.Contains(err.Error(), "reasoning effort"):
				code = "bad-request"
			}
			return nativeRPCFailure(code, err.Error(), map[string]any{
				"provider": req.Provider, "model": req.Model,
			})
		}
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
	if s.nativeDefaultModelFn != nil {
		// DSH treats a successful session selection as the default for the next
		// session as well. The session override above remains authoritative for
		// the current session and its future history.
		s.nativeDefaultModelFn(r.Context(), req.Provider, req.Model, req.ReasoningEffort)
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
		// The sidebar is a control-plane read and must not replay a complete
		// production conversation on every refresh. Keep the list baseline
		// bounded to the newest event window; session.history performs the full
		// projection when the conversation is opened, and subsequent mux frames
		// advance it. This is critical for 100k+ event sessions while a turn is
		// still appending to the same database.
		if events, _, loadErr := s.store.LoadSessionPage(r.Context(), m.ID, 0, 0, nativeSessionListTailLimit); loadErr == nil {
			tailCursor := newNativeProjectionCursor()
			tailCursor.setContextWindow(s.contextWindowForSession(m.ID))
			for _, ev := range events {
				tailCursor.project(m.ID, ev)
			}
			metadata := tailCursor.list
			item.Blank = metadata.Blank
			item.ParentSessionID, item.Origin, _ = nativeSessionLineage(events)
			if configs, ok := s.store.(store.SessionConfigStore); ok {
				if config, configErr := configs.GetSessionConfig(r.Context(), m.ID); configErr == nil {
					item.AgentPreset = config.AgentPreset
				}
			}
			item.UpdatedAt = m.UpdatedAt.UnixMilli()
			if metadata.LastPromptAt != nil && *metadata.LastPromptAt > item.UpdatedAt {
				item.UpdatedAt = *metadata.LastPromptAt
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
			// The bounded tail is sufficient for sidebar metadata, but it is not
			// sufficient to build a full state projection. Reuse an exact durable
			// checkpoint when available; on a miss, replay the complete raw log and
			// only then write a checkpoint. Never persist a tail-only fold at the
			// full session revision.
			if cached, ok := s.nativeCachedProjection(r.Context(), m.ID, uint64(maxInt64(lastSeq, 0))); ok {
				baseline := cached.Block
				if baseline.Values == nil {
					baseline.Values = make(map[string]any)
				}
				baseline.Values["title"] = m.Title
				if limits := s.nativeImageLimitsProjection(); limits != nil {
					baseline.Values["imageLimits"] = limits
				}
				if cachedMetadata, metadataOK := nativeSessionListMetadataFromValues(baseline.Values); metadataOK {
					metadata = cachedMetadata
					item.Blank = metadata.Blank
					if metadata.LastPromptAt != nil && *metadata.LastPromptAt > item.UpdatedAt {
						item.UpdatedAt = *metadata.LastPromptAt
					}
				}
				item.Projections = &baseline
			} else if fullEvents, fullErr := loadRawSession(r.Context(), s.store, m.ID); fullErr == nil {
				fullCursor := newNativeProjectionCursor()
				fullCursor.setContextWindow(s.contextWindowForSession(m.ID))
				for _, ev := range fullEvents {
					fullCursor.project(m.ID, ev)
				}
				metadata = fullCursor.list
				item.Blank = metadata.Blank
				if metadata.LastPromptAt != nil && *metadata.LastPromptAt > item.UpdatedAt {
					item.UpdatedAt = *metadata.LastPromptAt
				}
				item.ParentSessionID, item.Origin, _ = nativeSessionLineage(fullEvents)
				fullLastSeq := int64(-1)
				if len(fullEvents) > 0 {
					fullLastSeq = int64(fullEvents[len(fullEvents)-1].Seq)
				}
				baseline := fullCursor.projectionBlock(m.Title, fullLastSeq, permission)
				if limits := s.nativeImageLimitsProjection(); limits != nil {
					baseline.Values["imageLimits"] = limits
				}
				item.Projections = &baseline
				s.saveNativeProjection(r.Context(), m.ID, fullLastSeq, baseline, fullCursor.surfaceSnapshot())
			}
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
	events, err := loadNativeProjectedSession(r.Context(), s.store, req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	canonical, err := projection.Build(events)
	if err != nil {
		return nativeStoreFailure(fmt.Errorf("native history projection: %w", err))
	}
	meta, err := s.store.GetSessionMeta(r.Context(), req.SessionID)
	if err != nil {
		return nativeStoreFailure(err)
	}
	entries := make([]nativeHistoryEntry, 0, len(events))
	projection := newNativeProjectionCursor()
	projection.setContextWindow(s.contextWindowForSession(req.SessionID))
	projected := make([]nativeSessionEvent, 0, len(events))
	for _, ev := range events {
		projected = append(projected, projection.project(req.SessionID, ev))
	}
	start, end := nativeHistoryPageBounds(projected, req.BeforeSeq, limit)
	start, end = nativeHistoryTransportBounds(start, end)
	for _, event := range projected[start:end] {
		entries = append(entries, nativeHistoryEntry{Event: event})
	}
	title := meta.Title
	if strings.TrimSpace(title) == "" {
		title = canonical.Title
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
		baseline := projection.projectionBlock(title, lastSeq, permission)
		if limits := s.nativeImageLimitsProjection(); limits != nil {
			baseline.Values["imageLimits"] = limits
		}
		if cached, ok := s.nativeCachedProjection(r.Context(), req.SessionID, uint64(maxInt64(lastSeq, 0))); ok {
			baseline = cached.Block
			baseline.Values["title"] = title
			if cached.Surface != nil {
				value["surface"] = cached.Surface
			}
		}
		value["projections"] = baseline
		s.saveNativeProjection(r.Context(), req.SessionID, lastSeq, baseline, projection.surfaceSnapshot())
	}
	return nativeRPCSuccess(value)
}

func loadRawSession(ctx context.Context, st store.Store, sessionID string) ([]session.Event, error) {
	if raw, ok := st.(store.SessionRawStore); ok {
		return raw.LoadSessionRaw(ctx, sessionID)
	}
	return st.LoadSession(ctx, sessionID)
}

// loadNativeProjectedSession keeps the raw-store open-tail behavior needed by
// live history, while still enforcing the durable event vocabulary before a
// client-facing projection is derived.  Recovery/lifecycle validation remains
// owned by Store.LoadSession; this boundary only prevents an unknown persisted
// event from being relabeled as an ignorable wire extension.
func loadNativeProjectedSession(ctx context.Context, st store.Store, sessionID string) ([]session.Event, error) {
	events, err := loadRawSession(ctx, st, sessionID)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if err := session.ValidateDurableEvent(event.Type, event.Data); err != nil {
			return nil, fmt.Errorf("web: invalid durable event at seq %d: %w", event.Seq, err)
		}
	}
	return events, nil
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

// nativeHistoryEventLimit is a transport safety bound, independent of the
// message-count page size. A single streamed assistant message can contain
// tens of thousands of raw events, so honoring only maxMessages would still
// make the initial JSON payload and browser mount unbounded. Older events stay
// reachable through beforeSeq pagination; this bound may split one raw message
// span because raw event rows are individually renderable by the trajectory.
// Keep the first native history payload bounded enough for the DSH browser to
// parse and virtualize without a long task on large real sessions. The full
// projection still runs server-side and older raw events remain available via
// the beforeSeq cursor.
const nativeHistoryEventLimit = 2048

func nativeHistoryTransportBounds(start, end int) (int, int) {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end-start <= nativeHistoryEventLimit {
		return start, end
	}
	return end - nativeHistoryEventLimit, end
}

func nativeIsAppendSurfaceMessage(event nativeSessionEvent) bool {
	if event.Type != session.EventUserMessage && event.Type != session.EventAssistantMessage {
		return false
	}
	return event.SurfaceOp == "append"
}

// nativeSessionCreatePostFlightChecks preserves DSH's split between one shared
// creation operation and caller-specific assertions. A concurrent retry with a
// different cwd or explicit preset must conflict even though the leader's
// successful result was reused.
func nativeSessionCreatePostFlightChecks(
	created NativeSessionCreateResult,
	requestedPreset, desiredCWD string,
) (nativeRPCResult, bool) {
	if requestedPreset != "" && created.AgentPreset != requestedPreset {
		message := fmt.Sprintf(
			"session %q already runs agent preset %q; requested %q. A session's preset is fixed at creation.",
			created.SessionID, created.AgentPreset, requestedPreset)
		if created.AgentPreset == "" {
			message = fmt.Sprintf(
				"session %q records no agent preset, so it cannot be adopted under one; "+
					"a deployment composing no roster records none on any session — requested %q. "+
					"A session's preset is fixed at creation.", created.SessionID, requestedPreset)
		}
		details := map[string]any{
			"sessionId": created.SessionID, "requestedPreset": requestedPreset,
		}
		if created.AgentPreset != "" {
			details["existingPreset"] = created.AgentPreset
		}
		return nativeRPCFailure("agent-preset-conflict", message, details), true
	}
	if created.CWD != desiredCWD {
		return nativeRPCFailure("session-conflict",
			fmt.Sprintf("session %q already exists with cwd %q; requested %q",
				created.SessionID, created.CWD, desiredCWD),
			map[string]any{
				"sessionId": created.SessionID, "requestedCwd": desiredCWD, "existingCwd": created.CWD,
			}), true
	}
	return nativeRPCResult{}, false
}

// nativeSessionCreateDeduped applies DSH's per-identity create single-flight.
// The first named create request resolves fresh-versus-adoption and runs the
// creator; concurrent retries await and receive the same outcome instead of
// racing the durable uniqueness boundary.
func (s *Server) nativeSessionCreateDeduped(
	ctx context.Context,
	req *nativeSessionCreateRequest,
	requestedPreset, resolvedCWD string,
) (NativeSessionCreateResult, nativeRPCResult, bool) {
	return s.withNativeSessionCreateFlight(ctx, strings.TrimSpace(req.SessionID), func() (NativeSessionCreateResult, nativeRPCResult, bool) {
		existingIdentity := false
		if req.SessionID != "" {
			_, err := s.store.GetSessionMeta(ctx, req.SessionID)
			switch {
			case err == nil:
				existingIdentity = true
			case errors.Is(err, store.ErrNotFound):
			default:
				return NativeSessionCreateResult{}, nativeStoreFailure(err), true
			}
		}
		agentPreset := strings.TrimSpace(requestedPreset)
		if !existingIdentity {
			// Fresh identities compose the requested/default preset. Existing
			// identities are adopted under their already-running composition.
			if agentPreset == "" {
				agentPreset = s.nativeAgentPresetDefault(ctx)
			}
			if agentPreset != "" && !s.nativeAgentPresetAvailable(ctx, agentPreset) {
				return NativeSessionCreateResult{}, nativeRPCFailure("agent-preset-invalid",
					"agent preset is not available",
					map[string]any{"agentPreset": agentPreset, "reason": "preset is not available"}), true
			}
		}
		desiredCWD := resolvedCWD
		if req.WorkspaceID == "" {
			desiredCWD = req.CWD
		}
		if desiredCWD == "" {
			desiredCWD = s.defaultWorkdir
		}
		created, err := s.nativeSessionCreateFn(ctx, NativeSessionCreateSpec{
			SessionID:            req.SessionID,
			CWD:                  desiredCWD,
			WorkspaceID:          req.WorkspaceID,
			AgentPreset:          agentPreset,
			AgentPresetRequested: requestedPreset != "",
		})
		if err != nil {
			var createErr *NativeSessionCreateError
			if errors.As(err, &createErr) {
				return NativeSessionCreateResult{}, nativeRPCFailure(createErr.Code, createErr.Message, createErr.Details), true
			}
			return NativeSessionCreateResult{}, nativeRPCFailure("internal",
				fmt.Sprintf("failed to create session %q: %v", req.SessionID, err), nil), true
		}
		return created, nativeRPCResult{}, false
	})
}

func (s *Server) withNativeSessionCreateFlight(
	ctx context.Context,
	sessionID string,
	run func() (NativeSessionCreateResult, nativeRPCResult, bool),
) (NativeSessionCreateResult, nativeRPCResult, bool) {
	if sessionID == "" {
		return run()
	}
	s.nativeSessionCreateMu.Lock()
	if flight, ok := s.nativeSessionCreateFlights[sessionID]; ok {
		s.nativeSessionCreateMu.Unlock()
		select {
		case <-flight.done:
		case <-ctx.Done():
			return NativeSessionCreateResult{}, nativeRPCFailure("cancelled", "request context cancelled", nil), true
		}
		if flight.failed {
			return flight.result, flight.failure, true
		}
		return flight.result, nativeRPCResult{}, false
	}
	flight := &nativeSessionCreateFlight{done: make(chan struct{})}
	if s.nativeSessionCreateFlights == nil {
		s.nativeSessionCreateFlights = make(map[string]*nativeSessionCreateFlight)
	}
	s.nativeSessionCreateFlights[sessionID] = flight
	s.nativeSessionCreateMu.Unlock()

	flight.result, flight.failure, flight.failed = run()
	close(flight.done)
	s.nativeSessionCreateMu.Lock()
	delete(s.nativeSessionCreateFlights, sessionID)
	s.nativeSessionCreateMu.Unlock()
	if flight.failed {
		return flight.result, flight.failure, true
	}
	return flight.result, nativeRPCResult{}, false
}

func (s *Server) nativeSessionCreate(r *http.Request, raw json.RawMessage) nativeRPCResult {
	if s.sessFn == nil && s.nativeSessionCreateFn == nil {
		return nativeRPCFailure("not-supported", "session manager not wired", nil)
	}
	var req nativeSessionCreateRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if req.WorkspaceID != "" && req.CWD != "" {
		return nativeRPCFailure("bad-request", "session.create accepts workspaceId or cwd, not both", nil)
	}
	requestedPreset := strings.TrimSpace(req.AgentPreset)
	// DSH resolves the workspace before creating a session. Creating first
	// would turn an invalid sidebar selection into an orphan session even when
	// the attach step subsequently fails.
	resolvedCWD := ""
	if req.WorkspaceID != "" {
		cwd, err := s.workspaceWorkdir(r.Context(), req.WorkspaceID)
		if err != nil {
			if strings.Contains(err.Error(), "workspace not found") {
				return nativeWorkspaceNotFound(req.WorkspaceID)
			}
			return nativeRPCFailure("workspace-invalid-path", err.Error(), map[string]any{"workspaceId": req.WorkspaceID})
		}
		resolvedCWD = cwd
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	// DSH fences a named subagent before preset/CWD adoption checks.
	if req.SessionID != "" {
		if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
			return failure
		}
	}
	if s.nativeSessionCreateFn != nil {
		created, failure, handled := s.nativeSessionCreateDeduped(r.Context(), &req, requestedPreset, resolvedCWD)
		if handled {
			return failure
		}
		desiredCWD := resolvedCWD
		if req.WorkspaceID == "" {
			desiredCWD = req.CWD
		}
		if desiredCWD == "" {
			desiredCWD = s.defaultWorkdir
		}
		if failure, failed := nativeSessionCreatePostFlightChecks(created, requestedPreset, desiredCWD); failed {
			return failure
		}
		// The native mux is already connected while the user creates a session.
		// Publish the new address before returning so its live event
		// subscription is installed before the client can submit the first prompt.
		s.notifyNativeMuxSessionAdded(created.SessionID)
		value := map[string]any{"sessionId": created.SessionID}
		if created.AgentPreset != "" {
			value["agentPreset"] = created.AgentPreset
		}
		return nativeRPCSuccess(value)
	}
	existingIdentity := false
	if req.SessionID != "" {
		_, err := s.store.GetSessionMeta(r.Context(), req.SessionID)
		switch {
		case err == nil:
			existingIdentity = true
		case errors.Is(err, store.ErrNotFound):
		default:
			return nativeStoreFailure(err)
		}
	}
	req.AgentPreset = strings.TrimSpace(req.AgentPreset)
	if !existingIdentity {
		// Composition and roster checks belong to a fresh identity only. DSH
		// adopts an existing session under its already-running preset first;
		// an explicitly named but different preset is a conflict, not a new
		// preset admission request.
		if req.AgentPreset == "" {
			req.AgentPreset = s.nativeAgentPresetDefault(r.Context())
		}
		if req.AgentPreset != "" && !s.nativeAgentPresetAvailable(r.Context(), req.AgentPreset) {
			return nativeRPCFailure("agent-preset-invalid", "agent preset is not available", map[string]any{
				"agentPreset": req.AgentPreset, "reason": "preset is not available",
			})
		}
	}
	desiredCWD := resolvedCWD
	if req.WorkspaceID == "" {
		desiredCWD = req.CWD
	}
	if desiredCWD == "" {
		desiredCWD = s.defaultWorkdir
	}
	id, err := s.sessFn(r.Context(), "new", req.SessionID)
	if err != nil {
		return nativeRPCFailure("session-create-failed", err.Error(), nil)
	}
	if req.WorkspaceID != "" {
		if err := s.store.SetSessionWorkspace(r.Context(), id, req.WorkspaceID); err != nil {
			return nativeRPCFailure("workspace-attach-failed", err.Error(), map[string]any{"sessionId": id, "workspaceId": req.WorkspaceID})
		}
		if headers, ok := s.store.(store.SessionHeaderStore); ok {
			if err := headers.SetSessionCWD(r.Context(), id, resolvedCWD); err != nil {
				return nativeRPCFailure("workspace-attach-failed", err.Error(), map[string]any{"sessionId": id, "workspaceId": req.WorkspaceID})
			}
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
	// The native mux is already connected while the user creates a session.
	// Publish the new address before returning so its live event subscription is
	// installed before the client can submit the first prompt.
	s.notifyNativeMuxSessionAdded(id)
	value := map[string]any{"sessionId": id}
	if req.AgentPreset != "" {
		value["agentPreset"] = req.AgentPreset
	}
	return nativeRPCSuccess(value)
}

// nativeSessionFork clones a completed-turn prefix into a new session. DSH's
// fork anchor is a message sequence, not an arbitrary event cut: the first
// turn/end at or after the anchor closes the copied prefix. A missing anchor
// (or an anchor beyond the log) falls back to the latest completed turn, while
// an empty/in-progress log is not forkable.
func (s *Server) nativeSessionFork(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionForkRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	events, err := loadRawSession(r.Context(), s.store, req.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("session-not-found", "session not found", map[string]any{"sessionId": req.SessionID})
		}
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	end := nativeForkBoundary(events, req.AtSeq)
	if end < 0 {
		lastSeq := uint64(0)
		if len(events) != 0 {
			lastSeq = events[len(events)-1].Seq
		}
		if req.AtSeq != nil && *req.AtSeq <= lastSeq {
			return nativeRPCFailure("fork-unavailable",
				fmt.Sprintf("session %q has not completed the turn containing event %d", req.SessionID, *req.AtSeq),
				map[string]any{"sessionId": req.SessionID})
		}
		return nativeRPCFailure("fork-unavailable",
			fmt.Sprintf("session %q has no completed turn to fork from", req.SessionID),
			map[string]any{"sessionId": req.SessionID})
	}
	seedEnd := nativeForkSeedEnd(events, end)
	workspaceID, err := s.nativeForkWorkspaceID(r.Context(), req.SessionID)
	if err != nil {
		return nativeRPCFailure("internal",
			fmt.Sprintf("failed to resolve fork workspace for session %q: %v", req.SessionID, err), nil)
	}
	forkStore, ok := s.store.(store.SessionForkStore)
	if !ok {
		return nativeRPCFailure("not-supported", "atomic session fork is unavailable", map[string]any{"sessionId": req.SessionID})
	}
	forkID, err := store.GenerateReservedID(r.Context(), s.store, "session", newSessionID)
	if err != nil {
		return nativeRPCFailure("fork-failed", err.Error(), nil)
	}
	options := store.SessionForkOptions{InheritParentMetadata: true, WorkspaceID: workspaceID}
	if title, source := nativeForkSeedTitle(events[:seedEnd]); title != "" {
		options.Title = title
		options.TitleSource = source
	}
	// The store reads the parent header, sidebar projection, runtime config,
	// and closed seed under one SQLite transaction. Publishing those pieces via
	// separate Store calls would let a crash expose a runnable child whose
	// transcript and metadata describe different states.
	if err := forkStore.ForkSessionWithOptions(r.Context(), req.SessionID, forkID, events[seedEnd-1].Seq, options); err != nil {
		return nativeRPCFailure("fork-failed", err.Error(), nil)
	}
	// A fork is a new live session as well. Attach it to every resident native
	// mux before returning so the first follow-up prompt is not missed.
	s.notifyNativeMuxSessionAdded(forkID)
	return nativeRPCSuccess(map[string]any{"sessionId": forkID})
}

// nativeForkSeedEnd extends the completed-turn cut through trailing
// out-of-band events (title, approvals, injections, etc.) exactly as DSH does.
// These standalone records belong to the forked turn even though the lifecycle
// boundary itself has already closed.
func nativeForkSeedEnd(events []session.Event, boundaryIndex int) int {
	end := boundaryIndex + 1
	for end < len(events) && events[end].Type != session.EventTurnStart {
		end++
	}
	return end
}

// nativeForkSeedTitle folds the latest title event in the copied seed. DSH
// derives the child's title from its seed log rather than copying the parent's
// mutable latest-title metadata, so an older boundary keeps the title that
// existed when that turn completed.
func nativeForkSeedTitle(events []session.Event) (string, string) {
	title := ""
	source := session.TitleSourceFallback
	for _, event := range events {
		if event.Type != session.EventSessionTitle {
			continue
		}
		var data struct {
			Title  string `json:"title"`
			Source struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if json.Unmarshal(event.Data, &data) != nil || data.Title == "" {
			continue
		}
		switch data.Source.Kind {
		case "provider":
			source = session.TitleSourceLLM
		case "user":
			source = session.TitleSourceUser
		default:
			source = session.TitleSourceFallback
		}
		title = data.Title
	}
	return title, source
}

// nativeForkWorkspaceID resolves DSH's fork membership: a direct workspace for
// ordinary sources, or the nearest workspace-bearing ancestor for a subagent.
func (s *Server) nativeForkWorkspaceID(ctx context.Context, sessionID string) (string, error) {
	header := store.SessionHeader{ID: sessionID}
	if headers, ok := s.store.(store.SessionLineageStore); ok {
		resolved, err := headers.GetSessionHeader(ctx, sessionID)
		if err != nil {
			return "", err
		}
		header = resolved
	}
	sourceMeta, err := s.store.GetSessionMeta(ctx, header.ID)
	if err != nil {
		return "", err
	}
	// DSH first honors direct workspace membership, even for a subagent;
	// ancestor lookup is only the fallback for an unlisted child.
	if sourceMeta.WorkspaceID != "" {
		return sourceMeta.WorkspaceID, nil
	}
	if header.Origin == "subagent" {
		for header.Parent != "" {
			headers, ok := s.store.(store.SessionLineageStore)
			if !ok {
				break
			}
			resolved, err := headers.GetSessionHeader(ctx, header.Parent)
			if err != nil {
				return "", err
			}
			header = resolved
			if header.Origin != "subagent" {
				break
			}
		}
		if header.Origin == "subagent" {
			return "", nil
		}
	}
	if header.ID == sessionID {
		return sourceMeta.WorkspaceID, nil
	}
	meta, err := s.store.GetSessionMeta(ctx, header.ID)
	if err != nil {
		return "", err
	}
	return meta.WorkspaceID, nil
}

func nativeForkBoundary(events []session.Event, atSeq *uint64) int {
	lastCompleted := -1
	for index, event := range events {
		if event.Type == session.EventTurnEnd && !nativeInterruptedTurnEnd(event) {
			lastCompleted = index
			if atSeq != nil && event.Seq >= *atSeq {
				return index
			}
		}
	}
	return lastCompleted
}

func nativeInterruptedTurnEnd(event session.Event) bool {
	var data struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(event.Data, &data) != nil {
		return false
	}
	return data.Status == "interrupted" || data.Reason == "interrupted"
}

func (s *Server) nativeSessionUpdateQueue(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionUpdateQueueRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ItemID = strings.TrimSpace(req.ItemID)
	if req.SessionID == "" || req.ItemID == "" {
		return nativeRPCFailure("bad-request", "sessionId and itemId are required", nil)
	}
	if req.Action.Kind != "edit" && req.Action.Kind != "remove" && req.Action.Kind != "steer" {
		return nativeRPCFailure("bad-request", "action.kind must be edit, remove, or steer", nil)
	}
	text := ""
	if req.Action.Kind == "edit" {
		var builder strings.Builder
		if len(req.Action.Content) == 0 {
			return nativeRPCFailure("bad-request", "edit action requires text content", nil)
		}
		for _, part := range req.Action.Content {
			if part.Type != "text" {
				return nativeRPCFailure("attachment-error", "queue edits accept text content only", map[string]any{
					"reason": "QUEUE_EDIT_NON_TEXT",
				})
			}
			builder.WriteString(part.Text)
		}
		text = strings.TrimSpace(builder.String())
		if text == "" {
			return nativeRPCFailure("bad-request", "edit content must not be blank", nil)
		}
	}
	if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
		return failure
	}
	if s.nativeQueueUpdateFn != nil {
		if err := s.nativeQueueUpdateFn(r.Context(), req.SessionID, req.ItemID, req.Action.Kind, text); err != nil {
			return nativeQueueFailure(err, req.ItemID)
		}
		s.notifyNativeMux(req.SessionID)
		return nativeRPCSuccess(map[string]any{"accepted": true})
	}
	if s.queueUpdateFn == nil {
		return nativeRPCFailure("not-supported", "queue manager not wired", nil)
	}
	legacyAction := map[string]string{"remove": "delete", "steer": "steer"}[req.Action.Kind]
	if legacyAction == "" {
		return nativeRPCFailure("not-supported", "native queue edit handler not wired", nil)
	}
	if err := s.queueUpdateFn(r.Context(), req.SessionID, req.ItemID, legacyAction); err != nil {
		return nativeQueueFailure(err, req.ItemID)
	}
	s.notifyNativeMux(req.SessionID)
	return nativeRPCSuccess(map[string]any{"accepted": true})
}

func nativeQueueFailure(err error, itemID string) nativeRPCResult {
	switch {
	case errors.Is(err, ErrQueueItemNotFound):
		return nativeRPCFailure("queue-item-not-found", ErrQueueItemNotFound.Error(), map[string]any{"itemId": itemID})
	case errors.Is(err, ErrSteerUnavailable):
		return nativeRPCFailure("steer-unavailable", ErrSteerUnavailable.Error(), map[string]any{"itemId": itemID})
	default:
		return nativeRPCFailure("queue-update-failed", err.Error(), nil)
	}
}

// nativePromptRouteFailure applies DSH's prompt admission rule: an unserved
// provider must fail before durable prompt/attachment admission instead of
// turning into a late model-loop error.
func (s *Server) nativePromptRouteFailure(ctx context.Context, sessionID string) (nativeRPCResult, bool) {
	if s.cfgFn == nil {
		return nativeRPCResult{}, false
	}
	view := s.cfgFn()
	provider := nativeString(view["llm_provider"])
	if provider == "" {
		provider = nativeString(view["provider"])
	}
	model := nativeString(view["model"])
	effort := nativeString(view["reasoning_effort"])
	if effort == "" {
		effort = nativeString(view["reasoningEffort"])
	}
	if configs, ok := s.store.(store.SessionConfigStore); ok {
		if config, err := configs.GetSessionConfig(ctx, sessionID); err == nil {
			if config.Provider != "" {
				provider = config.Provider
			}
			if config.Model != "" {
				model = config.Model
			}
			if config.ReasoningEffort != "" {
				effort = config.ReasoningEffort
			}
		}
	}
	providers := nativeConfigProviderMaps(view["providers"])
	served := false
	if len(providers) == 0 {
		// DSH permits prompts when the host composes no LLM registry; the
		// legacy embedded runtime has the same absence-of-registry meaning.
		return nativeRPCResult{}, false
	}
	for _, candidate := range providers {
		if nativeString(candidate["id"]) == provider &&
			nativeBool(candidate["configured"]) &&
			nativeBool(candidate["available"]) {
			served = true
			break
		}
	}
	if served {
		return nativeRPCResult{}, false
	}
	return nativeRPCFailure("model-unavailable",
		fmt.Sprintf("no adapter serves provider %q; select a model for this session", provider),
		map[string]any{"provider": provider, "model": model}), true
}

func (s *Server) nativeSessionPrompt(r *http.Request, raw json.RawMessage) nativeRPCResult {
	var req nativeSessionPromptRequest
	if failure := nativeDecode(raw, &req); !failure.OK && failure.Error != nil {
		return failure
	}
	if req.Mode != "queue" && req.Mode != "steer" {
		return nativeRPCFailure("bad-request", "mode must be queue or steer", nil)
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	// DSH validates the browser's canonical time-zone request before resolving
	// the agent route or opening any admission side effects.
	if strings.TrimSpace(req.ClientTimeZone) != "" {
		if _, err := loadCanonicalPromptTimeZone(req.ClientTimeZone); err != nil {
			return nativePromptTimeZoneFailure(req.ClientTimeZone)
		}
	}
	if failure, ok := s.nativeSubagentOwnershipGate(r.Context(), req.SessionID); !ok {
		return failure
	}
	if _, err := s.store.GetSessionMeta(r.Context(), req.SessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("session-not-found", "session not found", map[string]any{"sessionId": req.SessionID})
		}
		return nativeStoreFailure(err)
	}
	if failure, blocked := s.nativePromptRouteFailure(r.Context(), req.SessionID); blocked {
		return failure
	}
	// DSH checks the selected model's image modality before decoding or
	// persisting image content. This preserves that failure precedence and
	// keeps an unsupported-model prompt free of Base64 admission side effects.
	hasImage := false
	for _, part := range req.Content {
		if part.Type == "image" {
			hasImage = true
			break
		}
	}
	if hasImage && s.nativeImageCapabilityFn != nil && !s.nativeImageCapabilityFn(r.Context(), req.SessionID) {
		return nativeImageCapabilityFailure()
	}
	var text string
	imageInputs := make([]attachment.ImageInput, 0)
	queueContent := make([]llm.ContentBlock, 0, len(req.Content))
	queueImageIndexes := make([]int, 0)
	for _, part := range req.Content {
		switch part.Type {
		case "text":
			text += part.Text
			queueContent = append(queueContent, llm.Text(part.Text))
		case "image":
			if s.att == nil {
				return nativeRPCFailure("not-supported", "native image prompt is not wired", nil)
			}
			encoded := strings.TrimSpace(part.Data)
			if strings.HasPrefix(encoded, "data:") {
				separator := strings.IndexByte(encoded, ',')
				if separator < 0 {
					return nativeRPCFailure("bad-request", "image data URL is invalid", nil)
				}
				encoded = encoded[separator+1:]
			}
			data, err := nativeDecodeImageBase64(encoded)
			if err != nil {
				return nativeImageBase64Failure(err)
			}
			imageInputs = append(imageInputs, attachment.ImageInput{
				MediaType: strings.TrimSpace(part.MediaType),
				Data:      data,
				Name:      strings.TrimSpace(part.Name),
			})
			queueImageIndexes = append(queueImageIndexes, len(queueContent))
			queueContent = append(queueContent, llm.ContentBlock{Kind: llm.BlockImage})
		default:
			return nativeRPCFailure("bad-request", "unsupported prompt content type", nil)
		}
	}
	if strings.TrimSpace(text) == "" && len(imageInputs) == 0 {
		return nativeRPCFailure("bad-request", "text or image content is required", nil)
	}
	// Rich prompt admission is one batch boundary: validate every image before
	// publishing the first object. This matches ACP/MCP and prevents a late
	// malformed image from leaving earlier attachments behind.
	images := make([]llm.ImageRef, 0, len(imageInputs))
	if len(imageInputs) > 0 {
		refs, err := s.att.SaveImages(imageInputs, maxWebImageBytes)
		if err != nil {
			return nativeAttachmentFailure(err)
		}
		images = refs
	}
	for index, ref := range images {
		queueContent[queueImageIndexes[index]].Image = ref
	}
	meta := PromptMeta{RPCID: nativeRPCID(), ClientTimeZone: strings.TrimSpace(req.ClientTimeZone)}
	if meta.ClientTimeZone != "" {
		if _, err := loadCanonicalPromptTimeZone(meta.ClientTimeZone); err != nil {
			return nativePromptTimeZoneFailure(meta.ClientTimeZone)
		}
	}
	// DSH queues complete pending messages, including image blocks. Admission
	// has already persisted the attachment batch, so only durable refs cross
	// the queue boundary.
	if req.Mode == "queue" && s.queueEnqueueFn != nil {
		if _, err := s.queueEnqueueFn(r.Context(), req.SessionID, text, queueContent, meta); err != nil {
			return nativeRPCFailure("prompt-failed", err.Error(), nil)
		}
		s.notifyNativeMux(req.SessionID)
		return nativeRPCSuccess(map[string]any{"accepted": true})
	}
	if s.msgFn == nil {
		return nativeRPCFailure("not-supported", "message handler not wired", nil)
	}
	if req.Mode == "steer" && s.stopFn != nil {
		if err := s.stopFn(req.SessionID); err != nil && !strings.Contains(err.Error(), "no turn running") {
			return nativeRPCFailure("prompt-failed", err.Error(), nil)
		}
	}
	// DSH treats session.prompt as an admission RPC: the response confirms that
	// the host accepted the turn, while the turn lifecycle and its chunks arrive
	// on the mux stream. Do not hold the RPC open for the model/tool loop; doing
	// so serializes follow-up native calls (including session.list) behind a long
	// task and makes the UI appear stalled. The request context is detached after
	// admission because a browser navigation or fetch timeout must not cancel an
	// accepted turn. webMessage owns the process lifetime and explicit stop path.
	turnCtx := context.WithoutCancel(r.Context())
	go func() {
		if err := s.msgFn(turnCtx, req.SessionID, text, images, meta); err != nil {
			// Errors are persisted/published by the composition-root message
			// handler. Keep a transport log as a last-resort diagnostic for
			// handlers that fail before they can append a turn/end event.
			log.Printf("native session.prompt %s: %v", req.SessionID, err)
		}
	}()
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
	items, archived := nativeWorkspaceViews(workspaces, metas)
	return nativeRPCSuccess(nativeWorkspaceListValue{Items: items, ArchivedSessionIDs: archived})
}

func nativeWorkspaceViews(workspaces []store.WorkspaceMeta, metas []store.SessionMeta) ([]nativeWorkspaceView, []string) {
	byWorkspace := make(map[string][]store.SessionMeta)
	archived := make([]string, 0)
	for _, meta := range metas {
		if !meta.ArchivedAt.IsZero() {
			archived = append(archived, meta.ID)
		}
		if meta.WorkspaceID != "" {
			byWorkspace[meta.WorkspaceID] = append(byWorkspace[meta.WorkspaceID], meta)
		}
	}
	items := make([]nativeWorkspaceView, 0, len(workspaces))
	for _, workspace := range workspaces {
		members := append([]store.SessionMeta(nil), byWorkspace[workspace.ID]...)
		sort.SliceStable(members, func(left, right int) bool {
			if members[left].Sort != members[right].Sort {
				return members[left].Sort < members[right].Sort
			}
			if !members[left].UpdatedAt.Equal(members[right].UpdatedAt) {
				return members[left].UpdatedAt.After(members[right].UpdatedAt)
			}
			return members[left].ID < members[right].ID
		})
		ids := make([]string, 0, len(members))
		for _, member := range members {
			ids = append(ids, member.ID)
		}
		createdAt := nativeWorkspaceTime(workspace.CreatedAt)
		items = append(items, nativeWorkspaceView{
			WorkspaceID: workspace.ID, Path: workspace.Path, Title: workspace.Title,
			SessionIDs: ids, CreatedAt: createdAt, UpdatedAt: createdAt,
		})
	}
	return items, archived
}

func nativeWorkspaceNotFound(workspaceID string) nativeRPCResult {
	return nativeRPCFailure("workspace-not-found", fmt.Sprintf("workspace %q not found", workspaceID), map[string]any{"workspaceId": workspaceID})
}

func nativeWorkspaceTime(value time.Time) string {
	if value.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func nativeWorkspaceFind(items []nativeWorkspaceView, id string) (nativeWorkspaceView, bool) {
	for _, item := range items {
		if item.WorkspaceID == id {
			return item, true
		}
	}
	return nativeWorkspaceView{}, false
}

func nativeWorkspaceCanonicalPath(rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", errors.New("workspace path is required")
	}
	abs, err := pathsecure.ResolveExisting(path)
	if err != nil {
		return "", fmt.Errorf("workspace path %q is unavailable: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace path %q is unavailable: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

func (s *Server) nativeWorkspaceCreate(r *http.Request, rawPath string) nativeRPCResult {
	s.workspaceMutationMu.Lock()
	defer s.workspaceMutationMu.Unlock()
	path, err := nativeWorkspaceCanonicalPath(rawPath)
	if err != nil {
		return nativeRPCFailure("workspace-invalid-path", err.Error(), map[string]any{"path": strings.TrimSpace(rawPath)})
	}
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	items, _ := nativeWorkspaceViews(workspaces, metas)
	for _, workspace := range items {
		if filepath.Clean(workspace.Path) == path {
			return nativeRPCSuccess(map[string]any{"workspace": workspace, "created": false})
		}
	}
	id, err := newWorkspaceID()
	if err != nil {
		return nativeRPCFailure("workspace-create-failed", err.Error(), nil)
	}
	if pathStore, ok := s.store.(store.WorkspacePathStore); ok {
		err = pathStore.CreateWorkspaceWithPath(r.Context(), id, filepath.Base(path), path)
	} else {
		err = s.store.CreateWorkspace(r.Context(), id, filepath.Base(path))
	}
	if err != nil {
		return nativeRPCFailure("workspace-create-failed", err.Error(), nil)
	}
	now := nativeWorkspaceTime(time.Now())
	created := nativeWorkspaceView{
		WorkspaceID: id, Path: path, Title: filepath.Base(path), SessionIDs: []string{},
		CreatedAt: now, UpdatedAt: now,
	}
	return nativeRPCSuccess(map[string]any{"workspace": created, "created": true})
}

func (s *Server) nativeWorkspaceRename(r *http.Request, id, rawTitle string) nativeRPCResult {
	s.workspaceMutationMu.Lock()
	defer s.workspaceMutationMu.Unlock()
	id = strings.TrimSpace(id)
	title := strings.TrimSpace(boundRunes(rawTitle, maxWorkspaceTitle))
	if id == "" || title == "" {
		return nativeRPCFailure("bad-request", "workspaceId and a non-blank title are required", nil)
	}
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	items, _ := nativeWorkspaceViews(workspaces, metas)
	current, found := nativeWorkspaceFind(items, id)
	if !found {
		return nativeWorkspaceNotFound(id)
	}
	for _, workspace := range items {
		if workspace.WorkspaceID != id && workspace.Title == title {
			return nativeRPCFailure("workspace-name-conflict", fmt.Sprintf("workspace name %q is already in use", title), map[string]any{"name": title})
		}
	}
	if current.Title == title {
		return nativeRPCSuccess(map[string]any{"workspace": current})
	}
	if err := s.store.SetWorkspaceTitle(r.Context(), id, title); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeWorkspaceNotFound(id)
		}
		return nativeRPCFailure("workspace-rename-failed", err.Error(), nil)
	}
	current.Title = title
	return nativeRPCSuccess(map[string]any{"workspace": current})
}

func (s *Server) nativeWorkspaceDelete(r *http.Request, id string) nativeRPCResult {
	s.workspaceMutationMu.Lock()
	defer s.workspaceMutationMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return nativeRPCFailure("bad-request", "workspaceId is required", nil)
	}
	if err := s.store.DeleteWorkspace(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeWorkspaceNotFound(id)
		}
		return nativeRPCFailure("workspace-delete-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"deleted": true})
}

func (s *Server) nativeWorkspaceInsertBefore(r *http.Request, id, beforeID string) nativeRPCResult {
	id = strings.TrimSpace(id)
	beforeID = strings.TrimSpace(beforeID)
	if id == "" {
		return nativeRPCFailure("bad-request", "workspaceId is required", nil)
	}
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	order := make([]string, 0, len(workspaces))
	known := make(map[string]bool, len(workspaces))
	for _, workspace := range workspaces {
		order = append(order, workspace.ID)
		known[workspace.ID] = true
	}
	if !known[id] || (beforeID != "" && !known[beforeID]) {
		missing := id
		if known[id] {
			missing = beforeID
		}
		return nativeWorkspaceNotFound(missing)
	}
	if id == beforeID {
		return nativeRPCSuccess(map[string]any{"workspaceIds": order})
	}
	without := make([]string, 0, len(order)-1)
	for _, workspaceID := range order {
		if workspaceID != id {
			without = append(without, workspaceID)
		}
	}
	index := len(without)
	if beforeID != "" {
		for position, workspaceID := range without {
			if workspaceID == beforeID {
				index = position
				break
			}
		}
	}
	order = append(without[:index:index], append([]string{id}, without[index:]...)...)
	if err := s.store.ReorderWorkspaces(r.Context(), order); err != nil {
		return nativeRPCFailure("workspace-order-failed", err.Error(), nil)
	}
	return nativeRPCSuccess(map[string]any{"workspaceIds": order})
}

func (s *Server) nativeWorkspaceInsertSessionBefore(r *http.Request, workspaceID, sessionID, beforeID string) nativeRPCResult {
	workspaceID, sessionID, beforeID = strings.TrimSpace(workspaceID), strings.TrimSpace(sessionID), strings.TrimSpace(beforeID)
	if workspaceID == "" || sessionID == "" {
		return nativeRPCFailure("bad-request", "workspaceId and sessionId are required", nil)
	}
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	items, _ := nativeWorkspaceViews(workspaces, metas)
	workspace, found := nativeWorkspaceFind(items, workspaceID)
	if !found {
		return nativeWorkspaceNotFound(workspaceID)
	}
	order := append([]string(nil), workspace.SessionIDs...)
	contains := func(id string) bool {
		for _, member := range order {
			if member == id {
				return true
			}
		}
		return false
	}
	if !contains(sessionID) || (beforeID != "" && !contains(beforeID)) {
		moveDetails := map[string]any{"workspaceId": workspaceID, "sessionId": sessionID}
		if beforeID != "" {
			moveDetails["beforeSessionId"] = beforeID
		}
		return nativeRPCFailure("workspace-move-invalid", "session or anchor is not accounted by this workspace", moveDetails)
	}
	if sessionID == beforeID {
		return nativeRPCSuccess(map[string]any{"workspace": workspace})
	}
	without := make([]string, 0, len(order)-1)
	for _, member := range order {
		if member != sessionID {
			without = append(without, member)
		}
	}
	index := len(without)
	if beforeID != "" {
		for position, member := range without {
			if member == beforeID {
				index = position
				break
			}
		}
	}
	order = append(without[:index:index], append([]string{sessionID}, without[index:]...)...)
	if err := s.store.ReorderSessions(r.Context(), workspaceID, order); err != nil {
		moveDetails := map[string]any{"workspaceId": workspaceID, "sessionId": sessionID}
		if beforeID != "" {
			moveDetails["beforeSessionId"] = beforeID
		}
		return nativeRPCFailure("workspace-move-invalid", err.Error(), moveDetails)
	}
	workspace.SessionIDs = order
	return nativeRPCSuccess(map[string]any{"workspace": workspace})
}

func (s *Server) nativeWorkspaceArchiveSession(r *http.Request, sessionID string) nativeRPCResult {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nativeRPCFailure("bad-request", "sessionId is required", nil)
	}
	if err := s.store.ArchiveSession(r.Context(), sessionID, true); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nativeRPCFailure("session-not-found", "session not found", map[string]any{"sessionId": sessionID})
		}
		return nativeRPCFailure("archive-failed", err.Error(), nil)
	}
	metas, err := s.store.ListSessions(r.Context())
	if err != nil {
		return nativeRPCFailure("internal", err.Error(), nil)
	}
	archived := make([]string, 0)
	for _, meta := range metas {
		if !meta.ArchivedAt.IsZero() {
			archived = append(archived, meta.ID)
		}
	}
	return nativeRPCSuccess(map[string]any{"archivedSessionIds": archived})
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
	if !s.trackNativeConnection(conn) {
		return
	}
	defer s.untrackNativeConnection(conn)
	ctx, cancel := contextWithConnection(r)
	defer cancel()
	defer conn.Close()
	var writes sync.Mutex
	writeWithID := func(method string, payload any, rpcID string) error {
		body, err := json.Marshal(nativeEventEnvelope{
			Type: nativeRPCTypeServerRequest, RPCID: rpcID, Method: method, Payload: payload,
		})
		if err != nil {
			return err
		}
		writes.Lock()
		defer writes.Unlock()
		return writeNativeWebSocketText(conn, body)
	}
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
		return writeWithID(method, payload, nativeRPCID())
	}
	writeRemoteEvent := func(event string, args ...any) error {
		body, err := json.Marshal(nativeRemoteEventFrame{Type: "host/remote-event", Event: event, Args: args})
		if err != nil {
			return err
		}
		writes.Lock()
		defer writes.Unlock()
		return writeNativeWebSocketText(conn, body)
	}
	settingsUnsub := s.subscribeNativeSettingsDocumentUpdated(func(namespace string, revision int) {
		_ = writeRemoteEvent("settings/document-updated", namespace, revision)
	})
	defer settingsUnsub()
	credentialUnsub := s.subscribeNativeCredentialUpdated(func(ref string) {
		_ = writeRemoteEvent("credentials/updated", ref)
	})
	defer credentialUnsub()
	llmAdapterUnsub := s.subscribeNativeLLMAdaptersUpdated(func() {
		_ = writeRemoteEvent("llm/adapters-updated")
	})
	defer llmAdapterUnsub()
	interactionKinds := make(map[string]string)
	var interactionKindsMu sync.Mutex
	emitInteraction := func(sessionID string, ev session.Event) {
		method, payload, id, kind, ok := nativeInteractionFrame(sessionID, ev)
		if !ok {
			return
		}
		interactionKindsMu.Lock()
		knownKind := interactionKinds[id]
		interactionKindsMu.Unlock()
		if kind == "" {
			kind = knownKind
		}
		if (ev.Type == session.EventInteractResolve || ev.Type == session.EventApprovalDecided) && kind == "question" {
			outcome := "answered"
			if ev.Type == session.EventApprovalDecided {
				var fact struct {
					Outcome string `json:"outcome"`
				}
				_ = json.Unmarshal(ev.Data, &fact)
				if fact.Outcome == string(interact.StatusCanceled) {
					outcome = "cancelled"
				}
			}
			method = "question/resolved"
			payload = map[string]any{
				"type": "question/resolved", "sessionId": sessionID,
				"questionRpcId": id, "outcome": outcome,
			}
		}
		if ev.Type == session.EventInteractCancel && kind == "question" {
			method = "question/resolved"
			payload = map[string]any{
				"type": "question/resolved", "sessionId": sessionID,
				"questionRpcId": id, "outcome": "cancelled",
			}
		}
		if kind != "" {
			interactionKindsMu.Lock()
			interactionKinds[id] = kind
			interactionKindsMu.Unlock()
			s.rememberNativeInteraction(id, sessionID)
		}
		if method == "" {
			return
		}
		_ = writeWithID(method, payload, id)
		if ev.Type == session.EventInteractResolve || ev.Type == session.EventInteractCancel || ev.Type == session.EventApprovalDecided {
			interactionKindsMu.Lock()
			delete(interactionKinds, id)
			interactionKindsMu.Unlock()
			s.forgetNativeInteraction(id)
		}
	}
	// Queue snapshots are refreshed by notifyNativeMux after the queue RPC
	// mutates the queue. Job snapshots are refreshed by the durable job/*
	// lifecycle events below. Do not resend either control-plane snapshot for
	// every assistant chunk: a dense stream can otherwise turn one text delta
	// into several redundant WebSocket frames and React state updates.
	eventNeedsControlSnapshots := func(ev session.Event) bool {
		switch ev.Type {
		case session.EventJobStart, session.EventJobStatus, session.EventJobDone:
			return true
		default:
			return false
		}
	}
	sessionMu := sync.Mutex{}
	subscribed := make(map[string]struct{})
	unsubs := make(map[string]func())
	snapshotUnsubs := make(map[string]func())
	var subscribeSession func(store.SessionMeta) error
	subscribeSession = func(meta store.SessionMeta) error {
		if !meta.ArchivedAt.IsZero() {
			return nil
		}
		sessionMu.Lock()
		if _, exists := subscribed[meta.ID]; exists {
			sessionMu.Unlock()
			return nil
		}
		subscribed[meta.ID] = struct{}{}
		sessionMu.Unlock()
		forgetSubscription := func() {
			sessionMu.Lock()
			delete(subscribed, meta.ID)
			sessionMu.Unlock()
		}
		events, loadErr := s.store.LoadSession(ctx, meta.ID)
		if loadErr != nil {
			forgetSubscription()
			return loadErr
		}
		projection := newNativeProjectionCursor()
		projection.setContextWindow(s.contextWindowForSession(meta.ID))
		var projectionMu sync.Mutex
		lastSeq := int64(-1)
		if len(events) > 0 {
			lastSeq = int64(events[len(events)-1].Seq)
		}
		for _, ev := range events {
			projection.project(meta.ID, ev)
		}
		if lastSeq >= 0 {
			permission := ""
			if configs, ok := s.store.(store.SessionConfigStore); ok {
				if config, configErr := configs.GetSessionConfig(ctx, meta.ID); configErr == nil {
					permission = config.Permission
				}
			}
			checkpoint := projection.projectionBlock(meta.Title, lastSeq, permission)
			if limits := s.nativeImageLimitsProjection(); limits != nil {
				checkpoint.Values["imageLimits"] = limits
			}
			s.saveNativeProjection(ctx, meta.ID, lastSeq, checkpoint, projection.surfaceSnapshot())
		}
		if err := write(nativeSubscribedFrame{Type: "session/subscribed", SessionID: meta.ID, LastSeq: lastSeq}); err != nil {
			forgetSubscription()
			return err
		}
		if s.interactionFn != nil {
			if pending, listErr := s.interactionFn(ctx, meta.ID); listErr == nil {
				for _, item := range pending {
					if item.Status != interact.StatusPending {
						continue
					}
					method, payload, id, kind, ok := nativePendingInteractionFrame(meta.ID, item)
					if !ok {
						continue
					}
					interactionKindsMu.Lock()
					interactionKinds[id] = kind
					interactionKindsMu.Unlock()
					s.rememberNativeInteraction(id, meta.ID)
					_ = writeWithID(method, payload, id)
				}
			}
		}
		if s.queueListFn != nil {
			if items, listErr := s.queueListFn(ctx, meta.ID); listErr == nil {
				_ = writeWithID("session/queue", nativeQueueFrame(meta.ID, items), nativeRPCID())
			}
		}
		if s.jobsFn != nil {
			if jobs, listErr := s.jobsFn(ctx, meta.ID); listErr == nil && len(jobs) > 0 {
				_ = writeWithID("session/jobs", map[string]any{
					"type": "session/jobs", "sessionId": meta.ID, "jobs": nativeJobViews(jobs),
				}, nativeRPCID())
			}
		}
		emitSnapshots := func() {
			if s.queueListFn != nil {
				if items, listErr := s.queueListFn(ctx, meta.ID); listErr == nil {
					_ = writeWithID("session/queue", nativeQueueFrame(meta.ID, items), nativeRPCID())
				}
			}
			if s.jobsFn != nil {
				if jobs, listErr := s.jobsFn(ctx, meta.ID); listErr == nil {
					_ = writeWithID("session/jobs", map[string]any{
						"type": "session/jobs", "sessionId": meta.ID, "jobs": nativeJobViews(jobs),
					}, nativeRPCID())
				}
			}
		}
		removeSnapshotSubscription := s.subscribeNativeMux(meta.ID, emitSnapshots)
		eventUnsub := s.evSrc(meta.ID, func(ev session.Event) {
			projectionMu.Lock()
			projected := projection.project(meta.ID, ev)
			changes := projection.projectionChanges()
			checkpoint := nativeProjectionBlock{}
			checkpointSurface := map[string]any(nil)
			if nativeProjectionCheckpointEvent(ev.Type) {
				permission := ""
				if configs, ok := s.store.(store.SessionConfigStore); ok {
					if config, configErr := configs.GetSessionConfig(ctx, meta.ID); configErr == nil {
						permission = config.Permission
					}
				}
				checkpoint = projection.projectionBlock(meta.Title, int64(ev.Seq), permission)
				if limits := s.nativeImageLimitsProjection(); limits != nil {
					checkpoint.Values["imageLimits"] = limits
				}
				checkpointSurface = projection.surfaceSnapshot()
			}
			projectionMu.Unlock()
			if nativeProjectionCheckpointEvent(ev.Type) {
				s.saveNativeProjection(ctx, meta.ID, int64(ev.Seq), checkpoint, checkpointSurface)
			}
			_ = write(nativeSessionEventFrame{Type: "session/event", SessionID: meta.ID, Event: projected})
			for key, value := range changes {
				_ = write(nativeProjectionFrame{Type: "session/projection", SessionID: meta.ID, Key: key, Value: value, Seq: ev.Seq})
			}
			emitInteraction(meta.ID, ev)
			if eventNeedsControlSnapshots(ev) {
				emitSnapshots()
			}
		})
		sessionMu.Lock()
		snapshotUnsubs[meta.ID] = removeSnapshotSubscription
		unsubs[meta.ID] = eventUnsub
		sessionMu.Unlock()
		return nil
	}
	addedUnsub := s.subscribeNativeMuxSessionAdded(func(sessionID string) {
		meta, metaErr := s.store.GetSessionMeta(ctx, sessionID)
		if metaErr != nil {
			return
		}
		_ = subscribeSession(meta)
	})
	defer addedUnsub()
	metas, err := s.store.ListSessions(ctx)
	if err != nil {
		return
	}
	defer func() {
		sessionMu.Lock()
		eventUnsubs := make([]func(), 0, len(unsubs))
		for _, unsub := range unsubs {
			if unsub != nil {
				eventUnsubs = append(eventUnsubs, unsub)
			}
		}
		snapshotRemovals := make([]func(), 0, len(snapshotUnsubs))
		for _, remove := range snapshotUnsubs {
			snapshotRemovals = append(snapshotRemovals, remove)
		}
		sessionMu.Unlock()
		for _, unsub := range eventUnsubs {
			unsub()
		}
		for _, remove := range snapshotRemovals {
			remove()
		}
	}()
	for _, meta := range metas {
		if err := subscribeSession(meta); err != nil {
			return
		}
	}
	go drainNativeWebSocket(reader, cancel)
	<-ctx.Done()
}

func (s *Server) handleNativeHostWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, reader, err := upgradeNativeWebSocket(w, r)
	if err != nil {
		return
	}
	if !s.trackNativeConnection(conn) {
		return
	}
	defer s.untrackNativeConnection(conn)
	ctx, cancel := contextWithConnection(r)
	defer cancel()
	defer conn.Close()
	var writes sync.Mutex
	write := func(method string, payload any) error {
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
	writeRemoteEvent := func(event string, args ...any) error {
		body, err := json.Marshal(nativeRemoteEventFrame{Type: "host/remote-event", Event: event, Args: args})
		if err != nil {
			return err
		}
		writes.Lock()
		defer writes.Unlock()
		return writeNativeWebSocketText(conn, body)
	}
	settingsUnsub := s.subscribeNativeSettingsDocumentUpdated(func(namespace string, revision int) {
		_ = writeRemoteEvent("settings/document-updated", namespace, revision)
	})
	defer settingsUnsub()
	credentialUnsub := s.subscribeNativeCredentialUpdated(func(ref string) {
		_ = writeRemoteEvent("credentials/updated", ref)
	})
	defer credentialUnsub()
	llmAdapterUnsub := s.subscribeNativeLLMAdaptersUpdated(func() {
		_ = writeRemoteEvent("llm/adapters-updated")
	})
	defer llmAdapterUnsub()
	metas, err := s.store.ListSessions(ctx)
	if err != nil {
		return
	}
	active := make(map[string]store.SessionMeta)
	unsubs := make(map[string]func())
	subscribe := func(meta store.SessionMeta, announce bool) error {
		events, loadErr := s.store.LoadSession(ctx, meta.ID)
		if loadErr != nil {
			return loadErr
		}
		if announce {
			if err := write("host/session-added", s.nativeHostSessionAdded(meta, events)); err != nil {
				return err
			}
			if s.statusFn != nil {
				status := s.statusFn(ctx, meta)
				if err := write("host/session-status", map[string]any{
					"type": "host/session-status", "sessionId": meta.ID, "running": status.State == "ongoing",
				}); err != nil {
					return err
				}
			}
		}
		active[meta.ID] = meta
		if s.evSrc != nil {
			sessionID := meta.ID
			unsub := s.evSrc(sessionID, func(ev session.Event) {
				running, ok := nativeHostRunningTransition(ev.Type)
				if ok {
					_ = write("host/session-status", map[string]any{
						"type": "host/session-status", "sessionId": sessionID, "running": running,
					})
				}
				if message := nativeHostAgentError(ev); message != "" {
					_ = write("host/agent-error", map[string]any{
						"type": "host/agent-error", "sessionId": sessionID, "message": message,
					})
				}
			})
			if unsub != nil {
				unsubs[meta.ID] = unsub
			}
		}
		return nil
	}
	for _, meta := range metas {
		if meta.ArchivedAt.IsZero() {
			if err := subscribe(meta, true); err != nil {
				return
			}
		}
	}
	workspaces, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return
	}
	workspaceViews, archived := nativeWorkspaceViews(workspaces, metas)
	for _, workspace := range workspaceViews {
		if err := write("host/workspace-changed", map[string]any{
			"type": "host/workspace-changed", "workspace": workspace,
		}); err != nil {
			return
		}
	}
	if err := write("host/archived-sessions-changed", map[string]any{
		"type": "host/archived-sessions-changed", "archivedSessionIds": archived,
	}); err != nil {
		return
	}
	previousWorkspaces := make(map[string]nativeWorkspaceView, len(workspaceViews))
	previousWorkspaceOrder := make([]string, 0, len(workspaceViews))
	for _, workspace := range workspaceViews {
		previousWorkspaces[workspace.WorkspaceID] = workspace
		previousWorkspaceOrder = append(previousWorkspaceOrder, workspace.WorkspaceID)
	}
	previousArchived := append([]string{}, archived...)
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				currentMetas, listErr := s.store.ListSessions(ctx)
				if listErr != nil {
					continue
				}
				currentActive := make(map[string]store.SessionMeta)
				currentArchived := make([]string, 0)
				for _, meta := range currentMetas {
					if !meta.ArchivedAt.IsZero() {
						currentArchived = append(currentArchived, meta.ID)
						continue
					}
					currentActive[meta.ID] = meta
					if _, exists := active[meta.ID]; !exists {
						if subscribeErr := subscribe(meta, true); subscribeErr != nil {
							continue
						}
					}
				}
				for id := range active {
					if _, exists := currentActive[id]; !exists {
						if unsub, subscribed := unsubs[id]; subscribed {
							unsub()
							delete(unsubs, id)
						}
						_ = write("host/session-removed", map[string]any{"type": "host/session-removed", "sessionId": id})
					}
				}
				active = currentActive
				if !reflect.DeepEqual(previousArchived, currentArchived) {
					previousArchived = append([]string(nil), currentArchived...)
					_ = write("host/archived-sessions-changed", map[string]any{
						"type": "host/archived-sessions-changed", "archivedSessionIds": currentArchived,
					})
				}
				currentWorkspaces, workspaceErr := s.store.ListWorkspaces(ctx)
				if workspaceErr != nil {
					continue
				}
				views, _ := nativeWorkspaceViews(currentWorkspaces, currentMetas)
				currentWorkspaceMap := make(map[string]nativeWorkspaceView, len(views))
				currentWorkspaceOrder := make([]string, 0, len(views))
				for _, workspace := range views {
					currentWorkspaceMap[workspace.WorkspaceID] = workspace
					currentWorkspaceOrder = append(currentWorkspaceOrder, workspace.WorkspaceID)
					previous, exists := previousWorkspaces[workspace.WorkspaceID]
					if !exists || !reflect.DeepEqual(previous, workspace) {
						_ = write("host/workspace-changed", map[string]any{
							"type": "host/workspace-changed", "workspace": workspace,
						})
					}
				}
				for id := range previousWorkspaces {
					if _, exists := currentWorkspaceMap[id]; !exists {
						_ = write("host/workspace-removed", map[string]any{"type": "host/workspace-removed", "workspaceId": id})
					}
				}
				if !reflect.DeepEqual(previousWorkspaceOrder, currentWorkspaceOrder) {
					_ = write("host/workspace-order-changed", map[string]any{
						"type": "host/workspace-order-changed", "workspaceIds": currentWorkspaceOrder,
					})
				}
				previousWorkspaces = currentWorkspaceMap
				previousWorkspaceOrder = currentWorkspaceOrder
			}
		}
	}()
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}()
	go drainNativeWebSocket(reader, cancel)
	<-ctx.Done()
	<-pollDone
}

func (s *Server) nativeHostSessionAdded(meta store.SessionMeta, events []session.Event) map[string]any {
	parent, origin, depth := nativeSessionLineage(events)
	added := map[string]any{
		"type": "host/session-added", "sessionId": meta.ID, "blank": len(events) == 0,
	}
	if parent != "" {
		added["parentSessionId"] = parent
	}
	if origin != "" {
		added["origin"] = origin
	}
	if depth > 0 {
		added["delegationDepth"] = depth
	}
	if meta.CWD != "" {
		added["cwd"] = meta.CWD
	}
	if configs, ok := s.store.(store.SessionConfigStore); ok {
		if cfg, err := configs.GetSessionConfig(context.Background(), meta.ID); err == nil && cfg.AgentPreset != "" {
			added["agentPreset"] = cfg.AgentPreset
		}
	}
	return added
}

func nativeHostAgentError(ev session.Event) string {
	if ev.Type != session.EventToolResult && ev.Type != "tool/error" {
		return ""
	}
	data := nativeJSONObject(ev.Data)
	if nativeBool(data["isError"]) || nativeBool(data["is_error"]) {
		if message := nativeEventString(data, "error", "message", "reason"); message != "" {
			return message
		}
		return "tool execution failed"
	}
	return ""
}

func nativeHostRunningTransition(eventType string) (bool, bool) {
	switch eventType {
	case session.EventTurnStart:
		return true, true
	case session.EventTurnEnd:
		return false, true
	default:
		return false, false
	}
}

func nativePendingInteractionFrame(sessionID string, item interact.Request) (string, any, string, string, bool) {
	if strings.TrimSpace(item.ID) == "" {
		return "", nil, "", "", false
	}
	if len(item.Questions) > 0 {
		questions := make([]map[string]any, 0, len(item.Questions))
		for _, question := range item.Questions {
			if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Question) == "" {
				continue
			}
			entry := map[string]any{"id": question.ID, "question": question.Question}
			if question.Detail != "" {
				entry["detail"] = question.Detail
			}
			if question.Header != "" {
				entry["header"] = question.Header
			}
			if question.MultiSelect {
				entry["multiSelect"] = true
			}
			if len(question.Options) > 0 {
				options := make([]map[string]any, 0, len(question.Options))
				for _, option := range question.Options {
					entry := map[string]any{"label": option.Label}
					if option.Description != "" {
						entry["description"] = option.Description
					}
					options = append(options, entry)
				}
				entry["options"] = options
			}
			questions = append(questions, entry)
		}
		if len(questions) == 0 {
			return "", nil, "", "", false
		}
		return "question/requested", map[string]any{"type": "question/requested", "sessionId": sessionID, "questions": questions}, item.ID, "question", true
	}
	payload := map[string]any{
		"type": "approval/requested", "sessionId": sessionID, "approvalId": item.ID,
		"toolName": item.ToolName,
	}
	if item.Prompt != "" {
		payload["reason"] = item.Prompt
	}
	return "approval/requested", payload, item.ID, "approval", true
}

// nativeQueueFrame converts Shutu's process-local queue items to DSH's
// message-shaped session/queue projection. QueueItem intentionally stays small
// for the legacy REST surface; the native adapter supplies the DSH source and
// content blocks here instead of leaking that shape into the generic server.
func nativeQueueFrame(sessionID string, items []QueueItem) map[string]any {
	view := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		placement := item.Placement
		switch placement {
		case "queued", "steering", "context":
		default:
			placement = "queued"
		}
		content := item.Content
		if len(content) == 0 {
			content = []llm.ContentBlock{llm.Text(item.Text)}
		}
		view = append(view, map[string]any{
			"id":        item.ID,
			"placement": placement,
			"message": map[string]any{
				"id":      item.ID,
				"role":    "user",
				"content": content,
				"source":  map[string]any{"kind": "user", "rpcId": item.ID},
			},
		})
	}
	return map[string]any{"type": "session/queue", "sessionId": sessionID, "items": view}
}

// nativeJobViews converts the existing sanitized job view to DSH's camelCase
// and millisecond timestamp contract. The generic jobs provider is also used
// by the legacy REST API, so the conversion belongs only at this wire edge.
func nativeJobViews(jobs []map[string]any) []map[string]any {
	view := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		item := make(map[string]any, len(job))
		for key, value := range job {
			switch key {
			case "started_at":
				item["startedAt"] = nativeJobTimestamp(value)
			case "finished_at":
				item["finishedAt"] = nativeJobTimestamp(value)
			case "startedAt", "finishedAt":
				item[key] = nativeJobTimestamp(value)
			default:
				item[key] = value
			}
		}
		view = append(view, item)
	}
	return view
}

func nativeJobTimestamp(value any) any {
	switch timestamp := value.(type) {
	case time.Time:
		return timestamp.UnixMilli()
	case *time.Time:
		if timestamp == nil {
			return nil
		}
		return timestamp.UnixMilli()
	default:
		return value
	}
}

func nativeInteractionFrame(sessionID string, ev session.Event) (string, any, string, string, bool) {
	switch ev.Type {
	case session.EventInteractRequest:
		var item interact.Request
		if err := json.Unmarshal(ev.Data, &item); err != nil {
			return "", nil, "", "", false
		}
		return nativePendingInteractionFrame(sessionID, item)
	case session.EventApprovalAsked:
		var item struct {
			ID        string              `json:"id"`
			CallID    string              `json:"callId"`
			ToolName  string              `json:"toolName"`
			Prompt    string              `json:"prompt"`
			Reason    string              `json:"reason"`
			Questions []interact.Question `json:"questions"`
		}
		if err := json.Unmarshal(ev.Data, &item); err != nil || item.ID == "" || item.ToolName == "" {
			return "", nil, "", "", false
		}
		if item.Prompt == "" {
			item.Prompt = item.Reason
		}
		return nativePendingInteractionFrame(sessionID, interact.Request{
			ID: item.ID, CallID: item.CallID, ToolName: item.ToolName,
			Prompt: item.Prompt, Questions: item.Questions,
		})
	case session.EventInteractResolve:
		var fact struct {
			ID       string `json:"id"`
			Approved bool   `json:"approved"`
		}
		if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
			return "", nil, "", "", false
		}
		outcome := "rejected"
		if fact.Approved {
			outcome = "allowed-once"
		}
		return "approval/resolved", map[string]any{
			"type": "approval/resolved", "sessionId": sessionID,
			"approvalId": fact.ID, "outcome": outcome,
		}, fact.ID, "", true
	case session.EventApprovalDecided:
		var fact struct {
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" || fact.Outcome == "" {
			return "", nil, "", "", false
		}
		return "approval/resolved", map[string]any{
			"type": "approval/resolved", "sessionId": sessionID,
			"approvalId": fact.ID, "outcome": fact.Outcome,
		}, fact.ID, "", true
	case session.EventInteractCancel:
		var fact struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
			return "", nil, "", "", false
		}
		return "", nil, fact.ID, "", true
	default:
		return "", nil, "", "", false
	}
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
