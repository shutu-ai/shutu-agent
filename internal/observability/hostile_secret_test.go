package observability

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// TestSessionTelemetryRedactsHostileCredentialDiagnostics is the egress
// defense-in-depth gate: even if a legacy or third-party event contains a
// credential-shaped string, the telemetry wire cannot carry it.
func TestSessionTelemetryRedactsHostileCredentialDiagnostics(t *testing.T) {
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read OTLP payload: %v", err)
		} else {
			received <- payload
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	exporter, err := NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: TelemetryFull, Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := `{
		"message":"temporary failure api_key=hostile-secret-value",
		"authorization":"Bearer hostile-bearer-value",
		"nested":{"password":"hostile-password-value","items":["token=hostile-token-value"]},
		"safe":"ordinary provider failure"
	}`
	exporter.Observe("hostile-session", session.Event{
		Seq: 9, Type: session.EventToolResult, Version: session.EventVersion,
		At: time.Unix(10, 0).UTC(), Data: json.RawMessage(data),
	})

	var payload []byte
	select {
	case payload = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("hostile telemetry event was not exported")
	}
	encoded := string(payload)
	for _, secret := range []string{
		"hostile-secret-value", "hostile-bearer-value",
		"hostile-password-value", "hostile-token-value",
	} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("telemetry leaked %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(encoded, "ordinary provider failure") || !strings.Contains(encoded, "[REDACTED]") {
		t.Fatalf("telemetry lost useful redacted context: %s", encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exporter.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
