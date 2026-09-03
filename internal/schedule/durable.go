package schedule

// durable.go implements the version-1 durable reminder projection used by the
// Goal scheduler. It follows dsh-schedule's important boundary: the session
// event log is authoritative, while this projection and its timer are
// disposable runtime state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

const (
	DurableChangeVersion       = 1
	MinEverySeconds      int64 = 300
)

type DurableKind string

const (
	DurableAfter DurableKind = "after"
	DurableAt    DurableKind = "at"
	DurableEvery DurableKind = "every"
)

// DurableRecord is the canonical Session-local schedule record. ScheduledAt
// is always stored as a UTC instant; for every records it is the creation
// anchor of the next not-yet-dispatched occurrence.
type DurableRecord struct {
	ID           string      `json:"id"`
	Kind         DurableKind `json:"kind"`
	Prompt       string      `json:"prompt"`
	AfterSeconds int64       `json:"afterSeconds,omitempty"`
	EverySeconds int64       `json:"everySeconds,omitempty"`
	ScheduledAt  time.Time   `json:"scheduledAt"`
}

type DurableView struct {
	DurableRecord
	State        string `json:"state"`
	DeliveryMode string `json:"deliveryMode"`
}

type DurableCreateRequest struct {
	Prompt       string
	AfterSeconds *int64
	At           string
	AtLocal      *LocalAtInput
	EverySeconds *int64
}

// LocalAtInput is the DSH local-calendar selector for schedule_create.
type LocalAtInput struct {
	Date     string
	Time     string
	TimeZone string
}

// DurableChange is the strict v1 event vocabulary. A delete/one-shot dispatch
// carries only id; every dispatch carries acceptedAt so replay can skip all
// missed occurrences without enumerating them.
type DurableChange struct {
	Version    int            `json:"version"`
	Operation  string         `json:"operation"`
	Schedule   *DurableRecord `json:"schedule,omitempty"`
	ID         string         `json:"id,omitempty"`
	AcceptedAt *time.Time     `json:"acceptedAt,omitempty"`
}

type DurableDue struct {
	Kind       DurableKind
	Records    []DurableRecord
	AcceptedAt time.Time
}

var (
	ErrDurableClosed      = errors.New("schedule: durable scheduler closed")
	ErrInvalidPrompt      = errors.New("schedule: invalid prompt")
	ErrInvalidSelector    = errors.New("schedule: exactly one selector is required")
	ErrNotFuture          = errors.New("schedule: target must be in the future")
	ErrInvalidTimeZone    = errors.New("schedule: invalid time zone")
	ErrFrequencyTooHigh   = errors.New("schedule: every interval is below five minutes")
	ErrScheduleNotFound   = errors.New("schedule: durable schedule not found")
	ErrCorruptScheduleLog = errors.New("schedule: corrupt schedule/change log")
)

// DurableScheduler owns only the folded projection. onChange must durably
// append the supplied change before it returns; this gives tools and dispatch
// the same append-on-write semantics as the rest of the Agent session log.
type DurableScheduler struct {
	mu       sync.Mutex
	active   map[string]DurableRecord
	seen     map[string]struct{}
	order    []string
	nextID   int
	onChange func(DurableChange) error
	closed   bool
}

func NewDurableScheduler(onChange func(DurableChange) error) *DurableScheduler {
	return &DurableScheduler{
		active:   map[string]DurableRecord{},
		seen:     map[string]struct{}{},
		onChange: onChange,
	}
}

// Restore folds only schedule/change events. It is intentionally strict about
// lifecycle transitions: a malformed or stale event must not silently create
// a different schedule projection after restart.
func (s *DurableScheduler) Restore(events []session.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrDurableClosed
	}
	s.active = map[string]DurableRecord{}
	s.seen = map[string]struct{}{}
	s.order = nil
	s.nextID = 0
	for _, ev := range events {
		if ev.Type != session.EventScheduleChange {
			continue
		}
		var change DurableChange
		if err := unmarshalDurableChange(ev.Data, &change); err != nil {
			return err
		}
		if err := s.applyLocked(change); err != nil {
			return err
		}
	}
	return nil
}

