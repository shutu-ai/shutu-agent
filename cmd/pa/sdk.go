package main

// sdk.go implements the small, stable runtime protocol consumed by external
// Harness clients. It is intentionally separate from ACP: ACP is a host
// interaction protocol, while this protocol exposes durable Agent enqueue
// receipts and session-event/status notifications for a client library.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jabing/shutu-agent/internal/agent"
	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/projection"
	"github.com/jabing/shutu-agent/internal/sdkclient"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/store"
	"github.com/jabing/shutu-agent/internal/tools"
)

const (
	sdkServerName    = "deepseek-harness-sdk-runtime"
	sdkServerVersion = "0.0.1"
)

type sdkRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type sdkResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *sdkRPCError    `json:"error,omitempty"`
}

type sdkRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type sdkContentBlock = sdkclient.ContentBlock
type sdkImageAttachmentRef = sdkclient.ImageAttachmentRef

type sdkSessionRecord struct {
	sessionID     string
	handle        *agent.Handle
	unsub         func()
	watchMu       sync.Mutex
	watching      bool
	forwardMu     sync.Mutex
	forwardedSeq  uint64
	forwardSignal chan struct{}
}

type sdkPendingStatus struct {
	method string
	params any
	record *sdkSessionRecord
	seq    uint64
	armed  bool
}

type sdkServer struct {
	app *app
	in  io.Reader
	out io.Writer

	writeMu          sync.Mutex
	orderMu          sync.Mutex
	statusMu         sync.Mutex
	statusSignal     chan struct{}
	pendingStatuses  map[string][]*sdkPendingStatus
	sessionsMu       sync.Mutex
	createMu         sync.Mutex
	sessions         map[string]*sdkSessionRecord
	allUnsub         func()
	initialized      bool
	closed           bool
	closeOnce        sync.Once
	closeCh          chan struct{}
	shutdownMu       sync.Mutex
	shutdownDone     chan struct{}
	shutdownRun      bool
	shutdownComplete bool
	shutdownErr      *sdkRPCError
	handlers         sync.WaitGroup
	lifecycleMu      sync.Mutex

	cwd       string
	provider  string
	model     string
	maxTokens int
}

func newSDKServer(a *app, in io.Reader, out io.Writer) *sdkServer {
	return &sdkServer{
		app: a, in: in, out: out, sessions: make(map[string]*sdkSessionRecord),
		closeCh: make(chan struct{}), shutdownDone: make(chan struct{}),
		statusSignal: make(chan struct{}), pendingStatuses: make(map[string][]*sdkPendingStatus),
	}
}

func (s *sdkServer) run(ctx context.Context) error {
	if s == nil || s.in == nil || s.out == nil {
		return errors.New("sdk: transport is unavailable")
	}
	// Shutdown closes the server admission channel while the request that
	// initiated it is still finishing. Do not return from the process loop until
	// every already-dispatched handler has settled; otherwise main's deferred
	// lifecycle cleanup can tear down the provider/session owners underneath a
	// racing initialize or prompt handler.
	defer s.handlers.Wait()
	lines := bufio.NewScanner(s.in)
	lines.Buffer(make([]byte, 4096), 4<<20)
	// Preserve JSON-RPC request order at the server boundary. The reference
	// client sends initialize before any session work and shutdown after it, but
	// stdin can deliver both frames before a handler goroutine is scheduled.
	// Without an ordered chain, shutdown could win the scheduler race and tear
	// down the runtime before initialize, producing nondeterministic responses.
	var previousHandler <-chan struct{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closeCh:
			return nil
		default:
		}
		if !lines.Scan() {
			if err := lines.Err(); err != nil {
				return fmt.Errorf("sdk: read request: %w", err)
			}
			return nil
		}
		var request sdkRequest
		if err := json.Unmarshal(lines.Bytes(), &request); err != nil {
			// The SDK line transport treats malformed JSON frames as noise. Do
			// not synthesize a response with a null id: clients cannot correlate
			// it, and the pinned protocol explicitly ignores malformed lines.
			continue
		}
		prior := previousHandler
		completed := make(chan struct{})
		previousHandler = completed
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			defer close(completed)
			if prior != nil {
				<-prior
			}
			s.handle(ctx, request)
		}()
	}
}

