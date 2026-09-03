package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

type referenceReplayOutput struct {
	Surface []int            `json:"surface"`
	History []map[string]any `json:"history"`
}

// TestCoreTurnReplayMatchesReference is the executable double-replay gate.
// It is opt-in because the Go project does not vendor the reference checkout;
// CI that has both workspaces sets DSH_REFERENCE_ROOT. Without that external
// dependency this test skips rather than weakening the local fixture tests or
// claiming a reference comparison happened.
func TestCoreTurnReplayMatchesReference(t *testing.T) {
	referenceRoot := os.Getenv("DSH_REFERENCE_ROOT")
	if referenceRoot == "" {
		t.Skip("DSH_REFERENCE_ROOT is not set; reference double replay is not available")
	}
	if _, err := os.Stat(filepath.Join(referenceRoot, "packages/core/session/src/index.ts")); err != nil {
		t.Fatalf("reference Session source is unavailable: %v", err)
	}
	loader := filepath.Join(referenceRoot, "node_modules", "tsx", "dist", "loader.mjs")
	if _, err := os.Stat(loader); err != nil {
		t.Fatalf("reference tsx loader is unavailable: %v", err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	fixture := filepath.Join(repoRoot, "internal", "contractfixture", "core-turn-replay.json")
	script := filepath.Join(repoRoot, "scripts", "verify-reference-replay.mjs")
	cmd := exec.Command("node", "--import", fileURL(loader), script, fixture, referenceRoot)
	cmd.Dir = referenceRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("reference replay failed: %v\n%s", err, stderr.String())
	}
	var got referenceReplayOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("reference replay output is invalid JSON: %v\n%s", err, stdout.String())
	}

	records, err := loadCoreTurnFixtureForReferenceGate()
	if err != nil {
		t.Fatal(err)
	}
	log := New()
	for _, raw := range records {
		var wire struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(wire.Type, wire.Data); err != nil {
			t.Fatalf("append fixture event: %v", err)
		}
	}
	if got.Surface == nil {
		t.Fatal("reference replay returned no surface nodes")
	}
	goHistory := log.DeriveHistory()
	if len(got.History) != len(goHistory) {
		t.Fatalf("reference history length = %d, Go history length = %d", len(got.History), len(goHistory))
	}
	goSurface := make([]uint64, 0, len(log.Events()))
	for _, event := range log.Events() {
		if event.Type == EventUserMessage || event.Type == EventAssistantMessage || event.Type == EventToolResult {
			goSurface = append(goSurface, event.Seq)
		}
	}
	if len(got.Surface) != len(goSurface) {
		t.Fatalf("reference surface length = %d, Go surface length = %d", len(got.Surface), len(goSurface))
	}
	for index, seq := range goSurface {
		if got.Surface[index]+1 != int(seq) {
			t.Fatalf("surface[%d]: reference=%d Go=%d (reference is zero-based, Go is one-based)", index, got.Surface[index], seq)
		}
	}
	for index, message := range goHistory {
		expectedRole := string(message.Role)
		if message.Role == llm.RoleTool {
			expectedRole = "tool"
		}
		if got.History[index]["role"] != expectedRole {
			t.Fatalf("history[%d] role: reference=%v Go=%q", index, got.History[index]["role"], expectedRole)
		}
		content, err := referenceContent(message)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEquivalent(got.History[index]["content"], content) {
			t.Fatalf("history[%d] content differs: reference=%v Go=%v", index, got.History[index]["content"], content)
		}
		if message.Role == llm.RoleTool {
			if got.History[index]["toolCallId"] != message.ToolCallID {
				t.Fatalf("history[%d] tool call id differs: reference=%v Go=%q", index, got.History[index]["toolCallId"], message.ToolCallID)
			}
		}
	}
}

func referenceContent(message llm.Message) (any, error) {
	blocks := make([]map[string]any, 0, len(message.Content))
	for _, block := range message.Content {
		item := map[string]any{"type": string(block.Kind)}
		switch block.Kind {
		case llm.BlockText, llm.BlockReasoning:
			item["text"] = block.Text
		case llm.BlockImage:
			item["image"] = map[string]any{"attachmentId": block.Image.ID, "mediaType": block.Image.MediaType}
		default:
			return nil, fmt.Errorf("unsupported Go fixture content block %q", block.Kind)
		}
		blocks = append(blocks, item)
	}
	return blocks, nil
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func jsonEquivalent(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func loadCoreTurnFixtureForReferenceGate() ([]json.RawMessage, error) {
	// Keep this helper local to the test package so the executable gate verifies
	// the exact embedded fixture bytes independently of the production loader.
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "contractfixture", "core-turn-replay.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read core replay fixture: %w", err)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("decode core replay fixture: %w", err)
	}
	return records, nil
}
