package spill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// fileProvider is the durable local backend for conversation memories. The
// memory table is deliberately separate from the session event log: session
// events remain the source of truth for replay, while this file is the
// cross-session search projection that can be rebuilt or replaced later.
type fileProvider struct {
	mu     sync.Mutex
	path   string
	memos  map[string]Memo
	closed bool
}

// NewFileProvider opens a durable memory table. A missing file is treated as
// an empty table; malformed JSON fails closed so startup cannot silently use a
// truncated memory store.
func NewFileProvider(path string) (Provider, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, fmt.Errorf("spill: memory path is required")
	}
	memos := map[string]Memo{}
	raw, err := os.ReadFile(path)
	if err == nil {
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &memos); err != nil {
				return nil, fmt.Errorf("spill: decode memory store %s: %w", path, err)
			}
			if memos == nil {
				memos = map[string]Memo{}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("spill: read memory store %s: %w", path, err)
	}
	return &fileProvider{path: path, memos: memos}, nil
}

func (p *fileProvider) Name() string { return "file" }

func (p *fileProvider) List(ctx context.Context) ([]Memo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderClosed
	}
	out := make([]Memo, 0, len(p.memos))
	for _, memo := range p.memos {
		out = append(out, memo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *fileProvider) Add(ctx context.Context, memo Memo) (Memo, error) {
	if err := ctx.Err(); err != nil {
		return Memo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Memo{}, ErrProviderClosed
	}
	if memo.ID == "" {
		return Memo{}, fmt.Errorf("spill: memo id required")
	}
	next := cloneMemos(p.memos)
	if old, ok := next[memo.ID]; ok {
		memo.CreatedAt = old.CreatedAt
	}
	next[memo.ID] = memo
	if err := p.persistLocked(next); err != nil {
		return Memo{}, err
	}
	p.memos = next
	return memo, nil
}

func (p *fileProvider) Get(ctx context.Context, id string) (Memo, error) {
	if err := ctx.Err(); err != nil {
		return Memo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Memo{}, ErrProviderClosed
	}
	memo, ok := p.memos[id]
	if !ok {
		return Memo{}, fmt.Errorf("%w: %s", ErrUnknownMemo, id)
	}
	return memo, nil
}

func (p *fileProvider) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrProviderClosed
	}
	if _, ok := p.memos[id]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownMemo, id)
	}
	next := cloneMemos(p.memos)
	delete(next, id)
	if err := p.persistLocked(next); err != nil {
		return err
	}
	p.memos = next
	return nil
}

func (p *fileProvider) Search(ctx context.Context, query string, limit int) ([]Memo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderClosed
	}
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]Memo, 0)
	for _, memo := range p.memos {
		if q != "" && !strings.Contains(strings.ToLower(memo.Content), q) {
			continue
		}
		out = append(out, memo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (p *fileProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func cloneMemos(src map[string]Memo) map[string]Memo {
	dst := make(map[string]Memo, len(src))
	for id, memo := range src {
		dst[id] = memo
	}
	return dst
}

func (p *fileProvider) persistLocked(memos map[string]Memo) error {
	data, err := json.MarshalIndent(memos, "", "  ")
	if err != nil {
		return fmt.Errorf("spill: encode memory store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return fmt.Errorf("spill: create memory directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("spill: create memory temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(0o600)
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("spill: write memory store: %w", err)
	}
	if err := os.Rename(tmpName, p.path); err != nil {
		// Windows does not replace an existing file on every filesystem. The
		// fallback preserves the already-serialized value if replacement fails.
		if writeErr := os.WriteFile(p.path, data, 0o600); writeErr != nil {
			return fmt.Errorf("spill: replace memory store: %w (fallback: %v)", err, writeErr)
		}
	}
	return nil
}
