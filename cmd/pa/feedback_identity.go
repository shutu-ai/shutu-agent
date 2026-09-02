package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const anonymousFeedbackIdentityFile = ".anonymous-user-id"

// feedbackAnonymousUserID returns the stable identity shared by feedback
// acknowledgements in one data directory. It is created only after accepted
// feedback, matching dsh's lazy anonymous-user-id behavior. Deployments that
// construct an app without a data directory (small embedders and unit tests)
// receive a process-local fallback rather than a misleading persisted id.
func (a *app) feedbackAnonymousUserID() (string, error) {
	if a == nil {
		return "", errors.New("feedback identity is unavailable")
	}
	a.feedbackMu.Lock()
	defer a.feedbackMu.Unlock()
	if a.feedbackAnonymousID != "" {
		return a.feedbackAnonymousID, nil
	}

	dataDir := strings.TrimSpace(a.cfg.DataDir)
	if dataDir == "" {
		id, err := newAnonymousFeedbackID()
		if err != nil {
			return "", err
		}
		a.feedbackAnonymousID = id
		return id, nil
	}
	path := filepath.Join(dataDir, anonymousFeedbackIdentityFile)
	if raw, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(raw))
		if validAnonymousFeedbackID(id) {
			a.feedbackAnonymousID = id
			return id, nil
		}
		return "", fmt.Errorf("invalid anonymous feedback identity in %q", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read anonymous feedback identity: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create feedback identity directory: %w", err)
	}
	id, err := newAnonymousFeedbackID()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return "", fmt.Errorf("read concurrently-created feedback identity: %w", readErr)
			}
			id = strings.TrimSpace(string(raw))
			if !validAnonymousFeedbackID(id) {
				return "", fmt.Errorf("invalid anonymous feedback identity in %q", path)
			}
			a.feedbackAnonymousID = id
			return id, nil
		}
		return "", fmt.Errorf("create anonymous feedback identity: %w", err)
	}
	if _, err := file.WriteString(id + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write anonymous feedback identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("sync anonymous feedback identity: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close anonymous feedback identity: %w", err)
	}
	a.feedbackAnonymousID = id
	return id, nil
}

func newAnonymousFeedbackID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate anonymous feedback identity: %w", err)
	}
	return "anonymous-" + hex.EncodeToString(raw[:]), nil
}

func validAnonymousFeedbackID(id string) bool {
	if len(id) != len("anonymous-")+32 || !strings.HasPrefix(id, "anonymous-") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "anonymous-"))
	return err == nil
}
