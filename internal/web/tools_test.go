package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/tools"
)

// stubSearchProvider 是内存 SearchProvider：按查询返回固定结果、记录收到的
// 查询、可注入统一错误。
type stubSearchProvider struct {
	results map[string]WebSearchResult
	queries []string
	err     error // 非 nil 时所有查询都返回该错误
}

func (f *stubSearchProvider) ID() string      { return "fake-search" }
func (f *stubSearchProvider) Available() bool { return true }
func (f *stubSearchProvider) Search(ctx context.Context, req WebSearchRequest) (WebSearchResult, error) {
	f.queries = append(f.queries, req.Query)
	if f.err != nil {
		return WebSearchResult{}, f.err
	}
	if res, ok := f.results[req.Query]; ok {
		return res, nil
	}
	return WebSearchResult{}, nil
}

// stubFetchProvider 是内存 FetchProvider：返回固定结果或注入错误。
type stubFetchProvider struct {
	result WebFetchResult
	err    error
}

func (f *stubFetchProvider) ID() string      { return "fake-fetch" }
func (f *stubFetchProvider) Available() bool { return true }
func (f *stubFetchProvider) Fetch(ctx context.Context, req WebFetchRequest) (WebFetchResult, error) {
	if f.err != nil {
		return WebFetchResult{}, f.err
	}
	return f.result, nil
}

// TestWebToolsDefaults 覆盖 NewWebTools 的 0→默认规则（provider id 复用稳定 id）。
func TestWebToolsDefaults(t *testing.T) {
	wt := NewWebTools(NewEngine(), Options{}, nil)
	if wt.opts.SearchID != deepseekProviderID {
		t.Errorf("SearchID = %q, want %q", wt.opts.SearchID, deepseekProviderID)
	}
	if wt.opts.FetchID != httpFetchProviderID {
		t.Errorf("FetchID = %q, want %q", wt.opts.FetchID, httpFetchProviderID)
	}
	if wt.opts.SearchMaxResults != defaultSearchMaxResults || wt.opts.SearchMaxQueries != defaultSearchMaxQueries ||
		wt.opts.SearchTimeoutMs != defaultSearchTimeoutMs || wt.opts.FetchTimeoutMs != defaultFetchTimeoutMs ||
		wt.opts.FetchMaxOutputChars != defaultFetchMaxOutputChars {
		t.Errorf("defaults not applied: %+v", wt.opts)
	}
}

// TestParseSearchArgs 覆盖净化：空数组/全空白 → 错误；重复去重保序；空白串
// 丢弃；超 maxQueries 拒绝（raw 计数，照 D7 maxItems 语义，D7 之外的工具层
// 防御）。
func TestParseSearchArgs(t *testing.T) {
	if _, err := parseSearchArgs(nil, 4); err == nil {
		t.Error("empty queries must error")
	}
	if _, err := parseSearchArgs([]string{"", "  "}, 4); err == nil {
		t.Error("all-blank queries must error")
	}
	if _, err := parseSearchArgs([]string{" ", "  ", ""}, 4); err == nil {
		t.Error("blank-only queries must error after dropping blanks")
	}
	// 重复去重保序（首个出现优先）。
	if got := strings.Join(parseOrErr(t, []string{"a", "a", "b", "b", "a"}, 5), ","); got != "a,b" {
		t.Errorf("dedupe = %q, want a,b", got)
	}
	// 空白串丢弃（保留原始查询文本）。
	if got := strings.Join(parseOrErr(t, []string{" a ", "", "b", "  "}, 4), ","); got != " a ,b" {
		t.Errorf("blank drop = %q, want \" a ,b\"", got)
	}
	// 超 maxQueries 拒绝（对 raw 数组长度，D7 之前工具层先挡一道）。
	if _, err := parseSearchArgs([]string{"a", "b", "c", "d", "e"}, 4); err == nil {
		t.Error("over maxQueries must error at the tool layer too")
	}
	if _, err := parseSearchArgs([]string{"a", "b", "c", "d"}, 4); err != nil {
		t.Errorf("at maxQueries must pass: %v", err)
	}
}

func parseOrErr(t *testing.T, in []string, max int) []string {
	t.Helper()
	out, err := parseSearchArgs(in, max)
	if err != nil {
		t.Fatalf("parseSearchArgs(%v): %v", in, err)
	}
	return out
}

