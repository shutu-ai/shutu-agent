package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testFactory struct {
	mu        sync.Mutex
	session   *testSession
	ready     chan struct{}
	done      chan struct{}
	identity  string
	resumeErr error
}

func (f *testFactory) ResumeSession(_ context.Context, id string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	f.session = &testSession{prompt: "restored:" + id, done: f.done}
	return f.session, nil
}

func (f *testFactory) NewSession(_ context.Context, _ string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.session = &testSession{done: f.done, identity: f.identity}
	if f.ready != nil {
		close(f.ready)
		f.ready = nil
	}
	return f.session, nil
}

type testSession struct {
	mu          sync.Mutex
	prompt      string
	cancelled   bool
	done        chan struct{}
	identity    string
	closedCount int
}

type sessionGateReader struct {
	payload []byte
	offset  int
	gate    <-chan struct{}
	waited  bool
}

func (r *sessionGateReader) Read(p []byte) (int, error) {
	if r.offset < len(r.payload) {
		n := copy(p, r.payload[r.offset:])
		r.offset += n
		return n, nil
	}
	if !r.waited {
		r.waited = true
		<-r.gate
	}
	return 0, io.EOF
}

type sessionGateWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	gate chan struct{}
	once sync.Once
}

type permissionErrorReader struct {
	payload []byte
	gate    <-chan struct{}
	done    bool
}

func (r *permissionErrorReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.payload), nil
	}
	<-r.gate
	return 0, errors.New("scripted transport failure")
}

type permissionGateWriter struct {
	gate chan struct{}
	once sync.Once
}

func (w *permissionGateWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"method":"session/request_permission"`)) {
		w.once.Do(func() { close(w.gate) })
	}
	return len(p), nil
}

func (w *sessionGateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes.Contains(p, []byte(`"sessionId"`)) {
		w.once.Do(func() { close(w.gate) })
	}
	return w.buf.Write(p)
}

func TestServerClientEOFCancelsAndClosesEstablishedSessions(t *testing.T) {
	gate := make(chan struct{})
	payload := []byte(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"C:\\work"}}`,
	}, "\n") + "\n")
	reader := &sessionGateReader{payload: payload, gate: gate}
	writer := &sessionGateWriter{gate: gate}
	factory := &testFactory{}
	server := &Server{Factory: factory, In: reader, Out: writer}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	factory.mu.Lock()
	sess := factory.session
	factory.mu.Unlock()
	if sess == nil {
		t.Fatal("session/new did not establish a session before client EOF")
	}
	sess.mu.Lock()
	cancelled, closedCount := sess.cancelled, sess.closedCount
	sess.mu.Unlock()
	if !cancelled || closedCount != 1 {
		t.Fatalf("session teardown = cancelled:%v closed:%d, want cancelled:true closed:1", cancelled, closedCount)
	}
}

type permissionWaitFactory struct {
	session *permissionWaitSession
}

func (f *permissionWaitFactory) NewSession(context.Context, string) (Session, error) {
	f.session = &permissionWaitSession{entered: make(chan struct{}), result: make(chan error, 1)}
	return f.session, nil
}

func (*permissionWaitFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{}
}

type permissionWaitSession struct {
	mu        sync.Mutex
	requester PermissionRequester
	entered   chan struct{}
	result    chan error
}

func (s *permissionWaitSession) SetPermissionRequester(requester PermissionRequester) {
	s.mu.Lock()
	s.requester = requester
	s.mu.Unlock()
}

func (s *permissionWaitSession) Prompt(context.Context, string, func(Update)) (StopReason, error) {
	s.mu.Lock()
	requester := s.requester
	s.mu.Unlock()
	close(s.entered)
	_, err := requester.RequestPermission(context.Background(), PermissionRequest{
		ToolCallID: "call-1",
		ToolName:   "shell",
	})
	s.result <- err
	return StopCancelled, nil
}

func (*permissionWaitSession) Cancel() error { return nil }
func (*permissionWaitSession) Close() error  { return nil }

