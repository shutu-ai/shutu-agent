package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/credential"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/llm/anthropic"
	"github.com/jabing/shutu-agent/internal/llm/deepseek"
	"github.com/jabing/shutu-agent/internal/llm/google"
	"github.com/jabing/shutu-agent/internal/llm/openai"
	"github.com/jabing/shutu-agent/internal/llm/openairesponses"
	llmretry "github.com/jabing/shutu-agent/internal/llm/retry"
	"github.com/jabing/shutu-agent/internal/store"
)

type generationTestProvider struct {
	id       string
	attempts atomic.Int32
	closed   atomic.Int32
}

func (p *generationTestProvider) ID() string      { return p.id }
func (p *generationTestProvider) Available() bool { return true }
func (p *generationTestProvider) Close() error {
	p.closed.Add(1)
	return nil
}
func (p *generationTestProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	p.attempts.Add(1)
	return &generationTestReader{}, nil
}

type unavailableTestProvider struct {
	id       string
	attempts atomic.Int32
}

func (p *unavailableTestProvider) ID() string      { return p.id }
func (p *unavailableTestProvider) Available() bool { return false }
func (p *unavailableTestProvider) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	p.attempts.Add(1)
	return nil, errors.New("must not stream")
}

type generationTestReader struct{ done bool }

func (r *generationTestReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	r.done = true
	return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: "ok"}, nil
}

// TestProviderWireFailureAndPolicyMatrixCoversEveryProtocolFamily binds every
// wire adapter to the shared normalized HTTP failure vocabulary and to one
// provider-scoped retry policy at composition time. Production adapters
// deliberately disable private retries; the loop executes the route policy.
func TestProviderWireFailureAndPolicyMatrixCoversEveryProtocolFamily(t *testing.T) {
	secret := "wire-matrix-secret"
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "2")
		w.Header().Set("X-Request-ID", "request-matrix")
		w.Header().Set("Request-ID", "request-matrix")
		w.Header().Set("X-DeepSeek-Request-ID", "request-matrix")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary provider failure","api_key":"` + secret + `"}}`))
	}))
	defer srv.Close()

	newProvider := func(t *testing.T, family string) llm.Provider {
		t.Helper()
		switch family {
		case "deepseek-completions":
			return deepseek.New(deepseek.Config{
				ProviderName: family, BaseURL: srv.URL, APIKey: secret,
				DisableRetry: true,
			})
		case "openai-completions":
			return openai.New(openai.Config{
				ID: family, BaseURL: srv.URL, APIKey: secret,
				DisableRetry: true,
			})
		case "anthropic-messages":
			return anthropic.New(anthropic.Config{
				ID: family, BaseURL: srv.URL, APIKey: secret,
			})
		case "google-generative-ai":
			return google.New(google.Config{
				ID: family, BaseURL: srv.URL, APIKey: secret,
			})
		case "openai-responses":
			return openairesponses.New(openairesponses.Config{
				ID: family, BaseURL: srv.URL, APIKey: secret,
			})
		default:
			t.Fatalf("unknown protocol family %q", family)
			return nil
		}
	}

	cfg := config.Config{}
	cfg.LLM.Retry.Mode = "normal"
	cfg.LLM.Retry.MaxRetries = 7
	cfg.LLM.Retry.InitialBackoff = config.Duration{Duration: 3 * time.Millisecond}
	cfg.LLM.Retry.MaxBackoff = config.Duration{Duration: 9 * time.Millisecond}
	cfg.LLM.Retry.JitterRatio = 0.25
	cfg.LLM.Retry.RetryableCodes = []string{"SERVER", "TIMEOUT", "TRANSPORT"}

	for _, family := range []string{
		"deepseek-completions", "openai-completions", "anthropic-messages",
		"google-generative-ai", "openai-responses",
	} {
		t.Run(family, func(t *testing.T) {
			routeID := family
			wireLabel := family
			if family == "deepseek-completions" {
				routeID = "deepseek-official"
			} else if family == "anthropic-messages" {
				wireLabel = "anthropic"
			} else if family == "google-generative-ai" {
				wireLabel = "google"
			} else if family == "openai-responses" {
				wireLabel = "openairesponses"
			}
			mode := cfg.LLM.Retry.Mode
			maxRetries := cfg.LLM.Retry.MaxRetries
			initial := cfg.LLM.Retry.InitialBackoff
			maxBackoff := cfg.LLM.Retry.MaxBackoff
			jitter := cfg.LLM.Retry.JitterRatio
			codes := append([]string(nil), cfg.LLM.Retry.RetryableCodes...)
			cfg.LLM.Retry.Providers = map[string]config.RetryProviderConfig{
				routeID: {
					Mode: &mode, MaxRetries: &maxRetries,
					InitialBackoff: &initial, MaxBackoff: &maxBackoff,
					JitterRatio: &jitter, RetryableCodes: &codes,
				},
			}

			provider := wrapProvider(newProvider(t, family), &cfg)
			if provider.ID() != routeID {
				t.Fatalf("wrapped provider ID = %q, want %q", provider.ID(), routeID)
			}
			policyProvider, ok := provider.(llm.RetryPolicyProvider)
			if !ok {
				t.Fatalf("%s provider does not publish a route policy", family)
			}
			policy := policyProvider.RetryPolicy()
			wantPolicy := llm.RetryPolicy{
				Mode: "normal", MaxRetries: 7, RetryableCodes: codes,
				InitialDelayMS: 3, MaxDelayMS: 9, JitterRatio: 0.25,
			}
			if !reflect.DeepEqual(policy, wantPolicy) {
				t.Fatalf("%s route policy = %+v, want %+v", family, policy, wantPolicy)
			}

			before := requests.Load()
			_, err := provider.Stream(context.Background(), llm.ChatRequest{
				Messages: []llm.Message{{
					Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("wire failure matrix")},
				}},
			})
			if err == nil {
				t.Fatalf("%s unexpectedly accepted rate-limited response", family)
			}
			if got := requests.Load(); got != before+1 {
				t.Fatalf("%s made %d requests for one disabled private retry, want one", family, got-before)
			}
			facts, ok := llm.FailureFacts(err)
			if !ok {
				t.Fatalf("%s error is not a typed llm failure: %v", family, err)
			}
			wantFacts := llm.Failure{
				Message:              facts.Message,
				Code:                 "RATE_LIMIT",
				Status:               http.StatusTooManyRequests,
				ProviderRetryAfterMS: 2_000,
				RequestID:            "request-matrix",
			}
			if facts.Code != wantFacts.Code || facts.Status != wantFacts.Status ||
				facts.ProviderRetryAfterMS != wantFacts.ProviderRetryAfterMS ||
				facts.RequestID != wantFacts.RequestID {
				t.Fatalf("%s normalized failure = %+v, want code/status/retry/request %+v", family, facts, wantFacts)
			}
			if !strings.Contains(facts.Message, wireLabel) || !strings.Contains(facts.Message, "temporary provider failure") {
				t.Fatalf("%s failure lost provider/failure context: %+v", family, facts)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s wire diagnostic leaked credential: %q", family, err)
			}

			// RATE_LIMIT deliberately falls outside this route's configured
			// policy. The decision must be made from the shared typed code,
			// never by re-parsing provider-specific text.
			retryConfig := llmretry.Config{
				Mode: "normal", MaxRetries: 7, MaxRetriesSet: true,
				InitialBackoff: 3 * time.Millisecond, MaxBackoff: 9 * time.Millisecond,
				JitterRatio: 0.25, RetryableCodes: codes,
			}
			if llmretry.ShouldRetry(retryConfig, err) {
				t.Fatalf("%s RATE_LIMIT unexpectedly matched route policy %+v", family, retryConfig)
			}
		})
	}
}

