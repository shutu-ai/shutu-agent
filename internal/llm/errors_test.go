package llm

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyHTTPFailureRedactsCredentialShapedDiagnostics(t *testing.T) {
	err := ClassifyHTTPFailure("deepseek", 401, "401 Unauthorized", `{"message":"bad key","api_key":"secret-api-key","authorization":"Bearer secret-bearer"}`)
	if failure, ok := FailureFacts(err); !ok || failure.Code != "AUTH" {
		t.Fatalf("failure = %+v (typed=%v), want AUTH", failure, ok)
	}
	if strings.Contains(err.Error(), "secret-api-key") || strings.Contains(err.Error(), "secret-bearer") {
		t.Fatalf("diagnostic leaked credential: %q", err)
	}
	if !strings.Contains(err.Error(), "bad key") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("diagnostic lost useful context: %q", err)
	}
}

func TestRedactDiagnosticHandlesCompactJSONKeys(t *testing.T) {
	got := RedactDiagnostic(`{"nested":{"password":"hostile-password-value","items":["token=hostile-token-value"]},"safe":"ordinary"}`)
	if strings.Contains(got, "hostile-password-value") || strings.Contains(got, "hostile-token-value") {
		t.Fatalf("compact JSON diagnostic leaked credential: %q", got)
	}
	if !strings.Contains(got, "ordinary") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("compact JSON diagnostic lost useful redacted context: %q", got)
	}
}

func TestClassifyHTTPFailurePreservesRetryMetadata(t *testing.T) {
	err := ClassifyHTTPFailureWithMetadata("deepseek", 429, "429 Too Many Requests", "busy", 2_000, "req-7")
	failure, ok := FailureFacts(err)
	if !ok || failure.Code != "RATE_LIMIT" || failure.Status != 429 || failure.ProviderRetryAfterMS != 2_000 || failure.RequestID != "req-7" {
		t.Fatalf("failure = %+v (typed=%v), want structured retry metadata", failure, ok)
	}
}

func TestRetryAfterMillisecondsSupportsSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if got := RetryAfterMilliseconds("2", now); got != 2_000 {
		t.Fatalf("seconds retry-after = %d, want 2000", got)
	}
	if got := RetryAfterMilliseconds("Sat, 29 Aug 2026 12:00:03 GMT", now); got != 3_000 {
		t.Fatalf("date retry-after = %d, want 3000", got)
	}
	for _, value := range []string{"0", "-1", "not-a-date", "Sat, 29 Aug 2026 11:59:59 GMT"} {
		if got := RetryAfterMilliseconds(value, now); got != 0 {
			t.Fatalf("invalid/past retry-after %q = %d, want 0", value, got)
		}
	}
}

func TestRedactDiagnosticLeavesOrdinaryProviderText(t *testing.T) {
	got := RedactDiagnostic("context window exceeded; retry after token limit")
	if got != "context window exceeded; retry after token limit" {
		t.Fatalf("ordinary diagnostic = %q", got)
	}
}
