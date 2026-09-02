package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/acp"
	"github.com/jabing/shutu-agent/internal/acpclient"
	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

const acpSecondClientHelperEnv = "SHUTU_ACP_SECOND_CLIENT_HELPER"

type acpSecondClientProvider struct {
	toolPath      string
	imageObserved string
	replayPath    string
	imageSeen     atomic.Bool
	reconnectWip  atomic.Bool
}

func (p *acpSecondClientProvider) ID() string           { return "acp-second-client-provider" }
func (p *acpSecondClientProvider) Available() bool      { return true }
func (p *acpSecondClientProvider) SupportsImages() bool { return true }
func (p *acpSecondClientProvider) Close() error         { return nil }
func (p *acpSecondClientProvider) Stream(ctx context.Context, request llm.ChatRequest) (llm.StreamReader, error) {
	var userText strings.Builder
	hasToolResult := false
	for _, message := range request.Messages {
		if message.Role == llm.RoleUser {
			userText.WriteString("\n")
			userText.WriteString(message.Text())
		}
		if message.Role == llm.RoleTool {
			hasToolResult = true
		}
	}

	switch {
	case strings.Contains(userText.String(), "after second reconnect"):
		if p.reconnectWip.CompareAndSwap(false, true) {
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
			if err := os.WriteFile(p.replayPath, raw, 0o600); err != nil {
				return nil, err
			}
		}
		return successfulReader("second client reconnected"), nil
	case strings.Contains(userText.String(), "wait for second client cancel"):
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, context.DeadlineExceeded
		}
	case strings.Contains(userText.String(), "second client image"):
		for _, message := range request.Messages {
			for _, block := range message.Content {
				if block.Kind == llm.BlockImage && block.Image.ID != "" && p.imageSeen.CompareAndSwap(false, true) {
					raw, err := json.Marshal(block.Image)
					if err != nil {
						return nil, err
					}
					if err := os.WriteFile(p.imageObserved, raw, 0o600); err != nil {
						return nil, err
					}
				}
			}
		}
		return successfulReader("saw second client image"), nil
	case hasToolResult:
		return successfulReader("permission tool completed"), nil
	case strings.Contains(userText.String(), "second client permission"):
		arguments, err := json.Marshal(map[string]any{
			"command":   "create",
			"path":      p.toolPath,
			"file_text": "approved by second client",
		})
		if err != nil {
			return nil, err
		}
		event := llm.StreamEvent{
			Kind:         llm.StreamFinish,
			FinishReason: "tool_calls",
			ToolCalls: []llm.ToolCall{{
				ID:        "second-client-call",
				Name:      "str_replace_editor",
				Arguments: string(arguments),
			}},
		}
		return &acpDisconnectReader{events: []llm.StreamEvent{event}}, nil
	default:
		return successfulReader("unexpected request"), nil
	}
}

func successfulReader(text string) llm.StreamReader {
	return &acpDisconnectReader{events: []llm.StreamEvent{
		{Kind: llm.StreamTextDelta, Text: text},
		{Kind: llm.StreamFinish, FinishReason: "stop"},
	}}
}

