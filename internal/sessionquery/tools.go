// Package sessionquery provides the first local session-query seam.
//
// The five tools mirror dsh's read-only model-facing session-query surface,
// but use the existing SQLite Store as the authoritative corpus. This package
// deliberately does not depend on the loop: querying is a projection over
// already committed session events (D1/D4).
package sessionquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
)

const (
	SessionSearchToolName = "session_search"
	EventSearchToolName   = "session_event_search"
	SessionTraceToolName  = "session_trace"
	EventTraceToolName    = "session_event_trace"
	EventReadToolName     = "session_event_read"
	DefaultMaxResults     = 100
	MaxResults            = 100
)

// Backend is the minimal read-only persistence surface needed by the tools.
// Keeping this smaller than store.Store makes the capability easy to test and
// prevents the query package from gaining mutation authority.
type Backend interface {
	SearchSessions(context.Context, string) ([]store.SearchHit, error)
	LoadSession(context.Context, string) ([]session.Event, error)
	ListSessions(context.Context) ([]store.SessionMeta, error)
}

// Tools bundles the five read-only consumers over one backend.
type Tools struct {
	backend        Backend
	current        func() string
	currentContext func(context.Context) string
	maxResults     int
	searchTimeout  time.Duration
}

type sessionSearchArgs struct {
	Query            string   `json:"query"`
	SessionIDs       []string `json:"session_ids"`
	CreatedAtFrom    string   `json:"created_at_from"`
	CreatedAtTo      string   `json:"created_at_to"`
	ParentSessionIDs []string `json:"parent_session_ids"`
	IncludeRoot      bool     `json:"include_root_sessions"`
	Availability     []string `json:"availability"`
	EventSeqFrom     *uint64  `json:"event_seq_from"`
	EventSeqTo       *uint64  `json:"event_seq_to"`
	EventTimeFrom    string   `json:"event_time_from"`
	EventTimeTo      string   `json:"event_time_to"`
	EventTypes       []string `json:"event_types"`
	EventSurfaces    []string `json:"event_surfaces"`
}

type eventSearchArgs struct {
	SessionID  string   `json:"session_id"`
	Query      string   `json:"query"`
	SeqFrom    *uint64  `json:"seq_from"`
	SeqTo      *uint64  `json:"seq_to"`
	TimeFrom   string   `json:"time_from"`
	TimeTo     string   `json:"time_to"`
	EventTypes []string `json:"event_types"`
	Surfaces   []string `json:"surfaces"`
}

// authorizedMetas projects the local workspace boundary for the calling
// session. Store already persists WorkspaceID for the web sidebar; reusing it
// here keeps session-query read-only while preventing cross-workspace reads.
func (t *Tools) authorizedMetas(ctx context.Context) (map[string]store.SessionMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current := t.currentID(ctx)
	if current == "" {
		return nil, errors.New("current session is unavailable")
	}
	metas, err := t.backend.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	var currentMeta *store.SessionMeta
	for i := range metas {
		if metas[i].ID == current {
			currentMeta = &metas[i]
			break
		}
	}
	if currentMeta == nil {
		return nil, fmt.Errorf("current session %q is not persisted", current)
	}
	scope := make(map[string]store.SessionMeta)
	for _, meta := range metas {
		if meta.WorkspaceID != currentMeta.WorkspaceID {
			continue
		}
		if currentMeta.CWD != "" && meta.CWD != currentMeta.CWD {
			continue
		}
		scope[meta.ID] = meta
	}
	return scope, nil
}

