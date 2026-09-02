package meter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
	"github.com/jabing/shutu-agent/internal/session"
)

func TestUsageAnchorUsesProviderSourceChunksNotDurableRewrite(t *testing.T) {
	log := session.New()
	request := llm.ChatRequest{
		Provider: "deepseek", Model: "chat",
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("system")}}},
	}
	_, _ = log.Append(session.EventRequestHeader, session.NewRequestHeader("r-1", request, "initial"))
	chunk, _ := log.Append(session.EventAssistantChunk, session.NewAssistantChunkAt(1, 1, "provider output"))
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsageAndSources(
		1, 1, "provider output after durable normalization", nil, "stop", "", llm.TokenUsage{TotalTokens: 1000}, []uint64{chunk.Seq},
	))

	measurement := New().Measure("source-anchor", log, &request)
	if measurement.Baseline.Kind != "usage" {
		t.Fatalf("measurement = %+v, want provider baseline", measurement)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.Text("provider output")}}
	expectedAnchorSurface := EstimateMessage(assistant)
	if got := measurement.SurfaceDeltaTokens; got != measurement.SurfaceTokens-expectedAnchorSurface {
		t.Fatalf("surface delta = %d, surface=%d anchor=%d; durable rewrite was used as provider output", got, measurement.SurfaceTokens, expectedAnchorSurface)
	}
	if measurement.SurfaceDeltaTokens <= 0 {
		t.Fatalf("surface delta = %+v, want positive durable-rewrite delta", measurement)
	}

	raw, _ := json.Marshal(session.NewAssistantMessageAtWithUsageAndSources(1, 1, "", nil, "stop", "", llm.TokenUsage{}, nil))
	if strings.Contains(string(raw), "sourceEventSeqs") {
		t.Fatalf("nil source provenance must remain omitted: %s", raw)
	}
	raw, _ = json.Marshal(session.NewAssistantMessageAtWithUsageAndSources(1, 1, "", nil, "stop", "", llm.TokenUsage{}, []uint64{}))
	if !strings.Contains(string(raw), `"sourceEventSeqs":[]`) {
		t.Fatalf("known empty provider stream must serialize as []: %s", raw)
	}
}

func TestMeasureReturnsDetachedSurfaceAndUsageBaseline(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("hello world"))
	request := llm.ChatRequest{Provider: "deepseek", Model: "chat", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hello world")}}}}
	meter := New()
	first := meter.Measure("s", log, &request)
	if first.LogRevision != 1 || first.SurfaceTokens == 0 || len(first.Nodes) != 1 || first.Baseline.Kind != "estimated" {
		t.Fatalf("first measurement = %+v", first)
	}
	meter.RecordSuccessfulUsage("s", request, llm.TokenUsage{InputTokens: 20, OutputTokens: 10, TotalTokens: 30})
	second := meter.Measure("s", log, &request)
	if second.Baseline.Kind != "usage" || second.Baseline.Total != 30 || second.BaselineUsageTokens != 30 || second.Baseline.Usage.TotalTokens != 30 {
		t.Fatalf("usage measurement = %+v", second)
	}
	second.Nodes[0].Tokens = 0
	third := meter.Measure("s", log, &request)
	if third.Nodes[0].Tokens == 0 {
		t.Fatal("measurement nodes alias internal state")
	}
}

func TestMeasureUsesCurrentPositionalSurfaceAfterReplacement(t *testing.T) {
	log := session.New()
	first, _ := log.Append(session.EventUserMessage, session.NewUserMessage("old one"))
	second, _ := log.Append(session.EventAssistantMessage, session.NewAssistantMessage("old two", nil, "stop"))
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessageReplace("summary", int64(first.Seq), int64(second.Seq)))
	measurement := New().Measure("s", log, nil)
	if len(measurement.Nodes) != 1 || measurement.Nodes[0].Seq != 3 {
		t.Fatalf("positional nodes = %+v, want only replacement seq 3", measurement.Nodes)
	}
	if measurement.SurfaceTokens != EstimateMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("summary")}}) {
		t.Fatalf("surface tokens = %d, want replacement price", measurement.SurfaceTokens)
	}
}

