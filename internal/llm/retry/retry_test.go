package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
)

type fakeProvider struct {
	calls int
	errs  []error
}

func (p *fakeProvider) ID() string      { return "fake" }
func (p *fakeProvider) Available() bool { return true }
func (p *fakeProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	p.calls++
	if len(p.errs) > 0 {
		err := p.errs[0]
		p.errs = p.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	return fakeReader{}, nil
}

type fakeReader struct{}

func (fakeReader) Next() (llm.StreamEvent, error) {
	return llm.StreamEvent{Kind: llm.StreamFinish, Usage: llm.TokenUsage{TotalTokens: 3}}, nil
}

type retryObserverFixture struct {
	scheduled []llm.RetryEvent
	started   []llm.RetryEvent
	signal    chan struct{}
}

func (o *retryObserverFixture) RetryScheduled(_ context.Context, _ llm.ChatRequest, event llm.RetryEvent) error {
	o.scheduled = append(o.scheduled, event)
	if o.signal != nil {
		select {
		case o.signal <- struct{}{}:
		default:
		}
	}
	return nil
}

func (o *retryObserverFixture) RetryStarted(_ context.Context, _ llm.ChatRequest, event llm.RetryEvent) error {
	o.started = append(o.started, event)
	return nil
}

func TestWrapProviderRetriesTransientErrors(t *testing.T) {
	p := &fakeProvider{errs: []error{errors.New("upstream 503"), nil}}
	wrapped := WrapProvider(p, Config{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	reader, err := wrapped.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	info, ok := reader.(llm.RetryInfo)
	if !ok || info.Attempts() != 2 || len(info.RetryEvents()) != 1 {
		t.Fatalf("retry info = %#v, want attempts=2 and one event", info)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", p.calls)
	}
}

func TestRetryMetadataRedactsCredentialShapedDiagnostics(t *testing.T) {
	p := &fakeProvider{errs: []error{llm.NewFailureFactsError(llm.Failure{
		Message: `provider failed: authorization: Bearer super-secret`, Code: "SERVER",
	}, nil)}}
	wrapped := WrapProvider(p, Config{MaxRetries: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterRatio: 0})
	observer := &retryObserverFixture{}
	ctx := llm.WithRetryObserver(context.Background(), observer)
	if _, err := wrapped.Stream(ctx, llm.ChatRequest{}); err != nil {
		// The provider has no successful second response; the error is expected.
	}
	if len(observer.scheduled) != 1 {
		t.Fatalf("scheduled events = %d, want 1", len(observer.scheduled))
	}
	if got := observer.scheduled[0].Error; strings.Contains(got, "super-secret") || strings.Contains(got, "Bearer super-secret") {
		t.Fatalf("retry diagnostic leaked credential: %q", got)
	}
}

func TestWrapProviderDelegatesOverCapRetryAfterInNormalMode(t *testing.T) {
	p := &fakeProvider{errs: []error{llm.NewFailureFactsError(llm.Failure{
		Message: "wait", Code: "RATE_LIMIT", ProviderRetryAfterMS: 11,
	}, nil), nil}}
	wrapped := WrapProvider(p, Config{
		Mode: "normal", MaxRetries: 2, InitialBackoff: time.Millisecond,
		MaxBackoff: 10 * time.Millisecond, JitterRatio: 0,
		RetryableCodes: []string{"RATE_LIMIT"},
	})
	_, err := wrapped.Stream(context.Background(), llm.ChatRequest{})
	if err == nil {
		t.Fatal("over-cap Retry-After was retried in normal mode")
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want one delegated attempt", p.calls)
	}
}

func TestWrapProviderPublishesRetryLifecycleBeforeReturning(t *testing.T) {
	p := &fakeProvider{errs: []error{errors.New("upstream 503"), nil}}
	wrapped := WrapProvider(p, Config{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	observer := &retryObserverFixture{}
	ctx := llm.WithRetryObserver(context.Background(), observer)
	if _, err := wrapped.Stream(ctx, llm.ChatRequest{Provider: "fake", Model: "model"}); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if len(observer.scheduled) != 1 || len(observer.started) != 1 {
		t.Fatalf("retry lifecycle = scheduled %d started %d, want one of each", len(observer.scheduled), len(observer.started))
	}
	if observer.scheduled[0].RetryID == "" || observer.scheduled[0].RetryID != observer.started[0].RetryID {
		t.Fatalf("retry ids = %q and %q, want one shared id", observer.scheduled[0].RetryID, observer.started[0].RetryID)
	}
	if observer.scheduled[0].Failure == nil || observer.scheduled[0].Failure.Code != "UNKNOWN" {
		t.Fatalf("scheduled failure = %#v, want UNKNOWN facts", observer.scheduled[0].Failure)
	}
}

func TestWrapProviderExposesProviderOwnedPolicy(t *testing.T) {
	inner := &fakeProvider{errs: []error{llm.NewFailureError("busy", "SERVER", nil)}}
	wrapped := WrapProvider(inner, Config{Mode: "always", MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: 3 * time.Millisecond, JitterRatio: 0, RetryableCodes: []string{"SERVER"}})
	policy, ok := wrapped.(llm.RetryPolicyProvider)
	if !ok {
		t.Fatal("wrapped provider does not expose its retry policy")
	}
	got := policy.RetryPolicy()
	if got.Mode != "always" || got.MaxRetries != 2 || got.InitialDelayMS != 1 || got.MaxDelayMS != 3 || len(got.RetryableCodes) != 1 || got.RetryableCodes[0] != "SERVER" {
		t.Fatalf("policy = %+v, want captured provider policy", got)
	}
}

func TestExplicitZeroMaxRetriesIsPreservedForRoutePolicy(t *testing.T) {
	p := &fakeProvider{errs: []error{llm.NewFailureError("busy", "SERVER", nil)}}
	wrapped := WrapProvider(p, Config{
		Mode: "normal", MaxRetries: 0, MaxRetriesSet: true,
		InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		RetryableCodes: []string{"SERVER"},
	})
	policy, ok := wrapped.(llm.RetryPolicyProvider)
	if !ok || policy.RetryPolicy().MaxRetries != 0 {
		t.Fatalf("route policy = %+v, want maxRetries=0", policy.RetryPolicy())
	}
	if _, err := wrapped.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("zero-retry route unexpectedly retried")
	}
}

func TestWrapProviderDoesNotPublishStartedAfterCancelledBackoff(t *testing.T) {
	p := &fakeProvider{errs: []error{errors.New("upstream 503")}}
	wrapped := WrapProvider(p, Config{MaxRetries: 2, InitialBackoff: time.Hour})
	observer := &retryObserverFixture{signal: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = llm.WithRetryObserver(ctx, observer)
	done := make(chan error, 1)
	go func() {
		_, err := wrapped.Stream(ctx, llm.ChatRequest{})
		done <- err
	}()
	<-observer.signal
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	if len(observer.started) != 0 {
		t.Fatalf("started events = %d, want none after cancelled backoff", len(observer.started))
	}
}

func TestWrapProviderDoesNotRetryClientErrors(t *testing.T) {
	p := &fakeProvider{errs: []error{errors.New("upstream 401")}}
	wrapped := WrapProvider(p, Config{MaxRetries: 2, InitialBackoff: time.Millisecond})
	if _, err := wrapped.Stream(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("Stream() error = nil, want 401 error")
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
}

func TestWrapProviderPreservesExhaustedRetryMetadata(t *testing.T) {
	p := &fakeProvider{errs: []error{errors.New("upstream 503"), errors.New("upstream 503"), errors.New("upstream 503")}}
	wrapped := WrapProvider(p, Config{MaxRetries: 2, InitialBackoff: time.Millisecond})
	_, err := wrapped.Stream(context.Background(), llm.ChatRequest{})
	if err == nil {
		t.Fatal("Stream() error = nil, want exhausted retry error")
	}
	info, ok := err.(llm.RetryInfo)
	if !ok || info.Attempts() != 3 || len(info.RetryEvents()) != 2 {
		t.Fatalf("retry failure info = %#v, want attempts=3 and two events", info)
	}
}

func TestWrapProviderCancellationBreaksBackoff(t *testing.T) {
	p := &fakeProvider{errs: []error{errors.New("upstream 503")}}
	wrapped := WrapProvider(p, Config{MaxRetries: 2, InitialBackoff: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := wrapped.Stream(ctx, llm.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	if p.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", p.calls)
	}
}

func TestIsRetryableUsesStructuredFailureCode(t *testing.T) {
	tests := []struct {
		code string
		want bool
	}{
		{"SERVER", true},
		{"TRANSPORT", true},
		{"RATE_LIMIT", true},
		{"TIMEOUT", true},
		{"EMPTY_RESPONSE", true},
		{"QUOTA", false},
		{"AUTH", false},
		{"INVALID_REQUEST", false},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			if got := IsRetryable(llm.NewFailureError("provider failure", tc.code, nil)); got != tc.want {
				t.Fatalf("IsRetryable(%s) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}