func (t *Tools) authorizeTarget(ctx context.Context, id string) (map[string]store.SessionMeta, error) {
	scope, err := t.authorizedMetas(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := scope[id]; !ok {
		return nil, fmt.Errorf("session %q is outside the caller workspace", id)
	}
	return scope, nil
}

// NewTools binds the query consumers to the durable session backend. current
// returns the active session id; nil means calls must provide session_id.
func NewTools(backend Backend, current func() string, maxResults ...int) *Tools {
	limit := DefaultMaxResults
	if len(maxResults) > 0 && maxResults[0] >= 1 && maxResults[0] <= MaxResults {
		limit = maxResults[0]
	}
	return &Tools{backend: backend, current: current, maxResults: limit, searchTimeout: 30 * time.Second}
}

// NewToolsWithConfig binds DSH's deployment-owned search cap and cooperative
// search deadline. The model cannot override either value.
func NewToolsWithConfig(backend Backend, current func() string, maxResults int, searchTimeout time.Duration) *Tools {
	return newTools(backend, current, nil, maxResults, searchTimeout)
}

// NewToolsWithConfigContext binds the current-session resolver to the
// addressed runtime context. A non-nil context resolver is authoritative: if
// the context has no session it returns no current session instead of falling
// back to a process-global selection. This is the constructor used by the
// Agent-owned composition root; NewToolsWithConfig remains source-compatible
// for legacy embedders.
func NewToolsWithConfigContext(backend Backend, current func(context.Context) string, maxResults int, searchTimeout time.Duration) *Tools {
	return newTools(backend, nil, current, maxResults, searchTimeout)
}

func newTools(backend Backend, current func() string, currentContext func(context.Context) string, maxResults int, searchTimeout time.Duration) *Tools {
	if maxResults < 1 || maxResults > MaxResults {
		maxResults = DefaultMaxResults
	}
	if searchTimeout <= 0 {
		searchTimeout = 30 * time.Second
	}
	return &Tools{backend: backend, current: current, currentContext: currentContext, maxResults: maxResults, searchTimeout: searchTimeout}
}

func (SearchTool) ConcurrencySafe(any) bool      { return false }
func (EventSearchTool) ConcurrencySafe(any) bool { return false }
func (TraceTool) ConcurrencySafe(any) bool       { return true }
func (EventTraceTool) ConcurrencySafe(any) bool  { return true }
func (EventReadTool) ConcurrencySafe(any) bool   { return true }

func (t *Tools) Search() SearchTool           { return SearchTool{tools: t} }
func (t *Tools) EventSearch() EventSearchTool { return EventSearchTool{tools: t} }
func (t *Tools) Trace() TraceTool             { return TraceTool{tools: t} }
func (t *Tools) EventTrace() EventTraceTool   { return EventTraceTool{tools: t} }
func (t *Tools) Read() EventReadTool          { return EventReadTool{tools: t} }

type SearchTool struct{ tools *Tools }

func (SearchTool) Name() string { return SessionSearchToolName }
func (SearchTool) Description() string {
	return "search prior sessions in the caller workspace and return the strongest matching event from each session"
}
func (SearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"query":                 map[string]any{"type": "string", "minLength": 1, "description": "literal full-text query over prior session history"},
			"session_ids":           map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"created_at_from":       map[string]any{"type": "string", "description": "inclusive timezone-qualified ISO 8601 creation-time lower bound"},
			"created_at_to":         map[string]any{"type": "string", "description": "inclusive timezone-qualified ISO 8601 creation-time upper bound"},
			"parent_session_ids":    map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"include_root_sessions": map[string]any{"type": "boolean"},
			"availability":          map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": []string{"live", "persisted"}}},
			"event_seq_from":        map[string]any{"type": "integer", "minimum": 0},
			"event_seq_to":          map[string]any{"type": "integer", "minimum": 0},
			"event_time_from":       map[string]any{"type": "string", "description": "inclusive timezone-qualified ISO 8601 event-time lower bound"},
			"event_time_to":         map[string]any{"type": "string", "description": "inclusive timezone-qualified ISO 8601 event-time upper bound"},
			"event_types":           map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"event_surfaces":        map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": []string{"current", "shadowed", "log-only"}}},
		},
		"required": []string{"query"},
	}
}
func (t SearchTool) Execute(ctx context.Context, args any) (string, error) {
	ctx, cancel := t.tools.searchContext(ctx)
	defer cancel()
	var a sessionSearchArgs
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	query, err := normalizeQuery(a.Query)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	if err := validateSessionSearchArgs(a); err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	scope, err := t.tools.authorizedMetas(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	current := t.tools.currentID(ctx)
	hits, err := t.tools.searchHits(ctx, query, 0, scope, current)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	lines := []string{}
	capped := false
	for _, hit := range hits {
		if hit.SessionID == current {
			continue
		}
		if _, ok := scope[hit.SessionID]; !ok {
			continue
		}
		meta := scope[hit.SessionID]
		parentID, events, err := t.tools.sessionContext(ctx, hit.SessionID)
		if err != nil {
			return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
		}
		if !sessionMatches(meta, parentID, a) {
			continue
		}
		if len(a.ParentSessionIDs) > 0 && parentID != "" {
			if _, authorized := scope[parentID]; !authorized {
				continue
			}
		}
		surfaces, err := projection.ClassifyEventSurfaces(events)
		if err != nil {
			return "", fmt.Errorf("%s: project event surfaces: %w", SessionSearchToolName, err)
		}
		match := bestEventMatch(events, surfaces, query, a.EventSeqFrom, a.EventSeqTo, a.EventTimeFrom, a.EventTimeTo, a.EventTypes, a.EventSurfaces)
		if hasEventFilters(a) && match == nil {
			continue
		}
		if len(lines) == t.tools.maxResults {
			capped = true
			break
		}
		lines = append(lines, formatSessionHit(len(lines)+1, hit, meta, parentID, match))
	}
	if len(lines) == 0 {
		return "No prior session matches found.", nil
	}
	out := fmt.Sprintf("Session search results (%d):\n%s", len(lines), strings.Join(lines, "\n"))
	if capped {
		out += "\n\nResult cap reached. Narrow the query or add filters to find additional matches."
	}
	return out, nil
}

type EventSearchTool struct{ tools *Tools }

func (EventSearchTool) Name() string { return EventSearchToolName }
func (EventSearchTool) Description() string {
	return "search prior events in one authorized session"
}
func (EventSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"session_id":  map[string]any{"type": "string", "description": "target session id; omit for the current session"},
			"query":       map[string]any{"type": "string", "minLength": 1, "description": "literal full-text query over the target session"},
			"seq_from":    map[string]any{"type": "integer", "minimum": 0},
			"seq_to":      map[string]any{"type": "integer", "minimum": 0},
			"time_from":   map[string]any{"type": "string", "description": "inclusive timezone-qualified ISO 8601 event-time lower bound"},
			"time_to":     map[string]any{"type": "string", "description": "inclusive timezone-qualified ISO 8601 event-time upper bound"},
			"event_types": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"surfaces":    map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": []string{"current", "shadowed", "log-only"}}},
		},
		"required": []string{"query"},
	}
}
func (t EventSearchTool) Execute(ctx context.Context, args any) (string, error) {
	ctx, cancel := t.tools.searchContext(ctx)
	defer cancel()
	var in eventSearchArgs
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	query, err := normalizeQuery(in.Query)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	if err := validateEventSearchArgs(in); err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	id := t.tools.targetID(ctx, in.SessionID)
	if id == "" {
		return "", fmt.Errorf("%s: session_id is required when there is no current session", EventSearchToolName)
	}
	if _, err := t.tools.authorizeTarget(ctx, id); err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	events, err := t.tools.backend.LoadSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	if id == t.tools.currentID(ctx) && latestStepStart(events) == 0 {
		return "", fmt.Errorf("%s: current-session search requires an active step boundary", EventSearchToolName)
	}
	surfaces, err := projection.ClassifyEventSurfaces(events)
	if err != nil {
		return "", fmt.Errorf("%s: project event surfaces: %w", EventSearchToolName, err)
	}
	needle := foldText(query)
	lines := []string{}
	capped := false
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if id == t.tools.currentID(ctx) && isAfterCurrentStep(events, ev.Seq) {
			continue
		}
		text := eventText(ev)
		if !strings.Contains(foldText(text), needle) || !eventMatches(surfaces, ev, in.SeqFrom, in.SeqTo, in.TimeFrom, in.TimeTo, in.EventTypes, in.Surfaces) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. seq %d | %s | %s | %s\n   Snippet: %s", len(lines)+1, ev.Seq, ev.Type, surfaces[ev.Seq], ev.At.UTC().Format(time.RFC3339), bound(text, 500)))
		if len(lines) == t.tools.maxResults {
			capped = true
			break
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Session %s\n\nNo prior event matches found.", id), nil
	}
	title := "(untitled)"
	if scope, scopeErr := t.tools.authorizedMetas(ctx); scopeErr == nil {
		if meta, ok := scope[id]; ok {
			title = titleOrUntitled(meta.Title)
		}
	}
	out := fmt.Sprintf("Session %s — %s\n\nEvent search results (%d):\n%s", id, title, len(lines), strings.Join(lines, "\n"))
	if capped {
		out += "\n\nResult cap reached. Narrow the query or add filters to find additional matches."
	}
	return out, nil
}

