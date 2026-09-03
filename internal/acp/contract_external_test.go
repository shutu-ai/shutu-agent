package acp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/acp"
	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
)

const externalACPProcessHelperEnv = "SHUTU_ACP_EXTERNAL_PROCESS_HELPER"

// TestExternalACPProcessHelper is a real child-process ACP peer. The parent
// test below owns the wire and deliberately closes stdin after session/new;
// the helper's stderr markers make session cancellation/close observable
// without coupling the contract to acp.Server's private fields.
func TestExternalACPProcessHelper(t *testing.T) {
	if os.Getenv(externalACPProcessHelperEnv) != "1" {
		t.Skip("external process helper")
	}
	server := &acp.Server{
		Factory: externalProcessFactory{},
		In:      os.Stdin,
		Out:     os.Stdout,
	}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type externalProcessFactory struct{}

func (externalProcessFactory) NewSession(context.Context, string) (acp.Session, error) {
	return externalProcessSession{}, nil
}

type externalProcessSession struct{}

func (externalProcessSession) SessionID() string { return "external-process-session" }
func (externalProcessSession) Prompt(context.Context, string, func(acp.Update)) (acp.StopReason, error) {
	return acp.StopEndTurn, nil
}
func (externalProcessSession) Cancel() error {
	_, _ = fmt.Fprintln(os.Stderr, "ACP_EXTERNAL_CANCELLED")
	return nil
}
func (externalProcessSession) Close() error {
	_, _ = fmt.Fprintln(os.Stderr, "ACP_EXTERNAL_CLOSED")
	return nil
}

func TestExternalACPProcessDisconnectCleansEstablishedSession(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestExternalACPProcessHelper$", "-test.v=false")
	command.Env = append(os.Environ(), externalACPProcessHelperEnv+"=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stderrDone := make(chan struct{})
	var stderrBytes []byte
	go func() {
		stderrBytes, _ = io.ReadAll(stderr)
		close(stderrDone)
	}()
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()

	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	responses := bufio.NewScanner(stdout)
	responses.Buffer(make([]byte, 1024), 1<<20)
	readResponse := func(wantID string) map[string]any {
		t.Helper()
		for responses.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(responses.Bytes(), &frame); err != nil {
				t.Fatalf("invalid child ACP frame %q: %v", responses.Bytes(), err)
			}
			if fmt.Sprint(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("child ACP stdout ended before response %s: %v", wantID, responses.Err())
		return nil
	}
	if initialize := readResponse("1"); initialize["error"] != nil {
		t.Fatalf("child initialize failed: %#v", initialize)
	}
	if err := encoder.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "session/new",
		"params": map[string]any{"cwd": t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	newResponse := readResponse("2")
	result, ok := newResponse["result"].(map[string]any)
	if !ok || result["sessionId"] != "external-process-session" {
		t.Fatalf("child session/new response = %#v", newResponse)
	}

	// EOF is the client-disconnect boundary. The child must first cancel all
	// established sessions and then close them before its process exits.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("child ACP process exit = %v", err)
	}
	<-stderrDone
	stderrText := string(stderrBytes)
	if !strings.Contains(stderrText, "ACP_EXTERNAL_CANCELLED") || !strings.Contains(stderrText, "ACP_EXTERNAL_CLOSED") {
		t.Fatalf("child cleanup markers = %q", stderrText)
	}
}

type externalContractFactory struct{}

func (externalContractFactory) NewSession(context.Context, string) (acp.Session, error) {
	return externalContractSession{}, nil
}

func (externalContractFactory) ResumeSession(context.Context, string) (acp.Session, error) {
	return externalContractSession{}, nil
}

func (externalContractFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": true}
}

type externalContractSession struct{}

func (externalContractSession) Prompt(context.Context, string, func(acp.Update)) (acp.StopReason, error) {
	return acp.StopEndTurn, nil
}
func (externalContractSession) Cancel() error { return nil }
func (externalContractSession) Close() error  { return nil }

type externalRichFactory struct {
	ready   chan struct{}
	release chan struct{}
	session *externalRichSession
}

func (f *externalRichFactory) NewSession(context.Context, string) (acp.Session, error) {
	f.session = &externalRichSession{}
	if f.ready != nil {
		close(f.ready)
	}
	if f.release != nil {
		<-f.release
	}
	return f.session, nil
}

func (*externalRichFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": true}
}

type externalRichSession struct {
	blocks []acp.PromptContentBlock
}

func (s *externalRichSession) Prompt(context.Context, string, func(acp.Update)) (acp.StopReason, error) {
	return acp.StopEndTurn, nil
}

func (s *externalRichSession) PromptContent(_ context.Context, blocks []acp.PromptContentBlock, emit func(acp.Update)) (acp.StopReason, error) {
	s.blocks = append([]acp.PromptContentBlock(nil), blocks...)
	emit(acp.Update{Content: &acp.PromptContentBlock{Type: "text", Text: "rich answer"}})
	return acp.StopEndTurn, nil
}

func (*externalRichSession) Cancel() error { return nil }
func (*externalRichSession) Close() error  { return nil }

// TestExternalACPWireContract intentionally uses only exported ACP symbols.
// This catches accidental reliance on package-private dispatch behavior and
// verifies the protocol/version/error envelope a real host sees.
func TestExternalACPWireContract(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"C:\\work"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"relative"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"unknown/method","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	server := &acp.Server{Factory: externalContractFactory{}, In: strings.NewReader(input), Out: &output}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	responses := make(map[string]map[string]any)
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("invalid ACP frame %q: %v", line, err)
		}
		id, ok := response["id"]
		if !ok {
			t.Fatalf("response missing id: %#v", response)
		}
		responses[wireID(id)] = response
	}
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want 4: %s", len(responses), output.String())
	}
	initResponse := responses["1"]
	if initResponse == nil {
		t.Fatalf("initialize response missing: %#v", responses)
	}
	if got := initResponse["result"].(map[string]any)["protocolVersion"]; got != float64(acp.ProtocolVersion) {
		t.Fatalf("protocolVersion = %#v, want %d", got, acp.ProtocolVersion)
	}
	newResponse := responses["2"]
	if _, ok := newResponse["result"].(map[string]any)["sessionId"]; !ok {
		t.Fatalf("session/new response missing sessionId: %#v", newResponse)
	}
	badResponse := responses["3"]
	if got := badResponse["error"].(map[string]any)["code"]; got != float64(-32602) {
		t.Fatalf("relative cwd error code = %#v, want -32602", got)
	}
	if got := responses["4"]["error"].(map[string]any)["code"]; got != float64(-32601) {
		t.Fatalf("unknown method error code = %#v, want -32601", got)
	}
}

