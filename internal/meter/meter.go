// Package meter provides detached request-pressure measurements. It is a
// conservative heuristic layer: provider usage is an optional baseline and
// never replaces pricing of the current replayed surface.
package meter

import (
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/projection"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

type Baseline struct {
	Kind  string // estimated | usage
	Total int
	Usage llm.TokenUsage
}

type SurfaceNode struct {
	Seq    uint64
	Tokens int
}

type Measurement struct {
	LogRevision uint64
	Baseline    Baseline
	// These explicit projections make the accounting auditable by callers that
	// need to distinguish a provider baseline from the replayed surface delta.
	BaselineEstimatedTokens int
	BaselineUsageTokens     int
	SurfaceDeltaTokens      int
	TotalTokens             int
	SurfaceTokens           int
	Nodes                   []SurfaceNode
	// Completion is the replayable output-side ledger. It is kept alongside
	// pressure rather than inferred by a UI so restored, live and native
	// projections all expose the same committed boundaries.
	Completion CompletionProjection
}

type usageAnchor struct {
	fingerprint string
	request     llm.ChatRequest
	total       int
	usage       llm.TokenUsage
	heuristic   int
	surface     int
	hasSurface  bool
}

type Meter struct {
	mu      sync.Mutex
	anchors map[string]usageAnchor
}

func New() *Meter { return &Meter{anchors: make(map[string]usageAnchor)} }

// RecordSuccessfulUsage saves a provider usage anchor for a session. The
// request fingerprint includes route and model-visible message shape, so a
// later request never reuses usage priced for a different envelope.
func (m *Meter) RecordSuccessfulUsage(sessionID string, request llm.ChatRequest, usage llm.TokenUsage) {
	m.recordSuccessfulUsage(sessionID, request, usage, nil)
}

// RecordSuccessfulUsageAt records the same provider anchor together with the
// exact model-visible surface at the successful assistant/message boundary.
// This is the replay-aware form used by the loop; the legacy method remains a
// useful standalone seam for callers that do not own a session log.
func (m *Meter) RecordSuccessfulUsageAt(sessionID string, request llm.ChatRequest, usage llm.TokenUsage, log *session.Log) {
	m.recordSuccessfulUsage(sessionID, request, usage, log)
}

func (m *Meter) recordSuccessfulUsage(sessionID string, request llm.ChatRequest, usage llm.TokenUsage, log *session.Log) {
	if m == nil || sessionID == "" {
		return
	}
	if usage.Empty() {
		// A successful call without provider accounting is an explicit
		// replacement of the previous anchor in the reference replay fold.
		m.mu.Lock()
		delete(m.anchors, sessionID)
		m.mu.Unlock()
		return
	}
	heuristic := estimateRequest(request)
	anchor := usageAnchor{fingerprint: fingerprint(request), request: cloneChatRequest(request), total: usageTotal(usage), usage: usage, heuristic: heuristic}
	if log != nil {
		anchor.surface = surfaceTokens(log)
		if surface, ok := latestAssistantAnchorSurface(log.Events()); ok {
			anchor.surface = surface
		}
		anchor.hasSurface = true
		// Usage covers the complete canonical request: header plus the
		// model-visible surface at the successful assistant boundary.
		heuristic += anchor.surface
	}
	m.mu.Lock()
	m.anchors[sessionID] = anchor
	m.mu.Unlock()
}

func (m *Meter) Measure(sessionID string, log *session.Log, request *llm.ChatRequest) Measurement {
	measurement := Measurement{Baseline: Baseline{Kind: "estimated"}}
	if log == nil {
		return measurement
	}
	events := log.Events()
	measurement.Completion = ProjectCompletion(events)
	if next := log.NextSeq(); next > 0 {
		measurement.LogRevision = next - 1
	}
	snapshot, err := projection.Build(events)
	if err != nil {
		// Metering is advisory, but it must not invent a surface for an invalid
		// durable stream. The caller can still display the completion ledger;
		// request admission remains responsible for reporting the projection
		// failure through its own strict path.
		return measurement
	}
	history := snapshot.History
	for _, entry := range snapshot.Surface {
		measurement.Nodes = append(measurement.Nodes, SurfaceNode{Seq: entry.Seq, Tokens: EstimateMessage(entry.Message)})
	}
	for _, node := range measurement.Nodes {
		measurement.SurfaceTokens += node.Tokens
	}
	// Legacy logs whose event sequence is absent still expose the detached
	// history as positional diagnostics; this does not affect the folded total.
	if len(measurement.Nodes) == 0 {
		for index, message := range history {
			measurement.Nodes = append(measurement.Nodes, SurfaceNode{Seq: uint64(index + 1), Tokens: EstimateMessage(message)})
		}
	}
	// The pressure pre-step runs before the next request has been assembled.
	// Reuse the latest successful canonical request in that case; otherwise the
	// pre-step would see only surface tokens and lose the provider baseline as
	// soon as the previous turn completed.
	effectiveRequest := request
	var anchored usageAnchor
	var haveAnchor bool
	if m != nil {
		m.mu.Lock()
		anchored, haveAnchor = m.anchors[sessionID]
		m.mu.Unlock()
		// A durable assistant/message usage field is the replay source of
		// truth. It restores the last provider anchor after process restart,
		// when the detached in-memory callback map is empty.
		if !haveAnchor {
			anchored, haveAnchor = durableUsageAnchor(events)
		}
		if effectiveRequest == nil && haveAnchor {
			effectiveRequest = &anchored.request
		}
		if effectiveRequest == nil {
			// Before the next request is assembled, retain the latest durable
			// system/tool header instead of silently dropping header pressure.
			effectiveRequest = latestHeaderRequest(events)
		}
	}
	requestTokens := 0
	if effectiveRequest != nil {
		requestTokens = estimateRequest(*effectiveRequest)
	}
	measurement.TotalTokens = requestTokens + measurement.SurfaceTokens
	if haveAnchor && effectiveRequest != nil && anchored.fingerprint == fingerprint(*effectiveRequest) && anchored.total >= anchored.heuristic {
		measurement.Baseline = Baseline{Kind: "usage", Total: anchored.total, Usage: anchored.usage}
		measurement.BaselineUsageTokens = anchored.total
		baselineSurface := 0
		if anchored.hasSurface {
			baselineSurface = anchored.surface
		}
		measurement.TotalTokens = anchored.total + (measurement.SurfaceTokens - baselineSurface)
		measurement.SurfaceDeltaTokens = measurement.SurfaceTokens - baselineSurface
	}
	if measurement.Baseline.Kind == "estimated" {
		measurement.BaselineEstimatedTokens = measurement.TotalTokens
	}
	if effectiveRequest == nil && !haveAnchor && measurement.SurfaceTokens == 0 {
		measurement.Baseline = Baseline{Kind: "none"}
		measurement.BaselineEstimatedTokens = 0
	}
	if measurement.TotalTokens < 0 {
		measurement.TotalTokens = 0
	}
	return measurement
}

// usageTotal follows the reference's disjoint bucket accounting. Older Go
// adapters sometimes populated only TotalTokens, so retain that legacy shape
// when no component buckets are available.
func usageTotal(usage llm.TokenUsage) int {
	// ReasoningTokens is a diagnostic subdivision of output, not another
	// disjoint billing bucket. Count it only through OutputTokens, matching the
	// reference's input + cache-read/write + output projection.
	buckets := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	if usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
		// Old durable events used one aggregate cache field. Read it only when
		// the explicit buckets are absent, avoiding a double count during the
		// migration window.
		buckets += usage.CachedInputTokens
	}
	if buckets > 0 {
		return buckets
	}
	return usage.TotalTokens
}