type TraceTool struct{ tools *Tools }

func (TraceTool) Name() string        { return SessionTraceToolName }
func (TraceTool) Description() string { return "read the known local lineage around a session" }
func (TraceTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"session_id": map[string]any{"type": "string", "description": "target session; defaults to the current session"},
	}}
}
func (t TraceTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", SessionTraceToolName, err)
	}
	id := t.tools.targetID(ctx, in.SessionID)
	if id == "" {
		return "", fmt.Errorf("%s: session_id is required when there is no current session", SessionTraceToolName)
	}
	scope, err := t.tools.authorizeTarget(ctx, id)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionTraceToolName, err)
	}
	parents, children, err := t.tools.lineage(ctx, scope)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionTraceToolName, err)
	}
	return formatLineage(id, scope, parents, children), nil
}

type EventTraceTool struct{ tools *Tools }

func (EventTraceTool) Name() string { return EventTraceToolName }
func (EventTraceTool) Description() string {
	return "read replacement relationships around one local event"
}
func (EventTraceTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"session_id": map[string]any{"type": "string", "description": "target session; defaults to the current session"},
		"seq":        map[string]any{"type": "integer", "minimum": 0, "description": "target event sequence number"},
	}, "required": []string{"seq"}}
}
func (t EventTraceTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		SessionID string `json:"session_id"`
		Seq       uint64 `json:"seq"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", EventTraceToolName, err)
	}
	id := t.tools.targetID(ctx, in.SessionID)
	if id == "" {
		return "", fmt.Errorf("%s: session_id is required when there is no current session", EventTraceToolName)
	}
	if _, err := t.tools.authorizeTarget(ctx, id); err != nil {
		return "", fmt.Errorf("%s: %w", EventTraceToolName, err)
	}
	events, err := t.tools.backend.LoadSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventTraceToolName, err)
	}
	if in.Seq == 0 {
		return "", fmt.Errorf("%s: seq must be positive", EventTraceToolName)
	}
	target := -1
	for i := range events {
		if events[i].Seq == in.Seq {
			target = i
			break
		}
	}
	if target < 0 {
		return "", fmt.Errorf("%s: event seq %d not found", EventTraceToolName, in.Seq)
	}
	relations, err := projection.EventRelations(events, in.Seq)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventTraceToolName, err)
	}
	return fmt.Sprintf("Session %s\nTarget: seq %d | %s | %s\nReplaced by: %s\nReplacement chain: %s\nEvents replaced by target: %s\nEvents cited directly as sources: %s\nDirect derived events: %s",
		id, events[target].Seq, events[target].Type, events[target].At.UTC().Format(time.RFC3339),
		seqOrNone(relations.ReplacedBy), seqListOrNone(relations.ReplacementChain), formatReplacedRange(relations.Replaces),
		seqListOrNone(relations.Sources), seqListOrNone(relations.Derived),
	), nil
}

type EventReadTool struct{ tools *Tools }

func (EventReadTool) Name() string { return EventReadToolName }
func (EventReadTool) Description() string {
	return "read one full local event and bounded neighboring event summaries"
}
func (EventReadTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"session_id": map[string]any{"type": "string", "description": "target session; defaults to the current session"},
		"seq":        map[string]any{"type": "integer", "minimum": 0, "description": "target event sequence number"},
		"before":     map[string]any{"type": "integer", "minimum": 0, "description": "preceding raw events to summarize; omit for none"},
		"after":      map[string]any{"type": "integer", "minimum": 0, "description": "following raw events to summarize; omit for none"},
	}, "required": []string{"seq"}}
}
func (t EventReadTool) Execute(ctx context.Context, args any) (string, error) {
	var in struct {
		SessionID string `json:"session_id"`
		Seq       uint64 `json:"seq"`
		Before    int    `json:"before"`
		After     int    `json:"after"`
	}
	if err := agenttools.DecodeArgs(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", EventReadToolName, err)
	}
	id := t.tools.targetID(ctx, in.SessionID)
	if id == "" {
		return "", fmt.Errorf("%s: session_id is required when there is no current session", EventReadToolName)
	}
	if _, err := t.tools.authorizeTarget(ctx, id); err != nil {
		return "", fmt.Errorf("%s: %w", EventReadToolName, err)
	}
	if in.Seq == 0 {
		return "", fmt.Errorf("%s: seq must be positive", EventReadToolName)
	}
	if in.Before < 0 || in.After < 0 {
		return "", fmt.Errorf("%s: before/after must be non-negative", EventReadToolName)
	}
	events, err := t.tools.backend.LoadSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventReadToolName, err)
	}
	index := -1
	for i := range events {
		if events[i].Seq == in.Seq {
			index = i
			break
		}
	}
	if index < 0 {
		return "", fmt.Errorf("%s: event seq %d not found", EventReadToolName, in.Seq)
	}
	title := "(untitled)"
	if scope, scopeErr := t.tools.authorizedMetas(ctx); scopeErr == nil {
		if meta, ok := scope[id]; ok {
			title = titleOrUntitled(meta.Title)
		}
	}
	start, end := index-in.Before, index+in.After
	if start < 0 {
		start = 0
	}
	if end >= len(events) {
		end = len(events) - 1
	}
	var target any
	if err := json.Unmarshal(events[index].Data, &target); err != nil {
		target = string(events[index].Data)
	}
	raw, _ := json.MarshalIndent(map[string]any{"seq": events[index].Seq, "type": events[index].Type, "time": events[index].At.UTC(), "version": events[index].Version, "data": target}, "", "  ")
	lines := []string{fmt.Sprintf("Session %s — %s", id, title), fmt.Sprintf("Target event seq %d:", in.Seq), "```json", string(raw), "```"}
	for i := start; i <= end; i++ {
		if i == index {
			continue
		}
		if i < index && i == start {
			lines = append(lines, "", "Before:")
		}
		if i > index && i == index+1 {
			lines = append(lines, "", "After:")
		}
		lines = append(lines, fmt.Sprintf("- seq %d | %s | %s\n  %s", events[i].Seq, events[i].Type, events[i].At.UTC().Format(time.RFC3339), bound(eventText(events[i]), 500)))
	}
	return strings.Join(lines, "\n"), nil
}

func (t *Tools) currentID(ctx ...context.Context) string {
	if len(ctx) > 0 {
		if id := runtimectx.SessionID(ctx[0]); id != "" {
			return id
		}
		if t.currentContext != nil {
			return strings.TrimSpace(t.currentContext(ctx[0]))
		}
	}
	if t.current == nil {
		return ""
	}
	return strings.TrimSpace(t.current())
}

func (t *Tools) searchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if t.searchTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, t.searchTimeout)
}

func (t *Tools) sessionContext(ctx context.Context, id string) (string, []session.Event, error) {
	events, err := t.backend.LoadSession(ctx, id)
	if err != nil {
		return "", nil, err
	}
	var parent string
	for _, ev := range events {
		if ev.Type != session.EventSubagentStart {
			continue
		}
		var data struct {
			ID            string `json:"id"`
			ParentSession string `json:"parentSession"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.ID == id {
			parent = data.ParentSession
			break
		}
	}
	return parent, events, nil
}

