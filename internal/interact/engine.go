package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jabing/shutu-agent/internal/session"
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

	mu sync.Mutex
	// requestMu serializes the provider List+Create admission window. Without
	// it, concurrent Agent turns could all observe the same pending count and
	// collectively exceed pendingLimit even though each individual request was
	// valid.
	requestMu     sync.Mutex
	closed        bool
	closeDone     chan struct{}
	pendingLimit  int
	poll          time.Duration
	answers       map[string]string
	defaultPolicy ApprovalPolicy
	policies      map[string]ApprovalPolicy
	requestTTL    time.Duration
	expiryAuditor func(context.Context, Request) error
}

// SetRequestTTL configures automatic expiry for pending requests. Zero
// disables expiry; expiry is evaluated on List/Await/Resolve reads and is
// recorded by the provider as a terminal StatusExpired decision.
func (e *engine) SetRequestTTL(ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	e.mu.Lock()
	e.requestTTL = ttl
	e.mu.Unlock()
}

func (e *engine) SetExpiryAuditor(auditor func(context.Context, Request) error) {
	e.mu.Lock()
	e.expiryAuditor = auditor
	e.mu.Unlock()
}

// NewEngine returns an engine backed by prov; a nil prov selects the default
// in-memory Provider (newMemProvider). Each engine should own its provider:
// Close releases it.
func NewEngine(prov Provider) *engine {
	if prov == nil {
		prov = newMemProvider()
	}
	return &engine{
		prov:          prov,
		pendingLimit:  defaultPendingLimit,
		poll:          defaultPollInterval,
		answers:       make(map[string]string),
		defaultPolicy: PolicyAsk,
		policies:      make(map[string]ApprovalPolicy),
		closeDone:     make(chan struct{}),
	}
}

func (e *engine) SetDefaultPolicy(policy ApprovalPolicy) error {
	if policy != PolicyAsk && policy != PolicyNever {
		return fmt.Errorf("%w: %q", ErrInvalidPolicy, policy)
	}
	e.mu.Lock()
	e.defaultPolicy = policy
	e.mu.Unlock()
	return nil
}

func (e *engine) SetSessionPolicy(sessionID string, policy ApprovalPolicy) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidPolicy)
	}
	if policy != PolicyAsk && policy != PolicyNever {
		return fmt.Errorf("%w: %q", ErrInvalidPolicy, policy)
	}
	e.mu.Lock()
	e.policies[sessionID] = policy
	e.mu.Unlock()
	return nil
}

func (e *engine) SessionPolicy(sessionID string) ApprovalPolicy {
	e.mu.Lock()
	defer e.mu.Unlock()
	if policy, ok := e.policies[sessionID]; ok {
		return policy
	}
	return e.defaultPolicy
}

func (e *engine) ClearSessionPolicy(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	e.mu.Lock()
	delete(e.policies, sessionID)
	e.mu.Unlock()
}

// CancelForSession marks all currently pending requests owned by sessionID as
// cancelled. Each transition still goes through the normal provider CAS, so
// a concurrent answerer can win one request without affecting the others.
func (e *engine) CancelForSession(ctx context.Context, sessionID string) ([]Request, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("%w: session id is required", ErrWrongSession)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.checkOpen(); err != nil {
		return nil, err
	}
	items, err := e.ListForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	cancelled := make([]Request, 0)
	for _, item := range items {
		if item.Status != StatusPending {
			continue
		}
		resolved, resolveErr := e.resolveForSession(ctx, sessionID, item.ID, StatusCanceled, "")
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrAlreadyResolved) {
				continue
			}
			return cancelled, resolveErr
		}
		cancelled = append(cancelled, resolved)
	}
	return cancelled, nil
}

func (e *engine) RequestForSession(ctx context.Context, sessionID, prompt, toolName, args string) (Request, error) {
	return e.requestForSessionWithCallID(ctx, sessionID, "", prompt, toolName, args)
}

// RequestForSessionWithCallID is the correlation-preserving approval entry
// point used by tool gates. The call id is data, not an authorization key.
func (e *engine) RequestForSessionWithCallID(ctx context.Context, sessionID, callID, prompt, toolName, args string) (Request, error) {
	return e.requestForSessionWithCallID(ctx, sessionID, callID, prompt, toolName, args)
}

