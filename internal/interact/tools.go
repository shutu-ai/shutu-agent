// tools.go — the M6d-2 Consumer half of the interact seam (design.md §8
// Consumer / D2, dispatch-m6d-2 §3): interact_ask and interact_status are
// registered into the tools.Registry by the composition root (cmd/pa) when
// interact.enabled, and auto-whitelisted by config.applyDefaults the same way
// the job_*/subagent_*/skill_*/schedule_*/plan_*/spill_* tools are. They
// implement the tools.Tool method set structurally (Go structural typing), so
// this package never imports the tools package — the seam stays decoupled (D2).
//
// D7 is enforced by the registry: every Execute validates the model-generated
// arguments against the compiled JSON Schema below (additionalProperties:
// false; the required prompt/id fields) before this code runs; the checks are
// repeated here so a direct call can never bypass them.
//
// D3 event logging follows the M5a-2 tool-layer decision (ADR 决策 ① 实施说明 /
// dispatch-m6d-2 §3): interact_ask emits interact/request on a successful
// create and interact_status emits interact/status on a lookup — all through
// the injected onEvent sink (the composition root wires it to the session log),
// and each append happens inside a tool Execute — the serial main-loop path
// (D5). interact/resolve and interact/deny are emitted by the wiring layer's
// sensitive-tool gate (see cmd/pa), not by a tool.
package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	agenttools "github.com/shutu-ai/shutu-agent/internal/tools"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// Tool names (whitelisted when interact.enabled; see config.interactToolNames).
const (
	ToolAskName             = "interact_ask"
	ToolAskUserQuestionName = "ask_user_question"
	ToolStatusName          = "interact_status"
)

// InteractTools bundles the shared state of the two interact_* tools: the
// Engine service and the event sink.
type InteractTools struct {
	e          Engine
	onEvent    func(typ string, data any)
	onEventErr func(typ string, data any) error
	sessionID  func() string
	logFor     func(context.Context) (*session.Log, error)
}

// NewInteractTools returns the shared interact-tool bundle bound to an Engine.
// onEvent, when non-nil, receives the interact/* event payloads; the
// composition root wires it to the session log (D3).
func NewInteractTools(e Engine, onEvent func(typ string, data any)) *InteractTools {
	return &InteractTools{e: e, onEvent: onEvent}
}

// NewInteractToolsWithSession binds requests to the currently addressed
// session. The callback is evaluated at execution time so session switches do
// not leave an approval request owned by the previous conversation.
func NewInteractToolsWithSession(e Engine, onEvent func(typ string, data any), sessionID func() string) *InteractTools {
	return &InteractTools{e: e, onEvent: onEvent, sessionID: sessionID}
}

// NewInteractToolsWithSessionAndErrorSink is the durable composition-root
// variant. Legacy callers retain the void callback, while runtimes that need
// approval facts to be commit-before-visible can return append failures to the
// tool execution instead of silently continuing with an unlogged request.
func NewInteractToolsWithSessionAndErrorSink(e Engine, onEvent func(typ string, data any), onEventErr func(typ string, data any) error, sessionID func() string) *InteractTools {
	return &InteractTools{e: e, onEvent: onEvent, onEventErr: onEventErr, sessionID: sessionID}
}

// SetSessionLogResolver enables the durable creation-side approval seam for
// interact tools. It is optional so package-level embedders and compatibility
// test doubles can keep the legacy event callback.
func (t *InteractTools) SetSessionLogResolver(resolve func(context.Context) (*session.Log, error)) {
	if t != nil {
		t.logFor = resolve
	}
}

func (t *InteractTools) currentSession(ctx ...context.Context) string {
	if len(ctx) > 0 {
		if sessionID := runtimectx.SessionID(ctx[0]); sessionID != "" {
			return sessionID
		}
	}
	if t.sessionID == nil {
		return ""
	}
	return strings.TrimSpace(t.sessionID())
}

func (t *InteractTools) request(ctx context.Context, prompt, toolName, args string) (Request, error) {
	if sessionID := t.currentSession(ctx); sessionID != "" {
		if requester, ok := t.e.(SessionRequester); ok {
			return requester.RequestForSession(ctx, sessionID, prompt, toolName, args)
		}
	}
	return t.e.Request(ctx, prompt, toolName, args)
}