func (s *DurableScheduler) Create(ctx context.Context, req DurableCreateRequest, now time.Time) (DurableRecord, error) {
	if err := ctx.Err(); err != nil {
		return DurableRecord{}, err
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return DurableRecord{}, ErrInvalidPrompt
	}
	selectors := 0
	if req.AfterSeconds != nil {
		selectors++
	}
	if strings.TrimSpace(req.At) != "" {
		selectors++
	}
	if req.AtLocal != nil {
		selectors++
	}
	if req.EverySeconds != nil {
		selectors++
	}
	if selectors != 1 {
		return DurableRecord{}, ErrInvalidSelector
	}
	now = now.UTC()
	record := DurableRecord{Kind: DurableAfter, Prompt: prompt}
	switch {
	case req.AfterSeconds != nil:
		if *req.AfterSeconds <= 0 || *req.AfterSeconds > int64((1<<63-1)/int64(time.Second)) {
			return DurableRecord{}, fmt.Errorf("%w: after_seconds must be positive", ErrInvalidSpec)
		}
		record.AfterSeconds = *req.AfterSeconds
		record.ScheduledAt = now.Add(time.Duration(*req.AfterSeconds) * time.Second)
	case strings.TrimSpace(req.At) != "":
		target, err := parseFutureInstant(req.At, now)
		if err != nil {
			return DurableRecord{}, err
		}
		record.Kind = DurableAt
		record.ScheduledAt = target
	case req.AtLocal != nil:
		target, err := parseFutureLocalInstant(*req.AtLocal, now)
		if err != nil {
			return DurableRecord{}, err
		}
		record.Kind = DurableAt
		record.ScheduledAt = target
	case req.EverySeconds != nil:
		if *req.EverySeconds < MinEverySeconds || *req.EverySeconds > int64((1<<63-1)/int64(time.Second)) {
			return DurableRecord{}, ErrFrequencyTooHigh
		}
		record.Kind = DurableEvery
		record.EverySeconds = *req.EverySeconds
		record.ScheduledAt = now.Add(time.Duration(*req.EverySeconds) * time.Second)
	}
	if record.ScheduledAt.Year() < 1 || record.ScheduledAt.Year() > 9999 {
		return DurableRecord{}, fmt.Errorf("%w: scheduled time is outside four-digit year range", ErrInvalidSpec)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return DurableRecord{}, ErrDurableClosed
	}
	record.ID = s.allocateIDLocked()
	change := DurableChange{Version: DurableChangeVersion, Operation: "create", Schedule: &record}
	if err := s.appendLocked(change); err != nil {
		return DurableRecord{}, err
	}
	if err := s.applyLocked(change); err != nil {
		return DurableRecord{}, err
	}
	return record, nil
}

func (s *DurableScheduler) Delete(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrDurableClosed
	}
	if _, ok := s.active[id]; !ok {
		return false, nil
	}
	change := DurableChange{Version: DurableChangeVersion, Operation: "delete", ID: id}
	if err := s.appendLocked(change); err != nil {
		return false, err
	}
	return true, s.applyLocked(change)
}

func (s *DurableScheduler) List(ctx context.Context, now time.Time) ([]DurableView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrDurableClosed
	}
	now = now.UTC()
	out := make([]DurableView, 0, len(s.active))
	for _, id := range s.order {
		record, ok := s.active[id]
		if !ok {
			continue
		}
		state := "scheduled"
		if !record.ScheduledAt.After(now) {
			state = "overdue"
		}
		out = append(out, DurableView{DurableRecord: cloneDurableRecord(record), State: state, DeliveryMode: "session-local"})
	}
	return out, nil
}

// NextWake returns the earliest active target. The application uses it to arm
// a bounded timer instead of polling at a coarse fixed cadence.
func (s *DurableScheduler) NextWake(ctx context.Context) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return time.Time{}, false, ErrDurableClosed
	}
	var next time.Time
	for _, id := range s.order {
		record, ok := s.active[id]
		if !ok {
			continue
		}
		if next.IsZero() || record.ScheduledAt.Before(next) {
			next = record.ScheduledAt
		}
	}
	return next, !next.IsZero(), nil
}

