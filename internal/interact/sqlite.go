package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
)

// SQLiteProvider is the durable approval backend used by the application when
// its Store exposes ApprovalStore. The approval event stream remains the audit
// and replay contract; this projection keeps pending/resolved requests
// available between process restarts and makes concurrent answerers compare
// and set the same durable row.
type SQLiteProvider struct {
	mu     sync.Mutex
	store  store.ApprovalStore
	nextID uint64
	closed bool
}

func NewSQLiteProvider(backend store.ApprovalStore) (Provider, error) {
	if backend == nil {
		return nil, fmt.Errorf("interact: approval store is nil")
	}
	return &SQLiteProvider{store: backend, nextID: uint64(time.Now().UnixNano())}, nil
}

func (p *SQLiteProvider) Name() string { return "sqlite" }

func (p *SQLiteProvider) List(ctx context.Context) ([]Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrProviderClosed
	}
	rows, err := p.store.ListApprovalRows(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Request, 0, len(rows))
	for _, row := range rows {
		request, err := requestFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, nil
}

// ListForSession keeps the durable read itself scoped. Do not implement this
// as List followed by an in-process filter: Web/ACP answerers are security
// boundaries, and a large approval prompt/args payload for another session
// should never be loaded into their process path.
func (p *SQLiteProvider) ListForSession(ctx context.Context, sessionID string) ([]Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("interact: session id is required")
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrProviderClosed
	}
	scoped, ok := p.store.(store.ApprovalSessionStore)
	if !ok {
		// Compatibility stores can still be used safely; the Engine applies the
		// ownership filter before returning the result to its caller.
		items, err := p.List(ctx)
		if err != nil {
			return nil, err
		}
		result := make([]Request, 0)
		for _, item := range items {
			if item.SessionID == sessionID {
				result = append(result, item)
			}
		}
		return result, nil
	}
	rows, err := scoped.ListApprovalRowsForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	result := make([]Request, 0, len(rows))
	for _, row := range rows {
		request, err := requestFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, nil
}

func (p *SQLiteProvider) Create(ctx context.Context, request Request) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Request{}, ErrProviderClosed
	}
	for {
		p.nextID++
		request.ID = fmt.Sprintf("req-%d", p.nextID)
		err := p.store.CreateApprovalRow(ctx, rowFromRequest(request))
		if err == nil {
			return cloneRequest(request), nil
		}
		// A nanosecond-derived ID is unique in normal operation. If another
		// process happened to choose it, advance and retry; other storage errors
		// must remain visible to the caller.
		if !isApprovalIDConflict(err) {
			return Request{}, err
		}
	}
}

// CreateWithEvent allocates the request id and commits the pending projection
// with its asked audit event in one SQLite transaction. The callback receives
// the detached request carrying the final provider id.
func (p *SQLiteProvider) CreateWithEvent(ctx context.Context, request Request, event func(Request) session.Event) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if event == nil {
		return Request{}, errors.New("interact: approval event callback is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Request{}, ErrProviderClosed
	}
	backend, ok := p.store.(store.ApprovalRequestEventStore)
	if !ok {
		return Request{}, errors.New("interact: approval store does not support atomic request events")
	}
	for {
		p.nextID++
		request.ID = fmt.Sprintf("req-%d", p.nextID)
		created := cloneRequest(request)
		audit := event(created)
		if err := backend.CreateApprovalAndAppendEvent(ctx, rowFromRequest(created), created.SessionID, audit); err == nil {
			return created, nil
		} else if !isApprovalIDConflict(err) {
			return Request{}, err
		}
	}
}

func (p *SQLiteProvider) Resolve(ctx context.Context, id string, status ApprovalStatus) error {
	return p.resolve(ctx, id, status, "")
}

func (p *SQLiteProvider) ResolveWithAnswer(ctx context.Context, id string, status ApprovalStatus, answer string) error {
	return p.resolve(ctx, id, status, answer)
}