func (t *InteractTools) requestWithQuestions(ctx context.Context, prompt, toolName, args string, questions []Question) (Request, error) {
	if sessionID := t.currentSession(ctx); sessionID != "" {
		if requester, ok := t.e.(StructuredSessionRequester); ok {
			return requester.RequestForSessionWithQuestions(ctx, sessionID, prompt, toolName, args, questions)
		}
	}
	requester, ok := t.e.(StructuredRequester)
	if !ok {
		return Request{}, fmt.Errorf("structured questions are unavailable")
	}
	return requester.RequestWithQuestions(ctx, prompt, toolName, args, questions)
}

// requestWithAudit creates an approval request and, when a durable session log
// plus an atomic provider are available, returns the already-committed asked
// event for projection into that log. The bool is false for compatibility
// providers, which continue through emitContext and rollback on append failure.
func (t *InteractTools) requestWithAudit(ctx context.Context, prompt, toolName, args string, questions []Question) (Request, bool, session.Event, *session.Log, error) {
	sessionID := t.currentSession(ctx)
	if sessionID == "" || t.logFor == nil {
		if len(questions) > 0 {
			req, err := t.requestWithQuestions(ctx, prompt, toolName, args, questions)
			return req, false, session.Event{}, nil, err
		}
		req, err := t.request(ctx, prompt, toolName, args)
		return req, false, session.Event{}, nil, err
	}
	log, err := t.logFor(ctx)
	if err != nil || log == nil {
		if err == nil {
			err = errors.New("session log is unavailable")
		}
		return Request{}, false, session.Event{}, nil, err
	}
	if correlation, ok := runtimectx.CorrelationOf(ctx); ok && correlation.TurnID != "" && !session.HasOpenTurn(log.Events()) {
		return Request{}, false, session.Event{}, nil, errors.New("interact: approval request requires an open turn")
	}
	callID := agenttools.CallIDFromContext(ctx)
	var asked session.Event
	makeEvent := func(created Request) session.Event {
		payload := session.NewInteractRequestDetailWithCallID(created.ID, callID, toolName, created.Prompt, created.Args, created.Questions)
		asked = session.Event{Seq: log.NextSeq(), Type: session.EventApprovalAsked, At: time.Now().UTC(), Version: session.EventVersion, Data: marshalEvent(payload)}
		return asked
	}
	if len(questions) > 0 {
		requester, ok := t.e.(AtomicStructuredSessionRequester)
		if !ok {
			req, fallbackErr := t.requestWithQuestions(ctx, prompt, toolName, args, questions)
			return req, false, session.Event{}, nil, fallbackErr
		}
		req, atomic, eventErr := requester.RequestForSessionWithQuestionsAndEvent(ctx, sessionID, prompt, toolName, args, questions, makeEvent)
		return req, atomic, asked, log, eventErr
	}
	requester, ok := t.e.(AtomicSessionCallRequester)
	if !ok {
		req, fallbackErr := t.request(ctx, prompt, toolName, args)
		return req, false, session.Event{}, nil, fallbackErr
	}
	req, atomic, eventErr := requester.RequestForSessionWithCallIDAndEvent(ctx, sessionID, callID, prompt, toolName, args, makeEvent)
	return req, atomic, asked, log, eventErr
}

