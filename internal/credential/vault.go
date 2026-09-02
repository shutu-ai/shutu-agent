// Package credential owns provider credentials independently from runtime
// settings. Configuration and protocol surfaces should carry only a stable
// reference (for example OPENAI_API_KEY); the vault resolves the value for one
// operation and controls rotation, revocation, leases, and process shutdown.
package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/store"
)

var (
	ErrClosed     = errors.New("credential: vault is closed")
	ErrNotFound   = errors.New("credential: reference not found")
	ErrRevoked    = errors.New("credential: reference is revoked")
	ErrEmptyValue = errors.New("credential: value is empty")
)

type entry struct {
	value      []byte
	generation uint64
	revoked    bool
	leases     int
}

// Vault is the process-side credential owner. A backend is optional for
// embedders/tests, but production composition should provide a durable or OS
// protected store.CredentialRecordStore implementation.
type Vault struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	backend store.CredentialRecordStore
	entries map[string]*entry
	// generations is kept separately from entries because revoked entries can
	// remain alive while an in-flight lease drains. Failed writes still consume
	// a generation locally, preventing concurrent Set calls from reusing one.
	generations map[string]uint64
	closed      bool
}

// New loads active records from backend. The returned Vault owns copies of all
// byte slices returned by the backend.
func New(ctx context.Context, backend store.CredentialRecordStore) (*Vault, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	v := &Vault{backend: backend, entries: make(map[string]*entry), generations: make(map[string]uint64)}
	if backend == nil {
		return v, nil
	}
	records, err := backend.ListCredentialRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential: load records: %w", err)
	}
	for _, record := range records {
		reference := strings.TrimSpace(record.Reference)
		if reference == "" || len(record.Value) == 0 || record.Revoked {
			continue
		}
		value := append([]byte(nil), record.Value...)
		v.entries[reference] = &entry{
			value:      value,
			generation: record.Generation,
		}
		if record.Generation > v.generations[reference] {
			v.generations[reference] = record.Generation
		}
	}
	return v, nil
}

// Acquire leases one credential until Release. Rotation/revocation prevents
// new leases but does not erase a value still used by an in-flight operation.
func (v *Vault) Acquire(ctx context.Context, reference string) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, ErrNotFound
	}
	if err := v.refresh(ctx, reference); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, ErrClosed
	}
	record, ok := v.entries[reference]
	if !ok || len(record.value) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, reference)
	}
	if record.revoked {
		return nil, fmt.Errorf("%w: %s", ErrRevoked, reference)
	}
	record.leases++
	return &Lease{vault: v, reference: reference, record: record}, nil
}

// Resolve is the short-lived convenience form of Acquire. Provider adapters
// that expose a stream should prefer Acquire when they can release at stream
// completion; this method still gives ordinary one-shot operations a bounded
// lease window.
func (v *Vault) Resolve(ctx context.Context, reference string) (string, error) {
	lease, err := v.Acquire(ctx, reference)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	return lease.Value(), nil
}

// Set rotates or creates a credential. The previous entry is revoked at the
// in-memory boundary and is wiped as soon as its outstanding leases drain.
func (v *Vault) Set(ctx context.Context, reference, value string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ErrNotFound
	}
	if value == "" {
		return ErrEmptyValue
	}
	newValue := []byte(value)
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		wipe(newValue)
		return ErrClosed
	}
	generation := v.generations[reference] + 1
	if generation == 0 {
		generation = 1
	}
	v.generations[reference] = generation
	v.mu.Unlock()

	if v.backend != nil {
		record := store.CredentialRecord{
			Reference:  reference,
			Value:      append([]byte(nil), newValue...),
			Generation: generation,
			UpdatedAt:  time.Now().UTC(),
		}
		if err := v.backend.PutCredentialRecord(ctx, record); err != nil {
			wipe(record.Value)
			wipe(newValue)
			return fmt.Errorf("credential: persist %s: %w", reference, err)
		}
		wipe(record.Value)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		wipe(newValue)
		return ErrClosed
	}
	if current := v.entries[reference]; current != nil {
		current.revoked = true
		if current.leases == 0 {
			wipe(current.value)
			delete(v.entries, reference)
		}
	}
	v.entries[reference] = &entry{value: newValue, generation: generation}
	return nil
}

