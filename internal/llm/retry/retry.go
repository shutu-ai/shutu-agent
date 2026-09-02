// Package retry provides the single request-level retry policy used by all
// providers. Retries happen only while Stream is establishing a stream; a
// partially consumed stream is never replayed and duplicated to the user.
package retry

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

const (
	DefaultMaxRetries  = 5
	DefaultBackoff     = 500 * time.Millisecond
	DefaultMaxBackoff  = 10 * time.Second
	DefaultJitterRatio = 0.1
)

// Config controls request-level retry behavior. MaxRetries counts retries
// after the initial attempt. Non-positive MaxRetries uses the safe default.
type Config struct {
	// Mode is "normal" (bounded transient retries) or "always" (unbounded
	// recovery until cancellation). Empty selects normal.
	Mode       string
	MaxRetries int
	// MaxRetriesSet preserves an explicit route policy value of zero. It is
	// false for the legacy Go config shape, where zero means “use defaults”.
	MaxRetriesSet  bool
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	JitterRatio    float64
	Retryable      func(error) bool
	RetryableCodes []string
}

func (c Config) normalized() Config {
	if !c.MaxRetriesSet && c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.Mode == "" {
		c.Mode = "normal"
	}
	if c.Mode != "normal" && c.Mode != "always" {
		c.Mode = "normal"
	}
	if c.RetryableCodes == nil {
		c.RetryableCodes = []string{"EMPTY_RESPONSE", "RATE_LIMIT", "SERVER", "TIMEOUT", "TRANSPORT"}
	}
	if c.JitterRatio < 0 || c.JitterRatio > 1 {
		c.JitterRatio = DefaultJitterRatio
	}
	if c.Retryable == nil {
		c.Retryable = IsRetryable
	}
	return c
}

// WrapProvider applies the common policy while preserving provider identity
// and availability for the registry.
func WrapProvider(p llm.Provider, cfg Config) llm.Provider {
	if p == nil {
		return nil
	}
	return &provider{inner: p, cfg: cfg.normalized()}
}

// WrapProviderForLoop publishes the provider policy but leaves retry execution
// to the Agent loop. This is the production path: each failed request is
// visible at the durable step boundary, so retry scheduling cannot hide a
// provider attempt inside one Stream call. WrapProvider remains the legacy
// request-level compatibility API for embedders that explicitly want it.
func WrapProviderForLoop(p llm.Provider, cfg Config) llm.Provider {
	if p == nil {
		return nil
	}
	return &provider{inner: p, cfg: cfg.normalized(), deferRetries: true}
}

type provider struct {
	inner        llm.Provider
	cfg          Config
	deferRetries bool
}

// Error preserves retry metadata when every request attempt fails. It still
// unwraps to the provider/context error for errors.Is/errors.As callers.
type Error struct {
	Cause         error
	AttemptsCount int
	Events        []llm.RetryEvent
}

func (e *Error) Error() string { return e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }
func (e *Error) Attempts() int { return e.AttemptsCount }
func (e *Error) RetryEvents() []llm.RetryEvent {
	return append([]llm.RetryEvent(nil), e.Events...)
}

func (p *provider) ID() string      { return p.inner.ID() }
func (p *provider) Available() bool { return p.inner.Available() }

func (p *provider) SupportsImages() bool {
	capability, ok := p.inner.(llm.ImageCapability)
	return ok && capability.SupportsImages()
}

// ListModels forwards the optional provider-owned catalog without replacing
// it with retry/config guesses. Catalog calls remain provider-controlled.
func (p *provider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	catalog, ok := p.inner.(llm.ModelCatalogProvider)
	if !ok {
		return nil, fmt.Errorf("%w: provider %q does not expose listModels", llm.ErrModelCatalogUnavailable, p.ID())
	}
	return catalog.ListModels(ctx)
}

// ResolveModelInfo forwards exact model resolution through the same wrapped
// provider generation used for streaming.
func (p *provider) ResolveModelInfo(ctx context.Context, model string) (llm.ModelInfo, error) {
	catalog, ok := p.inner.(llm.ModelCatalogProvider)
	if !ok {
		return llm.ModelInfo{}, fmt.Errorf("%w: provider %q does not expose resolveModelInfo", llm.ErrModelCatalogUnavailable, p.ID())
	}
	return catalog.ResolveModelInfo(ctx, model)
}

func (p *provider) RetryPolicy() llm.RetryPolicy {
	c := p.cfg
	codes := append([]string(nil), c.RetryableCodes...)
	return llm.RetryPolicy{Mode: c.Mode, MaxRetries: c.MaxRetries, RetryableCodes: codes,
		InitialDelayMS: c.InitialBackoff.Milliseconds(), MaxDelayMS: c.MaxBackoff.Milliseconds(), JitterRatio: c.JitterRatio}
}

