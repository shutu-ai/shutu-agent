// Package session implements the append-only event log that is the single
// source of truth for a conversation (D1). Model-visible history is always
// derived from the log (DeriveHistory); it is never stored separately.
package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

const EventScheduleChange = "schedule/change"

// Event type discriminators (v1 vocabulary, see design.md §3).
const (
	EventTurnStart          = "turn/start"
	EventTurnEnd            = "turn/end"
	EventStepStart          = "step/start"
	EventStepEnd            = "step/end"
	EventLLMRequestStart    = "llm/request_start"
	EventLLMRequestEnd      = "llm/request_end"
	EventLLMRetry           = "llm/retry"
	EventUserMessage        = "user/message"
	EventAssistantChunk     = "assistant/chunk"
	EventAssistantReasoning = "assistant/reasoning" // M8: one streamed reasoning delta
	EventAssistantMessage   = "assistant/message"
	// EventToolCall is the dsh-compatible durable call event. EventToolStart is
	// kept as a source-compatibility alias for callers written against the old
	// shutu vocabulary; new events are still emitted as tool/call.
	EventToolCall   = "tool/call"
	EventToolStart  = EventToolCall
	EventToolResult = "tool/result"
	// EventToolError aliases tool/result for source compatibility. The literal
	// "tool/error" remains readable below for pre-alignment logs, but is never
	// emitted by the current loop.
	EventToolError        = EventToolResult
	EventFeedbackRecord   = "feedback/record"    // dsh /feedback; log-only
	EventWebCommandResult = "web/command-result" // Web-only command acknowledgement

	// M4 knowledge-base events (design.md §3): kb/recall lands with the M4a
	// kernel so the D3 logging mechanism exists before any orchestration;
	// kb/add arrives with M4b (explicit writes) and kb/extract with M4c
	// (post-answer extraction writeback). DeriveHistory ignores these types
	// (the recall is injected into context by the caller, design.md §8; the
	// extraction outcome is a log fact, not conversation), so adding them never
	// changes the turn/step structure (D4).
	EventKBRecall  = "kb/recall"
	EventKBAdd     = "kb/add"
	EventKBExtract = "kb/extract"

	// M5a background-job events (design.md §3 / ADR 2026-08-18-m5-agent-core.md
	// 决策 ① / dispatch-m5a-2): job/start lands when a job registers
	// successfully, job/status on a non-terminal transition (e.g.
	// running→stopping), job/done on a terminal settle. They are log-only
	// (D3): the model sees job state/output through the job_* tools' tool/result
	// events, and DeriveHistory treats these types as opaque data, so adding
	// them never changes the turn/step structure (D4). The payloads are pure
	// data projections — the session package never imports the jobs package.
	EventJobStart  = "job/start"
	EventJobStatus = "job/status"
	EventJobDone   = "job/done"

	// M9 persistent-terminal events (design.md §3 / ADR 2026-08-20-m9-terminal.md
	// / dispatch-m9-2 §3): terminal/start lands when a session starts,
	// terminal/stop when it is closed. They are log-only (D3): the model sees
	// session output through the terminal_* tools' tool/result events, and
	// DeriveHistory treats these types as opaque data, so adding them never
	// changes the turn/step structure (D4). The payloads are pure data
	// projections — the session package never imports the terminal package.
	EventTerminalStart = "terminal/start"
	EventTerminalStop  = "terminal/stop"

	// M-eval evaluation events (design.md §3 / ADR 2026-08-20-eval-seam.md
	// D-EVAL-5): eval/run lands when an evaluation completes. It is log-only
	// (D3): the model sees the deliverable and verdict through the eval_* tools'
	// tool/result events, and DeriveHistory treats these types as opaque data,
	// so adding them never changes the turn/step structure (D4). The payload is
	// a lean summary — never the deliverable output (D-EVAL-5).
	EventEvalRun = "eval/run"

	// GAP-2 ralph events (design.md §3 / ADR 2026-08-20-standard-gaps.md
	// D-GAP-3 / dispatch-gap-2 §4): ralph/run lands when the ralph loop settles
	// (done / blocked / round-limit). It is log-only (D3): the model sees the
	// final report through the ralph tool's tool/result event, and DeriveHistory
	// treats this type as opaque data, so adding it never changes the turn/step
	// structure (D4). The payload is a lean summary — the objective and the
	// outcome markers — never the worker outputs.
	EventRalphRun = "ralph/run"

	// GAP-3 workflow events (design.md §3 / ADR 2026-08-20-standard-gaps.md
	// D-GAP-2 / dispatch-gap-3 §4): workflow/run lands when a workflow DAG
	// settles (every task completed or failed). It is log-only (D3): the model
	// sees the per-task reports through the workflow_run tool's tool/result
	// event, and DeriveHistory treats this type as opaque data, so adding it
	// never changes the turn/step structure (D4). The payload is a lean summary
	// — the task/completed/failed counts — never the task outputs.
	EventWorkflowRun        = "workflow/run"
	EventWorkflowStart      = "workflow/start"
	EventWorkflowPhase      = "workflow/phase"
	EventWorkflowLog        = "workflow/log"
	EventWorkflowAgentStart = "workflow/agent-start"
	EventWorkflowAgentEnd   = "workflow/agent-end"
	EventWorkflowEnd        = "workflow/end"

	// M5b subagent events (design.md §3 / ADR 2026-08-18-m5-agent-core.md
	// 决策 ② / dispatch-m5b-2 §1): subagent/start lands when a delegation
	// registers successfully, subagent/end when a child settles (observed on
	// the serial tool path, exactly once per child), subagent/report for an
	// explicit child→parent report. They are log-only (D3): the model sees
	// subagent state/output through the subagent_* tools' tool/result events,
	// and DeriveHistory treats these types as opaque data, so adding them
	// never changes the turn/step structure (D4). The payloads are pure data
	// projections — the session package never imports the subagent package.
	EventSubagentStart  = "subagent/start"
	EventSubagentEnd    = "subagent/end"
	EventSubagentReport = "subagent/report"

	// Goal-round driver events (dsh goal-round-driver): opaque lifecycle facts
	// for same-session continuation. The prompt itself remains a user/message.
	EventGoalRoundStart = "goal/round_start"
	EventGoalRoundEnd   = "goal/round_end"

	// M5c compaction events (design.md §3 / ADR 2026-08-18-m5-agent-core.md
	// 决策 ③ / dispatch-m5c-2 §1): compaction/start lands when a compaction
	// attempt begins (with its reason/trigger), compaction/summary records the
	// generated summary (bounded 200 runes) when it lands, compaction/end when
	// the attempt completes (with the shadowed surface range and tokens saved),
	// compaction/prune when a tool-result prune settles. They are log-only
	// observation events (D3): the summary itself is a user/message carrying
	// surfaceOp.replace (M5c-1a) that shadows the old surface range, and these
	// events record that fact; DeriveHistory treats them as opaque data, so
	// adding them never changes the turn/step structure (D4). The payloads are
	// pure data projections — the session package never imports the compaction
	// package.
	EventCompactionStart   = "compaction/start"
	EventCompactionSummary = "compaction/summary"
	EventCompactionEnd     = "compaction/end"
	EventCompactionPrune   = "compaction/prune"

	// M5d-2 skill events (design.md §3 / ADR 2026-08-18-m5-agent-core.md
	// 决策 ④ / dispatch-m5d-2): skill/catalog lands when the composition root
	// injects the skill catalog (sorted name + description, bounded) as
	// pre-step context, carrying the entry count and a catalog version
	// (a digest over the catalog, so consumers can detect catalog drift);
	// skill/load lands when skill_load loads a skill body for the model,
	// carrying the skill name/source plus a bounded body summary. They are
	// log-only (D3): the model sees the catalog through the pre-step injected
	// message and the body through skill_load's tool/result, and DeriveHistory
	// treats these types as opaque data, so adding them never changes the
	// turn/step structure (D4). The payloads are pure data projections — the
	// session package never imports the skill package.
	EventSkillCatalog = "skill/catalog"
	EventSkillLoad    = "skill/load"

	// M6a-2 schedule events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6a / dispatch-m6a-2 §1): schedule/create lands when schedule_create
	// stores a trigger, schedule/list when schedule_list returns the table,
	// schedule/delete when schedule_delete removes one, schedule/fire when the
	// serial pre-step path advances the schedule clock and a trigger is due.
	// They are log-only (D3): the model sees the schedule table through the
	// schedule_* tools' tool/result events and any fired payload through the
	// enqueued job, and DeriveHistory treats these types as opaque data, so
	// adding them never changes the turn/step structure (D4). The payloads are
	// pure data projections — the session package never imports the schedule
	// package.
	EventScheduleCreate = "schedule/create"
	EventScheduleList   = "schedule/list"
	EventScheduleDelete = "schedule/delete"
	EventScheduleFire   = "schedule/fire"

	// M6b-2 plan events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6b / dispatch-m6b-2 §1): plan/create lands when plan_goal /
	// plan_plan / plan_todo store a goal/plan/todo (scope tells which tree
	// level), plan/status when plan_status advances or blocks one record,
	// plan/delete when plan_remove deletes one, plan/list when plan_list
	// returns the aggregation tree. plan/update is reserved vocabulary for a
	// future plan-editing tool (M6b-2 ships no editing tool). They are log-only
	// (D3): the model sees the plan tree through the plan_* tools' tool/result
	// events, and DeriveHistory treats these types as opaque data, so adding
	// them never changes the turn/step structure (D4). The payloads are pure
	// data projections — the session package never imports the plan package.
	EventPlanCreate = "plan/create"
	EventPlanUpdate = "plan/update"
	EventPlanDelete = "plan/delete"
	EventPlanStatus = "plan/status"
	EventPlanList   = "plan/list"
	EventPlanMode   = "plan/mode"

	// M6c-2 spill events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6c / dispatch-m6c-2 §1): spill/write lands when spill_write stores
	// an explicit memo or the auto-sedimentation path spills a new one
	// (carrying the memo id + a bounded content summary), spill/recall when
	// spill_recall returns its hits (query + count), spill/list when spill_list
	// returns the table (count), spill/delete when spill_delete removes one
	// (id). They are log-only (D3): the model sees the memo bodies through the
	// spill_* tools' tool/result events, and DeriveHistory treats these types
	// as opaque data, so adding them never changes the turn/step structure
	// (D4). The payloads are pure data projections — the session package never
	// imports the spill package.
	EventSpillWrite  = "spill/write"
	EventSpillRecall = "spill/recall"
	EventSpillList   = "spill/list"
	EventSpillDelete = "spill/delete"

	// M6d-2 interact events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6d / dispatch-m6d-2 §1): interact/request lands when a request is
	// created (by interact_ask or by the sensitive-tool gate), interact/resolve
	// when a user decision (approved/rejected) is recorded, interact/deny when
	// the sensitive-tool gate blocks a tool's execution after a rejection, and
	// interact/status when interact_status reports a request's current status.
	// They are log-only (D3): the model sees request/status through the
	// interact_* tools' tool/result events, and DeriveHistory treats these types
	// as opaque data, so adding them never changes the turn/step structure (D4).
	// The payloads are pure data projections — the session package never imports
	// the interact package.
	EventInteractRequest = "interact/request"
	EventInteractResolve = "interact/resolve"
	EventInteractCancel  = "interact/cancel"
	EventInteractDeny    = "interact/deny"
	EventInteractStatus  = "interact/status"

	// M6e-2 code-sandbox events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6e / dispatch-m6e-2 §1): code/run lands when run_code completes a
	// sandbox execution (a run that happened — zero or non-zero exit, with or
	// without a timeout/truncation marker; a run that failed to happen at all
	// surfaces as tool/error instead). It is log-only (D3): the model sees the
	// run outcome through run_code's tool/result, and DeriveHistory treats this
	// type as opaque data, so adding it never changes the turn/step structure
	// (D4). The payload is a pure data projection — the session package never
	// imports the code package.
	EventCodeRun = "code/run"

	// M6f-2 mcp events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6f / dispatch-m6f-2 §1): mcp/list lands when mcp_list lists a
	// configured server's tools (carrying the count), mcp/call when mcp_call
	// invokes one (carrying the tool name and whether the server reported a
	// tool-level failure inside a successful result). They are log-only (D3):
	// the model sees the tool table through mcp_list's tool/result and the call
	// outcome through mcp_call's tool/result, and DeriveHistory treats these
	// types as opaque data, so adding them never changes the turn/step
	// structure (D4). The payloads are pure data projections — the session
	// package never imports the mcp package.
	EventMcpList = "mcp/list"
	EventMcpCall = "mcp/call"

	// M6f-3 fs events (design.md §3 / ADR 2026-08-19-m6-agent-full.md
	// 决策 M6f / dispatch-m6f-3 §2): fs/read lands when read returns a
	// file (carrying the path and the returned byte size), fs/write when
	// write creates or overwrites a file (path), fs/list when list
	// lists a directory (dir + entry count). They are log-only (D3): the
	// model sees the file content / write outcome / listing through the fs_*
	// tools' tool/result events, and DeriveHistory treats these types as
	// opaque data, so adding them never changes the turn/step structure (D4).
	// The payloads are pure data projections — the session package never
	// imports the fs package.
	EventFsRead  = "fs/read"
	EventFsWrite = "fs/write"
	EventFsList  = "fs/list"
)

