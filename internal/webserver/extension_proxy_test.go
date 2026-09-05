package webserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtensionRoutesProxyAndList(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("extension-page:" + r.URL.Path + ":" + r.Header.Get("X-Shutu-Extension-ID")))
	}))
	defer backend.Close()

	srv, _ := newTestServer(t, "tok")
	srv.SetExtensionRoutes([]ExtensionRoute{{
		ExtensionID: "demo", Title: "Demo", Route: "/extensions/demo/", Icon: "🧩",
		NavigationEnabled: true, NavigationGroup: "Data", Order: 20, Ready: true, ServiceURL: backend.URL,
	}})
	page := doReq(t, srv.Handler(), http.MethodGet, "/extensions/demo/app.js", "tok")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "extension-page:/app.js:demo") {
		t.Fatalf("extension page = %d/%s", page.Code, page.Body.String())
	}
	list := doReq(t, srv.Handler(), http.MethodGet, "/api/extensions", "tok")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"icon":"🧩"`) ||
		!strings.Contains(list.Body.String(), `"navigationGroup":"Data"`) || !strings.Contains(list.Body.String(), `"order":20`) {
		t.Fatalf("extension list = %d/%s", list.Code, list.Body.String())
	}
	if unauth := doReq(t, srv.Handler(), http.MethodGet, "/extensions/demo/", ""); unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated extension route = %d", unauth.Code)
	}
}