func TestServerScannerErrorCancelsPendingPermissionBeforeWaiting(t *testing.T) {
	permissionWritten := make(chan struct{})
	reader := &permissionErrorReader{
		payload: []byte(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"C:\\work"}}`,
			`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"shutu-1","prompt":[{"type":"text","text":"ask"}]}}`,
		}, "\n") + "\n"),
		gate: permissionWritten,
	}
	factory := &permissionWaitFactory{}
	server := &Server{Factory: factory, In: reader, Out: &permissionGateWriter{gate: permissionWritten}}
	done := make(chan error, 1)
	go func() { done <- server.Run(context.Background()) }()
	select {
	case <-permissionWritten:
	case <-time.After(time.Second):
		t.Fatal("permission request was not sent before transport failure")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "scripted transport failure") {
			t.Fatalf("Run error = %v, want scanner transport failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run waited forever on a pending permission request")
	}
	select {
	case err := <-factory.session.result:
		if err == nil || !strings.Contains(err.Error(), "ACP transport failed") {
			t.Fatalf("permission result = %v, want transport cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("permission request did not settle")
	}
}

func (s *testSession) SessionID() string { return s.identity }

func (*testSession) ResumeMetadata() map[string]any {
	return map[string]any{"durable": true, "eventCursor": 0, "nextTurn": 1}
}

type richTestFactory struct{ session *richTestSession }

func (f *richTestFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": true}
}

func (f *richTestFactory) NewSession(context.Context, string) (Session, error) {
	f.session = &richTestSession{}
	return f.session, nil
}

type richTestSession struct {
	blocks []PromptContentBlock
}

func (s *richTestSession) Prompt(context.Context, string, func(Update)) (StopReason, error) {
	return StopEndTurn, nil
}

func (s *richTestSession) PromptContent(_ context.Context, blocks []PromptContentBlock, _ func(Update)) (StopReason, error) {
	s.blocks = append([]PromptContentBlock(nil), blocks...)
	return StopEndTurn, nil
}

func (s *richTestSession) Cancel() error { return nil }
func (s *richTestSession) Close() error  { return nil }

type disabledRichFactory struct{ session *richTestSession }

func (f *disabledRichFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": false}
}

func (f *disabledRichFactory) NewSession(context.Context, string) (Session, error) {
	f.session = &richTestSession{}
	return f.session, nil
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

func (s *testSession) Close() error {
	s.mu.Lock()
	s.closedCount++
	s.mu.Unlock()
	return nil
}

func TestServerSessionResumeAndReconnectUseDurableFactory(t *testing.T) {
	factory := &testFactory{}
	server := &Server{Factory: factory, sessions: map[string]Session{}}
	result, rpcErr, notify := server.dispatch(context.Background(), "session/resume", json.RawMessage(`{"sessionId":"persisted-1"}`))
	if rpcErr != nil || notify {
		t.Fatalf("session/resume = result=%#v err=%v notify=%v", result, rpcErr, notify)
	}
	if got := result.(map[string]any)["sessionId"]; got != "persisted-1" {
		t.Fatalf("resumed session id = %q", got)
	}
	if _, ok := server.session("persisted-1"); !ok {
		t.Fatal("resumed session was not published")
	}
	if _, rpcErr, _ = server.dispatch(context.Background(), "session/reconnect", json.RawMessage(`{"sessionId":"persisted-1"}`)); rpcErr != nil {
		t.Fatalf("session/reconnect = %v", rpcErr)
	}
}

func TestServerSessionResumeExposesDurableMetadata(t *testing.T) {
	factory := &testFactory{}
	server := &Server{Factory: factory, sessions: make(map[string]Session)}
	result, rpcErr, notify := server.dispatch(context.Background(), "session/resume", json.RawMessage(`{"sessionId":"persisted-1"}`))
	if rpcErr != nil || notify {
		t.Fatalf("resume = result=%#v err=%v notify=%v", result, rpcErr, notify)
	}
	values, ok := result.(map[string]any)["metadata"].(map[string]any)
	if !ok || values["durable"] != true || values["eventCursor"] != 0 || values["nextTurn"] != 1 {
		t.Fatalf("resume metadata = %#v", result)
	}
}

func TestServerSessionResumeNotFoundIsInvalidParams(t *testing.T) {
	factory := &testFactory{resumeErr: fmt.Errorf("%w: persisted-1", ErrSessionNotFound)}
	server := &Server{Factory: factory, sessions: make(map[string]Session)}
	_, rpcErr, _ := server.dispatch(context.Background(), "session/resume", json.RawMessage(`{"sessionId":"persisted-1"}`))
	if rpcErr == nil || rpcErr.Code != -32602 || !strings.Contains(rpcErr.Message, "not found") {
		t.Fatalf("not-found error = %#v, want invalid params", rpcErr)
	}
}

func TestServerSessionResumeDoesNotReplaceActivePrompt(t *testing.T) {
	old := &testSession{}
	factory := &testFactory{}
	server := &Server{
		Factory:      factory,
		sessions:     map[string]Session{"persisted-1": old},
		promptActive: map[string]bool{"persisted-1": true},
	}
	result, rpcErr, _ := server.dispatch(context.Background(), "session/resume", json.RawMessage(`{"sessionId":"persisted-1"}`))
	if result != nil || rpcErr == nil || rpcErr.Code != -32000 || rpcErr.Message != ErrPromptInFlight.Error() {
		t.Fatalf("active-prompt resume = result=%#v err=%#v", result, rpcErr)
	}
	if got, _ := server.session("persisted-1"); got != old {
		t.Fatal("active runtime was replaced")
	}
	if factory.session == nil || factory.session.closedCount != 1 {
		t.Fatalf("replacement runtime = %#v, want closed once", factory.session)
	}
}

func TestServerSessionNewUsesDurableIdentityWhenAvailable(t *testing.T) {
	factory := &testFactory{identity: "durable-session-7"}
	server := &Server{Factory: factory, sessions: make(map[string]Session)}
	result, rpcErr, notify := server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"C:\\work"}`))
	if rpcErr != nil || notify {
		t.Fatalf("session/new = result=%#v err=%v notify=%v", result, rpcErr, notify)
	}
	if got := result.(map[string]any)["sessionId"]; got != "durable-session-7" {
		t.Fatalf("session id = %q, want durable identity", got)
	}
	if _, ok := server.session("durable-session-7"); !ok {
		t.Fatal("durable session was not published under its identity")
	}
}

