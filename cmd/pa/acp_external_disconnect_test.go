package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/acp"
	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/contractfixture"
	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
	"github.com/shutu-ai/shutu-agent/internal/store"
	"github.com/shutu-ai/shutu-agent/internal/tools"
)

const acpProductionDisconnectHelperEnv = "SHUTU_ACP_PRODUCTION_DISCONNECT_HELPER"
const acpAttachmentHelperEnv = "SHUTU_ACP_ATTACHMENT_HELPER"

// The child owns the complete production ACP composition root: SQLite,
// acpFactory, the tool registry, the loop, and acp.Server. The parent is only
// an external line-protocol client; it never reaches into child memory.
func TestACPProductionExternalDisconnectHelper(t *testing.T) {
	if os.Getenv(acpProductionDisconnectHelperEnv) != "1" {
		t.Skip("ACP production disconnect helper")
	}
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_DISCONNECT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &app{
		cfg: config.Config{
			Model: "acp-disconnect-model",
			Mode:  config.ModeMinimal,
		},
		store: st,
		llm: &acpDisconnectProvider{
			targetPath:  os.Getenv("SHUTU_ACP_DISCONNECT_FILE"),
			outsidePath: os.Getenv("SHUTU_ACP_DISCONNECT_OUTSIDE"),
			output:      fixture.Tool.Output,
			assistant:   fixture.Assistant,
		},
		reg:        tools.New(),
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	server := &acp.Server{Factory: &acpFactory{app: app}, In: os.Stdin, Out: os.Stdout}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type acpDisconnectProvider struct {
	targetPath  string
	outsidePath string
	output      string
	assistant   string
	calls       atomic.Int32
}

func (p *acpDisconnectProvider) Stream(_ context.Context, _ llm.ChatRequest) (llm.StreamReader, error) {
	switch p.calls.Add(1) {
	case 1:
		arguments, _ := json.Marshal(map[string]any{
			"command":   "create",
			"path":      p.targetPath,
			"file_text": p.output,
		})
		return &acpDisconnectReader{events: []llm.StreamEvent{{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID:        "disconnect-call",
				Name:      "str_replace_editor",
				Arguments: string(arguments),
			}},
		}}}, nil
	case 2:
		arguments, _ := json.Marshal(map[string]any{
			"command":   "create",
			"path":      p.outsidePath,
			"file_text": "must-not-write",
		})
		return &acpDisconnectReader{events: []llm.StreamEvent{{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID: "outside-call", Name: "str_replace_editor", Arguments: string(arguments),
			}},
		}}}, nil
	default:
		return &acpDisconnectReader{events: []llm.StreamEvent{
			{Kind: llm.StreamTextDelta, Text: p.assistant},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}, nil
	}
}

type acpDisconnectReader struct {
	events []llm.StreamEvent
	index  int
}