func (s *sdkServer) handle(ctx context.Context, request sdkRequest) {
	result, rpcErr, shutdown := s.dispatch(ctx, request.Method, request.Params)
	if len(request.ID) == 0 || string(request.ID) == "null" {
		return
	}
	response := sdkResponse{JSONRPC: "2.0", ID: request.ID, Result: result, Error: rpcErr}
	_ = s.write(response)
	if shutdown {
		s.closeOnce.Do(func() { close(s.closeCh) })
	}
}

func (s *sdkServer) write(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	encoder := json.NewEncoder(s.out)
	return encoder.Encode(value)
}

func (s *sdkServer) notify(method string, params any) {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	s.notifyLocked(method, params)
}

func (s *sdkServer) notifyLocked(method string, params any) {
	_ = s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *sdkServer) reserveForwardStatus(method string, params any, record *sdkSessionRecord) *sdkPendingStatus {
	if record == nil {
		return nil
	}
	ticket := &sdkPendingStatus{method: method, params: params, record: record}
	s.statusMu.Lock()
	s.pendingStatuses[record.sessionID] = append(s.pendingStatuses[record.sessionID], ticket)
	s.signalStatusChangeLocked()
	s.statusMu.Unlock()
	return ticket
}

func (s *sdkServer) armForwardStatus(ticket *sdkPendingStatus, seq uint64) {
	if ticket == nil {
		return
	}
	s.statusMu.Lock()
	ticket.seq = seq
	ticket.armed = true
	s.signalStatusChangeLocked()
	s.statusMu.Unlock()
	s.flushReadyStatuses()
}

func (s *sdkServer) cancelForwardStatus(ticket *sdkPendingStatus) {
	if ticket == nil {
		return
	}
	s.statusMu.Lock()
	queue := s.pendingStatuses[ticket.record.sessionID]
	for i, candidate := range queue {
		if candidate == ticket {
			queue = append(queue[:i], queue[i+1:]...)
			break
		}
	}
	if len(queue) == 0 {
		delete(s.pendingStatuses, ticket.record.sessionID)
	} else {
		s.pendingStatuses[ticket.record.sessionID] = queue
	}
	s.signalStatusChangeLocked()
	s.statusMu.Unlock()
	s.flushReadyStatuses()
}

func (s *sdkServer) signalStatusChangeLocked() {
	if s.statusSignal == nil {
		return
	}
	close(s.statusSignal)
	s.statusSignal = make(chan struct{})
}

// waitStatusReservation prevents an event from overtaking a status that was
// reserved before the operation produced that event. If no status is pending,
// the event is free to proceed immediately.
func (s *sdkServer) waitStatusReservation(sessionID string) bool {
	for {
		s.statusMu.Lock()
		queue := s.pendingStatuses[sessionID]
		if len(queue) == 0 || queue[0].armed {
			s.statusMu.Unlock()
			return true
		}
		signal := s.statusSignal
		s.statusMu.Unlock()
		select {
		case <-signal:
		case <-s.closeCh:
			return false
		}
	}
}

func (s *sdkServer) flushReadyStatuses() {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	s.flushReadyStatusesLocked()
}

func (s *sdkServer) flushReadyStatusesLocked() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	for sessionID, queue := range s.pendingStatuses {
		if len(queue) == 0 {
			delete(s.pendingStatuses, sessionID)
			continue
		}
		record := queue[0].record
		forwarded := uint64(0)
		if record != nil {
			record.forwardMu.Lock()
			forwarded = record.forwardedSeq
			record.forwardMu.Unlock()
		}
		for len(queue) > 0 && queue[0].armed && forwarded >= queue[0].seq {
			ticket := queue[0]
			s.notifyLocked(ticket.method, ticket.params)
			queue = queue[1:]
		}
		if len(queue) == 0 {
			delete(s.pendingStatuses, sessionID)
		} else {
			s.pendingStatuses[sessionID] = queue
		}
	}
}

