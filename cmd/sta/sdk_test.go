package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/agent"
	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/prompt"
	"github.com/shutu-ai/shutu-agent/internal/sdkclient"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

func TestSDKServerShutdownIsConcurrentAndIdempotent(t *testing.T) {
	server := newSDKServer(nil, strings.NewReader(""), &strings.Builder{})
	results := make(chan *sdkRPCError, 8)
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- server.shutdown()
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent shutdown = %#v, want nil", err)
		}
	}
	if err := server.shutdown(); err != nil {
		t.Fatalf("shutdown after completion = %#v, want nil", err)
	}
	server.shutdownMu.Lock()
	complete := server.shutdownComplete
	server.shutdownMu.Unlock()
	if !complete {
		t.Fatal("shutdown did not record completion")
	}
}

func TestSDKServerPreservesWireRequestOrderAroundShutdown(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"cwd":"."}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`,
	}, "\n") + "\n"
	var output strings.Builder
	server := newSDKServer(nil, strings.NewReader(input), &output)
	if err := server.run(context.Background()); err != nil {
		t.Fatalf("run = %v", err)
	}
	raw := output.String()
	first := strings.Index(raw, `"id":1`)
	second := strings.Index(raw, `"id":2`)
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("responses are not in wire order: %s", raw)
	}
	if !strings.Contains(raw, `"message":"no adapter registered for provider`) {
		t.Fatalf("initialize response = %s", raw)
	}
}

func TestSDKServerIgnoresMalformedJSONLines(t *testing.T) {
	var output strings.Builder
	server := newSDKServer(nil, strings.NewReader("not-json\n"), &output)
	if err := server.run(context.Background()); err != nil {
		t.Fatalf("run = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("malformed JSON produced a response: %s", output.String())
	}
}

func TestSDKContentValidationRejectsInvalidBatchBeforeSessionCreation(t *testing.T) {
	err := validateSDKContentBlocks([]sdkContentBlock{
		{Type: "text", Text: "ok"},
		{Type: "image", MimeType: "image/png", Data: "not-base64"},
	})
	if err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("validation error = %v, want canonical base64 rejection", err)
	}
}

func TestSDKDecodeContentPreservesReferenceBlockUnion(t *testing.T) {
	server := newSDKServer(nil, strings.NewReader(""), &strings.Builder{})
	blocks := []sdkContentBlock{
		sdkclient.TextContent("visible"),
		sdkclient.ReasoningContent("private"),
		sdkclient.ToolCallContent("call-1", "read", `{"path":"x"}`),
		sdkclient.ToolResultContent("call-1", []sdkContentBlock{sdkclient.TextContent("ok")}, true),
	}
	decoded, err := server.decodeContent(blocks, "session")
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.ContentBlock{
		{Kind: llm.BlockText, Text: "visible"},
		{Kind: llm.BlockReasoning, Text: "private"},
		{Kind: llm.BlockToolCall, CallID: "call-1", Name: "read", Arguments: `{"path":"x"}`},
		{Kind: llm.BlockToolResult, CallID: "call-1", IsError: true, Blocks: []llm.ContentBlock{llm.Text("ok")}},
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded = %#v, want %#v", decoded, want)
	}
}

