// Tools.go is the Consumer half of the kb seam (design.md §8 Consumer / D2/D9,
// dispatch-m4b §1): kb_search, kb_read and kb_add are registered into the
// tools.Registry by the composition root (cmd/pa) when kb.enabled, and are
// auto-whitelisted by config.applyDefaults the same way run_command is. They
// implement the tools.Tool method set structurally (Go structural typing), so
// this package never imports the tools package — the seam stays decoupled.
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below before this code runs, so
// the tools only ever unmarshal already-valid arguments.
package kb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"
)

// Tool names (whitelisted when kb.enabled; see config.kbToolNames).
const (
	toolSearchName = "kb_search"
	toolReadName   = "kb_read"
	toolAddName    = "kb_add"
)

// CatalogText is the lightweight knowledge-base catalog injected into the
// system prompt's knowledge section when kb is enabled and kb.catalog is true
// (dispatch-m4b §2, design.md §7). It carries only the KB's name/description
// and when to use the tools — never entry bodies — mirroring dsh-knowledge's
// formatMountCatalog for our single global knowledge base.
func CatalogText() string {
	return "Knowledge base enabled (personal-knowledge, SQLite FTS5 full-text search). " +
		"Before answering from memory about personal preferences, facts, decisions, " +
		"procedures, or lessons, use kb_search to retrieve related entries; use kb_read " +
		"to open a full entry; when the user explicitly asks to remember/record/save " +
		"something, use kb_add to write it explicitly."
}

// KBSearchTool searches the knowledge base and returns ranked entry snippets
// plus source and score (read-only). Every result carries its id so the model
// can open the full entry with kb_read.
type KBSearchTool struct {
	kb   KB
	topK int // default result count when the limit argument is absent (kb.top_k)
}

// NewSearchTool returns a kb_search tool bound to a KB provider. topK is the
// default result count (<=0 falls back to DefaultTopK).
func NewSearchTool(k KB, topK int) KBSearchTool {
	if topK <= 0 {
		topK = DefaultTopK
	}
	return KBSearchTool{kb: k, topK: topK}
}

func (KBSearchTool) Name() string { return toolSearchName }

func (KBSearchTool) Description() string {
	return "search the personal knowledge base (read-only); returns ranked snippets with source and score"
}

func (KBSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "what to find; prefer focused words from the current request",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     MaxTopK,
				"description": "maximum ranked results (default from kb.top_k)",
			},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}

func (t KBSearchTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("kb_search: %w", err)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = t.topK
	}
	hits, err := t.kb.Search(ctx, a.Query, SearchOpts{TopK: limit})
	if err != nil {
		return "", fmt.Errorf("kb_search: %w", err)
	}
	return formatSearchResults(a.Query, hits), nil
}

// KBReadTool returns one full knowledge entry by its exact id (read-only).
type KBReadTool struct {
	kb KB
}

// NewReadTool returns a kb_read tool bound to a KB provider.
func NewReadTool(k KB) KBReadTool { return KBReadTool{kb: k} }

func (KBReadTool) Name() string { return toolReadName }

func (KBReadTool) Description() string {
	return "read one full knowledge entry by its exact id (read-only)"
}

func (KBReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "exact entry id returned by kb_search or a kb/recall; never reconstruct one",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t KBReadTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("kb_read: %w", err)
	}
	e, err := t.kb.Get(ctx, a.ID)
	if err != nil {
		return "", fmt.Errorf("kb_read: %w", err)
	}
	return formatEntry(e), nil
}

// KBAddTool explicitly writes one knowledge entry (dispatch-m4b §1, write,
// source="manual"). Each call adds a distinct entry: the generated unique
// manual source avoids Add's same-source update folding unrelated notes into a
// single versioned row.
type KBAddTool struct {
	kb      KB
	onAdded func(Entry) // optional hook; cmd/pa wires it to log the kb/add event (D3)
}

// NewAddTool returns a kb_add tool bound to a KB provider. onAdded, when
// non-nil, is called with the assigned entry after a successful write.
func NewAddTool(k KB, onAdded func(Entry)) KBAddTool {
	return KBAddTool{kb: k, onAdded: onAdded}
}