// EventVersion is the current event vocabulary version. It is stored per event
// (design.md D8) so a future event type or payload shape never requires
// migrating old rows: readers that do not understand a version keep the row as
// opaque data and derive history only from the types they know.
const EventVersion = 1

// Event is one append-only row of the session log. Seq is monotonically
// increasing and becomes the cross-restart primary key once persisted. Version
// carries the event vocabulary version (see EventVersion); Data is an opaque
// JSON blob whose shape is owned by Type.
type Event struct {
	Seq     uint64
	Type    string
	At      time.Time
	Version int
	Data    json.RawMessage
}

// Log is an in-memory append-only event log. It is not safe for concurrent
// use: the agent loop is strictly serial (D5).
type Log struct {
	events []Event
	seq    uint64
	sink   func(Event) error // optional durable sink (D8), called after each append
}

// New returns an empty in-memory log.
func New() *Log {
	return &Log{}
}

// SetSink installs an optional durable sink that receives every committed
// event (typically forwarding it to a store). A sink error rolls the event
// back out of the in-memory log and fails the Append, so the log never drifts
// from what was actually persisted (D1: the log is the source of truth).
func (l *Log) SetSink(sink func(Event) error) {
	l.sink = sink
}

// Restore rebuilds the log from scratch with previously persisted events
// (startup replay, D8). Events must arrive in strictly increasing Seq order;
// after a successful Restore the next Append continues after the last Seq.
// Restore never invokes the sink — replaying is loading, not appending.
func (l *Log) Restore(events []Event) error {
	l.events = nil
	l.seq = 0
	var last uint64
	for _, ev := range events {
		if ev.Seq <= last {
			return fmt.Errorf("session: non-monotonic seq %d after %d in replay", ev.Seq, last)
		}
		l.events = append(l.events, ev)
		last = ev.Seq
	}
	l.seq = last
	return nil
}

// Append marshals data and appends one event, assigning the next Seq, At and
// Version. When a durable sink is installed it is called with the committed
// event; a sink error rolls the event back and is returned.
func (l *Log) Append(typ string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	l.seq++
	ev := Event{Seq: l.seq, Type: typ, At: time.Now().UTC(), Version: EventVersion, Data: raw}
	l.events = append(l.events, ev)
	if l.sink != nil {
		if err := l.sink(ev); err != nil {
			l.events = l.events[:len(l.events)-1]
			return Event{}, fmt.Errorf("session: persist %s event: %w", typ, err)
		}
	}
	return ev, nil
}