type catalogTestFactory struct {
	*testFactory
	catalog    ToolCatalog
	catalogErr error
}

func (f *catalogTestFactory) ToolCatalog(context.Context) (ToolCatalog, error) {
	return f.catalog, f.catalogErr
}

func TestServerExposesToolCatalogRevisionAndRejectsProviderError(t *testing.T) {
	catalog := ToolCatalog{
		SchemaVersion: 1, Revision: 7, Digest: "digest-7",
		Tools: []ToolCatalogEntry{{Name: "read", Profile: "standard", Provenance: "builtin", Generation: 7, Visible: true}},
	}
	factory := &catalogTestFactory{testFactory: &testFactory{}, catalog: catalog}
	server := &Server{Factory: factory, sessions: make(map[string]Session)}

	result, rpcErr, notify := server.dispatch(context.Background(), "initialize", nil)
	if rpcErr != nil || notify {
		t.Fatalf("initialize = result=%#v err=%v notify=%v", result, rpcErr, notify)
	}
	if got := result.(map[string]any)["toolCatalog"]; !reflect.DeepEqual(got, &catalog) {
		t.Fatalf("initialize catalog = %#v, want %#v", got, catalog)
	}

	result, rpcErr, notify = server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"C:\\work"}`))
	if rpcErr != nil || notify {
		t.Fatalf("session/new = result=%#v err=%v notify=%v", result, rpcErr, notify)
	}
	if got := result.(map[string]any)["toolCatalog"]; !reflect.DeepEqual(got, &catalog) {
		t.Fatalf("session/new catalog = %#v, want %#v", got, catalog)
	}

	// A later inventory provider failure must fail reconnection, not silently
	// create a session whose advertised tools are unknown.
	factory.catalogErr = errors.New("registry unavailable")
	if _, rpcErr, _ = server.dispatch(context.Background(), "session/reconnect", json.RawMessage(`{"sessionId":"persisted-1"}`)); rpcErr == nil || rpcErr.Code != -32603 {
		t.Fatalf("reconnect catalog error = %#v, want internal error", rpcErr)
	}
}

func TestServerRejectsInvalidCatalogContract(t *testing.T) {
	for _, test := range []struct {
		name    string
		catalog ToolCatalog
	}{
		{name: "schema", catalog: ToolCatalog{SchemaVersion: 2, Digest: "digest"}},
		{name: "digest", catalog: ToolCatalog{SchemaVersion: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{Factory: &catalogTestFactory{
				testFactory: &testFactory{}, catalog: test.catalog,
			}, sessions: make(map[string]Session)}
			if _, rpcErr, _ := server.dispatch(context.Background(), "initialize", nil); rpcErr == nil || rpcErr.Code != -32603 {
				t.Fatalf("error = %#v, want internal error", rpcErr)
			}
		})
	}
}

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
			// Normally the first prompt closes this channel. Keep a defensive
			// bound so a handler scheduling race cannot deadlock the suite; the
			// assertions below still reject missing prompt output.
			select {
			case <-r.finish:
			case <-time.After(5 * time.Second):
			}
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

func TestServerAdvertisesAndDispatchesRichPrompt(t *testing.T) {
	factory := &richTestFactory{}
	server := &Server{Factory: factory, sessions: make(map[string]Session)}
	initResult, rpcErr, _ := server.dispatch(context.Background(), "initialize", nil)
	if rpcErr != nil || !initResult.(map[string]any)["agentCapabilities"].(map[string]any)["promptCapabilities"].(map[string]bool)["image"] {
		t.Fatalf("initialize did not advertise image capability: %#v, %v", initResult, rpcErr)
	}
	if _, rpcErr, _ = server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"C:\\work"}`)); rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	_, rpcErr, _ = server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"text","text":"look"},{"type":"image","data":"aGk=","mimeType":"image/png"}]}`))
	if rpcErr != nil {
		t.Fatalf("session/prompt: %v", rpcErr)
	}
	if factory.session == nil || len(factory.session.blocks) != 2 || factory.session.blocks[1].MimeType != "image/png" {
		t.Fatalf("rich prompt was not delivered: %#v", factory.session)
	}
}

func TestServerRejectsRichPromptWhenImageCapabilityIsDisabled(t *testing.T) {
	factory := &disabledRichFactory{}
	server := &Server{Factory: factory, sessions: make(map[string]Session)}
	if _, rpcErr, _ := server.dispatch(context.Background(), "initialize", nil); rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	if _, rpcErr, _ := server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"C:\\work"}`)); rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	if _, rpcErr, _ := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"image","data":"aGk=","mimeType":"image/png"}]}`)); rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("image prompt with disabled capability = %v, want invalid params", rpcErr)
	}
	if factory.session == nil || len(factory.session.blocks) != 0 {
		t.Fatalf("disabled image was delivered to the rich session: %#v", factory.session)
	}
}

func TestServerRejectsRichPromptWhenImageCapabilityIsUndeclared(t *testing.T) {
	server := &Server{Factory: &testFactory{}, Out: &bytes.Buffer{}, sessions: map[string]Session{"shutu-1": &richTestSession{}}}
	if _, rpcErr, _ := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"image","data":"aGk=","mimeType":"image/png"}]}`)); rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("image prompt without declared capability = %v, want invalid params", rpcErr)
	}
}

