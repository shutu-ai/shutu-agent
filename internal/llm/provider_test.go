package llm

import (
	"context"
	"testing"
)

// fakeProvider is a minimal Provider for registry tests.
type fakeProvider struct {
	id        string
	available bool
}

type closeableFakeProvider struct {
	fakeProvider
	closed bool
}

func (f *closeableFakeProvider) Close() error {
	f.closed = true
	return nil
}

func (f *fakeProvider) ID() string      { return f.id }
func (f *fakeProvider) Available() bool { return f.available }
func (f *fakeProvider) Stream(context.Context, ChatRequest) (StreamReader, error) {
	return nil, nil
}

// TestRegistryRegisterGetList verifies the D2 三件套 basics: Register under a
// stable id, Get by id, and List in registration order (dispatch-m8-2 §2/§7).
func TestRegistryRegisterGetList(t *testing.T) {
	r := NewRegistry()
	a := &fakeProvider{id: "deepseek"}
	b := &fakeProvider{id: "openai"}
	if err := r.Register(a); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := r.Register(b); err != nil {
		t.Fatalf("register b: %v", err)
	}

	got, err := r.Get("openai")
	if err != nil {
		t.Fatalf("Get openai: %v", err)
	}
	if got != b {
		t.Fatalf("Get = %v, want the openai provider", got)
	}

	list := r.List()
	if len(list) != 2 || list[0] != a || list[1] != b {
		t.Fatalf("List = %v, want [deepseek openai] in registration order", list)
	}
}

// TestRegistryRegisterDuplicate verifies a duplicate id is rejected
// (dispatch-m8-2 §2).
func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeProvider{id: "deepseek"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(&fakeProvider{id: "deepseek"}); err == nil {
		t.Fatal("duplicate provider id must be rejected")
	}
}

// TestRegistryRegisterNilAndEmptyID verifies nil and empty-id providers are
// rejected at registration.
func TestRegistryRegisterNilAndEmptyID(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("nil provider must be rejected")
	}
	if err := r.Register(&fakeProvider{id: ""}); err == nil {
		t.Fatal("empty provider id must be rejected")
	}
}

// TestRegistryGetMissing verifies Get on an absent id returns an error
// (dispatch-m8-2 §2: 不存在报错) — the fail-closed path the composition root
// relies on for an unknown llm.provider.
func TestRegistryGetMissing(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("Get on an unregistered id must error")
	}
}

// TestRegistryListEmpty verifies an empty registry lists nothing without
// panicking.
func TestRegistryListEmpty(t *testing.T) {
	if got := NewRegistry().List(); len(got) != 0 {
		t.Fatalf("empty registry List = %v, want none", got)
	}
}

func TestRegistryCloseDisposesCloseableProviders(t *testing.T) {
	r := NewRegistry()
	provider := &closeableFakeProvider{fakeProvider: fakeProvider{id: "secret"}}
	if err := r.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !provider.closed {
		t.Fatal("registry Close did not dispose closeable provider")
	}
}
