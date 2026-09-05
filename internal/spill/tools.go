// tools.go — the M6c-2 Consumer half of the spill seam (design.md §8 Consumer /
// D2, dispatch-m6c-2 §3): spill_write, spill_recall, spill_list and
// spill_delete are registered into the tools.Registry by the composition root
// (cmd/sta) when spill.enabled, and auto-whitelisted by config.applyDefaults
// the same way the job_*/subagent_*/skill_*/schedule_*/plan_* tools are. They
// implement the tools.Tool method set structurally (Go structural typing), so
// this package never imports the tools package — the seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false) before this code runs; the empty-content / unknown-id checks are
// repeated here so a direct call can never bypass them.
//
// D3 event logging follows the M5a tool-layer decision (ADR 决策 ① 实施说明 /
// dispatch-m6c-2 §3): spill_write emits spill/write on a successful store,
// spill_recall emits spill/recall, spill_list emits spill/list, spill_delete
// emits spill/delete — all through the injected onEvent sink (the composition
// root wires it to the session log), and each append happens inside a tool
// Execute — the serial main-loop path (D5). The auto-sedimentation spill/write
// events are emitted by the wiring layer's turn-completion path (see cmd/sta),
// not by a tool.
package spill

import (
	"context"
	"fmt"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// Tool names (whitelisted when spill.enabled; see config.spillToolNames).
const (
	ToolWriteName  = "spill_write"
	ToolRecallName = "spill_recall"
	ToolListName   = "spill_list"
	ToolDeleteName = "spill_delete"
)

// SpillTools bundles the shared state of the four spill_* tools: the Engine
// service and the event sink.
type SpillTools struct {
	e       Engine
	onEvent func(typ string, data any)
}

// NewSpillTools returns the shared spill-tool bundle bound to an Engine.
// onEvent, when non-nil, receives the spill/* event payloads; the composition
// root wires it to the session log (D3).
func NewSpillTools(e Engine, onEvent func(typ string, data any)) *SpillTools {
	return &SpillTools{e: e, onEvent: onEvent}
}

// Write returns the spill_write tool.
func (t *SpillTools) Write() SpillWriteTool { return SpillWriteTool{t: t} }

// Recall returns the spill_recall tool.
func (t *SpillTools) Recall() SpillRecallTool { return SpillRecallTool{t: t} }

// List returns the spill_list tool.
func (t *SpillTools) List() SpillListTool { return SpillListTool{t: t} }

// Delete returns the spill_delete tool.
func (t *SpillTools) Delete() SpillDeleteTool { return SpillDeleteTool{t: t} }

// emit forwards one spill/* event payload to the injected sink (D3).
func (t *SpillTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *SpillTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		return runtime.Emit(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// formatMemos renders a memo list as model-facing text: one
// "- <id>: <content> (<source>)" line per memo.
func formatMemos(memos []Memo) string {
	if len(memos) == 0 {
		return "no memories"
	}
	var sb strings.Builder
	for _, m := range memos {
		fmt.Fprintf(&sb, "- %s: %s (%s)\n", m.ID, m.Content, m.Source)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// SpillWriteTool explicitly writes one conversation-derived memory and returns
// its memo id. The same content is deduplicated (content-hash idempotence), so
// re-writing an existing memory returns it unchanged.
type SpillWriteTool struct {
	t *SpillTools
}

func (SpillWriteTool) Name() string { return ToolWriteName }

func (SpillWriteTool) Description() string {
	return "explicitly write one conversation-derived memory to long-term memory and return its memo id; the same content is deduplicated (idempotent), observe with spill_recall / spill_list, remove with spill_delete"
}

func (SpillWriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the memory text to store (a durable fact derived from the conversation)",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "optional provenance label (e.g. \"session:<seq>\" or a free note); defaults to the tool name when empty",
			},
		},
		"required":             []string{"content"},
		"additionalProperties": false,
	}
}

func (t SpillWriteTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Content string `json:"content"`
		Source  string `json:"source"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("spill_write: %w", err)
	}
	if strings.TrimSpace(a.Content) == "" {
		return "", fmt.Errorf("spill_write: empty content")
	}
	if strings.TrimSpace(a.Source) == "" {
		a.Source = ToolWriteName
	}
	m, err := t.t.e.Spill(ctx, a.Content, a.Source)
	if err != nil {
		return "", fmt.Errorf("spill_write: %w", err)
	}
	// spill/write is a log-only fact (D3); the memo id + bounded content
	// summary are logged, and the returned text is what the loop logs as
	// tool/result.
	if err := t.t.emitContext(ctx, session.EventSpillWrite, session.NewSpillWrite(m.ID, m.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("spilled memo %s", m.ID), nil
}

// SpillRecallTool recalls memories whose content matches a query (v1:
// case-insensitive substring) and returns them as text.
type SpillRecallTool struct {
	t *SpillTools
}

func (SpillRecallTool) Name() string { return ToolRecallName }

func (SpillRecallTool) Description() string {
	return "recall memories whose content matches a query (case-insensitive substring) and return the matching memos (id, content, source)"
}

func (SpillRecallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "search text matched against memory contents",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "max memories to return (0/absent = the default 5)",
			},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}

func (t SpillRecallTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("spill_recall: %w", err)
	}
	hits, err := t.t.e.Recall(ctx, a.Query, a.Limit)
	if err != nil {
		return "", fmt.Errorf("spill_recall: %w", err)
	}
	// spill/recall is a log-only fact (D3) carrying the query and hit count.
	if err := t.t.emitContext(ctx, session.EventSpillRecall, session.NewSpillRecall(a.Query, len(hits))); err != nil {
		return "", err
	}
	return formatMemos(hits), nil
}

// SpillListTool returns the current memo table as text.
type SpillListTool struct {
	t *SpillTools
}

func (SpillListTool) Name() string { return ToolListName }

func (SpillListTool) Description() string {
	return "list every current memory (id, content, source)"
}

func (SpillListTool) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

func (t SpillListTool) Execute(ctx context.Context, args any) (string, error) {
	all, err := t.t.e.List(ctx)
	if err != nil {
		return "", fmt.Errorf("spill_list: %w", err)
	}
	// spill/list is a log-only fact (D3) carrying the returned table size.
	if err := t.t.emitContext(ctx, session.EventSpillList, session.NewSpillList(len(all))); err != nil {
		return "", err
	}
	return formatMemos(all), nil
}

// SpillDeleteTool removes one memo; an unknown id is rejected.
type SpillDeleteTool struct {
	t *SpillTools
}

func (SpillDeleteTool) Name() string { return ToolDeleteName }

func (SpillDeleteTool) Description() string {
	return "remove one memory by its memo id (returned by spill_write / spill_recall / spill_list)"
}

func (SpillDeleteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the memo id returned by spill_write / spill_recall / spill_list",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t SpillDeleteTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("spill_delete: %w", err)
	}
	if err := t.t.e.Remove(ctx, a.ID); err != nil {
		return "", fmt.Errorf("spill_delete: %w", err)
	}
	// spill/delete is a log-only fact (D3).
	if err := t.t.emitContext(ctx, session.EventSpillDelete, session.NewSpillDelete(a.ID)); err != nil {
		return "", err
	}
	return "deleted memo " + a.ID, nil
}