// Events returns a snapshot copy of the current event log.
func (l *Log) Events() []Event {
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// NextSeq returns the Seq the next Append will assign (current Seq + 1). M3
// uses it to name a spill file after the tool/result event that will carry the
// locator.
func (l *Log) NextSeq() uint64 { return l.seq + 1 }

// NextTurn returns the 1-based dsh turn number for the next live turn.
// shutu historically did not persist the number on turn/start, so it is
// reconstructed from the append-only lifecycle anchors.
func (l *Log) NextTurn() int {
	turn := 1
	for _, ev := range l.events {
		if ev.Type == EventTurnStart {
			turn++
		}
	}
	return turn
}

// DeriveHistory folds the log into model-visible messages (design.md §3:
// history is a pure derivation of the log). assistant/chunk rows are streaming
// fidelity records and are folded away in favor of the authoritative
// assistant/message row that closes the step.
func (l *Log) DeriveHistory() []llm.Message {
	return derive(l.events)
}

func derive(events []Event) []llm.Message {
	// tagged pairs each derived message with the Seq of the event it came from,
	// so a compaction summary marker (user/message with surfaceOp.replace, M5c)
	// can drop the messages derived from its shadowed seq range and substitute
	// the summary in their place. Without such a marker the tagged pass rebuilds
	// exactly the same []llm.Message as a plain pass (no-replace behavior is
	// unchanged).
	type tagged struct {
		msg llm.Message
		seq uint64
	}
	var out []tagged
	var skipUntil uint64 // >0: also skip events whose Seq <= skipUntil (defensive forward skip)
	for _, ev := range events {
		if skipUntil != 0 && ev.Seq <= skipUntil {
			continue
		}
		switch ev.Type {
		case EventUserMessage:
			var d userMessageData
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			if op := d.SurfaceOp; op != nil && op.Op == surfaceReplaceOp {
				// The summary substitutes the shadowed surface range [Start, End].
				// Its events precede this marker in the append-only log (seq is
				// monotonic, so the marker's own Seq is > End), so drop the messages
				// already derived from that range and put the summary where they
				// began; then keep skipping any subsequent Seq in the range until
				// Seq > End (contract M5c-1a). Only Seq comparison is used — the
				// shadowed event contents are never parsed.
				start, end := op.Start, op.End
				if start >= 0 && end >= start {
					first, last := -1, -1
					for i, t := range out {
						if s := int64(t.seq); s >= start && s <= end {
							if first < 0 {
								first = i
							}
							last = i
						}
					}
					summary := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text(d.Text)}}
					if first >= 0 {
						head := out[:first]
						tail := out[last+1:]
						rebuilt := make([]tagged, 0, len(head)+1+len(tail))
						rebuilt = append(rebuilt, head...)
						rebuilt = append(rebuilt, tagged{msg: summary, seq: ev.Seq})
						rebuilt = append(rebuilt, tail...)
						out = rebuilt
					} else {
						out = append(out, tagged{msg: summary, seq: ev.Seq})
					}
					skipUntil = uint64(end)
					continue
				}
				// malformed range (negative Start or End < Start): no shadowing,
				// keep the message as a plain user turn.
			}
			// M8: prefer the logged content blocks (M8-3 reservation); old
			// logs carry only text, which folds back into a single text block
			// (D8, old-format replay).
			content := d.Content
			if len(content) == 0 {
				content = []llm.ContentBlock{llm.Text(d.Text)}
			}
			out = append(out, tagged{msg: llm.Message{Role: llm.RoleUser, Content: content}, seq: ev.Seq})
		case EventAssistantMessage:
			var d assistantMessageData
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			// M8: reasoning is folded as a reasoning block before the text
			// block (dsh order: reasoning first, text after). Old logs carry
			// no reasoning, so they fold to a single text block (D8).
			content := make([]llm.ContentBlock, 0, 2)
			if d.Reasoning != "" {
				content = append(content, llm.ContentBlock{Kind: llm.BlockReasoning, Text: d.Reasoning})
			}
			content = append(content, llm.Text(d.Text))
			out = append(out, tagged{msg: llm.Message{
				Role:      llm.RoleAssistant,
				Content:   content,
				ToolCalls: d.ToolCalls,
			}, seq: ev.Seq})
		case EventToolResult:
			var d toolResultData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				// EventToolError is a compatibility alias for tool/result, so
				// old callers may have placed the legacy string-error payload
				// under the new event type. Preserve its history semantics.
				var legacy toolErrorData
				if json.Unmarshal(ev.Data, &legacy) == nil && legacy.CallID != "" {
					out = append(out, tagged{msg: llm.Message{
						Role:       llm.RoleTool,
						ToolCallID: legacy.CallID,
						Content:    []llm.ContentBlock{llm.Text("Error: " + legacy.Error)},
					}, seq: ev.Seq})
				}
				continue
			}
			content := d.Content
			if len(content) == 0 && d.Message != nil {
				for _, block := range d.Message.Content {
					if block.Type == "text" {
						content = append(content, llm.Text(block.Text))
					}
				}
			}
			if len(content) == 0 {
				content = []llm.ContentBlock{llm.Text(d.Output)}
			}
			callID := d.CallID
			if callID == "" && d.Message != nil {
				callID = d.Message.Source.CallID
			}
			out = append(out, tagged{msg: llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: callID,
				Content:    content,
			}, seq: ev.Seq})
		case "tool/error":
			// Pre-alignment logs used a separate tool/error envelope. Keep it
			// readable, but never emit this type for new executions.
			var d toolErrorData
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			out = append(out, tagged{msg: llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: d.CallID,
				Content:    []llm.ContentBlock{llm.Text("Error: " + d.Error)},
			}, seq: ev.Seq})
		}
	}
	msgs := make([]llm.Message, len(out))
	for i, t := range out {
		msgs[i] = t.msg
	}
	return msgs
}

// Payload structs for each v1 event type. Kept private: only the session
// package knows the on-disk shapes, and the loop builds them through the
// New* helpers below so model-visible inputs cannot be logged ad hoc.

type userMessageData struct {
	Text      string             `json:"text"`
	Content   []llm.ContentBlock `json:"content,omitempty"`   // M8-3 reservation: content blocks (images); not written this milestone
	SurfaceOp *SurfaceReplace    `json:"surfaceOp,omitempty"` // set by compaction summaries (M5c)
}

type feedbackRecordData struct {
	Text string `json:"text"`
}

type webCommandResultData struct {
	Text    string `json:"text"`
	Command string `json:"command,omitempty"`
}

type turnStartData struct{}

