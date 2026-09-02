package session

import "encoding/json"

// SessionListMetadata is the small, cross-surface session-list projection
// shared by Web and native. Blank means no turn has started yet; a stray
// user/message or a title/event row alone does not make a session engaged.
// LastPromptAt tracks only human user prompts and is represented as Unix ms to
// match the native wire projection.
type SessionListMetadata struct {
	Blank        bool   `json:"blank"`
	LastPromptAt *int64 `json:"lastPromptAt,omitempty"`
}

// NewSessionListMetadata returns the initial list projection for a new log.
func NewSessionListMetadata() SessionListMetadata {
	return SessionListMetadata{Blank: true}
}

// Apply folds one durable event into the session-list projection. Missing
// source metadata is accepted for legacy logs; explicit non-user provenance is
// excluded from prompt recency, matching dsh session-title eligibility.
func (m *SessionListMetadata) Apply(event Event) {
	if m == nil {
		return
	}
	if event.Type == EventTurnStart {
		m.Blank = false
	}
	if event.Type != EventUserMessage || !humanUserMessage(event.Data) {
		return
	}
	now := event.At.UnixMilli()
	if now < 0 {
		now = 0
	}
	m.LastPromptAt = &now
}

// ProjectSessionListMetadata rebuilds list metadata from the durable event
// stream. Callers may also use NewSessionListMetadata plus Apply for an
// incremental live fold.
func ProjectSessionListMetadata(events []Event) SessionListMetadata {
	metadata := NewSessionListMetadata()
	for _, event := range events {
		metadata.Apply(event)
	}
	return metadata
}

func humanUserMessage(data json.RawMessage) bool {
	var raw struct {
		Source *struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return true
	}
	return raw.Source == nil || raw.Source.Kind == "" || raw.Source.Kind == TitleSourceUser
}