func validateSessionSearchArgs(a sessionSearchArgs) error {
	if err := validateStringList("session_ids", a.SessionIDs); err != nil {
		return err
	}
	if err := validateStringList("parent_session_ids", a.ParentSessionIDs); err != nil {
		return err
	}
	if err := validateStringList("event_types", a.EventTypes); err != nil {
		return err
	}
	if err := validateStringList("event_surfaces", a.EventSurfaces); err != nil {
		return err
	}
	if err := validateAvailability(a.Availability); err != nil {
		return err
	}
	if err := validateSurfaces(a.EventSurfaces); err != nil {
		return err
	}
	if err := validateTimeRange("created_at", a.CreatedAtFrom, a.CreatedAtTo); err != nil {
		return err
	}
	return validateEventRange(a.EventSeqFrom, a.EventSeqTo, a.EventTimeFrom, a.EventTimeTo, a.EventTypes, a.EventSurfaces)
}

func validateEventSearchArgs(a eventSearchArgs) error {
	if err := validateStringList("event_types", a.EventTypes); err != nil {
		return err
	}
	if err := validateStringList("surfaces", a.Surfaces); err != nil {
		return err
	}
	if err := validateSurfaces(a.Surfaces); err != nil {
		return err
	}
	return validateEventRange(a.SeqFrom, a.SeqTo, a.TimeFrom, a.TimeTo, a.EventTypes, a.Surfaces)
}

