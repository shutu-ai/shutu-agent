// web.go — the M7-2 composition-root orchestration (dispatch-m7-2 §6). This is
// where the web capability seam is wired into the REPL: registerWeb creates the
// web Engine + the DeepSeek search provider (env key only, absent ⇒ provider
// unavailable so Search returns ErrCredential) + the HTTP fetch provider + the
// two web_* tools when web.enabled (D10), and wires the D3 web search request
// log via the provider's session-aware request callback. The wiring sits entirely in the tool
// registration layer — the loop's turn/step structure is untouched (D4) — and
// every web_* tool executes on the serial tool path (D5, no background
// goroutine). It must run before registerInteracts so the sensitive-tool gate
// can wrap the web tools too.
package main

import (
	"context"
	"fmt"

	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/runtimectx"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/tools"
	"github.com/shutu-ai/shutu-agent/internal/web"
)

// registerWeb 在 web.enabled 时创建 Engine + provider + 注册 web_* 工具（白名单
// 已由 config.applyDefaults 加入）；disabled 零操作（D10，照 registerFs/
// registerMcps 同款）。web.enabled 同时开搜索与抓取——不设独立的
// search_enabled/fetch_enabled 开关（dispatch-m7-2 §6 决策：按 dsh
// {search:true, fetch:true} 语义简化为 web.enabled 总开关）。
func (a *app) registerWeb() error {
	if !config.Enabled(a.cfg.Web.Enabled) {
		return nil
	}
	engine := web.NewEngine()
	// 搜索 provider 使用 native web-search settings 和 credential source 解析；
	// credential 缺失时 provider 仍注册但 fail-closed，后续设置或凭据变更可生效。
	// The native Web settings page owns provider fields and the credential
	// reference. Resolve them per search so settings and vault writes take
	// effect without rebuilding the tool registry.
	if err := engine.RegisterSearchProvider(dynamicDeepSeekSearchProvider{app: a}); err != nil {
		return fmt.Errorf("sta: register DeepSeek search provider: %w", err)
	}
	fp := web.NewHttpFetchProvider(web.FetchLimits{
		MaxURLBytes:      a.cfg.Web.FetchMaxURLBytes,
		MaxResponseBytes: a.cfg.Web.FetchMaxResponseBytes,
		MaxBodyChars:     a.cfg.Web.FetchMaxOutputChars,
		TimeoutMs:        a.cfg.Web.FetchTimeoutMs,
		MaxRedirects:     a.cfg.Web.FetchMaxRedirects,
		UserAgent:        a.cfg.Web.FetchUserAgent,
	})
	if err := engine.RegisterFetchProvider(fp); err != nil {
		return fmt.Errorf("sta: register HTTP fetch provider: %w", err)
	}
	// SearchID/FetchID 留空 → NewWebTools 落到 provider 的稳定 id
	// (deepseek-official / http)，与上面注册的 id 一致（单一事实来源）。
	wt := web.NewWebTools(engine, web.Options{
		SearchMaxResults:    a.cfg.Web.SearchMaxResults,
		SearchMaxQueries:    a.cfg.Web.SearchMaxQueries,
		SearchTimeoutMs:     a.cfg.Web.SearchTimeoutMs,
		FetchTimeoutMs:      a.cfg.Web.FetchTimeoutMs,
		FetchMaxOutputChars: a.cfg.Web.FetchMaxOutputChars,
	}, nil)
	for _, tl := range []tools.Tool{wt.Search(), wt.Fetch()} {
		if err := a.reg.Register(tl); err != nil {
			return fmt.Errorf("sta: register %s: %w", tl.Name(), err)
		}
	}
	a.web = engine
	return nil
}

// dynamicDeepSeekSearchProvider keeps the stable DeepSeek provider id while
// delegating every availability check and search to the current native
// web-search settings and credential vault.
type dynamicDeepSeekSearchProvider struct {
	app *app
}

func (p dynamicDeepSeekSearchProvider) ID() string { return "deepseek-official" }

func (p dynamicDeepSeekSearchProvider) Available() bool {
	return web.NewDeepSeekProvider(p.app.nativeWebSearchConfig(context.Background())).Available()
}

func (p dynamicDeepSeekSearchProvider) Search(ctx context.Context, request web.WebSearchRequest) (web.WebSearchResult, error) {
	return web.NewDeepSeekProvider(p.app.nativeWebSearchConfig(ctx)).Search(ctx, request)
}

// webSearchRequestEventLogger is D3's secret-free search request sink. It
// binds the request to the addressed Agent session log at dispatch time.
func (a *app) webSearchRequestEventLogger(ctx context.Context, event web.SearchRequestEvent) error {
	log := a.runtimeLog(ctx)
	if log == nil {
		if _, runtimeOwned := runtimectx.Get(ctx); runtimeOwned {
			return fmt.Errorf("sta: no Agent-owned session log for web search request")
		}
		log = a.log
	}
	if log == nil {
		return fmt.Errorf("sta: no session log for web search request")
	}
	_, err := log.Append(session.EventWebSearchLLMRequest, event)
	return err
}