// Due selects one earliest one-shot, or one complete batch of overdue Every
// records. The caller must successfully run the follow-up before Dispatch.
func (s *DurableScheduler) Due(ctx context.Context, now time.Time) (DurableDue, bool, error) {
	if err := ctx.Err(); err != nil {
		return DurableDue{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return DurableDue{}, false, ErrDurableClosed
	}
	now = now.UTC()
	var oneShots []DurableRecord
	var every []DurableRecord
	for _, id := range s.order {
		record, ok := s.active[id]
		if !ok || record.ScheduledAt.After(now) {
			continue
		}
		if record.Kind == DurableEvery {
			every = append(every, cloneDurableRecord(record))
		} else {
			oneShots = append(oneShots, cloneDurableRecord(record))
		}
	}
	byTarget := func(left, right DurableRecord) bool {
		if left.ScheduledAt.Equal(right.ScheduledAt) {
			return indexOf(s.order, left.ID) < indexOf(s.order, right.ID)
		}
		return left.ScheduledAt.Before(right.ScheduledAt)
	}
	sort.SliceStable(oneShots, func(i, j int) bool { return byTarget(oneShots[i], oneShots[j]) })
	sort.SliceStable(every, func(i, j int) bool { return byTarget(every[i], every[j]) })
	if len(oneShots) > 0 {
		return DurableDue{Kind: oneShots[0].Kind, Records: oneShots[:1], AcceptedAt: now}, true, nil
	}
	if len(every) > 0 {
		return DurableDue{Kind: DurableEvery, Records: every, AcceptedAt: now}, true, nil
	}
	return DurableDue{}, false, nil
}

// Dispatch records the accepted follow-up. Every schedules advance directly
// to the first anchor-aligned target after acceptedAt; missed occurrences are
// never replayed.
func (s *DurableScheduler) Dispatch(ctx context.Context, due DurableDue) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	accepted := due.AcceptedAt.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrDurableClosed
	}
	for _, record := range due.Records {
		current, ok := s.active[record.ID]
		if !ok || current.Kind != record.Kind || !current.ScheduledAt.Equal(record.ScheduledAt) {
			return fmt.Errorf("%w: %s", ErrScheduleNotFound, record.ID)
		}
		var acceptedAt *time.Time
		if record.Kind == DurableEvery {
			acceptedAt = &accepted
		}
		change := DurableChange{Version: DurableChangeVersion, Operation: "dispatch", ID: record.ID, AcceptedAt: acceptedAt}
		if err := s.appendLocked(change); err != nil {
			return err
		}
		if err := s.applyLocked(change); err != nil {
			return err
		}
	}
	return nil
}

func (s *DurableScheduler) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *DurableScheduler) appendLocked(change DurableChange) error {
	if s.onChange != nil {
		return s.onChange(change)
	}
	return nil
}

func (s *DurableScheduler) applyLocked(change DurableChange) error {
	if change.Version != DurableChangeVersion {
		return ErrCorruptScheduleLog
	}
	switch change.Operation {
	case "create":
		if change.Schedule == nil || change.Schedule.ID == "" || strings.TrimSpace(change.Schedule.Prompt) == "" {
			return ErrCorruptScheduleLog
		}
		if _, ok := s.seen[change.Schedule.ID]; ok {
			return fmt.Errorf("%w: schedule id reused: %s", ErrCorruptScheduleLog, change.Schedule.ID)
		}
		if change.Schedule.Kind != DurableAfter && change.Schedule.Kind != DurableAt && change.Schedule.Kind != DurableEvery {
			return ErrCorruptScheduleLog
		}
		if change.Schedule.Kind == DurableEvery && change.Schedule.EverySeconds < MinEverySeconds {
			return ErrCorruptScheduleLog
		}
		s.seen[change.Schedule.ID] = struct{}{}
		s.order = append(s.order, change.Schedule.ID)
		s.active[change.Schedule.ID] = cloneDurableRecord(*change.Schedule)
		if n := scheduleSequence(change.Schedule.ID); n > s.nextID {
			s.nextID = n
		}
	case "delete":
		if _, ok := s.active[change.ID]; !ok {
			return fmt.Errorf("%w: delete %s", ErrCorruptScheduleLog, change.ID)
		}
		delete(s.active, change.ID)
	case "dispatch":
		record, ok := s.active[change.ID]
		if !ok {
			return fmt.Errorf("%w: dispatch %s", ErrCorruptScheduleLog, change.ID)
		}
		if record.Kind != DurableEvery {
			if change.AcceptedAt != nil {
				return ErrCorruptScheduleLog
			}
			delete(s.active, change.ID)
			break
		}
		if change.AcceptedAt == nil || change.AcceptedAt.Before(record.ScheduledAt) {
			return ErrCorruptScheduleLog
		}
		interval := time.Duration(record.EverySeconds) * time.Second
		elapsed := change.AcceptedAt.Sub(record.ScheduledAt)
		steps := elapsed/interval + 1
		next := record.ScheduledAt.Add(steps * interval)
		record.ScheduledAt = next
		s.active[change.ID] = record
	default:
		return ErrCorruptScheduleLog
	}
	return nil
}

func (s *DurableScheduler) allocateIDLocked() string {
	for {
		s.nextID++
		id := fmt.Sprintf("schedule-%d", s.nextID)
		if _, ok := s.seen[id]; !ok {
			return id
		}
	}
}

