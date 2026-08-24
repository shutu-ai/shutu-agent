package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jabing/shutu-agent/internal/tools"
)

const structuredOutputToolName = "structured_output"

// structuredOutputTool is installed only in a child registry for a run that
// requested OutputSchema. The ordinary parent registry never sees it.
type structuredOutputTool struct {
	schema  map[string]any
	capture func(any) error
}

func (structuredOutputTool) Name() string { return structuredOutputToolName }

func (structuredOutputTool) Description() string {
	return "Report the final structured result. Call exactly once when the task is complete."
}

func (t structuredOutputTool) Schema() map[string]any { return t.schema }

func (t structuredOutputTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal(args, &value); err != nil {
		return "", fmt.Errorf("structured_output: invalid JSON: %w", err)
	}
	if t.capture != nil {
		if err := t.capture(value); err != nil {
			return "", err
		}
	}
	return "Structured output recorded.", nil
}

func structuredPrompt(prompt string, schema map[string]any) string {
	raw, err := json.Marshal(schema)
	if err != nil {
		return prompt + "\n\nWhen complete, call the structured_output tool with the final object."
	}
	return prompt + "\n\nWhen your task is complete, you MUST call the `" + structuredOutputToolName + "` tool exactly once with a JSON object matching this schema. Do not finish with a plain-text answer:\n" + string(raw)
}

var _ tools.Tool = structuredOutputTool{}