func marshalEvent(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

// Ask returns the interact_ask tool.
func (t *InteractTools) Ask() InteractAskTool { return InteractAskTool{t: t} }

// AskUserQuestion returns the blocking DSH-compatible question tool.
func (t *InteractTools) AskUserQuestion() AskUserQuestionTool {
	return AskUserQuestionTool{t: t}
}

// Status returns the interact_status tool.
func (t *InteractTools) Status() InteractStatusTool { return InteractStatusTool{t: t} }

// emit forwards one interact/* event payload to the injected sink (D3).
func (t *InteractTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

func (t *InteractTools) emitContext(ctx context.Context, typ string, data any) error {
	if runtime, ok := runtimectx.Get(ctx); ok && runtime.Emit != nil {
		if canonical, value, projected := session.CanonicalApprovalEvent(typ, data); projected {
			return runtime.Emit(canonical, value)
		}
		return runtime.Emit(typ, data)
	}
	if t.onEventErr != nil {
		return t.onEventErr(typ, data)
	}
	t.emit(typ, data)
	return nil
}

// boundArgs trims args to the engine's stored-args bound (maxArgsLen runes,
// engine.go) so an over-long payload can never make Request fail on a tool the
// caller is legitimately invoking — the full args the model sees stay in the
// tool/result event, while the request row only carries the bounded projection.
func boundArgs(args string) string {
	runes := []rune(args)
	if len(runes) > maxArgsLen {
		return string(runes[:maxArgsLen])
	}
	return args
}

// InteractAskTool raises a question or approval request for the user and
// returns the request id plus its current status. The CLI interaction happens
// on the user's terminal; the tool returns without blocking, so the model
// continues and the user answers in their own time.
type InteractAskTool struct {
	t *InteractTools
}

func (InteractAskTool) Name() string { return ToolAskName }

func (InteractAskTool) Description() string {
	return "ask the user a question or request approval; returns the request id and its current status (the CLI interaction happens on the user's terminal, the model continues)"
}

func (InteractAskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the user-facing question or the sensitive action to approve",
			},
			"questions": map[string]any{
				"type":        "array",
				"description": "optional structured questions; each answer is returned with selected option labels",
				"items": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"id": map[string]any{"type": "string"}, "question": map[string]any{"type": "string"}, "detail": map[string]any{"type": "string"}, "header": map[string]any{"type": "string"}, "multi_select": map[string]any{"type": "boolean"}, "options": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"label": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, "required": []string{"label"}, "additionalProperties": false}}},
					"required":             []string{"id", "question"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
}

