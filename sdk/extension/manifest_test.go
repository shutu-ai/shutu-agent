package extension

import (
	"testing"
)

func TestParseManifestAndNegotiation(t *testing.T) {
	manifest, err := ParseManifest([]byte(`
id: demo
name: Demo
version: 0.1.0
extension_api: 1.0
capabilities:
  tools: true
  context_provider: true
  lifecycle: true
  health: true
transport:
  type: stdio
  command: demo
tools:
  definitions:
    - name: echo
      description: Echo input
      input_schema:
        type: object
      risk: read
context_provider:
  enabled: true
  strategy: before_every_model_call
health:
  enabled: true
permissions:
  - name: user.input
    required: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !CompatibleAgentAPI("1.2", manifest.RequiredAgentAPI) {
		t.Fatal("empty required API must be satisfied by host v1")
	}
	if !CompatibleAgentAPI("1.2", "1.1") {
		t.Fatal("v1 minor versions must be backward compatible")
	}
	if CompatibleAgentAPI("1.0", "2.0") {
		t.Fatal("major versions must be incompatible")
	}
}

func TestParseManifestRejectsUnknownField(t *testing.T) {
	_, err := ParseManifest([]byte("id: demo\nname: Demo\nversion: 0.1.0\nextension_api: 1.0\nfuture: yes\n"))
	if err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestManifestRejectsUnsafeNamesAndEnvironment(t *testing.T) {
	base := "id: demo\nname: Demo\nversion: 0.1.0\nextension_api: 1.0\ncapabilities: {tools: true}\n"
	if _, err := ParseManifest([]byte(base + "transport: {type: stdio, command: demo, env: [\"API_TOKEN=x\"]}\ntools: {definitions: [{name: okay, input_schema: {type: object}, risk: read}]}\n")); err == nil {
		t.Fatal("credential-shaped environment was accepted")
	}
	if _, err := ParseManifest([]byte(base + "transport: {type: stdio, command: demo}\ntools: {definitions: [{name: bad/name, input_schema: {type: object}, risk: read}]}\n")); err == nil {
		t.Fatal("unsafe tool name was accepted")
	}
}

func TestEventSubscriptionValidation(t *testing.T) {
	base := "id: events\nname: Events\nversion: 0.1.0\nextension_api: 1.0\ntransport: {type: stdio, command: events}\n"
	valid := base + "capabilities: {events: true}\nevents:\n  subscribe:\n    - turn.completed\n    - tool.failed\n"
	manifest, err := ParseManifest([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Events.Subscribe) != 2 {
		t.Fatalf("subscriptions = %#v", manifest.Events.Subscribe)
	}
	if _, err := ParseManifest([]byte(base + "capabilities: {events: true}\nevents:\n  subscribe: [unknown.event]\n")); err == nil {
		t.Fatal("unknown event subscription was accepted")
	}
	if _, err := ParseManifest([]byte(base + "events:\n  subscribe: [turn.completed]\n")); err == nil {
		t.Fatal("event subscription without events capability was accepted")
	}
}