// refresh makes an operation observe rotations and revocations committed by a
// different process. It intentionally reads only one reference and is called
// at the lease boundary, keeping the steady-state provider path bounded while
// preserving cross-process lifecycle semantics.
func (v *Vault) refresh(ctx context.Context, reference string) error {
	if v == nil || v.backend == nil {
		return nil
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	record, err := v.backend.GetCredentialRecord(ctx, reference)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return ErrClosed
	}
	current := v.entries[reference]
	if err == nil && current != nil && record.Generation < current.generation {
		// A local Set has already published a newer generation; do not let a
		// concurrent backend read install an older value over it.
		return nil
	}
	if err != nil {
		if !errors.Is(err, store.ErrCredentialNotFound) {
			return fmt.Errorf("credential: refresh %s: %w", reference, err)
		}
		if current != nil {
			current.revoked = true
			if current.leases == 0 {
				wipe(current.value)
				delete(v.entries, reference)
			}
		}
		return nil
	}
	if record.Revoked || len(record.Value) == 0 {
		if current != nil {
			current.revoked = true
			if current.leases == 0 {
				wipe(current.value)
				delete(v.entries, reference)
			}
		}
		if record.Generation > v.generations[reference] {
			v.generations[reference] = record.Generation
		}
		return nil
	}
	if record.Generation > v.generations[reference] {
		v.generations[reference] = record.Generation
	}
	if current != nil && current.generation == record.Generation && bytes.Equal(current.value, record.Value) && !current.revoked {
		return nil
	}
	if current != nil {
		current.revoked = true
		if current.leases == 0 {
			wipe(current.value)
			delete(v.entries, reference)
		}
	}
	v.entries[reference] = &entry{
		value:      append([]byte(nil), record.Value...),
		generation: record.Generation,
	}
	return nil
}

// Unset revokes and deletes a credential. An in-flight lease retains only its
// private value until Release, after which the value is wiped.
func (v *Vault) Unset(ctx context.Context, reference string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ErrNotFound
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.backend != nil {
		if err := v.backend.DeleteCredentialRecord(ctx, reference); err != nil {
			return fmt.Errorf("credential: delete %s: %w", reference, err)
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return ErrClosed
	}
	record, ok := v.entries[reference]
	if !ok {
		return nil
	}
	record.revoked = true
	if record.leases == 0 {
		wipe(record.value)
		delete(v.entries, reference)
	}
	return nil
}

// Has reports whether a new operation can acquire reference.
func (v *Vault) Has(reference string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	record := v.entries[strings.TrimSpace(reference)]
	return !v.closed && record != nil && !record.revoked && len(record.value) > 0
}

// References returns active references only; it never returns secret values.
func (v *Vault) References() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	refs := make([]string, 0, len(v.entries))
	for reference, record := range v.entries {
		if !record.revoked && len(record.value) > 0 {
			refs = append(refs, reference)
		}
	}
	sort.Strings(refs)
	return refs
}

// Close revokes and wipes every process-side value. Durable records are not
// deleted: restart must be able to recover credentials through the backend,
// while an OS keyring/KMS backend can enforce its own process/session policy.
func (v *Vault) Close() error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	for reference, record := range v.entries {
		record.revoked = true
		wipe(record.value)
		delete(v.entries, reference)
	}
	return nil
}

// Lease is a provider-operation credential lease.
type Lease struct {
	vault     *Vault
	reference string
	record    *entry
	once      sync.Once
}

// Value returns a string copy for an HTTP header or provider adapter. The
// caller must not retain it beyond the operation and must call Release.
func (l *Lease) Value() string {
	if l == nil || l.vault == nil || l.record == nil {
		return ""
	}
	l.vault.mu.Lock()
	defer l.vault.mu.Unlock()
	// A revoked entry remains readable only through leases that were acquired
	// before revocation. New Acquire calls reject it, and Release wipes it once
	// the in-flight operation drains.
	if len(l.record.value) == 0 {
		return ""
	}
	return string(l.record.value)
}

// Release drains the lease and wipes a revoked value when no operation still
// owns it. It is idempotent.
func (l *Lease) Release() {
	if l == nil || l.vault == nil || l.record == nil {
		return
	}
	l.once.Do(func() {
		l.vault.mu.Lock()
		defer l.vault.mu.Unlock()
		if l.record.leases > 0 {
			l.record.leases--
		}
		if l.record.revoked && l.record.leases == 0 {
			wipe(l.record.value)
			if current := l.vault.entries[l.reference]; current == l.record {
				delete(l.vault.entries, l.reference)
			}
		}
	})
}

func wipe(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
