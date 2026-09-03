package observability

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

type blockedTelemetryTransport struct {
	started atomic.Int32
	release chan struct{}
}

func (t *blockedTelemetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.started.Add(1)
	select {
	case <-request.Context().Done():
		return nil, request.Context().Err()
	case <-t.release:
	}
	return nil, errors.New("telemetry transport was released without a response")
}

func TestSessionTelemetryShutdownBoundsAnInFlightCollector(t *testing.T) {
	transport := &blockedTelemetryTransport{release: make(chan struct{})}
	defer close(transport.release)
	exporter, err := NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: TelemetryFull, Endpoint: "http://collector.invalid",
		Client: &http.Client{Transport: transport, Timeout: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	exporter.Observe("bounded-shutdown", session.Event{
		Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"hello"}`),
	})
	for transport.started.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := exporter.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung collector shutdown = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("shutdown took %v; must stay bounded by its context", elapsed)
	}
	select {
	case <-exporter.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown left the telemetry worker running")
	}
}

func TestSessionTelemetryBackpressureNeverBlocksCommittedEventObservers(t *testing.T) {
	transport := &blockedTelemetryTransport{release: make(chan struct{})}
	defer close(transport.release)
	exporter, err := NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: TelemetryFull, Endpoint: "http://collector.invalid",
		Client: &http.Client{Transport: transport, Timeout: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for seq := uint64(1); seq <= telemetryQueueSize+256; seq++ {
		exporter.Observe("backpressure", session.Event{
			Seq: seq, Type: session.EventAssistantChunk, Version: session.EventVersion,
			Data: json.RawMessage(`{"text":"dropped under pressure"}`),
		})
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("queue overflow blocked committed-event observation for %v", elapsed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = exporter.Shutdown(ctx)
}

func TestSessionTelemetryPreservesRouteSessionUserAndResourceIdentity(t *testing.T) {
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err == nil {
			received <- payload
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	exporter, err := NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: TelemetryFull, Endpoint: server.URL,
		ServiceName:    "shutu-test",
		ServiceVersion: "test-version",
		UserID:         "user-resource-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	exporter.Observe("session-route-identity", session.Event{
		Seq: 12, Type: session.EventLLMRequestStart, Version: session.EventVersion,
		Data: json.RawMessage(`{"provider":"deepseek-official","model":"deepseek-v4-flash"}`),
	})
	encoded := string(<-received)
	for _, want := range []string{
		`"provider.id"`, `"deepseek-official"`,
		`"session.id"`, `"session-route-identity"`,
		`"service.name"`, `"shutu-test"`,
		`"service.version"`, `"test-version"`,
		`"user.id"`, `"user-resource-id"`,
	} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("OTLP identity payload missing %q: %s", want, encoded)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exporter.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
