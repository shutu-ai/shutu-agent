// Package contractfixture exposes the checked-in cross-package wire fixture.
// Tests in session, Web projection, persistence and protocol adapters consume
// the same bytes so each layer cannot quietly invent a different event shape.
package contractfixture

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed core-turn-replay.json
var coreTurnReplay []byte

//go:embed protocol-lifecycle.json
var protocolLifecycle []byte

// ProtocolLifecycle is the transport-neutral scenario shared by ACP, MCP and
// SDK protocol boundary tests. Protocol adapters keep their own wire methods;
// this fixture owns the common lifecycle facts they must not reinvent.
type ProtocolLifecycle struct {
	Scenario  string            `json:"scenario"`
	Workspace string            `json:"workspace"`
	SessionID string            `json:"sessionId"`
	MessageID string            `json:"messageId"`
	Prompt    []json.RawMessage `json:"prompt"`
	Tool      struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Output    string          `json:"output"`
	} `json:"tool"`
	Assistant string   `json:"assistant"`
	Stages    []string `json:"stages"`
}

// ProtocolLifecycleFixture decodes and validates the shared protocol scenario.
func ProtocolLifecycleFixture() (ProtocolLifecycle, error) {
	var fixture ProtocolLifecycle
	if err := json.Unmarshal(protocolLifecycle, &fixture); err != nil {
		return ProtocolLifecycle{}, err
	}
	if fixture.Scenario == "" || fixture.Workspace == "" || fixture.SessionID == "" ||
		fixture.MessageID == "" || len(fixture.Prompt) == 0 || fixture.Tool.Name == "" ||
		fixture.Assistant == "" || len(fixture.Stages) == 0 {
		return ProtocolLifecycle{}, fmt.Errorf("invalid shared protocol lifecycle fixture")
	}
	return fixture, nil
}

// CoreTurnReplay returns detached canonical wire-event records.
func CoreTurnReplay() ([]json.RawMessage, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(coreTurnReplay, &records); err != nil {
		return nil, err
	}
	return records, nil
}

// EventRecord is the transport-neutral representation of one fixture row.
// Consumers convert it into their own event type at the package boundary.
type EventRecord struct {
	Type string
	Seq  uint64
	Time int64
	Data json.RawMessage
}

// CoreTurnEvents decodes the shared fixture into transport-neutral records.
// Cross-package contract tests should use this helper instead of each
// reimplementing JSON record parsing.
func CoreTurnEvents() ([]EventRecord, error) {
	records, err := CoreTurnReplay()
	if err != nil {
		return nil, err
	}
	events := make([]EventRecord, 0, len(records))
	for _, raw := range records {
		var wire struct {
			Type string          `json:"type"`
			Seq  uint64          `json:"seq"`
			Time int64           `json:"time"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, fmt.Errorf("decode core turn fixture: %w", err)
		}
		events = append(events, EventRecord{
			Seq: wire.Seq, Type: wire.Type, Time: wire.Time,
			Data: append(json.RawMessage(nil), wire.Data...),
		})
	}
	return events, nil
}
