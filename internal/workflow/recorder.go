package workflow

import (
	"context"
	"encoding/json"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// toolWorkflowRecorder projects reference-shaped durable records into the
// calling parent session. Runtime workflow failures are handled separately by
// the event sink; a recorder append failure disables this run's projection
// without changing the model-visible workflow result, matching the reference.
type toolWorkflowRecorder struct {
	emit     func(context.Context, string, any) error
	runID    string
	endState string
	active   bool
	disabled bool
}

func (r *toolWorkflowRecorder) observe(ctx context.Context, event ScriptEvent) {
	if r == nil || r.disabled {
		return
	}
	switch event.Type {
	case session.EventWorkflowStart:
		runID, name, ok := workflowRecordStart(event.Data)
		if !ok {
			r.disabled = true
			return
		}
		r.runID, r.active = runID, true
		r.append(ctx, session.EventToolWorkflowRunStart, map[string]any{"runId": runID, "name": name})
	case session.EventWorkflowAgentStart:
		runID, seq, label, phase, childID, ok := workflowRecordAgent(event.Data)
		if !r.active || !ok || runID != r.runID || childID == "" {
			r.disabled = true
			return
		}
		value := map[string]any{"runId": runID, "seq": seq, "label": label, "childId": childID}
		if phase != "" {
			value["phase"] = phase
		}
		r.append(ctx, session.EventToolWorkflowAgentStart, value)
	case session.EventWorkflowAgentEnd:
		runID, seq, _, _, childID, ok := workflowRecordAgent(event.Data)
		outcome, _ := workflowRecordString(event.Data, "outcome")
		if !r.active || !ok || runID != r.runID || childID == "" ||
			(outcome != "completed" && outcome != "failed" && outcome != "cancelled") {
			r.disabled = true
			return
		}
		r.append(ctx, session.EventToolWorkflowAgentEnd, map[string]any{"runId": runID, "seq": seq, "outcome": outcome})
	case session.EventWorkflowEnd:
		runID, ok := workflowRecordRunID(event.Data)
		stopReason, _ := workflowRecordString(event.Data, "stop_reason")
		if !r.active || !ok || runID != r.runID ||
			(stopReason != "completed" && stopReason != "cancelled" && stopReason != "error") {
			r.disabled = true
			return
		}
		r.endState = stopReason
	}
}

func (r *toolWorkflowRecorder) finish(ctx context.Context) {
	if r == nil || r.disabled || !r.active || r.endState == "" {
		return
	}
	r.append(ctx, session.EventToolWorkflowRunEnd, map[string]any{"runId": r.runID, "stopReason": r.endState})
	r.active = false
}

func (r *toolWorkflowRecorder) append(ctx context.Context, typ string, data any) {
	if err := r.emit(ctx, typ, data); err != nil {
		r.disabled = true
	}
}

func workflowRecordMap(data any) (map[string]any, bool) {
	value, ok := data.(map[string]any)
	return value, ok
}

func workflowRecordRunID(data any) (string, bool) {
	value, ok := workflowRecordMap(data)
	if !ok {
		return "", false
	}
	runID, _ := value["run_id"].(string)
	return runID, runID != ""
}

func workflowRecordStart(data any) (string, string, bool) {
	value, ok := workflowRecordMap(data)
	if !ok {
		return "", "", false
	}
	runID, _ := value["run_id"].(string)
	meta, _ := value["meta"].(map[string]any)
	name, _ := meta["name"].(string)
	return runID, name, runID != "" && name != ""
}

func workflowRecordString(data any, key string) (string, bool) {
	value, ok := workflowRecordMap(data)
	if !ok {
		return "", false
	}
	text, ok := value[key].(string)
	return text, ok
}

func workflowRecordAgent(data any) (runID string, seq int, label, phase, childID string, ok bool) {
	value, mapOK := workflowRecordMap(data)
	if !mapOK {
		return "", 0, "", "", "", false
	}
	runID, _ = value["run_id"].(string)
	label, _ = value["label"].(string)
	phase, _ = value["phase"].(string)
	childID, _ = value["child_id"].(string)
	var number float64
	switch typed := value["seq"].(type) {
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case float64:
		number = typed
	case json.Number:
		number, _ = typed.Float64()
	default:
		number = 0
	}
	seq = int(number)
	ok = runID != "" && number >= 1 && float64(seq) == number
	return runID, seq, label, phase, childID, ok
}
