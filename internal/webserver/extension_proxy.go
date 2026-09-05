package webserver

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

func (s *Server) extensionRoute(prefix string) (ExtensionRoute, bool) {
	s.extensionMu.RLock()
	defer s.extensionMu.RUnlock()
	for _, route := range s.extensionRoutes {
		if "/extensions/"+urlPathSegment(route.ExtensionID)+"/" == prefix ||
			"/extensions/"+urlPathSegment(route.ExtensionID) == prefix {
			return route, true
		}
	}
	return ExtensionRoute{}, false
}

func (s *Server) handleExtensionList(w http.ResponseWriter, r *http.Request) {
	s.extensionMu.RLock()
	routes := append([]ExtensionRoute(nil), s.extensionRoutes...)
	s.extensionMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"extensions": routes})
}

func (s *Server) handleExtensionRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path+"/", http.StatusTemporaryRedirect)
}

func (s *Server) handleExtensionReverseProxy(w http.ResponseWriter, r *http.Request) {
	segments := strings.SplitN(r.URL.Path, "/", 4)
	if len(segments) < 4 {
		http.NotFound(w, r)
		return
	}
	prefix := "/extensions/" + strings.Trim(segments[2], "/") + "/"
	route, ok := s.extensionRoute(prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, err := url.Parse(route.ServiceURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		http.Error(w, "extension web service is unavailable", http.StatusServiceUnavailable)
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = singleJoiningSlash(target.Path, strings.TrimPrefix(req.URL.Path, prefix))
			req.URL.RawPath = ""
			req.Host = target.Host
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Set("X-Shutu-Extension-ID", route.ExtensionID)
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, proxyErr error) {
			http.Error(writer, "extension web service is unavailable", http.StatusServiceUnavailable)
		},
	}
	proxy.ServeHTTP(w, r)
}

func singleJoiningSlash(left, right string) string {
	leftSlash := strings.HasSuffix(left, "/")
	rightSlash := strings.HasPrefix(right, "/")
	switch {
	case leftSlash && rightSlash:
		return left + right[1:]
	case !leftSlash && !rightSlash:
		return left + "/" + right
	default:
		return left + right
	}
}

func urlPathSegment(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '_':
			out.WriteRune(char)
		case char == '.' || char == ' ':
			out.WriteRune('-')
		}
	}
	return out.String()
}
