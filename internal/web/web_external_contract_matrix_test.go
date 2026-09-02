package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agenttools "github.com/jabing/shutu-agent/internal/tools"
)

// TestRemoteWebProviderFaultMatrix pins the real provider/client contract for
// auth, malformed payloads, redirects, unsupported content and non-2xx pages.
// Search faults are transport failures; fetch non-2xx is deliberately a result.
func TestRemoteWebProviderFaultMatrix(t *testing.T) {
	searchCases := []struct {
		name    string
		status  int
		content string
		wantErr error
		want    string
	}{
		{name: "malformed json", status: http.StatusOK, content: `{not-json`, wantErr: ErrProvider},
		{name: "unauthorized message", status: http.StatusUnauthorized, content: `{"message":"invalid api key"}`, wantErr: ErrProvider, want: "invalid api key"},
		{name: "missing result block", status: http.StatusOK, content: `{"content":[{"type":"text","text":"prose"}]}`, wantErr: ErrProvider, want: "web_search_tool_result"},
	}
	for _, tc := range searchCases {
		t.Run("search/"+tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/messages" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.content))
			}))
			defer server.Close()
			provider := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: server.URL})
			_, err := provider.Search(context.Background(), WebSearchRequest{Query: "query"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("search error = %v, want %v", err, tc.wantErr)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("search error = %q, want %q", err, tc.want)
			}
		})
	}

	t.Run("search/redirect-is-not-followed", func(t *testing.T) {
		var targetHit bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHit = true
			w.WriteHeader(http.StatusOK)
		}))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer server.Close()
		provider := NewDeepSeekProvider(Config{APIKey: "k", BaseURL: server.URL})
		if _, err := provider.Search(context.Background(), WebSearchRequest{Query: "query"}); !errors.Is(err, ErrProvider) {
			t.Fatalf("redirect search error = %v, want ErrProvider", err)
		}
		if targetHit {
			t.Fatal("search provider followed a redirect")
		}
	})

	fetchCases := []struct {
		name       string
		path       string
		status     int
		location   string
		content    string
		wantErr    error
		wantErrOut bool
		wantStatus int
		wantBody   string
	}{
		{name: "unsupported content", path: "/binary", status: http.StatusOK, content: "binary", wantErr: ErrProvider, wantErrOut: true},
		{name: "non-2xx is result", path: "/missing", status: http.StatusNotFound, content: "<p>missing</p>", wantStatus: http.StatusNotFound, wantBody: "missing"},
		{name: "same-origin redirect", path: "/final", status: http.StatusFound, location: "/landing", wantStatus: http.StatusOK, wantBody: "landing"},
	}
	for _, tc := range fetchCases {
		t.Run("fetch/"+tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path && tc.location != "" {
					http.Redirect(w, r, tc.location, tc.status)
					return
				}
				if r.URL.Path == "/landing" {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("landing"))
					return
				}
				if tc.path == "/binary" {
					w.Header().Set("Content-Type", "image/png")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47})
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.content))
			}))
			defer server.Close()
			provider := NewHttpFetchProvider(FetchLimits{})
			result, err := provider.Fetch(context.Background(), WebFetchRequest{URL: server.URL + tc.path})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("fetch error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if result.StatusCode != tc.wantStatus || !strings.Contains(result.Body.Content, tc.wantBody) {
				t.Fatalf("fetch result = %#v, want status %d and body %q", result, tc.wantStatus, tc.wantBody)
			}
		})
	}

	t.Run("fetch/cross-origin-redirect-stays-blocked", func(t *testing.T) {
		var targetHit bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetHit = true
			w.WriteHeader(http.StatusOK)
		}))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer source.Close()
		provider := NewHttpFetchProvider(FetchLimits{})
		_, err := provider.Fetch(context.Background(), WebFetchRequest{URL: source.URL})
		if !errors.Is(err, ErrRedirectBlocked) {
			t.Fatalf("cross-origin error = %v, want ErrRedirectBlocked", err)
		}
		if targetHit {
			t.Fatal("fetch provider followed a cross-origin redirect")
		}
	})
}

// TestWebToolRegistryFaultAndSchemaMatrix proves malformed calls never reach a
// provider and that the public tool/catalog schemas remain closed DSH unions.
func TestWebToolRegistryFaultAndSchemaMatrix(t *testing.T) {
	wt := NewWebTools(NewEngine(), Options{SearchMaxQueries: 2}, nil)
	registry := agenttools.New()
	registry.SetPolicy(agenttools.Policy{Enabled: []string{ToolSearchName, ToolFetchName}})
	if err := registry.Register(wt.Search()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(wt.Fetch()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, tool, args string
		wantRegistryErr  bool
	}{
		{name: "search missing queries", tool: ToolSearchName, args: `{}`, wantRegistryErr: true},
		{name: "search over cap", tool: ToolSearchName, args: `{"queries":["a","b","c"]}`},
		{name: "search extras", tool: ToolSearchName, args: `{"queries":["a"],"extra":true}`, wantRegistryErr: true},
		{name: "fetch missing url", tool: ToolFetchName, args: `{}`, wantRegistryErr: true},
		{name: "fetch extras", tool: ToolFetchName, args: `{"url":"https://example.test","extra":true}`, wantRegistryErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := registry.Execute(context.Background(), tc.tool, []byte(tc.args))
			if tc.wantRegistryErr {
				if err == nil {
					t.Fatal("D7 schema fault was admitted")
				}
				return
			}
			if err != nil {
				t.Fatalf("schema fault escaped as registry error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("schema fault result = %+v, want tool failure", result)
			}
		})
	}

	catalog := map[string]agenttools.CatalogEntry{}
	for _, entry := range registry.Catalog() {
		catalog[entry.Name] = entry
	}
	for name, requiredProperties := range map[string][]string{
		ToolSearchName: {"sources", "truncated"},
		ToolFetchName:  {"url", "statusCode", "body", "truncated"},
	} {
		entry, ok := catalog[name]
		if !ok || entry.OutputSchema == nil || entry.OutputSchema["additionalProperties"] != false {
			t.Fatalf("%s catalog output schema = %#v", name, entry.OutputSchema)
		}
		properties, _ := entry.OutputSchema["properties"].(map[string]any)
		for _, property := range requiredProperties {
			if _, ok := properties[property]; !ok {
				t.Fatalf("%s output schema is missing %q: %#v", name, property, properties)
			}
		}
	}
}