func (p *provider) Close() error {
	if closeable, ok := p.inner.(llm.Closeable); ok {
		return closeable.Close()
	}
	return nil
}

func (p *provider) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	if p.deferRetries {
		return p.inner.Stream(ctx, req)
	}
	events := make([]llm.RetryEvent, 0)
	retryID := newRetryID()
	observer, _ := llm.RetryObserverFromContext(ctx)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reader, err := p.inner.Stream(ctx, req)
		if err == nil {
			return &readerWithRetryInfo{StreamReader: reader, attempts: attempt, events: events}, nil
		}
		if (p.cfg.Mode == "normal" && attempt > p.cfg.MaxRetries) ||
			(p.cfg.Mode != "always" && (!p.cfg.Retryable(err) || !retryCodeAllowed(err, p.cfg.RetryableCodes))) {
			if len(events) > 0 {
				return nil, &Error{Cause: err, AttemptsCount: attempt, Events: events}
			}
			return nil, err
		}
		failure, ok := llm.FailureFacts(err)
		if !ok {
			failure = llm.Failure{Message: llm.RedactDiagnostic(err.Error()), Code: "UNKNOWN"}
		}
		failure.Message = llm.RedactDiagnostic(failure.Message)
		delay := backoff(p.cfg, attempt)
		if failure.ProviderRetryAfterMS > 0 {
			if failure.ProviderRetryAfterMS > p.cfg.MaxBackoff.Milliseconds() {
				// A normal policy delegates an over-cap provider instruction to
				// the caller instead of silently retrying early. Always mode has
				// no such delegation boundary and falls back to local backoff.
				if p.cfg.Mode == "normal" {
					return nil, err
				}
			} else {
				delay = time.Duration(failure.ProviderRetryAfterMS) * time.Millisecond
			}
		}
		event := llm.RetryEvent{
			RetryID: retryID,
			Attempt: attempt, MaxRetries: func() int {
				if p.cfg.Mode == "always" {
					return 0
				}
				return p.cfg.MaxRetries
			}(),
			DelayMS: delay.Milliseconds(), Error: llm.RedactDiagnostic(err.Error()),
			Mode:      p.cfg.Mode,
			PolicyKey: policyKey(p.cfg),
			Failure:   &failure,
		}
		events = append(events, event)
		if observer != nil {
			if observeErr := observer.RetryScheduled(ctx, req, event); observeErr != nil {
				return nil, &Error{Cause: errors.Join(err, observeErr), AttemptsCount: attempt, Events: events}
			}
		}
		if err := wait(ctx, delay); err != nil {
			return nil, &Error{Cause: err, AttemptsCount: attempt, Events: events}
		}
		if observer != nil {
			if observeErr := observer.RetryStarted(ctx, req, event); observeErr != nil {
				return nil, &Error{Cause: observeErr, AttemptsCount: attempt, Events: events}
			}
		}
	}
}

// ShouldRetry applies the normalized provider policy's eligibility predicate.
// It is exported for the Agent loop, which owns the durable retry boundary.
func ShouldRetry(cfg Config, err error) bool {
	c := cfg.normalized()
	return c.Retryable(err) && retryCodeAllowed(err, c.RetryableCodes)
}

// BackoffFor returns the local bounded jittered delay for one retry number.
// attempt is one-based and counts the failed attempt being recovered.
func BackoffFor(cfg Config, attempt int) time.Duration {
	return backoff(cfg.normalized(), attempt)
}

// PolicyKeyFor returns the canonical behavior-affecting policy identity used
// by durable retry events and replay invariants.
func PolicyKeyFor(cfg Config) string { return policyKey(cfg.normalized()) }

