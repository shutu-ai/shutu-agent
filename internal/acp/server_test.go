package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

type testFactory struct {
	mu      sync.Mutex
	session *testSession
	ready   chan struct{}
	done    chan struct{}
}

func (f *testFactory) NewSession(_ context.Context, _ string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.session = &testSession{done: f.done}
	if f.ready != nil {
		close(f.ready)
		f.ready = nil
	}
	return f.session, nil
}

type testSession struct {
	mu        sync.Mutex
	prompt    string
	cancelled bool
	done      chan struct{}
}

func (s *testSession) Prompt(ctx context.Context, text string, emit func(Update)) (StopReason, error) {
	s.mu.Lock()
	s.prompt = text
	cancelled := s.cancelled
	s.mu.Unlock()
	if cancelled || ctx.Err() != nil {
		return StopCancelled, nil
	}
	emit(Update{Text: "answer"})
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	return StopEndTurn, nil
}

func (s *testSession) Cancel() error {
	s.mu.Lock()
	s.cancelled = true
	s.mu.Unlock()
	return nil
}

func (s *testSession) Close() error { return nil }

type stagedReader struct {
	first  string
	rest   string
	ready  <-chan struct{}
	finish <-chan struct{}
	done   bool
}

func (r *stagedReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.first), nil
	}
	<-r.ready
	if r.rest == "" {
		if r.finish != nil {
			<-r.finish
		}
		return 0, io.EOF
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func TestServerProtocol(t *testing.T) {
	first := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"C:\\work"}}`,
	}, "\n") + "\n"
	rest := strings.Join([]string{
		`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"shutu-1","prompt":[{"type":"text","text":"hello"}]}}`,
		`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"shutu-1","prompt":[{"type":"image","data":"x"}]}}`,
		"",
	}, "\n")
	factory := &testFactory{}
	factory.ready = make(chan struct{})
	factory.done = make(chan struct{})
	var output bytes.Buffer
	server := &Server{Factory: factory, In: &stagedReader{first: first, rest: rest, ready: factory.ready, finish: factory.done}, Out: &output}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("invalid output %q: %v", line, err)
		}
		messages = append(messages, message)
	}
	if len(messages) != 5 {
		t.Fatalf("got %d output messages, want 5: %s", len(messages), output.String())
	}
	if factory.session == nil || factory.session.prompt != "hello" {
		t.Fatalf("prompt was not delivered: %#v", factory.session)
	}
	joined := output.String()
	if !strings.Contains(joined, `"protocolVersion":1`) || !strings.Contains(joined, `"sessionId":"shutu-1"`) || !strings.Contains(joined, `"text":"answer"`) {
		t.Fatalf("missing expected ACP response/update: %s", joined)
	}
	if !strings.Contains(joined, `"code":-32602`) {
		t.Fatalf("unsupported image should be invalid params: %s", joined)
	}
}

func TestServerRejectsUnknownSession(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"missing","prompt":[{"type":"text","text":"hello"}]}}` + "\n"
	var output bytes.Buffer
	if err := (&Server{Factory: &testFactory{}, In: strings.NewReader(input), Out: &output}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(output.String(), `"code":-32602`) {
		t.Fatalf("unknown session should be invalid params: %s", output.String())
	}
}
