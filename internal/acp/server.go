// Package acp implements the small, automation-only ACP JSON-RPC surface.
//
// The transport is newline-delimited JSON. Stdout belongs exclusively to ACP
// frames; callers own diagnostics and should write them to stderr.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const ProtocolVersion = 1

type SessionFactory interface {
	NewSession(context.Context, string) (Session, error)
}

type Session interface {
	Prompt(context.Context, string, func(Update)) (StopReason, error)
	Cancel() error
	Close() error
}

type Update struct {
	Text string
}

type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopCancelled StopReason = "cancelled"
)

type Server struct {
	Factory       SessionFactory
	In            io.Reader
	Out           io.Writer
	MaxFrameBytes int
	AgentName     string
	AgentVersion  string

	outMu      sync.Mutex
	sessionsMu sync.Mutex
	sessions   map[string]Session
	nextID     int
	closed     bool
	workers    sync.WaitGroup
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func (s *Server) Run(ctx context.Context) error {
	if s.Factory == nil || s.In == nil || s.Out == nil {
		return errors.New("acp: factory, input and output are required")
	}
	if s.MaxFrameBytes <= 0 {
		s.MaxFrameBytes = 4 << 20
	}
	if s.AgentName == "" {
		s.AgentName = "shutu-agent"
	}
	if s.AgentVersion == "" {
		s.AgentVersion = "dev"
	}
	s.sessions = make(map[string]Session)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	scanner := bufio.NewScanner(s.In)
	scanner.Buffer(make([]byte, 64<<10), s.MaxFrameBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
			_ = s.writeResponse(response{JSONRPC: "2.0", ID: nullID(), Error: &rpcError{Code: -32600, Message: "invalid request"}})
			continue
		}
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			s.handle(runCtx, req)
		}()
	}
	if err := scanner.Err(); err != nil {
		cancel()
		s.cancelSessions()
		s.workers.Wait()
		s.closeSessions()
		return fmt.Errorf("acp: read: %w", err)
	}
	cancel()
	s.cancelSessions()
	s.workers.Wait()
	s.closeSessions()
	return nil
}

func (s *Server) handle(ctx context.Context, req request) {
	result, err, notify := s.dispatch(ctx, req.Method, req.Params)
	if notify || len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}
	if err != nil {
		_ = s.writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: err})
		return
	}
	_ = s.writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, *rpcError, bool) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion":   ProtocolVersion,
			"agentInfo":         map[string]string{"name": s.AgentName, "version": s.AgentVersion},
			"agentCapabilities": map[string]any{"promptCapabilities": map[string]bool{"image": false, "audio": false, "embeddedContext": false}},
			"authMethods":       []any{},
		}, nil, false
	case "authenticate":
		return map[string]any{}, nil, false
	case "session/new":
		var p struct {
			CWD                   string   `json:"cwd"`
			AdditionalDirectories []string `json:"additionalDirectories"`
			MCPServers            []any    `json:"mcpServers"`
		}
		if err := decodeParams(raw, &p); err != nil || p.CWD == "" {
			return nil, invalidParams("session/new requires cwd"), false
		}
		if len(p.AdditionalDirectories) != 0 || len(p.MCPServers) != 0 {
			return nil, invalidParams("additionalDirectories and mcpServers are unsupported"), false
		}
		sess, err := s.Factory.NewSession(ctx, p.CWD)
		if err != nil {
			return nil, internalError(err), false
		}
		s.sessionsMu.Lock()
		if s.closed {
			s.sessionsMu.Unlock()
			_ = sess.Close()
			return nil, internalError(errors.New("server is closed")), false
		}
		s.nextID++
		id := newSessionID(s.nextID)
		s.sessions[id] = sess
		s.sessionsMu.Unlock()
		return map[string]string{"sessionId": id}, nil, false
	case "session/prompt":
		var p struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err := decodeParams(raw, &p); err != nil || p.SessionID == "" || len(p.Prompt) == 0 {
			return nil, invalidParams("session/prompt requires sessionId and prompt"), false
		}
		var text strings.Builder
		for _, block := range p.Prompt {
			if block.Type != "text" || block.Text == "" {
				return nil, invalidParams("only non-empty text prompt blocks are supported"), false
			}
			text.WriteString(block.Text)
		}
		if text.Len() == 0 {
			return nil, invalidParams("prompt text is empty"), false
		}
		sess, ok := s.session(p.SessionID)
		if !ok {
			return nil, invalidParams("unknown session"), false
		}
		stop, err := sess.Prompt(ctx, text.String(), func(u Update) {
			if strings.TrimSpace(u.Text) == "" {
				return
			}
			_ = s.writeNotification(notification{JSONRPC: "2.0", Method: "session/update", Params: map[string]any{
				"sessionId": p.SessionID,
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": u.Text}},
			}})
		})
		if err != nil {
			return nil, internalError(err), false
		}
		if stop == "" {
			stop = StopEndTurn
		}
		return map[string]string{"stopReason": string(stop)}, nil, false
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := decodeParams(raw, &p); err != nil || p.SessionID == "" {
			return nil, invalidParams("session/cancel requires sessionId"), false
		}
		if sess, ok := s.session(p.SessionID); ok {
			if err := sess.Cancel(); err != nil {
				return nil, internalError(err), false
			}
		}
		return nil, nil, true
	case "session/request_permission":
		return nil, invalidParams("permission requests are unsupported"), false
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}, false
	}
}

func (s *Server) session(id string) (Session, bool) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *Server) closeSessions() {
	s.sessionsMu.Lock()
	if s.closed {
		s.sessionsMu.Unlock()
		return
	}
	s.closed = true
	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.Unlock()
	for _, sess := range sessions {
		_ = sess.Close()
	}
}

func (s *Server) cancelSessions() {
	s.sessionsMu.Lock()
	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.Unlock()
	for _, sess := range sessions {
		_ = sess.Cancel()
	}
}

func (s *Server) writeResponse(v response) error {
	return s.write(v)
}

func (s *Server) writeNotification(v notification) error {
	return s.write(v)
}

func (s *Server) write(v any) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.Out, "%s\n", b)
	return err
}

func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("missing params")
	}
	return json.Unmarshal(raw, dst)
}

func invalidParams(message string) *rpcError { return &rpcError{Code: -32602, Message: message} }
func internalError(err error) *rpcError {
	return &rpcError{Code: -32603, Message: "internal error", Data: err.Error()}
}
func nullID() json.RawMessage   { return json.RawMessage("null") }
func newSessionID(n int) string { return fmt.Sprintf("shutu-%d", n) }