// RequestForSessionWithCallIDAndEvent uses an atomic durable provider when one
// is available. The bool is false for compatibility providers; callers must
// then append the asked event through their normal rollback-aware path.
func (e *engine) RequestForSessionWithCallIDAndEvent(ctx context.Context, sessionID, callID, prompt, toolName, args string, event func(Request) session.Event) (Request, bool, error) {
	if event == nil {
		return Request{}, false, fmt.Errorf("interact: approval event callback is nil")
	}
	if err := ctx.Err(); err != nil {
		return Request{}, false, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, false, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, false, fmt.Errorf("%w: session id is required", ErrInvalidPolicy)
	}
	if prompt == "" {
		return Request{}, false, fmt.Errorf("%w: prompt is empty", ErrInvalidPrompt)
	}
	if utf8.RuneCountInString(args) > maxArgsLen {
		return Request{}, false, fmt.Errorf("%w: args exceed %d runes", ErrInvalidArgs, maxArgsLen)
	}
	if e.SessionPolicy(sessionID) == PolicyNever {
		// DSH's never policy is a deterministic denial, not an unavailable
		// answerer. Keep this terminal and id-less: no pending approval exists
		// and therefore there is nothing for a caller to await or resolve.
		return Request{SessionID: sessionID, CallID: callID, Prompt: prompt, ToolName: toolName, Args: args, Status: StatusRejected, CreatedAt: time.Now().UTC()}, false, nil
	}
	return e.requestWithCallIDResult(ctx, sessionID, callID, prompt, toolName, args, nil, event)
}

func (e *engine) requestForSessionWithCallID(ctx context.Context, sessionID, callID, prompt, toolName, args string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, fmt.Errorf("%w: session id is required", ErrInvalidPolicy)
	}
	if prompt == "" {
		return Request{}, fmt.Errorf("%w: prompt is empty", ErrInvalidPrompt)
	}
	if utf8.RuneCountInString(args) > maxArgsLen {
		return Request{}, fmt.Errorf("%w: args exceed %d runes", ErrInvalidArgs, maxArgsLen)
	}
	if e.SessionPolicy(sessionID) == PolicyNever {
		return Request{SessionID: sessionID, CallID: callID, Prompt: prompt, ToolName: toolName, Args: args, Status: StatusRejected, CreatedAt: time.Now().UTC()}, nil
	}
	r, _, err := e.requestWithCallIDResult(ctx, sessionID, callID, prompt, toolName, args, nil, nil)
	if err != nil {
		return Request{}, err
	}
	return r, nil
}

func (e *engine) requestWithCallID(ctx context.Context, sessionID, callID, prompt, toolName, args string, questions []Question) (Request, error) {
	request, _, err := e.requestWithCallIDResult(ctx, sessionID, callID, prompt, toolName, args, questions, nil)
	return request, err
}

func (e *engine) requestWithCallIDResult(ctx context.Context, sessionID, callID, prompt, toolName, args string, questions []Question, event func(Request) session.Event) (Request, bool, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, false, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, false, err
	}
	if prompt == "" {
		return Request{}, false, fmt.Errorf("%w: prompt is empty", ErrInvalidPrompt)
	}
	if utf8.RuneCountInString(args) > maxArgsLen {
		return Request{}, false, fmt.Errorf("%w: args exceed %d runes", ErrInvalidArgs, maxArgsLen)
	}
	// Pending-cap admission is a read/modify/write operation at the Provider
	// boundary. Keep the lock across the provider List and Create so multiple
	// concurrent sessions cannot pass the same stale count.
	e.requestMu.Lock()
	defer e.requestMu.Unlock()
	all, err := e.prov.List(ctx)
	if err != nil {
		return Request{}, false, err
	}
	pending := 0
	for _, r := range all {
		if r.Status == StatusPending {
			pending++
		}
	}
	if pending >= e.pendingLimit {
		return Request{}, false, fmt.Errorf("%w: %d pending", ErrPendingLimit, pending)
	}
	copyQuestions := append([]Question(nil), questions...)
	for i := range copyQuestions {
		copyQuestions[i].Options = append([]QuestionOption(nil), copyQuestions[i].Options...)
	}
	createdAt := time.Now().UTC()
	var expiresAt *time.Time
	e.mu.Lock()
	ttl := e.requestTTL
	e.mu.Unlock()
	if ttl > 0 {
		expires := createdAt.Add(ttl)
		expiresAt = &expires
	}
	request := Request{SessionID: sessionID, CallID: callID, Prompt: prompt, ToolName: toolName,
		Args: args, Questions: copyQuestions, Status: StatusPending, CreatedAt: createdAt, ExpiresAt: expiresAt}
	if event != nil {
		if creator, ok := e.prov.(interface {
			CreateWithEvent(context.Context, Request, func(Request) session.Event) (Request, error)
		}); ok {
			created, err := creator.CreateWithEvent(ctx, request, event)
			return created, true, err
		}
	}
	created, err := e.prov.Create(ctx, request)
	return created, false, err
}

