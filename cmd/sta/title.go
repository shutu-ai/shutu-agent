package main

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
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
				_, _ = a.appendSessionTitle(ctx, sessionID, title, session.TitleSourceFallback)
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
	if a.shutdownStarted() {
		return
	}
	// Only attempt when a provider is selected and the app is fully started.
	// The minimal test apps leave baseCtx nil, so they never fire a background
	// model call.
	if a.baseCtx == nil {
		return
	}
	providerID, model, err := a.sessionProviderModelStrict(sessionID)
	if err != nil || a.llmFor(providerID) == nil {
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
	a.titleWG.Add(1)
	go func() {
		defer a.titleWG.Done()
		a.generateLLMTitle(sessionID, providerID, model)
	}()
}

// waitTitleWorkers joins asynchronous title generation before providers and
// the session store are closed during process shutdown.
func (a *app) waitTitleWorkers() {
	if a != nil {
		a.titleWG.Wait()
	}
}

// generateLLMTitle runs one model title call in the background and, unless the
// session became user-pinned in the meantime, stores the normalized result with
// the model source. Fail-open: any error (including a store torn down under the
// goroutine during process/test teardown) keeps the current title.
func (a *app) generateLLMTitle(sessionID, providerID, model string) {
	defer func() { _ = recover() }()
	if meta, err := a.store.GetSessionMeta(a.baseCtx, sessionID); err != nil || meta.TitleSource == session.TitleSourceUser {
		return
	}
	text := a.firstEligibleUserText(a.baseCtx, sessionID)
	if text == "" {
		return
	}
	title, err := a.llmTitleFor(a.baseCtx, providerID, model, text)
	if err != nil {
		return
	}
	// Re-check the pin before writing so a concurrent rename is never overwritten.
	if meta, err := a.store.GetSessionMeta(a.baseCtx, sessionID); err != nil || meta.TitleSource == session.TitleSourceUser {
		return
	}
	// Serialize the final pin check with the user rename. Otherwise an
	// in-flight automatic result could append after a rename and overwrite the
	// user-owned title even though the metadata check just passed.
	a.titleMu.Lock()
	if meta, err := a.store.GetSessionMeta(a.baseCtx, sessionID); err != nil || meta.TitleSource == session.TitleSourceUser {
		a.titleMu.Unlock()
		return
	}
	_, _ = a.appendSessionTitleWithRouteLocked(a.baseCtx, sessionID, title, session.TitleSourceLLM, providerID, model)
	a.titleMu.Unlock()
}

// appendSessionTitle commits the accepted title to the canonical session log.
// The SQLite sink projects the same event into sidebar metadata atomically;
// callers must not write the metadata column as a second source of truth.
func (a *app) appendSessionTitle(ctx context.Context, sessionID, title, source string) (session.Event, error) {
	a.titleMu.Lock()
	defer a.titleMu.Unlock()
	return a.appendSessionTitleLocked(ctx, sessionID, title, source)
}

func (a *app) appendSessionTitleLocked(ctx context.Context, sessionID, title, source string) (session.Event, error) {
	return a.appendSessionTitleWithRouteLocked(ctx, sessionID, title, source, "", "")
}

func (a *app) appendSessionTitleWithRouteLocked(ctx context.Context, sessionID, title, source, providerID, model string) (session.Event, error) {
	log, err := a.sessionLogForAgent(ctx, sessionID)
	if err != nil {
		return session.Event{}, err
	}
	kind := source
	sourceValue := map[string]any{"kind": kind}
	if source == session.TitleSourceLLM {
		kind = "provider"
		sourceValue = map[string]any{"kind": kind, "provider": providerID}
		if model != "" {
			sourceValue["model"] = map[string]any{"provider": providerID, "model": model}
		}
	}
	return log.Append(session.EventSessionTitle, map[string]any{
		"title":       title,
		"messageSeqs": []uint64{},
		"source":      sourceValue,
	})
}

// nativeRenameSession is the production composition-root callback for the
// native session.rename RPC. It returns the actual durable event sequence so
// the client can settle its title projection before the mux push arrives.
func (a *app) nativeRenameSession(ctx context.Context, sessionID, title string) (int64, error) {
	ev, err := a.appendSessionTitle(ctx, sessionID, title, session.TitleSourceUser)
	if err != nil {
		return 0, err
	}
	return int64(ev.Seq), nil
}

// llmTitle uses the selected provider to produce a one-line title for the given
// first-message text. The output is normalized and byte-bounded.
func (a *app) llmTitle(ctx context.Context, text string) (string, error) {
	return a.llmTitleFor(ctx, "", a.providerConfigSnapshot().Model, text)
}

func (a *app) llmTitleFor(ctx context.Context, providerID, model, text string) (string, error) {
	provider := a.llmFor(providerID)
	if provider == nil {
		return "", errors.New("title: no LLM provider")
	}
	user := llm.Message{Role: llm.RoleUser}
	user.SetText("Generate the session title from this human message:\n" + text)
	system := llm.Message{Role: llm.RoleSystem}
	system.SetText(titleSystemPrompt)
	reader, err := provider.Stream(ctx, llm.ChatRequest{
		Model:    model,
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
	projected, err := projection.Build(events)
	if err != nil {
		return ""
	}
	return projected.FirstUserText
}
