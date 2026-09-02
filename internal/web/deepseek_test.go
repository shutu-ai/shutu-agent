package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// successJSON 是一个含 text citations + web_search_tool_result 的典型成功响应，
// 含重复 url（用于去重断言）与空 title 项。
const successJSON = `{
  "content": [
    {"type": "text", "text": "summary", "citations": [
      {"url": "https://example.com/a", "cited_text": "snippet-a"},
      {"url": "https://example.com/b", "cited_text": "snippet-b"}
    ]},
    {"type": "web_search_tool_result", "content": [
      {"type": "web_search_result", "url": "https://example.com/a", "title": "Title A", "page_age": "2026-01-01"},
      {"type": "web_search_result", "url": "https://example.com/b", "title": "Title B", "page_age": ""},
      {"type": "web_search_result", "url": "https://example.com/a", "title": "Dup", "page_age": "x"},
      {"type": "web_search_result", "url": "https://example.com/c", "title": "", "page_age": ""}
    ]}
  ]
}`

// TestDeepSeekDefaults 覆盖空 Config 落到默认值。
func TestDeepSeekDefaults(t *testing.T) {
	p := NewDeepSeekProvider(Config{})
	if p.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", p.baseURL, defaultBaseURL)
	}
	if p.model != defaultModel {
		t.Fatalf("model = %q, want %q", p.model, defaultModel)
	}
	if p.apiVersion != defaultAPIVersion {
		t.Fatalf("apiVersion = %q, want %q", p.apiVersion, defaultAPIVersion)
	}
	if p.maxTokens != defaultMaxTokens {
		t.Fatalf("maxTokens = %d, want %d", p.maxTokens, defaultMaxTokens)
	}
	if p.maxUses != defaultMaxUses {
		t.Fatalf("maxUses = %d, want %d", p.maxUses, defaultMaxUses)
	}
	if p.ID() != deepseekProviderID {
		t.Fatalf("ID = %q, want %q", p.ID(), deepseekProviderID)
	}
	if p.Available() {
		t.Fatal("Available() = true with empty API key, want false")
	}
}

// TestDeepSeekAvailable 覆盖 Available 的本地检查。
func TestDeepSeekAvailable(t *testing.T) {
	if !NewDeepSeekProvider(Config{APIKey: "k"}).Available() {
		t.Fatal("Available() = false with key + defaults, want true")
	}
	if NewDeepSeekProvider(Config{APIKey: "k", MaxTokens: -1}).Available() {
		t.Fatal("Available() = true with negative MaxTokens, want false")
	}
	if NewDeepSeekProvider(Config{APIKey: "k", MaxUses: -1}).Available() {
		t.Fatal("Available() = true with negative MaxUses, want false")
	}
	if NewDeepSeekProvider(Config{APIKey: "k", BaseURL: "://bad"}).Available() {
		t.Fatal("Available() = true with unparseable baseURL, want false")
	}
}

// TestDeepSeekSearchRequestShape 覆盖请求组装：method/path/headers/body。
func TestDeepSeekSearchRequestShape(t *testing.T) {
	var gotMethod, gotPath string
	var gotHeaders http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &gotBody)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(successJSON))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "test-key-123", BaseURL: srv.URL + "/anthropic/v1"})
	res, err := p.Search(context.Background(), WebSearchRequest{Query: "golang generics"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want /anthropic/v1/messages", gotPath)
	}
	if gotHeaders.Get("x-api-key") != "test-key-123" {
		t.Fatalf("x-api-key = %q", gotHeaders.Get("x-api-key"))
	}
	if gotHeaders.Get("authorization") != "Bearer test-key-123" {
		t.Fatalf("authorization = %q", gotHeaders.Get("authorization"))
	}
	if gotHeaders.Get("anthropic-version") != defaultAPIVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotHeaders.Get("anthropic-version"), defaultAPIVersion)
	}
	if gotHeaders.Get("content-type") != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotHeaders.Get("content-type"))
	}
	if gotHeaders.Get("accept") != "application/json" {
		t.Fatalf("accept = %q, want application/json", gotHeaders.Get("accept"))
	}

	wantBody := map[string]any{
		"model":      defaultModel,
		"max_tokens": float64(defaultMaxTokens),
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Perform a web search for the query: golang generics",
					},
				},
			},
		},
		"tools": []any{
			map[string]any{
				"type":     "web_search_20250305",
				"name":     "web_search",
				"max_uses": float64(defaultMaxUses),
			},
		},
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Fatalf("body mismatch:\n got: %#v\nwant: %#v", gotBody, wantBody)
	}

	if len(res.Sources) != 3 {
		t.Fatalf("len(Sources) = %d, want 3 (dedup)", len(res.Sources))
	}
}