type turnEndData struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type stepData struct {
	Step   int    `json:"step"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

type llmRequestData struct {
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
	Effort   string          `json:"reasoningEffort,omitempty"`
	Status   string          `json:"status,omitempty"`
	Error    string          `json:"error,omitempty"`
	Usage    *llm.TokenUsage `json:"usage,omitempty"`
	Attempts int             `json:"attempts,omitempty"`
}

// NewTurnStart and NewTurnEnd provide durable lifecycle anchors for a turn.
func NewTurnStart() any { return turnStartData{} }

func NewTurnEnd(status, errText string) any {
	return turnEndData{Status: status, Error: errText}
}

// NewStepStart and NewStepEnd provide durable lifecycle anchors for a model
// request/tool step. They are opaque to history derivation.
func NewStepStart(step int) any { return stepData{Step: step} }

func NewStepEnd(step int, status, errText string) any {
	return stepData{Step: step, Status: status, Error: errText}
}

func NewLLMRequestStart(provider, model, effort string) any {
	return llmRequestData{Provider: provider, Model: model, Effort: effort}
}

func NewLLMRequestEnd(provider, model, effort, status, errText string) any {
	return llmRequestData{Provider: provider, Model: model, Effort: effort, Status: status, Error: errText}
}

func NewLLMRequestEndWithUsage(provider, model, effort, status, errText string, usage llm.TokenUsage, attempts int) any {
	var u *llm.TokenUsage
	if !usage.Empty() {
		copy := usage
		u = &copy
	}
	return llmRequestData{Provider: provider, Model: model, Effort: effort, Status: status, Error: errText, Usage: u, Attempts: attempts}
}

func NewLLMRetry(provider, model string, retry llm.RetryEvent) any {
	return struct {
		Provider   string `json:"provider,omitempty"`
		Model      string `json:"model,omitempty"`
		Attempt    int    `json:"attempt"`
		MaxRetries int    `json:"maxRetries"`
		DelayMS    int64  `json:"delayMs"`
		Error      string `json:"error,omitempty"`
	}{Provider: provider, Model: model, Attempt: retry.Attempt, MaxRetries: retry.MaxRetries, DelayMS: retry.DelayMS, Error: retry.Error}
}

// surfaceReplaceOp is the only SurfaceReplace operation currently defined: the
// user/message carries a summary that substitutes the shadowed surface range
// [Start, End] (old events stay in the log, D1; derive() folds them out).
const surfaceReplaceOp = "replace"

// SurfaceReplace marks a user/message as a compaction summary that shadows the
// surface events whose Seq falls in [Start, End] (M5c, 决策 ③). Op is
// surfaceReplaceOp ("replace"); Start/End are the first/last shadowed event
// Seq, recorded at append time (Seq is monotonic, so End < the marker's Seq).
type SurfaceReplace struct {
	Op    string `json:"op"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type assistantChunkData struct {
	Text string `json:"text"`
}

// assistantReasoningData is the assistant/reasoning payload: one streamed
// reasoning delta (the model's thinking text, M8). It is a streaming-fidelity
// record parallel to assistant/chunk; the authoritative assistant/message row
// closes the step and carries the joined reasoning, and DeriveHistory folds
// the deltas away in favor of that row.
type assistantReasoningData struct {
	Text string `json:"text"`
}

type assistantMessageData struct {
	Text         string          `json:"text"`
	Reasoning    string          `json:"reasoning,omitempty"` // assistant reasoning (M8, D3): folded to a reasoning block on derive
	ToolCalls    []llm.ToolCall  `json:"toolCalls,omitempty"`
	FinishReason string          `json:"finishReason,omitempty"`
	Interrupted  bool            `json:"interrupted,omitempty"`
	Usage        *llm.TokenUsage `json:"usage,omitempty"`
}

type toolResultData struct {
	// The first fields are the dsh wire shape. The legacy fields below remain
	// intentionally duplicated so old shutu web/replay readers can consume a
	// newly written event while they are being migrated.
	Turn            int                    `json:"turn,omitempty"`
	Step            int                    `json:"step,omitempty"`
	Message         *toolResultMessageData `json:"message,omitempty"`
	Error           *toolResultErrorData   `json:"error,omitempty"`
	Meta            any                    `json:"meta,omitempty"`
	SourceEventSeqs []uint64               `json:"sourceEventSeqs,omitempty"`

	CallID  string             `json:"callId,omitempty"`
	Name    string             `json:"name,omitempty"`
	Output  string             `json:"output,omitempty"`
	Spill   *SpillRef          `json:"spill,omitempty"`
	Code    string             `json:"code,omitempty"`
	Content []llm.ContentBlock `json:"content,omitempty"`
}

type toolCallData struct {
	Turn      int    `json:"turn"`
	Step      int    `json:"step"`
	CallID    string `json:"callId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolResultMessageData struct {
	Source  toolResultSourceData  `json:"source"`
	Content []toolResultBlockData `json:"content"`
}

type toolResultSourceData struct {
	CallID string `json:"callId"`
}

type toolResultBlockData struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"isError,omitempty"`
}

type toolResultErrorData struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// toolStartData is the tool/start payload: a tool call dispatched, before its
// execution settles. It is a streaming-fidelity record (dsh shows the running
// row the moment the call dispatches); DeriveHistory folds it away — the
// pairing with the model's tool call lives on the assistant/message row and
// the outcome on tool/result / tool/error.
type toolStartData struct {
	CallID string `json:"callId"`
	Name   string `json:"name"`
	Args   string `json:"args"`
}

// SpillRef is recorded on a tool/result event when the tool output exceeded
// the output limit and the full text was spilled to disk. The locator is
// model-visible — the model reads the full file through it — so it must be
// logged (D3). Output already carries the truncation notice with the locator;
// this structured copy is for tooling/replay.
type SpillRef struct {
	Locator string `json:"locator"`
	Bytes   int    `json:"bytes"`
}

type toolErrorData struct {
	CallID string `json:"callId"`
	Name   string `json:"name"`
	Error  string `json:"error"`
}

// NewUserMessage builds the user/message payload.
func NewUserMessage(text string) any { return userMessageData{Text: text} }

// NewFeedbackRecord builds the dsh-compatible log-only feedback payload. It is
// deliberately not a user/message, so feedback never enters model history.
func NewFeedbackRecord(text string) any { return feedbackRecordData{Text: text} }

// NewWebCommandResult builds a Web-only acknowledgement event. command is an
// optional browser-side action such as export; DeriveHistory ignores it like
// other log-only projections.
func NewWebCommandResult(text string, command ...string) any {
	data := webCommandResultData{Text: text}
	if len(command) > 0 {
		data.Command = command[0]
	}
	return data
}

// NewUserMessageWithBlocks builds a user/message payload carrying explicit
// content blocks (M8-3, /attach; dispatch-m8-3 §4): the image attachment lands
// as an image block carrying only its ImageRef — never the image bytes (dsh
// 7078918: 落库只存引用). text is the optional accompanying plain text ("" for a
// pure /attach). derive() prefers Content blocks over Text when folding (M8-1
// reservation), so a later request replays the image ref into the model-visible
// history. Constructed in the same New* style as NewAssistantMessage.
func NewUserMessageWithBlocks(text string, blocks []llm.ContentBlock) any {
	return userMessageData{Text: text, Content: blocks}
}

// NewUserMessageReplace builds a user/message payload for a compaction summary
// (M5c, 决策 ③): it carries the summary text plus a surfaceOp.replace marker
// shadowing the surface events whose Seq is in [start, end]. derive() then
// substitutes the summary for those events. The events themselves stay in the
// log (D1, append-only). NewUserMessage remains unchanged and is what normal
// turns use; only a compaction writes this payload.
func NewUserMessageReplace(text string, start, end int64) any {
	return userMessageData{Text: text, SurfaceOp: &SurfaceReplace{Op: surfaceReplaceOp, Start: start, End: end}}
}

// NewAssistantChunk builds one assistant/chunk payload (streaming fidelity).
func NewAssistantChunk(text string) any { return assistantChunkData{Text: text} }

// NewAssistantReasoning builds one assistant/reasoning payload: a streamed
// reasoning delta (M8, D3). The model's thinking is logged as it arrives so
// the UI can show it in order (thinking before tool calls); DeriveHistory
// ignores these rows and uses the joined reasoning on assistant/message.
func NewAssistantReasoning(text string) any { return assistantReasoningData{Text: text} }

// NewAssistantMessage builds the authoritative assistant/message payload that
// closes a step. reasoning is optional (M8): when non-empty it is logged as the
// assistant's reasoning text (D3) and folded back into a reasoning block by
// DeriveHistory; plain callers may omit it and the payload stays reasoning-free
// (backward compatible with all existing call sites).
func NewAssistantMessage(text string, toolCalls []llm.ToolCall, finishReason string, reasoning ...string) any {
	var r string
	if len(reasoning) > 0 {
		r = reasoning[0]
	}
	return assistantMessageData{Text: text, ToolCalls: toolCalls, FinishReason: finishReason, Reasoning: r}
}

func NewAssistantMessageWithUsage(text string, toolCalls []llm.ToolCall, finishReason, reasoning string, usage llm.TokenUsage) any {
	var u *llm.TokenUsage
	if !usage.Empty() {
		copy := usage
		u = &copy
	}
	return assistantMessageData{Text: text, ToolCalls: toolCalls, FinishReason: finishReason, Reasoning: reasoning, Usage: u}
}

// NewInterruptedAssistantMessage closes a stream that was interrupted after
// producing partial output, preserving that output for replay/history.
func NewInterruptedAssistantMessage(text string, toolCalls []llm.ToolCall, reasoning string) any {
	return assistantMessageData{
		Text:        text,
		ToolCalls:   toolCalls,
		Reasoning:   reasoning,
		Interrupted: true,
	}
}

// NewToolStart builds the tool/start payload logged the moment a tool call
// dispatches (dsh: the row appears running before it settles).
func NewToolStart(callID, name, args string) any {
	return NewToolCall(0, 0, callID, name, args)
}

// NewToolCall builds the dsh-compatible durable invocation event.
func NewToolCall(turn, step int, callID, name, args string) any {
	return toolCallData{Turn: turn, Step: step, CallID: callID, Name: name, Arguments: args}
}

// NewToolResult builds one successful tool/result payload. spill is the
// truncation record (non-nil only when the output was spilled to disk, M3).
func NewToolResult(callID, name, output string, spill *SpillRef) any {
	return newToolResult(0, 0, callID, name, output, nil, false, spill, "")
}

// NewToolResultAt builds a dsh-compatible successful result at a turn/step.
func NewToolResultAt(turn, step int, callID, name, output string, spill *SpillRef) any {
	return newToolResult(turn, step, callID, name, output, nil, false, spill, "")
}

// NewToolResultAtWithSource attaches the tool/call event sequence that caused
// this result, matching dsh's durable sourceEventSeqs linkage.
func NewToolResultAtWithSource(turn, step int, callID, name, output string, spill *SpillRef, sourceSeq uint64) any {
	return addToolResultSource(NewToolResultAt(turn, step, callID, name, output, spill), sourceSeq)
}

// NewToolResultWithContent records a tool result carrying provider-neutral
// content blocks, such as a read_image attachment reference.
func NewToolResultWithContent(callID, name, output string, content []llm.ContentBlock) any {
	return newToolResult(0, 0, callID, name, output, content, false, nil, "")
}

// NewToolResultWithContentAt is the turn/step-aware rich result constructor.
func NewToolResultWithContentAt(turn, step int, callID, name, output string, content []llm.ContentBlock) any {
	return newToolResult(turn, step, callID, name, output, content, false, nil, "")
}

// NewToolResultWithContentAtSource is the source-linked rich-result form.
func NewToolResultWithContentAtSource(turn, step int, callID, name, output string, content []llm.ContentBlock, sourceSeq uint64) any {
	return addToolResultSource(NewToolResultWithContentAt(turn, step, callID, name, output, content), sourceSeq)
}

// NewToolErrorResultAt records a structured ToolResult whose content is
// model-visible but marked as an error, matching dsh's isError result bit.
func NewToolErrorResultAt(turn, step int, callID, name, output string, spill *SpillRef) any {
	return NewToolErrorResultAtCode(turn, step, callID, name, output, spill, "TOOL_RESULT_ERROR")
}

// NewToolErrorResultAtCode records a model-visible tool failure while keeping
// the dsh error name/code on the durable result envelope.
func NewToolErrorResultAtCode(turn, step int, callID, name, output string, spill *SpillRef, code string) any {
	result := newToolResult(turn, step, callID, name, output, nil, true, spill, code)
	result.Error = &toolResultErrorData{Name: toolErrorName(code), Code: code}
	return result
}

// NewToolErrorResultAtCodeWithSource is the source-linked form of
// NewToolErrorResultAtCode.
func NewToolErrorResultAtCodeWithSource(turn, step int, callID, name, output string, spill *SpillRef, code string, sourceSeq uint64) any {
	return addToolResultSource(NewToolErrorResultAtCode(turn, step, callID, name, output, spill, code), sourceSeq)
}

// NewToolErrorResultWithContentAt is the rich-content form of
// NewToolErrorResultAt.
func NewToolErrorResultWithContentAt(turn, step int, callID, name, output string, content []llm.ContentBlock) any {
	return NewToolErrorResultWithContentAtCode(turn, step, callID, name, output, content, "TOOL_RESULT_ERROR")
}

// NewToolErrorResultWithContentAtCode is the rich-content form of
// NewToolErrorResultAtCode.
func NewToolErrorResultWithContentAtCode(turn, step int, callID, name, output string, content []llm.ContentBlock, code string) any {
	result := newToolResult(turn, step, callID, name, output, content, true, nil, code)
	result.Error = &toolResultErrorData{Name: toolErrorName(code), Code: code}
	return result
}

// NewToolErrorResultWithContentAtCodeWithSource is the rich, source-linked
// form of NewToolErrorResultWithContentAtCode.
func NewToolErrorResultWithContentAtCodeWithSource(turn, step int, callID, name, output string, content []llm.ContentBlock, code string, sourceSeq uint64) any {
	return addToolResultSource(NewToolErrorResultWithContentAtCode(turn, step, callID, name, output, content, code), sourceSeq)
}

// NewAbortedToolResult records a tool call that was present in the assistant
// response but could not be dispatched because the turn was cancelled.
func NewAbortedToolResult(callID, name string) any {
	return NewAbortedToolResultAt(0, 0, callID, name)
}

// NewAbortedToolResultAt records the dsh synthetic error for an undispatched
// call. The caller must also append the corresponding tool/call event.
func NewAbortedToolResultAt(turn, step int, callID, name string) any {
	const code = "ABORTED_BEFORE_DISPATCH"
	return newToolResult(turn, step, callID, name, "Error: tool call aborted before dispatch", nil, true, nil, code)
}

// NewAbortedToolResultAtWithSource is the source-linked synthetic result for
// an undispatched call.
func NewAbortedToolResultAtWithSource(turn, step int, callID, name string, sourceSeq uint64) any {
	return addToolResultSource(NewAbortedToolResultAt(turn, step, callID, name), sourceSeq)
}

// NewToolError builds one failed tool/error payload.
func NewToolError(callID, name, err string) any {
	// Legacy constructor: callers that explicitly append the literal
	// "tool/error" event remain able to create the old compact payload. New
	// execution code uses NewToolErrorAt and writes the dsh result envelope.
	return toolErrorData{CallID: callID, Name: name, Error: err}
}

// NewToolErrorAt records an execution failure in dsh's result envelope.
func NewToolErrorAt(turn, step int, callID, name, err string) any {
	return NewToolErrorAtCode(turn, step, callID, name, err, "TOOL_EXECUTION_ERROR")
}

// NewToolErrorAtCode records a dsh-compatible structured failure while
// preserving the stable error code selected by the execution pipeline.
func NewToolErrorAtCode(turn, step int, callID, name, err, code string) any {
	result := newToolResult(turn, step, callID, name, "Error: "+err, nil, true, nil, code)
	result.Error = &toolResultErrorData{Name: toolErrorName(code), Code: code}
	return result
}

// NewToolErrorAtCodeWithSource is the source-linked form of NewToolErrorAtCode.
func NewToolErrorAtCodeWithSource(turn, step int, callID, name, err, code string, sourceSeq uint64) any {
	return addToolResultSource(NewToolErrorAtCode(turn, step, callID, name, err, code), sourceSeq)
}

func addToolResultSource(payload any, sourceSeq uint64) any {
	if sourceSeq == 0 {
		return payload
	}
	result, ok := payload.(toolResultData)
	if !ok {
		return payload
	}
	result.SourceEventSeqs = []uint64{sourceSeq}
	return result
}

func toolErrorName(code string) string {
	switch code {
	case "UNKNOWN_TOOL":
		return "ToolNotFoundError"
	case "INVALID_ARGS":
		return "ToolArgsError"
	case "TOOL_TIMEOUT":
		return "ToolTimeoutError"
	case "ABORTED", "ABORTED_BEFORE_DISPATCH":
		return "AbortError"
	default:
		return "ToolError"
	}
}

func newToolResult(turn, step int, callID, name, output string, content []llm.ContentBlock, isError bool, spill *SpillRef, code string) toolResultData {
	if len(content) == 0 {
		content = []llm.ContentBlock{llm.Text(output)}
	}
	blocks := make([]toolResultBlockData, 0, len(content))
	for _, block := range content {
		if block.Kind == llm.BlockText || block.Text != "" {
			blocks = append(blocks, toolResultBlockData{Type: "text", Text: block.Text, IsError: isError})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, toolResultBlockData{Type: "text", Text: output, IsError: isError})
	}
	result := toolResultData{
		Turn: turn, Step: step, CallID: callID, Name: name, Output: output,
		Spill: spill, Code: code, Content: content,
		Message: &toolResultMessageData{Source: toolResultSourceData{CallID: callID}, Content: blocks},
	}
	if isError {
		result.Error = &toolResultErrorData{Name: "ToolError", Code: code}
	}
	return result
}

// RecallHit is one knowledge-entry projection carried by a kb/recall event:
// the bounded summary the model is about to see. It is a plain data shape so
// the session package never depends on the kb package.
type RecallHit struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet,omitempty"` // bounded body fragment
	Type    string   `json:"type,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Source  string   `json:"source,omitempty"`
	Score   float64  `json:"score"`
}

type kbRecallData struct {
	Query string      `json:"query"`
	Hits  []RecallHit `json:"hits,omitempty"`
}

// NewKBRecall builds the kb/recall payload (design.md §8 / D3). M4b's recall
// orchestration calls this immediately before injecting the recall into the
// model context, so the model-visible input is durably logged. DeriveHistory
// treats it as opaque data.
func NewKBRecall(query string, hits []RecallHit) any {
	return kbRecallData{Query: query, Hits: hits}
}

// kbAddData is the kb/add payload: a bounded summary of an explicit knowledge
// write (dispatch-m4b §3). Only the summary is logged, never the full body, so
// the log stays lean. DeriveHistory treats it as opaque data.
type kbAddData struct {
	EntryID string   `json:"entryId"`
	Title   string   `json:"title"`
	Type    string   `json:"type"`
	Tags    []string `json:"tags,omitempty"`
	Source  string   `json:"source,omitempty"`
	Version int      `json:"version"`
}

// NewKBAdd builds the kb/add payload recorded when an explicit write lands
// (dispatch-m4b §3 / D3).
func NewKBAdd(entryID, title, typ string, tags []string, source string, version int) any {
	return kbAddData{EntryID: entryID, Title: title, Type: typ, Tags: tags, Source: source, Version: version}
}

// kbExtractData is the kb/extract payload: the outcome of a post-answer
// extraction job (dispatch-m4c §2 / D3). Status is one of created | skipped |
// failed; Reason explains a skip or failure; IDs carries the ids of the entries
// created by a successful run. Only the summary is logged, never the model
// output or entry bodies. DeriveHistory treats it as opaque data.
type kbExtractData struct {
	Status  string   `json:"status"` // created | skipped | failed
	Session string   `json:"session,omitempty"`
	Turn    int      `json:"turn,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	IDs     []string `json:"ids,omitempty"` // created entry ids
}