func (e *engine) RequestForSessionWithQuestions(ctx context.Context, sessionID, prompt, toolName, args string, questions []Question) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, fmt.Errorf("%w: session id is required", ErrInvalidPolicy)
	}
	if prompt == "" {
		return Request{}, fmt.Errorf("%w: prompt is empty", ErrInvalidPrompt)
	}
	if utf8.RuneCountInString(args) > maxArgsLen {
		return Request{}, fmt.Errorf("%w: args exceed %d runes", ErrInvalidArgs, maxArgsLen)
	}
	if e.SessionPolicy(sessionID) == PolicyNever {
		return Request{SessionID: sessionID, Prompt: prompt, ToolName: toolName, Args: args, Questions: questions, Status: StatusRejected, CreatedAt: time.Now().UTC()}, nil
	}
	return e.requestWithCallID(ctx, sessionID, "", prompt, toolName, args, questions)
}

// RequestForSessionWithQuestionsAndEvent atomically creates a structured
// question and its canonical asked event when the durable provider supports
// the transaction. The false result retains compatibility with providers that
// can only create the request; callers must then append the event through their
// normal rollback-aware path.
func (e *engine) RequestForSessionWithQuestionsAndEvent(ctx context.Context, sessionID, prompt, toolName, args string, questions []Question, event func(Request) session.Event) (Request, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, false, fmt.Errorf("%w: session id is required", ErrInvalidPolicy)
	}
	if e.SessionPolicy(sessionID) == PolicyNever {
		return Request{SessionID: sessionID, Prompt: prompt, ToolName: toolName, Args: args, Questions: questions, Status: StatusRejected, CreatedAt: time.Now().UTC()}, false, nil
	}
	return e.requestWithCallIDResult(ctx, sessionID, "", prompt, toolName, args, questions, event)
}

// Request validates the prompt and args, applies the pending cap, and creates a
// pending request through the Provider, returning it with its provider-issued
// id.
func (e *engine) Request(ctx context.Context, prompt, toolName, args string) (Request, error) {
	return e.request(ctx, "", prompt, toolName, args, nil)
}

// RequestWithQuestions creates a DSH-style structured question request while
// retaining the same provider and lifecycle as ordinary approvals.
func (e *engine) RequestWithQuestions(ctx context.Context, prompt, toolName, args string, questions []Question) (Request, error) {
	return e.request(ctx, "", prompt, toolName, args, questions)
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
	if err := restorer.Restore(ctx, requests); err != nil {
		return err
	}
	e.mu.Lock()
	for _, request := range requests {
		if request.Status == StatusPending {
			// A durable-append rollback may restore a request that was resolved
			// in memory. Do not leave its answer in the side map: a later Await
			// must observe the restored pending record only.
			delete(e.answers, request.ID)
		} else if request.Answer != "" {
			e.answers[request.ID] = request.Answer
		}
	}
	e.mu.Unlock()
	return nil
}

func (e *engine) request(ctx context.Context, sessionID, prompt, toolName, args string, questions []Question) (Request, error) {
	return e.requestWithCallID(ctx, sessionID, "", prompt, toolName, args, questions)
}

