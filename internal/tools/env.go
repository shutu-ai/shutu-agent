package tools

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
)

// ManagedEnvFunc supplies trusted harness facts for one shell invocation.
// Values are collected per call so session identity never leaks across turns.
type ManagedEnvFunc func() map[string]string

var bashEnvOverrides = []string{
	"NO_COLOR=1",
	"TERM=dumb",
	"PAGER=cat",
	"GIT_PAGER=cat",
}

func shellEnv(managed ManagedEnvFunc) []string {
	env := scrubbedEnv()
	if managed == nil {
		return env
	}
	values := managed()
	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func bashEnv(managed ManagedEnvFunc) []string {
	return overrideEnv(shellEnv(managed), bashEnvOverrides)
}

func shellEnvContext(ctx context.Context, legacy ManagedEnvFunc, scoped func(context.Context) map[string]string) []string {
	if scoped == nil {
		return shellEnv(legacy)
	}
	return shellEnvValues(scoped(ctx))
}

func bashEnvContext(ctx context.Context, legacy ManagedEnvFunc, scoped func(context.Context) map[string]string) []string {
	return overrideEnv(shellEnvContext(ctx, legacy, scoped), bashEnvOverrides)
}

func shellEnvValues(values map[string]string) []string {
	env := scrubbedEnv()
	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func overrideEnv(env, overrides []string) []string {
	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		replaced := false
		for _, ov := range overrides {
			overrideName, _, _ := strings.Cut(ov, "=")
			if strings.EqualFold(name, overrideName) {
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return append(out, overrides...)
}

// NewManagedDshEnv returns the standard dsh environment projection for a
// process-local data directory and a dynamic session-id provider.
func NewManagedDshEnv(home string, sessionID func() string) ManagedEnvFunc {
	return func() map[string]string {
		id := ""
		if sessionID != nil {
			id = sessionID()
		}
		return dshEnv(home, id)
	}
}

func dshEnv(home, sessionID string) map[string]string {
	values := map[string]string{"DSH_SHELL": "1"}
	if home != "" {
		if !filepath.IsAbs(home) {
			if abs, err := filepath.Abs(home); err == nil {
				home = abs
			}
		}
		values["DSH_HOME"] = home
	}
	if sessionID != "" {
		values["DSH_SESSION_ID"] = sessionID
	}
	return values
}
