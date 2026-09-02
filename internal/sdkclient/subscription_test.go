package sdkclient

import (
	"context"
	"errors"
	"testing"
)

func TestSubscriptionCloseDropsQueueButPreservesFirstFailure(t *testing.T) {
	failure := errors.New("runtime exited")
	state := &subscription{}
	state.push(Notification{Method: "session/update"})
	state.fail(failure)
	handle := &SubscriptionHandle{state: state}
	handle.Close()
	if _, ok := handle.TryNext(); ok {
		t.Fatal("closed subscription retained a queued notification")
	}
	if _, err := handle.Next(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("closed subscription error = %v, want original runtime failure", err)
	}
}
