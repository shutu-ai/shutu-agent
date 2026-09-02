package llm

import (
	"net/http"
	"strings"
)

// AttributionUserAgent identifies provider traffic made by the shutu agent.
// Provider adapters call ApplyAttributionHeaders at the final HTTP boundary so
// direct clients and composition-root clients expose the same application
// identity without allowing an adapter to overwrite an explicit caller value.
const AttributionUserAgent = "shutu-agent/0.1 (+https://github.com/jabing/shutu-agent)"

// ApplyAttributionHeaders adds the stable application User-Agent when the
// caller has not supplied one. The helper is deliberately side-effect free for
// nil headers and never copies credentials or session data into the header.
func ApplyAttributionHeaders(header http.Header) {
	if header == nil || strings.TrimSpace(header.Get("User-Agent")) != "" {
		return
	}
	header.Set("User-Agent", AttributionUserAgent)
}