// NewKBExtract builds the kb/extract payload recorded when the post-answer
// extraction writeback finishes for one session:turn (dispatch-m4c §2 / D3).
// status is created | skipped | failed.
func NewKBExtract(status, sessionID string, turn int, reason string, ids []string) any {
	return kbExtractData{Status: status, Session: sessionID, Turn: turn, Reason: reason, IDs: ids}
}

// jobStartData is the job/start payload: the registry-issued id plus the
// registration facts (kind/label/owner). DeriveHistory treats it as opaque
// data.
type jobStartData struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	OwnerSession string `json:"ownerSession,omitempty"`
}

// jobStatusData is the job/status payload: one observed non-terminal
// transition (e.g. running→stopping) with its kind-specific detail.
// DeriveHistory treats it as opaque data.
type jobStatusData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// jobDoneData is the job/done payload: a terminal settle (completed/killed/
// failed) plus a bounded output summary. The log only ever carries the
// summary, never the full output (which the model sees through job_read's
// tool/result). DeriveHistory treats it as opaque data.
type jobDoneData struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Detail        string `json:"detail,omitempty"`
	OutputSummary string `json:"outputSummary,omitempty"`
}

// jobOutputSummaryMax bounds the job/done output summary (dispatch-m5a-2: 输出
// 只记摘要，有界), keeping the log lean regardless of what the caller passes.
const jobOutputSummaryMax = 200

// NewJobStart builds the job/start payload recorded when a job registers
// successfully (dispatch-m5a-2 §1 / D3).
func NewJobStart(id, kind, label, ownerSession string) any {
	return jobStartData{ID: id, Kind: kind, Label: label, OwnerSession: ownerSession}
}

