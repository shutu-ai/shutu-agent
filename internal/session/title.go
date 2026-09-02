// Title utilities: deterministic session-title fallback and strict
// normalization, aligned with @shutu-ai/dsh-session-title semantics
// (packages/session/session-title/src/normalize.ts). A title is always one
// trimmed line: terminal control sequences and deceptive invisible controls are
// removed, whitespace is collapsed, and the result is truncated to a UTF-8 byte
// budget without splitting a code point.
package session

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Title limits, echoing the dsh default bundle
// (packages/bundle/base/cordis.patch.yml session-title row).
const (
	// TitleMaxBytes is the UTF-8 byte budget accepted from any source (the
	// maximum a rendered title may occupy).
	TitleMaxBytes = 80
	// TitleFallbackMaxWords is the whitespace-word cap of the deterministic
	// first-prompt fallback.
	TitleFallbackMaxWords = 5
	// TitleFallbackMaxBytes is the UTF-8 byte budget of the fallback. It is
	// lower than TitleMaxBytes so the fallback never crowds a later LLM title.
	TitleFallbackMaxBytes = 40
)

// Title source discriminators persisted alongside the accepted title.
const (
	TitleSourceFallback = "fallback"
	TitleSourceLLM      = "llm"
	TitleSourceUser     = "user"
)

// OSC sequences (ESC ] ... / 0x9D ...) are consumed here rather than via a
// regexp because the terminating BEL / ST / end-of-input rule is a negative
// lookahead the RE2 engine cannot express.
var (
	csiSequence      = regexp.MustCompile(`(?:\x1B\[|\x9B)[0-?]*[ -/]*[@-~]`)
	escSequence      = regexp.MustCompile(`\x1B[@-_]`)
	controlCharacter = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F-\x9F]`)
	directional      = regexp.MustCompile(`[\x{200B}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2064}\x{2066}-\x{206F}\x{FEFF}]`)
)

// removeOSC strips one or more operating-system-command sequences, including
// unterminated tails (consumed through the end of the input).
func removeOSC(input string) string {
	var b strings.Builder
	for i := 0; i < len(input); {
		if input[i] == 0x1B && i+1 < len(input) && input[i+1] == ']' {
			i = skipOSC(input, i+2)
			continue
		}
		if input[i] == 0x9D {
			i = skipOSC(input, i+1)
			continue
		}
		b.WriteByte(input[i])
		i++
	}
	return b.String()
}

// skipOSC advances past an OSC body starting at start, stopping at BEL, at the
// two-byte ST (ESC \), or at the end of the input (unterminated tail).
func skipOSC(input string, start int) int {
	for i := start; i < len(input); {
		if input[i] == 0x07 {
			return i + 1
		}
		if input[i] == 0x1B && i+1 < len(input) && input[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return len(input)
}

// cleanTitleText renders one trimmed, whitespace-normalized line from untrusted
// input, removing terminal control and deceptively invisible sequences. All
// Unicode whitespace (including leading and trailing) collapses to single
// spaces, so the result is always one line.
func cleanTitleText(input string) string {
	cleaned := directional.ReplaceAllString(
		controlCharacter.ReplaceAllString(
			escSequence.ReplaceAllString(
				csiSequence.ReplaceAllString(removeOSC(input), ""),
				""),
			""),
		"")
	return strings.Join(strings.Fields(cleaned), " ")
}

// TruncateUTF8 returns the longest leading code-point prefix of s that fits in
// maxBytes UTF-8 bytes (never splitting a code point). Empty when maxBytes is
// not positive.
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	used := 0
	output := make([]byte, 0, maxBytes)
	for _, r := range s {
		n := utf8.RuneLen(r)
		if used+n > maxBytes {
			break
		}
		output = utf8.AppendRune(output, r)
		used += n
	}
	return string(output)
}

// NormalizeTitle cleans untrusted text into one terminal-safe, byte-bounded
// line. It may return an empty string after sanitization. Whitespace is
// collapsed and the result is trimmed; the trailing whitespace is trimmed after
// the byte budget so a truncation cannot end on a run of spaces.
func NormalizeTitle(input string, maxBytes int) string {
	return strings.TrimRight(TruncateUTF8(cleanTitleText(input), maxBytes), " ")
}

// FallbackTitle derives the deterministic first-prompt fallback: the leading
// maxWords whitespace-delimited words of the first eligible human message,
// normalized and bounded to maxBytes UTF-8 bytes. Empty when no word survives.
func FallbackTitle(input string, maxWords, maxBytes int) string {
	if maxWords <= 0 {
		return ""
	}
	words := strings.Fields(cleanTitleText(input))
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	return strings.TrimRight(TruncateUTF8(strings.Join(words, " "), maxBytes), " ")
}

// FirstEligibleUserText returns the first non-empty text projection from a
// human user/message event. It deliberately goes through DeriveEventMessage so
// legacy text payloads and rich content blocks have the same interpretation at
// every title consumer (Web, native, ACP/SDK composition roots). Messages with
// explicit non-user provenance (for example Team/plugin input) are not title
// candidates, matching dsh's session-title service.
func FirstEligibleUserText(events []Event) string {
	for _, event := range events {
		if event.Type != EventUserMessage {
			continue
		}
		var raw struct {
			Source *struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if err := json.Unmarshal(event.Data, &raw); err != nil {
			continue
		}
		if raw.Source != nil && raw.Source.Kind != "" && raw.Source.Kind != "user" {
			continue
		}
		message, ok := DeriveEventMessage(event)
		if !ok {
			continue
		}
		textParts := make([]string, 0, len(message.Content))
		for _, block := range message.Content {
			if block.Kind == "text" {
				textParts = append(textParts, block.Text)
			}
		}
		text := strings.Join(textParts, "\n")
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}
