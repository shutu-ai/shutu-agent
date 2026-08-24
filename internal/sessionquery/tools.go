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
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

const (
	SessionSearchToolName = "session_search"
	EventSearchToolName   = "session_event_search"
	SessionTraceToolName  = "session_trace"
	EventTraceToolName    = "session_event_trace"
	EventReadToolName     = "session_event_read"
	DefaultMaxResults     = 20
	MaxResults            = 100
	MaxEventWindow        = 20
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
	backend    Backend
	current    func() string
	maxResults int
}

// authorizedMetas projects the local workspace boundary for the calling
// session. Store already persists WorkspaceID for the web sidebar; reusing it
// here keeps session-query read-only while preventing cross-workspace reads.
func (t *Tools) authorizedMetas(ctx context.Context) (map[string]store.SessionMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current := t.currentID()
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
	return &Tools{backend: backend, current: current, maxResults: limit}
}

func (t *Tools) Search() SearchTool           { return SearchTool{tools: t} }
func (t *Tools) EventSearch() EventSearchTool { return EventSearchTool{tools: t} }
func (t *Tools) Trace() TraceTool             { return TraceTool{tools: t} }
func (t *Tools) EventTrace() EventTraceTool   { return EventTraceTool{tools: t} }
func (t *Tools) Read() EventReadTool          { return EventReadTool{tools: t} }

type SearchTool struct{ tools *Tools }

func (SearchTool) Name() string { return SessionSearchToolName }
func (SearchTool) Description() string {
	return "search prior local sessions and return the strongest matching event from each session"
}
func (SearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "minLength": 1, "description": "literal text to find in prior session events"},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResults, "description": "maximum sessions to return; default 20"},
		},
		"required": []string{"query"},
	}
}
func (t SearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	query, err := normalizeQuery(a.Query)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	limit, err := t.tools.resultLimit(a.Limit)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	scope, err := t.tools.authorizedMetas(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	current := t.tools.currentID()
	hits, err := t.tools.searchHits(ctx, query, limit, scope, current)
	if err != nil {
		return "", fmt.Errorf("%s: %w", SessionSearchToolName, err)
	}
	lines := []string{}
	for _, hit := range hits {
		if hit.SessionID == current {
			continue
		}
		if _, ok := scope[hit.SessionID]; !ok {
			continue
		}
		if len(lines) == limit {
			break
		}
		lines = append(lines, fmt.Sprintf("- %s | %s | %s | %s", hit.SessionID, titleOrUntitled(hit.Title), hit.UpdatedAt.UTC().Format(time.RFC3339), bound(hit.Snippet, 500)))
	}
	if len(lines) == 0 {
		return "No prior session matches found.", nil
	}
	return fmt.Sprintf("Session search results (%d):\n%s", len(lines), strings.Join(lines, "\n")), nil
}

type EventSearchTool struct{ tools *Tools }

func (EventSearchTool) Name() string { return EventSearchToolName }
func (EventSearchTool) Description() string {
	return "search earlier events in one local session and return matching event summaries"
}
func (EventSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"session_id": map[string]any{"type": "string", "description": "target session; defaults to the current session"},
			"query":      map[string]any{"type": "string", "minLength": 1, "description": "literal text to find in event data"},
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResults, "description": "maximum events to return; default 20"},
		},
		"required": []string{"query"},
	}
}
func (t EventSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// Use a second struct because encoding/json tags on an anonymous combined
	// declaration are easy to get wrong and would silently widen the contract.
	var in struct {
		SessionID string `json:"session_id"`
		Query     string `json:"query"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	query, err := normalizeQuery(in.Query)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	limit, err := t.tools.resultLimit(in.Limit)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EventSearchToolName, err)
	}
	id := t.tools.targetID(in.SessionID)
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
	needle := foldText(query)
	lines := []string{}
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		text := eventText(ev)
		if !strings.Contains(foldText(text), needle) {
			continue
		}
		lines = append(lines, fmt.Sprintf("- seq %d | %s | %s\n  %s", ev.Seq, ev.Type, ev.At.UTC().Format(time.RFC3339), bound(text, 500)))
		if len(lines) == limit {
			break
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Session %s\n\nNo prior event matches found.", id), nil
	}
	return fmt.Sprintf("Session %s\n\nEvent search results (%d):\n%s", id, len(lines), strings.Join(lines, "\n")), nil
}

type TraceTool struct{ tools *Tools }

func (TraceTool) Name() string        { return SessionTraceToolName }
func (TraceTool) Description() string { return "read the known local lineage around a session" }
func (TraceTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"session_id": map[string]any{"type": "string", "description": "target session; defaults to the current session"},
	}}
}
func (t TraceTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", SessionTraceToolName, err)
	}
	id := t.tools.targetID(in.SessionID)
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
		"seq":        map[string]any{"type": "integer", "minimum": 1, "description": "target event sequence"},
	}, "required": []string{"seq"}}
}
func (t EventTraceTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		SessionID string `json:"session_id"`
		Seq       uint64 `json:"seq"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", EventTraceToolName, err)
	}
	id := t.tools.targetID(in.SessionID)
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
	var replacedBy uint64
	for _, ev := range events {
		var op struct {
			SurfaceOp *struct {
				Op    string `json:"op"`
				Start int64  `json:"start"`
				End   int64  `json:"end"`
			} `json:"surfaceOp"`
		}
		if json.Unmarshal(ev.Data, &op) == nil && op.SurfaceOp != nil && op.SurfaceOp.Op == "replace" {
			if in.Seq >= uint64(maxInt64(op.SurfaceOp.Start)) && in.Seq <= uint64(maxInt64(op.SurfaceOp.End)) {
				replacedBy = ev.Seq
			}
		}
	}
	_ = replacedBy // retained for compatibility with the original replacement scan
	relations := eventRelations(events, target)
	return fmt.Sprintf("Session %s\nTarget: seq %d | %s | %s\nReplaced by: %s\nEvents replaced by target: %s\nEvents cited directly as sources: %s\nDirect derived events: %s", id, events[target].Seq, events[target].Type, events[target].At.UTC().Format(time.RFC3339), seqOrNone(relations.replacedBy), replacementRange(events[target]), seqListOrNone(relations.sources), seqListOrNone(relations.derived)), nil
}