type generationFinishReader struct{ done bool }

func (r *generationFinishReader) Next() (llm.StreamEvent, error) {
	if r.done {
		return llm.StreamEvent{}, io.EOF
	}
	r.done = true
	return llm.StreamEvent{Kind: llm.StreamFinish, FinishReason: "stop"}, nil
}

func TestProviderGenerationClosesAfterLastStreamLease(t *testing.T) {
	provider := &generationTestProvider{id: "deepseek-official"}
	reg := llm.NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: config.Config{LLM: config.LLMConfig{Provider: provider.id}}, llm: provider, llmReg: reg}
	generation := &providerGeneration{registry: reg}
	a.providerGeneration = generation

	reader, err := a.llmFor(provider.id).Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	generation.retire()
	if got := provider.closed.Load(); got != 0 {
		t.Fatalf("provider closed while stream was still live: %d", got)
	}
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream termination error = %v, want EOF", err)
	}
	if got := provider.closed.Load(); got != 1 {
		t.Fatalf("provider close count = %d, want 1", got)
	}
}

func TestProviderGenerationReleasesAtStreamFinish(t *testing.T) {
	provider := &generationTestProvider{id: "deepseek-official"}
	reg := llm.NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	generation := &providerGeneration{registry: reg}
	routed := &leasedLLM{inner: provider, gen: generation}
	// Replace the normal reader with a finish-only reader to model consumers
	// that stop at StreamFinish and never probe EOF.
	routed.inner = finishOnlyLLM{reader: &generationFinishReader{}}
	reader, err := routed.Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	generation.retire()
	if got := provider.closed.Load(); got != 0 {
		t.Fatalf("provider closed while stream was still live: %d", got)
	}
	if event, err := reader.Next(); err != nil || event.Kind != llm.StreamFinish {
		t.Fatalf("finish = %+v, err=%v", event, err)
	}
	if got := provider.closed.Load(); got != 1 {
		t.Fatalf("provider close count after finish = %d, want 1", got)
	}
}

