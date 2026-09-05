package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/attachment"
	"github.com/shutu-ai/shutu-agent/internal/config"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// attachTestPNG is a tiny fake PNG payload (the store does not decode — bytes
// only matter for size and round-trip equality, M8 裁剪).
var attachTestPNG = func() []byte {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	return data
}()

// makeAttachApp builds a minimal app for /attach wiring tests: a multimodal
// config (enabled or not), a fresh log, and — when enabled — a real attachment
// store in a temp dir (dispatch-m8-3 §4: disabled ⇒ no store is created).
func makeAttachApp(t *testing.T, enabled bool) *app {
	t.Helper()
	mmEnabled := enabled
	a := &app{
		cfg: config.Config{
			LLM: config.LLMConfig{
				Multimodal: config.MultimodalConfig{Enabled: &mmEnabled, MaxImageBytes: 10 * 1024 * 1024},
			},
		},
		log: session.New(),
	}
	if enabled {
		st, err := attachment.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		a.attachStore = st
	}
	return a
}

// TestAttachCommandDisabled verifies the D10 gate (dispatch-m8-3 §4/§5): with
// llm.multimodal.enabled=false /attach errors with the disabled message and
// logs nothing — even when a valid image path is given.
func TestAttachCommandDisabled(t *testing.T) {
	a := makeAttachApp(t, false)
	if err := a.attachCommand(context.Background(), []string{"x.png"}); err == nil {
		t.Fatal("/attach must fail when multimodal disabled")
	} else if !strings.Contains(err.Error(), "multimodal disabled (llm.multimodal.enabled=false)") {
		t.Errorf("err = %q, want the disabled message", err)
	}
	if n := len(a.log.Events()); n != 0 {
		t.Fatalf("disabled /attach logged %d events, want none", n)
	}
}

// TestAttachCommandRequiresPath verifies the usage gate.
func TestAttachCommandRequiresPath(t *testing.T) {
	a := makeAttachApp(t, true)
	if err := a.attachCommand(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "usage: /attach") {
		t.Fatalf("err = %v, want the usage error", err)
	}
}

// TestAttachCommandEnabledLogsImageRef verifies the enabled happy path
// (dispatch-m8-3 §4/§5): a valid PNG is stored, exactly one user/message event
// is appended whose content carries the image block with only the ImageRef
// (no bytes, no base64), width/height are 0, and the printed hint reports the
// attachment id and byte size.
func TestAttachCommandEnabledLogsImageRef(t *testing.T) {
	a := makeAttachApp(t, true)
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, attachTestPNG, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out := captureStdout(func() {
		if err := a.attachCommand(context.Background(), []string{path}); err != nil {
			t.Fatalf("attachCommand: %v", err)
		}
	})
	// The hint prints ref metadata only: attached <path> as image <id> (png, N bytes).
	if !strings.Contains(out, "attached "+path+" as image ") ||
		!strings.Contains(out, "(png, "+strconv.Itoa(len(attachTestPNG))+" bytes)") {
		t.Errorf("hint = %q, want 'attached <path> as image <id> (png, N bytes)'", out)
	}

	// Exactly one user/message event.
	evs := a.log.Events()
	if len(evs) != 1 || evs[0].Type != session.EventUserMessage {
		t.Fatalf("events = %+v, want one user/message", evs)
	}
	// The event data carries the image block with only the ImageRef — never the
	// bytes (dsh 7078918: 落库只存引用).
	raw := string(evs[0].Data)
	if strings.Contains(raw, base64.StdEncoding.EncodeToString(attachTestPNG)) {
		t.Fatalf("user/message payload must not serialize image bytes: %s", raw)
	}
	var data struct {
		Content []struct {
			Type       string `json:"type"`
			Attachment struct {
				AttachmentID string `json:"attachmentId"`
				MediaType    string `json:"mediaType"`
				Bytes        int64  `json:"bytes"`
				Width        int    `json:"width"`
				Height       int    `json:"height"`
			}
		}
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("unmarshal event data: %v", err)
	}
	if len(data.Content) != 1 {
		t.Fatalf("content = %+v, want one block", data.Content)
	}
	img := data.Content[0]
	if img.Type != "image" {
		t.Errorf("type = %q, want image", img.Type)
	}
	if img.Attachment.AttachmentID == "" || img.Attachment.MediaType != "image/png" ||
		img.Attachment.Bytes != int64(len(attachTestPNG)) || img.Attachment.Width != 1 || img.Attachment.Height != 1 {
		t.Errorf("canonical image ref = %+v, want attachmentId/mediaType/bytes set", img.Attachment)
	}
}

// TestAttachCommandRejectsBadExtension verifies fail-closed on a file whose
// extension is not in SupportedMediaTypes (dispatch-m8-3 §4 步骤 3/§5).
func TestAttachCommandRejectsBadExtension(t *testing.T) {
	a := makeAttachApp(t, true)
	path := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(path, []byte("not an image"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := a.attachCommand(context.Background(), []string{path}); err == nil {
		t.Fatal("bad extension must fail closed")
	} else if !strings.Contains(err.Error(), "unsupported image type") {
		t.Errorf("err = %q, want the unsupported-type error", err)
	}
	if n := len(a.log.Events()); n != 0 {
		t.Fatalf("rejected /attach logged %d events, want none", n)
	}
}

// TestAttachCommandRejectsTooLarge verifies fail-closed when the file exceeds
// llm.multimodal.max_image_bytes (dispatch-m8-3 §4 步骤 3/§5: 超限 fail-closed).
func TestAttachCommandRejectsTooLarge(t *testing.T) {
	a := makeAttachApp(t, true)
	a.cfg.LLM.Multimodal.MaxImageBytes = 8 // smaller than the fixture (16 bytes)
	path := filepath.Join(t.TempDir(), "big.png")
	if err := os.WriteFile(path, attachTestPNG, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := a.attachCommand(context.Background(), []string{path}); err == nil {
		t.Fatal("over-limit image must fail closed")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %q, want the too-large error", err)
	}
	if n := len(a.log.Events()); n != 0 {
		t.Fatalf("rejected /attach logged %d events, want none", n)
	}
}

// TestAttachCommandMissingFile verifies fail-closed when the path does not
// exist (dispatch-m8-3 §4 步骤 2).
func TestAttachCommandMissingFile(t *testing.T) {
	a := makeAttachApp(t, true)
	if err := a.attachCommand(context.Background(), []string{filepath.Join(t.TempDir(), "nope.png")}); err == nil {
		t.Fatal("missing file must fail closed")
	} else if !strings.Contains(err.Error(), "read") {
		t.Errorf("err = %q, want the read error", err)
	}
	if n := len(a.log.Events()); n != 0 {
		t.Fatalf("failed /attach logged %d events, want none", n)
	}
}

// TestAttachCommandViaDispatcher verifies the /attach case is wired into the
// command dispatcher (dispatch-m8-3 §4: command switch 增加).
func TestAttachCommandViaDispatcher(t *testing.T) {
	a := makeAttachApp(t, true)
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, attachTestPNG, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out := captureStdout(func() {
		if err := a.command(context.Background(), "/attach "+path); err != nil {
			t.Fatalf("command: %v", err)
		}
	})
	if !strings.Contains(out, "attached "+path+" as image ") {
		t.Errorf("hint = %q, want the attached hint", out)
	}
	events := a.log.Events()
	if len(events) != 3 || events[0].Type != session.EventCommandRun || events[1].Type != session.EventUserMessage || events[2].Type != session.EventCommandDone {
		t.Fatalf("events = %+v, want command/run + user/message + command/done", events)
	}
}
