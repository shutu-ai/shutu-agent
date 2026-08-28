package mcp

import (
	"regexp"
	"strings"
	"testing"
)

func TestPublicToolNameKeepsCleanNames(t *testing.T) {
	if got := PublicToolName("filesystem", "read_file"); got != "mcp__filesystem__read_file" {
		t.Fatalf("clean name = %q", got)
	}
}

func TestPublicToolNameNormalizesAndHashesLossyNames(t *testing.T) {
	first := PublicToolName("server.with punctuation", strings.Repeat("tool/", 30))
	second := PublicToolName("server.with punctuation", strings.Repeat("tool/", 30)+"x")
	if len(first) > 64 || len(second) > 64 {
		t.Fatalf("names exceed DSH limit: %d/%d", len(first), len(second))
	}
	if !regexp.MustCompile(`^mcp__[A-Za-z0-9_-]+_[0-9a-f]{12}$`).MatchString(first) {
		t.Fatalf("normalized name = %q, want normalized prefix plus hash", first)
	}
	if first == second {
		t.Fatalf("distinct MCP identities collapsed to %q", first)
	}
}