func TestExternalACPResumeAndReconnectWireContract(t *testing.T) {
	for _, method := range []string{"session/resume", "session/reconnect"} {
		t.Run(method, func(t *testing.T) {
			input := fmt.Sprintf(`{"jsonrpc":"2.0","id":"resume-1","method":%q,"params":{"sessionId":"persisted-7"}}`+"\n", method)
			var output bytes.Buffer
			server := &acp.Server{Factory: externalContractFactory{}, In: strings.NewReader(input), Out: &output}
			if err := server.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			var response map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &response); err != nil {
				t.Fatalf("invalid response: %v (%s)", err, output.String())
			}
			result, ok := response["result"].(map[string]any)
			if !ok || result["sessionId"] != "persisted-7" {
				t.Fatalf("%s response = %#v", method, response)
			}
		})
	}
}

func TestExternalACPSharedProtocolLifecycleFixture(t *testing.T) {
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	var textBlock struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(fixture.Prompt[0], &textBlock); err != nil || textBlock.Text == "" {
		t.Fatalf("fixture text prompt = (%#v, %v)", textBlock, err)
	}
	prompt, err := json.Marshal(map[string]any{"type": "text", "text": textBlock.Text})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	release := make(chan struct{})
	close(release)
	reader := &externalStagedReader{
		first: strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%q}}`, fixture.Workspace),
		}, "\n") + "\n",
		second:  fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[%s]}}`+"\n", fixture.SessionID, prompt),
		ready:   ready,
		release: release,
	}
	var output bytes.Buffer
	server := &acp.Server{Factory: &externalSharedFactory{sessionID: fixture.SessionID, ready: ready}, In: reader, Out: &output}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(output.String(), fixture.SessionID) ||
		!strings.Contains(output.String(), `"stopReason":"end_turn"`) {
		t.Fatalf("shared ACP lifecycle output = %s", output.String())
	}
}