func TestServerRejectsWhitespaceOnlyPrompt(t *testing.T) {
	server := &Server{Factory: &testFactory{}, Out: &bytes.Buffer{}, sessions: map[string]Session{"shutu-1": &testSession{}}}
	if _, rpcErr, _ := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"text","text":"  \t\n"}]}`)); rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("whitespace-only prompt = %v, want invalid params", rpcErr)
	}
}

func TestServerRejectsRichPromptWithoutExtension(t *testing.T) {
	factory := &testFactory{}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"C:\\work"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"shutu-1","prompt":[{"type":"image","data":"aGk=","mimeType":"image/png"}]}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := (&Server{Factory: factory, In: strings.NewReader(input), Out: &output}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(output.String(), `"code":-32602`) {
		t.Fatalf("rich prompt should fail for text-only session: %s", output.String())
	}
}

func TestServerRejectsUnsupportedSessionOptionsAndContentBlocks(t *testing.T) {
	server := &Server{Factory: &testFactory{}, Out: &bytes.Buffer{}, sessions: make(map[string]Session)}
	if _, err, _ := server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"relative/work"}`)); err == nil || err.Code != -32602 {
		t.Fatalf("relative cwd error = %+v, want invalid params", err)
	}
	if _, err, _ := server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"C:\\work","additionalDirectories":["C:\\other"]}`)); err == nil || err.Code != -32602 {
		t.Fatalf("additionalDirectories error = %+v, want invalid params", err)
	}
	if _, err, _ := server.dispatch(context.Background(), "session/new", json.RawMessage(`{"cwd":"C:\\work","mcpServers":[{}]}`)); err == nil || err.Code != -32602 {
		t.Fatalf("mcpServers error = %+v, want invalid params", err)
	}
	server.sessions["s"] = &testSession{}
	for _, typ := range []string{"audio", "embeddedContext", "unknown"} {
		raw := fmt.Sprintf(`{"sessionId":"s","prompt":[{"type":%q,"data":"x"}]}`, typ)
		if _, err, _ := server.dispatch(context.Background(), "session/prompt", json.RawMessage(raw)); err == nil || err.Code != -32602 {
			t.Fatalf("content type %q error = %+v, want invalid params", typ, err)
		}
	}
}

type failingACPWriter struct{}

func (failingACPWriter) Write([]byte) (int, error) { return 0, errors.New("client pipe closed") }

func TestServerContainsSessionUpdateWriteFailure(t *testing.T) {
	sess := &testSession{}
	server := &Server{Factory: &testFactory{}, Out: failingACPWriter{}, sessions: map[string]Session{"shutu-1": sess}}
	_, rpcErr, notify := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"text","text":"hello"}]}`))
	if notify || rpcErr == nil || rpcErr.Code != -32603 || !strings.Contains(rpcErr.Message, "internal error") {
		t.Fatalf("prompt update write failure = err=%v notify=%v, want internal delivery failure", rpcErr, notify)
	}
}

