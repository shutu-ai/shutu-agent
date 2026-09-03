package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// SessionTelemetryMode is the deployment-selected session sharing policy.
// Disabled is the safe default; enabling either upload mode is explicit.
type SessionTelemetryMode string

const (
	TelemetryFull         SessionTelemetryMode = "FULL"
	TelemetryFeedbackOnly SessionTelemetryMode = "FEEDBACK_ONLY"
	TelemetryDisabled     SessionTelemetryMode = "DISABLED"
)

const (
	telemetryQueueSize             = 1024
	telemetryBatchSize             = 64
	telemetryBatchWait             = 250 * time.Millisecond
	telemetryExportMaxAttempts     = 3
	telemetryExportRetryBackoff    = 100 * time.Millisecond
	telemetryDefaultServiceName    = "shutu-agent"
	telemetryDefaultServiceVersion = "dev"
	telemetryAnonymousIDFile       = ".anonymous-user-id"
)

// SessionTelemetryConfig configures the optional OTLP/HTTP log exporter.
// Endpoint is required for uploading modes and is never contacted in disabled
// mode. The exporter is asynchronous and bounded; an unavailable collector
// can lose observations but cannot fail a durable session append or turn.
type SessionTelemetryConfig struct {
	Mode     SessionTelemetryMode
	Endpoint string
	Client   *http.Client
	// DataDir is used for the stable anonymous identity shared by telemetry
	// and feedback. It is optional for embedders without a persistent profile.
	DataDir string
	// Resource identity is attached once per OTLP batch, not per record.
	ServiceName    string
	ServiceVersion string
	UserID         string
}

// SessionTelemetryExporter captures canonical session events and sends them
// as OTLP/HTTP JSON log records. Observe is deliberately non-blocking so the
// durable session path remains authoritative and independent of telemetry.
type SessionTelemetryExporter struct {
	mode           SessionTelemetryMode
	endpoint       string
	client         *http.Client
	serviceName    string
	serviceVersion string
	userID         string
	queue          chan telemetryRecord
	exportCtx      context.Context
	exportCancel   context.CancelFunc
	done           chan struct{}
	closed         chan struct{}
	closeOne       sync.Once
}

type telemetryRecord struct {
	sessionID string
	event     session.Event
	channel   string
}

// NewSessionTelemetryExporter validates configuration and starts the bounded
// background exporter. Disabled mode returns nil, nil and starts no worker.
func NewSessionTelemetryExporter(cfg SessionTelemetryConfig) (*SessionTelemetryExporter, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = TelemetryDisabled
	}
	switch mode {
	case TelemetryDisabled:
		return nil, nil
	case TelemetryFull, TelemetryFeedbackOnly:
	default:
		return nil, fmt.Errorf("session telemetry: unsupported mode %q", mode)
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("session telemetry: endpoint is required when telemetry is enabled")
	}
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("session telemetry: endpoint must be an http(s) URL")
	}
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = telemetryDefaultServiceName
	}
	serviceVersion := strings.TrimSpace(cfg.ServiceVersion)
	if serviceVersion == "" {
		serviceVersion = telemetryDefaultServiceVersion
	}
	userID := strings.TrimSpace(cfg.UserID)
	if userID == "" && strings.TrimSpace(cfg.DataDir) != "" {
		userID, err = getOrCreateAnonymousTelemetryID(cfg.DataDir)
		if err != nil {
			return nil, err
		}
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	e := &SessionTelemetryExporter{
		mode: mode, endpoint: cfg.Endpoint, client: client,
		serviceName: serviceName, serviceVersion: serviceVersion, userID: userID,
		queue: make(chan telemetryRecord, telemetryQueueSize),
		done:  make(chan struct{}), closed: make(chan struct{}),
	}
	e.exportCtx, e.exportCancel = context.WithCancel(context.Background())
	go e.run()
	return e, nil
}