func (KBAddTool) Name() string { return toolAddName }

func (KBAddTool) Description() string {
	return "explicitly write one knowledge entry (title, body, type, optional tags); each call adds a distinct manual entry"
}

func (KBAddTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   200,
				"description": "entry title",
			},
			"body": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   50000,
				"description": "entry body",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{TypePreference, TypeFact, TypeDecision, TypeProcedure, TypeLesson},
				"description": "knowledge type",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    32,
				"description": "optional tags",
			},
		},
		"required":             []string{"title", "body", "type"},
		"additionalProperties": false,
	}
}

func (t KBAddTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Title string   `json:"title"`
		Body  string   `json:"body"`
		Type  string   `json:"type"`
		Tags  []string `json:"tags"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("kb_add: %w", err)
	}
	source, err := manualSource()
	if err != nil {
		return "", fmt.Errorf("kb_add: %w", err)
	}
	e, err := t.kb.Add(ctx, Entry{
		Title:      a.Title,
		Body:       a.Body,
		Type:       a.Type,
		Tags:       a.Tags,
		Source:     source,
		Confidence: 1.0, // an explicit manual write is high-confidence
	})
	if err != nil {
		return "", fmt.Errorf("kb_add: %w", err)
	}
	if t.onAdded != nil {
		t.onAdded(e)
	}
	return fmt.Sprintf("added knowledge entry %s (version %d); open it with kb_read", e.ID, e.Version), nil
}

// manualSource returns a unique source marking an explicit manual write
// (dispatch-m4b §1: kb_add writes with source "manual"). The random suffix
// keeps every explicit add a distinct entry: Add folds drafts that share a
// Source into one versioned entry, which would collapse unrelated notes into a
// single row if they all carried the literal source "manual".
func manualSource() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "manual:" + hex.EncodeToString(b[:]), nil
}

// formatSearchResults renders kb_search output: ranked title + metadata +
// bounded snippet + id. The result must be self-contained so the model can
// open entries with kb_read.
func formatSearchResults(query string, hits []Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No matches for %q in the knowledge base. Try different or broader terms.", query)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d ranked result(s) for %q:", len(hits), query)
	for i, h := range hits {
		e := h.Entry
		fmt.Fprintf(&sb, "\n\n[%d] %s (score=%.3f, type=%s", i+1, e.Title, h.Score, e.Type)
		if e.Source != "" {
			fmt.Fprintf(&sb, ", source=%s", e.Source)
		}
		if len(e.Tags) > 0 {
			fmt.Fprintf(&sb, ", tags=%s", strings.Join(e.Tags, ","))
		}
		sb.WriteString(")")
		sb.WriteString("\n  " + Snippet(e.Body))
		fmt.Fprintf(&sb, "\n  id: %s", e.ID)
	}
	sb.WriteString("\n\nUse kb_read with an entry id to open the full entry.")
	return sb.String()
}

// formatEntry renders kb_read output: the full entry with its metadata.
func formatEntry(e Entry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n", e.Title)
	fmt.Fprintf(&sb, "type=%s · scope=%s · source=%s · confidence=%.2f · version=%d\n",
		e.Type, orGlobal(e.Scope), orNone(e.Source), e.Confidence, e.Version)
	if len(e.Tags) > 0 {
		fmt.Fprintf(&sb, "tags: %s\n", strings.Join(e.Tags, ", "))
	}
	fmt.Fprintf(&sb, "id: %s\n\n%s\n", e.ID, e.Body)
	return sb.String()
}

// Snippet returns a bounded, whitespace-compacted body fragment for search
// results and recall hits (dsh-knowledge compact + slice). Snippets keep the
// model context and the kb/recall log lean (design.md §8 "有界摘要"); both the
// tools and the cmd/pa recall orchestration share it so the bound never drifts.
func Snippet(body string) string {
	compact := strings.Join(strings.Fields(body), " ")
	runes := []rune(compact)
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return compact
}

func orGlobal(s string) string {
	if s == "" {
		return "global"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