// durableUsageAnchor reconstructs the latest replayable provider sample from
// assistant/message. The session event is intentionally enough to recover
// pressure accounting without a live Meter callback or process-local state.
func durableUsageAnchor(events []session.Event) (usageAnchor, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != session.EventAssistantMessage {
			continue
		}
		var payload struct {
			Usage *llm.TokenUsage `json:"usage"`
		}
		if json.Unmarshal(events[i].Data, &payload) != nil || payload.Usage == nil || payload.Usage.Empty() {
			// A later assistant boundary may legitimately omit provider usage
			// (for example a compatibility/mock response). Keep walking so a
			// restart can still recover the nearest earlier durable anchor.
			continue
		}
		request := latestHeaderRequestBefore(events, i)
		if request == nil {
			continue
		}
		surface := surfaceTokensThrough(events[:i+1])
		if anchoredSurface, ok := assistantAnchorSurface(events, i); ok {
			surface = anchoredSurface
		}
		heuristic := estimateRequest(*request) + surface
		return usageAnchor{
			fingerprint: fingerprint(*request),
			request:     cloneChatRequest(*request),
			total:       usageTotal(*payload.Usage),
			usage:       *payload.Usage,
			heuristic:   heuristic,
			surface:     surface,
			hasSurface:  true,
		}, true
	}
	return usageAnchor{}, false
}