// NewSessionTelemetryExporterFromEnv applies the reference-compatible
// deployment switches. Any non-empty DSH_TELEMETRY_DISABLED wins over mode.
func NewSessionTelemetryExporterFromEnv() (*SessionTelemetryExporter, error) {
	return NewSessionTelemetryExporterFromEnvAt(strings.TrimSpace(os.Getenv("DSH_DATA_DIR")))
}

// NewSessionTelemetryExporterFromEnvAt is the composition-root form. The
// explicit data directory keeps identity independent of process cwd.
func NewSessionTelemetryExporterFromEnvAt(dataDir string) (*SessionTelemetryExporter, error) {
	if strings.TrimSpace(os.Getenv("DSH_TELEMETRY_DISABLED")) != "" {
		return nil, nil
	}
	mode := SessionTelemetryMode(strings.TrimSpace(os.Getenv("DSH_TELEMETRY_MODE")))
	if mode == "" {
		mode = TelemetryDisabled
	}
	return NewSessionTelemetryExporter(SessionTelemetryConfig{
		Mode: mode, Endpoint: strings.TrimSpace(os.Getenv("DSH_TELEMETRY_OTLP_URL")), DataDir: dataDir,
		ServiceName: os.Getenv("DSH_SERVICE_NAME"), ServiceVersion: os.Getenv("DSH_SERVICE_VERSION"),
		UserID: os.Getenv("DSH_TELEMETRY_USER_ID"),
	})
}

// Observe queues one already-committed canonical event. Events are ignored in
// disabled mode and, for FEEDBACK_ONLY, until the canonical feedback event.
func (e *SessionTelemetryExporter) Observe(sessionID string, event session.Event) {
	if e == nil || e.mode == TelemetryDisabled || sessionID == "" {
		return
	}
	if e.mode == TelemetryFeedbackOnly && event.Type != session.EventFeedbackRecord {
		return
	}
	e.enqueue(telemetryRecord{sessionID: sessionID, event: event, channel: "ledger"})
}

// ObserveSession replays a canonical prefix for FEEDBACK_ONLY. This mirrors
// the reference consent boundary: feedback is the committed trigger, and the
// uploaded record is the session-log prefix through that feedback event, not
// an independently supplied live-bus payload.
func (e *SessionTelemetryExporter) ObserveSession(sessionID string, events []session.Event, throughSeq uint64) {
	if e == nil || e.mode != TelemetryFeedbackOnly || sessionID == "" {
		return
	}
	for _, event := range events {
		if event.Seq > throughSeq {
			break
		}
		e.enqueue(telemetryRecord{sessionID: sessionID, event: event, channel: "ledger"})
	}
}

func (e *SessionTelemetryExporter) enqueue(record telemetryRecord) {
	select {
	case <-e.done:
		return
	default:
	}
	select {
	case e.queue <- record:
	default:
		// Observation loss is intentional under pressure; durable replay remains
		// complete and can be exported later by a canonical-log reader.
	}
}

// Shutdown drains queued observations until ctx expires. Collector failures
// are returned for diagnostics, but are never propagated into agent execution.
func (e *SessionTelemetryExporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.closeOne.Do(func() {
		if e.mode == TelemetryFull {
			e.enqueue(telemetryRecord{
				channel: "ops",
				event: session.Event{
					Type: "telemetry/shutdown", Version: session.EventVersion, At: time.Now().UTC(),
					Data: json.RawMessage(`{"op":"shutdown"}`),
				},
			})
		}
		close(e.done)
	})
	select {
	case <-e.closed:
		return nil
	case <-ctx.Done():
		// A queued drain alone is bounded, but an in-flight HTTP request could
		// otherwise remain owned by its client timeout after Shutdown returns.
		// Cancel the lifecycle context first, then wait for the worker to exit.
		e.exportCancel()
		<-e.closed
		return ctx.Err()
	}
}

