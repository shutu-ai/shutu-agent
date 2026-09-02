// webserver_test.go - the M10a composition-root wiring tests (docs/dispatch-m10
// section 5): the D10 gate (disabled => no server), the fail-closed empty-token
// path, and the enabled path serving health/sessions through the authenticated
// handler. The server goroutine binds 127.0.0.1:0 (ephemeral) so tests never
// collide with a real port; assertions go through Handler() on httptest.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/webserver"
)

func intPtr(value int) *int { return &value }

func stringPtr(value string) *string { return &value }

func makeWebServerApp(t *testing.T, enabled bool, token string) (*app, *store.SQLiteStore) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<div id=\"react-root\">DSH</div>"), 0o644); err != nil {
		t.Fatalf("write frontend index: %v", err)
	}
	return &app{
		cfg: config.Config{WebServer: config.WebServerConfig{
			Enabled: enabled,
			Addr:    "127.0.0.1:0", // ephemeral in tests; production default is 127.0.0.1:8080
			Token:   token,
			DistDir: dist,
		}},
		store: st,
	}, st
}

func TestRedactMCPHeadersNeverReturnsValues(t *testing.T) {
	got := redactMCPHeaders(map[string]string{"Authorization": "Bearer secret", "X-Empty": ""})
	if got["Authorization"] != "[redacted]" || strings.Contains(got["Authorization"], "secret") {
		t.Fatalf("authorization header was not redacted: %#v", got)
	}
	if got["X-Empty"] != "" {
		t.Fatalf("empty header changed: %#v", got)
	}
}

func TestRedactMCPArgsPreservesShapeAndRestoresMaskedValues(t *testing.T) {
	original := []string{"--api-key", "secret-key", "--mode", "stdio", "TOKEN=env-secret", "--header=Authorization: Bearer secret"}
	masked := redactMCPArgs(original)
	want := []string{"--api-key", redactedMCPValue, "--mode", "stdio", "TOKEN=" + redactedMCPValue, "--header=" + redactedMCPValue}
	if !reflect.DeepEqual(masked, want) {
		t.Fatalf("masked args = %#v, want %#v", masked, want)
	}
	if got := restoreMCPArgs(masked, original); !reflect.DeepEqual(got, original) {
		t.Fatalf("restored args = %#v, want %#v", got, original)
	}
}

func TestRedactMCPURLPreservesEndpointAndRestoresCredentials(t *testing.T) {
	original := "https://user:secret@example.test/mcp?api_key=query-secret&keep=value"
	masked := redactMCPURL(original)
	if strings.Contains(masked, "secret") || !strings.Contains(masked, "keep=value") || !strings.Contains(masked, "redacted") {
		t.Fatalf("masked URL = %q", masked)
	}
	if got := restoreMCPURL(masked, original); got != original {
		t.Fatalf("restored URL = %q, want %q", got, original)
	}
}

func TestMCPUpdateRestoresProjectedSecrets(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app{
		store: st,
		cfg: config.Config{Mcp: config.McpConfig{Servers: []config.McpServer{{
			Name: "secure", Transport: "streamable-http", URL: "https://u:pass@example.test/mcp?api_key=query-secret&keep=value",
			Args: []string{"--token", "arg-secret"}, Headers: map[string]string{"Authorization": "Bearer header-secret"}, Env: map[string]string{"MCP_SECRET": "env-secret", "MCP_MODE": "strict"}, Cwd: "/srv/mcp", ToolCallTimeoutMS: 1234, FailOnStartupError: true,
		}}}},
	}
	server := a.cfg.Mcp.Servers[0]
	_, err = a.webManageMCP(context.Background(), "update", webserver.MCPServerEdit{
		OriginalName: server.Name, Name: server.Name, Transport: server.Transport,
		URL: redactMCPURL(server.URL), Args: redactMCPArgs(server.Args), Headers: redactMCPHeaders(server.Headers), Env: redactMCPEnv(server.Env), Cwd: stringPtr(server.Cwd), ToolCallTimeoutMS: intPtr(server.ToolCallTimeoutMS),
	})
	if err != nil {
		t.Fatalf("update projected MCP config: %v", err)
	}
	got := a.providerConfigSnapshot().Mcp.Servers[0]
	if got.URL != server.URL || !reflect.DeepEqual(got.Args, server.Args) || !reflect.DeepEqual(got.Headers, server.Headers) || !reflect.DeepEqual(got.Env, server.Env) || got.Cwd != server.Cwd || got.ToolCallTimeoutMS != server.ToolCallTimeoutMS || got.FailOnStartupError != server.FailOnStartupError {
		t.Fatalf("projected update changed secrets: got=%+v want=%+v", got, server)
	}
	view := a.webMCPServers()
	if len(view) != 1 {
		t.Fatalf("MCP view = %#v, want one server", view)
	}
	env, _ := view[0]["env"].(map[string]string)
	if env["MCP_SECRET"] != redactedMCPValue || env["MCP_MODE"] != redactedMCPValue {
		t.Fatalf("MCP view leaked or dropped env values: %#v", view[0]["env"])
	}
}