func TestUsageBaselineUsesSurfaceAtAnchorAndReplacementAwareSurface(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("short"))
	request := llm.ChatRequest{Provider: "deepseek", Model: "chat", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("short")}}}}
	meter := New()
	meter.RecordSuccessfulUsageAt("s", request, llm.TokenUsage{TotalTokens: 100}, log)
	before := meter.Measure("s", log, &request)
	if before.SurfaceDeltaTokens != 0 {
		t.Fatalf("anchor delta = %+v", before)
	}
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("a much longer follow-up that is new surface"))
	after := meter.Measure("s", log, &request)
	if after.Baseline.Kind != "usage" || after.SurfaceDeltaTokens <= 0 || after.TotalTokens <= 100 {
		t.Fatalf("replacement-aware delta = %+v", after)
	}
}

func TestMeasureReusesLatestRequestWhenPressureHasNoRequestArgument(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("first"))
	request := llm.ChatRequest{
		Provider: "deepseek", Model: "chat",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("first")}}},
	}
	meter := New()
	meter.RecordSuccessfulUsageAt("s", request, llm.TokenUsage{TotalTokens: 80}, log)

	// The production pressure injector cannot assemble the next ChatRequest yet.
	// It must nevertheless retain the provider baseline from the last success.
	withoutRequest := meter.Measure("s", log, nil)
	if withoutRequest.Baseline.Kind != "usage" || withoutRequest.TotalTokens != 80 || withoutRequest.SurfaceDeltaTokens != 0 {
		t.Fatalf("requestless anchored measurement = %+v, want usage baseline 80", withoutRequest)
	}

	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("new follow-up"))
	withDelta := meter.Measure("s", log, nil)
	if withDelta.Baseline.Kind != "usage" || withDelta.TotalTokens <= 80 || withDelta.SurfaceDeltaTokens <= 0 {
		t.Fatalf("requestless surface delta = %+v, want usage plus positive delta", withDelta)
	}
}

func TestEstimateRequestPricesOnlyCanonicalHeader(t *testing.T) {
	request := llm.ChatRequest{
		Provider: "deepseek", Model: "chat", ReasoningEffort: "high",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("abcde")}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("hello world")}},
		},
	}
	// system: ceil(5/4)+4 = 6. The user message is surface content and must
	// not be counted in the request-header estimate.
	if got, want := estimateRequest(request), 6; got != want {
		t.Fatalf("estimateRequest = %d, want canonical header price %d", got, want)
	}
}

func TestMeasureRequestlessUsesLatestDurableHeader(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("hello"))
	request := llm.ChatRequest{
		Provider: "deepseek", Model: "chat",
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("system")}}},
	}
	_, _ = log.Append(session.EventRequestHeader, session.NewRequestHeader("r-1", request, "initial"))
	measurement := New().Measure("s", log, nil)
	if measurement.TotalTokens <= measurement.SurfaceTokens {
		t.Fatalf("requestless measurement dropped durable header: %+v", measurement)
	}
}

func TestUsageAnchorWithoutLogDoesNotSubtractHeaderAsSurface(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("hello"))
	request := llm.ChatRequest{
		Provider: "deepseek", Model: "chat",
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("system")}}},
	}
	meter := New()
	meter.RecordSuccessfulUsage("s", request, llm.TokenUsage{TotalTokens: 80})
	measurement := meter.Measure("s", log, &request)
	if measurement.Baseline.Kind != "usage" || measurement.TotalTokens != 80+measurement.SurfaceTokens {
		t.Fatalf("header-only usage anchor = %+v, want usage plus current surface", measurement)
	}
}

func TestMeasureEmptyLogDoesNotUnderflowRevision(t *testing.T) {
	m := New().Measure("empty", session.New(), nil)
	if m.LogRevision != 0 || m.TotalTokens != 0 || m.Baseline.Kind != "none" {
		t.Fatalf("empty measurement = %+v", m)
	}
}

func TestMeasureReplaysDurableUsageAnchorWithoutCallbackState(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessage("ask"))
	request := llm.ChatRequest{Provider: "deepseek", Model: "chat", Messages: []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.Text("system")}}}}
	_, _ = log.Append(session.EventRequestHeader, session.NewRequestHeader("request-1", request, "turn"))
	_, _ = log.Append(session.EventStepStart, session.NewStepStartAt(1, 1))
	usage := llm.TokenUsage{InputTokens: 20, OutputTokens: 7, CachedInputTokens: 3, ReasoningTokens: 6, TotalTokens: 999}
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(1, 1, "answer", nil, "stop", "", usage))

	// No RecordSuccessfulUsage call: a fresh Meter must recover the provider
	// anchor from the durable assistant/message replay just like the reference.
	measurement := New().Measure("cold", log, nil)
	if measurement.Baseline.Kind != "usage" || measurement.Baseline.Total != 30 || measurement.SurfaceDeltaTokens != 0 {
		t.Fatalf("durable usage measurement = %+v, want replayed disjoint bucket baseline", measurement)
	}
}