func (r *acpDisconnectReader) Next() (llm.StreamEvent, error) {
	if r.index >= len(r.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := r.events[r.index]
	r.index++
	return event, nil
}

func TestACPProductionExternalDisconnectPreservesDurableSideEffect(t *testing.T) {
	root := t.TempDir()
	fixture, err := contractfixture.ProtocolLifecycleFixture()
	if err != nil {
		t.Fatal(err)
	}
	var promptBlock struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(fixture.Prompt[0], &promptBlock); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "side-effect.txt")
	db := filepath.Join(root, "pa.db")
	outside := filepath.Join(root, "outside.txt")

	command := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalDisconnectHelper$", "-test.v=false")
	command.Env = append(os.Environ(),
		acpProductionDisconnectHelperEnv+"=1",
		"SHUTU_ACP_DISCONNECT_DB="+db,
		"SHUTU_ACP_DISCONNECT_FILE="+target,
		"SHUTU_ACP_DISCONNECT_OUTSIDE="+outside,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	frames := bufio.NewScanner(stdout)
	frames.Buffer(make([]byte, 4096), 1<<20)
	send := func(id any, method string, params map[string]any) {
		t.Helper()
		frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			frame["params"] = params
		}
		if err := json.NewEncoder(stdin).Encode(frame); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
	}
	readResponse := func(wantID string) map[string]any {
		t.Helper()
		for frames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(frames.Bytes(), &frame); err != nil {
				t.Fatalf("invalid child frame %q: %v", frames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("child stdout ended before response %s: err=%v stderr=%s", wantID, frames.Err(), stderr.String())
		return nil
	}

	send(1, "initialize", map[string]any{})
	if response := readResponse("1"); response["error"] != nil {
		t.Fatalf("initialize = %#v stderr=%s", response, stderr.String())
	}
	send(2, "session/new", map[string]any{"cwd": workspace})
	response := readResponse("2")
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("session/new = %#v stderr=%s", response, stderr.String())
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("session/new result = %#v", result)
	}
	send(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": promptBlock.Text}},
	})
	settled := readResponse("3")
	if settled["error"] != nil {
		t.Fatalf("prompt = %#v stderr=%s", settled, stderr.String())
	}
	stopReason, ok := settled["result"].(map[string]any)["stopReason"].(string)
	if !ok || stopReason != string(acp.StopEndTurn) {
		t.Fatalf("prompt result = %#v", settled["result"])
	}

	// The client disconnect is a real EOF on the child's stdin. Wait for the
	// child process to die, then prove the effect and transcript from separate
	// SQLite/runtime handles; parent-local knowledge is not allowed.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("child exit = %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("child did not exit after client disconnect stderr=%s", stderr.String())
	}

	if content, err := os.ReadFile(target); err != nil || string(content) != fixture.Tool.Output {
		t.Fatalf("post-disconnect side effect = %q, %v", content, err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped write outside workspace = (%#v, %v), want ErrNotExist", outside, err)
	}
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatalf("open independent post-disconnect store: %v", err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load post-disconnect session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("post-disconnect lifecycle invalid: %v", err)
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	for _, typ := range []string{session.EventTurnStart, session.EventTurnEnd, session.EventUserMessage, session.EventAssistantMessage, session.EventToolCall, session.EventToolResult, session.EventFsWrite} {
		if counts[typ] == 0 {
			t.Fatalf("post-disconnect durable events missing %s: %#v", typ, counts)
		}
	}
	var sawToolError bool
	for _, event := range events {
		if event.Type != session.EventToolError {
			continue
		}
		var payload struct {
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && payload.Error != nil {
			sawToolError = true
		}
	}
	if !sawToolError {
		t.Fatal("ACP post-disconnect transcript contains no stable tool/error result")
	}

	parentStore := st
	resumeApp := &app{
		cfg:   config.Config{Model: "acp-disconnect-model", Mode: config.ModeMinimal},
		store: parentStore,
		llm: &acpDisconnectProvider{
			targetPath:  target,
			outsidePath: outside,
			output:      fixture.Tool.Output,
			assistant:   fixture.Assistant,
		},
		reg:        tools.New(),
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	resumedValue, err := (&acpFactory{app: resumeApp}).ResumeSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("resume post-disconnect session: %v", err)
	}
	defer resumedValue.Close()
	resumed := resumedValue.(*acpSession)
	if resumed.SessionID() != sessionID || resumed.cwd != workspace {
		t.Fatalf("resumed identity/cwd = (%q, %q), want (%q, %q)", resumed.SessionID(), resumed.cwd, sessionID, workspace)
	}
	metadata := resumed.ResumeMetadata()
	if metadata["durable"] != true || metadata["cwd"] != workspace || metadata["eventCursor"] != uint64(events[len(events)-1].Seq) {
		t.Fatalf("resume metadata = %#v", metadata)
	}
	snapshot, err := projection.Build(resumed.log.Events())
	if err != nil {
		t.Fatalf("post-disconnect projection: %v", err)
	}
	var userText, assistantText string
	for _, entry := range snapshot.History {
		if entry.Role == llm.RoleUser && userText == "" {
			userText = strings.TrimSpace(entry.Text())
		}
		if entry.Role == llm.RoleAssistant && assistantText == "" {
			assistantText = strings.TrimSpace(entry.Text())
		}
	}
	if userText != strings.TrimSpace(promptBlock.Text) || assistantText != fixture.Assistant {
		t.Fatalf("post-disconnect projection user=%q assistant=%q", userText, assistantText)
	}
}

func frameID(value any) string {
	switch typed := value.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(typed))
	case string:
		return typed
	default:
		return ""
	}
}

// The attachment helper owns the production multimodal ACP route: ACP image
// admission, attachment publication, durable user-message references, Agent
// loop execution, SQLite, and acp.Server.
func TestACPProductionExternalAttachmentHelper(t *testing.T) {
	if os.Getenv(acpAttachmentHelperEnv) != "1" {
		t.Skip("ACP production attachment helper")
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_ATTACHMENT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	attachments, err := attachment.NewStore(os.Getenv("SHUTU_ACP_ATTACHMENT_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	multimodal := true
	provider := &acpAttachmentCaptureProvider{
		observedPath:      os.Getenv("SHUTU_ACP_ATTACHMENT_OBSERVED"),
		postReconnectPath: os.Getenv("SHUTU_ACP_ATTACHMENT_POST_RECONNECT_OBSERVED"),
	}
	app := &app{
		cfg: config.Config{
			Model: "acp-vision-model",
			Mode:  config.ModeMinimal,
			LLM: config.LLMConfig{
				Provider:             "acp-vision-provider",
				ModelInputModalities: "text,image",
				Multimodal:           config.MultimodalConfig{Enabled: &multimodal, MaxImageBytes: 10 * 1024 * 1024},
			},
		},
		customProviders: []customProviderProfile{{
			ID: "acp-vision-provider", Name: "ACP vision fixture", Model: "acp-vision-model",
			Models: []customModel{{
				ID:    "acp-vision-model",
				Input: []string{"text", "image"},
			}},
		}},
		store:       st,
		llmReg:      llm.NewRegistry(),
		attachStore: attachments,
		reg:         tools.New(),
		baseCtx:     context.Background(),
		basePolicy:  tools.DefaultPolicy(),
	}
	if err := app.llmReg.Register(provider); err != nil {
		t.Fatal(err)
	}
	server := &acp.Server{Factory: &acpFactory{app: app}, In: os.Stdin, Out: os.Stdout}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type acpAttachmentCaptureProvider struct {
	observedPath      string
	postReconnectPath string
	wrote             atomic.Bool
	postWrote         atomic.Bool
}

func (p *acpAttachmentCaptureProvider) ID() string      { return "acp-vision-provider" }
func (p *acpAttachmentCaptureProvider) Available() bool { return true }
func (p *acpAttachmentCaptureProvider) SupportsImages() bool {
	return true
}

func (p *acpAttachmentCaptureProvider) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	foundPostReconnectPrompt := false
	for _, message := range request.Messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), "after reconnect") {
			foundPostReconnectPrompt = true
			break
		}
	}
	if foundPostReconnectPrompt {
		if p.postWrote.CompareAndSwap(false, true) {
			observation := acpProviderHistoryObservation{}
			for _, message := range request.Messages {
				item := acpProviderHistoryMessage{Role: string(message.Role), Text: message.Text()}
				for _, block := range message.Content {
					if strings.TrimSpace(block.Text) != "" {
						item.ContentText = append(item.ContentText, block.Text)
					}
					if block.Kind == llm.BlockImage && block.Image.ID != "" {
						item.Images = append(item.Images, block.Image)
					}
				}
				observation.Messages = append(observation.Messages, item)
			}
			raw, err := json.Marshal(observation)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(p.postReconnectPath, raw, 0o600); err != nil {
				return nil, err
			}
		}
		return &acpDisconnectReader{events: []llm.StreamEvent{
			{Kind: llm.StreamTextDelta, Text: "image replayed"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}, nil
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Kind != llm.BlockImage || block.Image.ID == "" || block.Image.Path == "" {
				continue
			}
			encoded, err := json.Marshal(block.Image)
			if err != nil {
				return nil, err
			}
			if p.wrote.CompareAndSwap(false, true) {
				if err := os.WriteFile(p.observedPath, encoded, 0o600); err != nil {
					return nil, err
				}
			}
			return &acpDisconnectReader{events: []llm.StreamEvent{
				{Kind: llm.StreamTextDelta, Text: "saw image"},
				{Kind: llm.StreamFinish, FinishReason: "stop"},
			}}, nil
		}
	}
	// Title generation and other post-turn projections may reuse the route.
	// They are not the attachment admission boundary and must not overwrite the
	// successful provider-request observation.
	return &acpDisconnectReader{events: []llm.StreamEvent{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

func TestACPProductionExternalAttachmentSurvivesDisconnect(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	attachmentDir := filepath.Join(root, "attachments")
	db := filepath.Join(root, "pa.db")
	observed := filepath.Join(root, "provider-observed.json")
	postReconnectObserved := filepath.Join(root, "provider-post-reconnect.json")

	command := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalAttachmentHelper$", "-test.v=false")
	command.Env = append(os.Environ(),
		acpAttachmentHelperEnv+"=1",
		"SHUTU_ACP_ATTACHMENT_DB="+db,
		"SHUTU_ACP_ATTACHMENT_DIR="+attachmentDir,
		"SHUTU_ACP_ATTACHMENT_OBSERVED="+observed,
		"SHUTU_ACP_ATTACHMENT_POST_RECONNECT_OBSERVED="+postReconnectObserved,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	frames := bufio.NewScanner(stdout)
	frames.Buffer(make([]byte, 4096), 1<<20)
	send := func(id any, method string, params map[string]any) {
		t.Helper()
		if err := json.NewEncoder(stdin).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
	}
	readResponse := func(wantID string) map[string]any {
		t.Helper()
		for frames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(frames.Bytes(), &frame); err != nil {
				t.Fatalf("invalid attachment-child frame %q: %v", frames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("attachment child ended before response %s: err=%v stderr=%s", wantID, frames.Err(), stderr.String())
		return nil
	}

	send(1, "initialize", map[string]any{})
	if response := readResponse("1"); response["error"] != nil {
		t.Fatalf("attachment initialize = %#v stderr=%s", response, stderr.String())
	}
	send(2, "session/new", map[string]any{"cwd": workspace})
	response := readResponse("2")
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("attachment session/new = %#v stderr=%s", response, stderr.String())
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("attachment session/new result = %#v", result)
	}
	send(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": "describe this image"},
			{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(attachTestPNG)},
		},
	})
	settled := readResponse("3")
	if settled["error"] != nil {
		t.Fatalf("attachment prompt = %#v stderr=%s", settled, stderr.String())
	}
	stopReason, ok := settled["result"].(map[string]any)["stopReason"].(string)
	if !ok || stopReason != string(acp.StopEndTurn) {
		t.Fatalf("attachment prompt result = %#v", settled["result"])
	}

	// Reconnect through the production ACP server before process exit. The
	// replacement runtime must reload durable state and resolve the image
	// through the shared projection before it re-enters provider history.
	send(4, "session/reconnect", map[string]any{"sessionId": sessionID})
	reconnected := readResponse("4")
	reconnectResult, ok := reconnected["result"].(map[string]any)
	if !ok || reconnectResult["sessionId"] != sessionID {
		t.Fatalf("attachment reconnect = %#v stderr=%s", reconnected, stderr.String())
	}
	send(5, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "after reconnect"}},
	})
	replayed := readResponse("5")
	if replayed["error"] != nil {
		t.Fatalf("post-reconnect image prompt = %#v stderr=%s", replayed, stderr.String())
	}
	replayResult, ok := replayed["result"].(map[string]any)
	if !ok || replayResult["stopReason"] != string(acp.StopEndTurn) {
		t.Fatalf("post-reconnect image prompt result = %#v", replayed["result"])
	}

	rawReplayed, err := os.ReadFile(postReconnectObserved)
	if err != nil {
		t.Fatalf("provider did not observe post-reconnect history: %v", err)
	}
	var replayObservation acpProviderHistoryObservation
	if err := json.Unmarshal(rawReplayed, &replayObservation); err != nil {
		t.Fatalf("decode post-reconnect history: %v", err)
	}
	var sawOriginalImage, sawInitialAssistant, sawReconnectPrompt bool
	for _, message := range replayObservation.Messages {
		switch {
		case message.Role == string(llm.RoleUser) && strings.Contains(message.Text, "describe this image"):
			for _, image := range message.Images {
				if image.ID != "" && image.Path != "" {
					sawOriginalImage = true
				}
			}
		case message.Role == string(llm.RoleAssistant) && strings.Contains(message.Text, "saw image"):
			sawInitialAssistant = true
		case message.Role == string(llm.RoleUser) && strings.Contains(message.Text, "after reconnect"):
			sawReconnectPrompt = true
		}
	}
	if !sawOriginalImage || !sawInitialAssistant || !sawReconnectPrompt {
		t.Fatalf("post-reconnect provider history incomplete: originalImage=%v assistant=%v prompt=%v observation=%s",
			sawOriginalImage, sawInitialAssistant, sawReconnectPrompt, string(rawReplayed))
	}

	// EOF is the external client disconnect. Every assertion below uses state
	// reopened after the production child has exited; no child memory survives.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("attachment child exit = %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("attachment child did not exit stderr=%s", stderr.String())
	}

	var observedRef llm.ImageRef
	rawObserved, err := os.ReadFile(observed)
	if err != nil {
		t.Fatalf("provider did not observe attachment: %v", err)
	}
	if err := json.Unmarshal(rawObserved, &observedRef); err != nil {
		t.Fatalf("decode provider observation: %v", err)
	}
	attachments, err := attachment.NewStore(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	independentRef, err := attachments.GetByID(observedRef.ID)
	if err != nil {
		t.Fatalf("reopen attachment %s: %v", observedRef.ID, err)
	}
	content, err := attachments.Read(independentRef)
	if err != nil {
		t.Fatalf("read reopened attachment: %v", err)
	}
	if !bytes.Equal(content, attachTestPNG) {
		t.Fatalf("reopened attachment bytes differ: got %d want %d", len(content), len(attachTestPNG))
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatalf("open independent attachment store: %v", err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load attachment session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("attachment lifecycle invalid: %v", err)
	}
	resumeApp := &app{
		cfg: config.Config{
			Model: "acp-vision-model", Mode: config.ModeMinimal,
			LLM: config.LLMConfig{
				Provider:             "acp-vision-provider",
				ModelInputModalities: "text,image",
				Multimodal:           config.MultimodalConfig{Enabled: ptrBoolAttachment(true), MaxImageBytes: 10 * 1024 * 1024},
			},
		},
		customProviders: []customProviderProfile{{
			ID: "acp-vision-provider", Name: "ACP vision fixture", Model: "acp-vision-model",
			Models: []customModel{{ID: "acp-vision-model", Input: []string{"text", "image"}}},
		}},
		store:       st,
		attachStore: attachments,
		reg:         tools.New(),
		baseCtx:     context.Background(),
		basePolicy:  tools.DefaultPolicy(),
	}
	resumedValue, err := (&acpFactory{app: resumeApp}).ResumeSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("resume attachment session: %v", err)
	}
	defer resumedValue.Close()
	resumed := resumedValue.(*acpSession)
	metadata := resumed.ResumeMetadata()
	if metadata["durable"] != true || metadata["eventCursor"] != uint64(events[len(events)-1].Seq) {
		t.Fatalf("attachment resume metadata = %#v", metadata)
	}
	snapshot, err := projection.BuildWithImageResolver(events, resumed.log.ImageResolver())
	if err != nil {
		t.Fatalf("attachment projection: %v", err)
	}
	foundImage := false
	for _, message := range snapshot.History {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Kind != llm.BlockImage {
				continue
			}
			if block.Image.ID != observedRef.ID || block.Image.Path != independentRef.Path {
				t.Fatalf("restored image = %#v, want id/path %#v/%q", block.Image, observedRef.ID, independentRef.Path)
			}
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("restored projection has no user image: %#v", snapshot.History)
	}
}

func ptrBoolAttachment(value bool) *bool { return &value }

const acpResourceReplayHelperEnv = "SHUTU_ACP_RESOURCE_REPLAY_HELPER"

type acpProviderHistoryMessage struct {
	Role        string         `json:"role"`
	Text        string         `json:"text"`
	ContentText []string       `json:"contentText"`
	Images      []llm.ImageRef `json:"images,omitempty"`
}

type acpProviderHistoryObservation struct {
	Messages []acpProviderHistoryMessage `json:"messages"`
}

// The resource helper owns the same production ACP/SQLite/loop composition
// root. Its provider publishes the full model request only when the client's
// post-reconnect prompt arrives, so the parent can prove durable rich input
// actually re-entered provider history rather than merely passing admission.
func TestACPProductionExternalResourceReplayHelper(t *testing.T) {
	if os.Getenv(acpResourceReplayHelperEnv) != "1" {
		t.Skip("ACP production resource replay helper")
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_RESOURCE_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &app{
		cfg: config.Config{
			Model: "acp-resource-model",
			Mode:  config.ModeMinimal,
		},
		store:      st,
		llm:        &acpResourceReplayProvider{observedPath: os.Getenv("SHUTU_ACP_RESOURCE_OBSERVED")},
		reg:        tools.New(),
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	server := &acp.Server{Factory: &acpFactory{app: app}, In: os.Stdin, Out: os.Stdout}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type acpResourceReplayProvider struct {
	observedPath string
	providerID   string
	wrote        atomic.Bool
}

func (p *acpResourceReplayProvider) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	foundPostReconnectPrompt := false
	for _, message := range request.Messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), "after reconnect") {
			foundPostReconnectPrompt = true
			break
		}
	}
	if foundPostReconnectPrompt && p.wrote.CompareAndSwap(false, true) {
		observation := acpProviderHistoryObservation{}
		for _, message := range request.Messages {
			item := acpProviderHistoryMessage{Role: string(message.Role), Text: message.Text()}
			for _, block := range message.Content {
				if strings.TrimSpace(block.Text) != "" {
					item.ContentText = append(item.ContentText, block.Text)
				}
				if block.Kind == llm.BlockImage && block.Image.ID != "" {
					item.Images = append(item.Images, block.Image)
				}
			}
			observation.Messages = append(observation.Messages, item)
		}
		raw, err := json.Marshal(observation)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(p.observedPath, raw, 0o600); err != nil {
			return nil, err
		}
	}
	text := "initial"
	if foundPostReconnectPrompt {
		text = "resource replayed"
	}
	return &acpDisconnectReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: text},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

func (p *acpResourceReplayProvider) ID() string {
	if p.providerID != "" {
		return p.providerID
	}
	return "acp-resource-provider"
}
func (*acpResourceReplayProvider) Available() bool { return true }

func TestACPProductionExternalResourceLinkReplaysAfterReconnect(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "pa.db")
	observedPath := filepath.Join(root, "provider-history.json")
	command := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalResourceReplayHelper$", "-test.v=false")
	command.Env = append(os.Environ(),
		acpResourceReplayHelperEnv+"=1",
		"SHUTU_ACP_RESOURCE_DB="+db,
		"SHUTU_ACP_RESOURCE_OBSERVED="+observedPath,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	frames := bufio.NewScanner(stdout)
	frames.Buffer(make([]byte, 4096), 1<<20)
	send := func(id any, method string, params map[string]any) {
		t.Helper()
		if err := json.NewEncoder(stdin).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
	}
	readResponse := func(wantID string) map[string]any {
		t.Helper()
		for frames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(frames.Bytes(), &frame); err != nil {
				t.Fatalf("invalid resource-child frame %q: %v", frames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("resource child ended before response %s: err=%v stderr=%s", wantID, frames.Err(), stderr.String())
		return nil
	}
	send(1, "initialize", map[string]any{})
	if response := readResponse("1"); response["error"] != nil {
		t.Fatalf("resource initialize = %#v stderr=%s", response, stderr.String())
	}
	send(2, "session/new", map[string]any{"cwd": workspace})
	response := readResponse("2")
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("resource session/new = %#v stderr=%s", response, stderr.String())
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("resource session/new result = %#v", result)
	}
	send(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": "inspect this source"},
			{"type": "resource_link", "name": "source", "uri": "file:///workspace/source.go"},
		},
	})
	settled := readResponse("3")
	if settled["error"] != nil {
		t.Fatalf("initial resource prompt = %#v stderr=%s", settled, stderr.String())
	}

	// This is a real same-server replacement through acpFactory.ResumeSession:
	// the old runtime is disposed and the durable transcript is reloaded before
	// the next prompt crosses the provider boundary.
	send(4, "session/reconnect", map[string]any{"sessionId": sessionID})
	reconnected := readResponse("4")
	reconnectResult, ok := reconnected["result"].(map[string]any)
	if !ok || reconnectResult["sessionId"] != sessionID {
		t.Fatalf("resource reconnect = %#v stderr=%s", reconnected, stderr.String())
	}
	send(5, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "after reconnect"}},
	})
	final := readResponse("5")
	if final["error"] != nil {
		t.Fatalf("post-reconnect prompt = %#v stderr=%s", final, stderr.String())
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("resource child exit = %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("resource child did not exit stderr=%s", stderr.String())
	}

	rawObserved, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("provider did not observe post-reconnect history: %v", err)
	}
	var observed acpProviderHistoryObservation
	if err := json.Unmarshal(rawObserved, &observed); err != nil {
		t.Fatalf("decode provider history: %v", err)
	}
	var historyText strings.Builder
	for _, message := range observed.Messages {
		historyText.WriteString("\n")
		historyText.WriteString(message.Text)
		for _, content := range message.ContentText {
			historyText.WriteString("\n")
			historyText.WriteString(content)
		}
	}
	resourceMarker := acp.ResourceLinkText("source", "file:///workspace/source.go")
	for _, want := range []string{
		"inspect this source",
		resourceMarker,
		"initial",
		"after reconnect",
	} {
		if !strings.Contains(historyText.String(), want) {
			t.Fatalf("post-reconnect provider history missing %q: %s", want, historyText.String())
		}
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatalf("open independent resource store: %v", err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load resource session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("resource lifecycle invalid: %v", err)
	}
	resumeApp := &app{
		cfg:        config.Config{Model: "acp-resource-model", Mode: config.ModeMinimal},
		store:      st,
		llm:        &acpResourceReplayProvider{observedPath: observedPath},
		reg:        tools.New(),
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	resumedValue, err := (&acpFactory{app: resumeApp}).ResumeSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("resume resource session: %v", err)
	}
	defer resumedValue.Close()
	resumed := resumedValue.(*acpSession)
	snapshot, err := projection.Build(resumed.log.Events())
	if err != nil {
		t.Fatalf("resource projection: %v", err)
	}
	projectedText := ""
	for _, message := range snapshot.History {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), "inspect this source") {
			projectedText = message.Text()
			break
		}
	}
	if !strings.Contains(projectedText, resourceMarker) {
		t.Fatalf("post-exit projection lost resource marker: %q", projectedText)
	}
}

