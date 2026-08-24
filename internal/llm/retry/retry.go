// Package retry provides the single request-level retry policy used by all
// providers. Retries happen only while Stream is establishing a stream; a
// partially consumed stream is never replayed and duplicated to the user.
package retry

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

const (
	DefaultMaxRetries = 2
	DefaultBackoff    = 500 * time.Millisecond
	DefaultMaxBackoff = 8 * time.Second
)

// Config controls request-level retry behavior. MaxRetries counts retries
// after the initial attempt. Non-positive MaxRetries uses the safe default.
type Config struct {
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Retryable      func(error) bool
}

func (c Config) normalized() Config {
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
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

type provider struct {
	inner llm.Provider
	cfg   Config
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

func (p *provider) Stream(ctx context.Context, req llm.ChatRequest) (llm.StreamReader, error) {
	events := make([]llm.RetryEvent, 0)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reader, err := p.inner.Stream(ctx, req)
		if err == nil {
			return &readerWithRetryInfo{StreamReader: reader, attempts: attempt, events: events}, nil
		}
		if attempt > p.cfg.MaxRetries || !p.cfg.Retryable(err) {
			if len(events) > 0 {
				return nil, &Error{Cause: err, AttemptsCount: attempt, Events: events}
			}
			return nil, err
		}
		delay := backoff(p.cfg, attempt)
		events = append(events, llm.RetryEvent{
			Attempt: attempt, MaxRetries: p.cfg.MaxRetries,
			DelayMS: delay.Milliseconds(), Error: err.Error(),
		})
		if err := wait(ctx, delay); err != nil {
			return nil, &Error{Cause: err, AttemptsCount: attempt, Events: events}
		}
	}
}

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
		return c.MaxBackoff
	}
	return d
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

// IsRetryable retries transient transport failures and HTTP 429/5xx errors,
// while failing closed for authentication, invalid-request and other 4xx
// responses.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
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