// TestRetiredProviderGenerationRejectsNewStreamAndDrainsOldLease proves both
// halves of the replacement contract: a retired credential-bearing generation
// cannot acquire a new stream, while an already-acquired in-flight stream keeps
// the old provider alive exactly until its terminal stream boundary.
func TestRetiredProviderGenerationRejectsNewStreamAndDrainsOldLease(t *testing.T) {
	provider := &generationTestProvider{id: "retired-route"}
	reg := llm.NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: config.Config{LLM: config.LLMConfig{Provider: provider.id}}, llm: provider, llmReg: reg}
	generation := &providerGeneration{registry: reg}
	a.providerGeneration = generation

	oldReader, err := a.llmFor(provider.id).Stream(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	generation.retire()

	var retiredErr error
	retiredReader, retiredErr := a.llmFor(provider.id).Stream(context.Background(), llm.ChatRequest{})
	if retiredReader != nil || retiredErr == nil || !strings.Contains(retiredErr.Error(), "provider generation retired") {
		t.Fatalf("retired stream = reader=%#v err=%v, want generation rejection", retiredReader, retiredErr)
	}
	if got := provider.attempts.Load(); got != 1 {
		t.Fatalf("provider attempts after retirement = %d, want 1", got)
	}
	if got := provider.closed.Load(); got != 0 {
		t.Fatalf("provider closed while old lease was live: %d", got)
	}

	if _, err := oldReader.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := oldReader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("old stream termination error = %v, want EOF", err)
	}
	if got := provider.closed.Load(); got != 1 {
		t.Fatalf("provider close count after drain = %d, want 1", got)
	}
}

type finishOnlyLLM struct{ reader llm.StreamReader }

func (l finishOnlyLLM) Stream(context.Context, llm.ChatRequest) (llm.StreamReader, error) {
	return l.reader, nil
}

func TestProviderRuntimeSnapshotKeepsProviderAndRegistryGenerationTogether(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	a := &app{cfg: config.Config{
		Model: "deepseek-chat",
		LLM: config.LLMConfig{
			Provider: "deepseek-official",
			OpenAI:   config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
		},
	}}
	if err := a.registerLLM(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime := a.providerRuntimeSnapshot("")
			if runtime.selected == nil || runtime.selectedID != runtime.provider {
				t.Errorf("inconsistent runtime snapshot: provider=%q selected=%v", runtime.provider, runtime.selected)
				return
			}
		}
	}()
	for i := 0; i < 12; i++ {
		if err := a.webSwitchModel(context.Background(), "openai", "gpt-4o-mini", ""); err != nil {
			t.Fatal(err)
		}
		if err := a.webSwitchModel(context.Background(), "deepseek-official", "deepseek-chat", ""); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestProviderRuntimeSnapshotRejectsUnavailableRouteBeforeStream(t *testing.T) {
	provider := &unavailableTestProvider{id: "dormant"}
	reg := llm.NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	generation := &providerGeneration{registry: reg}
	a := &app{
		cfg:                config.Config{LLM: config.LLMConfig{Provider: "dormant"}},
		llm:                provider,
		llmReg:             reg,
		providerGeneration: generation,
	}

	pinned := a.providerRuntimeSnapshotPinned("dormant")
	unavailable, ok := pinned.selected.(unavailableLLM)
	if !ok {
		t.Fatalf("pinned selected = %T, want unavailableLLM", pinned.selected)
	}
	if !errors.Is(unavailable.err, llm.ErrProviderUnavailable) || pinned.selectedID != "" {
		t.Fatalf("pinned route error = %v, selectedID=%q", unavailable.err, pinned.selectedID)
	}
	if pinned.release != nil {
		t.Fatal("unavailable route acquired a generation lease")
	}

	snapshot := a.providerRuntimeSnapshot("dormant")
	if snapshot.selectedID != "" {
		t.Fatalf("non-pinned snapshot selected unavailable route: %+v", snapshot)
	}
	if _, err := snapshot.selected.Stream(context.Background(), llm.ChatRequest{}); !errors.Is(err, llm.ErrProviderUnavailable) {
		t.Fatalf("non-pinned stream error = %v, want ErrProviderUnavailable", err)
	}
	if _, err := a.llmFor("dormant").Stream(context.Background(), llm.ChatRequest{}); !errors.Is(err, llm.ErrProviderUnavailable) {
		t.Fatalf("routed stream error = %v, want ErrProviderUnavailable", err)
	}
	if got := provider.attempts.Load(); got != 0 {
		t.Fatalf("provider stream attempts = %d, want 0", got)
	}
}

// boolPtr returns a pointer to v (test helper for *bool config fields, e.g.
// llm.multimodal.enabled which defaults to on — 用户拍板「图片附件默认打开」).
func boolPtr(v bool) *bool { return &v }

// TestRegisterLLMDefaultDeepseekRegression verifies the M8-2 default-provider
// regression (dispatch-m8-2 §7): with no OPENAI_API_KEY only deepseek is
// registered, and with the default llm.provider (deepseek) the selected
// provider is injected into a.llm — behavior identical to before M8-2.
func TestRegisterLLMDefaultDeepseekRegression(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	if a.llm == nil || a.llmReg == nil {
		t.Fatal("registerLLM must set a.llm and a.llmReg")
	}

	// Only deepseek is registered.
	ids := make([]string, 0, 1)
	for _, p := range a.llmReg.List() {
		ids = append(ids, p.ID())
	}
	if len(ids) != 1 || ids[0] != "deepseek-official" {
		t.Fatalf("registered providers = %v, want [deepseek]", ids)
	}

	// The selected (default) provider is deepseek and it is the injected llm.
	sel, err := a.llmReg.Get(a.cfg.LLM.Provider)
	if err != nil {
		t.Fatalf("Get %q: %v", a.cfg.LLM.Provider, err)
	}
	if !sel.Available() {
		t.Fatal("deepseek must be available with DEEPSEEK_API_KEY set")
	}
	if a.llm != sel {
		t.Fatal("a.llm must be the selected provider")
	}
}

// TestRegisterLLMRegistersOpenaiWhenKeyPresent verifies the openai registration
// gate (dispatch-m8-2 §6): when OPENAI_API_KEY is present the openai provider
// is registered too and can be selected.
func TestRegisterLLMRegistersOpenaiWhenKeyPresent(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "dk")
	t.Setenv("OPENAI_API_KEY", "ok")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider: "openai",
				OpenAI:   config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini"},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, p := range a.llmReg.List() {
		ids = append(ids, p.ID())
	}
	if len(ids) != 2 || ids[0] != "deepseek-official" || ids[1] != "openai" {
		t.Fatalf("registered providers = %v, want [deepseek openai]", ids)
	}
	sel, err := a.llmReg.Get("openai")
	if err != nil {
		t.Fatalf("Get openai: %v", err)
	}
	if !sel.Available() {
		t.Fatal("openai must be available with OPENAI_API_KEY set")
	}
	if a.llm != sel {
		t.Fatal("a.llm must be the selected openai provider")
	}
}