func (s *sdkServer) waitForward(record *sdkSessionRecord, seq uint64) bool {
	for {
		record.forwardMu.Lock()
		forwarded := record.forwardedSeq
		signal := record.forwardSignal
		record.forwardMu.Unlock()
		if forwarded >= seq {
			return true
		}
		select {
		case <-signal:
		case <-s.closeCh:
			return false
		}
	}
}

func (s *sdkServer) advanceForwardLocked(sessionID string, seq uint64) {
	s.sessionsMu.Lock()
	record := s.sessions[sessionID]
	s.sessionsMu.Unlock()
	if record == nil {
		return
	}
	record.forwardMu.Lock()
	if seq > record.forwardedSeq {
		record.forwardedSeq = seq
	}
	record.forwardMu.Unlock()
	select {
	case record.forwardSignal <- struct{}{}:
	default:
	}
	s.flushReadyStatusesLocked()
}

func (s *sdkServer) advanceForward(sessionID string, seq uint64) {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	s.advanceForwardLocked(sessionID, seq)
}

func (s *sdkServer) lastSessionSeq(id string) uint64 {
	if s.app == nil {
		return 0
	}
	log, err := s.app.sessionLogForAgent(context.Background(), id)
	if err != nil {
		return 0
	}
	events := log.Events()
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

func (s *sdkServer) promptReceiptSeq(id, messageID string) uint64 {
	if s.app == nil {
		return 0
	}
	log, err := s.app.sessionLogForAgent(context.Background(), id)
	if err != nil {
		return 0
	}
	for _, event := range log.Events() {
		if event.Type != session.EventAgentInboxSpliced {
			continue
		}
		var inboxEvent agent.InboxEvent
		if json.Unmarshal(event.Data, &inboxEvent) != nil {
			continue
		}
		for _, message := range inboxEvent.Inserted {
			if message.ID == messageID {
				return event.Seq
			}
		}
	}
	return 0
}

func (s *sdkServer) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, *sdkRPCError, bool) {
	switch method {
	case "initialize":
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		s.sessionsMu.Lock()
		closed := s.closed
		s.sessionsMu.Unlock()
		if closed {
			return nil, sdkInvalid("SDK runtime is closed"), false
		}
		var params struct {
			CWD       string       `json:"cwd"`
			Provider  string       `json:"provider"`
			Model     string       `json:"model"`
			MaxTokens *json.Number `json:"maxTokens"`
		}
		if err := json.Unmarshal(raw, &params); err != nil || strings.TrimSpace(params.CWD) == "" {
			return nil, sdkInvalid("initialize requires cwd"), false
		}
		cwd, err := filepath.Abs(params.CWD)
		if err != nil {
			return nil, sdkInvalid("initialize cwd is invalid"), false
		}
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			return nil, sdkInvalid("initialize cwd must be a directory"), false
		}
		maxTokens := 0
		if params.MaxTokens != nil {
			parsed, parseErr := strconv.ParseInt(string(*params.MaxTokens), 10, 64)
			if parseErr != nil || parsed <= 0 || parsed > (1<<53)-1 || int64(int(parsed)) != parsed {
				return nil, sdkInvalid("initialize maxTokens must be a positive safe integer"), false
			}
			maxTokens = int(parsed)
		}
		provider := strings.TrimSpace(params.Provider)
		model := strings.TrimSpace(params.Model)
		if provider == "" {
			provider = "deepseek-official"
		}
		if model == "" {
			model = "deepseek-v4-flash"
		}
		if s.app == nil || s.app.providerRuntimeSnapshot(provider).selectedID == "" {
			return nil, sdkInternal(fmt.Errorf("no adapter registered for provider %q", provider)), false
		}
		catalog, err := s.toolCatalog()
		if err != nil {
			return nil, sdkInternal(err), false
		}
		s.sessionsMu.Lock()
		if s.initialized {
			s.sessionsMu.Unlock()
			return nil, sdkInvalid("initialize may only be called once"), false
		}
		s.initialized = true
		s.cwd, s.provider, s.model = cwd, provider, model
		s.maxTokens = maxTokens
		s.sessionsMu.Unlock()
		if err := s.ensureAllSubscription(); err != nil {
			s.sessionsMu.Lock()
			s.initialized = false
			s.cwd, s.provider, s.model, s.maxTokens = "", "", "", 0
			s.sessionsMu.Unlock()
			return nil, sdkInternal(err), false
		}
		return map[string]any{"serverInfo": map[string]string{"name": sdkServerName, "version": sdkServerVersion}, "toolCatalog": catalog}, nil, false

	case "session/prompt":
		var params struct {
			SessionID string            `json:"sessionId"`
			Content   []sdkContentBlock `json:"contentBlocks"`
		}
		if err := json.Unmarshal(raw, &params); err != nil || strings.TrimSpace(params.SessionID) == "" || len(params.Content) == 0 {
			return nil, sdkInvalid("session/prompt requires sessionId and contentBlocks"), false
		}
		s.sessionsMu.Lock()
		initialized := s.initialized && !s.closed
		s.sessionsMu.Unlock()
		if !initialized {
			return nil, sdkInvalid("SDK runtime is not initialized"), false
		}
		if err := validateSDKContentBlocks(params.Content); err != nil {
			return nil, sdkInvalid(err.Error()), false
		}
		if sdkHasImageBlock(params.Content) && (s.app == nil || !s.app.multimodalEnabled() || s.app.attachStore == nil || !s.app.llmSupportsImagesForRoute(s.provider, s.model)) {
			return nil, sdkInvalid("image prompt capability is unavailable"), false
		}
		record, err := s.session(ctx, params.SessionID)
		if err != nil {
			return nil, sdkInternal(err), false
		}
		content, err := s.decodeContent(params.Content, params.SessionID)
		if err != nil {
			return nil, sdkInvalid(err.Error()), false
		}
		if err := s.ensureSubscription(params.SessionID, record); err != nil {
			return nil, sdkInternal(err), false
		}
		runningStatus := s.reserveForwardStatus(
			"session.status",
			map[string]any{"sessionId": params.SessionID, "status": "running"},
			record,
		)
		message, err := record.handle.FollowupContentWithID(content, nil)
		if err != nil {
			s.cancelForwardStatus(runningStatus)
			return nil, sdkInternal(err), false
		}
		s.armForwardStatus(runningStatus, s.promptReceiptSeq(params.SessionID, message.ID))
		s.watchIdle(ctx, params.SessionID, record)
		return map[string]string{"messageId": message.ID}, nil, false

	case "session/snapshot":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(raw, &params); err != nil || strings.TrimSpace(params.SessionID) == "" {
			return nil, sdkInvalid("session/snapshot requires sessionId"), false
		}
		s.sessionsMu.Lock()
		initialized := s.initialized && !s.closed
		s.sessionsMu.Unlock()
		if !initialized {
			return nil, sdkInvalid("SDK runtime is not initialized"), false
		}
		if s.app == nil {
			return nil, sdkInternal(errors.New("SDK app runtime is unavailable")), false
		}
		log, err := s.app.sessionLogForAgent(ctx, params.SessionID)
		if err != nil {
			return nil, sdkInternal(err), false
		}
		snapshot, err := projection.Build(log.Events())
		if err != nil {
			return nil, sdkInternal(err), false
		}
		return sdkclient.SessionSnapshot{SessionID: params.SessionID, Snapshot: snapshot}, nil, false

	case "shutdown":
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		return map[string]any{}, s.shutdown(), true
	default:
		return nil, &sdkRPCError{Code: -32601, Message: "unknown SDK runtime method"}, false
	}
}

