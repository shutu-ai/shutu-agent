package session

import "encoding/json"

// CanonicalApprovalEvent projects the compatibility interact/* audit facts
// into DSH's approval vocabulary. The bool-based legacy decision is normalized
// to the one-shot outcome contract. ok=false means the legacy fact is a UI
// status/denial marker with no canonical durable counterpart.
func CanonicalApprovalEvent(typ string, data any) (canonical string, value any, ok bool) {
	var raw map[string]any
	encoded, err := json.Marshal(data)
	if err != nil || json.Unmarshal(encoded, &raw) != nil {
		return "", nil, false
	}
	switch typ {
	case EventInteractRequest:
		// Keep the complete approval card in the canonical projection. The
		// reason is useful to ACP clients, while prompt/args/questions preserve
		// enough information for a restart to reconstruct the exact request.
		out := map[string]any{"id": raw["id"], "toolName": raw["toolName"]}
		if callID, exists := raw["callId"]; exists && callID != "" {
			out["callId"] = callID
		}
		if prompt, exists := raw["prompt"]; exists && prompt != "" {
			out["prompt"] = prompt
			out["reason"] = prompt
		}
		if args, exists := raw["args"]; exists {
			out["args"] = args
		}
		if questions, exists := raw["questions"]; exists {
			out["questions"] = questions
		}
		return EventApprovalAsked, out, true
	case EventInteractResolve:
		outcome := "rejected"
		if approved, _ := raw["approved"].(bool); approved {
			outcome = "allowed-once"
		}
		out := map[string]any{"id": raw["id"], "outcome": outcome}
		if callID, exists := raw["callId"]; exists && callID != "" {
			out["callId"] = callID
		}
		if answer, exists := raw["answer"]; exists && answer != "" {
			out["answer"] = answer
		}
		return EventApprovalDecided, out, true
	case EventInteractCancel:
		out := map[string]any{"id": raw["id"], "outcome": "cancelled"}
		if callID, exists := raw["callId"]; exists && callID != "" {
			out["callId"] = callID
		}
		return EventApprovalDecided, out, true
	default:
		return "", nil, false
	}
}
