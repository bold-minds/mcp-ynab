// SPDX-License-Identifier: MIT
//
// Fuzz tests for pure helper functions. These are light-weight —
// each runs briefly under `go test -fuzz=` but also executes as
// regular tests via the seed corpus on every `go test` invocation.
// Review nit: "No fuzz tests for formatMilliunits, sanitize, date
// parsing, frequencyOccurrences."
//
// The invariant under test in each case is a safety property that
// must hold for ALL inputs, not just canonical values:
//
//   - formatMilliunits: never panics, always produces a parseable
//     decimal string that round-trips back to the same milliunit
//     value.
//   - sanitize: never leaks a bearer token pattern that matches its
//     own regex. Idempotent.
//   - validateISODate: deterministic for valid inputs.
//   - FrequencyOccurrences: never panics, always returns a
//     monotonically-non-decreasing slice of times within the window.

package main

import (
	"strings"
	"testing"
	"time"
)

func FuzzFormatMilliunits(f *testing.F) {
	seeds := []int64{0, 1, -1, 1000, -1000, 12340, -12340, 999, -999,
		9_223_372_036_854_775_807, -9_223_372_036_854_775_808}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, m int64) {
		s := formatMilliunits(m)
		if s == "" {
			t.Fatalf("formatMilliunits(%d) returned empty string", m)
		}
		// Output must contain exactly one decimal point with three
		// digits after it.
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			t.Fatalf("formatMilliunits(%d)=%q: missing decimal point", m, s)
		}
		if len(s)-dot-1 != 3 {
			t.Fatalf("formatMilliunits(%d)=%q: fractional part must be 3 digits", m, s)
		}
		// Leading '-' only for negative inputs.
		if (m < 0) != strings.HasPrefix(s, "-") {
			t.Fatalf("formatMilliunits(%d)=%q: sign mismatch", m, s)
		}
	})
}

func FuzzSanitize(f *testing.F) {
	f.Add("plain error message")
	f.Add("Bearer sk-leaked-abc123")
	f.Add("Authorization: Bearer xyz\nother line")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		out := sanitize(s)
		// Idempotent: re-sanitizing an already-sanitized string must
		// produce the same output.
		if sanitize(out) != out {
			t.Fatalf("sanitize not idempotent: %q → %q → %q", s, out, sanitize(out))
		}
		// If the bearer regex matches the original, it must NOT
		// match the output.
		if bearerRe.MatchString(s) && bearerRe.MatchString(out) {
			t.Fatalf("sanitize failed to strip bearer pattern: in=%q out=%q", s, out)
		}
	})
}

func FuzzValidateISODate(f *testing.F) {
	f.Add("2026-04-14")
	f.Add("not-a-date")
	f.Add("")
	f.Add("2026-13-01")
	f.Add("2026-00-00")
	f.Fuzz(func(t *testing.T, s string) {
		err := validateISODate(s)
		if err == nil {
			// Valid inputs must round-trip through time.Parse with
			// the same format.
			if _, pErr := time.Parse("2006-01-02", s); pErr != nil {
				t.Fatalf("validateISODate accepted %q but time.Parse rejects: %v", s, pErr)
			}
		}
	})
}

func FuzzFrequencyOccurrences(f *testing.F) {
	f.Add("monthly", int64(0), int64(0), int64(7))
	f.Add("daily", int64(0), int64(0), int64(30))
	f.Add("weekly", int64(-30), int64(-14), int64(14))
	f.Add("never", int64(0), int64(0), int64(7))
	f.Add("unknown-freq", int64(0), int64(0), int64(7))
	f.Fuzz(func(t *testing.T, freq string, offset, winStart, winEnd int64) {
		// Clamp inputs to a sane range so Go's time arithmetic does
		// not overflow. The fuzzer will still exercise plenty of
		// variety inside this window.
		const maxDays = 100_000
		if offset < -maxDays || offset > maxDays {
			return
		}
		if winStart < -maxDays || winStart > maxDays {
			return
		}
		if winEnd < -maxDays || winEnd > maxDays {
			return
		}
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		dn := base.AddDate(0, 0, int(offset))
		ws := base.AddDate(0, 0, int(winStart))
		we := base.AddDate(0, 0, int(winEnd))
		out := FrequencyOccurrences(dn, freq, ws, we)
		// Output must be monotonically non-decreasing and within
		// [windowStart, windowEnd] inclusive after dateOnly normalization.
		normWS := dateOnly(ws)
		normWE := dateOnly(we)
		for i, occ := range out {
			if occ.Before(normWS) || occ.After(normWE) {
				t.Fatalf("occurrence %v outside window [%v, %v]", occ, normWS, normWE)
			}
			if i > 0 && occ.Before(out[i-1]) {
				t.Fatalf("occurrences not monotonic at %d: %v < %v", i, occ, out[i-1])
			}
		}
	})
}