const acpRichOutputReplayHelperEnv = "SHUTU_ACP_RICH_OUTPUT_REPLAY_HELPER"

type acpRichOutputSeedFactory struct {
	inner     *acpFactory
	ref       llm.ImageRef
	appendWip atomic.Bool
}

func (f *acpRichOutputSeedFactory) ResumeSession(ctx context.Context, id string) (acp.Session, error) {
	return f.inner.ResumeSession(ctx, id)
}

func (f *acpRichOutputSeedFactory) Capabilities(ctx context.Context) map[string]bool {
	return f.inner.Capabilities(ctx)
}

func (f *acpRichOutputSeedFactory) ToolCatalog(ctx context.Context) (acp.ToolCatalog, error) {
	return f.inner.ToolCatalog(ctx)
}

func (f *acpRichOutputSeedFactory) NewSession(ctx context.Context, cwd string) (acp.Session, error) {
	sessionValue, err := f.inner.NewSession(ctx, cwd)
	if err != nil {
		return nil, err
	}
	// Provider streams settle assistant text/reasoning/tool calls. ACP also
	// owns durable rich assistant blocks. Seed the canonical durable shape
	// through the production session sink so this fixture isolates the
	// reconnect/replay boundary rather than provider image generation.
	if f.appendWip.CompareAndSwap(false, true) {
		s := sessionValue.(*acpSession)
		richAssistant := map[string]any{
			"turn": 1, "step": 1, "finishReason": "stop",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "rich output"},
					map[string]any{"type": "image", "attachment": map[string]any{
						"attachmentId": f.ref.ID, "mediaType": f.ref.MediaType, "bytes": f.ref.Bytes,
					}},
				},
			},
		}
		seed := []struct {
			typ  string
			data any
		}{
			{session.EventTurnStart, session.NewTurnStartAt(1)},
			{session.EventStepStart, session.NewStepStartAt(1, 1)},
			{session.EventAssistantMessage, richAssistant},
			{session.EventStepEnd, session.NewStepEndAt(1, 1, "completed", "")},
			{session.EventTurnEnd, session.NewTurnEndAt(1, "completed", "")},
		}
		for _, event := range seed {
			if _, err := s.log.Append(event.typ, event.data); err != nil {
				_ = sessionValue.Close()
				return nil, err
			}
		}
	}
	return sessionValue, nil
}