type shortACPWriter struct{}

func (shortACPWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestServerRejectsShortProtocolWrites(t *testing.T) {
	server := &Server{Factory: &testFactory{}, Out: shortACPWriter{}}
	if err := server.write(map[string]string{"ok": "no"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short protocol write = %v, want io.ErrShortWrite", err)
	}
}

type failingPromptSession struct{}

func (failingPromptSession) Prompt(_ context.Context, _ string, emit func(Update)) (StopReason, error) {
	emit(Update{Text: "partial"})
	return StopReason("failed"), errors.New("model failed")
}
func (failingPromptSession) Cancel() error { return nil }
func (failingPromptSession) Close() error  { return nil }

func TestServerPrioritizesOutputDeliveryFailureOverPromptFailure(t *testing.T) {
	server := &Server{Factory: &testFactory{}, Out: failingACPWriter{}, sessions: map[string]Session{
		"shutu-1": failingPromptSession{},
	}}
	_, rpcErr, notify := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"text","text":"hello"}]}`))
	if notify || rpcErr == nil || !strings.Contains(fmt.Sprint(rpcErr.Data), "session/update delivery failed") {
		t.Fatalf("settlement error = %#v notify=%v, want output delivery failure", rpcErr, notify)
	}
}

type singleFlightSession struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
	max     atomic.Int32
}

func (s *singleFlightSession) Prompt(context.Context, string, func(Update)) (StopReason, error) {
	active := s.active.Add(1)
	for {
		old := s.max.Load()
		if active <= old || s.max.CompareAndSwap(old, active) {
			break
		}
	}
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	s.active.Add(-1)
	return StopEndTurn, nil
}
func (s *singleFlightSession) Cancel() error { return nil }
func (s *singleFlightSession) Close() error  { return nil }

func TestServerRejectsConcurrentPromptsPerSession(t *testing.T) {
	sess := &singleFlightSession{entered: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	server := &Server{Factory: &testFactory{}, Out: &bytes.Buffer{}, sessions: map[string]Session{"s": sess}}
	results := make(chan *rpcError, 2)
	call := func() {
		_, err, _ := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"s","prompt":[{"type":"text","text":"x"}]}`))
		results <- err
	}
	go call()
	select {
	case <-sess.entered:
		// The first prompt owns the session boundary.
	case <-time.After(time.Second):
		t.Fatal("first prompt did not start")
	}
	go call()
	select {
	case err := <-results:
		if err == nil || err.Code != -32602 {
			t.Fatalf("concurrent prompt error = %v, want invalid params", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent prompt was queued instead of rejected")
	}
	// Release only the first invocation. The second call must have been
	// rejected at the protocol boundary and must never enter the session.
	sess.release <- struct{}{}
	if got := sess.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent prompts = %d, want 1", got)
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("first prompt error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first prompt did not complete")
	}
}