func TestMeasureReplaysEarlierDurableUsageWhenLatestAssistantOmitsUsage(t *testing.T) {
	log := session.New()
	request := llm.ChatRequest{Provider: "deepseek", Model: "chat"}
	_, _ = log.Append(session.EventRequestHeader, session.NewRequestHeader("request-1", request, "turn"))
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessageAtWithUsage(
		1, 1, "first", nil, "stop", "", llm.TokenUsage{InputTokens: 20, OutputTokens: 10},
	))
	// A later compatibility response has no provider usage. It must not erase
	// the earlier durable anchor during cold replay.
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessage("later", nil, "stop"))

	measurement := New().Measure("cold", log, nil)
	if measurement.Baseline.Kind != "usage" || measurement.Baseline.Total != 30 {
		t.Fatalf("replayed fallback anchor = %+v, want earlier usage baseline 30", measurement)
	}
}

func TestUsageBaselineUsesDisjointCacheReadAndWriteBuckets(t *testing.T) {
	request := llm.ChatRequest{Provider: "deepseek", Model: "chat"}
	m := New()
	m.RecordSuccessfulUsage("cache", request, llm.TokenUsage{
		InputTokens: 20, CacheReadTokens: 7, CacheWriteTokens: 3,
		OutputTokens: 5, TotalTokens: 999,
	})
	measurement := m.Measure("cache", session.New(), &request)
	if measurement.Baseline.Kind != "usage" || measurement.Baseline.Total != 35 {
		t.Fatalf("disjoint cache baseline = %+v, want 20+7+3+5", measurement)
	}
}

func TestUsageAnchorFingerprintIncludesGenerationControls(t *testing.T) {
	base := llm.ChatRequest{Provider: "deepseek", Model: "chat"}
	m := New()
	m.RecordSuccessfulUsage("controls", base, llm.TokenUsage{InputTokens: 20, OutputTokens: 10})

	temperature := 0.2
	changed := base
	changed.MaxTokens = 512
	changed.Temperature = &temperature
	changed.Stop = []string{"DONE"}
	measurement := m.Measure("controls", session.New(), &changed)
	if measurement.Baseline.Kind != "estimated" {
		t.Fatalf("generation-control change reused usage anchor: %+v", measurement)
	}
}

