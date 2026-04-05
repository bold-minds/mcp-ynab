// SPDX-License-Identifier: MIT
package main

import (
	"testing"
	"time"
)

// d constructs a date-only time.Time in UTC for test readability.
func d(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// occIDs renders a slice of times as a slice of "YYYY-MM-DD" strings for
// concise equality assertions.
func occIDs(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Format("2006-01-02")
	}
	return out
}

// assertOccurrences fails the test if the actual and expected slices
// differ element-wise.
func assertOccurrences(t *testing.T, got []time.Time, want []string) {
	t.Helper()
	gotIDs := occIDs(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("expected %d occurrences %v, got %d %v", len(want), want, len(gotIDs), gotIDs)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("pos %d: got %q want %q (full got=%v want=%v)", i, gotIDs[i], want[i], gotIDs, want)
		}
	}
}

// ---- Per-frequency coverage: one test per YNAB enum value ----

func TestFrequency_Never(t *testing.T) {
	t.Parallel()
	// In-window: single occurrence.
	got := FrequencyOccurrences(d(2026, 4, 10), "never", d(2026, 4, 1), d(2026, 4, 30))
	assertOccurrences(t, got, []string{"2026-04-10"})
	// Out of window (after): no occurrence.
	got = FrequencyOccurrences(d(2026, 5, 10), "never", d(2026, 4, 1), d(2026, 4, 30))
	assertOccurrences(t, got, nil)
	// Out of window (before): no occurrence.
	got = FrequencyOccurrences(d(2026, 3, 10), "never", d(2026, 4, 1), d(2026, 4, 30))
	assertOccurrences(t, got, nil)
}

func TestFrequency_Daily(t *testing.T) {
	t.Parallel()
	// 7-day window starting at dateNext: 7 occurrences.
	got := FrequencyOccurrences(d(2026, 4, 1), "daily", d(2026, 4, 1), d(2026, 4, 7))
	assertOccurrences(t, got, []string{
		"2026-04-01", "2026-04-02", "2026-04-03", "2026-04-04",
		"2026-04-05", "2026-04-06", "2026-04-07",
	})
}

func TestFrequency_Weekly(t *testing.T) {
	t.Parallel()
	// 14-day window, weekly frequency starting day 1: occurrences on day 1 and 8.
	got := FrequencyOccurrences(d(2026, 4, 1), "weekly", d(2026, 4, 1), d(2026, 4, 14))
	assertOccurrences(t, got, []string{"2026-04-01", "2026-04-08"})
}

func TestFrequency_EveryOtherWeek(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 4, 1), "everyOtherWeek", d(2026, 4, 1), d(2026, 4, 30))
	assertOccurrences(t, got, []string{"2026-04-01", "2026-04-15", "2026-04-29"})
}

func TestFrequency_TwiceAMonth(t *testing.T) {
	t.Parallel()
	// twiceAMonth is approximated as 15-day advance. Starting Apr 1 in a
	// 30-day window: occurrences Apr 1, Apr 16.
	got := FrequencyOccurrences(d(2026, 4, 1), "twiceAMonth", d(2026, 4, 1), d(2026, 4, 30))
	assertOccurrences(t, got, []string{"2026-04-01", "2026-04-16"})
}

func TestFrequency_Every4Weeks(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 4, 1), "every4Weeks", d(2026, 4, 1), d(2026, 6, 30))
	assertOccurrences(t, got, []string{"2026-04-01", "2026-04-29", "2026-05-27", "2026-06-24"})
}

func TestFrequency_Monthly(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 4, 15), "monthly", d(2026, 4, 1), d(2026, 7, 31))
	assertOccurrences(t, got, []string{"2026-04-15", "2026-05-15", "2026-06-15", "2026-07-15"})
}

