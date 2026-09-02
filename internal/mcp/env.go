package mcp

import (
	"os"
	"strings"
)

// credentialEnvTokens intentionally follows the subprocess policy used by
// the local code and terminal providers.  MCP servers are external programs,
// so ambient credentials must not cross this boundary by default.
var credentialEnvTokens = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "API"}

func scrubbedEnv() []string {
	return scrubEnv(os.Environ())
}

func scrubEnv(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || sensitiveEnvName(name) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func sensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, token := range credentialEnvTokens {
		if strings.Contains(upper, token) {
			return true
		}
	}
	return false
}
