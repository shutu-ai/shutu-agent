package webserver

import "testing"

func TestNativeProjectionBaselineIncludesMountedDSHKeys(t *testing.T) {
	values := newNativeProjectionCursor().projectionBlock("", -1).Values
	want := []string{
		"title", "todos", "plan", "tokenUsage", "contextPressure", "contextBreakdown",
		"goal", "permissions", "subagent", "subagentTiming", "sessionListMetadata", "sessionStats",
	}
	for _, key := range want {
		if _, ok := values[key]; !ok {
			t.Fatalf("projection baseline is missing DSH key %q: %#v", key, values)
		}
	}
	if values["title"] != nil || values["goal"] != nil || values["todos"] != nil {
		t.Fatalf("empty projection sent non-null optional values: %#v", values)
	}
	plan, ok := values["plan"].(map[string]any)
	if !ok || plan["active"] != false || plan["pending"] != false {
		t.Fatalf("initial plan projection = %#v", values["plan"])
	}
	usage, ok := values["tokenUsage"].(map[string]any)
	if !ok || usage["uncachedInputTokens"] != int64(0) || usage["outputTokens"] != int64(0) {
		t.Fatalf("initial token usage projection = %#v", values["tokenUsage"])
	}
}