// TestRegisterLLMUnknownProviderFailsClosed verifies the fail-closed startup
// rule (dispatch-m8-2 §5/§6/§7): an unknown llm.provider errors instead of
// silently falling back.
func TestRegisterLLMUnknownProviderFailsClosed(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{cfg: config.Config{LLM: config.LLMConfig{Provider: "nope"}}}
	if err := a.registerLLM(); err == nil {
		t.Fatal("unknown llm.provider must fail closed at startup")
	} else if !strings.Contains(err.Error(), "no such provider") {
		t.Errorf("err = %q, want the registry no-such-provider error", err)
	}
	if a.llm != nil {
		t.Fatal("a.llm must stay nil when registration fails")
	}
}

// TestRegisterLLMSelectedProviderUnavailableFailsClosed verifies the selected
// provider's credential gate: a missing key for the selected provider is a
// fail-closed startup error — preserving the M8-1-before behavior of a missing
// DEEPSEEK_API_KEY failing at startup, made provider-aware (纪律 6).
func TestRegisterLLMSelectedProviderUnavailableFailsClosed(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{cfg: config.Config{LLM: config.LLMConfig{Provider: "deepseek-official"}}}
	if err := a.registerLLM(); err == nil {
		t.Fatal("selected deepseek with no DEEPSEEK_API_KEY must fail closed at startup")
	} else if !strings.Contains(err.Error(), "not available") {
		t.Errorf("err = %q, want the not-available message", err)
	}
}

// TestLLMStatusOutput verifies the /llm-status report (dispatch-m8-2 §6/§7):
// the selected provider marked *, availability per registered provider, and the
// modalities line.
func TestLLMStatusOutput(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider: "openai",
				OpenAI:   config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini"},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	out := captureStdout(func() {
		if err := a.llmStatus(); err != nil {
			t.Errorf("llmStatus: %v", err)
		}
	})
	if !strings.Contains(out, "llm: enabled") {
		t.Errorf("output missing header: %q", out)
	}
	if !strings.Contains(out, "* openai: available (model=gpt-4o-mini)") {
		t.Errorf("output missing selected openai line: %q", out)
	}
	if !strings.Contains(out, "deepseek-official: available (model=deepseek-chat)") {
		t.Errorf("output missing deepseek-official line: %q", out)
	}
	if !strings.Contains(out, "modalities: text") {
		t.Errorf("output missing modalities line: %q", out)
	}
	if !strings.Contains(out, "multimodal: disabled") {
		t.Errorf("output missing multimodal disabled line (D10 default): %q", out)
	}
}

