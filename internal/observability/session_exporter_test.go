package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

func TestSessionTelemetryDisabledDoesNotRequireOrContactEndpoint(t *testing.T) {
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryDisabled})
	if err != nil || e != nil {
		t.Fatalf("disabled exporter = %v, %v; want nil, nil", e, err)
	}
}

func TestSessionTelemetryFullExportsCanonicalEventAsOTLPLog(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode OTLP payload: %v", err)
		} else {
			received <- payload
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFull, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	e.Observe("session-1", session.Event{Seq: 7, Type: session.EventUserMessage, Version: session.EventVersion, At: time.Unix(10, 0), Data: json.RawMessage(`{"text":"hello"}`)})
	var payload map[string]any
	select {
	case payload = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("telemetry event was not exported")
	}
	resourceLogs, ok := payload["resourceLogs"].([]any)
	if !ok || len(resourceLogs) != 1 {
		t.Fatalf("resourceLogs = %#v", payload["resourceLogs"])
	}
	encoded, _ := json.Marshal(payload)
	for _, want := range []string{"session.id", "session-1", "event.type", "user/message", "hello"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("OTLP payload missing %q: %s", want, encoded)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSessionTelemetryFeedbackOnlyIgnoresNonFeedbackEvents(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFeedbackOnly, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	e.Observe("session-1", session.Event{Seq: 1, Type: session.EventUserMessage, Data: json.RawMessage(`{}`)})
	select {
	case <-received:
		t.Fatal("feedback-only exporter sent a non-feedback event")
	case <-time.After(350 * time.Millisecond):
	}
	e.Observe("session-1", session.Event{Seq: 2, Type: session.EventFeedbackRecord, Data: json.RawMessage(`{"text":"ok"}`)})
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("feedback event was not exported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSessionTelemetryFeedbackOnlyReplaysCanonicalPrefix(t *testing.T) {
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			encoded, _ := json.Marshal(payload)
			received <- encoded
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFeedbackOnly, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	e.ObserveSession("session-1", []session.Event{
		{Seq: 1, Type: session.EventUserMessage, Data: json.RawMessage(`{"text":"before"}`)},
		{Seq: 2, Type: session.EventFeedbackRecord, Data: json.RawMessage(`{"text":"consent"}`)},
		{Seq: 3, Type: session.EventAssistantMessage, Data: json.RawMessage(`{"text":"after"}`)},
	}, 2)
	select {
	case payload := <-received:
		text := string(payload)
		if strings.Contains(text, "after") || !strings.Contains(text, "before") || !strings.Contains(text, "consent") {
			t.Fatalf("feedback prefix payload = %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("feedback prefix was not exported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSessionTelemetryExporterValidationAndFailureAreNonFatal(t *testing.T) {
	if _, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFull}); err == nil {
		t.Fatal("enabled telemetry without endpoint must fail closed")
	}
	if _, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: SessionTelemetryMode("OTHER"), Endpoint: "http://collector"}); err == nil {
		t.Fatal("unknown telemetry mode must fail closed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFull, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	e.Observe("session-1", session.Event{Seq: 1, Type: session.EventUserMessage, Data: json.RawMessage(`{}`)})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("collector failure must not fail shutdown: %v", err)
	}
}

func TestSessionTelemetryRetriesTransientCollectorFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFeedbackOnly, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	e.Observe("session-retry", session.Event{Seq: 1, Type: session.EventFeedbackRecord, Data: json.RawMessage(`{"ok":true}`)})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown after transient collector failure: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("collector calls = %d, want exactly three attempts", got)
	}
}

func TestSessionTelemetryResourceIdentityIsStableAndProfileLocal(t *testing.T) {
	received := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode OTLP payload: %v", err)
		} else {
			received <- payload
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	dataDir := t.TempDir()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: TelemetryFull, Endpoint: server.URL, DataDir: dataDir,
		ServiceName: "agent-test", ServiceVersion: "2026.08.29",
	})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	e.Observe("session-identity", session.Event{Seq: 1, Type: session.EventUserMessage, Data: json.RawMessage(`{"text":"hello"}`)})
	var payload map[string]any
	select {
	case payload = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("telemetry event was not exported")
	}
	resourceLogs := payload["resourceLogs"].([]any)
	resource := resourceLogs[0].(map[string]any)["resource"].(map[string]any)
	attrs := resource["attributes"].([]any)
	encodedAttrs, _ := json.Marshal(attrs)
	for _, want := range []string{"service.name", "agent-test", "service.version", "2026.08.29", "user.id", "anonymous-"} {
		if !strings.Contains(string(encodedAttrs), want) {
			t.Fatalf("resource attributes missing %q: %s", want, encodedAttrs)
		}
	}
	firstID, err := os.ReadFile(filepath.Join(dataDir, telemetryAnonymousIDFile))
	if err != nil {
		t.Fatalf("anonymous identity file: %v", err)
	}
	second, err := NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: TelemetryFull, Endpoint: server.URL, DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("reopen exporter: %v", err)
	}
	if second.userID != strings.TrimSpace(string(firstID)) {
		t.Fatalf("anonymous identity changed: first=%q second=%q", strings.TrimSpace(string(firstID)), second.userID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown first exporter: %v", err)
	}
	if err := second.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown second exporter: %v", err)
	}
}

func TestSessionTelemetryFullShutdownExportsSeparateOpsScope(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			received <- payload
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	e, err := NewSessionTelemetryExporter(SessionTelemetryConfig{Mode: TelemetryFull, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case payload := <-received:
		resourceLogs := payload["resourceLogs"].([]any)
		scopes := resourceLogs[0].(map[string]any)["scopeLogs"].([]any)
		if len(scopes) != 1 {
			t.Fatalf("shutdown scopes = %#v", scopes)
		}
		scope := scopes[0].(map[string]any)["scope"].(map[string]any)
		if scope["name"] != "shutu-agent/session-telemetry/ops" {
			t.Fatalf("shutdown scope = %#v", scope)
		}
		records := scopes[0].(map[string]any)["logRecords"].([]any)
		attrs, _ := json.Marshal(records[0].(map[string]any)["attributes"])
		if !strings.Contains(string(attrs), `"telemetry.op"`) || !strings.Contains(string(attrs), `"shutdown"`) {
			t.Fatalf("shutdown attrs = %s", attrs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown ops record was not exported")
	}
}
