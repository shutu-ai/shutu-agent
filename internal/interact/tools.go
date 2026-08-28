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
	agenttools "github.com/jabing/shutu-agent/internal/tools"
	"strings"

	"github.com/jabing/shutu-agent/internal/session"
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
	e       Engine
	onEvent func(typ string, data any)
}

// NewInteractTools returns the shared interact-tool bundle bound to an Engine.
// onEvent, when non-nil, receives the interact/* event payloads; the
// composition root wires it to the session log (D3).
func NewInteractTools(e Engine, onEvent func(typ string, data any)) *InteractTools {
	return &InteractTools{e: e, onEvent: onEvent}
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
	var req Request
	var err error
	if len(a.Questions) > 0 {
		structured, ok := t.t.e.(StructuredRequester)
		if !ok {
			return "", fmt.Errorf("structured questions are unavailable")
		}
		req, err = structured.RequestWithQuestions(ctx, a.Prompt, ToolAskName, boundArgs(string(rawArgs)), a.Questions)
	} else {
		req, err = t.t.e.Request(ctx, a.Prompt, ToolAskName, boundArgs(string(rawArgs)))
	}
	if err != nil {
		return "", fmt.Errorf("interact_ask: %w", err)
	}
	// interact/request is a log-only fact (D3); the created request's id and
	// triggering tool are logged, and the returned text is what the loop logs
	// as tool/result.
	t.t.emit(session.EventInteractRequest, session.NewInteractRequestDetail(req.ID, req.ToolName, req.Prompt, req.Args, req.Questions))
	return fmt.Sprintf("created approval request %s (status=%s); the user will answer on their terminal", req.ID, req.Status), nil
}

// AskUserQuestionTool raises a structured question batch and waits until the
// Web/host resolves it. Unlike legacy interact_ask, this tool does not return
// before the human answer is available.
type AskUserQuestionTool struct{ t *InteractTools }

func (AskUserQuestionTool) Name() string { return ToolAskUserQuestionName }
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
						"header":   map[string]any{"type": "string", "description": "Optional short heading for the question, such as Confirm or Choose Mode."},
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
	structured, ok := t.t.e.(StructuredRequester)
	if !ok {
		return "", fmt.Errorf("ask_user_question: structured questions are unavailable")
	}
	rawArgs, _ := json.Marshal(args)
	req, err := structured.RequestWithQuestions(ctx, "Please answer the following questions.", ToolAskUserQuestionName, boundArgs(string(rawArgs)), questions)
	if err != nil {
		return "", fmt.Errorf("ask_user_question: %w", err)
	}
	t.t.emit(session.EventInteractRequest, session.NewInteractRequestDetail(req.ID, req.ToolName, req.Prompt, req.Args, req.Questions))
	resolved, err := t.t.e.Await(ctx, req.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if canceler, ok := t.t.e.(Canceler); ok {
				if _, cancelErr := canceler.Cancel(context.Background(), req.ID); cancelErr == nil {
					t.t.emit(session.EventInteractCancel, session.NewInteractCancel(req.ID))
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
	if resolved.Status != StatusApproved {
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
	all, err := t.t.e.List(ctx)
	if err != nil {
		return "", fmt.Errorf("interact_status: %w", err)
	}
	for _, r := range all {
		if r.ID == a.ID {
			// interact/status is a log-only fact (D3).
			t.t.emit(session.EventInteractStatus, session.NewInteractStatus(r.ID, string(r.Status)))
			return fmt.Sprintf("request %s: status=%s", r.ID, r.Status), nil
		}
	}
	return "", fmt.Errorf("interact_status: unknown request %s", a.ID)
}