func TestServerProjectsResourceLinkIntoPromptText(t *testing.T) {
	sess := &testSession{}
	server := &Server{Factory: &testFactory{}, Out: &bytes.Buffer{}, sessions: map[string]Session{"shutu-1": sess}}
	_, rpcErr, _ := server.dispatch(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":"shutu-1","prompt":[{"type":"text","text":"inspect "},{"type":"resource_link","name":"source","uri":"file:///tmp/a.go"}]}`))
	if rpcErr != nil {
		t.Fatalf("resource link prompt: %v", rpcErr)
	}
	if !strings.Contains(sess.prompt, "inspect ") || !strings.Contains(sess.prompt, `[resource_link name="source" uri="file:///tmp/a.go"]`) {
		t.Fatalf("projected prompt = %q", sess.prompt)
	}
}

func TestPermissionRequesterRoundTrip(t *testing.T) {
	var output lockedBuffer
	server := &Server{Out: &output, permissions: make(map[string]chan permissionReply)}
	requester := permissionRequesterFunc{server: server, sessionID: "shutu-1"}
	done := make(chan struct{})
	var got PermissionOutcome
	var gotErr error
	go func() {
		got, gotErr = requester.RequestPermission(context.Background(), PermissionRequest{
			ToolCallID: "call-7", ToolName: "terminal", Reason: "needs a shell", Options: []PermissionOption{{ID: "allow-once", Label: "Allow once"}, {ID: "reject", Label: "Reject"}},
		})
		close(done)
	}()
	for i := 0; i < 100 && output.Len() == 0; i++ {
		// The requester writes synchronously; the short yield only avoids a
		// race between the goroutine and the assertion below.
		time.Sleep(time.Millisecond)
	}
	var wire struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			ToolCall  struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"toolCall"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &wire); err != nil {
		t.Fatalf("permission request JSON: %v (%s)", err, output.String())
	}
	if wire.Method != "session/request_permission" || wire.Params.SessionID != "shutu-1" || wire.Params.ToolCall.ToolCallID != "call-7" {
		t.Fatalf("permission request = %+v", wire)
	}
	response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"outcome":"allowed-once","optionId":"allow-once"}}`, wire.ID))
	if !server.resolvePermissionResponse(json.RawMessage(fmt.Sprintf(`%q`, wire.ID)), response) {
		t.Fatal("permission response was not routed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("permission requester did not return")
	}
	if gotErr != nil || got.Outcome != "allowed-once" || got.OptionID != "allow-once" {
		t.Fatalf("permission outcome = %+v, err=%v", got, gotErr)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPermissionResponseAfterCancellationIsDiscarded(t *testing.T) {
	var output bytes.Buffer
	server := &Server{Out: &output, permissions: make(map[string]chan permissionReply)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := server.requestPermission(ctx, PermissionRequest{SessionID: "s", ToolCallID: "c", ToolName: "shell"})
		done <- err
	}()
	for i := 0; i < 100 && output.Len() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	var wire struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &wire); err != nil || wire.ID == "" {
		t.Fatalf("permission request = %q, err=%v", output.String(), err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled permission did not return")
	}
	if server.resolvePermissionResponse(json.RawMessage(fmt.Sprintf(`%q`, wire.ID)), []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"outcome":"allowed-once"}}`, wire.ID))) {
		t.Fatal("late permission response was accepted")
	}
}