func latestAssistantAnchorSurface(events []session.Event) (int, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != session.EventAssistantMessage {
			continue
		}
		if _, _, _, ok := usageSample(events[i]); !ok {
			continue
		}
		return assistantAnchorSurface(events, i)
	}
	return 0, false
}

// assistantAnchorSurface returns the surface visible at the provider usage
// boundary. DSH anchors before the closing assistant/message joins the surface;
// when provenance is present, only the cited provider chunks are priced. This
// matters when a durable assistant row is rewritten or contains normalized
// text that was not emitted by the provider.
func assistantAnchorSurface(events []session.Event, index int) (int, bool) {
	if index < 0 || index >= len(events) || events[index].Type != session.EventAssistantMessage {
		return 0, false
	}
	before := surfaceTokensThrough(events[:index])
	sources, present := session.AssistantSourceEventSeqs(events[index])
	if !present {
		return surfaceTokensThrough(events[:index+1]), true
	}
	provider, ok := estimateProviderAssistant(events, events[index], sources)
	if !ok {
		return surfaceTokensThrough(events[:index+1]), true
	}
	return before + provider, true
}

func estimateProviderAssistant(events []session.Event, event session.Event, sources []uint64) (int, bool) {
	message, ok := session.DeriveEventMessage(event)
	if !ok {
		return 0, false
	}
	var content []llm.ContentBlock
	appendBlock := func(kind llm.ContentBlockKind, text string) {
		if text == "" {
			return
		}
		if len(content) > 0 && content[len(content)-1].Kind == kind {
			content[len(content)-1].Text += text
			return
		}
		content = append(content, llm.ContentBlock{Kind: kind, Text: text})
	}
	for _, seq := range sources {
		var source *session.Event
		for index := range events {
			if events[index].Seq == seq {
				source = &events[index]
				break
			}
		}
		if source == nil || source.Seq >= event.Seq {
			return 0, false
		}
		switch source.Type {
		case session.EventAssistantChunk:
			var data struct {
				Chunk struct {
					Text string `json:"text"`
				} `json:"chunk"`
				Text string `json:"text"`
			}
			if json.Unmarshal(source.Data, &data) != nil {
				return 0, false
			}
			text := data.Chunk.Text
			if text == "" {
				text = data.Text
			}
			appendBlock(llm.BlockText, text)
		case session.EventAssistantReasoning:
			var data struct {
				Chunk struct {
					Text string `json:"text"`
				} `json:"chunk"`
				Text string `json:"text"`
			}
			if json.Unmarshal(source.Data, &data) != nil {
				return 0, false
			}
			text := data.Chunk.Text
			if text == "" {
				text = data.Text
			}
			appendBlock(llm.BlockReasoning, text)
		default:
			return 0, false
		}
	}
	return EstimateMessage(llm.Message{Role: llm.RoleAssistant, Content: content, ToolCalls: message.ToolCalls}), true
}

func surfaceTokensThrough(events []session.Event) int {
	return projectedSurfaceTokens(events)
}

