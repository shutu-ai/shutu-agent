package lifecycle

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCoordinatorClosesInReverseRegistrationOrder(t *testing.T) {
	c := New()
	var got []string
	var mu sync.Mutex
	for _, name := range []string{"store", "jobs", "agents", "admission"} {
		name := name
		if err := c.Register(name, func() error {
			mu.Lock()
			got = append(got, name)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"admission", "agents", "jobs", "store"}) {
		t.Fatalf("close order = %v", got)
	}
	if err := c.Register("late", func() error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("late registration = %v, want ErrClosed", err)
	}
}

func TestCoordinatorConcurrentCloseSharesResultAndRunsOnce(t *testing.T) {
	c := New()
	want := errors.New("disk full")
	var calls int
	if err := c.Register("store", func() error { calls++; return want }); err != nil {
		t.Fatal(err)
	}
	const n = 16
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- c.Close() }()
	}
	wg.Wait()
	close(results)
	if calls != 1 {
		t.Fatalf("closer calls = %d, want 1", calls)
	}
	for err := range results {
		if err == nil || !errors.Is(err, want) {
			t.Fatalf("close result = %v, want wrapped disk-full error", err)
		}
	}
}

func TestCoordinatorCloserPanicDoesNotBreakQuiesceBarrier(t *testing.T) {
	c := New()
	var completed bool
	if err := c.Register("healthy", func() error {
		completed = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Register("panicking", func() error { panic("boom") }); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- c.Close() }()
	go func() { results <- c.Close() }()
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil || !strings.Contains(err.Error(), "panicking") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("close result = %v, want contained closer panic", err)
		}
	}
	if !completed || !c.Closed() {
		t.Fatalf("panic closer prevented remaining teardown: completed=%v closed=%v", completed, c.Closed())
	}
}