// TestLLMStatusShowsUnavailableProvider verifies an unconfigured (keyless)
// registered provider is reported as unavailable (dispatch-m8-2 §6: 未配置的
// provider 显示 unavailable). The deepseek provider is always registered; with
// no DEEPSEEK_API_KEY it stays in the registry and shows as unavailable while
// the selected openai provider (OPENAI_API_KEY set) is active.
func TestLLMStatusShowsUnavailableProvider(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider: "openai",
				OpenAI:   config.OpenAIProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini"},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	out := captureStdout(func() {
		if err := a.llmStatus(); err != nil {
			t.Errorf("llmStatus: %v", err)
		}
	})
	if !strings.Contains(out, "* openai: available (model=gpt-4o-mini)") {
		t.Errorf("output missing selected openai line: %q", out)
	}
	if !strings.Contains(out, "deepseek-official: unavailable") {
		t.Errorf("output must show deepseek-official as unavailable: %q", out)
	}
}

// TestLLMStatusWithoutRegistry verifies /llm-status before registerLLM reports
// the missing registry instead of panicking.
func TestLLMStatusWithoutRegistry(t *testing.T) {
	a := &app{}
	out := captureStdout(func() {
		if err := a.llmStatus(); err != nil {
			t.Errorf("llmStatus: %v", err)
		}
	})
	if !strings.Contains(out, "no provider registry") {
		t.Errorf("output = %q, want the no-registry report", out)
	}
}

// TestRegisterLLMRegistersAnthropicWhenKeyPresent verifies the anthropic
// registration gate (dispatch-m8-2b §3): when ANTHROPIC_API_KEY is present the
// anthropic provider is registered too and can be selected.
func TestRegisterLLMRegistersAnthropicWhenKeyPresent(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "dk")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider:  "anthropic",
				Anthropic: config.AnthropicProviderConfig{BaseURL: "https://api.anthropic.com/v1", Model: "claude-sonnet-4-5"},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, p := range a.llmReg.List() {
		ids = append(ids, p.ID())
	}
	if len(ids) != 2 || ids[0] != "deepseek-official" || ids[1] != "anthropic" {
		t.Fatalf("registered providers = %v, want [deepseek anthropic]", ids)
	}
	sel, err := a.llmReg.Get("anthropic")
	if err != nil {
		t.Fatalf("Get anthropic: %v", err)
	}
	if !sel.Available() {
		t.Fatal("anthropic must be available with ANTHROPIC_API_KEY set")
	}
	if a.llm != sel {
		t.Fatal("a.llm must be the selected anthropic provider")
	}
}

// TestRegisterLLMAnthropicNotRegisteredWithoutKey verifies the anthropic
// registration gate is key-gated (dispatch-m8-2b §3): without ANTHROPIC_API_KEY
// the anthropic provider is not registered, so selecting it fails closed.
func TestRegisterLLMAnthropicNotRegisteredWithoutKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "dk")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM:   config.LLMConfig{Provider: "anthropic"},
		},
	}
	if err := a.registerLLM(); err == nil {
		t.Fatal("selecting anthropic with no ANTHROPIC_API_KEY must fail closed")
	} else if !strings.Contains(err.Error(), "no such provider") {
		t.Errorf("err = %q, want the no-such-provider error", err)
	}
}

// TestLLMStatusShowsAnthropic verifies /llm-status includes the anthropic
// provider when registered (dispatch-m8-2b §3: /llm-status 自动涵盖 via the
// registry listing).
func TestLLMStatusShowsAnthropic(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "dk")
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider:  "anthropic",
				Anthropic: config.AnthropicProviderConfig{BaseURL: "https://api.anthropic.com/v1", Model: "claude-sonnet-4-5"},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	out := captureStdout(func() {
		if err := a.llmStatus(); err != nil {
			t.Errorf("llmStatus: %v", err)
		}
	})
	if !strings.Contains(out, "* anthropic: available (model=claude-sonnet-4-5)") {
		t.Errorf("output missing selected anthropic line: %q", out)
	}
	if !strings.Contains(out, "deepseek-official: available (model=deepseek-chat)") {
		t.Errorf("output missing deepseek-official line: %q", out)
	}
	if !strings.Contains(out, "modalities: text") {
		t.Errorf("output missing modalities line: %q", out)
	}
}