func surfaceTokens(log *session.Log) int {
	if log == nil {
		return 0
	}
	return projectedSurfaceTokens(log.Events())
}

func projectedSurfaceTokens(events []session.Event) int {
	snapshot, err := projection.Build(events)
	if err != nil {
		return 0
	}
	total := 0
	for _, entry := range snapshot.Surface {
		total += EstimateMessage(entry.Message)
	}
	return total
}

func EstimateMessage(message llm.Message) int {
	// Keep this pricing isomorphic to dsh-token-meter/estimate: every message
	// pays role framing, every text/reasoning block pays its own framing, and
	// nested tool-result blocks are priced recursively. Pricing the concatenated
	// Text() value here would undercount multi-block messages and floor rather
	// than ceil short content.
	tokens := 4
	var estimateBlocks func([]llm.ContentBlock) int
	estimateBlocks = func(blocks []llm.ContentBlock) int {
		total := 0
		for _, block := range blocks {
			switch block.Kind {
			case llm.BlockText, llm.BlockReasoning:
				total += ceilChars(block.Text) + 4
			case llm.BlockToolCall:
				total += ceilChars(block.Name) + ceilChars(block.Arguments) + 4
			case llm.BlockToolResult:
				total += estimateBlocks(block.Blocks) + 4
			case llm.BlockImage:
				encoded, _ := json.Marshal(block)
				total += 4 + ceilChars(string(encoded))
			default:
				encoded, _ := json.Marshal(block)
				total += 4 + ceilChars(string(encoded))
			}
		}
		return total
	}
	tokens += estimateBlocks(message.Content)
	for _, call := range message.ToolCalls {
		tokens += ceilChars(call.Name) + ceilChars(call.Arguments) + 4
	}
	return tokens
}

func ceilChars(value string) int {
	// The reference TypeScript estimator prices JavaScript string.length,
	// which counts UTF-16 code units. Go's rune count underprices astral
	// characters (emoji, many CJK extensions) and makes pressure decisions
	// diverge across the two implementations.
	count := len(utf16.Encode([]rune(value)))
	if count == 0 {
		return 0
	}
	return (count + 3) / 4
}

func estimateEvent(event session.Event) int { return len([]rune(string(event.Data)))/4 + 4 }

func estimateRequest(request llm.ChatRequest) int {
	// Routing metadata is not model-visible, and Messages are already priced
	// by the folded surface. Only the canonical system/tools header belongs in
	// this part of the estimate.
	total := 0
	var system strings.Builder
	for _, message := range request.Messages {
		if message.Role != llm.RoleSystem {
			continue
		}
		if text := message.Text(); text != "" {
			if system.Len() > 0 {
				system.WriteByte('\n')
			}
			system.WriteString(text)
		}
	}
	if system.Len() > 0 {
		total += ceilChars(system.String()) + 4
	}
	if len(request.Tools) > 0 {
		defs := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			defs = append(defs, map[string]any{
				"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters,
			})
		}
		encoded, _ := json.Marshal(defs)
		total += ceilChars(string(encoded)) + 4
	}
	return total
}

// latestHeaderRequest reconstructs the small canonical request envelope
// persisted by request/header. Conversation messages remain the folded
// surface and are deliberately not reconstructed here.
func latestHeaderRequest(events []session.Event) *llm.ChatRequest {
	var latest *llm.ChatRequest
	for i := range events {
		if request := requestHeaderAt(events[i]); request != nil {
			latest = request
		}
	}
	return latest
}

func latestHeaderRequestBefore(events []session.Event, index int) *llm.ChatRequest {
	if index >= len(events) {
		index = len(events) - 1
	}
	for i := index; i >= 0; i-- {
		if request := requestHeaderAt(events[i]); request != nil {
			return request
		}
	}
	return nil
}

