package main

import (
	"context"
	"errors"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/sessionreference"
)

// prepareSessionReference is the Web admission boundary for canonical session
// mentions. It snapshots referenced sessions before a turn runs and rewrites
// the direct prompt to the readable @label form.
func (a *app) prepareSessionReference(ctx context.Context, sessionID, text string) (string, *llm.Message, error) {
	if a == nil || a.store == nil {
		return text, nil, nil
	}
	prepared, err := sessionreference.PrepareText(ctx, a.store, sessionID, text)
	if err != nil {
		return "", nil, err
	}
	return prepared.Text, prepared.Context, nil
}

// appendSessionReferenceContext persists the prepared recall row immediately
// before the direct user message is projected by the turn.
func (a *app) appendSessionReferenceContext(ctx context.Context, sessionID string, message *llm.Message) error {
	if message == nil {
		return nil
	}
	log := a.log
	if a.agentRegistry != nil && sessionID != "" {
		addressed, err := a.sessionLogForAgent(ctx, sessionID)
		if err != nil {
			return err
		}
		log = addressed
	} else if sessionID != "" && sessionID != a.currentID {
		addressed := a.webLog(ctx)
		if addressed != nil {
			log = addressed
		}
	}
	if log == nil {
		return errors.New("no active session")
	}
	_, err := log.Append(session.EventUserMessage, session.NewContextMessageFromLLM(*message))
	return err
}