// TestLLMStatusMultimodalEnabled verifies the M8-3 /llm-status additions
// (dispatch-m8-3 §4): the modalities line reflects cfg.LLM.ModelInputModalities
// (text,image here) and the multimodal line reports enabled when
// llm.multimodal.enabled is true (vs the disabled default elsewhere).
func TestLLMStatusMultimodalEnabled(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	mm := true
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM: config.LLMConfig{
				Provider:             "deepseek-official",
				ModelInputModalities: "text,image",
				Multimodal:           config.MultimodalConfig{Enabled: &mm, MaxImageBytes: 1 << 20},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	out := captureStdout(func() {
		if err := a.llmStatus(); err != nil {
			t.Errorf("llmStatus: %v", err)
		}
	})
	if !strings.Contains(out, "modalities: text,image") {
		t.Errorf("output missing modalities text,image line: %q", out)
	}
	if !strings.Contains(out, "multimodal: enabled") {
		t.Errorf("output missing multimodal enabled line: %q", out)
	}
}

// TestLLMStatusMultimodalDisabledDefault verifies the D10 default shows
// multimodal: disabled even when a provider is registered and running.
func TestLLMStatusMultimodalDisabledDefault(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	out := captureStdout(func() {
		if err := a.llmStatus(); err != nil {
			t.Errorf("llmStatus: %v", err)
		}
	})
	if !strings.Contains(out, "multimodal: disabled") {
		t.Errorf("output missing multimodal disabled line: %q", out)
	}
	if !strings.Contains(out, "modalities: text") {
		t.Errorf("output missing modalities text line (fallback default): %q", out)
	}
}

// TestRegisterLLMDefaultDeepseekSupportsImagesFalse verifies the M8-3b wiring
// (dispatch-m8-3b §5/§7): with the default model_input_modalities=text the
// deepseek provider gets SupportsImages=false, so a request carrying an image
// fails closed at serialize time ("model does not support image input") — the
// default deepseek regression for the fail-closed contract.
func TestRegisterLLMDefaultDeepseekSupportsImagesFalse(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	p, err := a.llmReg.Get("deepseek-official")
	if err != nil {
		t.Fatalf("Get deepseek: %v", err)
	}
	// The fail-closed check runs before any network call or file read, so the
	// ImageRef needs only a media type (Path is never touched).
	_, err = p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png"}},
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "model does not support image input") {
		t.Fatalf("err = %v, want the fail-closed image error (modalities=text ⇒ SupportsImages=false)", err)
	}
}

// TestRegisterLLMSupportsImagesTrueWhenModalitiesImage verifies the positive
// wiring: with model_input_modalities=text,image the provider is wired with
// SupportsImages=true — the request proceeds past the fail-closed check and
// instead fails on the (missing) image file read, proving the check passed.
func TestRegisterLLMSupportsImagesTrueWhenModalitiesImage(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-chat",
			LLM:   config.LLMConfig{Provider: "deepseek-official", ModelInputModalities: "text,image"},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	p, err := a.llmReg.Get("deepseek-official")
	if err != nil {
		t.Fatalf("Get deepseek: %v", err)
	}
	_, err = p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Path: filepath.Join(t.TempDir(), "missing.png")}},
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "read image") {
		t.Fatalf("err = %v, want a read-image error (SupportsImages wired true, check passed)", err)
	}
}

// TestRegisterLLMWiresMaxRequestImageBytes verifies MaxRequestImageBytes flows
// from config through registerLLM into the provider: an image whose bytes
// exceed the configured request budget is offloaded to the placeholder before
// serialization (dispatch-m8-3b §5: 默认 20MiB 由 New 兜底, but an explicit small
// budget must be honored).
func TestRegisterLLMWiresMaxRequestImageBytes(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	a := &app{
		cfg: config.Config{
			Model:   "deepseek-chat",
			BaseURL: srv.URL,
			LLM: config.LLMConfig{
				Provider:             "deepseek-official",
				ModelInputModalities: "text,image",
				Multimodal: config.MultimodalConfig{
					Enabled:              boolPtr(true),
					MaxImageBytes:        1 << 20,
					MaxRequestImageBytes: 5, // tiny budget so the 100-byte image offloads
				},
			},
		},
	}
	if err := a.registerLLM(); err != nil {
		t.Fatalf("registerLLM: %v", err)
	}
	p, err := a.llmReg.Get("deepseek-official")
	if err != nil {
		t.Fatalf("Get deepseek: %v", err)
	}
	reader, err := p.Stream(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.Text("d"),
			{Kind: llm.BlockImage, Image: llm.ImageRef{MediaType: "image/png", Bytes: 100, Path: "does-not-matter"}},
		}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "d"+llm.OffloadedImageText {
		t.Fatalf("content = %v, want the offloaded text (MaxRequestImageBytes wired from config)", first["content"])
	}
}