// NewJobStatus builds the job/status payload recorded when a job's status
// transitions (e.g. running→stopping) (dispatch-m5a-2 §1 / D3).
func NewJobStatus(id, status, detail string) any {
	return jobStatusData{ID: id, Status: status, Detail: detail}
}

// NewJobDone builds the job/done payload recorded when a job settles
// terminally. output is bounded to a summary head by the constructor so the
// payload is always lean (dispatch-m5a-2 §1 / D3).
func NewJobDone(id, status, detail, output string) any {
	return jobDoneData{ID: id, Status: status, Detail: detail, OutputSummary: summaryHead(output)}
}

// terminalStartData is the terminal/start payload (dispatch-m9-2 §3).
type terminalStartData struct {
	ID    string `json:"id"`
	Owner string `json:"owner,omitempty"`
}

// terminalStopData is the terminal/stop payload (dispatch-m9-2 §3).
type terminalStopData struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// NewTerminalStart builds the terminal/start payload recorded when a
// persistent shell session starts (dispatch-m9-2 §3 / D3).
func NewTerminalStart(id, owner string) any {
	return terminalStartData{ID: id, Owner: owner}
}

// NewTerminalStop builds the terminal/stop payload recorded when a persistent
// shell session closes (dispatch-m9-2 §3 / D3).
func NewTerminalStop(id, reason string) any {
	return terminalStopData{ID: id, Reason: reason}
}

// evalRunData is the eval/run payload (D-EVAL-5): a lean summary only.
type evalRunData struct {
	ID            string `json:"id"`
	TaskID        string `json:"taskId,omitempty"`
	Verdict       string `json:"verdict"`
	Reason        string `json:"reason,omitempty"`
	EvaluatorKind string `json:"evaluatorKind,omitempty"`
	CriteriaCount int    `json:"criteriaCount"`
}

// NewEvalRun builds the eval/run payload (D-EVAL-5).
func NewEvalRun(id, taskID, verdict, reason, kind string, criteriaCount int) any {
	return evalRunData{ID: id, TaskID: taskID, Verdict: verdict, Reason: reason, EvaluatorKind: kind, CriteriaCount: criteriaCount}
}

// ralphRunData is the ralph/run payload (D-GAP-3 / dispatch-gap-2 §4): the
// immutable objective, the rounds actually spawned, and the terminal outcome
// markers (done / blocked). The full worker outputs stay in the tool/result
// event — this record is a lean log fact. DeriveHistory treats it as opaque
// data.
type ralphRunData struct {
	Objective string `json:"objective"`
	Rounds    int    `json:"rounds"`
	Done      bool   `json:"done"`
	Blocked   bool   `json:"blocked"`
}

// NewRalphRun builds the ralph/run payload recorded when the ralph loop
// settles (dispatch-gap-2 §4 / D3).
func NewRalphRun(objective string, rounds int, done, blocked bool) any {
	return ralphRunData{Objective: objective, Rounds: rounds, Done: done, Blocked: blocked}
}

// workflowRunData is the workflow/run payload (D-GAP-2 / dispatch-gap-3 §4):
// the task/completed/failed counts of one settled DAG run. The per-task
// reports stay in the workflow_run tool's tool/result event — this record is a
// lean log fact. DeriveHistory treats it as opaque data.
type workflowRunData struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// NewWorkflowRun builds the workflow/run payload recorded when a workflow DAG
// settles (dispatch-gap-3 §4 / D3).
func NewWorkflowRun(total, completed, failed int) any {
	return workflowRunData{Total: total, Completed: completed, Failed: failed}
}

// summaryHead returns a bounded, whitespace-compacted head of s for a log
// summary (mirrors kb.Snippet's bound; kept here so the session package owns
// the on-disk bound it serializes). It is shared by job/done, subagent/end,
// compaction/summary and skill/load (all bounded to 200 runes, dispatch-m5a-2
// §1 / dispatch-m5b-2 §1 / dispatch-m5c-2 §1 / dispatch-m5d-2 §1).
func summaryHead(s string) string {
	compact := strings.Join(strings.Fields(s), " ")
	runes := []rune(compact)
	if len(runes) > jobOutputSummaryMax {
		return string(runes[:jobOutputSummaryMax]) + "…"
	}
	return compact
}

// subagentStartData is the subagent/start payload: the provider-issued child
// session id, the provider name, the delegating parent session, and the
// delegation label. DeriveHistory treats it as opaque data.
type subagentStartData struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ParentSession string `json:"parentSession,omitempty"`
	Label         string `json:"label,omitempty"`
	Depth         int    `json:"depth,omitempty"`
}

// subagentEndData is the subagent/end payload: a terminal settle (stop reason
// from the subagent vocabulary: completed | aborted | error | max-tokens |
// refusal) plus a bounded output summary — the full output the model reads
// through subagent_status' tool/result. DeriveHistory treats it as opaque
// data.
type subagentEndData struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	StopReason    string `json:"stopReason"`
	OutputSummary string `json:"outputSummary,omitempty"`
}

// subagentReportData is the subagent/report payload: an explicit child→parent
// report (child session id + delegating parent session + report content).
// DeriveHistory treats it as opaque data.
type subagentReportData struct {
	ID            string `json:"id"`
	ParentSession string `json:"parentSession,omitempty"`
	Content       string `json:"content"`
}

// NewSubagentStart builds the subagent/start payload recorded when a
// delegation registers successfully (dispatch-m5b-2 §1 / D3).
func NewSubagentStart(childID, provider, parentSessionID, label string) any {
	return subagentStartData{ID: childID, Provider: provider, ParentSession: parentSessionID, Label: label}
}

// NewSubagentStartWithDepth is the durable child-session variant. The parent
// session can reconstruct lineage after a process restart without changing
// the older parent-log payload constructor.
func NewSubagentStartWithDepth(childID, provider, parentSessionID, label string, depth int) any {
	return subagentStartData{ID: childID, Provider: provider, ParentSession: parentSessionID, Label: label, Depth: depth}
}

// NewSubagentEnd builds the subagent/end payload recorded when a child settles
// (dispatch-m5b-2 §1 / D3). output is bounded to a summary head (200 runes,
// the same on-disk bound as job/done) so the payload is always lean.
func NewSubagentEnd(childID, provider, stopReason, outputSummary string) any {
	return subagentEndData{ID: childID, Provider: provider, StopReason: stopReason, OutputSummary: summaryHead(outputSummary)}
}

// NewSubagentReport builds the subagent/report payload recorded when a child
// explicitly reports to its parent session (dispatch-m5b-2 §1 / D3).
func NewSubagentReport(childID, parentSessionID, content string) any {
	return subagentReportData{ID: childID, ParentSession: parentSessionID, Content: content}
}

type goalRoundStartData struct {
	GoalID string `json:"goalId"`
	Round  int    `json:"round"`
	Prompt string `json:"prompt,omitempty"`
}

