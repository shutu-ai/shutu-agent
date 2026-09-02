package acpclient_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/acp"
	"github.com/jabing/shutu-agent/internal/acpclient"
)

type simpleSession struct{}

func (simpleSession) Prompt(_ context.Context, text string, emit func(acp.Update)) (acp.StopReason, error) {
	emit(acp.Update{Text: "echo:" + text})
	return acp.StopEndTurn, nil
}
func (simpleSession) Cancel() error { return nil }
func (simpleSession) Close() error  { return nil }

type simpleFactory struct{}

func (simpleFactory) NewSession(context.Context, string) (acp.Session, error) {
	return simpleSession{}, nil
}

func TestIndependentClientPromptRoundTrip(t *testing.T) {
	serverToClient, serverToClientWriter := io.Pipe()
	clientToServerReader, clientToServerWriter := io.Pipe()
	server := &acp.Server{Factory: simpleFactory{}, In: clientToServerReader, Out: serverToClientWriter}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(context.Background()) }()
	defer func() {
		_ = clientToServerReader.Close()
		if err := <-serverDone; err != nil && !strings.Contains(err.Error(), "read: EOF") && !strings.Contains(err.Error(), "read/write on closed pipe") {
			t.Fatalf("server: %v", err)
		}
	}()

	client := acpclient.New(serverToClient, clientToServerWriter)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	initialized, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initialized["protocolVersion"] != float64(acp.ProtocolVersion) {
		t.Fatalf("protocol = %#v", initialized["protocolVersion"])
	}
	sessionID, err := client.NewSession(ctx, `C:\work`)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	var updates []string
	stop, err := client.Prompt(ctx, sessionID, []acpclient.ContentBlock{{Type: "text", Text: "hello"}}, func(update acpclient.Update) {
		updates = append(updates, update.Content.Text)
	})
	if err != nil || stop != acpclient.StopEndTurn || len(updates) != 1 || updates[0] != "echo:hello" {
		t.Fatalf("Prompt stop=%v err=%v updates=%q", stop, err, updates)
	}
	_ = client.Close()
	_ = serverToClient.Close()
}