// TestWebSearchSingleQueryPassthrough 覆盖单查询：透传 provider 结果、只发一次
// 搜索、输出含来源行与引用提示。
func TestWebSearchSingleQueryPassthrough(t *testing.T) {
	engine := NewEngine()
	fake := &stubSearchProvider{results: map[string]WebSearchResult{
		"golang": {Sources: []WebSearchSource{
			{URL: "https://go.dev/", Title: "The Go Programming Language", Snippet: "official docs"},
		}},
	}}
	if err := engine.RegisterSearchProvider(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{SearchID: "fake-search"}, nil)
	out, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["golang"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fake.queries) != 1 || fake.queries[0] != "golang" {
		t.Fatalf("queries sent = %v, want [golang]", fake.queries)
	}
	want := "Sources:\n- [The Go Programming Language](https://go.dev/) \u2014 official docs\n\nCite the relevant URLs above as markdown links in your answer."
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestWebSearchEngineTruncates 覆盖 Engine 按 MaxResults 截断单查询结果（来源
// 多于上限 → 截断提示）。
func TestWebSearchEngineTruncates(t *testing.T) {
	engine := NewEngine()
	fake := &stubSearchProvider{results: map[string]WebSearchResult{
		"q": {Sources: []WebSearchSource{
			{URL: "https://ex.com/a"}, {URL: "https://ex.com/b"}, {URL: "https://ex.com/c"},
			{URL: "https://ex.com/d"}, {URL: "https://ex.com/e"},
		}},
	}}
	if err := engine.RegisterSearchProvider(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{SearchID: "fake-search", SearchMaxResults: 3}, nil)
	out, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["q"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "(Showing the first 3 sources. Refine the query for more.)") {
		t.Errorf("output missing truncation note: %q", out)
	}
	for _, u := range []string{"https://ex.com/a", "https://ex.com/b", "https://ex.com/c"} {
		if !strings.Contains(out, u) {
			t.Errorf("output missing %s: %q", u, out)
		}
	}
	if strings.Contains(out, "https://ex.com/d") {
		t.Errorf("output must not contain a dropped source: %q", out)
	}
}

// TestWebSearchMultiQueryMerge 覆盖多查询顺序扇出 + round-robin 合并 + URL 去重
// + content 标题拼接。q2 首个来源 URL 与 q1 重复，合并后只出现一次。
func TestWebSearchMultiQueryMerge(t *testing.T) {
	engine := NewEngine()
	fake := &stubSearchProvider{results: map[string]WebSearchResult{
		"golang": {
			Content: "answer1",
			Sources: []WebSearchSource{
				{URL: "https://ex.com/a1", Title: "A-One"},
				{URL: "https://ex.com/b1", Title: "B-One"},
				{URL: "https://ex.com/c1", Title: "C-One"},
			},
		},
		"web": {
			Content: "answer2",
			Sources: []WebSearchSource{
				{URL: "https://ex.com/a1", Title: "A-One dup"}, // 与 q1 重复
				{URL: "https://ex.com/e1", Title: "E-One"},
				{URL: "https://ex.com/f1", Title: "F-One"},
			},
		},
	}}
	if err := engine.RegisterSearchProvider(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{SearchID: "fake-search"}, nil)
	out, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["golang","web"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fake.queries) != 2 || fake.queries[0] != "golang" || fake.queries[1] != "web" {
		t.Fatalf("queries sent = %v, want [golang web] (顺序扇出)", fake.queries)
	}
	want := "### golang\n\nanswer1\n\n### web\n\nanswer2\n\n" +
		"Sources:\n- [A-One](https://ex.com/a1)\n- [B-One](https://ex.com/b1)\n- [E-One](https://ex.com/e1)\n- [C-One](https://ex.com/c1)\n- [F-One](https://ex.com/f1)\n\n" +
		"Cite the relevant URLs above as markdown links in your answer."
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestWebSearchMultiQueryTruncates 覆盖合并截断：round-robin 到达 SearchMaxResults
// 即停并置 droppedSource → Truncated → 输出带截断提示。
func TestWebSearchMultiQueryTruncates(t *testing.T) {
	engine := NewEngine()
	fake := &stubSearchProvider{results: map[string]WebSearchResult{
		"a": {Sources: []WebSearchSource{{URL: "https://ex.com/a1"}, {URL: "https://ex.com/b1"}, {URL: "https://ex.com/c1"}}},
		"b": {Sources: []WebSearchSource{{URL: "https://ex.com/d1"}, {URL: "https://ex.com/e1"}, {URL: "https://ex.com/f1"}}},
	}}
	if err := engine.RegisterSearchProvider(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{SearchID: "fake-search", SearchMaxResults: 3}, nil)
	out, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["a","b"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "(Showing the first 3 sources. Refine the query for more.)") {
		t.Errorf("output missing truncation note: %q", out)
	}
}

// TestWebSearchNoResults 覆盖无来源且无 content → "No results found." + 引用提示。
func TestWebSearchNoResults(t *testing.T) {
	engine := NewEngine()
	if err := engine.RegisterSearchProvider(&stubSearchProvider{results: map[string]WebSearchResult{"q": {}}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{SearchID: "fake-search"}, nil)
	out, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["q"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "No results found.\n\nCite the relevant URLs above as markdown links in your answer."
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestWebSearchProviderError 覆盖 provider 错误透出为可读文本。
func TestWebSearchProviderError(t *testing.T) {
	engine := NewEngine()
	fake := &stubSearchProvider{err: fmt.Errorf("%w: boom", ErrProvider)}
	if err := engine.RegisterSearchProvider(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{SearchID: "fake-search"}, nil)
	_, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["q"]}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "web_search: search provider error:") {
		t.Errorf("err = %q, want the readable provider-error text", err)
	}
}

// TestWebSearchNoProvider 覆盖未注册搜索 provider → ErrNoProvider 映射。
func TestWebSearchNoProvider(t *testing.T) {
	wt := NewWebTools(NewEngine(), Options{}, nil) // 默认 id，无 provider
	_, err := wt.Search().Execute(context.Background(), json.RawMessage(`{"queries":["q"]}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no search provider") {
		t.Errorf("err = %q, want the no-provider hint", err)
	}
}

// TestWebSearchOverMaxQueriesRejectedByD7 覆盖超 maxQueries 被 D7 拒绝：schema
// maxItems 与注册表校验路径（tools.Registry 编译 schema 后统一校验，D7）。
func TestWebSearchOverMaxQueriesRejectedByD7(t *testing.T) {
	wt := NewWebTools(NewEngine(), Options{SearchMaxQueries: 2}, nil)

	// schema 断言：maxItems 随 SearchMaxQueries 生成。
	schema := wt.Search().Schema()
	props, _ := schema["properties"].(map[string]any)
	queries, _ := props["queries"].(map[string]any)
	if max, _ := queries["maxItems"].(int); max != 2 {
		t.Errorf("schema maxItems = %v, want 2", queries["maxItems"])
	}

	// 注册表校验路径：3 个查询 > maxQueries=2 被 D7 拒绝，不进入工具实现。
	reg := tools.New()
	reg.SetPolicy(tools.Policy{Enabled: []string{"web_search", "web_fetch"}, Timeout: 0, OutputLimit: 0})
	if err := reg.Register(wt.Search()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := reg.Execute(context.Background(), "web_search", json.RawMessage(`{"queries":["a","b","c"]}`)); err == nil {
		t.Fatal("3 queries with maxQueries=2 must be rejected at the D7 gate")
	}
	// 2 个查询通过 D7（进入实现；无 provider → ErrNoProvider，证明校验已放行）。
	if res, err := reg.Execute(context.Background(), "web_search", json.RawMessage(`{"queries":["a","b"]}`)); err != nil || !res.IsError {
		t.Fatalf("web_search must return a structured no-provider error after D7 passed: result=%+v err=%v", res, err)
	}
}

// TestWebFetchTextOutput 覆盖 text body：原样返回 + URL/状态头两行。
func TestWebFetchTextOutput(t *testing.T) {
	engine := NewEngine()
	if err := engine.RegisterFetchProvider(&stubFetchProvider{result: WebFetchResult{
		URL: "https://example.com/page", StatusCode: 200,
		Body: WebFetchBody{Kind: "text", Content: "plain body"},
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{FetchID: "fake-fetch"}, nil)
	out, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/page"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "https://example.com/page\nHTTP 200\n\nplain body"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestWebFetchHTMLOutput 覆盖 html body：经 HTMLToMarkdown 转成 Markdown。
func TestWebFetchHTMLOutput(t *testing.T) {
	engine := NewEngine()
	if err := engine.RegisterFetchProvider(&stubFetchProvider{result: WebFetchResult{
		URL: "https://example.com/page", StatusCode: 200,
		Body: WebFetchBody{Kind: "html", Content: "<h1>Title</h1><p>Hello</p>"},
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{FetchID: "fake-fetch"}, nil)
	out, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/page"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, "https://example.com/page\nHTTP 200\n\n") {
		t.Errorf("output missing header: %q", out)
	}
	if !strings.Contains(out, "# Title") || !strings.Contains(out, "Hello") {
		t.Errorf("output must contain the converted markdown: %q", out)
	}
}

// TestWebFetchNon2xxIsResult 覆盖非 2xx 正常返回（状态码可见）。
func TestWebFetchNon2xxIsResult(t *testing.T) {
	engine := NewEngine()
	if err := engine.RegisterFetchProvider(&stubFetchProvider{result: WebFetchResult{
		URL: "https://example.com/missing", StatusCode: 404,
		Body: WebFetchBody{Kind: "text", Content: "missing"},
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{FetchID: "fake-fetch"}, nil)
	out, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/missing"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "HTTP 404") {
		t.Errorf("output must carry the status code: %q", out)
	}
}

// TestWebFetchTruncation 覆盖 body 超 FetchMaxOutputChars → 截断 + 提示。
func TestWebFetchTruncation(t *testing.T) {
	engine := NewEngine()
	if err := engine.RegisterFetchProvider(&stubFetchProvider{result: WebFetchResult{
		URL: "https://example.com/big", StatusCode: 200,
		Body: WebFetchBody{Kind: "text", Content: strings.Repeat("x", 100)},
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{FetchID: "fake-fetch", FetchMaxOutputChars: 10}, nil)
	out, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/big"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "[truncated: showing first 10 chars]") {
		t.Errorf("output missing truncation notice: %q", out)
	}
	if !strings.Contains(out, strings.Repeat("x", 10)) {
		t.Errorf("output must keep the first 10 chars: %q", out)
	}
	if strings.Contains(out, strings.Repeat("x", 11)) {
		t.Errorf("output must not contain beyond the cap: %q", out)
	}
}

// TestWebFetchProviderTruncationNotice 覆盖 provider 已截断（res.Truncated）也
// 追加提示（N = 实际展示字符数）。
func TestWebFetchProviderTruncationNotice(t *testing.T) {
	engine := NewEngine()
	if err := engine.RegisterFetchProvider(&stubFetchProvider{result: WebFetchResult{
		URL: "https://example.com/", StatusCode: 200, Truncated: true,
		Body: WebFetchBody{Kind: "text", Content: "hello"},
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{FetchID: "fake-fetch"}, nil)
	out, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "[truncated: showing first 5 chars]") {
		t.Errorf("output missing provider-truncation notice: %q", out)
	}
}

// TestWebFetchErrorMapping 覆盖抓取错误映射为可读文本。
func TestWebFetchErrorMapping(t *testing.T) {
	engine := NewEngine()
	fake := &stubFetchProvider{err: fmt.Errorf("%w: https://bad", ErrInvalidURL)}
	if err := engine.RegisterFetchProvider(fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt := NewWebTools(engine, Options{FetchID: "fake-fetch"}, nil)
	if _, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://bad"}`)); err == nil ||
		!strings.Contains(err.Error(), "web_fetch: invalid URL:") {
		t.Errorf("err = %v, want the readable invalid-URL text", err)
	}

	// 跨源重定向：provider 的提示原样透出。
	engine2 := NewEngine()
	if err := engine2.RegisterFetchProvider(&stubFetchProvider{err: fmt.Errorf("%w: cross-origin redirect to https://x is not followed automatically; fetch that URL directly", ErrRedirectBlocked)}); err != nil {
		t.Fatalf("register: %v", err)
	}
	wt2 := NewWebTools(engine2, Options{FetchID: "fake-fetch"}, nil)
	if _, err := wt2.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://a"}`)); err == nil ||
		!strings.Contains(err.Error(), "fetch that URL directly") {
		t.Errorf("err = %v, want the cross-origin hint", err)
	}
}

// TestWebFetchNoProvider 覆盖未注册抓取 provider → ErrNoProvider 映射。
func TestWebFetchNoProvider(t *testing.T) {
	wt := NewWebTools(NewEngine(), Options{}, nil)
	_, err := wt.Fetch().Execute(context.Background(), json.RawMessage(`{"url":"https://example.com"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no fetch provider") {
		t.Errorf("err = %q, want the no-provider hint", err)
	}
}

// TestMergeSearchResultsRoundRobin 直接单测 mergeSearchResults 纯函数：
// round-robin 顺序、URL 去重、截断置位、content 标题拼接。
func TestMergeSearchResultsRoundRobin(t *testing.T) {
	items := []searchItem{
		{query: "q1", result: WebSearchResult{
			Content: "c1",
			Sources: []WebSearchSource{{URL: "u1"}, {URL: "u2"}, {URL: "u3"}},
		}},
		{query: "q2", result: WebSearchResult{
			Content: "c2",
			Sources: []WebSearchSource{{URL: "u1"}, {URL: "u4"}}, // u1 重复
		}},
	}
	got := mergeSearchResults(items, 10)
	var urls []string
	for _, s := range got.Sources {
		urls = append(urls, s.URL)
	}
	if want := "u1,u2,u4,u3"; strings.Join(urls, ",") != want {
		t.Errorf("round-robin urls = %v, want %s", urls, want)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false (under cap, no per-item truncation)")
	}
	if want := "### q1\n\nc1\n\n### q2\n\nc2"; got.Content != want {
		t.Errorf("Content = %q, want %q", got.Content, want)
	}
}

// TestMergeSearchResultsDrops 覆盖 merge 截断：到达 max 即停并置 droppedSource。
func TestMergeSearchResultsDrops(t *testing.T) {
	items := []searchItem{
		{query: "q1", result: WebSearchResult{Sources: []WebSearchSource{{URL: "u1"}, {URL: "u2"}}}},
		{query: "q2", result: WebSearchResult{Sources: []WebSearchSource{{URL: "u3"}, {URL: "u4"}}}},
	}
	got := mergeSearchResults(items, 3)
	if !got.Truncated {
		t.Error("Truncated = false, want true (source dropped at the cap)")
	}
	if len(got.Sources) != 3 {
		t.Errorf("sources = %d, want 3", len(got.Sources))
	}
}

// TestMergeSearchResultsPerItemTruncation 覆盖任一 result.Truncated 传递到合并
// 结果（provider/Engine 已截断某一次查询）。
func TestMergeSearchResultsPerItemTruncation(t *testing.T) {
	items := []searchItem{
		{query: "q1", result: WebSearchResult{Truncated: true, Sources: []WebSearchSource{{URL: "u1"}}}},
		{query: "q2", result: WebSearchResult{Sources: []WebSearchSource{{URL: "u2"}}}},
	}
	got := mergeSearchResults(items, 10)
	if !got.Truncated {
		t.Error("Truncated = false, want true (a per-item truncation must propagate)")
	}
}

// TestWebSearchContentOnlyFormat 覆盖有 content 无来源时直接输出 content +
// 引用提示（不显示 "No results found."）。
func TestWebSearchContentOnlyFormat(t *testing.T) {
	out := formatSearchOutput(WebSearchResult{Content: "the answer"})
	want := "the answer\n\nCite the relevant URLs above as markdown links in your answer."
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestWebFetchSchemaRejectsExtras 覆盖 web_fetch schema：required=[url] 且
// additionalProperties=false（D7 契约）。
func TestWebFetchSchemaRejectsExtras(t *testing.T) {
	wt := NewWebTools(NewEngine(), Options{}, nil)
	schema := wt.Fetch().Schema()
	if v, ok := schema["additionalProperties"].(bool); !ok || v {
		t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
	}
	if req, _ := schema["required"].([]string); len(req) != 1 || req[0] != "url" {
		t.Errorf("required = %v, want [url]", schema["required"])
	}
}
