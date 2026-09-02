package llm

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	redactBearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[^\s,\"'}]+`)
	redactSecretPattern = regexp.MustCompile(`(?i)([\"']?(?:api[-_]?key|authorization|access[-_]?token|token|secret|password)[\"']?\s*[:=]\s*[\"']?)[^\s,\"'}]+`)
)

// ClassifyHTTPFailure maps common provider HTTP responses to the shared
// failure vocabulary. The response body passed here must already be bounded
// by the caller.
func ClassifyHTTPFailure(provider string, status int, statusText, body string) error {
	return ClassifyHTTPFailureWithMetadata(provider, status, statusText, body, 0, "")
}

// ClassifyHTTPFailureWithMetadata preserves provider retry hints and request
// correlation facts without making retry policy a transport concern.
func ClassifyHTTPFailureWithMetadata(provider string, status int, statusText, body string, retryAfterMS int64, requestID string) error {
	statusText = RedactDiagnostic(statusText)
	body = RedactDiagnostic(body)
	lower := strings.ToLower(statusText + " " + body)
	code := fmt.Sprintf("HTTP_%d", status)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = "AUTH"
	case status == http.StatusTooManyRequests && (strings.Contains(lower, "quota") || strings.Contains(lower, "balance") || strings.Contains(lower, "credit")):
		code = "QUOTA"
	case status == http.StatusTooManyRequests:
		code = "RATE_LIMIT"
	case status == http.StatusRequestTimeout:
		code = "TIMEOUT"
	case status == http.StatusBadRequest && IsContextOverflowText(lower):
		code = "CONTEXT_WINDOW_EXCEEDED"
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		code = "INVALID_REQUEST"
	case status >= http.StatusInternalServerError:
		code = "SERVER"
	}
	facts := Failure{Message: fmt.Sprintf("%s: provider error: %s: %s", provider, statusText, body), Code: code, Status: status, ProviderRetryAfterMS: retryAfterMS, RequestID: requestID}
	return &FailureError{Facts: redactFailure(facts)}
}

// RetryAfterMilliseconds parses the HTTP Retry-After seconds/date forms.
// Invalid, zero, and past values are ignored. The caller applies its policy
// cap; this helper only preserves a valid provider hint.
func RetryAfterMilliseconds(value string, now time.Time) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds > 0 {
			return int64(seconds * 1000)
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := when.Sub(now).Milliseconds()
	if delay <= 0 {
		return 0
	}
	return delay
}

// RedactDiagnostic removes credential-shaped values before provider
// diagnostics enter durable events, logs, or UI projections. It intentionally
// operates on already bounded text and leaves ordinary token-limit messages
// untouched unless they use a key/value or Bearer-shaped form.
func RedactDiagnostic(value string) string {
	value = redactBearerPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	return redactSecretPattern.ReplaceAllString(value, `${1}[REDACTED]`)
}

func IsContextOverflowText(value string) bool {
	for _, marker := range []string{"context length", "context window", "context_length", "maximum context", "too many tokens", "token limit", "prompt is too long"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
