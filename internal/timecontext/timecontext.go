// Package timecontext implements the DSH-compatible opt-in durable clock
// context provider. A reading records the entered request preparation point,
// the request's browser time-zone policy, and elapsed time against the correct
// durable baseline.
package timecontext

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/llm"
	"github.com/shutu-ai/shutu-agent/internal/session"
)

// SourceName is the exact plugin identity persisted on every reading.
const SourceName = "time-context"

// Config controls the opt-in provider. TimeZone empty means the process zone.
// RefreshIntervalMS zero means one reading for every eligible step.
type Config struct {
	TimeZone          string `yaml:"time_zone"`
	RefreshIntervalMS int    `yaml:"refresh_interval_ms"`
}

// Service validates configuration once and produces durable readings for
// addressed sessions. It owns no mutable per-session state: replay state comes
// from the authoritative session events on every call.
type Service struct {
	location     *time.Location
	locationName string
	refresh      time.Duration
	now          func() time.Time
}

var ianaTimeZonePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)

// New validates the configured interval and fallback time zone. Invalid values
// fail loudly instead of silently degrading the context provider.
func New(config Config) (*Service, error) {
	if config.RefreshIntervalMS < 0 {
		return nil, fmt.Errorf("time-context: refreshIntervalMs must be a non-negative safe integer, got %d", config.RefreshIntervalMS)
	}
	locationName := config.TimeZone
	var location *time.Location
	if strings.TrimSpace(locationName) == "" {
		location = time.Local
		locationName = location.String()
		if strings.TrimSpace(locationName) == "" {
			return nil, errors.New("time-context: failed to resolve the system time zone")
		}
	} else if locationName != "UTC" && !ianaTimeZonePattern.MatchString(locationName) {
		return nil, fmt.Errorf("time-context: invalid IANA timeZone %q", locationName)
	} else {
		resolved, err := time.LoadLocation(locationName)
		if err != nil {
			return nil, fmt.Errorf("time-context: invalid IANA timeZone %q", locationName)
		}
		location = resolved
	}
	return &Service{
		location: location, locationName: locationName,
		refresh: time.Duration(config.RefreshIntervalMS) * time.Millisecond,
		now:     time.Now,
	}, nil
}

// Now replaces the clock in tests. Production services leave the default.
func (s *Service) Now(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Reading derives and returns one durable time-context message. events is the
// durable prefix before the open step; messages are the already-entered plus
// proposed messages for the same open request.
func (s *Service) Reading(events []session.Event, turn, step int, messages []llm.Message) (*llm.Message, error) {
	if turn <= 0 || step <= 0 {
		return nil, fmt.Errorf("time-context: turn and step must be positive, got %d/%d", turn, step)
	}
	now := s.now()
	if latest, ok := latestInjection(events); ok && s.refresh > 0 &&
		!now.Before(latest.At) && now.Sub(latest.At) < s.refresh {
		return nil, nil
	}
	var previous time.Time
	var hasPrevious bool
	var baseline string
	if step == 1 {
		baseline = "model-visible message"
		if value, ok := precedingMessageTime(events); ok {
			previous = value.At
			hasPrevious = true
		} else {
			previous = now
		}
	} else {
		baseline = "step context"
		value, ok := precedingStepContextTime(events, turn)
		if ok {
			previous = value.At
			hasPrevious = true
		} else {
			previous = now
		}
	}
	browser, err := deriveBrowserTimeZoneContext(messages)
	if err != nil {
		return nil, err
	}
	selectedZone := s.locationName
	selectedLocation := s.location
	if browser.Kind == browserZoneResolved {
		resolved, err := loadCanonicalTimeZone(browser.TimeZone)
		if err != nil {
			return nil, err
		}
		selectedZone = browser.TimeZone
		selectedLocation = resolved
	}
	elapsed := "unavailable"
	if hasPrevious {
		elapsed = formatDuration(now.Sub(previous))
	}
	text := fmt.Sprintf(
		"Time sampled while preparing turn %d, step %d: %s\n%s\nElapsed since the preceding %s: %s.",
		turn, step, formatTimestamp(now.In(selectedLocation), selectedZone),
		renderBrowserTimeZoneContext(browser), baseline, elapsed,
	)
	return &llm.Message{
		Role:         llm.RoleUser,
		Content:      []llm.ContentBlock{llm.Text(text)},
		SourceKind:   "plugin",
		SourcePlugin: SourceName,
		SourceForm:   "snapshot",
		SourceSections: []llm.ContextSnapshotSection{
			{Name: SourceName, Text: text},
		},
	}, nil
}

type timeContextEventSource struct {
	Kind   string `json:"kind"`
	Plugin string `json:"plugin"`
}

func isTimeContextEvent(event session.Event) bool {
	if event.Type != session.EventUserMessage {
		return false
	}
	var payload struct {
		Source *timeContextEventSource `json:"source"`
	}
	if json.Unmarshal(event.Data, &payload) != nil || payload.Source == nil {
		return false
	}
	return payload.Source.Kind == "plugin" && payload.Source.Plugin == SourceName
}

func latestInjection(events []session.Event) (session.Event, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if isTimeContextEvent(events[index]) {
			return events[index], true
		}
	}
	return session.Event{}, false
}

