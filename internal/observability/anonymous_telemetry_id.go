package observability

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// getOrCreateAnonymousTelemetryID mirrors the reference's lazy, profile-local
// anonymous identity. The file is deliberately separate from credentials and
// is never derived from a session or machine identifier.
func getOrCreateAnonymousTelemetryID(dataDir string) (string, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return "", errors.New("session telemetry: data directory is empty")
	}
	path := filepath.Join(dataDir, telemetryAnonymousIDFile)
	if raw, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(raw))
		if validAnonymousTelemetryID(id) {
			return id, nil
		}
		return "", fmt.Errorf("session telemetry: invalid anonymous user id in %q", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("session telemetry: read anonymous user id: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("session telemetry: create data directory: %w", err)
	}
	rawID := make([]byte, 16)
	if _, err := rand.Read(rawID); err != nil {
		return "", fmt.Errorf("session telemetry: generate anonymous user id: %w", err)
	}
	id := "anonymous-" + hex.EncodeToString(rawID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", fmt.Errorf("session telemetry: read concurrently-created id: %w", readErr)
			}
			id = strings.TrimSpace(string(raw))
			if !validAnonymousTelemetryID(id) {
				return "", fmt.Errorf("session telemetry: invalid concurrently-created id in %q", path)
			}
			return id, nil
		}
		return "", fmt.Errorf("session telemetry: create anonymous user id: %w", err)
	}
	if _, err := file.WriteString(id + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("session telemetry: write anonymous user id: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("session telemetry: sync anonymous user id: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("session telemetry: close anonymous user id: %w", err)
	}
	return id, nil
}

func validAnonymousTelemetryID(id string) bool {
	if len(id) != len("anonymous-")+32 || !strings.HasPrefix(id, "anonymous-") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "anonymous-"))
	return err == nil
}
