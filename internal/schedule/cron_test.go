package schedule

import (
	"testing"
	"time"
)

func TestParseCronSpecValid(t *testing.T) {
	// Every accepted expression must parse and must not be rejected as
	// never-matching.
	valid := []string{
		"0 9 * * *",       // daily at 09:00
		"*/15 * * * *",    // every 15 minutes
		"0 0 * * 0",       // Sundays at midnight
		"0 0 1 1 *",       // Jan 1 at midnight
		"30 6-18 * * 1-5", // weekdays 06:30-18:30
		"0 12 * * 0,6",    // weekends at noon
		"5,35 7 * * *",    // 07:05 and 07:35
		"0 0 29 2 *",      // leap day (matches leap years)
		"0 0 * 2 *",       // every day in February
		"*/30 8-17 * * *", // every half hour 08:00-17:00
	}
	for _, spec := range valid {
		if _, err := parseCronSpec(spec); err != nil {
			t.Errorf("parseCronSpec(%q): unexpected error: %v", spec, err)
		}
	}
}

func TestParseCronSpecRejectsInvalid(t *testing.T) {
	invalid := []string{
		"",            // empty
		"0 9 * *",     // 4 fields
		"0 9 * * * *", // 6 fields (seconds not supported)
		"61 * * * *",  // minute out of range
		"0 24 * * *",  // hour out of range
		"0 0 0 * *",   // day 0
		"0 0 32 * *",  // day 32
		"0 0 * 13 *",  // month 13
		"0 0 * * 7",   // weekday 7
		"0 0 * * 8",   // weekday out of range
		"*/0 * * * *", // zero step
		"*/x * * * *", // non-numeric step
		"x 0 * * *",   // non-numeric value
		"0 0 31 2 *",  // Feb 31: never matches
		"0 0 30 2 *",  // Feb 30: never matches
		"0 0 31 4 *",  // Apr 31: never matches
		"0 0 5-3 * *", // reversed range
		"@daily",      // aliases not supported
		"* * * * MON", // names not supported
	}
	for _, spec := range invalid {
		if e, err := parseCronSpec(spec); err == nil {
			t.Errorf("parseCronSpec(%q) = %+v, want error", spec, e)
		}
	}
}

// nextAt is a helper that parses spec and returns the next fire strictly after
// after, failing the test on error.
func nextAt(t *testing.T, spec string, after time.Time) time.Time {
	t.Helper()
	e, err := parseCronSpec(spec)
	if err != nil {
		t.Fatalf("parseCronSpec(%q): %v", spec, err)
	}
	got, err := e.next(after)
	if err != nil {
		t.Fatalf("next(%q, %v): %v", spec, after, err)
	}
	return got
}

func TestCronNext(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		spec  string
		after time.Time
		want  time.Time
	}{
		// "0 9 * * *": next 09:00.
		{"0 9 * * *", time.Date(2026, 8, 19, 8, 30, 0, 0, utc), time.Date(2026, 8, 19, 9, 0, 0, 0, utc)},
		// Past 09:00 → next day.
		{"0 9 * * *", time.Date(2026, 8, 19, 9, 30, 0, 0, utc), time.Date(2026, 8, 20, 9, 0, 0, 0, utc)},
		// Strictly-after: 09:00:30 is not 09:00, so the next fire is tomorrow.
		{"0 9 * * *", time.Date(2026, 8, 19, 9, 0, 30, 0, utc), time.Date(2026, 8, 20, 9, 0, 0, 0, utc)},
		// "*/15 * * * *": every 15 minutes.
		{"*/15 * * * *", time.Date(2026, 8, 19, 10, 7, 0, 0, utc), time.Date(2026, 8, 19, 10, 15, 0, 0, utc)},
		// "30 6-18 * * 1-5": weekdays at :30. Friday 18:30 → Monday 06:30.
		{"30 6-18 * * 1-5", time.Date(2026, 8, 21, 18, 30, 0, 0, utc), time.Date(2026, 8, 24, 6, 30, 0, 0, utc)},
		// "0 12 * * 0,6": Sun/Sat at noon. Saturday 12:30 → Sunday 12:00.
		{"0 12 * * 0,6", time.Date(2026, 8, 22, 12, 30, 0, 0, utc), time.Date(2026, 8, 23, 12, 0, 0, 0, utc)},
		// "0 0 * * 0": Sundays. Tuesday midnight → Sunday midnight.
		{"0 0 * * 0", time.Date(2026, 8, 18, 0, 0, 0, 0, utc), time.Date(2026, 8, 23, 0, 0, 0, 0, utc)},
		// "0 0 1 * *": first of month. Aug 2 → Sep 1.
		{"0 0 1 * *", time.Date(2026, 8, 2, 0, 0, 0, 0, utc), time.Date(2026, 9, 1, 0, 0, 0, 0, utc)},
		// "0 0 29 2 *": leap day. After 2024-03-01 → 2028-02-29 (2028 is a leap year).
		{"0 0 29 2 *", time.Date(2024, 3, 1, 0, 0, 0, 0, utc), time.Date(2028, 2, 29, 0, 0, 0, 0, utc)},
		// Month-boundary daily: "0 23 * * *" at 23:00 rolls into the next month.
		{"0 23 * * *", time.Date(2026, 8, 31, 23, 30, 0, 0, utc), time.Date(2026, 9, 1, 23, 0, 0, 0, utc)},
	}
	for _, c := range cases {
		if got := nextAt(t, c.spec, c.after); !got.Equal(c.want) {
			t.Errorf("next(%q, %v) = %v, want %v", c.spec, c.after, got, c.want)
		}
	}
}