// TestProviderGenerationColdRestartRebuildsDurableCustomRoute models the
// provider cold-restart boundary with SQLite: the first process owns a
// generation, drains and retires it exactly once, and closes its in-memory
// credential vault. A fresh app rebuilds the route only from durable settings
// and the credential backend, then streams through a real HTTP adapter.
func TestProviderGenerationColdRestartRebuildsDurableCustomRoute(t *testing.T) {
	const helperEnv = "PA_PROVIDER_COLD_RESTART_HELPER"
	if os.Getenv(helperEnv) == "1" {
		if err := runProviderColdRestartChild(); err != nil {
			fmt.Fprintln(os.Stderr, "provider cold-restart helper:", err)
			os.Exit(1)
		}
		return
	}
	var gotAuth atomic.Value
	var gotPath atomic.Value
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			gotBody.Store(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"cold\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "provider-cold.db")
	firstStore, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := customProviderProfile{
		ID: "cold-route", Name: "Cold Route", BaseURL: srv.URL, Model: "cold-model",
		DefaultMaxTokens: 2345,
	}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.SetSetting(ctx, "llm.custom.cold-route", string(profileJSON)); err != nil {
		t.Fatal(err)
	}
	firstVault, err := credential.New(ctx, firstStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstVault.Set(ctx, "COLD_ROUTE_API_KEY", "cold-secret"); err != nil {
		t.Fatal(err)
	}
	firstCfg := config.Config{
		BaseURL: "https://default.invalid/v1",
		Model:   "default-model",
		LLM:     config.LLMConfig{Provider: "cold-route"},
	}
	first := &app{
		cfg:             firstCfg,
		store:           firstStore,
		credentials:     firstVault,
		llmKeys:         map[string]string{},
		customProviders: []customProviderProfile{profile},
	}
	if err := first.registerLLM(); err != nil {
		t.Fatalf("first registerLLM: %v", err)
	}
	oldGeneration := first.providerGeneration
	if oldGeneration == nil {
		t.Fatal("first process has no provider generation")
	}
	releaseGeneration := func() {
		t.Helper()
		snapshot := first.providerRuntimeSnapshotPinned("cold-route")
		if snapshot.selectedID != "cold-route" {
			t.Fatalf("first selected route = %q, want cold-route", snapshot.selectedID)
		}
		snapshot.release()
	}
	releaseGeneration()

	// Model process teardown at the cold boundary. The old in-memory credential
	// is rejected after vault close, and the registry-backed generation is
	// retired exactly once.
	if err := firstVault.Close(); err != nil {
		t.Fatalf("close first credential vault: %v", err)
	}
	if _, err := firstVault.Resolve(ctx, "COLD_ROUTE_API_KEY"); err == nil {
		t.Fatal("closed vault unexpectedly resolved a credential")
	}
	oldGeneration.retire()
	oldGeneration.mu.Lock()
	retired, closed, refs := oldGeneration.retired, oldGeneration.closed, oldGeneration.refs
	oldGeneration.mu.Unlock()
	if !retired || !closed || refs != 0 {
		t.Fatalf("retired generation state = retired:%v closed:%v refs:%d", retired, closed, refs)
	}
	if err := first.closeProviderGenerations(); err != nil {
		t.Fatalf("idempotent provider teardown: %v", err)
	}
	if err := firstVault.Close(); err != nil {
		t.Fatalf("second vault close: %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(t.TempDir(), "child.pid")
	helper := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	helper.Env = append(os.Environ(),
		helperEnv+"=1",
		"PA_PROVIDER_COLD_DB="+dbPath,
		"PA_PROVIDER_COLD_RESULT="+resultPath,
	)
	output := &bytes.Buffer{}
	helper.Stdout = output
	helper.Stderr = output
	if err := helper.Run(); err != nil {
		t.Fatalf("provider cold-restart child: %v\n%s", err, output)
	}
	childPID, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("provider cold-restart result: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(childPID)) == fmt.Sprint(os.Getpid()) {
		t.Fatalf("provider cold restart ran in parent process %s", childPID)
	}
	if auth := gotAuth.Load(); auth != "Bearer cold-secret" {
		t.Fatalf("cold request auth = %#v, want the durable credential", auth)
	}
	if path := gotPath.Load(); path != "/chat/completions" {
		t.Fatalf("cold request path = %#v, want /chat/completions", path)
	}
	bodyValue := gotBody.Load()
	body, _ := bodyValue.(map[string]any)
	if body == nil || body["model"] != "cold-model" || body["max_tokens"] != float64(2345) {
		t.Fatalf("cold request model = %#v, want cold-model", bodyValue)
	}
}

func runProviderColdRestartChild() error {
	ctx := context.Background()
	dbPath := os.Getenv("PA_PROVIDER_COLD_DB")
	if dbPath == "" {
		return errors.New("missing provider cold-restart database")
	}
	resultPath := os.Getenv("PA_PROVIDER_COLD_RESULT")
	if resultPath == "" {
		return errors.New("missing provider cold-restart result path")
	}
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return err
	}
	var profile customProviderProfile
	if err := json.Unmarshal([]byte(settings["llm.custom.cold-route"]), &profile); err != nil {
		return err
	}
	if profile.ID != "cold-route" || profile.Model != "cold-model" || profile.BaseURL == "" || profile.DefaultMaxTokens != 2345 {
		return fmt.Errorf("cold provider profile = %+v", profile)
	}
	vault, err := credential.New(ctx, st)
	if err != nil {
		return err
	}
	defer func() { _ = vault.Close() }()
	key, err := vault.Resolve(ctx, providerEnv("cold-route"))
	if err != nil {
		return err
	}
	a := &app{
		cfg: config.Config{
			BaseURL: "https://default.invalid/v1",
			Model:   "default-model",
			LLM:     config.LLMConfig{Provider: "cold-route"},
		},
		store:           st,
		credentials:     vault,
		llmKeys:         map[string]string{"cold-route": key},
		customProviders: []customProviderProfile{profile},
	}
	if err := a.registerLLM(); err != nil {
		return err
	}
	if a.providerGeneration == nil {
		return errors.New("cold process did not publish a provider generation")
	}
	snapshot := a.providerRuntimeSnapshotPinned("cold-route")
	if snapshot.selectedID != "cold-route" || snapshot.selected == nil {
		return fmt.Errorf("cold runtime snapshot = %+v", snapshot)
	}
	reader, err := snapshot.selected.Stream(ctx, llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("cold")}}},
	})
	if err != nil {
		return err
	}
	var text strings.Builder
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if event.Kind == llm.StreamTextDelta {
			text.WriteString(event.Text)
		}
	}
	snapshot.release()
	if text.String() != "cold" {
		return fmt.Errorf("cold stream text = %q, want cold", text.String())
	}
	return os.WriteFile(resultPath, []byte(fmt.Sprint(os.Getpid())), 0o600)
}

