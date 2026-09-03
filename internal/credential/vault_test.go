package credential

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/store"
)

type memoryBackend struct {
	mu      sync.Mutex
	values  map[string]store.CredentialRecord
	deleted []string
	failPut error
}

func (b *memoryBackend) ListCredentialRecords(context.Context) ([]store.CredentialRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]store.CredentialRecord, 0, len(b.values))
	for _, record := range b.values {
		record.Value = append([]byte(nil), record.Value...)
		out = append(out, record)
	}
	return out, nil
}

func (b *memoryBackend) GetCredentialRecord(_ context.Context, reference string) (store.CredentialRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.values[reference]
	if !ok {
		return store.CredentialRecord{}, store.ErrCredentialNotFound
	}
	record.Value = append([]byte(nil), record.Value...)
	return record, nil
}

func (b *memoryBackend) PutCredentialRecord(_ context.Context, record store.CredentialRecord) error {
	if b.failPut != nil {
		return b.failPut
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.values == nil {
		b.values = make(map[string]store.CredentialRecord)
	}
	record.Value = append([]byte(nil), record.Value...)
	b.values[record.Reference] = record
	return nil
}

func (b *memoryBackend) DeleteCredentialRecord(_ context.Context, reference string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.values, reference)
	b.deleted = append(b.deleted, reference)
	return nil
}

func (b *memoryBackend) generationOf(reference string) (store.CredentialRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.values[reference]
	return record, ok
}