func (p *SQLiteProvider) AtomicEventSupported() bool {
	_, ok := p.store.(store.ApprovalEventStore)
	return ok
}

// ResolveWithAnswerAndEvent delegates the terminal CAS plus audit append to
// the SQLite transaction. The engine only calls this after validating the
// session ownership, status and structured answer.
func (p *SQLiteProvider) ResolveWithAnswerAndEvent(ctx context.Context, id string, status ApprovalStatus, answer, sessionID string, event session.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrProviderClosed
	}
	backend, ok := p.store.(store.ApprovalEventStore)
	if !ok {
		return fmt.Errorf("interact: approval store does not support atomic audit commits")
	}
	err := backend.ResolveApprovalAndAppendEvent(ctx, id, string(status), answer, time.Now().UTC(), sessionID, event)
	if errors.Is(err, store.ErrApprovalNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownRequest, id)
	}
	if errors.Is(err, store.ErrApprovalResolved) {
		return fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	return err
}

func (p *SQLiteProvider) resolve(ctx context.Context, id string, status ApprovalStatus, answer string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrProviderClosed
	}
	err := p.store.ResolveApprovalRow(ctx, id, string(status), answer, time.Now().UTC())
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrApprovalNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownRequest, id)
	}
	if errors.Is(err, store.ErrApprovalResolved) {
		return fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	return err
}

// Restore is used both during startup replay and by the app's durable-append
// rollback path. Upsert preserves the request identity and terminal status.
func (p *SQLiteProvider) Restore(ctx context.Context, requests []Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrProviderClosed
	}
	rows := make([]store.ApprovalRow, 0, len(requests))
	for _, request := range requests {
		if request.ID == "" {
			return fmt.Errorf("interact: restored request id is empty")
		}
		rows = append(rows, rowFromRequest(request))
	}
	return p.store.RestoreApprovalRows(ctx, rows)
}

// Replace rebuilds the durable projection from the session event log. The
// provider remains the live CAS backend, while startup reconciliation removes
// rows that have no corresponding durable audit fact.
func (p *SQLiteProvider) Replace(ctx context.Context, requests []Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrProviderClosed
	}
	rows := make([]store.ApprovalRow, 0, len(requests))
	for _, request := range requests {
		if request.ID == "" {
			return fmt.Errorf("interact: replaced request id is empty")
		}
		rows = append(rows, rowFromRequest(request))
	}
	replacer, ok := p.store.(store.ApprovalProjectionReplacer)
	if !ok {
		return fmt.Errorf("interact: approval store cannot replace projection")
	}
	return replacer.ReplaceApprovalRows(ctx, rows)
}

func (p *SQLiteProvider) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func rowFromRequest(request Request) store.ApprovalRow {
	questions, _ := json.Marshal(request.Questions)
	return store.ApprovalRow{
		ID: request.ID, SessionID: request.SessionID, CallID: request.CallID,
		Prompt: request.Prompt, ToolName: request.ToolName, Args: request.Args,
		Questions: questions, Answer: request.Answer, Status: string(request.Status),
		CreatedAt: request.CreatedAt, ResolvedAt: request.ResolvedAt, ExpiresAt: request.ExpiresAt,
	}
}

func requestFromRow(row store.ApprovalRow) (Request, error) {
	var questions []Question
	if len(row.Questions) > 0 && string(row.Questions) != "null" {
		if err := json.Unmarshal(row.Questions, &questions); err != nil {
			return Request{}, fmt.Errorf("interact: decode durable questions %q: %w", row.ID, err)
		}
	}
	return Request{
		ID: row.ID, SessionID: row.SessionID, CallID: row.CallID, Prompt: row.Prompt,
		ToolName: row.ToolName, Args: row.Args, Questions: questions, Answer: row.Answer,
		Status: ApprovalStatus(row.Status), CreatedAt: row.CreatedAt,
		ResolvedAt: row.ResolvedAt, ExpiresAt: row.ExpiresAt,
	}, nil
}

func isApprovalIDConflict(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "constraint failed"))
}