func TestRequestHeaderReplayRestoresGenerationControlsFromCanonicalConfig(t *testing.T) {
	temperature := 0.4
	request := llm.ChatRequest{
		Provider: "deepseek", Model: "chat", MaxTokens: 256,
		Temperature: &temperature, Stop: []string{"DONE"},
	}
	encoded, err := json.Marshal(map[string]any{
		"provider": request.Provider,
		"model":    request.Model,
		"header": map[string]any{
			"config": map[string]any{
				"provider":    request.Provider,
				"model":       request.Model,
				"maxTokens":   request.MaxTokens,
				"temperature": request.Temperature,
				"stop":        request.Stop,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed := requestHeaderAt(session.Event{Type: session.EventRequestHeader, Data: encoded})
	if parsed == nil || parsed.MaxTokens != request.MaxTokens || parsed.Temperature == nil || *parsed.Temperature != *request.Temperature || len(parsed.Stop) != 1 || parsed.Stop[0] != "DONE" {
		t.Fatalf("replayed controls = %+v, want %+v", parsed, request)
	}
}

func TestUsageProjectionReplacesSameStepStreamingSample(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventAssistantChunk, map[string]any{
		"turn": 1, "step": 1,
		"chunk": map[string]any{"type": "usage", "usage": llm.TokenUsage{
			InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventAssistantMessage, map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"role": "assistant", "content": []any{}},
		"usage": llm.TokenUsage{
			InputTokens: 14, OutputTokens: 5, CacheReadTokens: 8, CacheWriteTokens: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := New().UsageProjection(log), (UsageProjection{
		UncachedInputTokens: 14, OutputTokens: 5, CacheReadTokens: 8, CacheWriteTokens: 1,
	}); got != want {
		t.Fatalf("usage projection = %+v, want %+v", got, want)
	}
}

func TestContextPressureProjectionExcludesOutputAndTracksSurfaceDelta(t *testing.T) {
	log := session.New()
	if _, err := log.Append("request/context", map[string]any{"contextWindow": 128000}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("question")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventAssistantChunk, map[string]any{
		"turn": 1, "step": 1,
		"chunk": map[string]any{"type": "usage", "usage": llm.TokenUsage{
			InputTokens: 100, OutputTokens: 4000, CacheReadTokens: 20, CacheWriteTokens: 5,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	got := New().ContextPressureProjection(log)
	if got.PressureTokens == nil || *got.PressureTokens != 125 {
		t.Fatalf("pressure = %+v, want 125 prompt tokens", got)
	}
	if got.ContextWindow == nil || *got.ContextWindow != 128000 {
		t.Fatalf("context window = %+v", got.ContextWindow)
	}
	if got.ProjectedTokens == nil || *got.ProjectedTokens < *got.PressureTokens {
		t.Fatalf("projected pressure = %+v, want surface growth", got.ProjectedTokens)
	}
}

func TestContextBreakdownProjectionTracksEnvelopeAndFoldedSurface(t *testing.T) {
	log := session.New()
	if _, err := log.Append(session.EventRequestHeader, map[string]any{
		"provider": "mock", "model": "mock",
		"header": map[string]any{
			"system": "be concise",
			"tools":  []any{map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(session.EventUserMessage, session.NewUserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	meter := New()
	got := meter.ContextBreakdownProjection(log)
	if got.SystemTokens == 0 || got.ToolsTokens == 0 || got.MessageTokens == 0 {
		t.Fatalf("breakdown = %+v, want all populated", got)
	}
	if got.MessageTokens != meter.Measure("projection", log, nil).SurfaceTokens {
		t.Fatalf("message tokens = %d, meter surface differs", got.MessageTokens)
	}
}

func TestEstimateMessageUsesCanonicalPerBlockCeilPricing(t *testing.T) {
	message := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.Text("a"),
		{Kind: llm.BlockReasoning, Text: "12345"},
		{Kind: llm.BlockToolResult, Blocks: []llm.ContentBlock{llm.Text("xy")}},
	}}
	// role 4 + text (ceil(1/4)+4) + reasoning (ceil(5/4)+4) +
	// nested tool result (text (ceil(2/4)+4) + result overhead 4).
	const want = 4 + (1 + 4) + (2 + 4) + ((1 + 4) + 4)
	if got := EstimateMessage(message); got != want {
		t.Fatalf("EstimateMessage = %d, want canonical %d", got, want)
	}
}

func TestEstimateMessageMatchesJavaScriptUTF16StringLength(t *testing.T) {
	message := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.Text("😀😀😀😀😀")}}
	// Five emoji occupy ten UTF-16 code units: ceil(10/4)+4 block overhead+4 role.
	if got, want := EstimateMessage(message), 11; got != want {
		t.Fatalf("emoji estimate = %d, want JavaScript UTF-16 estimate %d", got, want)
	}
}

func TestCompletionProjectionUsesCommittedBoundariesAndTracksAttachments(t *testing.T) {
	log := session.New()
	_, _ = log.Append(session.EventAssistantChunk, map[string]any{
		"turn": 1, "step": 1, "chunk": map[string]any{"type": "text", "text": "transient"},
	})
	ref := llm.ImageRef{ID: "img-1", MediaType: "image/png", Bytes: 12}
	_, _ = log.Append(session.EventAssistantMessage, session.NewAssistantMessageWithUsage(
		"answer", []llm.ToolCall{{ID: "c1", Name: "read", Arguments: `{}`}}, "stop", "think", llm.TokenUsage{}))
	_, _ = log.Append(session.EventUserMessage, session.NewUserMessageWithBlocks("", []llm.ContentBlock{{Kind: llm.BlockImage, Image: ref}}))
	got := New().CompletionProjection(log)
	if got.AssistantTokens == 0 || got.ToolCallTokens == 0 {
		t.Fatalf("completion ledger = %+v, want assistant and tool-call pricing", got)
	}
	if got.ReasoningTokens == 0 {
		t.Fatalf("completion ledger = %+v, want reasoning pricing", got)
	}
	if got.AttachmentBytes != 0 {
		// User attachments are input-surface accounting, not completion output.
		t.Fatalf("completion ledger counted user attachment bytes: %+v", got)
	}
}