func TestACPProductionExternalRichOutputReplayHelper(t *testing.T) {
	if os.Getenv(acpRichOutputReplayHelperEnv) != "1" {
		t.Skip("ACP production rich output replay helper")
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_RICH_OUTPUT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	attachments, err := attachment.NewStore(os.Getenv("SHUTU_ACP_RICH_OUTPUT_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := attachments.SaveImage("image/png", attachTestPNG, 10*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	multimodal := true
	provider := &acpResourceReplayProvider{
		observedPath: os.Getenv("SHUTU_ACP_RICH_OUTPUT_OBSERVED"),
		providerID:   "acp-rich-provider",
	}
	app := &app{
		cfg: config.Config{
			Model: "acp-rich-model",
			Mode:  config.ModeMinimal,
			LLM: config.LLMConfig{
				Provider:             "acp-rich-provider",
				ModelInputModalities: "text,image",
				Multimodal:           config.MultimodalConfig{Enabled: &multimodal, MaxImageBytes: 10 * 1024 * 1024},
			},
		},
		customProviders: []customProviderProfile{{
			ID: "acp-rich-provider", Name: "ACP rich fixture", Model: "acp-rich-model",
			Models: []customModel{{ID: "acp-rich-model", Input: []string{"text", "image"}}},
		}},
		store:       st,
		llmReg:      llm.NewRegistry(),
		attachStore: attachments,
		reg:         tools.New(),
		baseCtx:     context.Background(),
		basePolicy:  tools.DefaultPolicy(),
	}
	if err := app.llmReg.Register(provider); err != nil {
		t.Fatal(err)
	}
	server := &acp.Server{
		Factory: &acpRichOutputSeedFactory{inner: &acpFactory{app: app}, ref: ref},
		In:      os.Stdin,
		Out:     os.Stdout,
	}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestACPProductionExternalRichOutputReplaysAfterReconnect(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	attachmentDir := filepath.Join(root, "attachments")
	db := filepath.Join(root, "pa.db")
	observed := filepath.Join(root, "provider-history.json")
	command := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalRichOutputReplayHelper$", "-test.v=false")
	command.Env = append(os.Environ(),
		acpRichOutputReplayHelperEnv+"=1",
		"SHUTU_ACP_RICH_OUTPUT_DB="+db,
		"SHUTU_ACP_RICH_OUTPUT_DIR="+attachmentDir,
		"SHUTU_ACP_RICH_OUTPUT_OBSERVED="+observed,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	frames := bufio.NewScanner(stdout)
	frames.Buffer(make([]byte, 4096), 1<<20)
	send := func(id any, method string, params map[string]any) {
		t.Helper()
		if err := json.NewEncoder(stdin).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
	}
	readResponse := func(wantID string) map[string]any {
		t.Helper()
		for frames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(frames.Bytes(), &frame); err != nil {
				t.Fatalf("invalid rich-output-child frame %q: %v", frames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("rich-output child ended before response %s: err=%v stderr=%s", wantID, frames.Err(), stderr.String())
		return nil
	}

	send(1, "initialize", map[string]any{})
	if response := readResponse("1"); response["error"] != nil {
		t.Fatalf("rich-output initialize = %#v stderr=%s", response, stderr.String())
	}
	send(2, "session/new", map[string]any{"cwd": workspace})
	response := readResponse("2")
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("rich-output session/new = %#v stderr=%s", response, stderr.String())
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("rich-output session/new result = %#v", result)
	}

	send(3, "session/reconnect", map[string]any{"sessionId": sessionID})
	reconnected := readResponse("3")
	reconnectResult, ok := reconnected["result"].(map[string]any)
	if !ok || reconnectResult["sessionId"] != sessionID {
		t.Fatalf("rich-output reconnect = %#v stderr=%s", reconnected, stderr.String())
	}
	send(4, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "after reconnect"}},
	})
	settled := readResponse("4")
	if settled["error"] != nil {
		t.Fatalf("rich-output prompt = %#v stderr=%s", settled, stderr.String())
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("rich-output child exit = %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("rich-output child did not exit stderr=%s", stderr.String())
	}

	rawObserved, err := os.ReadFile(observed)
	if err != nil {
		t.Fatalf("provider did not observe rich-output history: %v", err)
	}
	var history acpProviderHistoryObservation
	if err := json.Unmarshal(rawObserved, &history); err != nil {
		t.Fatalf("decode rich-output history: %v", err)
	}
	var sawRichAssistant bool
	for _, message := range history.Messages {
		if message.Role != string(llm.RoleAssistant) || !strings.Contains(message.Text, "rich output") || len(message.Images) == 0 {
			continue
		}
		for _, image := range message.Images {
			if image.ID != "" && image.Path != "" {
				sawRichAssistant = true
			}
		}
	}
	if !sawRichAssistant {
		t.Fatalf("post-reconnect provider history lost rich assistant output: %s", string(rawObserved))
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatalf("open independent rich-output store: %v", err)
	}
	defer st.Close()
	attachments, err := attachment.NewStore(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load rich-output session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("rich-output lifecycle invalid: %v", err)
	}
	resumeApp := &app{
		cfg: config.Config{
			Model: "acp-rich-model", Mode: config.ModeMinimal,
			LLM: config.LLMConfig{
				Provider:             "acp-rich-provider",
				ModelInputModalities: "text,image",
				Multimodal:           config.MultimodalConfig{Enabled: ptrBoolAttachment(true), MaxImageBytes: 10 * 1024 * 1024},
			},
		},
		customProviders: []customProviderProfile{{
			ID: "acp-rich-provider", Name: "ACP rich fixture", Model: "acp-rich-model",
			Models: []customModel{{ID: "acp-rich-model", Input: []string{"text", "image"}}},
		}},
		store:       st,
		attachStore: attachments,
		reg:         tools.New(),
		baseCtx:     context.Background(),
		basePolicy:  tools.DefaultPolicy(),
	}
	resumedValue, err := (&acpFactory{app: resumeApp}).ResumeSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("resume rich-output session: %v", err)
	}
	defer resumedValue.Close()
	resumed := resumedValue.(*acpSession)
	snapshot, err := projection.BuildWithImageResolver(events, resumed.log.ImageResolver())
	if err != nil {
		t.Fatalf("rich-output projection: %v", err)
	}
	projectedRichOutput := false
	for _, message := range snapshot.History {
		if message.Role != llm.RoleAssistant {
			continue
		}
		hasText, hasResolvedImage := false, false
		for _, block := range message.Content {
			if block.Kind == llm.BlockText && block.Text == "rich output" {
				hasText = true
			}
			if block.Kind == llm.BlockImage && block.Image.Path != "" {
				hasResolvedImage = true
			}
		}
		projectedRichOutput = projectedRichOutput || (hasText && hasResolvedImage)
	}
	if !projectedRichOutput {
		t.Fatalf("post-exit projection lost rich assistant output: %#v", snapshot.History)
	}
}

const acpFaultReplayHelperEnv = "SHUTU_ACP_FAULT_REPLAY_HELPER"

type acpFaultStreamReader struct {
	emitted bool
}

func (r *acpFaultStreamReader) Next() (llm.StreamEvent, error) {
	if !r.emitted {
		r.emitted = true
		return llm.StreamEvent{Kind: llm.StreamTextDelta, Text: "partial before failure"}, nil
	}
	return llm.StreamEvent{}, errors.New("scripted provider stream failure")
}

type acpFaultReplayProvider struct {
	observedPath string
	wrote        atomic.Bool
}

func (p *acpFaultReplayProvider) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	foundFaultPrompt, foundRecoveryPrompt := false, false
	for _, message := range request.Messages {
		if message.Role != llm.RoleUser {
			continue
		}
		text := message.Text()
		if strings.Contains(text, "fail after partial") {
			foundFaultPrompt = true
		}
		if strings.Contains(text, "after provider fault") {
			foundRecoveryPrompt = true
		}
	}
	if foundRecoveryPrompt && p.wrote.CompareAndSwap(false, true) {
		observation := acpProviderHistoryObservation{}
		for _, message := range request.Messages {
			item := acpProviderHistoryMessage{Role: string(message.Role), Text: message.Text()}
			for _, block := range message.Content {
				if strings.TrimSpace(block.Text) != "" {
					item.ContentText = append(item.ContentText, block.Text)
				}
				if block.Kind == llm.BlockImage && block.Image.ID != "" {
					item.Images = append(item.Images, block.Image)
				}
			}
			observation.Messages = append(observation.Messages, item)
		}
		raw, err := json.Marshal(observation)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(p.observedPath, raw, 0o600); err != nil {
			return nil, err
		}
	}
	if foundRecoveryPrompt {
		return &acpDisconnectReader{events: []llm.StreamEvent{
			{Kind: llm.StreamTextDelta, Text: "recovered"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}, nil
	}
	if foundFaultPrompt {
		return &acpFaultStreamReader{}, nil
	}
	return &acpDisconnectReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: "recovered"},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

func TestACPProductionExternalFaultReplayHelper(t *testing.T) {
	if os.Getenv(acpFaultReplayHelperEnv) != "1" {
		t.Skip("ACP production fault replay helper")
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_FAULT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &app{
		cfg:        config.Config{Model: "acp-fault-model", Mode: config.ModeMinimal},
		store:      st,
		llm:        &acpFaultReplayProvider{observedPath: os.Getenv("SHUTU_ACP_FAULT_OBSERVED")},
		reg:        tools.New(),
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	server := &acp.Server{Factory: &acpFactory{app: app}, In: os.Stdin, Out: os.Stdout}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestACPProductionExternalProviderFaultPreservesPrefixAndReplays(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "pa.db")
	observedPath := filepath.Join(root, "provider-history.json")
	command := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalFaultReplayHelper$", "-test.v=false")
	command.Env = append(os.Environ(),
		acpFaultReplayHelperEnv+"=1",
		"SHUTU_ACP_FAULT_DB="+db,
		"SHUTU_ACP_FAULT_OBSERVED="+observedPath,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	frames := bufio.NewScanner(stdout)
	frames.Buffer(make([]byte, 4096), 1<<20)
	send := func(id any, method string, params map[string]any) {
		t.Helper()
		if err := json.NewEncoder(stdin).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
	}
	readResponse := func(wantID string) map[string]any {
		t.Helper()
		for frames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(frames.Bytes(), &frame); err != nil {
				t.Fatalf("invalid fault-child frame %q: %v", frames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("fault child ended before response %s: err=%v stderr=%s", wantID, frames.Err(), stderr.String())
		return nil
	}

	send(1, "initialize", map[string]any{})
	if response := readResponse("1"); response["error"] != nil {
		t.Fatalf("fault initialize = %#v stderr=%s", response, stderr.String())
	}
	send(2, "session/new", map[string]any{"cwd": workspace})
	response := readResponse("2")
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("fault session/new = %#v stderr=%s", response, stderr.String())
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("fault session/new result = %#v", result)
	}
	send(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "fail after partial"}},
	})
	failed := readResponse("3")
	errObj, ok := failed["error"].(map[string]any)
	if !ok || errObj["code"] != float64(-32603) || !strings.Contains(fmt.Sprint(errObj["data"]), "scripted provider stream failure") {
		t.Fatalf("provider fault did not fail at wire = %#v", failed)
	}

	send(4, "session/reconnect", map[string]any{"sessionId": sessionID})
	reconnected := readResponse("4")
	reconnectResult, ok := reconnected["result"].(map[string]any)
	if !ok || reconnectResult["sessionId"] != sessionID {
		t.Fatalf("fault reconnect = %#v stderr=%s", reconnected, stderr.String())
	}
	send(5, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "after provider fault"}},
	})
	recovered := readResponse("5")
	if recovered["error"] != nil {
		t.Fatalf("post-fault prompt = %#v stderr=%s", recovered, stderr.String())
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("fault child exit = %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("fault child did not exit stderr=%s", stderr.String())
	}

	rawObserved, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("provider did not observe post-fault history: %v", err)
	}
	var history acpProviderHistoryObservation
	if err := json.Unmarshal(rawObserved, &history); err != nil {
		t.Fatalf("decode post-fault history: %v", err)
	}
	var sawOriginalPrompt, sawInterruptedPrefix, sawRecoveryPrompt bool
	for _, message := range history.Messages {
		switch {
		case message.Role == string(llm.RoleUser) && strings.Contains(message.Text, "fail after partial"):
			sawOriginalPrompt = true
		case message.Role == string(llm.RoleAssistant) && strings.Contains(message.Text, "partial before failure"):
			sawInterruptedPrefix = true
		case message.Role == string(llm.RoleUser) && strings.Contains(message.Text, "after provider fault"):
			sawRecoveryPrompt = true
		}
	}
	if !sawOriginalPrompt || !sawInterruptedPrefix || !sawRecoveryPrompt {
		t.Fatalf("post-fault provider history incomplete: original=%v prefix=%v recovery=%v observation=%s",
			sawOriginalPrompt, sawInterruptedPrefix, sawRecoveryPrompt, string(rawObserved))
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatalf("open independent fault store: %v", err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load fault session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("fault lifecycle invalid: %v", err)
	}
	var interruptedAssistant bool
	for _, event := range events {
		if event.Type != session.EventAssistantMessage {
			continue
		}
		var payload struct {
			Interrupted bool `json:"interrupted"`
			Message     struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil && payload.Interrupted {
			for _, block := range payload.Message.Content {
				if block.Type == "text" && strings.Contains(block.Text, "partial before failure") {
					interruptedAssistant = true
				}
			}
		}
	}
	if !interruptedAssistant {
		t.Fatalf("durable interrupted assistant anchor missing: %#v", events)
	}
}

const acpOutputLossHelperEnv = "SHUTU_ACP_OUTPUT_LOSS_HELPER"
const acpOutputLossModeEnv = "SHUTU_ACP_OUTPUT_LOSS_MODE"
const acpOutputLossTriggerEnv = "SHUTU_ACP_OUTPUT_LOSS_TRIGGER"

type acpOutputLossProvider struct {
	mode         string
	triggerPath  string
	observedPath string
	wrote        atomic.Bool
}

func (p *acpOutputLossProvider) waitBeforeOutputLoss() error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.triggerPath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("output-loss trigger did not arrive")
}

func (p *acpOutputLossProvider) Stream(_ context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	foundRecoveryPrompt := false
	for _, message := range request.Messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), "after output loss") {
			foundRecoveryPrompt = true
			break
		}
	}
	if foundRecoveryPrompt {
		if p.wrote.CompareAndSwap(false, true) {
			observation := acpProviderHistoryObservation{}
			for _, message := range request.Messages {
				item := acpProviderHistoryMessage{Role: string(message.Role), Text: message.Text()}
				for _, block := range message.Content {
					if strings.TrimSpace(block.Text) != "" {
						item.ContentText = append(item.ContentText, block.Text)
					}
					if block.Kind == llm.BlockImage && block.Image.ID != "" {
						item.Images = append(item.Images, block.Image)
					}
				}
				observation.Messages = append(observation.Messages, item)
			}
			raw, err := json.Marshal(observation)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(p.observedPath, raw, 0o600); err != nil {
				return nil, err
			}
		}
		return &acpDisconnectReader{events: []llm.StreamEvent{
			{Kind: llm.StreamTextDelta, Text: "recovered after output loss"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}, nil
	}
	if p.mode == "output-loss" {
		if err := p.waitBeforeOutputLoss(); err != nil {
			return nil, err
		}
		return &acpDisconnectReader{events: []llm.StreamEvent{
			{Kind: llm.StreamTextDelta, Text: "committed before output loss"},
			{Kind: llm.StreamFinish, FinishReason: "stop"},
		}}, nil
	}
	return &acpDisconnectReader{events: []llm.StreamEvent{
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}, nil
}

func TestACPProductionExternalOutputLossHelper(t *testing.T) {
	if os.Getenv(acpOutputLossHelperEnv) != "1" {
		t.Skip("ACP production output-loss helper")
	}
	ignoreTransportBrokenPipe()
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_OUTPUT_LOSS_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &app{
		cfg:   config.Config{Model: "acp-output-loss-model", Mode: config.ModeMinimal},
		store: st,
		llm: &acpOutputLossProvider{
			mode:         os.Getenv(acpOutputLossModeEnv),
			triggerPath:  os.Getenv(acpOutputLossTriggerEnv),
			observedPath: os.Getenv("SHUTU_ACP_OUTPUT_LOSS_OBSERVED"),
		},
		reg:        tools.New(),
		baseCtx:    context.Background(),
		basePolicy: tools.DefaultPolicy(),
	}
	server := &acp.Server{Factory: &acpFactory{app: app}, In: os.Stdin, Out: os.Stdout}
	if err := server.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestACPProductionExternalOutputLossReconnectsAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "pa.db")
	trigger := filepath.Join(root, "output-loss.trigger")
	observed := filepath.Join(root, "provider-history.json")
	helperArgs := func(mode string) []string {
		return append(os.Environ(),
			acpOutputLossHelperEnv+"=1",
			acpOutputLossModeEnv+"="+mode,
			"SHUTU_ACP_OUTPUT_LOSS_DB="+db,
			acpOutputLossTriggerEnv+"="+trigger,
			"SHUTU_ACP_OUTPUT_LOSS_OBSERVED="+observed,
		)
	}

	// Leg 1 owns a real stdout pipe. After prompt admission, the client breaks
	// the output pipe before the provider emits its committed output.
	first := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalOutputLossHelper$", "-test.v=false")
	first.Env = helperArgs("output-loss")
	firstStdin, err := first.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	firstStdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var firstStderr bytes.Buffer
	first.Stderr = &firstStderr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	firstFrames := bufio.NewScanner(firstStdout)
	firstFrames.Buffer(make([]byte, 4096), 1<<20)
	firstSend := func(id any, method string, params map[string]any) {
		t.Helper()
		if err := json.NewEncoder(firstStdin).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("first output-loss send %s: %v", method, err)
		}
	}
	firstRead := func(wantID string) map[string]any {
		t.Helper()
		for firstFrames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(firstFrames.Bytes(), &frame); err != nil {
				t.Fatalf("first output-loss frame %q: %v", firstFrames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("first output-loss child ended before response %s: err=%v stderr=%s", wantID, firstFrames.Err(), firstStderr.String())
		return nil
	}
	firstSend(1, "initialize", map[string]any{})
	if response := firstRead("1"); response["error"] != nil {
		t.Fatalf("first output-loss initialize = %#v stderr=%s", response, firstStderr.String())
	}
	firstSend(2, "session/new", map[string]any{"cwd": workspace})
	response := firstRead("2")
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("first output-loss session/new = %#v stderr=%s", response, firstStderr.String())
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("first output-loss session/new result = %#v", result)
	}
	firstSend(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "trigger output loss"}},
	})
	if err := firstStdout.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trigger, []byte("break"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDurable := false
	outputDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(outputDeadline) {
		probe, err := store.OpenSQLite(db)
		if err != nil {
			t.Fatalf("open output-loss durability probe: %v", err)
		}
		events, err := probe.LoadSessionRaw(context.Background(), sessionID)
		_ = probe.Close()
		if err != nil {
			t.Fatalf("probe output-loss durability: %v", err)
		}
		for _, event := range events {
			if event.Type == session.EventAssistantMessage {
				outputDurable = true
			}
		}
		if outputDurable {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !outputDurable {
		t.Fatal("provider output did not become durable before stdin disconnect")
	}
	if err := firstStdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- first.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("first output-loss child exit = %v stderr=%s", err, firstStderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("first output-loss child did not exit stderr=%s", firstStderr.String())
	}

	// Leg 2 is a separate process opening the same durable database. It must
	// reconnect to the addressed session and replay the committed output into
	// the recovery provider request.
	second := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalOutputLossHelper$", "-test.v=false")
	second.Env = helperArgs("recovery")
	secondStdin, err := second.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	secondStdout, err := second.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var secondStderr bytes.Buffer
	second.Stderr = &secondStderr
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = secondStdin.Close()
		_ = second.Process.Kill()
		_ = second.Wait()
	}()
	secondFrames := bufio.NewScanner(secondStdout)
	secondFrames.Buffer(make([]byte, 4096), 1<<20)
	secondSend := func(id any, method string, params map[string]any) {
		t.Helper()
		if err := json.NewEncoder(secondStdin).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		}); err != nil {
			t.Fatalf("second output-loss send %s: %v", method, err)
		}
	}
	secondRead := func(wantID string) map[string]any {
		t.Helper()
		for secondFrames.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(secondFrames.Bytes(), &frame); err != nil {
				t.Fatalf("second output-loss frame %q: %v", secondFrames.Bytes(), err)
			}
			if frameID(frame["id"]) != wantID {
				continue
			}
			return frame
		}
		t.Fatalf("second output-loss child ended before response %s: err=%v stderr=%s", wantID, secondFrames.Err(), secondStderr.String())
		return nil
	}
	secondSend(1, "initialize", map[string]any{})
	if response := secondRead("1"); response["error"] != nil {
		t.Fatalf("second output-loss initialize = %#v stderr=%s", response, secondStderr.String())
	}
	secondSend(2, "session/reconnect", map[string]any{"sessionId": sessionID})
	reconnected := secondRead("2")
	reconnectResult, ok := reconnected["result"].(map[string]any)
	if !ok || reconnectResult["sessionId"] != sessionID {
		t.Fatalf("output-loss reconnect = %#v stderr=%s", reconnected, secondStderr.String())
	}
	secondSend(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "after output loss"}},
	})
	recovered := secondRead("3")
	recoverResult, ok := recovered["result"].(map[string]any)
	if !ok || recoverResult["stopReason"] != string(acp.StopEndTurn) {
		t.Fatalf("output-loss recovery prompt = %#v stderr=%s", recovered, secondStderr.String())
	}
	if err := secondStdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone = make(chan error, 1)
	go func() { waitDone <- second.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("second output-loss child exit = %v stderr=%s", err, secondStderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("second output-loss child did not exit stderr=%s", secondStderr.String())
	}

	rawObserved, err := os.ReadFile(observed)
	if err != nil {
		t.Fatalf("recovery provider did not observe history: %v", err)
	}
	var history acpProviderHistoryObservation
	if err := json.Unmarshal(rawObserved, &history); err != nil {
		t.Fatalf("decode recovery history: %v", err)
	}
	var sawLostOutput, sawRecoveryPrompt bool
	for _, message := range history.Messages {
		if message.Role != string(llm.RoleAssistant) {
			continue
		}
		if strings.Contains(message.Text, "committed before output loss") {
			sawLostOutput = true
		}
	}
	for _, message := range history.Messages {
		if message.Role == string(llm.RoleUser) && strings.Contains(message.Text, "after output loss") {
			sawRecoveryPrompt = true
		}
	}
	if !sawLostOutput || !sawRecoveryPrompt {
		t.Fatalf("recovery provider history incomplete: lostOutput=%v recoveryPrompt=%v observation=%s",
			sawLostOutput, sawRecoveryPrompt, string(rawObserved))
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatalf("open independent output-loss store: %v", err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load output-loss session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("output-loss lifecycle invalid: %v", err)
	}
	durableOutput := false
	for _, event := range events {
		if event.Type != session.EventAssistantMessage {
			continue
		}
		var payload struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			continue
		}
		for _, block := range payload.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, "committed before output loss") {
				durableOutput = true
			}
		}
	}
	if !durableOutput {
		t.Fatalf("durable pre-loss assistant output missing: %#v", events)
	}
}
