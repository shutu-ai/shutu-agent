package crashboundary

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestRequiredRegistryClassifiesEveryAcceptedFamily(t *testing.T) {
	registry := Required()
	byID := map[string]Contract{}
	for _, contract := range registry.List() {
		byID[contract.ID] = contract
	}
	required := map[string]CrashPolicy{
		"mcp.call":                  AtMostOnce,
		"terminal.foreground.write": AtMostOnce,
		"terminal.lifecycle":        AtMostOnce,
		"schedule.fire":             RetryableReceipt,
		"workflow.child":            AtMostOnce,
		"subagent.publication":      RetryableReceipt,
		"plugin.call":               AtMostOnce,
		"plugin.generation.reload":  AuditedUnorderedFailure,
	}
	for id, policy := range required {
		contract := byID[id]
		if contract.ID != id || contract.CrashPolicy != policy {
			t.Fatalf("%s contract = %+v, want crash policy %q", id, contract, policy)
		}
	}
	if _, err := registry.Get("not-a-boundary"); !errors.Is(err, ErrUnknownContract) {
		t.Fatalf("unknown boundary = %v, want ErrUnknownContract", err)
	}
}

func TestRequiredRegistryWireShapeIsStable(t *testing.T) {
	registry := Required()
	contracts := registry.List()
	encoded, err := json.Marshal(contracts)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Contract
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(contracts) {
		t.Fatalf("wire round trip changed contracts: %d -> %d", len(contracts), len(decoded))
	}
	for _, contract := range decoded {
		if contract.TransportFailurePolicy == NoAutomaticReplay && contract.AutomaticReplay {
			t.Fatalf("%s allows automatic replay after no-replay transport failure", contract.ID)
		}
	}
}