// TestProviderCredentialRevocationMatrixCoversEveryProtocolFamily binds every
// wire adapter family to one credential vault. An in-flight stream retains its
// lease across revocation, the next stream is rejected before the revoked
// secret can be reused, and draining the old stream releases the last reference.
func TestProviderCredentialRevocationMatrixCoversEveryProtocolFamily(t *testing.T) {
	secret := "revocation-matrix-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Deliberately malformed payloads: the stream remains open while the
		// revoked lease is probed; the terminal drain closes/releases it.
		_, _ = w.Write([]byte("data: not-json\n\n"))
	}))
	defer srv.Close()

	newVault := func(t *testing.T) *credential.Vault {
		t.Helper()
		vault, err := credential.New(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := vault.Set(context.Background(), "MATRIX_API_KEY", secret); err != nil {
			t.Fatal(err)
		}
		return vault
	}
	newProvider := func(t *testing.T, family string, vault *credential.Vault) llm.Provider {
		t.Helper()
		leaseProvider := func(context.Context) (llm.CredentialLease, error) {
			return vault.Acquire(context.Background(), "MATRIX_API_KEY")
		}
		switch family {
		case "deepseek-completions":
			return deepseek.New(deepseek.Config{
				ProviderName: family, BaseURL: srv.URL,
				CredentialLeaseProvider: leaseProvider, DisableRetry: true,
			})
		case "openai-completions":
			return openai.New(openai.Config{
				BaseURL: srv.URL, CredentialLeaseProvider: leaseProvider, DisableRetry: true,
			})
		case "anthropic-messages":
			return anthropic.New(anthropic.Config{
				BaseURL: srv.URL, CredentialLeaseProvider: leaseProvider,
			})
		case "google-generative-ai":
			return google.New(google.Config{
				BaseURL: srv.URL, CredentialLeaseProvider: leaseProvider,
			})
		case "openai-responses":
			return openairesponses.New(openairesponses.Config{
				BaseURL: srv.URL, CredentialLeaseProvider: leaseProvider,
			})
		default:
			t.Fatalf("unknown protocol family %q", family)
			return nil
		}
	}

	for _, family := range []string{
		"deepseek-completions", "openai-completions", "anthropic-messages",
		"google-generative-ai", "openai-responses",
	} {
		t.Run(family, func(t *testing.T) {
			vault := newVault(t)
			provider := newProvider(t, family, vault)
			request := llm.ChatRequest{Messages: []llm.Message{{
				Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("credential matrix")},
			}}}
			live, err := provider.Stream(context.Background(), request)
			if err != nil {
				t.Fatalf("%s first stream: %v", family, err)
			}
			if err := vault.Unset(context.Background(), "MATRIX_API_KEY"); err != nil {
				t.Fatal(err)
			}
			if vault.Has("MATRIX_API_KEY") {
				t.Fatal("revoked credential remains available for new acquisition")
			}

			_, err = provider.Stream(context.Background(), request)
			if err == nil {
				t.Fatalf("%s started a stream after credential revocation", family)
			}
			facts, ok := llm.FailureFacts(err)
			if !ok || facts.Code != "CREDENTIAL_UNAVAILABLE" {
				t.Fatalf("%s revocation failure = %#v, ok=%v; want CREDENTIAL_UNAVAILABLE", family, facts, ok)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s revocation diagnostic leaked secret: %q", family, err)
			}

			// Drain the already-owned stream to its terminal boundary. Every
			// adapter releases its credential lease when the reader closes.
			for {
				_, err := live.Next()
				if err != nil {
					break
				}
			}
			if vault.Has("MATRIX_API_KEY") {
				t.Fatal("vault still reports revoked credential after in-flight drain")
			}
			if closeable, ok := provider.(llm.Closeable); ok {
				if err := closeable.Close(); err != nil {
					t.Fatalf("%s provider close: %v", family, err)
				}
			}
		})
	}
}