func (e *SessionTelemetryExporter) run() {
	defer close(e.closed)
	ticker := time.NewTicker(telemetryBatchWait)
	defer ticker.Stop()
	batch := make([]telemetryRecord, 0, telemetryBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		_ = e.exportWithRetry(e.exportCtx, batch)
		batch = batch[:0]
	}
	for {
		select {
		case record := <-e.queue:
			batch = append(batch, record)
			if len(batch) >= telemetryBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-e.done:
			for {
				select {
				case record := <-e.queue:
					batch = append(batch, record)
					if len(batch) >= telemetryBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

type otlpLogs struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}
type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}
type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}
type otlpScopeLogs struct {
	Scope      map[string]string `json:"scope"`
	LogRecords []otlpLogRecord   `json:"logRecords"`
}
type otlpLogRecord struct {
	TimeUnixNano         string          `json:"timeUnixNano"`
	ObservedTimeUnixNano string          `json:"observedTimeUnixNano"`
	SeverityNumber       int             `json:"severityNumber"`
	SeverityText         string          `json:"severityText"`
	Body                 map[string]any  `json:"body"`
	Attributes           []otlpAttribute `json:"attributes"`
}

type telemetryHTTPError struct{ status int }

func (e *telemetryHTTPError) Error() string {
	return fmt.Sprintf("collector returned HTTP %d", e.status)
}

func (e *SessionTelemetryExporter) exportWithRetry(ctx context.Context, batch []telemetryRecord) error {
	var last error
	for attempt := 1; attempt <= telemetryExportMaxAttempts; attempt++ {
		last = e.export(ctx, batch)
		if last == nil {
			return nil
		}
		var httpErr *telemetryHTTPError
		if errors.As(last, &httpErr) && httpErr.status < 500 && httpErr.status != http.StatusTooManyRequests {
			return last
		}
		if attempt == telemetryExportMaxAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * telemetryExportRetryBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}

type otlpAttribute struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func (e *SessionTelemetryExporter) export(ctx context.Context, batch []telemetryRecord) error {
	if len(batch) == 0 {
		return nil
	}
	resourceAttributes := []otlpAttribute{
		{Key: "service.name", Value: stringValue(e.serviceName)},
		{Key: "service.version", Value: stringValue(e.serviceVersion)},
	}
	if e.userID != "" {
		resourceAttributes = append(resourceAttributes, otlpAttribute{Key: "user.id", Value: stringValue(e.userID)})
	}
	ledger := make([]otlpLogRecord, 0, len(batch))
	ops := make([]otlpLogRecord, 0, 1)
	for _, item := range batch {
		var decoded any
		if err := json.Unmarshal(item.event.Data, &decoded); err == nil {
			decoded = redactSecretValue(decoded)
		}
		body := any(map[string]any{"type": item.event.Type, "seq": item.event.Seq, "version": item.event.Version, "data": json.RawMessage(item.event.Data)})
		if decoded != nil {
			body = map[string]any{"type": item.event.Type, "seq": item.event.Seq, "version": item.event.Version, "data": decoded}
		}
		attrs := []otlpAttribute{
			{Key: "session.id", Value: stringValue(item.sessionID)},
			{Key: "event.type", Value: stringValue(item.event.Type)},
			{Key: "event.seq", Value: intValue(item.event.Seq)},
		}
		if provider := telemetryProvider(item.event.Data); provider != "" {
			attrs = append(attrs, otlpAttribute{Key: "provider.id", Value: stringValue(provider)})
		}
		severity, severityText := 9, "INFO"
		if item.event.Type == session.EventToolResult || item.event.Type == session.EventTurnEnd {
			var payload map[string]any
			if json.Unmarshal(item.event.Data, &payload) == nil {
				if failed, ok := payload["isError"].(bool); ok && failed {
					severity, severityText = 17, "ERROR"
				}
				if status, ok := payload["status"].(string); ok && (status == "failed" || status == "cancelled") {
					severity, severityText = 17, "ERROR"
				}
			}
		}
		record := otlpLogRecord{
			TimeUnixNano: fmt.Sprintf("%d", item.event.At.UnixNano()), ObservedTimeUnixNano: fmt.Sprintf("%d", time.Now().UnixNano()),
			SeverityNumber: severity, SeverityText: severityText, Body: otlpAnyValue(body), Attributes: attrs,
		}
		if item.channel == "ops" {
			record.Attributes = append(record.Attributes, otlpAttribute{Key: "telemetry.op", Value: stringValue("shutdown")})
			ops = append(ops, record)
		} else {
			ledger = append(ledger, record)
		}
	}
	scopes := make([]otlpScopeLogs, 0, 2)
	if len(ledger) > 0 {
		scopes = append(scopes, otlpScopeLogs{Scope: map[string]string{"name": "shutu-agent/session-telemetry"}, LogRecords: ledger})
	}
	if len(ops) > 0 {
		scopes = append(scopes, otlpScopeLogs{Scope: map[string]string{"name": "shutu-agent/session-telemetry/ops"}, LogRecords: ops})
	}
	logs := otlpLogs{ResourceLogs: []otlpResourceLogs{{
		Resource: otlpResource{Attributes: resourceAttributes}, ScopeLogs: scopes,
	}}}
	payload, err := json.Marshal(logs)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &telemetryHTTPError{status: resp.StatusCode}
	}
	return nil
}

func stringValue(value string) map[string]any { return map[string]any{"stringValue": value} }

// telemetryProvider extracts the stable route identity from the canonical
// request/header event when present. It is diagnostic metadata only; the event
// body remains authoritative and unchanged.
func telemetryProvider(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var payload struct {
		Provider   string `json:"provider"`
		ProviderID string `json:"providerId"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	if provider := strings.TrimSpace(payload.Provider); provider != "" {
		return provider
	}
	return strings.TrimSpace(payload.ProviderID)
}

// redactSecretValue is the telemetry egress defense in depth. Producers already
// redaction credential-shaped diagnostics, but the exporter must not blindly
// serialize a legacy or third-party canonical payload that slipped one through.
// It deliberately keeps ordinary text (including user content) unchanged.
func redactSecretValue(value any) any {
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var redacted any
	if err := json.Unmarshal([]byte(llm.RedactDiagnostic(string(raw))), &redacted); err != nil {
		return value
	}
	return redacted
}
func intValue(value uint64) map[string]any {
	return map[string]any{"intValue": fmt.Sprintf("%d", value)}
}

func otlpAnyValue(value any) map[string]any {
	if raw, ok := value.(json.RawMessage); ok {
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			return otlpAnyValue(decoded)
		}
	}
	switch value := value.(type) {
	case map[string]any:
		return map[string]any{"kvlistValue": kvlist(value)}
	case []any:
		values := make([]map[string]any, 0, len(value))
		for _, item := range value {
			values = append(values, otlpAnyValue(item))
		}
		return map[string]any{"arrayValue": map[string]any{"values": values}}
	case string:
		return stringValue(value)
	case bool:
		return map[string]any{"boolValue": value}
	case float64:
		return map[string]any{"doubleValue": value}
	case float32:
		return map[string]any{"doubleValue": value}
	case int:
		return map[string]any{"intValue": fmt.Sprintf("%d", value)}
	case int64:
		return map[string]any{"intValue": fmt.Sprintf("%d", value)}
	case uint64:
		return intValue(value)
	case nil:
		return map[string]any{"stringValue": "null"}
	default:
		encoded, _ := json.Marshal(value)
		return map[string]any{"stringValue": string(encoded)}
	}
}

func kvlist(object map[string]any) []map[string]any {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	// JSON object order is not semantically meaningful, but stable payloads make
	// collector tests and incident inspection deterministic.
	sortStrings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{"key": key, "value": otlpAnyValue(object[key])})
	}
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
