package session

import (
	"fmt"
	"math"
)

type workflowRecordFold struct {
	ended   bool
	members map[int]bool
}

// validateWorkflowRecordLifecycle mirrors the reference tool-workflow record
// fold. An unfinished run at the end of the log is valid: a process can crash
// after run-start or a member start. Anything else malformed—repeated run or
// member identity, an end without a matching open start, or an event after a
// run ended—must fail replay instead of silently reshaping durable state.
func validateWorkflowRecordLifecycle(events []Event) error {
	runs := make(map[string]*workflowRecordFold)
	for _, event := range events {
		switch event.Type {
		case EventToolWorkflowRunStart, EventToolWorkflowAgentStart,
			EventToolWorkflowAgentEnd, EventToolWorkflowRunEnd:
		default:
			continue
		}
		data, err := lifecycleObject(event.Data)
		if err != nil {
			return fmt.Errorf("session: %s at seq %d data is not an object: %w", event.Type, event.Seq, err)
		}
		runID, _ := data["runId"].(string)
		if event.Type == EventToolWorkflowRunStart {
			name, nameOK := data["name"].(string)
			if runID == "" || !nameOK || name == "" {
				return fmt.Errorf("session: %s at seq %d requires non-empty runId and name", event.Type, event.Seq)
			}
			if _, exists := runs[runID]; exists {
				return fmt.Errorf("session: %s at seq %d repeats run %q", event.Type, event.Seq, runID)
			}
			runs[runID] = &workflowRecordFold{members: make(map[int]bool)}
			continue
		}
		if runID == "" {
			return fmt.Errorf("session: %s at seq %d requires runId", event.Type, event.Seq)
		}
		run, exists := runs[runID]
		if !exists {
			return fmt.Errorf("session: %s at seq %d has no matching run-start for %q", event.Type, event.Seq, runID)
		}
		if run.ended {
			return fmt.Errorf("session: %s at seq %d follows run-end for %q", event.Type, event.Seq, runID)
		}
		switch event.Type {
		case EventToolWorkflowAgentStart:
			seq, err := workflowRecordSeq(data, event)
			if err != nil {
				return err
			}
			_, labelOK := data["label"].(string)
			childID, childOK := data["childId"].(string)
			if !labelOK || !childOK || childID == "" {
				return fmt.Errorf("session: %s at seq %d requires label and non-empty childId", event.Type, event.Seq)
			}
			if phase, exists := data["phase"]; exists {
				if _, valid := phase.(string); !valid {
					return fmt.Errorf("session: %s at seq %d phase must be a string when present", event.Type, event.Seq)
				}
			}
			if _, exists := run.members[seq]; exists {
				return fmt.Errorf("session: %s at seq %d repeats member seq %d in run %q", event.Type, event.Seq, seq, runID)
			}
			run.members[seq] = false
		case EventToolWorkflowAgentEnd:
			seq, err := workflowRecordSeq(data, event)
			if err != nil {
				return err
			}
			outcome, _ := data["outcome"].(string)
			if outcome != "completed" && outcome != "failed" && outcome != "cancelled" {
				return fmt.Errorf("session: %s at seq %d has invalid outcome %q", event.Type, event.Seq, outcome)
			}
			ended, exists := run.members[seq]
			if !exists {
				return fmt.Errorf("session: %s at seq %d has no matching member seq %d in run %q", event.Type, event.Seq, seq, runID)
			}
			if ended {
				return fmt.Errorf("session: %s at seq %d repeats member seq %d in run %q", event.Type, event.Seq, seq, runID)
			}
			run.members[seq] = true
		case EventToolWorkflowRunEnd:
			stopReason, _ := data["stopReason"].(string)
			if stopReason != "completed" && stopReason != "cancelled" && stopReason != "error" {
				return fmt.Errorf("session: %s at seq %d has invalid stopReason %q", event.Type, event.Seq, stopReason)
			}
			for seq, memberEnded := range run.members {
				if !memberEnded {
					return fmt.Errorf("session: %s at seq %d leaves member seq %d open in run %q", event.Type, event.Seq, seq, runID)
				}
			}
			run.ended = true
			run.members = make(map[int]bool)
		}
	}
	return nil
}

func workflowRecordSeq(data map[string]any, event Event) (int, error) {
	number, ok := data["seq"].(float64)
	if !ok || number != math.Trunc(number) || number < 1 || number > math.MaxInt32 {
		return 0, fmt.Errorf("session: %s at seq %d requires a positive member seq", event.Type, event.Seq)
	}
	return int(number), nil
}