func (e *engine) ResolveForSession(ctx context.Context, sessionID, id string, status ApprovalStatus) (Request, error) {
	return e.resolveForSession(ctx, sessionID, id, status, "")
}

func (e *engine) ResolveForSessionWithAnswer(ctx context.Context, sessionID, id string, status ApprovalStatus, answer string) (Request, error) {
	return e.resolveForSession(ctx, sessionID, id, status, answer)
}

// ResolveForSessionWithAnswerAndEvent is the crash-safe decision seam. When
// the provider exposes an atomic backend, it commits the approval CAS and the
// supplied canonical audit event together. Older providers use the normal
// resolver and return atomic=false so the caller can retain its compatibility
// append/rollback path.
func (e *engine) ResolveForSessionWithAnswerAndEvent(ctx context.Context, sessionID, id string, status ApprovalStatus, answer string, event session.Event) (Request, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, false, fmt.Errorf("%w: session id is required", ErrWrongSession)
	}
	if err := ctx.Err(); err != nil {
		return Request{}, false, err
	}
	if err := e.checkOpen(); err != nil {
		return Request{}, false, err
	}
	r, err := e.findRequest(ctx, id)
	if err != nil {
		return Request{}, false, err
	}
	if r.SessionID == "" || r.SessionID != sessionID {
		return Request{}, false, fmt.Errorf("%w: %s", ErrWrongSession, id)
	}
	if status != StatusApproved && status != StatusAllowedOnce && status != StatusRejected && status != StatusCanceled && status != StatusUnavailable {
		return Request{}, false, fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	if r.Status != StatusPending {
		return Request{}, false, fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	if err := validateAnswer(r.Questions, answer); err != nil {
		return Request{}, false, err
	}
	if resolver, ok := e.prov.(interface {
		AtomicEventSupported() bool
		ResolveWithAnswerAndEvent(context.Context, string, ApprovalStatus, string, string, session.Event) error
	}); ok && resolver.AtomicEventSupported() {
		if err := resolver.ResolveWithAnswerAndEvent(ctx, id, status, answer, sessionID, event); err != nil {
			return Request{}, false, err
		}
		e.mu.Lock()
		if answer != "" {
			e.answers[id] = answer
		}
		e.mu.Unlock()
		resolved, err := e.findRequest(ctx, id)
		return resolved, true, err
	}
	resolved, err := e.resolveWithAnswer(ctx, id, status, answer)
	return resolved, false, err
}

func (e *engine) resolveForSession(ctx context.Context, sessionID, id string, status ApprovalStatus, answer string) (Request, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, fmt.Errorf("%w: session id is required", ErrWrongSession)
	}
	r, err := e.findRequest(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if r.SessionID == "" || r.SessionID != sessionID {
		return Request{}, fmt.Errorf("%w: %s", ErrWrongSession, id)
	}
	return e.resolveWithAnswer(ctx, id, status, answer)
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
	if status != StatusApproved && status != StatusAllowedOnce && status != StatusRejected && status != StatusCanceled && status != StatusUnavailable {
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
	var resolveErr error
	if resolver, ok := e.prov.(interface {
		ResolveWithAnswer(context.Context, string, ApprovalStatus, string) error
	}); ok {
		resolveErr = resolver.ResolveWithAnswer(ctx, id, status, answer)
	} else {
		resolveErr = e.prov.Resolve(ctx, id, status)
	}
	if resolveErr != nil {
		return Request{}, resolveErr
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

// AwaitForSession is the ownership-preserving counterpart to Await. The
// ownership check is repeated on every poll because a durable provider may be
// shared by multiple app processes and the addressed record can disappear or
// be replaced while the waiter is asleep.
func (e *engine) AwaitForSession(ctx context.Context, sessionID, id string) (Request, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, fmt.Errorf("%w: session id is required", ErrWrongSession)
	}
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
		if r.SessionID == "" || r.SessionID != sessionID {
			return Request{}, fmt.Errorf("%w: %s", ErrWrongSession, id)
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
	if err := e.expireDue(ctx, items); err != nil {
		return nil, err
	}
	if expired := hasExpired(items); expired {
		items, err = e.prov.List(ctx)
		if err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	for i := range items {
		items[i].Answer = e.answers[items[i].ID]
	}
	e.mu.Unlock()
	return items, nil
}

// ListForSession returns only requests owned by sessionID. Empty or missing
// ownership is intentionally not treated as a wildcard: an answerer must
// prove the addressed session before receiving approval records.
func (e *engine) ListForSession(ctx context.Context, sessionID string) ([]Request, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("%w: session id is required", ErrWrongSession)
	}
	var items []Request
	var err error
	if scoped, ok := e.prov.(ProviderSessionLister); ok {
		items, err = scoped.ListForSession(ctx, sessionID)
	} else {
		items, err = e.List(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err := e.expireDue(ctx, items); err != nil {
		return nil, err
	}
	if expired := hasExpired(items); expired {
		if scoped, ok := e.prov.(ProviderSessionLister); ok {
			items, err = scoped.ListForSession(ctx, sessionID)
		} else {
			items, err = e.List(ctx)
		}
		if err != nil {
			return nil, err
		}
	}
	e.mu.Lock()
	for i := range items {
		items[i].Answer = e.answers[items[i].ID]
	}
	e.mu.Unlock()
	owned := make([]Request, 0, len(items))
	for _, item := range items {
		if item.SessionID == sessionID {
			owned = append(owned, item)
		}
	}
	return owned, nil
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
			if err := e.expireOne(ctx, r); err != nil {
				return Request{}, err
			}
			if r.Status == StatusPending && expired(r) {
				resolved, resolveErr := e.prov.List(ctx)
				if resolveErr != nil {
					return Request{}, resolveErr
				}
				for _, candidate := range resolved {
					if candidate.ID == id {
						r = candidate
						break
					}
				}
			}
			e.mu.Lock()
			r.Answer = e.answers[id]
			e.mu.Unlock()
			return r, nil
		}
	}
	return Request{}, fmt.Errorf("%w: %s", ErrUnknownRequest, id)
}

func expired(r Request) bool {
	return r.Status == StatusPending && r.ExpiresAt != nil && !time.Now().Before(*r.ExpiresAt)
}

func hasExpired(items []Request) bool {
	for _, r := range items {
		if expired(r) {
			return true
		}
	}
	return false
}

func (e *engine) expireOne(ctx context.Context, r Request) error {
	if !expired(r) {
		return nil
	}
	if err := e.prov.Resolve(ctx, r.ID, StatusExpired); err != nil {
		return err
	}
	e.mu.Lock()
	auditor := e.expiryAuditor
	e.mu.Unlock()
	if auditor != nil && r.SessionID != "" {
		if err := auditor(ctx, r); err != nil {
			return fmt.Errorf("interact: audit expired request %s: %w", r.ID, err)
		}
	}
	return nil
}

func (e *engine) expireDue(ctx context.Context, items []Request) error {
	for _, r := range items {
		if err := e.expireOne(ctx, r); err != nil && !errors.Is(err, ErrAlreadyResolved) {
			return err
		}
	}
	return nil
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
	// Serialize shutdown with the provider List+Create admission window. This
	// makes the pending sweep below a real lifecycle boundary: no request can
	// be admitted after the engine becomes closed, and private ACP engines do
	// not leave answerable requests behind after their session disappears.
	e.requestMu.Lock()
	e.mu.Lock()
	if e.closed {
		done := e.closeDone
		e.mu.Unlock()
		e.requestMu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	e.closed = true
	prov := e.prov
	e.mu.Unlock()
	var first error
	if items, err := prov.List(context.Background()); err != nil {
		first = err
	} else {
		for _, item := range items {
			if item.Status != StatusPending {
				continue
			}
			if err := prov.Resolve(context.Background(), item.ID, StatusUnavailable); err != nil && !errors.Is(err, ErrAlreadyResolved) && first == nil {
				first = err
			}
		}
	}
	e.requestMu.Unlock()
	if c, ok := prov.(closer); ok {
		err := c.Close()
		close(e.closeDone)
		if first == nil {
			first = err
		}
		return first
	}
	close(e.closeDone)
	return first
}