type EventReadTool struct{ tools *Tools }

func (EventReadTool) Name() string { return EventReadToolName }
func (EventReadTool) Description() string {
	return "read one full local event and bounded neighboring event summaries"
}
func (EventReadTool) Schema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"session_id": map[string]any{"type": "string", "description": "target session; defaults to the current session"},
		"seq":        map[string]any{"type": "integer", "minimum": 1, "description": "target event sequence"},
		"before":     map[string]any{"type": "integer", "minimum": 0, "maximum": MaxEventWindow, "description": "preceding events to summarize"},
		"after":      map[string]any{"type": "integer", "minimum": 0, "maximum": MaxEventWindow, "description": "following events to summarize"},
	}, "required": []string{"seq"}}
}
func (t EventReadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		SessionID     string `json:"session_id"`
		Seq           uint64 `json:"seq"`
		Before, After int
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("%s: %w", EventReadToolName, err)
	}
	id := t.tools.targetID(in.SessionID)
	if id == "" {
		return "", fmt.Errorf("%s: session_id is required when there is no current session", EventReadToolName)
	}
	if _, err := t.tools.authorizeTarget(ctx, id); err != nil {
		return "", fmt.Errorf("%s: %w", EventReadToolName, err)
	}
	if in.Seq == 0 {
		return "", fmt.Errorf("%s: seq must be positive", EventReadToolName)
	}
	if in.Before < 0 || in.After < 0 || in.Before > MaxEventWindow || in.After > MaxEventWindow {
		return "", fmt.Errorf("%s: before/after must be between 0 and %d", EventReadToolName, MaxEventWindow)
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
	lines := []string{fmt.Sprintf("Session %s", id), fmt.Sprintf("Target event seq %d:", in.Seq), "```json", string(raw), "```"}
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

func (t *Tools) currentID() string {
	if t.current == nil {
		return ""
	}
	return strings.TrimSpace(t.current())
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

func (t *Tools) targetID(id string) string {
	if strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return t.currentID()
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
func replacementRange(ev session.Event) string {
	var op struct {
		SurfaceOp *struct {
			Op    string `json:"op"`
			Start int64  `json:"start"`
			End   int64  `json:"end"`
		} `json:"surfaceOp"`
	}
	if json.Unmarshal(ev.Data, &op) != nil || op.SurfaceOp == nil || op.SurfaceOp.Op != "replace" {
		return "none"
	}
	return fmt.Sprintf("%d-%d", op.SurfaceOp.Start, op.SurfaceOp.End)
}

type eventRelationSet struct {
	replacedBy uint64
	sources    []uint64
	derived    []uint64
}

func replacementMarker(ev session.Event) (int64, int64, bool) {
	var op struct {
		SurfaceOp *struct {
			Op    string `json:"op"`
			Start int64  `json:"start"`
			End   int64  `json:"end"`
		} `json:"surfaceOp"`
	}
	if json.Unmarshal(ev.Data, &op) != nil || op.SurfaceOp == nil || op.SurfaceOp.Op != "replace" || op.SurfaceOp.Start < 1 || op.SurfaceOp.End < op.SurfaceOp.Start {
		return 0, 0, false
	}
	return op.SurfaceOp.Start, op.SurfaceOp.End, true
}

// eventRelations derives local source/derived edges from the append-only
// event vocabulary. Surface replacement ranges and tool call ids already
// carry enough correlation data, so this does not add a new event type.
func eventRelations(events []session.Event, target int) eventRelationSet {
	result := eventRelationSet{}
	targetSeq := events[target].Seq
	for _, ev := range events {
		if ev.Seq == targetSeq {
			continue
		}
		start, end, ok := replacementMarker(ev)
		if ok && ev.Seq > targetSeq && targetSeq >= uint64(start) && targetSeq <= uint64(end) && (result.replacedBy == 0 || ev.Seq < result.replacedBy) {
			result.replacedBy = ev.Seq
		}
	}
	if start, end, ok := replacementMarker(events[target]); ok {
		for _, ev := range events {
			if ev.Seq >= uint64(start) && ev.Seq <= uint64(end) && ev.Seq != targetSeq {
				result.sources = append(result.sources, ev.Seq)
			}
		}
	}
	targetCall, targetAssistantCalls := eventCallRefs(events[target])
	for i, ev := range events {
		if i == target {
			continue
		}
		call, assistantCalls := eventCallRefs(ev)
		if targetCall != "" && (events[target].Type == session.EventToolResult || events[target].Type == session.EventToolError) && ev.Type == session.EventToolStart && call == targetCall {
			result.sources = append(result.sources, ev.Seq)
		}
		if targetCall != "" && events[target].Type == session.EventToolStart && ev.Type == session.EventAssistantMessage && containsString(assistantCalls, targetCall) {
			result.sources = append(result.sources, ev.Seq)
		}
		if targetCall != "" && events[target].Type == session.EventToolStart && (ev.Type == session.EventToolResult || ev.Type == session.EventToolError) && call == targetCall {
			result.derived = append(result.derived, ev.Seq)
		}
		if len(targetAssistantCalls) > 0 && ev.Type == session.EventToolStart && containsString(targetAssistantCalls, call) {
			result.derived = append(result.derived, ev.Seq)
		}
	}
	result.sources = uniqueSeqs(result.sources)
	result.derived = uniqueSeqs(result.derived)
	return result
}

func eventCallRefs(ev session.Event) (string, []string) {
	if ev.Type == session.EventToolStart || ev.Type == session.EventToolResult || ev.Type == session.EventToolError {
		var v struct {
			CallID string `json:"callId"`
		}
		_ = json.Unmarshal(ev.Data, &v)
		return v.CallID, nil
	}
	if ev.Type == session.EventAssistantMessage {
		var v struct {
			ToolCalls []struct {
				ID string `json:"ID"`
			} `json:"toolCalls"`
		}
		_ = json.Unmarshal(ev.Data, &v)
		ids := make([]string, 0, len(v.ToolCalls))
		for _, call := range v.ToolCalls {
			if call.ID != "" {
				ids = append(ids, call.ID)
			}
		}
		return "", ids
	}
	return "", nil
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func uniqueSeqs(values []uint64) []uint64 {
	if len(values) < 2 {
		return values
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
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
func maxInt64(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// Keep deterministic ordering available to callers that use the package
// service directly rather than the SQLite SearchSessions implementation.
func sortEvents(events []session.Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
}
