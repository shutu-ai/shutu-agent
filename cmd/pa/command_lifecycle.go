package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
)

var commandSequence atomic.Uint64

func newCommandID() string {
	return fmt.Sprintf("shutu-cmd-%d-%d", time.Now().UnixNano(), commandSequence.Add(1))
}

func appendCommandRun(log *session.Log, name, args string) (string, error) {
	if log == nil {
		return "", errors.New("no active session")
	}
	// dsh-command-feedback deliberately sets recordInput:false: feedback is
	// sensitive user text and must appear only in feedback/record, never in the
	// generic command/run payload.
	if name == "feedback" {
		args = ""
	}
	id := newCommandID()
	if _, err := log.Append(session.EventCommandRun, session.NewCommandRun(id, name, args)); err != nil {
		return "", err
	}
	return id, nil
}

func appendCommandDone(log *session.Log, id, kind, text string, sourceEventSeq ...uint64) error {
	if log == nil {
		return errors.New("no active session")
	}
	_, err := log.Append(session.EventCommandDone, session.NewCommandDone(id, kind, text, sourceEventSeq...))
	return err
}

func latestWebCommandResultSeq(log *session.Log, after uint64) uint64 {
	if log == nil {
		return 0
	}
	var latest uint64
	for _, event := range log.Events() {
		if event.Seq > after && event.Type == session.EventWebCommandResult {
			latest = event.Seq
		}
	}
	return latest
}

func nativeCommandLifecycle(name string) bool {
	switch name {
	case "/help", "/list", "/llm-status", "/attach", "/term", "/eval-status", "/compact", "/feedback", "/plan":
		return true
	default:
		return false
	}
}

func commandArgs(line, name string) string {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) <= len(name) {
		return ""
	}
	return strings.TrimSpace(trimmed[len(name):])
}
