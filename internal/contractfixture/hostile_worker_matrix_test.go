package contractfixture_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/code"
	"github.com/jabing/shutu-agent/internal/projection"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

// TestHostileCodeWorkerFaultMatrix models the reference Code Mode rule that
// the worker is a hostile peer. A real Node worker commits one external host
// effect through the production binding bridge, then emits an oversized raw
// protocol frame. The host must settle with one bounded worker-exit receipt,
// never invoke the binding twice, and leave a cold SQLite projection whose only
// tool outcome is the durable failure.
func TestHostileCodeWorkerFaultMatrix(t *testing.T) {
	root := t.TempDir()
	effectPath := filepath.Join(root, "hostile-worker-effect.json")
	dbPath := filepath.Join(root, "hostile-worker.db")
	sessionID := "hostile-code-worker"

	first, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := first.CreateSession(ctx, sessionID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	events := []session.Event{
		{Type: session.EventTurnStart, Data: mustJSON(map[string]any{"turn": 1})},
		{Type: session.EventStepStart, Data: mustJSON(map[string]any{"turn": 1, "step": 1})},
		{Type: session.EventToolCall, Data: mustJSON(map[string]any{
			"turn": 1, "step": 1, "callId": "hostile-code-call", "name": "code", "arguments": `{}`,
		})},
	}
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].At = time.UnixMilli(int64(i + 1)).UTC()
		events[i].Version = session.EventVersion
	}
	if err := first.AppendEvents(ctx, sessionID, events); err != nil {
		t.Fatal(err)
	}
	callSeq := events[len(events)-1].Seq

	var bindingCalls atomic.Int32
	runtime := code.NewTypeScriptRuntime()
	result, runErr := runtime.RunProgram(ctx, code.ProgramRequest{
		Code: `
			await tools.commit({});
			rawStdoutWrite("x".repeat(20 * 1024 * 1024));
			return "must-not-settle";
		`,
		Cwd:       root,
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(_ context.Context, request code.ProgramBindingRequest) (any, error) {
			bindingCalls.Add(1)
			encoded, encodeErr := json.Marshal(map[string]string{
				"callId": request.CallID, "effect": "committed",
			})
			if encodeErr != nil {
				return nil, encodeErr
			}
			file, writeErr := os.OpenFile(effectPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if writeErr != nil {
				return nil, writeErr
			}
			if _, writeErr := file.Write(encoded); writeErr != nil {
				_ = file.Close()
				return nil, writeErr
			}
			if syncErr := file.Sync(); syncErr != nil {
				_ = file.Close()
				return nil, syncErr
			}
			if closeErr := file.Close(); closeErr != nil {
				return nil, closeErr
			}
			return map[string]any{"ok": true}, nil
		},
	})
	if runErr != nil {
		t.Fatalf("hostile worker did not settle as a program outcome: %v", runErr)
	}
	if result.Failure == nil || result.Failure.Kind != code.ProgramFailureWorkerExit {
		t.Fatalf("hostile worker result = %+v, want %s", result, code.ProgramFailureWorkerExit)
	}
	if got := bindingCalls.Load(); got != 1 {
		t.Fatalf("host binding calls = %d, want exactly one committed effect", got)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	effect, err := os.ReadFile(effectPath)
	if err != nil {
		t.Fatalf("read external worker effect: %v", err)
	}
	var committed struct {
		CallID string `json:"callId"`
		Effect string `json:"effect"`
	}
	if json.Unmarshal(effect, &committed) != nil || committed.Effect != "committed" ||
		committed.CallID == "" {
		t.Fatalf("external worker effect = %s", effect)
	}

	resultEvents := []session.Event{
		{
			Seq: uint64(len(events) + 1), Type: session.EventToolResult,
			At: time.UnixMilli(int64(len(events) + 1)).UTC(), Version: session.EventVersion,
			Data: mustJSON(session.NewToolErrorResultAtCodeWithSource(
				1, 1, "hostile-code-call", "code", result.Failure.Message, nil,
				tools.CodeToolExecutionError, callSeq,
			)),
		},
		{
			Seq: uint64(len(events) + 2), Type: session.EventStepEnd,
			At: time.UnixMilli(int64(len(events) + 2)).UTC(), Version: session.EventVersion,
			Data: mustJSON(session.NewStepEndAt(1, 1, "error", "")),
		},
		{
			Seq: uint64(len(events) + 3), Type: session.EventTurnEnd,
			At: time.UnixMilli(int64(len(events) + 3)).UTC(), Version: session.EventVersion,
			Data: mustJSON(session.NewTurnEndAt(1, "error", "")),
		},
	}
	if err := first.AppendEvents(ctx, sessionID, resultEvents); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen after the hostile worker and its host runtime are gone. The only
	// admissible replay outcome is the one terminal failure emitted by the
	// host; an oversized frame or duplicate binding must not invent success.
	second, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	reloaded, err := second.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ValidateLifecycle(reloaded); err != nil {
		t.Fatalf("post-hostile-worker lifecycle invalid: %v", err)
	}
	snapshot, err := projection.Build(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	toolResults := 0
	for _, event := range reloaded {
		if event.Type != session.EventToolResult {
			continue
		}
		toolResults++
		var payload struct {
			CallID string `json:"callId"`
			Output string `json:"output"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(event.Data, &payload) != nil || payload.CallID != "hostile-code-call" ||
			payload.Error == nil || payload.Error.Code != tools.CodeToolExecutionError ||
			payload.Output != result.Failure.Message {
			t.Fatalf("hostile worker durable result = %s", event.Data)
		}
	}
	if toolResults != 1 {
		t.Fatalf("hostile worker durable tool results = %d, want one failure", toolResults)
	}
	if snapshot.AsOfSeq != reloaded[len(reloaded)-1].Seq || len(snapshot.Surface) == 0 {
		t.Fatalf("post-hostile-worker projection = %#v", snapshot)
	}
}