func TestMCPUpdateOmittedCwdPreservesConfiguredDirectory(t *testing.T) {
	a := makePlanApp(true)
	a.store = nil
	a.cfg.Mcp.Servers = []config.McpServer{{Name: "local", Transport: "stdio", Cmd: "mcp", Cwd: "/srv/mcp", ToolCallTimeoutMS: 60000}}
	// The production path persists through SQLite; use the same helper setup as
	// the redaction test when the app has a store available.
	if a.store == nil {
		st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "mcp-cwd.db"))
		if err != nil {
			t.Fatal(err)
		}
		a.store = st
		defer st.Close()
	}
	server := a.cfg.Mcp.Servers[0]
	if _, err := a.webManageMCP(context.Background(), "update", webserver.MCPServerEdit{
		OriginalName: server.Name, Name: server.Name, Transport: server.Transport,
		Cmd: server.Cmd, Cwd: nil, ToolCallTimeoutMS: intPtr(server.ToolCallTimeoutMS),
	}); err != nil {
		t.Fatalf("update MCP config: %v", err)
	}
	got := a.providerConfigSnapshot().Mcp.Servers[0]
	if got.Cwd != server.Cwd {
		t.Fatalf("omitted cwd = %q, want %q", got.Cwd, server.Cwd)
	}
}

func TestRedactMCPErrorRemovesConfiguredSecrets(t *testing.T) {
	server := config.McpServer{
		URL:     "https://example.test/mcp?api_key=query-secret",
		Args:    []string{"--token", "arg-secret"},
		Headers: map[string]string{"Authorization": "Bearer header-secret"},
		Env:     map[string]string{"MCP_SECRET": "env-secret"},
	}
	err := errors.New("request https://example.test/mcp?api_key=query-secret token=arg-secret Authorization: Bearer header-secret MCP_SECRET=env-secret")
	got := redactMCPError(err, server)
	for _, secret := range []string{"query-secret", "arg-secret", "Bearer header-secret", "env-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("MCP diagnostic leaks %q: %q", secret, got)
		}
	}
}

func TestResolveFrontendDist(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "dist")
	tests := []struct {
		name       string
		configPath string
		distDir    string
		want       string
	}{
		{name: "same directory", configPath: "config.yaml", distDir: "web/dist", want: filepath.Clean("web/dist")},
		{name: "nested config", configPath: filepath.Join("configs", "agent.yaml"), distDir: filepath.Join("..", "web", "dist"), want: filepath.Clean(filepath.Join("web", "dist"))},
		{name: "absolute", configPath: "config.yaml", distDir: absolute, want: absolute},
		{name: "empty", configPath: "config.yaml", distDir: "", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveFrontendDist(test.configPath, test.distDir); got != test.want {
				t.Fatalf("resolveFrontendDist(%q, %q) = %q, want %q", test.configPath, test.distDir, got, test.want)
			}
		})
	}
}

// TestRegisterWebServerDisabledRegistersNothing verifies the D10 gate: with
// web_server.enabled=false the composition root leaves a.webserver nil and
// starts no listener (dispatch-m10 section 5).
func TestRegisterWebServerDisabledRegistersNothing(t *testing.T) {
	a, st := makeWebServerApp(t, false, "")
	defer st.Close()
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	if a.webserver != nil {
		t.Fatal("webserver must be nil when web_server.enabled=false")
	}
}