func unmarshalDurableChange(data []byte, into *DurableChange) error {
	if err := jsonUnmarshal(data, into); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptScheduleLog, err)
	}
	return nil
}

func parseFutureInstant(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if !validOffsetDateTime(value) {
		return time.Time{}, fmt.Errorf("%w: invalid at instant", ErrInvalidSpec)
	}
	target, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid at instant: %v", ErrInvalidSpec, err)
	}
	target = target.UTC()
	if target.Year() < 1 || target.Year() > 9999 {
		return time.Time{}, fmt.Errorf("%w: scheduled time is outside four-digit year range", ErrInvalidSpec)
	}
	if !target.After(now) {
		return time.Time{}, ErrNotFuture
	}
	return target, nil
}

func validOffsetDateTime(value string) bool {
	if len(value) < 20 || value[4] != '-' || value[7] != '-' || value[10] != 'T' || value[13] != ':' || value[16] != ':' {
		return false
	}
	for _, index := range []int{0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 14, 15, 17, 18} {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	zoneAt := strings.IndexAny(value[19:], "Z+-")
	if zoneAt < 0 {
		return false
	}
	zoneAt += 19
	if zoneAt > 19 {
		if value[19] != '.' {
			return false
		}
		fraction := value[20:zoneAt]
		if len(fraction) < 1 || len(fraction) > 3 || !allDigits(fraction) {
			return false
		}
	}
	zone := value[zoneAt:]
	if zone == "Z" {
		return true
	}
	return len(zone) == 6 && (zone[0] == '+' || zone[0] == '-') && zone[3] == ':' && allDigits(zone[1:3]) && allDigits(zone[4:6])
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseFutureLocalInstant(input LocalAtInput, now time.Time) (time.Time, error) {
	if len(input.Date) != 10 || input.Date[4] != '-' || input.Date[7] != '-' || !allDigits(input.Date[:4]+input.Date[5:7]+input.Date[8:]) {
		return time.Time{}, fmt.Errorf("%w: local at requires date YYYY-MM-DD", ErrInvalidSpec)
	}
	if len(input.Time) < 8 || len(input.Time) > 12 || input.Time[2] != ':' || input.Time[5] != ':' || (len(input.Time) > 8 && input.Time[8] != '.') || !allDigits(input.Time[:2]+input.Time[3:5]+input.Time[6:8]) {
		return time.Time{}, fmt.Errorf("%w: local at requires time HH:mm:ss with optional milliseconds", ErrInvalidSpec)
	}
	if len(input.Time) > 8 && (len(input.Time[9:]) < 1 || len(input.Time[9:]) > 3 || !allDigits(input.Time[9:])) {
		return time.Time{}, fmt.Errorf("%w: local at has invalid fractional seconds", ErrInvalidSpec)
	}
	if input.TimeZone == "" || strings.TrimSpace(input.TimeZone) != input.TimeZone || (input.TimeZone != "UTC" && !strings.Contains(input.TimeZone, "/")) {
		return time.Time{}, ErrInvalidTimeZone
	}
	loc, err := time.LoadLocation(input.TimeZone)
	if err != nil {
		return time.Time{}, ErrInvalidTimeZone
	}
	layout := "2006-01-02T15:04:05"
	if len(input.Time) > 8 {
		layout += "." + strings.Repeat("9", len(input.Time)-9)
	}
	parsed, err := time.ParseInLocation(layout, input.Date+"T"+input.Time, loc)
	if err != nil || parsed.Format("2006-01-02T15:04:05") != input.Date+"T"+input.Time[:8] {
		return time.Time{}, fmt.Errorf("%w: local at is not a real calendar date and time", ErrInvalidSpec)
	}
	parsed = parsed.UTC()
	if parsed.Year() < 1 || parsed.Year() > 9999 {
		return time.Time{}, fmt.Errorf("%w: scheduled time is outside four-digit year range", ErrInvalidSpec)
	}
	if !parsed.After(now) {
		return time.Time{}, ErrNotFuture
	}
	return parsed, nil
}

func cloneDurableRecord(record DurableRecord) DurableRecord {
	record.ScheduledAt = record.ScheduledAt.UTC()
	return record
}

func indexOf(ids []string, id string) int {
	for i, candidate := range ids {
		if candidate == id {
			return i
		}
	}
	return len(ids)
}

func scheduleSequence(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "schedule-%d", &n); err != nil {
		return 0
	}
	return n
}

// Kept as a tiny variable so the strict event decoder is easy to replace with
// a repository-wide decoder policy without changing the scheduler domain.
var jsonUnmarshal = func(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
