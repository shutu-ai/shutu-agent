package extension

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ServerCallbacks is the extension-owned implementation behind Protocol v1.
// All callbacks receive the caller's context; cancellation must be honored.
type ServerCallbacks struct {
	Manifest       Manifest
	WebBaseURL     func() string
	Restartable    bool
	DynamicTools   func() []ToolDefinition
	Health         func(context.Context) (HealthResult, error)
	ProvideContext func(context.Context, ContextRequest) (ContextResult, error)
	CallTool       func(context.Context, ToolCallRequest) (ToolCallResult, error)
	OnEvent        func(context.Context, Event) error
}

// Server is a language-neutral protocol server with a Go convenience adapter.
// It never imports Agent internals and can be embedded in any independently
// released executable.
type Server struct {
	callbacks ServerCallbacks
	mu        sync.Mutex
	nextID    uint64
}

func NewServer(callbacks ServerCallbacks) *Server {
	if callbacks.Manifest.ID == "" {
		callbacks.Manifest.ID = "extension"
	}
	return &Server{callbacks: callbacks}
}

// Run serves newline-delimited JSON-RPC until the input stream closes or the
// context is cancelled. Protocol errors are written as responses without
// terminating the process.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if writeErr := s.HandleLine(ctx, line, out); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// HandleLine processes one request or notification. Notifications return nil
// without writing a response.
func (s *Server) HandleLine(ctx context.Context, line []byte, out io.Writer) error {
	var request RPCRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return writeResponse(out, RPCResponse{JSONRPC: "2.0", Error: &RPCError{Code: -32700, Message: "parse error"}})
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return writeResponse(out, RPCResponse{ID: request.ID, Error: &RPCError{Code: -32600, Message: "invalid request"}})
	}
	result, rpcErr := s.invoke(ctx, request)
	if request.ID == 0 {
		return nil
	}
	response := RPCResponse{JSONRPC: "2.0", ID: request.ID}
	if rpcErr != nil {
		response.Error = &RPCError{Code: rpcErr.Code, Message: rpcErr.Message, Data: rpcErr.Data}
	} else if err := json.Unmarshal(result, &response.Result); err != nil {
		response.Error = &RPCError{Code: -32603, Message: "invalid result"}
	}
	return writeResponse(out, response)
}

type protocolError struct {
	Code    int
	Message string
	Data    any
}

func (e protocolError) Error() string { return e.Message }

func (s *Server) invoke(ctx context.Context, request RPCRequest) (json.RawMessage, *protocolError) {
	switch request.Method {
	case MethodInitialize:
		var params InitializeRequest
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, &protocolError{Code: -32602, Message: err.Error()}
		}
		if params.ProtocolVersion != ProtocolVersion {
			return nil, &protocolError{Code: -32001, Message: fmt.Sprintf("protocol mismatch: server supports %s", ProtocolVersion)}
		}
		manifest := s.callbacks.Manifest
		capabilities := manifest.Capabilities
		if !CompatibleAgentAPI(params.AgentAPIVersion, manifest.RequiredAgentAPI) {
			return nil, &protocolError{Code: -32002, Message: fmt.Sprintf("agent API %s does not satisfy required API %s", params.AgentAPIVersion, manifest.RequiredAgentAPI)}
		}
		tools := manifest.Tools.Definitions
		if s.callbacks.DynamicTools != nil {
			tools = append(append([]ToolDefinition(nil), tools...), s.callbacks.DynamicTools()...)
		}
		webURL := manifest.Web.ServiceURL
		if s.callbacks.WebBaseURL != nil {
			webURL = s.callbacks.WebBaseURL()
		}
		result := InitializeResult{
			ProtocolVersion:     ProtocolVersion,
			ExtensionAPIVersion: manifest.ExtensionAPI,
			Capabilities:        capabilities,
			Tools:               tools,
			WebBaseURL:          webURL,
			Restartable:         s.callbacks.Restartable,
		}
		return mustJSON(result), nil
	case MethodShutdown:
		return []byte(`{}`), nil
	case MethodHealth:
		callback := s.callbacks.Health
		if callback == nil {
			return mustJSON(HealthResult{Ready: true, Status: "ready"}), nil
		}
		result, err := callback(ctx)
		if err != nil {
			return nil, &protocolError{Code: -32003, Message: err.Error()}
		}
		return mustJSON(result), nil
	case MethodProvideContext:
		callback := s.callbacks.ProvideContext
		if callback == nil {
			return nil, &protocolError{Code: -32601, Message: "context provider is unavailable"}
		}
		var params ContextRequest
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, &protocolError{Code: -32602, Message: err.Error()}
		}
		result, err := callback(ctx, params)
		if err != nil {
			return nil, &protocolError{Code: -32004, Message: err.Error()}
		}
		return mustJSON(result), nil
	case MethodCallTool:
		callback := s.callbacks.CallTool
		if callback == nil {
			return nil, &protocolError{Code: -32601, Message: "tools are unavailable"}
		}
		var params ToolCallRequest
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, &protocolError{Code: -32602, Message: err.Error()}
		}
		result, err := callback(ctx, params)
		if err != nil {
			return nil, &protocolError{Code: -32005, Message: err.Error()}
		}
		if result.Error != "" {
			return mustJSON(result), nil
		}
		return mustJSON(result), nil
	case MethodEvent:
		callback := s.callbacks.OnEvent
		if callback == nil {
			return []byte(`{}`), nil
		}
		var params Event
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, &protocolError{Code: -32602, Message: err.Error()}
		}
		if err := callback(ctx, params); err != nil {
			return nil, &protocolError{Code: -32006, Message: err.Error()}
		}
		return []byte(`{}`), nil
	default:
		return nil, &protocolError{Code: -32601, Message: "method not found"}
	}
}

func decodeParams(params any, target any) error {
	if params == nil {
		return nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func writeResponse(out io.Writer, response RPCResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = out.Write(append(encoded, '\n'))
	return err
}

// WriteManifest emits YAML without changing the extension's in-memory state.
func WriteManifest(out io.Writer, manifest Manifest) error {
	encoder := yaml.NewEncoder(out)
	if err := encoder.Encode(manifest); err != nil {
		return err
	}
	return encoder.Close()
}