func validateStringList(name string, values []string) error {
	if values == nil {
		return nil
	}
	if len(values) == 0 {
		return fmt.Errorf("%s must contain at least one value when supplied", name)
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not contain empty values", name)
		}
	}
	return nil
}

func validateAvailability(values []string) error {
	for _, value := range values {
		if value != "live" && value != "persisted" {
			return fmt.Errorf("availability contains unsupported value %q", value)
		}
	}
	return nil
}

func validateSurfaces(values []string) error {
	for _, value := range values {
		if value != "current" && value != "shadowed" && value != "log-only" {
			return fmt.Errorf("surfaces contains unsupported value %q", value)
		}
	}
	return nil
}

func parseOptionalTime(name, value string) (time.Time, bool, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s must be an ISO 8601 timestamp with Z or a numeric offset", name)
	}
	return parsed.UTC(), true, nil
}

func validateTimeRange(name, from, to string) error {
	start, hasStart, err := parseOptionalTime(name+"_from", from)
	if err != nil {
		return err
	}
	end, hasEnd, err := parseOptionalTime(name+"_to", to)
	if err != nil {
		return err
	}
	if hasStart && hasEnd && start.After(end) {
		return fmt.Errorf("%s range from must be less than or equal to to", name)
	}
	return nil
}

func validateEventRange(from, to *uint64, timeFrom, timeTo string, eventTypes, surfaces []string) error {
	if from != nil && to != nil && *from > *to {
		return errors.New("event sequence range from must be less than or equal to to")
	}
	if err := validateTimeRange("time", timeFrom, timeTo); err != nil {
		return err
	}
	return nil
}