func TestSDKDecodeResolvesReferenceImageAttachment(t *testing.T) {
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	attachStore, err := attachment.NewStore(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := attachStore.SaveImage("image/png", imageBytes.Bytes(), len(imageBytes.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "attachments.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	registry := llm.NewRegistry()
	if err := registry.Register(sdkTurnProvider{model: &turnLLM{}}); err != nil {
		t.Fatal(err)
	}
	app := &app{
		cfg: config.Config{
			Model: "test-model",
			LLM:   config.LLMConfig{Provider: "deepseek-official", ModelInputModalities: "text,image"},
		},
		store:       st,
		attachStore: attachStore,
		llmReg:      registry,
		customProviders: []customProviderProfile{{
			ID: "deepseek-official", Model: "test-model",
			Models: []customModel{{ID: "test-model", Vision: catalogBool(true)}},
		}},
	}
	server := newSDKServer(app, strings.NewReader(""), &strings.Builder{})
	decoded, err := server.decodeContent([]sdkContentBlock{sdkclient.ImageAttachmentContent(sdkclient.ImageAttachmentRef{
		AttachmentID: ref.ID,
		MediaType:    ref.MediaType,
		Bytes:        ref.Bytes,
		Width:        ref.Width,
		Height:       ref.Height,
	})}, "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Kind != llm.BlockImage || decoded[0].Image.ID != ref.ID || decoded[0].Image.Path == "" {
		t.Fatalf("decoded attachment = %#v, want durable image ref", decoded)
	}
}

func TestSDKSharedProtocolLifecycleFixture(t *testing.T) {
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([]sdkContentBlock, 0, len(fixture.Prompt))
	for _, raw := range fixture.Prompt {
		var block sdkContentBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, block)
	}
	if err := validateSDKContentBlocks(blocks); err != nil {
		t.Fatal(err)
	}
	server := newSDKServer(nil, strings.NewReader(""), &strings.Builder{})
	decoded := make([]llm.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		content, err := server.decodeContent([]sdkContentBlock{block}, fixture.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, content...)
	}
	if len(decoded) != 1 || decoded[0].Text == "" || fixture.MessageID == "" || fixture.Assistant == "" {
		t.Fatalf("shared SDK fixture projection = %#v", decoded)
	}
}

func TestSDKContentExtensionRoundTripsAndInboxUsesReferenceVocabulary(t *testing.T) {
	var block sdkContentBlock
	extension := `{"type":"x-plugin/block","payload":{"opaque":true}}`
	if err := json.Unmarshal([]byte(extension), &block); err != nil {
		t.Fatal(err)
	}
	server := newSDKServer(nil, strings.NewReader(""), &strings.Builder{})
	decoded, err := server.decodeContent([]sdkContentBlock{block}, "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Kind != "x-plugin/block" || string(decoded[0].Raw) != extension {
		t.Fatalf("extension block = %#v", decoded[0])
	}

	log := session.New()
	journal := sessionInboxJournal{log: log}
	if err := journal.AppendInboxEvent(agent.InboxEvent{
		Target:   "next-turn",
		Inserted: []agent.Message{{ID: "message-1", Text: "hello", Content: []llm.ContentBlock{llm.Text("hello")}, Kind: agent.MessageNextTurn}},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(log.Events()[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"Kind"`) || strings.Contains(string(raw), `"Text"`) || !strings.Contains(string(raw), `"type":"text"`) {
		t.Fatalf("inbox receipt content = %s, want reference lowercase content vocabulary", raw)
	}
}

func TestSDKWireEventPreservesEnvelopeAndOpaqueData(t *testing.T) {
	at := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	event := session.Event{Seq: 7, Type: session.EventTurnStart, At: at, Version: session.EventVersion, Data: json.RawMessage(`{"turn":3,"extra":{"x":true}}`)}
	wire := sdkWireEvent(event)
	if wire["seq"] != uint64(7) || wire["type"] != session.EventTurnStart {
		t.Fatalf("wire envelope = %#v", wire)
	}
	if _, exists := wire["version"]; exists {
		t.Fatalf("wire envelope leaks non-reference version field: %#v", wire)
	}
	if wire["time"] != int64(at.UnixMilli()) {
		t.Fatalf("wire time = %#v, want numeric epoch milliseconds", wire["time"])
	}
	data, ok := wire["data"].(map[string]any)
	if !ok || data["turn"] != float64(3) {
		t.Fatalf("wire data = %#v, want decoded event payload", wire["data"])
	}
}

func TestSDKWireEventLiftsCanonicalSurfaceMetadata(t *testing.T) {
	user := session.Event{Seq: 4, Type: session.EventUserMessage, At: time.UnixMilli(10), Version: session.EventVersion, Data: mustJSON(map[string]any{
		"role":            "user",
		"content":         []map[string]any{{"type": "text", "text": "hello"}},
		"source":          map[string]any{"kind": "user"},
		"surfaceOp":       "internal-storage-copy",
		"sourceEventSeqs": []uint64{1},
	})}
	wire := sdkWireEvent(user)
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ValidateWireEvent(encoded); err != nil {
		t.Fatalf("canonical SDK surface event rejected: %v\n%s", err, encoded)
	}
	if got := wire["surfaceOp"]; got != "append" {
		t.Fatalf("user surfaceOp = %#v, want append", got)
	}
	if got, ok := wire["sourceEventSeqs"].([]uint64); !ok || len(got) != 1 || got[0] != 1 {
		t.Fatalf("user sourceEventSeqs = %#v", wire["sourceEventSeqs"])
	}
	data, ok := wire["data"].(map[string]any)
	if !ok {
		t.Fatalf("user data = %#v", wire["data"])
	}
	if _, ok := data["surfaceOp"]; ok {
		t.Fatalf("user data retained surfaceOp: %#v", data)
	}
	if _, ok := data["sourceEventSeqs"]; ok {
		t.Fatalf("user data retained sourceEventSeqs: %#v", data)
	}

	replacement := session.Event{Seq: 8, Type: session.EventUserMessage, At: time.UnixMilli(20), Version: session.EventVersion, Data: mustJSON(session.NewUserMessageReplace("summary", 1, 7))}
	wire = sdkWireEvent(replacement)
	op, ok := wire["surfaceOp"].(map[string]any)
	if !ok || op["op"] != "replace" || op["start"] != int64(1) || op["end"] != int64(7) {
		t.Fatalf("replacement surfaceOp = %#v", wire["surfaceOp"])
	}
}

func TestSDKDispatchRejectsUnknownMethodAndUninitializedPrompt(t *testing.T) {
	s := newSDKServer(nil, strings.NewReader(""), &strings.Builder{})
	if _, rpcErr, _ := s.dispatch(nil, "session/prompt", json.RawMessage(`{"sessionId":"s","contentBlocks":[{"type":"text","text":"x"}]}`)); rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("prompt error = %#v, want invalid-params", rpcErr)
	}
	if _, rpcErr, _ := s.dispatch(nil, "unknown", nil); rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("unknown method error = %#v, want method-not-found", rpcErr)
	}
}

func TestSDKServerAcceptsExternalClientInitializeAndShutdown(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "contract-test-key")
	a := &app{
		cfg: config.Config{
			Model: "deepseek-v4-flash",
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
		hub: NewEventHub(),
		reg: tools.New(),
	}
	if err := a.reg.Register(tools.GetTime{}); err != nil {
		t.Fatal(err)
	}
	if err := a.registerLLM(); err != nil {
		t.Fatal(err)
	}

	clientConn, serverConn := net.Pipe()
	server := newSDKServer(a, serverConn, serverConn)
	done := make(chan error, 1)
	go func() { done <- server.run(context.Background()) }()
	transport := sdkclient.NewLineTransport(clientConn, clientConn, nil, nil)
	transport.Start()
	defer func() {
		_ = transport.Close()
		_ = clientConn.Close()
		<-done
	}()

	raw, err := transport.Request(context.Background(), "initialize", json.RawMessage(`{"cwd":".","maxTokens":77}`), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		ToolCatalog *sdkclient.ToolCatalog `json:"toolCatalog"`
	}
	if json.Unmarshal(raw, &identity) != nil || identity.ServerInfo.Name != sdkServerName || identity.ServerInfo.Version != sdkServerVersion {
		t.Fatalf("initialize identity = %s", raw)
	}
	if identity.ToolCatalog == nil || identity.ToolCatalog.SchemaVersion != 1 || len(identity.ToolCatalog.Tools) != 1 || identity.ToolCatalog.Tools[0].Name != "get_time" {
		t.Fatalf("initialize tool catalog = %+v", identity.ToolCatalog)
	}
	if expected, err := filepath.Abs("."); err != nil || server.cwd != expected { // The contract records an absolute workspace root.
		t.Fatalf("SDK cwd = %q, want %q (%v)", server.cwd, expected, err)
	}
	raw, err = transport.Request(context.Background(), "shutdown", nil, 2*time.Second)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("shutdown = (%s, %v), want empty object", raw, err)
	}
}

func TestSDKServerExternalClientPromptRunsAgentThroughIdle(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "sdk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model := &turnLLM{}
	llmRegistry := llm.NewRegistry()
	if err := llmRegistry.Register(sdkTurnProvider{model: model}); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: config.Config{
			Model: "test-model",
			Mode:  config.ModeStandard,
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
		baseCtx:       context.Background(),
		store:         st,
		hub:           NewEventHub(),
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
		llm:           model,
		llmReg:        llmRegistry,
		reg:           tools.New(),
		prompt:        prompt.New("You are an SDK integration agent."),
	}

	clientConn, serverConn := net.Pipe()
	notifications := make(chan sdkclient.Notification, 64)
	server := newSDKServer(a, serverConn, serverConn)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.run(context.Background()) }()
	transport := sdkclient.NewLineTransport(clientConn, clientConn, func(notification sdkclient.Notification) {
		notifications <- notification
	}, nil)
	transport.Start()
	defer func() {
		_ = transport.Close()
		_ = clientConn.Close()
		<-serverDone
	}()

	raw, err := transport.Request(context.Background(), "initialize", json.RawMessage(`{"cwd":"."}`), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	promptParams, err := json.Marshal(sdkclient.SessionPromptParams{
		SessionID:     "sdk-integration",
		ContentBlocks: []sdkclient.ContentBlock{sdkclient.TextContent("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = transport.Request(context.Background(), "session/prompt", promptParams, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var receipt sdkclient.SessionPromptResult
	if json.Unmarshal(raw, &receipt) != nil || receipt.MessageID == "" {
		t.Fatalf("prompt receipt = %s", raw)
	}

	var events []sdkclient.SessionEvent
	methods := make([]string, 0, 16)
	received := false
	idle := false
	for {
		select {
		case notification := <-notifications:
			methods = append(methods, notification.Method)
			if notification.Method == "session.status" {
				if strings.Contains(string(notification.Params), `"idle"`) && received {
					idle = true
					break
				} else {
					continue
				}
			}
			var params struct {
				SessionID string                 `json:"sessionId"`
				Event     sdkclient.SessionEvent `json:"event"`
			}
			if json.Unmarshal(notification.Params, &params) != nil || params.SessionID != "sdk-integration" {
				t.Fatalf("invalid session notification = %#v", notification)
			}
			if !received {
				if !isSDKReceipt(params.Event, receipt.MessageID) {
					continue
				}
				received = true
			}
			events = append(events, params.Event)
		case <-time.After(2 * time.Second):
			t.Fatal("SDK agent did not stream receipt and idle through event hub")
		}
		if idle {
			break
		}
	}
	if !received {
		t.Fatal("durable prompt receipt was not observed")
	}
	if len(methods) < 3 || methods[0] != "session.event" || methods[1] != "session.status" || methods[len(methods)-1] != "session.status" {
		t.Fatalf("notification methods = %#v, want receipt event, running status, ..., idle status", methods)
	}
	raw, err = transport.Request(context.Background(), "session/snapshot", json.RawMessage(`{"sessionId":"sdk-integration"}`), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot sdkclient.SessionSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.SessionID != "sdk-integration" {
		t.Fatalf("SDK snapshot = %s, err=%v", raw, err)
	}
	if snapshot.Snapshot.AsOfSeq == 0 || len(snapshot.Snapshot.History) == 0 || len(snapshot.Snapshot.Surface) == 0 {
		t.Fatalf("SDK snapshot projection = %+v", snapshot.Snapshot)
	}
	var persisted []session.Event
	if persisted, err = st.LoadSession(context.Background(), "sdk-integration"); err != nil || len(persisted) < len(events) {
		t.Fatalf("persisted events = (%d, %v), streamed = %d", len(persisted), err, len(events))
	}
	expectedSnapshot, err := projection.Build(persisted)
	if err != nil {
		t.Fatalf("cold projection: %v", err)
	}
	gotSnapshotJSON, err := json.Marshal(snapshot.Snapshot)
	if err != nil {
		t.Fatalf("marshal SDK projection: %v", err)
	}
	wantSnapshotJSON, err := json.Marshal(expectedSnapshot)
	if err != nil {
		t.Fatalf("marshal cold projection: %v", err)
	}
	if !bytes.Equal(gotSnapshotJSON, wantSnapshotJSON) {
		t.Fatalf("SDK snapshot diverges from cold projection\ngot:  %s\nwant: %s", gotSnapshotJSON, wantSnapshotJSON)
	}
	raw, err = transport.Request(context.Background(), "shutdown", nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("shutdown result = %s", raw)
	}
}

func TestSDKClientDrivesRealRuntimeChildThroughIdle(t *testing.T) {
	workspace := t.TempDir()
	client := sdkclient.NewClient(sdkclient.ClientOptions{
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestSDKRuntimeChildProcess$"},
		Dir:            workspace,
		Env:            append(os.Environ(), "SHUTU_SDK_RUNTIME_CHILD=1"),
		RequestTimeout: 3 * time.Second,
	})
	defer client.Close()
	subscription := client.Subscribe(nil)
	defer subscription.Close()
	initialized, err := client.Initialize(context.Background(), sdkclient.InitializeParams{CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if initialized.ServerInfo.Name != sdkServerName {
		t.Fatalf("child SDK identity = %#v", initialized.ServerInfo)
	}
	messageID, err := client.Prompt(context.Background(), "sdk-child", []sdkclient.ContentBlock{sdkclient.TextContent("hello")})
	if err != nil || messageID == "" {
		t.Fatalf("child SDK prompt = %q, err=%v", messageID, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	methods := make([]string, 0, 16)
	sawReceipt := false
	sawAssistant := false
	for {
		notification, nextErr := subscription.Next(ctx)
		if nextErr != nil {
			t.Fatalf("child SDK notification: %v", nextErr)
		}
		methods = append(methods, notification.Method)
		if notification.Method == "session.event" {
			var params struct {
				SessionID string                 `json:"sessionId"`
				Event     sdkclient.SessionEvent `json:"event"`
			}
			if err := json.Unmarshal(notification.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.SessionID != "sdk-child" {
				t.Fatalf("child SDK event session = %q", params.SessionID)
			}
			if isSDKReceipt(params.Event, messageID) {
				sawReceipt = true
			}
			if params.Event.Type == session.EventAssistantMessage {
				sawAssistant = true
			}
		}
		if notification.Method == "session.status" &&
			strings.Contains(string(notification.Params), `"status":"idle"`) {
			break
		}
	}
	if !sawReceipt || !sawAssistant {
		t.Fatalf("child SDK lifecycle missed receipt/assistant: methods=%#v", methods)
	}
	if len(methods) < 3 || methods[0] != "session.event" || methods[1] != "session.status" || methods[len(methods)-1] != "session.status" {
		t.Fatalf("child SDK notification order = %#v", methods)
	}
	snapshot, err := client.Snapshot(ctx, "sdk-child")
	if err != nil {
		t.Fatalf("child SDK snapshot: %v", err)
	}
	if snapshot.SessionID != "sdk-child" || snapshot.Snapshot.AsOfSeq == 0 || len(snapshot.Snapshot.History) == 0 || len(snapshot.Snapshot.Surface) == 0 {
		t.Fatalf("child SDK snapshot projection = %+v", snapshot)
	}
}

// TestSDKRuntimeChildProcess is the real child process used by
// TestSDKClientDrivesRealRuntimeChildThroughIdle. It is intentionally a test
// binary entry point so the regression crosses exec/stdio and exercises the
// production SDK server, not a package-local transport substitute.
func TestSDKRuntimeChildProcess(t *testing.T) {
	if os.Getenv("SHUTU_SDK_RUNTIME_CHILD") != "1" {
		t.Skip("SDK child process")
	}
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "sdk-child.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model := &turnLLM{}
	llmRegistry := llm.NewRegistry()
	if err := llmRegistry.Register(sdkTurnProvider{model: model}); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg: config.Config{
			Model: "test-model",
			Mode:  config.ModeStandard,
			LLM:   config.LLMConfig{Provider: "deepseek-official"},
		},
		baseCtx:       context.Background(),
		store:         st,
		hub:           NewEventHub(),
		agentRegistry: agent.NewRegistry(),
		sessionAgents: make(map[string]*agent.Handle),
		llm:           model,
		llmReg:        llmRegistry,
		reg:           tools.New(),
		prompt:        prompt.New("You are an SDK child integration agent."),
	}
	server := newSDKServer(a, os.Stdin, os.Stdout)
	if err := server.run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type sdkTurnProvider struct{ model *turnLLM }

func (p sdkTurnProvider) ID() string           { return "deepseek-official" }
func (p sdkTurnProvider) Available() bool      { return p.model != nil }
func (p sdkTurnProvider) SupportsImages() bool { return true }
func (p sdkTurnProvider) Stream(ctx context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	return p.model.Stream(ctx, request)
}

func isSDKReceipt(event sdkclient.SessionEvent, messageID string) bool {
	if event.Type != session.EventAgentInboxSpliced {
		return false
	}
	var data struct {
		Inserted []struct {
			ID string `json:"id"`
		} `json:"inserted"`
	}
	if json.Unmarshal(event.Data, &data) != nil {
		return false
	}
	for _, message := range data.Inserted {
		if message.ID == messageID {
			return true
		}
	}
	return false
}

func TestSDKSubagentLifecycleUsesOwningParentWhenEndPayloadOmitsIt(t *testing.T) {
	var out strings.Builder
	s := newSDKServer(nil, strings.NewReader(""), &out)
	raw, _ := json.Marshal(session.NewSubagentEnd("child-1", "spawn", "completed", "answer"))
	s.notifySubagentLifecycle(session.Event{Type: session.EventSubagentEnd, Data: raw}, "parent-1")
	var notification struct {
		Method string `json:"method"`
		Params struct {
			Parent string `json:"parentSessionId"`
			Child  string `json:"childSessionId"`
			Status string `json:"status"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(out.String()), &notification); err != nil {
		t.Fatal(err)
	}
	if notification.Method != "subagent.finished" || notification.Params.Parent != "parent-1" || notification.Params.Child != "child-1" || notification.Params.Status != "ok" {
		t.Fatalf("notification = %#v, want parent fallback and successful status", notification)
	}
}
