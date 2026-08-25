package interact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// maxArgsLen bounds the stored args JSON in runes. An over-long payload is
	// rejected at Request time so a request can never grow unbounded.
	maxArgsLen = 200
	// defaultPendingLimit caps the number of concurrently pending requests
	// (default 20); a Request that would exceed it is rejected.
	defaultPendingLimit = 20
	// defaultPollInterval is the Await poll cadence: a short sleep between List
	// probes. The caller drives Await on its own serial path, so the interval
	// trades a little latency for zero background machinery (D5).
	defaultPollInterval = 100 * time.Millisecond
)

// engine is the default approval Service implementation (ADR 决策 M6d): it owns
// validation — prompt legality, args bound, status legality, duplicate-resolution
// rejection, the pending cap — delegating storage to a Provider. It is safe for
// concurrent use; Close is idempotent and releases the Provider. The unexported
// concrete type keeps the Engine interface the only public shape; NewEngine
// returns it as a concrete *engine that satisfies Engine.
type engine struct {
	prov Provider

	mu           sync.Mutex
	closed       bool
	pendingLimit int
	poll         time.Duration
	answers      map[string]string
}

// NewEngine returns an engine backed by prov; a nil prov selects the default
// in-memory Provider (newMemProvider). Each engine should own its provider:
// Close releases it.
func NewEngine(prov Provider) *engine {
	if prov == nil {
		prov = newMemProvider()
	}
	return &engine{
		prov:         prov,
		pendingLimit: defaultPendingLimit,
		poll:         defaultPollInterval,
		answers:      make(map[string]string),
	}
}

// Request validates the prompt and args, applies the pending cap, and creates a
// pending request through the Provider, returning it with its provider-issued
// id.
func (e *engine) Request(ctx context.Context, prompt, toolName, args string) (Request, error) {
	return e.request(ctx, prompt, toolName, args, nil)
}

// RequestWithQuestions creates a DSH-style structured question request while
// retaining the same provider and lifecycle as ordinary approvals.
func (e *engine) RequestWithQuestions(ctx context.Context, prompt, toolName, args string, questions []Question) (Request, error) {
	return e.request(ctx, prompt, toolName, args, questions)
}

// Restore repopulates a restorable provider during application startup. The
// in-memory provider supports this for compatibility with the durable session
// event log; ordinary request creation remains unchanged.
func (e *engine) Restore(ctx context.Context, requests []Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkOpen(); err != nil {
		return err
	}
	restorer, ok := e.prov.(RequestRestorer)
	if !ok {
		return fmt.Errorf("interact: provider %q cannot restore requests", e.prov.Name())
	}
	return restorer.Restore(ctx, requests)
}

func (e *engine) request(ctx context.Context, prompt, toolName, args string, questions []Question) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, err
	}
	if prompt == "" {
		return Request{}, fmt.Errorf("%w: prompt is empty", ErrInvalidPrompt)
	}
	if utf8.RuneCountInString(args) > maxArgsLen {
		return Request{}, fmt.Errorf("%w: args exceed %d runes", ErrInvalidArgs, maxArgsLen)
	}
	all, err := e.prov.List(ctx)
	if err != nil {
		return Request{}, err
	}
	pending := 0
	for _, r := range all {
		if r.Status == StatusPending {
			pending++
		}
	}
	if pending >= e.pendingLimit {
		return Request{}, fmt.Errorf("%w: %d pending", ErrPendingLimit, e.pendingLimit)
	}
	copyQuestions := append([]Question(nil), questions...)
	for i := range copyQuestions {
		copyQuestions[i].Options = append([]QuestionOption(nil), copyQuestions[i].Options...)
	}
	return e.prov.Create(ctx, Request{
		Prompt:    prompt,
		ToolName:  toolName,
		Args:      args,
		Questions: copyQuestions,
		Status:    StatusPending,
		CreatedAt: time.Now(),
	})
}

// Resolve records the user's decision (approved or rejected) for the request
// with id. An unknown id, a request already resolved and an invalid status are
// rejected; the resolved request is returned.
func (e *engine) Resolve(ctx context.Context, id string, status ApprovalStatus) (Request, error) {
	return e.resolveWithAnswer(ctx, id, status, "")
}