func TestACPProductionExternalSecondClientHelper(t *testing.T) {
	if os.Getenv(acpSecondClientHelperEnv) != "1" {
		t.Skip("ACP second client helper")
	}
	st, err := store.OpenSQLite(os.Getenv("SHUTU_ACP_SECOND_CLIENT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	attachments, err := attachment.NewStore(os.Getenv("SHUTU_ACP_SECOND_CLIENT_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	multimodal := true
	provider := &acpSecondClientProvider{
		toolPath:      os.Getenv("SHUTU_ACP_SECOND_CLIENT_TARGET"),
		imageObserved: os.Getenv("SHUTU_ACP_SECOND_CLIENT_IMAGE"),
		replayPath:    os.Getenv("SHUTU_ACP_SECOND_CLIENT_REPLAY"),
	}
	app := &app{
		cfg: config.Config{
			Model: "acp-second-client-model",
			Mode:  config.ModeMinimal,
			LLM: config.LLMConfig{
				Provider:             "acp-second-client-provider",
				ModelInputModalities: "text,image",
				Multimodal:           config.MultimodalConfig{Enabled: &multimodal, MaxImageBytes: 10 * 1024 * 1024},
			},
			Interact: config.InteractConfig{SensitiveTools: []string{"str_replace_editor"}},
		},
		customProviders: []customProviderProfile{{
			ID: "acp-second-client-provider", Name: "ACP second client", Model: "acp-second-client-model",
			Models: []customModel{{ID: "acp-second-client-model", Input: []string{"text", "image"}}},
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

func TestACPProductionExternalSecondClientLifecycle(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "second-client.txt")
	imageRefPath := filepath.Join(root, "image-ref.json")
	replayPath := filepath.Join(root, "replay-history.json")
	db := filepath.Join(root, "pa.db")
	attachmentDir := filepath.Join(root, "attachments")

	command := exec.Command(os.Args[0], "-test.run=^TestACPProductionExternalSecondClientHelper$", "-test.v=false")
	command.Env = append(os.Environ(),
		acpSecondClientHelperEnv+"=1",
		"SHUTU_ACP_SECOND_CLIENT_DB="+db,
		"SHUTU_ACP_SECOND_CLIENT_DIR="+attachmentDir,
		"SHUTU_ACP_SECOND_CLIENT_IMAGE="+imageRefPath,
		"SHUTU_ACP_SECOND_CLIENT_TARGET="+target,
		"SHUTU_ACP_SECOND_CLIENT_REPLAY="+replayPath,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	client := acpclient.New(stdout, stdin)
	if err := client.OnPermission(func(request acpclient.PermissionRequest) (acpclient.PermissionOutcome, error) {
		return acpclient.PermissionOutcome{Outcome: "selected", OptionID: "allow-once"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = client.Close()
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initialized, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("second client initialize: %v stderr=%s", err, stderr.String())
	}
	if initialized["protocolVersion"] != float64(acp.ProtocolVersion) {
		t.Fatalf("second client protocol = %#v", initialized["protocolVersion"])
	}
	authenticated, err := client.Authenticate(ctx)
	if err != nil {
		t.Fatalf("second client authenticate: %v stderr=%s", err, stderr.String())
	}
	if authenticated == nil {
		t.Fatal("second client authenticate returned no result")
	}
	sessionID, err := client.NewSession(ctx, workspace)
	if err != nil {
		t.Fatalf("second client session/new: %v stderr=%s", err, stderr.String())
	}

	var permissionUpdate string
	stop, err := client.Prompt(ctx, sessionID, []acpclient.ContentBlock{
		{Type: "text", Text: "second client permission"},
	}, func(update acpclient.Update) { permissionUpdate = update.Content.Text })
	if err != nil || stop != acpclient.StopEndTurn || permissionUpdate != "permission tool completed" {
		t.Fatalf("second client permission prompt stop=%v err=%v update=%q stderr=%s", stop, err, permissionUpdate, stderr.String())
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "approved by second client" {
		t.Fatalf("permission side effect = %q, %v", content, err)
	}

	encoded := base64.StdEncoding.EncodeToString(attachTestPNG)
	var imageUpdate string
	stop, err = client.Prompt(ctx, sessionID, []acpclient.ContentBlock{
		{Type: "text", Text: "second client image"},
		{Type: "image", MimeType: "image/png", Data: encoded},
	}, func(update acpclient.Update) { imageUpdate = update.Content.Text })
	if err != nil || stop != acpclient.StopEndTurn || imageUpdate != "saw second client image" {
		t.Fatalf("second client image prompt stop=%v err=%v update=%q stderr=%s", stop, err, imageUpdate, stderr.String())
	}
	var observedRef llm.ImageRef
	if raw, err := os.ReadFile(imageRefPath); err != nil {
		t.Fatalf("provider did not observe second-client image: %v", err)
	} else if err := json.Unmarshal(raw, &observedRef); err != nil {
		t.Fatal(err)
	}

	promptDone := make(chan error, 1)
	go func() {
		_, err := client.Prompt(ctx, sessionID, []acpclient.ContentBlock{
			{Type: "text", Text: "wait for second client cancel"},
		}, nil)
		promptDone <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := client.Cancel(sessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-promptDone:
		if err != nil {
			t.Fatalf("second client cancellation prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second client cancellation prompt did not settle")
	}

	if _, err := client.Reconnect(ctx, sessionID); err != nil {
		t.Fatalf("second client reconnect: %v stderr=%s", err, stderr.String())
	}
	var replayUpdate string
	stop, err = client.Prompt(ctx, sessionID, []acpclient.ContentBlock{
		{Type: "text", Text: "after second reconnect"},
	}, func(update acpclient.Update) { replayUpdate = update.Content.Text })
	if err != nil || stop != acpclient.StopEndTurn || replayUpdate != "second client reconnected" {
		t.Fatalf("second client reconnect prompt stop=%v err=%v update=%q stderr=%s", stop, err, replayUpdate, stderr.String())
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("second client child exit = %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("second client child did not exit stderr=%s", stderr.String())
	}

	rawReplay, err := os.ReadFile(replayPath)
	if err != nil {
		t.Fatalf("read replay history: %v", err)
	}
	var replay acpProviderHistoryObservation
	if err := json.Unmarshal(rawReplay, &replay); err != nil {
		t.Fatal(err)
	}
	var sawPermissionTool, sawImage, sawCancelled, sawReconnectPrompt bool
	for _, message := range replay.Messages {
		if message.Role == string(llm.RoleTool) && strings.Contains(message.Text, "New file created successfully") {
			sawPermissionTool = true
		}
		if len(message.Images) != 0 {
			sawImage = true
		}
		if strings.Contains(message.Text, "wait for second client cancel") {
			sawCancelled = true
		}
		if strings.Contains(message.Text, "after second reconnect") {
			sawReconnectPrompt = true
		}
	}
	if !sawPermissionTool || !sawImage || !sawCancelled || !sawReconnectPrompt {
		t.Fatalf("second client replay history incomplete: tool=%v image=%v cancelled=%v prompt=%v observation=%s",
			sawPermissionTool, sawImage, sawCancelled, sawReconnectPrompt, string(rawReplay))
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	events, err := st.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load second-client session: %v", err)
	}
	if err := session.ValidateLifecycle(events); err != nil {
		t.Fatalf("second-client lifecycle invalid: %v", err)
	}
	attachments, err := attachment.NewStore(attachmentDir)
	if err != nil {
		t.Fatal(err)
	}
	independentRef, err := attachments.GetByID(observedRef.ID)
	if err != nil {
		t.Fatalf("reopen second-client attachment: %v", err)
	}
	if content, err := attachments.Read(independentRef); err != nil || !bytes.Equal(content, attachTestPNG) {
		t.Fatalf("second-client attachment round trip changed bytes: %v", err)
	}
}