// TestVaultBackendFailureDoesNotPublishOrReuseGeneration injects persistence
// failure after the caller supplied a new secret. The failed generation is
// consumed, the old credential remains leaseable, and only the later successful
// rotation is visible to a restarted Vault.
func TestVaultBackendFailureDoesNotPublishOrReuseGeneration(t *testing.T) {
	backend := &memoryBackend{}
	vault, err := New(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(context.Background(), "OPENAI_API_KEY", "generation-one"); err != nil {
		t.Fatal(err)
	}
	old, err := vault.Acquire(context.Background(), "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}

	backend.failPut = errors.New("injected credential disk failure")
	if err := vault.Set(context.Background(), "OPENAI_API_KEY", "must-not-publish"); err == nil {
		t.Fatal("rotation with failed backend unexpectedly succeeded")
	}
	record, ok := backend.generationOf("OPENAI_API_KEY")
	if !ok || record.Generation != 1 || string(record.Value) != "generation-one" {
		t.Fatalf("backend after failed rotation = %+v, ok=%v; want generation one unchanged", record, ok)
	}
	if got := old.Value(); got != "generation-one" {
		t.Fatalf("old lease after failed rotation = %q", got)
	}
	if got, err := vault.Resolve(context.Background(), "OPENAI_API_KEY"); err != nil || got != "generation-one" {
		t.Fatalf("active credential after failed rotation = %q, %v", got, err)
	}

	backend.failPut = nil
	if err := vault.Set(context.Background(), "OPENAI_API_KEY", "generation-three"); err != nil {
		t.Fatal(err)
	}
	if got := old.Value(); got != "generation-one" {
		t.Fatalf("old lease after successful rotation = %q", got)
	}
	if got, err := vault.Resolve(context.Background(), "OPENAI_API_KEY"); err != nil || got != "generation-three" {
		t.Fatalf("active credential after recovery = %q, %v", got, err)
	}

	restarted, err := New(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.Resolve(context.Background(), "OPENAI_API_KEY"); err != nil || got != "generation-three" {
		t.Fatalf("restarted credential = %q, %v; want committed generation three", got, err)
	}
	old.Release()
}

func TestVaultRotationWaitsForInFlightLeaseAndWipesOldValue(t *testing.T) {
	backend := &memoryBackend{}
	vault, err := New(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(context.Background(), "OPENAI_API_KEY", "old-secret"); err != nil {
		t.Fatal(err)
	}
	old, err := vault.Acquire(context.Background(), "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if got := old.Value(); got != "old-secret" {
		t.Fatalf("old lease value = %q", got)
	}
	if err := vault.Set(context.Background(), "OPENAI_API_KEY", "new-secret"); err != nil {
		t.Fatal(err)
	}
	if got := old.Value(); got != "old-secret" {
		t.Fatalf("in-flight old lease lost value %q", got)
	}
	// The lease remains counted until release, so the old entry can be safely
	// drained without allowing a new operation to acquire it.
	newLease, err := vault.Acquire(context.Background(), "OPENAI_API_KEY")
	if err != nil {
		t.Fatalf("new generation should be acquirable: %v", err)
	}
	newLease.Release()
	old.Release()
	old.Release()
	if !vault.Has("OPENAI_API_KEY") {
		t.Fatal("rotated credential is not available")
	}
}

func TestVaultUnsetAndCloseRevokeFutureAcquires(t *testing.T) {
	vault, err := New(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(context.Background(), "REF", "secret"); err != nil {
		t.Fatal(err)
	}
	lease, err := vault.Acquire(context.Background(), "REF")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Unset(context.Background(), "REF"); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Acquire(context.Background(), "REF"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("acquire after unset = %v, want ErrRevoked", err)
	}
	lease.Release()
	if err := vault.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Acquire(context.Background(), "REF"); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close = %v, want ErrClosed", err)
	}
}

// TestCredentialLeaseDrainWipesRevokedBytes inspects the process-local buffer
// directly. Cancellation and disposal paths release leases; this proves the
// drain boundary actually zeroes the revoked value as far as Go permits.
func TestCredentialLeaseDrainWipesRevokedBytes(t *testing.T) {
	vault, err := New(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(context.Background(), "DRAIN_API_KEY", "hostile-drain-secret"); err != nil {
		t.Fatal(err)
	}
	lease, err := vault.Acquire(context.Background(), "DRAIN_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	record := vault.entries["DRAIN_API_KEY"]
	if record == nil || string(record.value) != "hostile-drain-secret" {
		t.Fatalf("in-flight credential buffer = %#v", record)
	}
	if err := vault.Unset(context.Background(), "DRAIN_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if string(record.value) != "hostile-drain-secret" {
		t.Fatal("revocation wiped an in-flight credential before drain")
	}
	lease.Release()
	if len(record.value) != len("hostile-drain-secret") {
		t.Fatalf("drained credential buffer length = %d, want %d", len(record.value), len("hostile-drain-secret"))
	}
	for _, value := range record.value {
		if value != 0 {
			t.Fatalf("drained credential buffer contains nonzero byte %d", value)
		}
	}
}

func TestVaultLoadsOnlyActiveReferences(t *testing.T) {
	backend := &memoryBackend{values: map[string]store.CredentialRecord{
		"ACTIVE":  {Reference: "ACTIVE", Value: []byte("secret"), Generation: 2},
		"REVOKED": {Reference: "REVOKED", Value: []byte("secret"), Revoked: true},
		"EMPTY":   {Reference: "EMPTY"},
	}}
	vault, err := New(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if got := vault.References(); len(got) != 1 || got[0] != "ACTIVE" {
		t.Fatalf("references = %#v, want [ACTIVE]", got)
	}
	if got, err := vault.Resolve(context.Background(), "ACTIVE"); err != nil || got != "secret" {
		t.Fatalf("resolve active = %q, %v", got, err)
	}
}

func TestVaultRefreshesRotationAndRevocationAcrossProcesses(t *testing.T) {
	backend := &memoryBackend{}
	first, err := New(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Set(context.Background(), "REF", "one"); err != nil {
		t.Fatal(err)
	}
	second, err := New(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Set(context.Background(), "REF", "two"); err != nil {
		t.Fatal(err)
	}
	if got, err := second.Resolve(context.Background(), "REF"); err != nil || got != "two" {
		t.Fatalf("cross-process resolve = %q, %v", got, err)
	}
	if err := first.Unset(context.Background(), "REF"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Acquire(context.Background(), "REF"); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrRevoked) {
		t.Fatalf("cross-process acquire after revoke = %v", err)
	}
}