func sessionMatches(meta store.SessionMeta, parent string, a sessionSearchArgs) bool {
	if len(a.SessionIDs) > 0 && !contains(a.SessionIDs, meta.ID) {
		return false
	}
	if len(a.Availability) > 0 && !contains(a.Availability, "persisted") {
		return false
	}
	if start, ok, _ := parseOptionalTime("created_at_from", a.CreatedAtFrom); ok && meta.CreatedAt.Before(start) {
		return false
	}
	if end, ok, _ := parseOptionalTime("created_at_to", a.CreatedAtTo); ok && meta.CreatedAt.After(end) {
		return false
	}
	if len(a.ParentSessionIDs) > 0 || a.IncludeRoot {
		matchesParent := len(a.ParentSessionIDs) > 0 && contains(a.ParentSessionIDs, parent)
		if a.IncludeRoot && parent == "" {
			matchesParent = true
		}
		if !matchesParent {
			return false
		}
	}
	return true
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func hasEventFilters(a sessionSearchArgs) bool {
	return a.EventSeqFrom != nil || a.EventSeqTo != nil || a.EventTimeFrom != "" || a.EventTimeTo != "" || len(a.EventTypes) > 0 || len(a.EventSurfaces) > 0
}

type eventMatch struct {
	Event   session.Event
	Surface string
	Snippet string
}

func bestEventMatch(events []session.Event, classified map[uint64]projection.EventSurface, query string, seqFrom, seqTo *uint64, timeFrom, timeTo string, types, surfaces []string) *eventMatch {
	needle := foldText(query)
	for _, ev := range events {
		text := eventText(ev)
		if !strings.Contains(foldText(text), needle) || !eventMatches(classified, ev, seqFrom, seqTo, timeFrom, timeTo, types, surfaces) {
			continue
		}
		return &eventMatch{Event: ev, Surface: string(classified[ev.Seq]), Snippet: bound(text, 500)}
	}
	return nil
}

func eventMatches(classified map[uint64]projection.EventSurface, ev session.Event, seqFrom, seqTo *uint64, timeFrom, timeTo string, types, surfaces []string) bool {
	if seqFrom != nil && ev.Seq < *seqFrom || seqTo != nil && ev.Seq > *seqTo {
		return false
	}
	if start, ok, _ := parseOptionalTime("time_from", timeFrom); ok && ev.At.Before(start) {
		return false
	}
	if end, ok, _ := parseOptionalTime("time_to", timeTo); ok && ev.At.After(end) {
		return false
	}
	if len(types) > 0 && !contains(types, ev.Type) {
		return false
	}
	if len(surfaces) > 0 && !contains(surfaces, string(classified[ev.Seq])) {
		return false
	}
	return true
}

func isAfterCurrentStep(events []session.Event, seq uint64) bool {
	start := latestStepStart(events)
	return start != 0 && seq >= start
}

func latestStepStart(events []session.Event) uint64 {
	var start uint64
	for _, ev := range events {
		if ev.Type == session.EventStepStart && ev.Seq > start {
			start = ev.Seq
		}
	}
	return start
}

func formatSessionHit(index int, hit store.SearchHit, meta store.SessionMeta, parent string, match *eventMatch) string {
	parentText := "root"
	if parent != "" {
		parentText = parent
	}
	seq, typ, surface, at, snippet := uint64(0), "unknown", "current", hit.UpdatedAt, bound(hit.Snippet, 500)
	if match != nil {
		seq, typ, surface, at, snippet = match.Event.Seq, match.Event.Type, match.Surface, match.Event.At, match.Snippet
	}
	return fmt.Sprintf("\n%d. Session %s — %s\n   Created: %s\n   Parent: %s\n   Availability: persisted\n   Best match: seq %d | %s | %s | %s\n   Snippet: %s", index, meta.ID, titleOrUntitled(meta.Title), meta.CreatedAt.UTC().Format(time.RFC3339), parentText, seq, typ, surface, at.UTC().Format(time.RFC3339), snippet)
}

func (t *Tools) searchHits(ctx context.Context, query string, limit int, scope map[string]store.SessionMeta, current string) ([]store.SearchHit, error) {
	if pager, ok := t.backend.(store.SessionSearchPager); ok {
		const pageSize = 20
		offset := 0
		var out []store.SearchHit
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page, more, err := pager.SearchSessionsPage(ctx, query, offset, pageSize)
			if err != nil {
				return nil, err
			}
			for _, hit := range page {
				if hit.SessionID == current {
					continue
				}
				if _, ok := scope[hit.SessionID]; !ok {
					continue
				}
				out = append(out, hit)
				if len(out) == limit {
					return out, nil
				}
			}
			offset += len(page)
			if !more {
				return out, nil
			}
		}
	}
	hits, err := t.backend.SearchSessions(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []store.SearchHit
	for _, hit := range hits {
		if hit.SessionID == current {
			continue
		}
		if _, ok := scope[hit.SessionID]; !ok {
			continue
		}
		out = append(out, hit)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (t *Tools) targetID(ctx context.Context, id string) string {
	if strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return t.currentID(ctx)
}

func (t *Tools) lineage(ctx context.Context, scope map[string]store.SessionMeta) (map[string]string, map[string][]string, error) {
	parents := make(map[string]string)
	children := make(map[string][]string)
	for id := range scope {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		events, err := t.backend.LoadSession(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		for _, ev := range events {
			if ev.Type != session.EventSubagentStart {
				continue
			}
			var data struct {
				ID            string `json:"id"`
				ParentSession string `json:"parentSession"`
			}
			if json.Unmarshal(ev.Data, &data) != nil || data.ID == "" || data.ParentSession == "" {
				continue
			}
			if _, ok := scope[data.ID]; !ok {
				continue
			}
			parents[data.ID] = data.ParentSession
			children[data.ParentSession] = append(children[data.ParentSession], data.ID)
		}
	}
	for parent := range children {
		sort.Strings(children[parent])
	}
	return parents, children, nil
}

func formatLineage(id string, scope map[string]store.SessionMeta, parents map[string]string, children map[string][]string) string {
	target := scope[id]
	lines := []string{
		fmt.Sprintf("Session %s | %s", id, titleOrUntitled(target.Title)),
		fmt.Sprintf("Created: %s", target.CreatedAt.UTC().Format(time.RFC3339)),
		"Availability: persisted",
		"",
		"Ancestors (nearest first):",
	}
	seen := map[string]bool{id: true}
	ancestorCount := 0
	for parent := parents[id]; parent != ""; parent = parents[parent] {
		if seen[parent] {
			lines = append(lines, "- [lineage cycle omitted]")
			break
		}
		seen[parent] = true
		meta, ok := scope[parent]
		if !ok {
			lines = append(lines, "- [outside workspace boundary]")
			break
		}
		lines = append(lines, fmt.Sprintf("- %s | %s | %s", parent, titleOrUntitled(meta.Title), meta.CreatedAt.UTC().Format(time.RFC3339)))
		ancestorCount++
	}
	if ancestorCount == 0 && len(lines) == 5 {
		lines = append(lines, "- none (target is a root session)")
	}
	lines = append(lines, "", "Descendants:")
	if len(children[id]) == 0 {
		lines = append(lines, "- none")
		return strings.Join(lines, "\n")
	}
	var walk func(string, int)
	walk = func(parent string, depth int) {
		for _, child := range children[parent] {
			meta, ok := scope[child]
			if !ok {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s- %s | %s | %s", strings.Repeat("  ", depth), child, titleOrUntitled(meta.Title), meta.CreatedAt.UTC().Format(time.RFC3339)))
			walk(child, depth+1)
		}
	}
	walk(id, 0)
	return strings.Join(lines, "\n")
}
func (t *Tools) resultLimit(n int) (int, error) {
	if n == 0 {
		if t.maxResults > 0 {
			return t.maxResults, nil
		}
		return DefaultMaxResults, nil
	}
	return resultLimit(n)
}
func normalizeQuery(s string) (string, error) {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	if s == "" {
		return "", errors.New("query must contain non-whitespace text")
	}
	if strings.ContainsRune(s, '\x00') {
		return "", errors.New("query must not contain NUL")
	}
	return s, nil
}
func resultLimit(n int) (int, error) {
	if n == 0 {
		return DefaultMaxResults, nil
	}
	if n < 1 || n > MaxResults {
		return 0, fmt.Errorf("limit must be between 1 and %d", MaxResults)
	}
	return n, nil
}
func foldText(s string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " "))
}
func bound(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
func titleOrUntitled(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(untitled)"
	}
	return bound(s, 120)
}
func eventText(ev session.Event) string {
	var v any
	if json.Unmarshal(ev.Data, &v) != nil {
		return string(ev.Data)
	}
	var out []string
	collectStrings(v, &out)
	if len(out) == 0 {
		return ev.Type
	}
	return strings.Join(out, " ")
}
func collectStrings(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case []any:
		for _, item := range x {
			collectStrings(item, out)
		}
	case map[string]any:
		for k, item := range x {
			if k != "id" && k != "seq" && k != "version" {
				collectStrings(item, out)
			}
		}
	}
}
func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
func formatReplacedRange(values []uint64) string {
	if len(values) == 0 {
		return "none"
	}
	first, last := values[0], values[0]
	for _, value := range values[1:] {
		if value < first {
			first = value
		}
		if value > last {
			last = value
		}
	}
	return fmt.Sprintf("%d-%d", first, last)
}
func seqOrNone(n uint64) string {
	if n == 0 {
		return "none"
	}
	return fmt.Sprint(n)
}

func seqListOrNone(values []uint64) string {
	if len(values) == 0 {
		return "none"
	}
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprint(value)
	}
	return strings.Join(parts, ", ")
}

// Keep deterministic ordering available to callers that use the package
// service directly rather than the SQLite SearchSessions implementation.
func sortEvents(events []session.Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
}
