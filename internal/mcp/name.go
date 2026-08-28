package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	maxPublicToolNameLength = 64
	publicToolNameHashSize  = 12
)

// PublicToolName returns DSH's deterministic model-facing MCP name. The raw
// server tool name is never recovered from this value; it remains a wire-only
// identity. Names that need replacement or truncation receive an identity
// hash so two distinct MCP tools cannot collapse into one registry entry.
func PublicToolName(serverName, rawName string) string {
	joined := "mcp__" + serverName + "__" + rawName
	normalized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, joined)
	if normalized == joined && len(normalized) <= maxPublicToolNameLength {
		return normalized
	}
	sum := sha256.Sum256([]byte(serverName + "\x00" + rawName))
	hash := hex.EncodeToString(sum[:])[:publicToolNameHashSize]
	keep := maxPublicToolNameLength - publicToolNameHashSize - 1
	return normalized[:keep] + "_" + hash
}
