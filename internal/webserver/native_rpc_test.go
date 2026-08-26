package webserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

func nativeResponse(t *testing.T, recBody []byte) nativeRPCResponse {
	t.Helper()
	var response nativeRPCResponse
	if err := json.Unmarshal(recBody, &response); err != nil {
		t.Fatalf("decode native response: %v; body=%s", err, recBody)
	}
	return response
}

func TestNativeRPCSessionHistoryAndPrompt(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-session", []session.Event{{
		Seq: 0, Type: session.EventUserMessage, At: time.UnixMilli(1234), Version: session.EventVersion,
		Data: json.RawMessage(`{"text":"hello native"}`),
	}})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"list-1","method":"session.list","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || response.RPCID != "list-1" {
		t.Fatalf("session.list response = %+v", response)
	}
	var list struct {
		Items []nativeSessionListItem `json:"items"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].SessionID != "native-session" || list.Items[0].Blank {
		t.Fatalf("session.list items = %+v", list.Items)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.history", "tok", `{"type":"client-request","rpcId":"history-1","method":"session.history","payload":{"sessionId":"native-session"}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("session.history response = %+v", response)
	}
	encoded, _ = json.Marshal(response.Result.Value)
	var history struct {
		Events []nativeHistoryEntry `json:"events"`
	}
	if err := json.Unmarshal(encoded, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 1 || history.Events[0].Event.Time != 1234 || history.Events[0].Event.Type != session.EventUserMessage {
		t.Fatalf("session.history events = %+v", history.Events)
	}

	var gotSession, gotText string
	srv.SetMessageHandler(func(_ context.Context, sessionID, text string, _ []llm.ImageRef) error {
		gotSession, gotText = sessionID, text
		return nil
	})
	rec = doReqBody(t, srv.Handler(), "POST", "/api/session.prompt", "tok", `{"type":"client-request","rpcId":"prompt-1","method":"session.prompt","payload":{"sessionId":"native-session","mode":"queue","content":[{"type":"text","text":"send me"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK || gotSession != "native-session" || gotText != "send me" {
		t.Fatalf("session.prompt response=%+v callback=(%q,%q)", response, gotSession, gotText)
	}
}

func TestNativeRPCRejectsMethodMismatchAndAuth(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "tok", `{"type":"client-request","rpcId":"bad-1","method":"host.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if response.Result.OK || response.Result.Error == nil || response.Result.Error.Code != "bad-request" {
		t.Fatalf("method mismatch response = %+v", response)
	}
	if rec := doReqBody(t, srv.Handler(), "POST", "/api/session.list", "", `{"type":"client-request","rpcId":"bad-2","method":"session.list","payload":{}}`); rec.Code != 401 {
		t.Fatalf("missing auth status = %d", rec.Code)
	}
}

func TestNativeSettingsDescribeAndMutate(t *testing.T) {
	srv, _ := newTestServer(t, "tok")

	rec := doReqBody(t, srv.Handler(), "POST", "/api/settings.describe", "tok", `{"type":"client-request","rpcId":"settings-1","method":"settings.describe","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.describe response = %+v", response)
	}
	var described struct {
		Namespaces []map[string]any `json:"namespaces"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &described); err != nil {
		t.Fatal(err)
	}
	if len(described.Namespaces) != 1 || described.Namespaces[0]["ns"] != nativeSettingsOnboarding {
		t.Fatalf("settings namespaces = %+v", described.Namespaces)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/settings.mutate", "tok", `{"type":"client-request","rpcId":"settings-2","method":"settings.mutate","payload":{"ns":"ui-onboarding","ops":[{"op":"set","path":["welcomeNoticeVersion"],"value":"2026-08-13.1"}]}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("settings.mutate response = %+v", response)
	}
	var view struct {
		NS       string         `json:"ns"`
		Value    map[string]any `json:"value"`
		Revision int            `json:"revision"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view.NS != nativeSettingsOnboarding || view.Value["welcomeNoticeVersion"] != "2026-08-13.1" || view.Revision != 1 {
		t.Fatalf("settings view = %+v", view)
	}
}

func TestNativeLLMCatalogUsesSanitizedConfig(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetConfigProvider(func() map[string]any {
		return map[string]any{"providers": []map[string]any{
			{
				"id": "deepseek-official", "name": "DeepSeek", "available": true,
				"configured": true, "env_var": "DEEPSEEK_API_KEY", "candidates": []string{"deepseek-v4-flash"},
			},
		}}
	})

	rec := doReqBody(t, srv.Handler(), "POST", "/api/llm.providers", "tok", `{"type":"client-request","rpcId":"llm-1","method":"llm.providers","payload":{}}`)
	response := nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.providers response = %+v", response)
	}
	var providers struct {
		Providers []map[string]any `json:"providers"`
	}
	encoded, _ := json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0]["provider"] != "deepseek-official" {
		t.Fatalf("llm providers = %+v", providers.Providers)
	}

	rec = doReqBody(t, srv.Handler(), "POST", "/api/llm.models", "tok", `{"type":"client-request","rpcId":"llm-2","method":"llm.models","payload":{}}`)
	response = nativeResponse(t, rec.Body.Bytes())
	if !response.Result.OK {
		t.Fatalf("llm.models response = %+v", response)
	}
	var models struct {
		Groups []map[string]any `json:"groups"`
	}
	encoded, _ = json.Marshal(response.Result.Value)
	if err := json.Unmarshal(encoded, &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Groups) != 1 {
		t.Fatalf("llm model groups = %+v", models.Groups)
	}
}

func TestNativeMuxWebSocketSendsSubscriptionBaseline(t *testing.T) {
	srv, st := newTestServer(t, "tok")
	seedSession(t, st, "native-ws", nil)
	srv.SetEventSource(func(_ string, _ func(session.Event)) func() { return func() {} })
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	address := strings.TrimPrefix(httpServer.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := fmt.Sprintf("GET /api/events.mux HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1uYXRpdmUta2V5\r\nAuthorization: Bearer tok\r\n\r\n", address)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	frame, err := readNativeTextFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Payload nativeSubscribedFrame `json:"payload"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Payload.Type != "session/subscribed" || envelope.Payload.SessionID != "native-ws" || envelope.Payload.LastSeq != -1 {
		t.Fatalf("subscription frame = %s", frame)
	}
}

func readNativeTextFrame(reader *bufio.Reader) ([]byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if first&0x0f != 1 {
		return nil, fmt.Errorf("opcode = %d", first&0x0f)
	}
	second, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	length := int(second & 0x7f)
	if length == 126 {
		var bytesLength [2]byte
		if _, err := io.ReadFull(reader, bytesLength[:]); err != nil {
			return nil, err
		}
		length = int(bytesLength[0])<<8 | int(bytesLength[1])
	} else if length == 127 {
		return nil, fmt.Errorf("unexpected large frame")
	}
	if second&0x80 != 0 {
		return nil, fmt.Errorf("server frame is masked")
	}
	payload := make([]byte, length)
	_, err = io.ReadFull(reader, payload)
	return payload, err
}