func TestExternalACPResumeMetadataWireContract(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":"resume-1","method":"session/resume","params":{"sessionId":"persisted-metadata"}}` + "\n"
	var output bytes.Buffer
	server := &acp.Server{Factory: &externalResumeMetadataFactory{}, In: strings.NewReader(input), Out: &output}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var response struct {
		Result struct {
			SessionID string         `json:"sessionId"`
			Metadata  map[string]any `json:"metadata"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &response); err != nil {
		t.Fatalf("resume response = %s (%v)", output.String(), err)
	}
	if response.Result.SessionID != "persisted-metadata" || response.Result.Metadata["durable"] != true ||
		response.Result.Metadata["eventCursor"] != float64(2) {
		t.Fatalf("resume response = %#v", response.Result)
	}
}

func TestExternalACPPromptFaultPropagationWireContract(t *testing.T) {
	ready := make(chan struct{})
	promptStarted := make(chan struct{})
	release := make(chan struct{})
	reader := newGateReader(
		func() []byte {
			return []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"C:\\work"}}` + "\n")
		},
		func() []byte {
			<-ready
			time.Sleep(20 * time.Millisecond)
			return []byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"fault-session","prompt":[{"type":"text","text":"fail"}]}}` + "\n")
		},
	)
	var output bytes.Buffer
	server := &acp.Server{
		Factory: &externalFaultFactory{ready: ready, promptStarted: promptStarted, release: release, canceled: make(chan struct{}), failPrompt: true},
		In:      reader, Out: &output,
	}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(output.String(), `"code":-32603`) || !strings.Contains(output.String(), "scripted prompt failure") {
		t.Fatalf("prompt fault output = %s", output.String())
	}
}

func TestExternalACPCancelAndConcurrentPromptWireContract(t *testing.T) {
	ready := make(chan struct{})
	promptStarted := make(chan struct{})
	release := make(chan struct{})
	reader := newGateReader(
		func() []byte {
			return []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"C:\\work"}}` + "\n")
		},
		func() []byte {
			<-ready
			return []byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"fault-session","prompt":[{"type":"text","text":"wait"}]}}` + "\n")
		},
		func() []byte {
			<-promptStarted
			return []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"fault-session","prompt":[{"type":"text","text":"second"}]}}` + "\n")
		},
		func() []byte {
			time.Sleep(10 * time.Millisecond)
			return []byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"fault-session"}}` + "\n")
		},
	)
	var output bytes.Buffer
	server := &acp.Server{
		Factory: &externalFaultFactory{ready: ready, promptStarted: promptStarted, release: release, canceled: make(chan struct{})},
		In:      reader, Out: &output,
	}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(output.String(), `"code":-32602`) || !strings.Contains(output.String(), "already in flight") {
		t.Fatalf("concurrent prompt output = %s", output.String())
	}
	if !strings.Contains(output.String(), `"stopReason":"cancelled"`) {
		t.Fatalf("cancelled prompt output = %s", output.String())
	}
}

func TestExternalACPCancelUnknownSessionIsIdempotent(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":7,"method":"session/cancel","params":{"sessionId":"missing"}}` + "\n"
	var output bytes.Buffer
	server := &acp.Server{Factory: externalContractFactory{}, In: strings.NewReader(input), Out: &output}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(output.String()) != "" {
		t.Fatalf("unknown cancel should be an ignored notification, got %s", output.String())
	}
}

type externalFaultFactory struct {
	ready         chan struct{}
	promptStarted chan struct{}
	release       chan struct{}
	canceled      chan struct{}
	cancelOnce    sync.Once
	failPrompt    bool
}

