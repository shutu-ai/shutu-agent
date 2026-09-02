// discover.go — the M11-pi-ai model discovery ("获取可用模型") for the Model
// settings page (dsh llm-pi-ai discovery.ts 对齐, user 2026-09). Asking a
// provider which models it serves fills the multi-model list on the
// 增加自定义提供方 / 编辑卡 instead of hand-typing every row.
//
// A route the built-in directory ships is answered from that directory (the
// suggested candidates), with no network call. Only a hand-declared endpoint is
// interrogated over the wire — and only OpenAI-compatible protocols can be:
// their GET /models listing is the one shape a gateway, a self-hosted server
// and the official endpoints all agree on (dsh LISTABLE_PROTOCOLS). Every other
// protocol reports that it cannot be interrogated, so the page falls back to
// hand-entry rather than guessing a response shape.
//
// Nothing here is stored: the request carries a draft the user is still
// editing, and the reply is candidate metadata offered for adoption.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// discoverMaxResponseBytes bounds a listing reply (dsh MAX_RESPONSE_BYTES).
// The endpoint is whatever URL the user typed, so the ceiling holds on the
// bytes actually read; a truncated listing is not parseable, so overflow
// rejects instead of truncating.
const discoverMaxResponseBytes = 4 * 1024 * 1024

// discoverProbeTimeout bounds one remote catalog probe even when the caller
// passes a background context. Caller deadlines and cancellation still apply.
var discoverProbeTimeout = 10 * time.Second

// discoverRequest is the wire probe payload (the form as it currently shows:
// base URL + protocol + a key typed but not yet saved).
type discoverRequest struct {
	Provider string `json:"provider"` // optional: a directory route answers from its catalog
	BaseURL  string `json:"base_url"` // endpoint as the form shows it
	Protocol string `json:"protocol"` // wire protocol; empty = openai-completions
	APIKey   string `json:"api_key"`  // key typed into the form, not yet stored
}

// webDiscoverModels answers "which models does this endpoint serve?" for the
// Model settings page. A directory route returns its suggested candidates; a
// hand-declared endpoint (OpenAI-compatible protocol only) is interrogated via
// GET {base}/models with the probe key. The reply is candidate metadata only —
// never stored, never configuration.
func (a *app) webDiscoverModels(ctx context.Context, req discoverRequest) ([]customModel, error) {
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.Protocol = strings.TrimSpace(req.Protocol)
	req.Provider = strings.TrimSpace(req.Provider)
	// A route the directory ships has its answer already: the suggested
	// candidates (dsh: the catalog is authoritative for its own providers).
	if req.Provider != "" {
		if bp, ok := builtinProviderByID(req.Provider); ok {
			owned := builtinModelCatalog[bp.id]
			out := make([]customModel, len(owned))
			copy(out, owned)
			return out, nil
		}
	}
	if req.BaseURL == "" {
		return nil, errors.New("base_url is required to probe a hand-declared endpoint")
	}
	// Only OpenAI-compatible protocols have a readable listing (dsh
	// LISTABLE_PROTOCOLS: openai-completions, openai-responses). Everything else
	// reports that it cannot be interrogated — the user enters models by hand.
	protocol := req.Protocol
	if protocol == "" {
		protocol = string(protocolCompletions)
	}
	switch providerProtocol(protocol) {
	case protocolCompletions, protocolResponses:
		// listable
	default:
		return nil, fmt.Errorf("协议 %q 无模型列表可读；请手动输入该提供方的模型", protocol)
	}
	return probeListing(ctx, req.BaseURL, req.APIKey)
}

// probeListing interrogates one OpenAI-compatible endpoint for its advertised
// models: GET {base}/models with Bearer auth (when a key is supplied), parsing
// data[].id/name/display_name/context_window/context_length/max_output_tokens/
// max_tokens. Entries without a usable id are skipped rather than failing the
// whole interrogation.
func probeListing(ctx context.Context, baseURL, apiKey string) ([]customModel, error) {
	u := strings.TrimRight(baseURL, "/") + "/models"
	if discoverProbeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, discoverProbeTimeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("model listing: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: discoverProbeTimeout}
	if http.DefaultClient.Transport != nil {
		client.Transport = http.DefaultClient.Transport
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("model discovery aborted: %w", ctx.Err())
		}
		return nil, fmt.Errorf("无法连接 %s", u)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		hint := ""
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			hint = "; 请检查 API Key"
		}
		return nil, fmt.Errorf("%s 应答 %d%s", u, resp.StatusCode, hint)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoverMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("model listing: read body: %w", err)
	}
	if len(body) > discoverMaxResponseBytes {
		return nil, fmt.Errorf("%s 应答超过 %d 字节", u, discoverMaxResponseBytes)
	}
	var listing struct {
		Data []struct {
			ID             any `json:"id"`
			Name           any `json:"name"`
			DisplayName    any `json:"display_name"`
			ContextWindow  any `json:"context_window"`
			ContextLength  any `json:"context_length"`
			MaxTokens      any `json:"max_tokens"`
			MaxOutputToken any `json:"max_output_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("%s 未应答 JSON", u)
	}
	out := make([]customModel, 0, len(listing.Data))
	for _, raw := range listing.Data {
		id, ok := raw.ID.(string)
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		model := customModel{ID: id}
		if name := firstLabel(raw.Name, raw.DisplayName); name != "" {
			model.Name = name
		}
		model.ContextWindow = firstCapacity(raw.ContextWindow, raw.ContextLength)
		model.MaxTokens = firstCapacity(raw.MaxOutputToken, raw.MaxTokens)
		out = append(out, model)
	}
	return out, nil
}

// firstCapacity returns the first positive integer among the candidates (dsh
// capacity(): only positive ints are usable).
func firstCapacity(candidates ...any) int {
	for _, c := range candidates {
		n, ok := c.(float64)
		if ok && n > 0 && n == float64(int64(n)) {
			return int(n)
		}
	}
	return 0
}

// firstLabel returns the first non-empty string among the candidates.
func firstLabel(candidates ...any) string {
	for _, c := range candidates {
		s, ok := c.(string)
		if ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// isListableProtocol reports whether a probe can read protocol's model listing
// (dsh LISTABLE_PROTOCOLS).
func isListableProtocol(protocol string) bool {
	switch providerProtocol(protocol) {
	case protocolCompletions, protocolResponses:
		return true
	}
	return false
}