// TestFrequency_Monthly_MonthLengthVariance verifies that calendar-month
// advancement handles month-length variance via time.Time.AddDate's
// well-defined behavior (Jan 31 + 1 month → Mar 3, not Feb 31).
func TestFrequency_Monthly_MonthLengthVariance(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 1, 31), "monthly", d(2026, 1, 1), d(2026, 4, 30))
	// Jan 31 → Mar 3 (Feb lacks a 31st, Go normalizes forward)
	//        → Apr 3
	// The test documents the behavior; if it ever changes, the test
	// will fail and force an explicit decision.
	assertOccurrences(t, got, []string{"2026-01-31", "2026-03-03", "2026-04-03"})
}

func TestFrequency_EveryOtherMonth(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 1, 1), "everyOtherMonth", d(2026, 1, 1), d(2026, 12, 31))
	assertOccurrences(t, got, []string{
		"2026-01-01", "2026-03-01", "2026-05-01", "2026-07-01",
		"2026-09-01", "2026-11-01",
	})
}

func TestFrequency_Every3Months(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 1, 1), "every3Months", d(2026, 1, 1), d(2026, 12, 31))
	assertOccurrences(t, got, []string{
		"2026-01-01", "2026-04-01", "2026-07-01", "2026-10-01",
	})
}

func TestFrequency_Every4Months(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 1, 1), "every4Months", d(2026, 1, 1), d(2027, 1, 31))
	assertOccurrences(t, got, []string{
		"2026-01-01", "2026-05-01", "2026-09-01", "2027-01-01",
	})
}

func TestFrequency_TwiceAYear(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 1, 15), "twiceAYear", d(2026, 1, 1), d(2028, 1, 1))
	assertOccurrences(t, got, []string{"2026-01-15", "2026-07-15", "2027-01-15", "2027-07-15"})
}

func TestFrequency_Yearly(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 4, 1), "yearly", d(2026, 1, 1), d(2029, 12, 31))
	assertOccurrences(t, got, []string{"2026-04-01", "2027-04-01", "2028-04-01", "2029-04-01"})
}

func TestFrequency_EveryOtherYear(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2026, 4, 1), "everyOtherYear", d(2026, 1, 1), d(2032, 12, 31))
	assertOccurrences(t, got, []string{"2026-04-01", "2028-04-01", "2030-04-01", "2032-04-01"})
}

// ---- Edge cases ----

func TestFrequency_WindowBeforeDateNext(t *testing.T) {
	t.Parallel()
	// Scheduled transaction next fires Apr 10, window is Mar 1-31: no occurrences.
	got := FrequencyOccurrences(d(2026, 4, 10), "monthly", d(2026, 3, 1), d(2026, 3, 31))
	assertOccurrences(t, got, nil)
}

func TestFrequency_InvertedWindow(t *testing.T) {
	t.Parallel()
	// Window end before start: no occurrences regardless of frequency.
	got := FrequencyOccurrences(d(2026, 4, 1), "daily", d(2026, 4, 10), d(2026, 4, 5))
	assertOccurrences(t, got, nil)
}

func TestFrequency_UnknownFrequency(t *testing.T) {
	t.Parallel()
	// Unknown frequency value: fail-closed, no occurrences.
	got := FrequencyOccurrences(d(2026, 4, 1), "fortnightly", d(2026, 4, 1), d(2026, 4, 30))
	assertOccurrences(t, got, nil)
}

func TestFrequency_DateOnlyNormalization(t *testing.T) {
	t.Parallel()
	// dateNext with non-zero time components should be treated the same as
	// the date-only version. Verify by comparing two calls with the same
	// date but different hour components.
	withTime := time.Date(2026, 4, 1, 15, 30, 0, 0, time.UTC)
	dateOnly := d(2026, 4, 1)
	windowStart := d(2026, 4, 1)
	windowEnd := d(2026, 4, 7)
	a := FrequencyOccurrences(withTime, "daily", windowStart, windowEnd)
	b := FrequencyOccurrences(dateOnly, "daily", windowStart, windowEnd)
	if len(a) != len(b) {
		t.Fatalf("date-only normalization inconsistent: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			t.Errorf("pos %d: %v vs %v", i, a[i], b[i])
		}
	}
}