func (f *externalFaultFactory) NewSession(context.Context, string) (acp.Session, error) {
	return &externalFaultSession{factory: f, canceled: make(chan struct{})}, nil
}

func (*externalFaultFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": true}
}

type externalFaultSession struct {
	factory  *externalFaultFactory
	canceled chan struct{}
}

func (s *externalFaultSession) SessionID() string { return "fault-session" }

func (s *externalFaultSession) SetPermissionRequester(acp.PermissionRequester) {
	close(s.factory.ready)
}

func (s *externalFaultSession) Prompt(ctx context.Context, _ string, emit func(acp.Update)) (acp.StopReason, error) {
	close(s.factory.promptStarted)
	if s.factory.failPrompt {
		return "", errors.New("scripted prompt failure")
	}
	select {
	case <-s.factory.canceled:
		return acp.StopCancelled, nil
	case <-s.factory.release:
		return acp.StopEndTurn, nil
	case <-ctx.Done():
		return acp.StopCancelled, nil
	}
}

func (s *externalFaultSession) Cancel() error {
	s.factory.cancelOnce.Do(func() { close(s.factory.canceled) })
	return nil
}
func (*externalFaultSession) Close() error { return nil }

type gateReader struct {
	stages []func() []byte
	index  int
}

func newGateReader(stages ...func() []byte) *gateReader {
	return &gateReader{stages: stages}
}

func (r *gateReader) Read(p []byte) (int, error) {
	if r.index >= len(r.stages) {
		return 0, io.EOF
	}
	stage := r.stages[r.index]
	r.index++
	data := stage()
	return copy(p, data), nil
}

type externalResumeMetadataFactory struct{}

func (externalResumeMetadataFactory) ResumeSession(context.Context, string) (acp.Session, error) {
	return externalResumeMetadataSession{}, nil
}

func (externalResumeMetadataFactory) NewSession(context.Context, string) (acp.Session, error) {
	return externalResumeMetadataSession{}, nil
}

type externalResumeMetadataSession struct{}

func (externalResumeMetadataSession) ResumeMetadata() map[string]any {
	return map[string]any{"durable": true, "eventCursor": 2}
}
func (externalResumeMetadataSession) Prompt(context.Context, string, func(acp.Update)) (acp.StopReason, error) {
	return acp.StopEndTurn, nil
}
func (externalResumeMetadataSession) Cancel() error { return nil }
func (externalResumeMetadataSession) Close() error  { return nil }

type externalSharedFactory struct {
	sessionID string
	ready     chan struct{}
}

func (f *externalSharedFactory) NewSession(context.Context, string) (acp.Session, error) {
	return &externalSharedSession{id: f.sessionID, ready: f.ready}, nil
}

func (*externalSharedFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": true}
}

type externalSharedSession struct {
	id    string
	ready chan struct{}
}

func (s *externalSharedSession) SessionID() string { return s.id }
func (s *externalSharedSession) SetPermissionRequester(acp.PermissionRequester) {
	close(s.ready)
}
func (s *externalSharedSession) Prompt(context.Context, string, func(acp.Update)) (acp.StopReason, error) {
	return acp.StopEndTurn, nil
}
func (*externalSharedSession) Cancel() error { return nil }
func (*externalSharedSession) Close() error  { return nil }

func TestExternalACPRichContentWireContract(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	factory := &externalRichFactory{ready: ready, release: release}
	reader := &externalStagedReader{
		first:   `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"C:\\work"}}` + "\n",
		second:  `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"shutu-1","prompt":[{"type":"text","text":"look"},{"type":"resource_link","name":"source","uri":"file:///tmp/a.go"},{"type":"image","data":"aGk=","mimeType":"image/png"}]}}` + "\n",
		ready:   ready,
		release: release,
	}
	var output bytes.Buffer
	server := &acp.Server{Factory: factory, In: reader, Out: &output}
	done := make(chan error, 1)
	go func() { done <- server.Run(context.Background()) }()
	select {
	case <-done:
		t.Fatal("server ended before rich prompt was released")
	case <-time.After(time.Second):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not finish")
	}
	if factory.session == nil || len(factory.session.blocks) != 3 || factory.session.blocks[2].MimeType != "image/png" {
		t.Fatalf("rich blocks = %#v", factory.session)
	}
	if !strings.Contains(output.String(), `"method":"session/update"`) || !strings.Contains(output.String(), `"stopReason":"end_turn"`) {
		t.Fatalf("rich wire output missing update/response: %s", output.String())
	}
}

