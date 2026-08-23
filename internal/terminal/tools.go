package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jabing/shutu-agent/internal/session"
)

// ToolPwshName is the persistent-shell tool name (dsh pwsh): one tool that
// runs commands in the configured persistent shell (Pwsh / Git Bash / WSL /
// Cmd). config.terminalToolNames whitelist corresponds to it.
const ToolPwshName = "pwsh"

// ErrNoActive reports that no terminal session exists for the current owner
// (the pwsh tool auto-starts on this error; any other access error, e.g. the
// owner fence, is surfaced to the model).
var ErrNoActive = errors.New("no active terminal session")

// toolViewportMax bounds every model-facing terminal text (dispatch-m9-2 §4).
const toolViewportMax = 8000

// TerminalAccess is implemented by the composition root (cmd/pa): it provides
// access to the current session's terminal and validates ownership.
type TerminalAccess interface {
	// Owner returns the current session id (owner of the terminal).
	Owner() string
	// GetActive returns the active session, or an error when there is none
	// or the owner does not match.
	GetActive() (*Session, error)
	// Start launches a fresh session; it errors with "already active" when a
	// session is already running.
	Start(opts SessionOpts) (*Session, error)
	// Stop shuts down the active session.
	Stop() error
}

// TerminalTools bundles the shared state of the pwsh tool.
type TerminalTools struct {
	acc     TerminalAccess
	onEvent func(typ string, data any)
}

// NewTerminalTools builds the shared tool state. onEvent receives the
// session.EventTerminalStart / session.EventTerminalStop notifications.
func NewTerminalTools(acc TerminalAccess, onEvent func(typ string, data any)) *TerminalTools {
	return &TerminalTools{acc: acc, onEvent: onEvent}
}

// Pwsh returns the pwsh tool.
func (t *TerminalTools) Pwsh() PwshTool { return PwshTool{t: t} }

// emit forwards a terminal lifecycle event to the registered callback.
func (t *TerminalTools) emit(typ string, data any) {
	if t.onEvent != nil {
		t.onEvent(typ, data)
	}
}

// PwshTool runs one command line in the persistent shell session (dsh pwsh).
// The shell session is created on first use (the configured shell: Pwsh / Git
// Bash / WSL / Cmd); every call writes the command, waits for the shell to go
// idle (or the read timeout), and returns the accumulated viewport output.
type PwshTool struct {
	t *TerminalTools
}

func (x PwshTool) Name() string { return ToolPwshName }

func (x PwshTool) Description() string {
	return "run a command line in the persistent shell session (created on first use); returns the command output"
}

func (x PwshTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "the command line to run in the persistent shell",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}
}

func (x PwshTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("pwsh: %w", err)
	}
	if a.Command == "" {
		return "", fmt.Errorf("pwsh: command is required")
	}
	sess, err := x.t.acc.GetActive()
	if errors.Is(err, ErrNoActive) {
		// No active session for the current owner: the persistent shell comes
		// up on first use (dsh).
		sess, err = x.t.acc.Start(SessionOpts{})
		if err != nil {
			return "", fmt.Errorf("pwsh: %w", err)
		}
		x.t.emit(session.EventTerminalStart, session.NewTerminalStart(sess.ID(), x.t.acc.Owner()))
	} else if err != nil {
		// An owner-fence or other access error is surfaced, never masked by an
		// auto-start attempt.
		return "", fmt.Errorf("pwsh: %w", err)
	}
	res, err := sess.Write(a.Command, true)
	if err != nil {
		return "", fmt.Errorf("pwsh: %w", err)
	}
	out := fmt.Sprintf("wait=%s status=%s", res.Wait, res.Status.Kind)
	out += "\nviewport:\n" + truncateView(res.Viewport, toolViewportMax)
	if res.Truncated {
		out += "\n[viewport truncated]"
	}
	return out, nil
}

const truncateNotice = "\n[terminal output truncated]"

// truncateView shortens model-facing terminal text to at most maxBytes,
// backing off to a rune boundary and appending a truncated notice.
func truncateView(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return s
	}
	if len(s) <= maxBytes {
		return s
	}
	head := truncateUTF8(s, maxBytes-len(truncateNotice))
	return head + truncateNotice
}

// truncateUTF8 shortens s to at most maxBytes bytes without splitting a rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for !utf8.ValidString(s) {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}