func precedingMessageTime(events []session.Event) (session.Event, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		switch events[index].Type {
		case session.EventUserMessage, session.EventAssistantMessage, session.EventToolResult:
			return events[index], true
		}
	}
	return session.Event{}, false
}

func precedingStepContextTime(events []session.Event, turn int) (session.Event, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type == session.EventTurnStart {
			var payload struct {
				Turn int `json:"turn"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Turn == turn {
				return session.Event{}, false
			}
			continue
		}
		if isTimeContextEvent(event) {
			return event, true
		}
	}
	return session.Event{}, false
}

type browserZoneKind string

const (
	browserZoneResolved browserZoneKind = "resolved"
	browserZoneMixed    browserZoneKind = "mixed"
	browserZoneMissing  browserZoneKind = "missing"
)

type browserTimeZoneContext struct {
	Kind      browserZoneKind `json:"kind"`
	TimeZone  string          `json:"timeZone,omitempty"`
	TimeZones []string        `json:"timeZones,omitempty"`
}

func deriveBrowserTimeZoneContext(messages []llm.Message) (browserTimeZoneContext, error) {
	seen := make(map[string]bool)
	zones := make([]string, 0)
	for _, message := range messages {
		zone := strings.TrimSpace(message.SourceClientTimeZone)
		if message.SourceKind != "user" || strings.TrimSpace(message.SourceRPCID) == "" || zone == "" {
			continue
		}
		if _, err := loadCanonicalTimeZone(zone); err != nil {
			return browserTimeZoneContext{}, err
		}
		if !seen[zone] {
			seen[zone] = true
			zones = append(zones, zone)
		}
	}
	sort.Strings(zones)
	if len(zones) == 0 {
		return browserTimeZoneContext{Kind: browserZoneMissing}, nil
	}
	if len(zones) == 1 {
		return browserTimeZoneContext{Kind: browserZoneResolved, TimeZone: zones[0]}, nil
	}
	return browserTimeZoneContext{Kind: browserZoneMixed, TimeZones: zones}, nil
}

func loadCanonicalTimeZone(value string) (*time.Location, error) {
	if value == "UTC" {
		return time.UTC, nil
	}
	if !ianaTimeZonePattern.MatchString(value) {
		return nil, fmt.Errorf("browser time zone must be canonical UTC or IANA Area/Location: %q", value)
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return nil, fmt.Errorf("browser time zone is unsupported: %q", value)
	}
	return location, nil
}

func renderBrowserTimeZoneContext(context browserTimeZoneContext) string {
	switch context.Kind {
	case browserZoneResolved:
		return fmt.Sprintf("Browser time zone for this request: %s. Interpret otherwise-unqualified dates and times in this zone.", context.TimeZone)
	case browserZoneMixed:
		encoded, _ := json.Marshal(context.TimeZones)
		return fmt.Sprintf("Browser time zone for this request: mixed %s. Ask the user to clarify otherwise-unqualified dates and times.", string(encoded))
	default:
		return "Browser time zone for this request: unavailable. Ask the user to clarify otherwise-unqualified dates and times."
	}
}

func formatDuration(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int64(elapsed / time.Second)
	days := seconds / (24 * 60 * 60)
	seconds %= 24 * 60 * 60
	hours := seconds / (60 * 60)
	seconds %= 60 * 60
	minutes := seconds / 60
	seconds %= 60
	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	parts = append(parts, fmt.Sprintf("%ds", seconds))
	return strings.Join(parts, " ")
}

func formatTimestamp(value time.Time, zone string) string {
	offset := value.Format("-07:00")
	return fmt.Sprintf("%s[%s]", value.Format("2006-01-02T15:04:05")+offset, zone)
}