// ResolveWithAnswer resolves a request and retains its structured answer for
// the blocked tool path to observe through Await.
func (e *engine) ResolveWithAnswer(ctx context.Context, id string, status ApprovalStatus, answer string) (Request, error) {
	return e.resolveWithAnswer(ctx, id, status, answer)
}

// Cancel marks a pending structured question as dismissed by the user or by
// its aborted caller.
func (e *engine) Cancel(ctx context.Context, id string) (Request, error) {
	return e.resolveWithAnswer(ctx, id, StatusCanceled, "")
}

func (e *engine) resolveWithAnswer(ctx context.Context, id string, status ApprovalStatus, answer string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, err
	}
	if status != StatusApproved && status != StatusRejected && status != StatusCanceled {
		return Request{}, fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	r, err := e.findRequest(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if r.Status != StatusPending {
		return Request{}, fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	if err := validateAnswer(r.Questions, answer); err != nil {
		return Request{}, err
	}
	if err := e.prov.Resolve(ctx, id, status); err != nil {
		return Request{}, err
	}
	e.mu.Lock()
	if answer != "" {
		e.answers[id] = answer
	}
	e.mu.Unlock()
	// Read the stored copy back so the returned request matches the record the
	// Provider persists (its ResolvedAt timestamp in particular).
	return e.findRequest(ctx, id)
}

func validateAnswer(questions []Question, answer string) error {
	if len(questions) == 0 || strings.TrimSpace(answer) == "" {
		return nil
	}
	var payload struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
			Custom   string   `json:"custom"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(answer), &payload); err != nil {
		return fmt.Errorf("interact: invalid structured answer: %w", err)
	}
	known := make(map[string]Question, len(questions))
	for _, question := range questions {
		known[question.ID] = question
	}
	seen := make(map[string]struct{}, len(payload.Answers))
	for _, item := range payload.Answers {
		question, ok := known[item.ID]
		if !ok {
			return fmt.Errorf("interact: answer references unknown question %q", item.ID)
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return fmt.Errorf("interact: duplicate answer for question %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		if !question.MultiSelect && len(item.Selected) > 1 {
			return fmt.Errorf("interact: question %q accepts one option", item.ID)
		}
		allowed := make(map[string]struct{}, len(question.Options))
		for _, option := range question.Options {
			allowed[option.Label] = struct{}{}
		}
		for _, selected := range item.Selected {
			if _, ok := allowed[selected]; !ok {
				return fmt.Errorf("interact: invalid option %q for question %q", selected, item.ID)
			}
		}
	}
	return nil
}

// Await blocks until the request with id leaves pending — a Resolve made by the
// user, a context cancellation or a disappearing request. v1 has no background
// wait (D5): Await polls the Provider on a short interval, so a resolution made
// in another goroutine becomes visible on the next poll. An unknown id fails
// fast.
func (e *engine) Await(ctx context.Context, id string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, err
	}
	timer := time.NewTimer(e.poll)
	defer timer.Stop()
	for {
		r, err := e.findRequest(ctx, id)
		if err != nil {
			return Request{}, err
		}
		if r.Status != StatusPending {
			return r, nil
		}
		timer.Reset(e.poll)
		select {
		case <-ctx.Done():
			return Request{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// List returns every current request, sorted by id.
func (e *engine) List(ctx context.Context) ([]Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.checkOpen(); err != nil {
		return nil, err
	}
	items, err := e.prov.List(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	for i := range items {
		items[i].Answer = e.answers[items[i].ID]
	}
	e.mu.Unlock()
	return items, nil
}

// findRequest locates the request with id through the Provider; an unknown id
// is rejected.
func (e *engine) findRequest(ctx context.Context, id string) (Request, error) {
	all, err := e.prov.List(ctx)
	if err != nil {
		return Request{}, err
	}
	for _, r := range all {
		if r.ID == id {
			e.mu.Lock()
			r.Answer = e.answers[id]
			e.mu.Unlock()
			return r, nil
		}
	}
	return Request{}, fmt.Errorf("%w: %s", ErrUnknownRequest, id)
}

// checkOpen rejects operations on a closed engine.
func (e *engine) checkOpen() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEngineClosed
	}
	return nil
}

// Close releases the backend (if it implements closer) and marks the engine
// closed so every other operation is rejected. It is idempotent.
func (e *engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	prov := e.prov
	e.mu.Unlock()
	if c, ok := prov.(closer); ok {
		return c.Close()
	}
	return nil
}