func TestExternalACPPermissionWireContract(t *testing.T) {
	permissionWritten := make(chan struct{})
	reader := &externalPermissionReader{
		first:  `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"C:\\work"}}` + "\n",
		second: `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"permission-session","prompt":[{"type":"text","text":"run"}]}}` + "\n",
		third:  `{"jsonrpc":"2.0","id":"permission-1","result":{"outcome":{"outcome":"selected","optionId":"allow-once"}}}` + "\n",
		ready:  permissionWritten,
	}
	output := externalPermissionWriter{ready: permissionWritten}
	server := &acp.Server{
		Factory: externalPermissionFactory{},
		In:      reader,
		Out:     &output,
	}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	frames := strings.Split(strings.TrimSpace(output.String()), "\n")
	var permissionRequest map[string]any
	permissionResponse := map[string]any{}
	for _, line := range frames {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("invalid ACP frame %q: %v", line, err)
		}
		if frame["method"] == "session/request_permission" {
			permissionRequest = frame
		}
		if wireID(frame["id"]) == "2" {
			permissionResponse = frame
		}
	}
	if permissionRequest == nil {
		t.Fatalf("server did not send session/request_permission: %s", output.String())
	}
	params, ok := permissionRequest["params"].(map[string]any)
	if !ok || params["sessionId"] != "permission-session" {
		t.Fatalf("permission request params = %#v", permissionRequest)
	}
	toolCall, ok := params["toolCall"].(map[string]any)
	if !ok || toolCall["toolCallId"] != "wire-call" || toolCall["name"] != "terminal" {
		t.Fatalf("permission tool call = %#v", params["toolCall"])
	}
	if permissionResponse["result"] == nil || !strings.Contains(output.String(), `"stopReason":"end_turn"`) {
		t.Fatalf("permission-approved prompt did not complete: %s", output.String())
	}
}

