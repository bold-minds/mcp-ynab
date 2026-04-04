// SPDX-License-Identifier: MIT
//
// recurrence.go is a pure-function module for expanding YNAB scheduled
// transaction frequencies into concrete occurrence dates within a window.
// It has no dependency on the YNAB HTTP client, no I/O, and no mutable
// state — it is deliberately isolated so it can be unit-tested directly
// and reused by any task-shaped tool that needs cash-flow projection
// (currently ynab_status.scheduled_next_7_days; future tools may also).
//
// YNAB's 13 frequency enum values are verified against the OpenAPI spec
// at mcp-ynab development time. The list is asserted by TestFrequency_
// EnumValuesCovered in recurrence_test.go — if YNAB ever adds a new
// frequency, that test will fail and force an explicit update here.
//
// Approximation notes:
//
//   - Frequencies based on fixed day-counts (daily, weekly, everyOtherWeek,
//     every4Weeks) are mathematically exact.
//   - Frequencies based on calendar months (monthly, everyOtherMonth,
//     every3Months, every4Months, twiceAYear, yearly, everyOtherYear) use
//     time.Time.AddDate which handles month-length variance correctly
//     (e.g., Jan 31 + 1 month = Mar 3, not Feb 31).
//   - `twiceAMonth` is approximated as 15-day advance. The YNAB app
//     actually uses two user-chosen days per month (e.g., the 1st and
//     15th, or the 5th and 20th), but the API does not expose the anchor
//     days. For 7-day dashboard windows the approximation under-counts
//     by at most ~1 occurrence per window, which is acceptable for the
//     status dashboard use case. For precise cash-flow projection beyond
//     7 days, a caller should use YNAB's own scheduled_transactions list
//     directly.
//   - `never` emits at most one occurrence (dateNext itself, if within
//     the window). It represents a one-time future-dated transaction.

package main

import "time"

// FrequencyOccurrences returns every occurrence date of a scheduled
// transaction within the inclusive window [windowStart, windowEnd],
// sorted ascending.
//
// dateNext is the next scheduled occurrence as reported by YNAB (the
// date_next field on a ScheduledTransactionSummary). Earlier occurrences
// are treated as having already fired and are not emitted.
//
// frequency is one of YNAB's 13 enum values; unknown values produce an
// empty slice (fail-closed rather than fail-loud — callers expecting
// strict validation should check the frequency against knownFrequencies
// before calling).
func FrequencyOccurrences(dateNext time.Time, frequency string, windowStart, windowEnd time.Time) []time.Time {
	if windowEnd.Before(windowStart) {
		return nil
	}
	// Fail-closed on unknown frequencies: if YNAB ever adds a new enum
	// value and our knownFrequencies list hasn't been updated, callers
	// get an empty slice rather than a silent 1-occurrence emission.
	// The enum-coverage test in recurrence_test.go will alert on any
	// drift in the knownFrequencies list.
	if !isKnownFrequency(frequency) {
		return nil
	}
	// Normalize to date-only (drop time of day). YNAB scheduled dates are
	// date-only; any hour/minute/second component on dateNext is an
	// artifact of how the caller parsed the ISO date and should not
	// affect occurrence math.
	dateNext = dateOnly(dateNext)
	windowStart = dateOnly(windowStart)
	windowEnd = dateOnly(windowEnd)

	if frequency == "never" {
		if !dateNext.Before(windowStart) && !dateNext.After(windowEnd) {
			return []time.Time{dateNext}
		}
		return nil
	}

	// Safety cap on iteration: 2000 steps covers >5 years of daily
	// occurrences, which is more than any realistic window for a YNAB
	// scheduled transaction (YNAB's own docs cap scheduled transactions
	// at 5 years into the future).
	const maxIter = 2000
	out := make([]time.Time, 0, 16)
	cur := dateNext
	for i := 0; i < maxIter; i++ {
		if cur.After(windowEnd) {
			break
		}
		if !cur.Before(windowStart) {
			out = append(out, cur)
		}
		next := advanceByFrequency(cur, frequency)
		// Infinite-loop guard: if advanceByFrequency returns a date that
		// is not strictly after the current value, something is wrong
		// (unknown frequency, zero-period bug, etc.) and we must stop.
		if !next.After(cur) {
			break
		}
		cur = next
	}
	return out
}

// advanceByFrequency returns the next occurrence date after t for the
// given frequency. Returns t unchanged for unknown frequencies — callers
// must handle that case (FrequencyOccurrences's loop guard does).
func advanceByFrequency(t time.Time, frequency string) time.Time {
	switch frequency {
	case "daily":
		return t.AddDate(0, 0, 1)
	case "weekly":
		return t.AddDate(0, 0, 7)
	case "everyOtherWeek":
		return t.AddDate(0, 0, 14)
	case "twiceAMonth":
		// Approximation: 15-day advance. See the file-level doc comment
		// for the rationale and limitation.
		return t.AddDate(0, 0, 15)
	case "every4Weeks":
		return t.AddDate(0, 0, 28)
	case "monthly":
		return t.AddDate(0, 1, 0)
	case "everyOtherMonth":
		return t.AddDate(0, 2, 0)
	case "every3Months":
		return t.AddDate(0, 3, 0)
	case "every4Months":
		return t.AddDate(0, 4, 0)
	case "twiceAYear":
		return t.AddDate(0, 6, 0)
	case "yearly":
		return t.AddDate(1, 0, 0)
	case "everyOtherYear":
		return t.AddDate(2, 0, 0)
	case "never":
		// Already handled at call site; return a far-future date so any
		// caller that bypasses the early return does not loop forever.
		return t.AddDate(100, 0, 0)
	default:
		// Unknown frequency — return unchanged so FrequencyOccurrences's
		// loop guard breaks out.
		return t
	}
}

// isKnownFrequency reports whether freq is in the canonical YNAB
// frequency enum.
func isKnownFrequency(freq string) bool {
	for _, k := range knownFrequencies {
		if k == freq {
			return true
		}
	}
	return false
}

// knownFrequencies is the complete set of YNAB scheduled-transaction
// frequency enum values as of the YNAB OpenAPI spec verified at v0.2
// development time. Any change to this list requires a matching update
// to advanceByFrequency AND the enum coverage test.
var knownFrequencies = []string{
	"never",
	"daily",
	"weekly",
	"everyOtherWeek",
	"twiceAMonth",
	"every4Weeks",
	"monthly",
	"everyOtherMonth",
	"every3Months",
	"every4Months",
	"twiceAYear",
	"yearly",
	"everyOtherYear",
}

// dateOnly returns t with its time components zeroed. Used to normalize
// inputs so occurrence comparisons are day-granular, matching YNAB's own
// day-granular date model.
func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