func sdkInvalid(message string) *sdkRPCError { return &sdkRPCError{Code: -32602, Message: message} }
func sdkInternal(err error) *sdkRPCError {
	if err == nil {
		return nil
	}
	return &sdkRPCError{Code: -32000, Message: err.Error()}
}

func (s *sdkServer) session(ctx context.Context, id string) (*sdkSessionRecord, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	s.sessionsMu.Lock()
	if s.closed || !s.initialized {
		s.sessionsMu.Unlock()
		return nil, errors.New("SDK runtime is not initialized")
	}
	if record := s.sessions[id]; record != nil {
		s.sessionsMu.Unlock()
		return record, nil
	}
	cwd, provider, model, maxTokens := s.cwd, s.provider, s.model, s.maxTokens
	s.sessionsMu.Unlock()
	if s.app == nil || s.app.store == nil {
		return nil, errors.New("SDK app runtime is unavailable")
	}
	created := true
	if _, err := s.app.store.GetSessionMeta(ctx, id); err == nil {
		created = false
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if created {
		if err := s.app.store.CreateSession(ctx, id, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	remove := created
	defer func() {
		if remove {
			_ = s.app.store.DeleteSession(context.Background(), id)
		}
	}()
	if created {
		if err := s.app.setSessionCWD(ctx, id, cwd); err != nil {
			return nil, err
		}
		appCfg := s.app.providerConfigSnapshot()
		effort := effectiveModelReasoningEffort(s.app.modelCapabilityForRoute(provider, model), appCfg.ReasoningEffort)
		if scs, ok := s.app.store.(store.SessionConfigStore); ok {
			if err := scs.SetSessionConfig(ctx, id, store.SessionConfig{AgentPreset: appCfg.Mode, Provider: provider, Model: model, ReasoningEffort: effort}); err != nil {
				return nil, err
			}
		}
	}
	if maxTokens > 0 {
		s.app.runtimeMaxTokensMu.Lock()
		if s.app.runtimeMaxTokens == nil {
			s.app.runtimeMaxTokens = make(map[string]int)
		}
		s.app.runtimeMaxTokens[id] = maxTokens
		s.app.runtimeMaxTokensMu.Unlock()
	}
	if _, err := s.app.sessionLogForAgent(ctx, id); err != nil {
		return nil, err
	}
	handle, err := s.app.sessionAgent(id)
	if err != nil {
		return nil, err
	}
	record := &sdkSessionRecord{sessionID: id, handle: handle, forwardSignal: make(chan struct{}, 1)}
	s.sessionsMu.Lock()
	if existing := s.sessions[id]; existing != nil {
		s.sessionsMu.Unlock()
		_ = handle.Close()
		remove = false
		return existing, nil
	}
	// shutdown seals admission before waiting for createMu. A creation that
	// crossed the initial admission check must still be rejected here; otherwise
	// shutdown can snapshot and dispose the old sessions while this late handle
	// is inserted into the now-closed server and leaked.
	if s.closed {
		s.sessionsMu.Unlock()
		_ = handle.Close()
		if s.app != nil {
			s.app.runtimeMaxTokensMu.Lock()
			delete(s.app.runtimeMaxTokens, id)
			s.app.runtimeMaxTokensMu.Unlock()
		}
		return nil, errors.New("SDK server is shutting down")
	}
	s.sessions[id] = record
	s.sessionsMu.Unlock()
	remove = false
	return record, nil
}

// toolCatalog captures and validates the registry-backed inventory exposed as
// an optional SDK initialize extension.
func (s *sdkServer) toolCatalog() (*sdkclient.ToolCatalog, error) {
	if s.app == nil || s.app.reg == nil {
		return nil, errors.New("tool registry is unavailable")
	}
	manifest, err := s.app.reg.CatalogManifest()
	if err != nil {
		return nil, err
	}
	if err := tools.ValidateCatalogManifest(manifest); err != nil {
		return nil, err
	}
	catalog := &sdkclient.ToolCatalog{
		SchemaVersion: manifest.SchemaVersion,
		Revision:      manifest.Revision,
		Digest:        manifest.Digest,
		Tools:         make([]sdkclient.ToolCatalogEntry, 0, len(manifest.Tools)),
	}
	for _, entry := range manifest.Tools {
		catalog.Tools = append(catalog.Tools, sdkclient.ToolCatalogEntry{
			Name:       entry.Name,
			Profile:    entry.Profile,
			Provenance: entry.Provenance,
			Generation: entry.Registration.Generation,
			Visible:    entry.Visible,
		})
	}
	return catalog, nil
}

func (s *sdkServer) ensureSubscription(id string, record *sdkSessionRecord) error {
	if s.app == nil || s.app.hub == nil {
		return errors.New("SDK event hub is unavailable")
	}
	s.sessionsMu.Lock()
	allSubscribed := s.allUnsub != nil
	s.sessionsMu.Unlock()
	if allSubscribed {
		return nil
	}
	record.watchMu.Lock()
	defer record.watchMu.Unlock()
	if record.unsub != nil {
		return nil
	}
	ch, unsub := s.app.hub.Subscribe(id)
	record.unsub = unsub
	go func() {
		for event := range ch {
			s.notify("session.event", map[string]any{"sessionId": id, "event": sdkWireEvent(event)})
			s.notifySubagentLifecycle(event, id)
		}
	}()
	return nil
}

func (s *sdkServer) ensureAllSubscription() error {
	if s.app == nil || s.app.hub == nil {
		return errors.New("SDK event hub is unavailable")
	}
	s.sessionsMu.Lock()
	if s.allUnsub != nil {
		s.sessionsMu.Unlock()
		return nil
	}
	ch, unsub := s.app.hub.SubscribeAll()
	s.allUnsub = unsub
	s.sessionsMu.Unlock()
	go func() {
		for delivery := range ch {
			if !s.waitStatusReservation(delivery.sessionID) {
				return
			}
			s.orderMu.Lock()
			s.notifyLocked("session.event", map[string]any{"sessionId": delivery.sessionID, "event": sdkWireEvent(delivery.event)})
			s.advanceForwardLocked(delivery.sessionID, delivery.event.Seq)
			s.orderMu.Unlock()
			s.notifySubagentLifecycle(delivery.event, delivery.sessionID)
		}
	}()
	return nil
}

func (s *sdkServer) notifySubagentLifecycle(event session.Event, parentSession string) {
	var data struct {
		ID            string `json:"id"`
		Provider      string `json:"provider"`
		ParentSession string `json:"parentSession"`
		StopReason    string `json:"stopReason"`
		OutputSummary string `json:"outputSummary"`
	}
	if json.Unmarshal(event.Data, &data) != nil || data.ID == "" {
		return
	}
	switch event.Type {
	case session.EventSubagentStart:
		if data.ParentSession == "" {
			data.ParentSession = parentSession
		}
		s.notify("subagent.started", map[string]any{"parentSessionId": data.ParentSession, "childSessionId": data.ID})
	case session.EventSubagentEnd:
		if data.ParentSession == "" {
			data.ParentSession = parentSession
		}
		status := "error"
		if data.StopReason == "completed" || data.StopReason == "max-tokens" {
			status = "ok"
		}
		payload := map[string]any{
			"provider": data.Provider, "agentId": data.ID, "parentSessionId": data.ParentSession,
			"childSessionId": data.ID, "status": status, "stopReason": data.StopReason,
		}
		if data.OutputSummary != "" {
			payload["lastAssistantMessage"] = []map[string]string{{"type": "text", "text": data.OutputSummary}}
		}
		s.notify("subagent.finished", payload)
	}
}

func sdkWireEvent(event session.Event) map[string]any {
	return session.WireEvent(event)
}

func (s *sdkServer) watchIdle(ctx context.Context, id string, record *sdkSessionRecord) {
	_ = ctx // lifecycle is bounded by the server close channel and Agent close.
	record.watchMu.Lock()
	if record.watching {
		record.watchMu.Unlock()
		return
	}
	record.watching = true
	record.watchMu.Unlock()
	go func() {
		defer func() {
			record.watchMu.Lock()
			record.watching = false
			record.watchMu.Unlock()
		}()
		idleStatus := s.reserveForwardStatus(
			"session.status",
			map[string]any{"sessionId": id, "status": "idle"},
			record,
		)
		// WhenIdle observes both the active runner and queued inbox work, so a
		// fast provider cannot race this notification before the prompt is
		// claimed, and queued prompts are not reported idle prematurely.
		_ = record.handle.WhenIdle(context.Background())
		select {
		case <-s.closeCh:
			s.cancelForwardStatus(idleStatus)
			return
		default:
			s.armForwardStatus(idleStatus, s.lastSessionSeq(id))
		}
	}()
}

func validateSDKContentBlocks(blocks []sdkContentBlock) error {
	for _, block := range blocks {
		if block.Type == "" {
			return errors.New("content block type must be non-empty")
		}
		switch block.Type {
		case "text":
			// The reference ContentBlock type permits empty strings; provider
			// admission remains the authority for whether a model accepts it.
		case "reasoning":
		case "image":
			if block.Attachment != nil {
				if block.Attachment.AttachmentID == "" || block.Attachment.MediaType == "" {
					return errors.New("image attachment requires attachmentId and mediaType")
				}
				continue
			}
			if block.MimeType != "image/png" && block.MimeType != "image/jpeg" && block.MimeType != "image/webp" && block.MimeType != "image/gif" {
				return fmt.Errorf("unsupported image mime type %q", block.MimeType)
			}
			data, err := base64.StdEncoding.DecodeString(block.Data)
			if err != nil || base64.StdEncoding.EncodeToString(data) != block.Data || len(data) == 0 {
				return errors.New("image data must be canonical non-empty base64")
			}
		case "tool-call":
			if block.ID == "" || block.Name == "" {
				return errors.New("tool-call block requires id and name")
			}
		case "tool-result":
			if block.ToolCallID == "" {
				return errors.New("tool-result block requires toolCallId")
			}
		default:
			// ContentBlockMap is merge-extensible; preserve unknown entries for
			// a provider/plugin adapter that understands the extension.
			if block.Raw == nil {
				encoded, err := json.Marshal(block)
				if err != nil {
					return fmt.Errorf("preserve content block extension: %w", err)
				}
				block.Raw = encoded
			}
		}
	}
	return nil
}

func sdkHasImageBlock(blocks []sdkContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "image" {
			return true
		}
	}
	return false
}

func (s *sdkServer) decodeContent(blocks []sdkContentBlock, sessionID string) ([]llm.ContentBlock, error) {
	if err := validateSDKContentBlocks(blocks); err != nil {
		return nil, err
	}
	out := make([]llm.ContentBlock, 0, len(blocks))
	images := make([]attachment.ImageInput, 0)
	for _, block := range blocks {
		switch block.Type {
		case "text":
			out = append(out, llm.Text(block.Text))
		case "reasoning":
			out = append(out, llm.ContentBlock{Kind: llm.BlockReasoning, Text: block.Text})
		case "image":
			if block.Attachment != nil {
				if s.app == nil || s.app.attachStore == nil || !s.app.llmSupportsImagesForSession(sessionID) {
					return nil, errors.New("image prompt capability is unavailable")
				}
				ref, err := s.app.attachStore.GetByID(block.Attachment.AttachmentID)
				if err != nil {
					return nil, fmt.Errorf("resolve image prompt attachment: %w", err)
				}
				if ref.MediaType != block.Attachment.MediaType || (block.Attachment.Bytes != 0 && ref.Bytes != block.Attachment.Bytes) {
					return nil, errors.New("image prompt attachment metadata mismatch")
				}
				out = append(out, llm.ContentBlock{Kind: llm.BlockImage, Image: ref})
				continue
			}
			if s.app == nil || !s.app.multimodalEnabled() || s.app.attachStore == nil || !s.app.llmSupportsImagesForSession(sessionID) {
				return nil, errors.New("image prompt capability is unavailable")
			}
			data, err := base64.StdEncoding.DecodeString(block.Data)
			if err != nil {
				return nil, errors.New("image data must be canonical non-empty base64")
			}
			images = append(images, attachment.ImageInput{MediaType: block.MimeType, Data: data})
		case "tool-call":
			out = append(out, llm.ContentBlock{Kind: llm.BlockToolCall, CallID: block.ID, Name: block.Name, Arguments: block.Arguments})
		case "tool-result":
			nested := make([]llm.ContentBlock, 0, len(block.Content))
			for _, child := range block.Content {
				decoded, err := s.decodeContent([]sdkContentBlock{child}, sessionID)
				if err != nil {
					return nil, err
				}
				nested = append(nested, decoded...)
			}
			item := llm.ContentBlock{Kind: llm.BlockToolResult, CallID: block.ToolCallID, Blocks: nested}
			if block.IsError != nil {
				item.IsError = *block.IsError
			}
			out = append(out, item)
		default:
			raw := append(json.RawMessage(nil), block.Raw...)
			if len(raw) == 0 {
				encoded, err := json.Marshal(block)
				if err != nil {
					return nil, err
				}
				raw = encoded
			}
			out = append(out, llm.ContentBlock{Kind: llm.ContentBlockKind(block.Type), Raw: raw})
		}
	}
	if len(images) > 0 {
		maxBytes := s.app.providerConfigSnapshot().LLM.Multimodal.MaxImageBytes
		if maxBytes <= 0 {
			maxBytes = config.DefaultMultimodalMaxImageBytes
		}
		refs, err := s.app.attachStore.SaveImages(images, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("save image prompt: %w", err)
		}
		index := 0
		decodedIndex := 0
		result := make([]llm.ContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if block.Type != "image" || block.Attachment != nil {
				result = append(result, out[decodedIndex])
				decodedIndex++
				continue
			}
			result = append(result, llm.ContentBlock{Kind: llm.BlockImage, Image: refs[index]})
			index++
		}
		return result, nil
	}
	return out, nil
}

func (s *sdkServer) shutdown() *sdkRPCError {
	// The first caller owns shutdown; concurrent callers wait for the same
	// teardown so a second response cannot report success before all Agent
	// handles and provider leases have actually been disposed.
	s.shutdownMu.Lock()
	if s.shutdownDone == nil {
		s.shutdownDone = make(chan struct{})
	}
	if s.shutdownComplete {
		err := s.shutdownErr
		s.shutdownMu.Unlock()
		return err
	}
	if s.shutdownRun {
		done := s.shutdownDone
		s.shutdownMu.Unlock()
		<-done
		s.shutdownMu.Lock()
		err := s.shutdownErr
		s.shutdownMu.Unlock()
		return err
	}
	s.shutdownRun = true
	done := s.shutdownDone
	s.shutdownMu.Unlock()

	// Seal new session admission before waiting for an in-flight creation. The
	// creator holds createMu for its complete publication path and will observe
	// closed before inserting its handle.
	s.sessionsMu.Lock()
	if s.closed {
		s.sessionsMu.Unlock()
		s.shutdownMu.Lock()
		s.shutdownErr = nil
		s.shutdownRun = false
		s.shutdownComplete = true
		close(done)
		s.shutdownMu.Unlock()
		return nil
	}
	s.closed = true
	allUnsub := s.allUnsub
	s.allUnsub = nil
	s.sessionsMu.Unlock()
	s.closeOnce.Do(func() { close(s.closeCh) })

	// Wait until a concurrent session() has either published its handle or
	// rejected it against the closed flag, then take the complete snapshot.
	s.createMu.Lock()
	s.createMu.Unlock()
	s.sessionsMu.Lock()
	records := make([]*sdkSessionRecord, 0, len(s.sessions))
	for _, record := range s.sessions {
		records = append(records, record)
	}
	s.sessions = make(map[string]*sdkSessionRecord)
	s.sessionsMu.Unlock()
	if allUnsub != nil {
		allUnsub()
	}
	var failures []error
	for _, record := range records {
		record.watchMu.Lock()
		if record.unsub != nil {
			record.unsub()
			record.unsub = nil
		}
		record.watchMu.Unlock()
		if record.handle != nil && record.handle.Close() != nil {
			failures = append(failures, errors.New("agent close failed"))
		}
		if record.handle != nil && s.app != nil {
			s.app.runtimeMaxTokensMu.Lock()
			delete(s.app.runtimeMaxTokens, string(record.handle.ID()))
			s.app.runtimeMaxTokensMu.Unlock()
		}
	}
	var result *sdkRPCError
	if len(failures) > 0 {
		result = sdkInternal(errors.Join(failures...))
	}
	s.shutdownMu.Lock()
	s.shutdownErr = result
	s.shutdownRun = false
	s.shutdownComplete = true
	close(done)
	s.shutdownMu.Unlock()
	return result
}