func requestHeaderAt(event session.Event) *llm.ChatRequest {
	if event.Type != session.EventRequestHeader {
		return nil
	}
	encoded, err := json.Marshal(event.Data)
	if err != nil {
		return nil
	}
	var wire struct {
		Provider    string   `json:"provider"`
		Model       string   `json:"model"`
		Effort      string   `json:"reasoningEffort"`
		MaxTokens   int      `json:"maxTokens"`
		Temperature *float64 `json:"temperature"`
		Stop        []string `json:"stop"`
		Header      struct {
			System string           `json:"system"`
			Tools  []llm.ToolSchema `json:"tools"`
			Config struct {
				Provider    string   `json:"provider"`
				Model       string   `json:"model"`
				Effort      string   `json:"reasoningEffort"`
				MaxTokens   *int     `json:"maxTokens"`
				Temperature *float64 `json:"temperature"`
				Stop        []string `json:"stop"`
			} `json:"config"`
		} `json:"header"`
	}
	if json.Unmarshal(encoded, &wire) != nil {
		return nil
	}
	provider, model, effort := wire.Provider, wire.Model, wire.Effort
	maxTokens := wire.MaxTokens
	temperature := wire.Temperature
	stop := wire.Stop
	// Older request/header events kept the effective generation controls only
	// in header.config. Accept that shape during replay so a restored usage
	// anchor cannot be matched against a request with different controls.
	if provider == "" {
		provider = wire.Header.Config.Provider
	}
	if model == "" {
		model = wire.Header.Config.Model
	}
	if effort == "" {
		effort = wire.Header.Config.Effort
	}
	if maxTokens == 0 && wire.Header.Config.MaxTokens != nil {
		maxTokens = *wire.Header.Config.MaxTokens
	}
	if temperature == nil {
		temperature = wire.Header.Config.Temperature
	}
	if len(stop) == 0 {
		stop = wire.Header.Config.Stop
	}
	request := &llm.ChatRequest{
		Provider: provider, Model: model, ReasoningEffort: effort,
		MaxTokens: maxTokens, Temperature: temperature,
		Stop: append([]string(nil), stop...), Tools: wire.Header.Tools,
	}
	if wire.Header.System != "" {
		request.Messages = []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text(wire.Header.System)}}}
	}
	return request
}

func fingerprint(request llm.ChatRequest) string {
	// JSON distinguishes nil and empty slices, while request/header replay is
	// allowed to materialize omitted wire arrays as []. Canonicalize those
	// representation-only differences before comparing a live request with its
	// durable header anchor.
	request = cloneChatRequest(request)
	if len(request.Stop) == 0 {
		request.Stop = nil
	}
	if len(request.Tools) == 0 {
		request.Tools = nil
	}
	value := struct {
		Provider, Model, ReasoningEffort string
		MaxTokens                        int
		Temperature                      *float64
		Stop                             []string
		Messages                         []llm.Message
		Tools                            []llm.ToolSchema
	}{request.Provider, request.Model, request.ReasoningEffort, request.MaxTokens, request.Temperature, request.Stop, request.Messages, request.Tools}
	encoded, _ := json.Marshal(value)
	return strings.TrimSpace(string(encoded))
}

func cloneChatRequest(request llm.ChatRequest) llm.ChatRequest {
	cloned := request
	if request.Messages != nil {
		cloned.Messages = make([]llm.Message, len(request.Messages))
	}
	for i, message := range request.Messages {
		cloned.Messages[i] = message
		cloned.Messages[i].Content = cloneContentBlocks(message.Content)
		cloned.Messages[i].ToolCalls = append([]llm.ToolCall(nil), message.ToolCalls...)
	}
	if request.Tools != nil {
		cloned.Tools = make([]llm.ToolSchema, len(request.Tools))
	}
	for i, tool := range request.Tools {
		cloned.Tools[i] = tool
		if tool.Parameters != nil {
			cloned.Tools[i].Parameters = cloneJSONMap(tool.Parameters)
		}
	}
	cloned.Stop = append([]string(nil), request.Stop...)
	return cloned
}

func cloneContentBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]llm.ContentBlock, len(blocks))
	for i, block := range blocks {
		out[i] = block
		out[i].Blocks = cloneContentBlocks(block.Blocks)
	}
	return out
}

func cloneJSONMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return input
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return input
	}
	return out
}
