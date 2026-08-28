package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

const exitPlanModeName = "exit_plan_mode"

// exitPlanModeTool is the composition-root consumer of the plan-mode state
// and the user-question channel. It is registered whenever planning is
// enabled, including while plan mode is inactive, so changing mode does not
// churn the model-facing catalog.
type exitPlanModeTool struct {
	app *app
}

func (exitPlanModeTool) Name() string { return exitPlanModeName }

func (exitPlanModeTool) Description() string {
	return "Use only in plan mode. Present your plan for the user's review and, on approval, leave plan mode. Send the COMPLETE plan as markdown, starting with a # heading that names it. The user may approve (carry out the plan from your next step) or keep planning — their feedback comes back in the tool result; revise and present again."
}

func (exitPlanModeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan": map[string]any{
				"type":        "string",
				"description": "The complete plan, as markdown, starting with a # heading that names it.",
			},
		},
		"required": []string{"plan"},
	}
}

func (exitPlanModeTool) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"approved": map[string]any{"type": "boolean", "const": true},
		},
		"required": []string{"approved"},
	}
}

func (t exitPlanModeTool) Execute(ctx context.Context, args any) (string, error) {
	var input struct {
		Plan string `json:"plan"`
	}
	if err := tools.DecodeArgs(args, &input); err != nil {
		return "", fmt.Errorf("exit_plan_mode: %w", err)
	}
	if t.app == nil || t.app.log == nil {
		return "", errors.New("exit_plan_mode requires an active session")
	}
	if !session.FoldPlanMode(t.app.log.Events()) {
		return "", errors.New("exit_plan_mode is only available in plan mode")
	}
	planText := strings.TrimSpace(input.Plan)
	if !strings.HasPrefix(planText, "#") || len(strings.TrimSpace(strings.TrimPrefix(planText, "#"))) == 0 {
		return "", errors.New("exit_plan_mode requires a non-empty markdown plan starting with a # heading")
	}
	if t.app.interacts == nil {
		return "", errors.New("no user-questions channel is available to review the plan; ask the user to switch the session mode instead")
	}
	structured, ok := t.app.interacts.(interact.StructuredRequester)
	if !ok {
		return "", errors.New("no user-questions channel is available to review the plan; ask the user to switch the session mode instead")
	}

	questions := []interact.Question{{
		ID: "plan-review", Header: "Plan review", Question: "Approve this plan and leave plan mode?", Detail: planText,
		Options: []interact.QuestionOption{
			{Label: "Approve", Description: "Leave plan mode; the plan is carried out from the next step."},
			{Label: "Keep planning", Description: "Stay in plan mode; feedback goes back to the model."},
		},
	}}
	rawArgs, _ := json.Marshal(map[string]string{"plan": input.Plan})
	req, err := structured.RequestWithQuestions(ctx, "Please review the plan.", exitPlanModeName, boundPlanArgs(string(rawArgs)), questions)
	if err != nil {
		return "", fmt.Errorf("exit_plan_mode: %w", err)
	}
	t.app.recordPlanQuestion(req)
	resolved, err := t.app.interacts.Await(ctx, req.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.app.cancelPlanQuestion(req.ID)
			return "", errors.New("ask_user_question was aborted before the user answered")
		}
		return "", fmt.Errorf("exit_plan_mode: %w", err)
	}
	if resolved.Status == interact.StatusCanceled {
		return "", errors.New("The user dismissed the plan review to speak instead; stay in plan mode, stop here, and wait for their message.")
	}
	if resolved.Status != interact.StatusApproved {
		return "", errors.New("The user chose to keep planning; revise the plan and present it again.")
	}
	approved, feedback, err := planReviewDecision(resolved.Answer)
	if err != nil {
		return "", fmt.Errorf("exit_plan_mode: invalid review answer: %w", err)
	}
	if !approved {
		if feedback == "" {
			return "", errors.New("The user chose to keep planning; revise the plan and present it again.")
		}
		return "", fmt.Errorf("The user chose to keep planning; their feedback: %s", feedback)
	}
	if _, err := t.app.log.Append(session.EventPlanMode, session.NewPlanMode(false)); err != nil {
		return "", fmt.Errorf("exit_plan_mode: leave plan mode: %w", err)
	}
	return `{"approved":true}`, nil
}

// ExecuteResult keeps the approved response as an object for the registry's
// output-schema validation. The legacy Execute string method remains useful
// for direct unit callers, but must not be the model-facing transport.
func (t exitPlanModeTool) ExecuteResult(ctx context.Context, args any) (tools.ToolResult, error) {
	raw, err := t.Execute(ctx, args)
	if err != nil {
		return tools.ToolResult{}, err
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return tools.ToolResult{}, fmt.Errorf("exit_plan_mode: encode result: %w", err)
	}
	return tools.ToolResult{Value: value, Output: raw}, nil
}

func boundPlanArgs(args string) string {
	runes := []rune(args)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return args
}

func planReviewDecision(raw string) (approved bool, feedback string, err error) {
	var payload struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
			Custom   *string  `json:"custom"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return false, "", err
	}
	for _, answer := range payload.Answers {
		if answer.ID != "plan-review" {
			continue
		}
		if len(answer.Selected) == 1 && answer.Selected[0] == "Approve" && answer.Custom == nil {
			return true, "", nil
		}
		if answer.Custom != nil {
			return false, *answer.Custom, nil
		}
		return false, "", nil
	}
	return false, "", nil
}

func (a *app) recordPlanQuestion(req interact.Request) {
	if req.ID != "" {
		a.interactionMu.Lock()
		if a.interactionSessions == nil {
			a.interactionSessions = make(map[string]string)
		}
		a.interactionSessions[req.ID] = a.currentID
		a.interactionMu.Unlock()
	}
	if a.log != nil {
		if _, err := a.log.Append(session.EventInteractRequest, session.NewInteractRequestDetail(req.ID, req.ToolName, req.Prompt, req.Args, req.Questions)); err != nil {
			fmt.Println("pa: interact/request event:", err)
		}
	}
}

func (a *app) cancelPlanQuestion(id string) {
	canceler, ok := a.interacts.(interact.Canceler)
	if !ok {
		return
	}
	if _, err := canceler.Cancel(context.Background(), id); err != nil {
		return
	}
	if a.log != nil {
		_, _ = a.log.Append(session.EventInteractCancel, session.NewInteractCancel(id))
	}
}
