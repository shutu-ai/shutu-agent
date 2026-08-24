package retry

import (
	"context"
	"errors"
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