type goalRoundEndData struct {
	GoalID string `json:"goalId"`
	Round  int    `json:"round"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func NewGoalRoundStart(goalID string, round int, prompt string) any {
	return goalRoundStartData{GoalID: goalID, Round: round, Prompt: summaryHead(prompt)}
}

func NewGoalRoundEnd(goalID string, round int, status, errText string) any {
	return goalRoundEndData{GoalID: goalID, Round: round, Status: status, Error: summaryHead(errText)}
}

// compactionStartData is the compaction/start payload: why a compaction
// attempt began (reason) and what triggered it (trigger, from the compaction
// vocabulary: pressure | context-overflow, or the /compact command). The
// compaction attempt locks the session surface until compaction/end. (Orphan
// start rows — a start with no matching end — reveal an interrupted attempt.)
// DeriveHistory treats it as opaque data.
type compactionStartData struct {
	Reason  string `json:"reason"`
	Trigger string `json:"trigger,omitempty"`
}

// compactionSummaryData is the compaction/summary payload: the compaction id
// and a bounded projection of the generated summary (200 runes — the summary
// body itself is a user/message with surfaceOp.replace, M5c-1a; this record is
// its log fact). DeriveHistory treats it as opaque data.
type compactionSummaryData struct {
	CompactionID   string  `json:"compactionId"`
	Summary        string  `json:"summary"`
	ShadowedSeqs   []int64 `json:"shadowedSeqs,omitempty"`
	ShadowedTokens int     `json:"shadowedTokens,omitempty"`
	Source         string  `json:"source,omitempty"`
}

// compactionEndData is the compaction/end payload: the compaction id, the
// shadowed surface range (first/last seq of the shadowed nodes) and the tokens
// saved. DeriveHistory treats it as opaque data.
type compactionEndData struct {
	CompactionID   string   `json:"compactionId"`
	ShadowedRange  [2]int64 `json:"shadowedRange"`
	ShadowedTokens int      `json:"shadowedTokens"`
	Error          string   `json:"error,omitempty"`
}

// compactionPruneData is the compaction/prune payload: the compaction id that
// triggered the prune, the number of tool results replaced and the bytes
// saved. DeriveHistory treats it as opaque data.
type compactionPruneData struct {
	CompactionID string `json:"compactionId"`
	Replaced     int    `json:"replaced"`
	SavedBytes   int    `json:"savedBytes"`
}

// NewCompactionStart builds the compaction/start payload recorded when a
// compaction attempt begins (dispatch-m5c-2 §1 / D3).
func NewCompactionStart(reason, trigger string) any {
	return compactionStartData{Reason: reason, Trigger: trigger}
}

// NewCompactionSummary builds the compaction/summary payload recorded when a
// compaction attempt lands its summary. summary is bounded to a summary head
// (200 runes, the same on-disk bound as job/done and subagent/end) so the
// payload is always lean.
func NewCompactionSummary(compactionID, summary string) any {
	return compactionSummaryData{CompactionID: compactionID, Summary: summaryHead(summary)}
}

// NewCompactionSummaryWithStats records the dsh-compatible shadow set and
// source metadata alongside the bounded summary projection.
func NewCompactionSummaryWithStats(compactionID, summary string, shadowedSeqs []int64, shadowedTokens int, source string) any {
	seqs := append([]int64(nil), shadowedSeqs...)
	return compactionSummaryData{
		CompactionID: compactionID, Summary: summaryHead(summary),
		ShadowedSeqs: seqs, ShadowedTokens: shadowedTokens, Source: source,
	}
}

// NewCompactionEnd builds the compaction/end payload recorded when a
// compaction attempt completes (dispatch-m5c-2 §1 / D3).
func NewCompactionEnd(compactionID string, shadowedRange [2]int64, shadowedTokens int) any {
	return compactionEndData{CompactionID: compactionID, ShadowedRange: shadowedRange, ShadowedTokens: shadowedTokens}
}

// NewCompactionEndError closes a failed compaction attempt, matching dsh's
// lifecycle guarantee that every started attempt has one terminal event.
func NewCompactionEndError(compactionID, errText string) any {
	return compactionEndData{CompactionID: compactionID, Error: summaryHead(errText)}
}

// NewCompactionPrune builds the compaction/prune payload recorded when a
// tool-result prune settles (dispatch-m5c-2 §1 / D3).
func NewCompactionPrune(compactionID string, replaced, savedBytes int) any {
	return compactionPruneData{CompactionID: compactionID, Replaced: replaced, SavedBytes: savedBytes}
}

// skillCatalogData is the skill/catalog payload: the number of skills in the
// injected catalog and an opaque catalog version (a digest over the sorted
// catalog, computed by the composition root) so consumers can detect that the
// catalog changed between turns. DeriveHistory treats it as opaque data.
type skillCatalogData struct {
	EntryCount int    `json:"entryCount"`
	Version    string `json:"version,omitempty"`
}

// skillLoadData is the skill/load payload: the loaded skill's name and source
// plus a bounded summary of the body the model is about to see. DeriveHistory
// treats it as opaque data.
type skillLoadData struct {
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Summary string `json:"summary"`
}

// NewSkillCatalog builds the skill/catalog payload recorded when the
// composition root injects the skill catalog as pre-step context
// (dispatch-m5d-2 §1 / D3). version is an opaque catalog version string
// (normally a digest over the sorted catalog, so consumers can detect drift).
func NewSkillCatalog(entryCount int, version string) any {
	return skillCatalogData{EntryCount: entryCount, Version: version}
}

// NewSkillLoad builds the skill/load payload recorded when skill_load loads a
// skill body for the model (dispatch-m5d-2 §1 / D3). summary is bounded to a
// summary head (200 runes, the same on-disk bound as job/done and subagent/end)
// so the payload is always lean.
func NewSkillLoad(name, source, summary string) any {
	return skillLoadData{Name: name, Source: source, Summary: summaryHead(summary)}
}

// scheduleCreateData is the schedule/create payload: the provider-issued id,
// the trigger kind and the spec of the newly created schedule. DeriveHistory
// treats it as opaque data.
type scheduleCreateData struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Spec string `json:"spec"`
}

// scheduleListData is the schedule/list payload: the number of schedules in
// the returned table. DeriveHistory treats it as opaque data.
type scheduleListData struct {
	Count int `json:"count"`
}

// scheduleDeleteData is the schedule/delete payload: the id of the removed
// schedule. DeriveHistory treats it as opaque data.
type scheduleDeleteData struct {
	ID string `json:"id"`
}

// scheduleFireData is the schedule/fire payload: the id of the fired schedule
// plus a bounded summary of the payload the executor receives. The log only
// ever carries the summary (200 runes, the same on-disk bound as job/done), not
// the full payload text — the enqueued job's output carries the full text and
// reaches the model through job_read's tool/result. DeriveHistory treats it as
// opaque data.
type scheduleFireData struct {
	ID      string `json:"id"`
	Payload string `json:"payload"`
}

// NewScheduleCreate builds the schedule/create payload recorded when
// schedule_create stores a trigger (dispatch-m6a-2 §1 / D3).
func NewScheduleCreate(id, kind, spec string) any {
	return scheduleCreateData{ID: id, Kind: kind, Spec: spec}
}

// NewScheduleList builds the schedule/list payload recorded when schedule_list
// returns the schedule table (dispatch-m6a-2 §1 / D3).
func NewScheduleList(count int) any {
	return scheduleListData{Count: count}
}

// NewScheduleDelete builds the schedule/delete payload recorded when
// schedule_delete removes a trigger (dispatch-m6a-2 §1 / D3).
func NewScheduleDelete(id string) any {
	return scheduleDeleteData{ID: id}
}

// NewScheduleFire builds the schedule/fire payload recorded when the serial
// pre-step path advances the schedule clock and a trigger is due
// (dispatch-m6a-2 §1 / D3). payload is bounded to a summary head (200 runes,
// the same on-disk bound as job/done) so the payload is always lean.
func NewScheduleFire(id, payload string) any {
	return scheduleFireData{ID: id, Payload: summaryHead(payload)}
}

// planCreateData is the plan/create payload: the tree level (scope: goal |
// plan | todo), the engine-issued id, the title and — for todos — the optional
// acceptance criteria (eval seam, ADR D-EVAL-4) of the stored record.
// DeriveHistory treats it as opaque data.
type planCreateData struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	Title string `json:"title"`
	// Acceptance carries the todo's eval criteria (nil for goals/plans, and
	// omitted from the payload when empty).
	Acceptance []string `json:"acceptance,omitempty"`
	// Detail is an optional full-record snapshot used by the plan projection
	// during restart. Older events intentionally omit it and remain readable.
	Detail map[string]any `json:"detail,omitempty"`
}

// planUpdateData is the plan/update payload: the tree level and id of an
// edited record. M6b-2 ships no plan-editing tool, so the type is reserved
// vocabulary (the constructor is exported so a future edit tool can emit it).
// DeriveHistory treats it as opaque data.
type planUpdateData struct {
	Scope     string `json:"scope"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Objective string `json:"objective,omitempty"`
}

// planDeleteData is the plan/delete payload: the tree level and id of the
// removed record. DeriveHistory treats it as opaque data.
type planDeleteData struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
}