func (t InteractAskTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		Prompt    string     `json:"prompt"`
		Questions []Question `json:"questions"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("interact_ask: %w", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", fmt.Errorf("interact_ask: prompt is required")
	}
	if err := validateQuestions(a.Questions); err != nil {
		return "", fmt.Errorf("interact_ask: %w", err)
	}
	rawArgs, _ := json.Marshal(args)
	req, atomic, askedEvent, askedLog, err := t.t.requestWithAudit(ctx, a.Prompt, ToolAskName, boundArgs(string(rawArgs)), a.Questions)
	if err != nil {
		return "", fmt.Errorf("interact_ask: %w", err)
	}
	if req.Status == StatusRejected && req.ID == "" {
		return "approval rejected by session policy (no pending request created)", nil
	}
	if req.Status == StatusUnavailable && req.ID == "" {
		return "approval unavailable (no answerer is configured)", nil
	}
	// interact/request is a log-only fact (D3); the created request's id and
	// triggering tool are logged, and the returned text is what the loop logs
	// as tool/result.
	var emitErr error
	if atomic {
		if askedLog != nil {
			emitErr = askedLog.AppendPersisted(askedEvent)
		} else {
			emitErr = errors.New("session log is unavailable")
		}
	} else {
		emitErr = t.t.emitContext(ctx, session.EventInteractRequest, session.NewInteractRequestDetail(req.ID, req.ToolName, req.Prompt, req.Args, req.Questions))
	}
	if emitErr != nil {
		// A request is not durable/answerable until its asked fact commits.
		// Roll back the in-memory row on a failed append so a retry cannot leave
		// an invisible pending item consuming the approval cap.
		if canceler, ok := t.t.e.(Canceler); ok && req.ID != "" {
			_, _ = canceler.Cancel(context.Background(), req.ID)
		}
		return "", fmt.Errorf("interact_ask: persist request event: %w", emitErr)
	}
	return fmt.Sprintf("created approval request %s (status=%s); the user will answer on their terminal", req.ID, req.Status), nil
}

// AskUserQuestionTool raises a structured question batch and waits until the
// Web/host resolves it. Unlike legacy interact_ask, this tool does not return
// before the human answer is available.
type AskUserQuestionTool struct{ t *InteractTools }

func (AskUserQuestionTool) Name() string { return ToolAskUserQuestionName }

// CancellationAware is explicit: awaiting the human answer observes the
// registry context and cancels the pending request on abort.
func (AskUserQuestionTool) CancellationAware() bool { return true }
func (AskUserQuestionTool) Description() string {
	return "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding. Send one or more questions, each with a stable id that will be echoed in the answer."
}
func (AskUserQuestionTool) Schema() map[string]any {
	return map[string]any{
		"type": "object", "properties": map[string]any{
			"questions": map[string]any{
				"type": "array", "description": "Questions to ask the user before continuing.",
				"items": map[string]any{
					"type": "object", "properties": map[string]any{
						"id":       map[string]any{"type": "string", "description": "Stable id for this question; echoed in the answer."},
						"question": map[string]any{"type": "string", "description": "The specific question to ask the user."},
						"header":   map[string]any{"type": "string", "description": "Optional short heading for the question, such as \"Confirm\" or \"Choose Mode\"."},
						"options": map[string]any{
							"type":        "array",
							"description": "Optional choices to show the user. If you recommend one, put it first and append (Recommended) to that label.",
							"items": map[string]any{
								"type": "object", "properties": map[string]any{
									"label":       map[string]any{"type": "string", "description": "Short user-facing option label."},
									"description": map[string]any{"type": "string", "description": "One sentence explaining the tradeoff or impact."},
								}, "required": []string{"label"}, "additionalProperties": true,
							},
						},
						"multi_select": map[string]any{"type": "boolean", "description": "Whether the user may select more than one option. Defaults to false."},
					}, "required": []string{"id", "question"}, "additionalProperties": true,
				},
			},
		}, "required": []string{"questions"},
	}
}

func (t AskUserQuestionTool) Execute(ctx context.Context, args any) (string, error) {
	var input struct {
		Questions []askUserQuestionInput `json:"questions"`
	}
	if err := agenttools.DecodeArgs(args, &input); err != nil {
		return "", fmt.Errorf("ask_user_question: %w", err)
	}
	if len(input.Questions) == 0 {
		return "", fmt.Errorf("ask_user_question: at least one question is required")
	}
	questions := make([]Question, 0, len(input.Questions))
	for _, item := range input.Questions {
		questions = append(questions, Question{
			ID: item.ID, Question: item.Question, Header: item.Header,
			Options: item.Options, MultiSelect: item.MultiSelect,
		})
	}
	if err := validateQuestions(questions); err != nil {
		return "", fmt.Errorf("ask_user_question: %w", err)
	}
	rawArgs, _ := json.Marshal(args)
	req, atomic, askedEvent, askedLog, err := t.t.requestWithAudit(ctx, "Please answer the following questions.", ToolAskUserQuestionName, boundArgs(string(rawArgs)), questions)
	if err != nil {
		return "", fmt.Errorf("ask_user_question: %w", err)
	}
	if req.Status == StatusRejected && req.ID == "" {
		return "", errors.New("ask_user_question was rejected by the session approval policy")
	}
	if req.Status == StatusUnavailable && req.ID == "" {
		return "", errors.New("ask_user_question is unavailable: no answerer is registered")
	}
	var emitErr error
	if atomic {
		if askedLog != nil {
			emitErr = askedLog.AppendPersisted(askedEvent)
		} else {
			emitErr = errors.New("session log is unavailable")
		}
	} else {
		emitErr = t.t.emitContext(ctx, session.EventInteractRequest, session.NewInteractRequestDetail(req.ID, req.ToolName, req.Prompt, req.Args, req.Questions))
	}
	if emitErr != nil {
		// Do not retain a pending question whose durable request fact did not
		// commit. A subsequent model retry must be able to create a fresh,
		// answerable request without an orphan occupying the pending cap.
		if canceler, ok := t.t.e.(Canceler); ok && req.ID != "" {
			_, _ = canceler.Cancel(context.Background(), req.ID)
		}
		return "", fmt.Errorf("ask_user_question: persist request event: %w", emitErr)
	}
	resolved, err := t.t.e.Await(ctx, req.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if canceler, ok := t.t.e.(Canceler); ok {
				if _, cancelErr := canceler.Cancel(context.Background(), req.ID); cancelErr == nil {
					if emitErr := t.t.emitContext(ctx, session.EventInteractCancel, session.NewInteractCancel(req.ID)); emitErr != nil {
						return "", errors.Join(errors.New("ask_user_question was aborted before the user answered"), fmt.Errorf("persist cancellation event: %w", emitErr))
					}
				}
			}
			// DSH intentionally exposes both caller cancellation and a
			// deadline as the same stable model-facing abort error.
			return "", errors.New("ask_user_question was aborted before the user answered")
		}
		return "", fmt.Errorf("ask_user_question: %w", err)
	}
	if resolved.Status == StatusCanceled {
		return "", errors.New("the user cancelled ask_user_question")
	}
	if resolved.Status == StatusUnavailable {
		return "", errors.New("ask_user_question is unavailable: no answerer is registered")
	}
	if resolved.Status != StatusApproved && resolved.Status != StatusAllowedOnce {
		return "", fmt.Errorf("ask_user_question: user rejected the questions")
	}
	if resolved.Answer == "" {
		return `{"answers":[]}`, nil
	}
	answer, err := canonicalAnswer(resolved.Answer)
	if err != nil {
		return "", fmt.Errorf("ask_user_question: invalid answer: %w", err)
	}
	return answer, nil
}

// ExecuteResult preserves the structured answer object when the tool is
// dispatched through tools.Registry. Execute is retained as the string seam
// for direct callers, but a JSON string is not a structured ToolResult value
// and would fail the DSH output schema at the registry boundary.
func (t AskUserQuestionTool) ExecuteResult(ctx context.Context, args any) (agenttools.ToolResult, error) {
	raw, err := t.Execute(ctx, args)
	if err != nil {
		return agenttools.ToolResult{}, err
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return agenttools.ToolResult{}, fmt.Errorf("ask_user_question: encode result: %w", err)
	}
	return agenttools.ToolResult{Value: value, Output: raw}, nil
}

// askUserQuestionInput intentionally contains only the DSH model-facing
// fields. The DSH tool accepts additional item properties for forward
// compatibility but projects the known fields before calling its provider;
// fields such as the legacy `detail` must not leak into the UI seam.
type askUserQuestionInput struct {
	ID          string           `json:"id"`
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select"`
}

type askUserQuestionAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   *string  `json:"custom,omitempty"`
}

type askUserQuestionAnswerPayload struct {
	Answers []askUserQuestionAnswer `json:"answers"`
}

// canonicalAnswer mirrors the DSH tool's provider-to-model projection: only
// id, selected, and an explicitly present custom answer are returned.
func canonicalAnswer(raw string) (string, error) {
	var payload askUserQuestionAnswerPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func validateQuestions(questions []Question) error {
	seen := make(map[string]struct{}, len(questions))
	for _, q := range questions {
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("each question requires id and question")
		}
		if _, ok := seen[q.ID]; ok {
			return fmt.Errorf("duplicate question id %q", q.ID)
		}
		seen[q.ID] = struct{}{}
		optionSeen := make(map[string]struct{}, len(q.Options))
		for _, option := range q.Options {
			if strings.TrimSpace(option.Label) == "" {
				return fmt.Errorf("question %q has an empty option label", q.ID)
			}
			if _, ok := optionSeen[option.Label]; ok {
				return fmt.Errorf("question %q has duplicate option %q", q.ID, option.Label)
			}
			optionSeen[option.Label] = struct{}{}
		}
	}
	return nil
}

// InteractStatusTool looks up the current approval status of one request.
type InteractStatusTool struct {
	t *InteractTools
}

func (InteractStatusTool) Name() string { return ToolStatusName }

func (InteractStatusTool) Description() string {
	return "look up the current approval status of one request by its id (pending | approved | rejected | expired)"
}

func (InteractStatusTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the request id returned by interact_ask or shown by the approval gate",
			},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}

func (t InteractStatusTool) Execute(ctx context.Context, args any) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := agenttools.DecodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("interact_status: %w", err)
	}
	if a.ID == "" {
		return "", fmt.Errorf("interact_status: id is required")
	}
	var all []Request
	var err error
	if sessionID := t.t.currentSession(ctx); sessionID != "" {
		if lister, ok := t.t.e.(SessionLister); ok {
			all, err = lister.ListForSession(ctx, sessionID)
		} else {
			all, err = t.t.e.List(ctx)
		}
	} else {
		all, err = t.t.e.List(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("interact_status: %w", err)
	}
	for _, r := range all {
		if r.ID == a.ID {
			if sessionID := t.t.currentSession(ctx); sessionID != "" && r.SessionID != sessionID {
				return "", fmt.Errorf("interact_status: unknown request %s", a.ID)
			}
			// interact/status is a log-only fact (D3).
			if err := t.t.emitContext(ctx, session.EventInteractStatus, session.NewInteractStatus(r.ID, string(r.Status))); err != nil {
				return "", fmt.Errorf("interact_status: persist status event: %w", err)
			}
			return fmt.Sprintf("request %s: status=%s", r.ID, r.Status), nil
		}
	}
	return "", fmt.Errorf("interact_status: unknown request %s", a.ID)
}