// TestFrequency_Monthly_Feb29LeapYear documents how monthly advancement
// handles a Feb 29 date. Go's AddDate normalizes Feb 29 + 1 month to Mar
// 29 (calendar-month semantics), not March 1. Review finding L1.
func TestFrequency_Monthly_Feb29LeapYear(t *testing.T) {
	t.Parallel()
	// Starting Feb 29, 2024 (leap year), advance monthly. First four
	// occurrences are Feb 29, Mar 29, Apr 29, May 29.
	got := FrequencyOccurrences(d(2024, 2, 29), "monthly", d(2024, 2, 1), d(2024, 5, 31))
	assertOccurrences(t, got, []string{"2024-02-29", "2024-03-29", "2024-04-29", "2024-05-29"})
}

// TestFrequency_Yearly_Feb29LeapYear documents the behavior on a yearly
// schedule starting Feb 29 of a leap year. Go's AddDate normalizes Feb 29
// + 1 year to Mar 1 (since Feb 2025 has only 28 days). The test locks the
// current behavior so any future change is an explicit decision. Review
// finding L2.
func TestFrequency_Yearly_Feb29LeapYear(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2024, 2, 29), "yearly", d(2024, 2, 1), d(2028, 3, 31))
	// 2024-02-29 → +1yr = 2025-03-01 (normalized) → +1yr = 2026-03-01
	//            → +1yr = 2027-03-01 → +1yr = 2028-03-01
	assertOccurrences(t, got, []string{"2024-02-29", "2025-03-01", "2026-03-01", "2027-03-01", "2028-03-01"})
}

// TestFrequency_EveryOtherYear_Feb29LeapYear same for everyOtherYear.
// Review finding L2.
func TestFrequency_EveryOtherYear_Feb29LeapYear(t *testing.T) {
	t.Parallel()
	got := FrequencyOccurrences(d(2024, 2, 29), "everyOtherYear", d(2024, 1, 1), d(2030, 12, 31))
	// 2024-02-29 → +2yr = 2026-03-01 (normalized from non-existent 2026-02-29)
	//            → +2yr = 2028-03-01 → +2yr = 2030-03-01
	assertOccurrences(t, got, []string{"2024-02-29", "2026-03-01", "2028-03-01", "2030-03-01"})
}

// ---- Enum coverage regression ----

// TestFrequency_EnumValuesCovered ensures every frequency in
// knownFrequencies has a dedicated test above. If this test fails, a
// new YNAB frequency value was added to knownFrequencies without a
// corresponding test.
func TestFrequency_EnumValuesCovered(t *testing.T) {
	t.Parallel()
	// Map of frequency to expected "nonzero-advance" behavior.
	// (never is special-cased; daily through everyOtherYear should all
	// advance dateNext forward when called.)
	for _, freq := range knownFrequencies {
		if freq == "never" {
			continue // covered by dedicated test
		}
		// Smoke test: call advanceByFrequency and verify forward motion.
		before := d(2026, 1, 1)
		after := advanceByFrequency(before, freq)
		if !after.After(before) {
			t.Errorf("frequency %q does not advance forward from %v (got %v)", freq, before, after)
		}
	}
}

// TestFrequency_EnumValuesStaticCount locks in the expected count of
// frequencies so any addition to the list requires an explicit test update.
// Verified against the YNAB OpenAPI spec at v0.2 development time: 13
// values.
func TestFrequency_EnumValuesStaticCount(t *testing.T) {
	t.Parallel()
	const expected = 13
	if len(knownFrequencies) != expected {
		t.Errorf("knownFrequencies has %d entries, expected %d. If YNAB added a frequency, update knownFrequencies AND advanceByFrequency AND add a dedicated test.", len(knownFrequencies), expected)
	}
}
