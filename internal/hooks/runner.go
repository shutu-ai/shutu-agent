// Package hooks provides a bounded, metadata-only external event hook.
// Hook processes observe committed event metadata after persistence succeeds;
// session contents, tool arguments, and tool outputs never leave the agent.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

const DefaultTimeout = 10 * time.Second

// Config describes one executable metadata notification hook. Command and
// Args are passed directly to exec.Command; shell syntax is unsupported.
type Config struct {
	Command    string
	Args       []string
	Events     []string
	Timeout    time.Duration
	WorkingDir string
}

// Runner tracks detached hook processes and drains them on Close.
type Runner struct {
	config Config
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
}

func New(config Config) (*Runner, error) {
	if strings.TrimSpace(config.Command) == "" {
		return nil, errors.New("hooks: command is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{config: config, ctx: ctx, cancel: cancel}, nil
}

// Notify schedules a metadata-only hook for matching event types. It returns
// immediately and is safe to call from the session persistence sink.
func (r *Runner) Notify(sessionID string, ev session.Event) {
	if r == nil || !r.matches(ev.Type) {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		_ = r.run(sessionID, ev)
	}()
}

// Close cancels active hooks and waits until every detached process exits.
func (r *Runner) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return nil
}

func (r *Runner) matches(eventType string) bool {
	if strings.HasPrefix(eventType, "hook/") {
		return false
	}
	if len(r.config.Events) == 0 {
		return true
	}
	for _, candidate := range r.config.Events {
		if strings.TrimSpace(candidate) == eventType {
			return true
		}
	}
	return false
}

func (r *Runner) run(sessionID string, ev session.Event) error {
	ctx, cancel := context.WithTimeout(r.ctx, r.config.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.config.Command, r.config.Args...)
	if r.config.WorkingDir != "" {
		cmd.Dir = r.config.WorkingDir
	}
	cmd.Env = scrubbedEnv()
	cmd.Stdin = strings.NewReader(payload(sessionID, ev))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func payload(sessionID string, ev session.Event) string {
	body, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"event": map[string]any{
			"seq": ev.Seq, "type": ev.Type, "at": ev.At.UTC(), "version": ev.Version,
		},
	})
	return string(body) + "\n"
}

func scrubbedEnv() []string {
	const sensitive = "KEY SECRET TOKEN PASSWORD API"
	out := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(name)
		blocked := false
		for _, token := range strings.Fields(sensitive) {
			if strings.Contains(upper, token) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, entry)
		}
	}
	return out
}
