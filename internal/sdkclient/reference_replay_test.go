package sdkclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessReplaysReferenceNotificationFixtures(t *testing.T) {
	referenceRoot := os.Getenv("SHUTU_REFERENCE_ROOT")
	if referenceRoot == "" {
		referenceRoot = filepath.Clean(filepath.Join("..", "..", ".reference", "dsh"))
		if _, err := os.Stat(filepath.Join(referenceRoot, "examples", "jsonrpc-agent", "tests", "snapshots", "text-turn", "notifications.expected.jsonl")); err != nil {
			t.Skip("reference SDK checkout is not available")
		}
	}
	scenarios := []string{"text-turn", "bash-tool", "persistent-tools", "subagent-spawn-in-process"}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			scenarioDir := filepath.Join(referenceRoot, "examples", "jsonrpc-agent", "tests", "snapshots", scenario)
			notificationPath := filepath.Join(scenarioDir, "notifications.expected.jsonl")
			resultPath := filepath.Join(scenarioDir, "result.expected.json")

			options := launchOptions("reference", func(options *ClientOptions) {
				options.Env = append(options.Env, referenceNotificationsEnv+"="+notificationPath)
			})
			harness := NewHarness(HarnessOptions{Launch: options, CWD: scenarioDir})
			result, err := harness.Run(context.Background(), "reference replay", "reference-root")
			if err != nil {
				t.Fatal(err)
			}
			if err := harness.Close(); err != nil {
				t.Fatal(err)
			}

			expectedNotifications := loadReferenceNotifications(t, notificationPath)
			var expectedResult struct {
				FinalResponse string `json:"finalResponse"`
			}
			raw, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(raw, &expectedResult) != nil {
				t.Fatalf("invalid reference result fixture: %s", raw)
			}
			if result.SessionID != "reference-root" || result.FinalResponse != expectedResult.FinalResponse {
				t.Fatalf("result = (%q, %q), want reference (%q, %q)", result.SessionID, result.FinalResponse, "reference-root", expectedResult.FinalResponse)
			}
			if len(result.Notifications) != len(expectedNotifications) {
				t.Fatalf("notification count = %d, want %d", len(result.Notifications), len(expectedNotifications))
			}
			for i, actual := range result.Notifications {
				expected := expectedNotifications[i]
				if actual.Method != expected.Method {
					t.Fatalf("notification %d method = %q, want %q", i, actual.Method, expected.Method)
				}
				if canonicalJSON(t, actual.Params) != canonicalJSON(t, expected.Params) {
					t.Fatalf("notification %d params differ\nactual:   %s\nexpected: %s", i, canonicalJSON(t, actual.Params), canonicalJSON(t, expected.Params))
				}
			}
		})
	}
}

func loadReferenceNotifications(t *testing.T, path string) []Notification {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Notification
	inChild := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		frame := materializeReferenceFrame(line, "reference-root", "reference-root-child", &inChild)
		var notification Notification
		if json.Unmarshal(frame, &notification) != nil {
			t.Fatalf("invalid reference notification fixture: %s", raw)
		}
		out = append(out, notification)
	}
	if len(out) == 0 {
		t.Fatal("reference fixture is empty")
	}
	return out
}

func canonicalJSON(t *testing.T, value json.RawMessage) string {
	t.Helper()
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		t.Fatalf("invalid JSON: %s", value)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