// TestDeepSeekSearchMapsSuccess 覆盖成功响应的规范化映射：url/title/snippet/
// publishedAt + 去重。
func TestDeepSeekSearchMapsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(successJSON))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	res, err := p.Search(context.Background(), WebSearchRequest{Query: "q"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []WebSearchSource{
		{URL: "https://example.com/a", Title: "Title A", Snippet: "snippet-a", PublishedAt: "2026-01-01"},
		{URL: "https://example.com/b", Title: "Title B", Snippet: "snippet-b"},
		{URL: "https://example.com/c"},
	}
	if !reflect.DeepEqual(res.Sources, want) {
		t.Fatalf("Sources mismatch:\n got: %#v\nwant: %#v", res.Sources, want)
	}
	if res.Truncated {
		t.Fatal("Truncated = true, want false")
	}
}

// TestMapAnthropicResponse 覆盖纯函数：snippet 首次出现优先、按 url 去重、
// 非 web_search_result 项忽略。
func TestMapAnthropicResponse(t *testing.T) {
	data := []byte(`{"content":[
		{"type":"text","text":"a","citations":[
			{"url":"https://e/a","cited_text":"first"},
			{"url":"https://e/b","cited_text":"bee"}
		]},
		{"type":"text","text":"b","citations":[
			{"url":"https://e/a","cited_text":"second-ignored"}
		]},
		{"type":"web_search_tool_result","content":[
			{"type":"web_search_result","url":"https://e/a","title":"A","page_age":"2026-05-01"},
			{"type":"web_search_result","url":"https://e/b","title":"B"},
			{"type":"web_search_result","url":"https://e/b","title":"B-dup"},
			{"type":"other","url":"https://e/ignored"}
		]}
	]}`)
	res, err := mapAnthropicResponse(data)
	if err != nil {
		t.Fatalf("mapAnthropicResponse: %v", err)
	}
	want := []WebSearchSource{
		{URL: "https://e/a", Title: "A", Snippet: "first", PublishedAt: "2026-05-01"},
		{URL: "https://e/b", Title: "B", Snippet: "bee"},
	}
	if !reflect.DeepEqual(res.Sources, want) {
		t.Fatalf("Sources:\n got: %#v\nwant: %#v", res.Sources, want)
	}
}

// TestDeepSeekSearchNoResultBlock 覆盖 fail-closed：无 web_search_tool_result
// block → ErrProvider。
func TestDeepSeekSearchNoResultBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"just prose"}]}`))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	if _, err := p.Search(context.Background(), WebSearchRequest{Query: "q"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider (fail-closed)", err)
	}
}

// TestDeepSeekSearchMalformedResponse 覆盖响应不可解析 → ErrProvider。
func TestDeepSeekSearchMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	if _, err := p.Search(context.Background(), WebSearchRequest{Query: "q"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
}

// TestDeepSeekSearchServerError 覆盖非 2xx + Anthropic error 信封 → ErrProvider
// 且消息含服务端 detail。
func TestDeepSeekSearchServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"upstream exploded"}}`))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	_, err := p.Search(context.Background(), WebSearchRequest{Query: "q"})
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("err = %q, want to contain server detail", err)
	}
}

// TestDeepSeekSearchPlainMessageError 覆盖非 2xx + 平铺 {"message":..} 形状。
func TestDeepSeekSearchPlainMessageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	_, err := p.Search(context.Background(), WebSearchRequest{Query: "q"})
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("err = %q, want to contain message", err)
	}
}

