package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
)

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
