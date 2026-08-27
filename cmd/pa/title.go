package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

// session-title alignment with @shutu-ai/dsh-session-title (full alignment,
// including the first-prompt model provider): after the first eligible human
// message, a deterministic fallback (first words) is stored immediately and an
// asynchronous model call frames a cleaner title, which replaces the fallback
// unless the user has pinned the session with a rename. Automatic work never
// blocks the turn and never overwrites a user-set title.

// titleSystemPrompt mirrors the dsh session-title-llm system instruction; the
// title is one plain line in the message's language, about 5 words or 10 CJK
// characters.
const titleSystemPrompt = `Create a concise title for an AI coding-assistant session from the supplied human messages.
Return only the title on one line, in plain text of natural language, with no quotes, prefix, explanation, Markdown, XML, or terminal control codes. No code is allowed.
Use the language of the messages.
Aim for about 5 words in non-CJK languages or 10 CJK characters.`

// ensureSessionTitle makes the session's display title ready after a user
// message: it materializes the deterministic fallback immediately and schedules
// the asynchronous model title. Fail-open; a title failure never affects the
// turn or later answers.
func (a *app) ensureSessionTitle(ctx context.Context, sessionID string) {
	if sessionID == "" || a.store == nil {
		return
	}
	meta, err := a.store.GetSessionMeta(ctx, sessionID)
	if err != nil {
		return
	}
	// Pinned titles (user rename) are never auto-revised (dsh rename semantics).
	if meta.TitleSource == session.TitleSourceUser {
		return
	}
	if meta.Title == "" {
		if text := a.firstEligibleUserText(ctx, sessionID); text != "" {
			title := session.FallbackTitle(text, session.TitleFallbackMaxWords, session.TitleFallbackMaxBytes)
			if title != "" {
				_ = a.store.SetSessionTitle(ctx, sessionID, title, session.TitleSourceFallback)
				meta.Title = title
				meta.TitleSource = session.TitleSourceFallback
			}
		}
	}
	a.maybeScheduleLLMTitle(sessionID)
}

// maybeScheduleLLMTitle fires the asynchronous model title for a session once
// per process lifetime (the stored source is 'fallback' until a run completes).
// A session already pinned or already titled by the model is skipped.
func (a *app) maybeScheduleLLMTitle(sessionID string) {
	// Only attempt when a provider is selected and the app is fully started.
	// The minimal test apps leave baseCtx nil, so they never fire a background
	// model call.
	if a.currentLLM() == nil || a.baseCtx == nil {
		return
	}
	a.titleMu.Lock()
	if a.titleDone == nil {
		a.titleDone = map[string]bool{}
	}
	if a.titleDone[sessionID] {
		a.titleMu.Unlock()
		return
	}
	a.titleDone[sessionID] = true // reserve so concurrent turns do not double-fire
	a.titleMu.Unlock()
	go a.generateLLMTitle(sessionID)
}

// generateLLMTitle runs one model title call in the background and, unless the
// session became user-pinned in the meantime, stores the normalized result with
// the model source. Fail-open: any error (including a store torn down under the
// goroutine during process/test teardown) keeps the current title.
func (a *app) generateLLMTitle(sessionID string) {
	defer func() { _ = recover() }()
	if meta, err := a.store.GetSessionMeta(a.baseCtx, sessionID); err != nil || meta.TitleSource == session.TitleSourceUser {
		return
	}
	text := a.firstEligibleUserText(a.baseCtx, sessionID)
	if text == "" {
		return
	}
	title, err := a.llmTitle(a.baseCtx, text)
	if err != nil {
		return
	}
	// Re-check the pin before writing so a concurrent rename is never overwritten.
	if meta, err := a.store.GetSessionMeta(a.baseCtx, sessionID); err != nil || meta.TitleSource == session.TitleSourceUser {
		return
	}
	_ = a.store.SetSessionTitle(a.baseCtx, sessionID, title, session.TitleSourceLLM)
}

// llmTitle uses the selected provider to produce a one-line title for the given
// first-message text. The output is normalized and byte-bounded.
func (a *app) llmTitle(ctx context.Context, text string) (string, error) {
	provider := a.currentLLM()
	if provider == nil {
		return "", errors.New("title: no LLM provider")
	}
	user := llm.Message{Role: llm.RoleUser}
	user.SetText("Generate the session title from this human message:\n" + text)
	system := llm.Message{Role: llm.RoleSystem}
	system.SetText(titleSystemPrompt)
	reader, err := provider.Stream(ctx, llm.ChatRequest{
		Model:    a.cfg.Model,
		Messages: []llm.Message{system, user},
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if ev.Kind == llm.StreamTextDelta {
			sb.WriteString(ev.Text)
		}
		if ev.Kind == llm.StreamFinish {
			break
		}
	}
	title := session.NormalizeTitle(sb.String(), session.TitleMaxBytes)
	if title == "" {
		return "", errors.New("title: model produced no text")
	}
	return title, nil
}

// firstEligibleUserText returns the first non-empty user/message text of a
// session, or "" when there is none yet.
func (a *app) firstEligibleUserText(ctx context.Context, sessionID string) string {
	if a.store == nil {
		return ""
	}
	events, err := a.store.LoadSession(ctx, sessionID)
	if err != nil {
		return ""
	}
	for _, ev := range events {
		if ev.Type != session.EventUserMessage {
			continue
		}
		var d struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		if strings.TrimSpace(d.Text) == "" {
			continue
		}
		return d.Text
	}
	return ""
}