// TestRegisterWebServerEmptyTokenServesOpen verifies the D-WEB-2 change (user
// decision 2026-08-20): enabled with an empty token starts and serves open to
// the local machine (dsh-style, no login) — the old fail-closed stance is gone.
func TestRegisterWebServerEmptyTokenServesOpen(t *testing.T) {
	a, st := makeWebServerApp(t, true, "")
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "s-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	if a.webserver == nil {
		t.Fatal("webserver must be set when web_server.enabled=true (empty token = open)")
	}
	defer a.webserver.Close()

	ts := httptest.NewServer(a.webserver.Handler())
	defer ts.Close()
	// No token configured → an anonymous request is served.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health without token (no token configured) -> %d, want 200", resp.StatusCode)
	}
}

// TestRegisterWebServerEnabledServes verifies the enabled path: the server is
// set, the authenticated health/sessions APIs respond, and an unauthenticated
// request is rejected.
func TestRegisterWebServerEnabledServes(t *testing.T) {
	a, st := makeWebServerApp(t, true, "tok")
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateSession(ctx, "s-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	if a.webserver == nil {
		t.Fatal("webserver must be set when web_server.enabled=true")
	}
	defer a.webserver.Close()

	ts := httptest.NewServer(a.webserver.Handler())
	defer ts.Close()

	get := func(path, token string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/api/health", "tok"); code != http.StatusOK {
		t.Fatalf("health with token -> %d, want 200", code)
	}
	if code := get("/api/health", ""); code != http.StatusUnauthorized {
		t.Fatalf("health without token -> %d, want 401", code)
	}
	if code := get("/api/health", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("health with wrong token -> %d, want 401", code)
	}
	if code := get("/api/sessions", "tok"); code != http.StatusOK {
		t.Fatalf("sessions with token -> %d, want 200", code)
	}
	if code := get("/", "tok"); code != http.StatusOK {
		t.Fatalf("static index with token -> %d, want 200", code)
	}
}

// TestRegisterWebServerEventSourcePublishesSSE verifies the complete
// composition-root path: EventHub.Publish -> webserver subscription -> HTTP
// SSE frame. The lower webserver package already tests an injected fake source;
// this test proves cmd/pa wires the real source into that handler.
func TestRegisterWebServerEventSourcePublishesSSE(t *testing.T) {
	a, st := makeWebServerApp(t, true, "tok")
	defer st.Close()
	if err := st.CreateSession(context.Background(), "s-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	defer a.webserver.Close()

	ts := httptest.NewServer(a.webserver.Handler())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions/s-1/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d, want 200", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("event stream content type = %q", contentType)
	}

	raw, err := json.Marshal(map[string]any{"text": "live from event hub"})
	if err != nil {
		t.Fatal(err)
	}
	a.hub.Publish("s-1", session.Event{
		Seq: 1, Type: session.EventUserMessage, Version: session.EventVersion,
		At: time.Now().UTC(), Data: raw,
	})

	reader := bufio.NewReader(resp.Body)
	found := false
	for lineCount := 0; lineCount < 16; lineCount++ {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read SSE frame: %v", readErr)
		}
		if strings.Contains(line, `"seq":1`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("SSE stream did not contain the published event")
	}
}

// TestWebSubagentsJobsProviders verifies the M10 W4 (D-WEB2-H) injection: the
// composition root wires both status providers into the webserver, and a
// disabled capability (app without jobs/subagents) answers an empty list
// rather than an error.
func TestWebSubagentsJobsProviders(t *testing.T) {
	a, st := makeWebServerApp(t, true, "tok")
	defer st.Close()
	if err := a.registerWebServer(); err != nil {
		t.Fatalf("registerWebServer: %v", err)
	}
	defer a.webserver.Close()
	h := a.webserver.Handlers()
	if h.Subagents == nil || h.Jobs == nil {
		t.Fatal("registerWebServer must wire SetSubagentProvider + SetJobsProvider")
	}
	out, err := h.Subagents(context.Background(), "")
	if err != nil {
		t.Fatalf("webSubagents: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("subagents = %#v, want empty (disabled)", out)
	}
	out, err = h.Jobs(context.Background(), "")
	if err != nil {
		t.Fatalf("webJobs: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("jobs = %#v, want empty (disabled)", out)
	}
}