// planStatusData is the plan/status payload: the tree level, id and new status
// of a record whose status was set. DeriveHistory treats it as opaque data.
type planStatusData struct {
	Scope  string `json:"scope"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// planListData is the plan/list payload: the number of goals in the returned
// aggregation tree. DeriveHistory treats it as opaque data.
type planListData struct {
	Count int `json:"count"`
}

// NewPlanCreate builds the plan/create payload recorded when plan_goal /
// plan_plan / plan_todo store a goal/plan/todo (dispatch-m6b-2 §1 / D3).
// acceptance is the todo's optional eval criteria list (ADR D-EVAL-4); goals
// and plans pass nil.
func NewPlanCreate(scope, id, title string, acceptance []string, detail ...map[string]any) any {
	var snapshot map[string]any
	if len(detail) > 0 {
		snapshot = detail[0]
	}
	return planCreateData{Scope: scope, ID: id, Title: title, Acceptance: acceptance, Detail: snapshot}
}

// NewPlanUpdate builds the plan/update payload — reserved vocabulary for a
// future plan-editing tool (dispatch-m6b-2 §1 / D3).
func NewPlanUpdate(scope, id string, detail ...map[string]string) any {
	data := planUpdateData{Scope: scope, ID: id}
	if len(detail) > 0 {
		data.Title = detail[0]["title"]
		data.Objective = detail[0]["objective"]
	}
	return data
}

type planModeData struct {
	Active bool `json:"active"`
}

// NewPlanMode records the durable per-session plan-mode switch.
func NewPlanMode(active bool) any { return planModeData{Active: active} }

// FoldPlanMode returns the last plan-mode value in an event stream. Plan mode
// is session state, so a missing event means the default inactive state.
func FoldPlanMode(events []Event) bool {
	active := false
	for _, ev := range events {
		if ev.Type != EventPlanMode {
			continue
		}
		var data planModeData
		if json.Unmarshal(ev.Data, &data) == nil {
			active = data.Active
		}
	}
	return active
}

// NewPlanDelete builds the plan/delete payload recorded when plan_remove
// deletes a record (dispatch-m6b-2 §1 / D3).
func NewPlanDelete(scope, id string) any {
	return planDeleteData{Scope: scope, ID: id}
}

// NewPlanStatus builds the plan/status payload recorded when plan_status sets
// a record's status (dispatch-m6b-2 §1 / D3).
func NewPlanStatus(scope, id string, st string, reason ...string) any {
	data := planStatusData{Scope: scope, ID: id, Status: st}
	if len(reason) > 0 {
		data.Reason = reason[0]
	}
	return data
}

// NewPlanList builds the plan/list payload recorded when plan_list returns the
// aggregation tree (dispatch-m6b-2 §1 / D3).
func NewPlanList(count int) any {
	return planListData{Count: count}
}

// spillWriteData is the spill/write payload: the memo id and a bounded summary
// of the memo content. Only the summary is logged (200 runes, the same
// on-disk bound as job/done), never the full body — the model sees the full
// memo through spill_recall/spill_list's tool/result. DeriveHistory treats it
// as opaque data.
type spillWriteData struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// spillRecallData is the spill/recall payload: the search query and the number
// of hits returned. DeriveHistory treats it as opaque data.
type spillRecallData struct {
	Query string `json:"query"`
	Count int    `json:"count"`
}

// spillListData is the spill/list payload: the number of memos in the returned
// table. DeriveHistory treats it as opaque data.
type spillListData struct {
	Count int `json:"count"`
}

// spillDeleteData is the spill/delete payload: the id of the removed memo.
// DeriveHistory treats it as opaque data.
type spillDeleteData struct {
	ID string `json:"id"`
}

// NewSpillWrite builds the spill/write payload recorded when a memo is stored
// — either by the spill_write tool or by the auto-sedimentation path
// (dispatch-m6c-2 §1 / D3). content is bounded to a summary head (200 runes,
// the same on-disk bound as job/done and subagent/end) so the payload is
// always lean.
func NewSpillWrite(id, content string) any {
	return spillWriteData{ID: id, Content: summaryHead(content)}
}

// NewSpillRecall builds the spill/recall payload recorded when spill_recall
// returns its hits (dispatch-m6c-2 §1 / D3).
func NewSpillRecall(query string, count int) any {
	return spillRecallData{Query: query, Count: count}
}

// NewSpillList builds the spill/list payload recorded when spill_list returns
// the memo table (dispatch-m6c-2 §1 / D3).
func NewSpillList(count int) any {
	return spillListData{Count: count}
}

// NewSpillDelete builds the spill/delete payload recorded when spill_delete
// removes a memo (dispatch-m6c-2 §1 / D3).
func NewSpillDelete(id string) any {
	return spillDeleteData{ID: id}
}

// interactRequestData is the interact/request payload: the provider-issued
// request id and the tool whose execution triggered the approval. DeriveHistory
// treats it as opaque data.
type interactRequestData struct {
	ID        string `json:"id"`
	ToolName  string `json:"toolName"`
	Prompt    string `json:"prompt,omitempty"`
	Args      string `json:"args,omitempty"`
	Questions any    `json:"questions,omitempty"`
}

// interactResolveData is the interact/resolve payload: the request id and the
// user's decision (approved = true, rejected = false). DeriveHistory treats it
// as opaque data.
type interactResolveData struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
}

// interactDenyData is the interact/deny payload: the request id of a sensitive
// tool execution that the gate blocked after a rejection. DeriveHistory treats
// it as opaque data.
type interactDenyData struct {
	ID string `json:"id"`
}

// interactStatusData is the interact/status payload: the request id and its
// current approval status (pending | approved | rejected | expired).
// DeriveHistory treats it as opaque data.
type interactStatusData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// NewInteractRequest builds the interact/request payload recorded when a
// request is created — by interact_ask or by the sensitive-tool gate
// (dispatch-m6d-2 §1 / D3).
func NewInteractRequest(id, toolName string) any {
	return interactRequestData{ID: id, ToolName: toolName}
}

// NewInteractRequestDetail records the bounded request projection needed to
// restore a Web approval card after restart. The legacy constructor remains
// intentionally small for old callers and old event fixtures.
func NewInteractRequestDetail(id, toolName, prompt, args string, questions any) any {
	return interactRequestData{ID: id, ToolName: toolName, Prompt: prompt, Args: args, Questions: questions}
}

// NewInteractResolve builds the interact/resolve payload recorded when a user
// decision is recorded for the request with id (dispatch-m6d-2 §1 / D3).
func NewInteractResolve(id string, approved bool) any {
	return interactResolveData{ID: id, Approved: approved}
}

// NewInteractCancel records a user-question dismissal separately from an
// explicit rejection so restart replay can preserve the outcome.
func NewInteractCancel(id string) any { return map[string]string{"id": id} }

// NewInteractDeny builds the interact/deny payload recorded when the
// sensitive-tool gate blocks a tool's execution after a rejection
// (dispatch-m6d-2 §1 / D3).
func NewInteractDeny(id string) any {
	return interactDenyData{ID: id}
}

// NewInteractStatus builds the interact/status payload recorded when
// interact_status reports a request's current status (dispatch-m6d-2 §1 / D3).
func NewInteractStatus(id, status string) any {
	return interactStatusData{ID: id, Status: status}
}

// codeRunData is the code/run payload: the executed language and the outcome
// markers of one sandbox run. The full stdout/stderr live in the tool/result
// event — this record is a lean log fact. DeriveHistory treats it as opaque
// data.
type codeRunData struct {
	Lang      string `json:"lang"`
	ExitCode  int    `json:"exitCode"`
	TimedOut  bool   `json:"timedOut"`
	Truncated bool   `json:"truncated"`
}

// NewCodeRun builds the code/run payload recorded when run_code completes a
// sandbox execution (dispatch-m6e-2 §1 / D3).
func NewCodeRun(lang string, exitCode int, timedOut, truncated bool) any {
	return codeRunData{Lang: lang, ExitCode: exitCode, TimedOut: timedOut, Truncated: truncated}
}

// mcpListData is the mcp/list payload: the number of tools the listed server
// advertised. DeriveHistory treats it as opaque data.
type mcpListData struct {
	Count int `json:"count"`
}

// mcpCallData is the mcp/call payload: the invoked tool name and whether the
// server reported a tool-level execution failure (isError) inside a successful
// result. A transport/protocol failure returns an error and logs nothing.
// DeriveHistory treats it as opaque data.
type mcpCallData struct {
	Name    string `json:"name"`
	IsError bool   `json:"isError"`
}

// NewMcpList builds the mcp/list payload recorded when mcp_list lists a
// configured server's tools (dispatch-m6f-2 §1 / D3).
func NewMcpList(count int) any {
	return mcpListData{Count: count}
}

// NewMcpCall builds the mcp/call payload recorded when mcp_call invokes a
// server tool (dispatch-m6f-2 §1 / D3).
func NewMcpCall(name string, isError bool) any {
	return mcpCallData{Name: name, IsError: isError}
}

// fsReadData is the fs/read payload: the requested path (as the caller spelled
// it) and the byte size of the content returned to the model. The content
// itself lives in the tool/result event — this record is a lean log fact.
// DeriveHistory treats it as opaque data.
type fsReadData struct {
	Path string `json:"path"`
	Size int    `json:"size"`
}

// fsWriteData is the fs/write payload: the path written (as the caller
// spelled it). DeriveHistory treats it as opaque data.
type fsWriteData struct {
	Path string `json:"path"`
}

// fsListData is the fs/list payload: the listed directory (as the caller
// spelled it) and the number of direct entries returned. DeriveHistory treats
// it as opaque data.
type fsListData struct {
	Dir   string `json:"dir"`
	Count int    `json:"count"`
}

// NewFsRead builds the fs/read payload recorded when read returns a file
// (dispatch-m6f-3 §2 / D3).
func NewFsRead(path string, size int) any {
	return fsReadData{Path: path, Size: size}
}

// NewFsWrite builds the fs/write payload recorded when write creates or
// overwrites a file (dispatch-m6f-3 §2 / D3).
func NewFsWrite(path string) any {
	return fsWriteData{Path: path}
}

// NewFsList builds the fs/list payload recorded when list lists a directory
// (dispatch-m6f-3 §2 / D3).
func NewFsList(dir string, count int) any {
	return fsListData{Dir: dir, Count: count}
}
