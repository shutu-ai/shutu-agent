// web.go — the M7-2 composition-root orchestration (dispatch-m7-2 §6). This is
// where the web capability seam is wired into the REPL: registerWeb creates the
// web Engine + the DeepSeek search provider (env key only, absent ⇒ provider
// unavailable so Search returns ErrCredential) + the HTTP fetch provider + the
// two web_* tools when web.enabled (D10), and wires the D3 web/search-request
// log via the provider's session-aware request callback. The wiring sits entirely in the tool
// registration layer — the loop's turn/step structure is untouched (D4) — and
// every web_* tool executes on the serial tool path (D5, no background
// goroutine). It must run before registerInteracts so the sensitive-tool gate
// can wrap the web tools too.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/runtimectx"
	"github.com/jabing/shutu-agent/internal/tools"
	"github.com/jabing/shutu-agent/internal/web"
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
	// 搜索 provider：DEEPSEEK_API_KEY env-only（纪律 6），缺失时 provider 不可用、
	// Search 返回 ErrCredential。Available 是廉价本地检查，绝不做网络调用。
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		// D3 event sink: web/search-request is appended to the active session
		// log by the provider's session-aware request callback before dispatch (M7-1). The callback
		// only ever runs inside a web_search Execute — the serial main-loop
		// path (D5). a.log is read at call time, so a session switch (/new,
		// /resume) is honored through the runtime context, with the global log as
		// a compatibility fallback for legacy direct calls.
		onReq := func(ctx context.Context, ev web.SearchRequestEvent) error {
			log := a.runtimeLog(ctx)
			if log == nil {
				if _, runtimeOwned := runtimectx.Get(ctx); runtimeOwned {
					return fmt.Errorf("pa: no Agent-owned session log for web search request")
				}
				log = a.log
			}
			if log == nil {
				return fmt.Errorf("pa: no session log for web search request")
			}
			if _, err := log.Append("web/search-request", ev); err != nil {
				return err
			}
			return nil
		}
		sp := web.NewDeepSeekProvider(web.Config{
			APIKey:           key,
			BaseURL:          a.cfg.Web.DeepSeek.BaseURL,
			Model:            a.cfg.Web.DeepSeek.Model,
			APIVersion:       a.cfg.Web.DeepSeek.APIVersion,
			MaxTokens:        a.cfg.Web.DeepSeek.MaxTokens,
			MaxUses:          a.cfg.Web.DeepSeek.MaxUses,
			OnRequestContext: onReq,
		})
		if err := engine.RegisterSearchProvider(sp); err != nil {
			return fmt.Errorf("pa: register DeepSeek search provider: %w", err)
		}
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
		return fmt.Errorf("pa: register HTTP fetch provider: %w", err)
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
			return fmt.Errorf("pa: register %s: %w", tl.Name(), err)
		}
	}
	a.web = engine
	return nil
}