// TestDeepSeekSearchMissingCredential 覆盖凭证为空 → ErrCredential。
func TestDeepSeekSearchMissingCredential(t *testing.T) {
	p := NewDeepSeekProvider(Config{}) // 空 APIKey
	_, err := p.Search(context.Background(), WebSearchRequest{Query: "q"})
	if !errors.Is(err, ErrCredential) {
		t.Fatalf("err = %v, want ErrCredential", err)
	}
	if !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("err = %q, want to mention DEEPSEEK_API_KEY", err)
	}
}

// TestDeepSeekSearchCancelled 覆盖 ctx 取消 → ErrAborted。
func TestDeepSeekSearchCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Search(ctx, WebSearchRequest{Query: "q"}); !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
}

// TestDeepSeekSearchRedirectBlocked 覆盖 redirect 策略：3xx 不跟随 → ErrProvider。
func TestDeepSeekSearchRedirectBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: srv.URL})
	if _, err := p.Search(context.Background(), WebSearchRequest{Query: "q"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider (redirect blocked)", err)
	}
}

// TestDeepSeekSearchOnRequest 覆盖 OnRequest 派发前调用且载荷含 query/endpoint/
// body；HTTP 请求确实发出。
func TestDeepSeekSearchOnRequest(t *testing.T) {
	var dispatched atomic.Bool
	var events []SearchRequestEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched.Store(true)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(successJSON))
	}))
	defer srv.Close()

	p := NewDeepSeekProvider(Config{
		APIKey:  "k",
		BaseURL: srv.URL + "/anthropic/v1",
		OnRequest: func(ev SearchRequestEvent) error {
			events = append(events, ev)
			return nil
		},
	})
	if _, err := p.Search(context.Background(), WebSearchRequest{Query: "hello world"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Query != "hello world" {
		t.Fatalf("event.Query = %q, want hello world", ev.Query)
	}
	if ev.Endpoint != srv.URL+"/anthropic/v1/messages" {
		t.Fatalf("event.Endpoint = %q", ev.Endpoint)
	}
	if ev.APIVersion != defaultAPIVersion || ev.Model != defaultModel {
		t.Fatalf("event APIVersion=%q Model=%q", ev.APIVersion, ev.Model)
	}
	if _, ok := ev.Body["tools"]; !ok {
		t.Fatalf("event.Body missing tools: %#v", ev.Body)
	}
	if _, ok := ev.Body["api_key"]; ok {
		t.Fatal("event.Body must be secret-free")
	}
	if !dispatched.Load() {
		t.Fatal("HTTP request was not dispatched")
	}
}

// TestDeepSeekSearchOnRequestBlocksDispatch 覆盖 OnRequest 返回错误 → Search
// 中止、不发 HTTP 请求。
func TestDeepSeekSearchOnRequestBlocksDispatch(t *testing.T) {
	var dispatched atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched.Store(true)
	}))
	defer srv.Close()

	logErr := errors.New("log sink rejected")
	p := NewDeepSeekProvider(Config{
		APIKey:  "k",
		BaseURL: srv.URL,
		OnRequest: func(ev SearchRequestEvent) error {
			return logErr
		},
	})
	_, err := p.Search(context.Background(), WebSearchRequest{Query: "q"})
	if !errors.Is(err, logErr) {
		t.Fatalf("err = %v, want the OnRequest error", err)
	}
	if dispatched.Load() {
		t.Fatal("HTTP request dispatched despite OnRequest error")
	}
}

func TestDeepSeekSearchOnRequestContextPreservesCallerContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(successJSON))
	}))
	defer srv.Close()

	type contextKey string
	const key contextKey = "session"
	var got any
	p := NewDeepSeekProvider(Config{
		APIKey:  "k",
		BaseURL: srv.URL,
		OnRequestContext: func(ctx context.Context, _ SearchRequestEvent) error {
			got = ctx.Value(key)
			return nil
		},
	})
	ctx := context.WithValue(context.Background(), key, "session-42")
	if _, err := p.Search(ctx, WebSearchRequest{Query: "q"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != "session-42" {
		t.Fatalf("callback context value = %#v, want session-42", got)
	}
}