// TestExternalACPClientAdmissionMatrix consolidates the wire cases that were
// previously scattered across focused tests. A real ACP peer must receive one
// stable class for every unsupported/rich/input shape; unlike in-process
// dispatch tests, this exercises the newline JSON transport and exported
// Server seam.
func TestExternalACPClientAdmissionMatrix(t *testing.T) {
	inputs := []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}},
		{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\text`}},
		{"jsonrpc": "2.0", "id": 3, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\resource`}},
		{"jsonrpc": "2.0", "id": 4, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\image`}},
		{"jsonrpc": "2.0", "id": 5, "method": "session/new", "params": map[string]any{"cwd": "relative"}},
		{"jsonrpc": "2.0", "id": 6, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\extra`, "additionalDirectories": []string{`C:\other`}}},
		{"jsonrpc": "2.0", "id": 7, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\mcp`, "mcpServers": []any{map[string]any{"name": "x"}}}},
		{"jsonrpc": "2.0", "id": 17, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\audio`}},
		{"jsonrpc": "2.0", "id": 18, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\context`}},
		{"jsonrpc": "2.0", "id": 19, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\empty`}},
		{"jsonrpc": "2.0", "id": 20, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\bad-base64`}},
		{"jsonrpc": "2.0", "id": 21, "method": "session/new", "params": map[string]any{"cwd": `C:\matrix\bad-type`}},
		{"jsonrpc": "2.0", "id": 8, "method": "session/prompt", "params": map[string]any{"sessionId": "text", "prompt": []any{map[string]any{"type": "text", "text": "plain"}}}},
		{"jsonrpc": "2.0", "id": 9, "method": "session/prompt", "params": map[string]any{"sessionId": "resource", "prompt": []any{map[string]any{"type": "resource_link", "name": "source", "uri": "file:///workspace/source.go"}}}},
		{"jsonrpc": "2.0", "id": 10, "method": "session/prompt", "params": map[string]any{"sessionId": "image", "prompt": []any{map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"}}}},
		{"jsonrpc": "2.0", "id": 11, "method": "session/prompt", "params": map[string]any{"sessionId": "audio", "prompt": []any{map[string]any{"type": "audio", "data": "aGk=", "mimeType": "audio/wav"}}}},
		{"jsonrpc": "2.0", "id": 12, "method": "session/prompt", "params": map[string]any{"sessionId": "context", "prompt": []any{map[string]any{"type": "embedded_context", "text": "context"}}}},
		{"jsonrpc": "2.0", "id": 13, "method": "session/prompt", "params": map[string]any{"sessionId": "missing", "prompt": []any{map[string]any{"type": "text", "text": "missing"}}}},
		{"jsonrpc": "2.0", "id": 14, "method": "session/prompt", "params": map[string]any{"sessionId": "empty", "prompt": []any{map[string]any{"type": "text", "text": "   "}}}},
		{"jsonrpc": "2.0", "id": 15, "method": "session/prompt", "params": map[string]any{"sessionId": "bad-base64", "prompt": []any{map[string]any{"type": "image", "data": "not-base64", "mimeType": "image/png"}}}},
		{"jsonrpc": "2.0", "id": 16, "method": "session/prompt", "params": map[string]any{"sessionId": "bad-type", "prompt": []any{map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/svg+xml"}}}},
	}
	lines := make([]string, 0, len(inputs))
	for _, input := range inputs {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	var output bytes.Buffer
	server := &acp.Server{Factory: &externalMatrixFactory{}, In: strings.NewReader(strings.Join(lines, "\n") + "\n"), Out: &output}
	if err := server.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	responses := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("invalid matrix frame %q: %v", line, err)
		}
		if id := wireID(frame["id"]); id != "" {
			responses[id] = frame
		}
	}
	for id := 1; id <= len(inputs); id++ {
		key := strconv.Itoa(id)
		if responses[key] == nil {
			t.Fatalf("matrix response %d missing: %s", id, output.String())
		}
	}
	for _, id := range []string{"2", "3", "4"} {
		if responses[id]["result"] == nil {
			t.Fatalf("valid session/new %s = %#v", id, responses[id])
		}
	}
	for _, id := range []string{"5", "6", "7", "11", "12", "13", "14", "15", "16"} {
		errObj, ok := responses[id]["error"].(map[string]any)
		if !ok || errObj["code"] != float64(-32602) {
			t.Fatalf("unsupported/invalid response %s = %#v, want -32602", id, responses[id])
		}
	}
	for _, id := range []string{"8", "9", "10"} {
		result, ok := responses[id]["result"].(map[string]any)
		if !ok || result["stopReason"] != string(acp.StopEndTurn) {
			t.Fatalf("accepted prompt %s = %#v", id, responses[id])
		}
	}

	text := externalMatrixResult(responses["2"])["sessionId"].(string)
	resource := externalMatrixResult(responses["3"])["sessionId"].(string)
	image := externalMatrixResult(responses["4"])["sessionId"].(string)
	if text != "text" || resource != "resource" || image != "image" {
		t.Fatalf("matrix session ids = %q/%q/%q", text, resource, image)
	}
}

func externalMatrixResult(frame map[string]any) map[string]any {
	result, _ := frame["result"].(map[string]any)
	return result
}

type externalMatrixFactory struct {
	mu       sync.Mutex
	sessions map[string]*externalMatrixSession
}

func (f *externalMatrixFactory) NewSession(_ context.Context, cwd string) (acp.Session, error) {
	id := strings.ReplaceAll(strings.TrimPrefix(strings.ReplaceAll(cwd, "\\", "/"), "C:/matrix/"), "/", "-")
	session := &externalMatrixSession{id: id}
	f.mu.Lock()
	if f.sessions == nil {
		f.sessions = map[string]*externalMatrixSession{}
	}
	f.sessions[id] = session
	f.mu.Unlock()
	return session, nil
}

func (*externalMatrixFactory) Capabilities(context.Context) map[string]bool {
	return map[string]bool{"image": true}
}

type externalMatrixSession struct {
	id      string
	texts   []string
	blocks  []acp.PromptContentBlock
	textMu  sync.Mutex
	blockMu sync.Mutex
}

func (s *externalMatrixSession) SessionID() string { return s.id }

func (s *externalMatrixSession) Prompt(_ context.Context, text string, emit func(acp.Update)) (acp.StopReason, error) {
	s.textMu.Lock()
	s.texts = append(s.texts, text)
	s.textMu.Unlock()
	emit(acp.Update{Text: "matrix"})
	return acp.StopEndTurn, nil
}

func (s *externalMatrixSession) PromptContent(_ context.Context, blocks []acp.PromptContentBlock, emit func(acp.Update)) (acp.StopReason, error) {
	s.blockMu.Lock()
	s.blocks = append(s.blocks, blocks...)
	s.blockMu.Unlock()
	emit(acp.Update{Text: "matrix"})
	return acp.StopEndTurn, nil
}

func (*externalMatrixSession) Cancel() error { return nil }
func (*externalMatrixSession) Close() error  { return nil }

type externalPermissionFactory struct{}

func (externalPermissionFactory) NewSession(context.Context, string) (acp.Session, error) {
	return &externalPermissionSession{}, nil
}

type externalPermissionSession struct {
	requester acp.PermissionRequester
}

func (*externalPermissionSession) SessionID() string { return "permission-session" }
func (s *externalPermissionSession) SetPermissionRequester(requester acp.PermissionRequester) {
	s.requester = requester
}
func (s *externalPermissionSession) Prompt(ctx context.Context, _ string, emit func(acp.Update)) (acp.StopReason, error) {
	outcome, err := s.requester.RequestPermission(ctx, acp.PermissionRequest{
		ToolCallID: "wire-call",
		ToolName:   "terminal",
		Reason:     "run a command",
		Options:    []acp.PermissionOption{{ID: "allow-once", Label: "Allow once"}},
	})
	if err != nil {
		return "", err
	}
	if outcome.OptionID != "allow-once" {
		return "", fmt.Errorf("unexpected permission outcome: %+v", outcome)
	}
	emit(acp.Update{Text: "approved"})
	return acp.StopEndTurn, nil
}
func (*externalPermissionSession) Cancel() error { return nil }
func (*externalPermissionSession) Close() error  { return nil }

type externalPermissionReader struct {
	first, second, third string
	ready                <-chan struct{}
	phase                int
}

func (r *externalPermissionReader) Read(p []byte) (int, error) {
	if r.phase == 0 {
		r.phase++
		return copy(p, r.first), nil
	}
	if r.phase == 1 {
		r.phase++
		return copy(p, r.second), nil
	}
	if r.phase == 2 {
		<-r.ready
		r.phase++
		return copy(p, r.third), nil
	}
	return 0, io.EOF
}

type externalPermissionWriter struct {
	mu       sync.Mutex
	output   bytes.Buffer
	wroteOne sync.Once
	ready    chan struct{}
}

func (w *externalPermissionWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.output.Write(p)
	if bytes.Contains(p, []byte(`"method":"session/request_permission"`)) {
		w.wroteOne.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func (w *externalPermissionWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.String()
}

type externalStagedReader struct {
	first, second  string
	ready, release <-chan struct{}
	phase          int
}

func (r *externalStagedReader) Read(p []byte) (int, error) {
	if r.phase == 0 {
		r.phase++
		return copy(p, r.first), nil
	}
	if r.phase == 1 {
		<-r.ready
		<-r.release
		r.phase++
		return copy(p, r.second), nil
	}
	return 0, io.EOF
}

func wireID(value any) string {
	switch number := value.(type) {
	case float64:
		return strconv.FormatInt(int64(number), 10)
	case string:
		return number
	default:
		return ""
	}
}