// RetryEventFor builds the durable retry schedule facts for one provider
// failure. A normal policy delegates an over-cap Retry-After to the caller;
// always mode falls back to local backoff because it has no finite provider
// delay authorization boundary.
func RetryEventFor(cfg Config, retryID string, attempt int, err error) (llm.RetryEvent, bool) {
	c := cfg.normalized()
	// The reference always policy is the downstream recovery fallback for
	// every model-request failure. It must not inherit normal mode's transient
	// code allow-list (AUTH/REFUSAL/etc. are intentionally retryable there),
	// while cancellation remains terminal at the loop boundary.
	if c.Mode != "always" && !ShouldRetry(c, err) {
		return llm.RetryEvent{}, false
	}
	if c.Mode == "always" && (err == nil || errors.Is(err, context.Canceled)) {
		return llm.RetryEvent{}, false
	}
	if c.Mode == "normal" && attempt > c.MaxRetries {
		return llm.RetryEvent{}, false
	}
	failure, ok := llm.FailureFacts(err)
	if !ok {
		failure = llm.Failure{Message: llm.RedactDiagnostic(err.Error()), Code: "UNKNOWN"}
	}
	failure.Message = llm.RedactDiagnostic(failure.Message)
	delay := backoff(c, attempt)
	if failure.ProviderRetryAfterMS > 0 {
		if failure.ProviderRetryAfterMS > c.MaxBackoff.Milliseconds() {
			if c.Mode == "normal" {
				return llm.RetryEvent{}, false
			}
		} else {
			delay = time.Duration(failure.ProviderRetryAfterMS) * time.Millisecond
		}
	}
	maxRetries := c.MaxRetries
	if c.Mode == "always" {
		maxRetries = 0
	}
	return llm.RetryEvent{
		RetryID: retryID, Attempt: attempt, MaxRetries: maxRetries,
		DelayMS: delay.Milliseconds(), Error: llm.RedactDiagnostic(err.Error()), Mode: c.Mode,
		PolicyKey: policyKey(c), Failure: &failure,
	}, true
}

func policyKey(c Config) string {
	codes := append([]string(nil), c.RetryableCodes...)
	sort.Strings(codes)
	if c.Mode == "always" {
		encoded, _ := json.Marshal([]any{c.Mode, c.InitialBackoff.Milliseconds(), c.MaxBackoff.Milliseconds(), c.JitterRatio})
		return string(encoded)
	}
	encoded, _ := json.Marshal([]any{c.Mode, c.MaxRetries, codes, c.InitialBackoff.Milliseconds(), c.MaxBackoff.Milliseconds(), c.JitterRatio})
	return string(encoded)
}

func retryCodeAllowed(err error, codes []string) bool {
	if len(codes) == 0 {
		return true
	}
	failure, ok := llm.FailureFacts(err)
	if !ok {
		return true
	}
	for _, code := range codes {
		if code == failure.Code {
			return true
		}
	}
	return false
}

func newRetryID() string {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	// A process-local fallback is still non-empty and scoped to this request;
	// crypto/rand failure must not make retry observability disappear.
	return fmt.Sprintf("retry-%d", time.Now().UnixNano())
}

// NewRetryID allocates the stable identity shared by all scheduled attempts
// in one durable request-recovery chain.
func NewRetryID() string { return newRetryID() }

type readerWithRetryInfo struct {
	llm.StreamReader
	attempts int
	events   []llm.RetryEvent
}

func (r *readerWithRetryInfo) Attempts() int { return r.attempts }
func (r *readerWithRetryInfo) RetryEvents() []llm.RetryEvent {
	return append([]llm.RetryEvent(nil), r.events...)
}

func backoff(c Config, attempt int) time.Duration {
	d := c.InitialBackoff
	for i := 1; i < attempt && d < c.MaxBackoff; i++ {
		d *= 2
	}
	if d > c.MaxBackoff {
		d = c.MaxBackoff
	}
	if c.JitterRatio == 0 {
		return d
	}
	// Match DSH's symmetric jitter around the local exponential delay. The
	// result is rounded to milliseconds because retry lifecycle payloads expose
	// integer millisecond delays; zero is valid at the lower full-jitter bound.
	ms := float64(d.Milliseconds()) * (1 - c.JitterRatio + 2*c.JitterRatio*rand.Float64())
	if ms > float64(c.MaxBackoff.Milliseconds()) {
		ms = float64(c.MaxBackoff.Milliseconds())
	}
	return time.Duration(math.Round(ms)) * time.Millisecond
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WaitFor is the cancellation-aware backoff primitive used by the Agent loop
// when retry execution is deferred to the durable request-error boundary.
func WaitFor(ctx context.Context, d time.Duration) error { return wait(ctx, d) }

// IsRetryable retries transient transport failures and HTTP 429/5xx errors,
// while failing closed for authentication, invalid-request and other 4xx
// responses.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if failure, ok := llm.FailureFacts(err); ok {
		switch failure.Code {
		case "TRANSPORT", "SERVER", "RATE_LIMIT", "TIMEOUT":
			return true
		case "EMPTY_RESPONSE":
			return true
		case "AUTH", "QUOTA", "CONTEXT_WINDOW_EXCEEDED", "INVALID_REQUEST", "ABORTED":
			return false
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "429") || strings.Contains(s, " 500") || strings.Contains(s, " 502") ||
		strings.Contains(s, " 503") || strings.Contains(s, " 504") ||
		strings.Contains(s, "timeout") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "temporarily unavailable") {
		return true
	}
	return false
}

var _ llm.Provider = (*provider)(nil)
var _ llm.RetryInfo = (*readerWithRetryInfo)(nil)
