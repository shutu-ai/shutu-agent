package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func workflowRecordEvent(t *testing.T, seq uint64, typ string, data map[string]any) Event {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return Event{Seq: seq, Type: typ, Version: EventVersion, Data: raw}
}

func TestValidateWorkflowRecordLifecycleAcceptsInterleavedAndOpenRuns(t *testing.T) {
	events := []Event{
		workflowRecordEvent(t, 1, EventToolWorkflowRunStart, map[string]any{"runId": "first", "name": "first"}),
		workflowRecordEvent(t, 2, EventToolWorkflowRunStart, map[string]any{"runId": "second", "name": "second"}),
		workflowRecordEvent(t, 3, EventToolWorkflowAgentStart, map[string]any{"runId": "second", "seq": 1, "label": "", "childId": "child-2"}),
		workflowRecordEvent(t, 4, EventToolWorkflowRunEnd, map[string]any{"runId": "first", "stopReason": "completed"}),
		workflowRecordEvent(t, 5, EventToolWorkflowAgentEnd, map[string]any{"runId": "second", "seq": 1, "outcome": "cancelled"}),
		workflowRecordEvent(t, 6, EventToolWorkflowRunEnd, map[string]any{"runId": "second", "stopReason": "cancelled"}),
		workflowRecordEvent(t, 7, EventToolWorkflowRunStart, map[string]any{"runId": "third", "name": "third"}),
		workflowRecordEvent(t, 8, EventToolWorkflowAgentStart, map[string]any{"runId": "third", "seq": 1, "label": "", "childId": "child-3"}),
		workflowRecordEvent(t, 9, EventToolWorkflowAgentEnd, map[string]any{"runId": "third", "seq": 1, "outcome": "failed"}),
		workflowRecordEvent(t, 10, EventToolWorkflowRunEnd, map[string]any{"runId": "third", "stopReason": "error"}),
		workflowRecordEvent(t, 11, EventToolWorkflowRunStart, map[string]any{"runId": "prefix", "name": "prefix"}),
		workflowRecordEvent(t, 12, EventToolWorkflowAgentStart, map[string]any{"runId": "prefix", "seq": 1, "label": "", "childId": "child-4"}),
	}
	if err := validateWorkflowRecordLifecycle(events); err != nil {
		t.Fatalf("valid workflow records rejected: %v", err)
	}
	if err := ValidateLifecycle(events); err != nil {
		t.Fatalf("workflow records rejected by lifecycle boundary: %v", err)
	}
}

func TestValidateWorkflowRecordLifecycleRejectsInvalidFolds(t *testing.T) {
	validStart := func(seq uint64, runID string) Event {
		return workflowRecordEvent(t, seq, EventToolWorkflowRunStart, map[string]any{"runId": runID, "name": runID})
	}
	validMemberStart := func(seq uint64, runID string) Event {
		return workflowRecordEvent(t, seq, EventToolWorkflowAgentStart, map[string]any{"runId": runID, "seq": 1, "label": "", "childId": "child-1"})
	}
	validMemberEnd := func(seq uint64, runID string) Event {
		return workflowRecordEvent(t, seq, EventToolWorkflowAgentEnd, map[string]any{"runId": runID, "seq": 1, "outcome": "completed"})
	}
	validRunEnd := func(seq uint64, runID string) Event {
		return workflowRecordEvent(t, seq, EventToolWorkflowRunEnd, map[string]any{"runId": runID, "stopReason": "completed"})
	}
	tests := []struct {
		name   string
		events []Event
		want   string
	}{
		{
			name:   "repeat run",
			events: []Event{validStart(1, "run"), validStart(2, "run")},
			want:   "repeats run",
		},
		{
			name: "member start without child",
			events: []Event{
				validStart(1, "run"),
				workflowRecordEvent(t, 2, EventToolWorkflowAgentStart, map[string]any{"runId": "run", "seq": 1, "label": ""}),
			},
			want: "requires label and non-empty childId",
		},
		{
			name: "member end without start",
			events: []Event{
				validStart(1, "run"),
				validMemberEnd(2, "run"),
			},
			want: "has no matching member seq",
		},
		{
			name: "run end with open member",
			events: []Event{
				validStart(1, "run"),
				validMemberStart(2, "run"),
				validRunEnd(3, "run"),
			},
			want: "leaves member seq 1 open",
		},
		{
			name: "event after run end",
			events: []Event{
				validStart(1, "run"),
				validMemberStart(2, "run"),
				validMemberEnd(3, "run"),
				validRunEnd(4, "run"),
				validMemberStart(5, "run"),
			},
			want: "follows run-end",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateLifecycle(test.events)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateLifecycle error = %v, want %q", err, test.want)
			}
		})
	}
}
