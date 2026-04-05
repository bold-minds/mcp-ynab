// SPDX-License-Identifier: MIT
package main

import (
	"net/url"
	"strings"
	"testing"
)

// TestValidateUUIDOrLookup is the boundary-validator unit test for the
// broader UUID-validation review finding. It exercises the three
// dimensions of the function's contract:
//
//  1. Canonical UUIDs of either case pass.
//  2. Everything non-canonical fails (wrong length, wrong dash positions,
//     non-hex digits, control characters, path-breakout chars).
//  3. Lookup keywords ("default" / "last-used") are accepted if and only
//     if allowLookup is true.
func TestValidateUUIDOrLookup(t *testing.T) {
	t.Parallel()

	type want int
	const (
		ok want = iota
		fail
	)

	cases := []struct {
		name        string
		s           string
		allowLookup bool
		want        want
	}{
		// --- canonical form ---
		{"canonical lowercase", "11111111-2222-3333-4444-555555555555", false, ok},
		{"canonical uppercase", "ABCDEF01-ABCD-ABCD-ABCD-ABCDEFABCDEF", false, ok},
		{"canonical mixed case", "AbCdEf01-AbCd-AbCd-AbCd-AbCdEfAbCdEf", false, ok},
		{"testPlanID constant", testPlanID, false, ok},

		// --- length / shape ---
		{"empty", "", false, fail},
		{"too short", "1234", false, fail},
		{"35 chars", "11111111-1111-1111-1111-11111111111", false, fail},
		{"37 chars", "11111111-1111-1111-1111-1111111111111", false, fail},
		{"missing dash at 8", "111111111-111-1111-1111-111111111111", false, fail},
		{"missing dash at 13", "11111111-11111-111-1111-111111111111", false, fail},
		{"missing dash at 18", "11111111-1111-11111-111-111111111111", false, fail},
		{"missing dash at 23", "11111111-1111-1111-11111-11111111111", false, fail},
		{"dash in hex position", "1111111--1111-1111-1111-111111111111", false, fail},

		// --- non-hex content ---
		{"letter g in hex slot", "g1111111-1111-1111-1111-111111111111", false, fail},
		{"space in middle", "11111111-1111-1111-1111-11111111111 ", false, fail},
		{"leading space", " 1111111-1111-1111-1111-111111111111", false, fail},

		// --- control chars and path-breakout attempts ---
		// These would otherwise reach url.PathEscape — validator rejects.
		// NB: the length check shortcuts most of these, but non-36-char
		// path-injection attempts (like "../plans/other") are covered by
		// the length gate, and 36-char mutations below exercise the
		// per-char check.
		{"36 chars with slash", "11111111-1111-1111-1111-1111111/1111", false, fail},
		{"36 chars with ?", "11111111-1111-1111-1111-1111111?1111", false, fail},
		{"36 chars with #", "11111111-1111-1111-1111-1111111#1111", false, fail},
		{"36 chars with CR", "11111111-1111-1111-1111-11111111\r111", false, fail},
		{"36 chars with NUL", "11111111-1111-1111-1111-1111111\x00111", false, fail},
		{"36 chars with %", "11111111-1111-1111-1111-1111111%1111", false, fail},

		// --- lookup keywords ---
		{"default lookup allowed", "default", true, ok},
		{"last-used lookup allowed", "last-used", true, ok},
		{"default lookup rejected when not allowed", "default", false, fail},
		{"last-used lookup rejected when not allowed", "last-used", false, fail},
		{"case-sensitive lookup (Default)", "Default", true, fail},
		{"case-sensitive lookup (LAST-USED)", "LAST-USED", true, fail},
		{"arbitrary lookup-ish string rejected", "latest", true, fail},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateUUIDOrLookup(c.s, c.allowLookup)
			gotOK := err == nil
			wantOK := c.want == ok
			if gotOK != wantOK {
				t.Errorf("validateUUIDOrLookup(%q, allowLookup=%v): want ok=%v, got err=%v",
					c.s, c.allowLookup, wantOK, err)
			}
		})
	}
}

// TestValidatePlanID verifies the call-site wrapper carries the
// allowLookup=true behavior and prefixes errors with "plan_id:".
func TestValidatePlanID(t *testing.T) {
	t.Parallel()
	if err := validatePlanID("default"); err != nil {
		t.Errorf("validatePlanID must accept the default lookup keyword: %v", err)
	}
	if err := validatePlanID("last-used"); err != nil {
		t.Errorf("validatePlanID must accept the last-used lookup keyword: %v", err)
	}
	if err := validatePlanID(testPlanID); err != nil {
		t.Errorf("validatePlanID must accept a canonical UUID: %v", err)
	}
	err := validatePlanID("nope")
	if err == nil || !strings.Contains(err.Error(), "plan_id:") {
		t.Errorf("validatePlanID must prefix errors with 'plan_id:', got %v", err)
	}
}

// TestValidateEntityID verifies the call-site wrapper rejects lookup
// keywords (they are plan-only) and prefixes errors with the supplied
// field name.
func TestValidateEntityID(t *testing.T) {
	t.Parallel()
	if err := validateEntityID("account_id", "default"); err == nil {
		t.Error("validateEntityID must reject the 'default' lookup keyword: it is plan-only")
	}
	if err := validateEntityID("account_id", "last-used"); err == nil {
		t.Error("validateEntityID must reject the 'last-used' lookup keyword: it is plan-only")
	}
	if err := validateEntityID("category_id", testCategoryID); err != nil {
		t.Errorf("validateEntityID must accept a canonical UUID: %v", err)
	}
	err := validateEntityID("transaction_id", "nope")
	if err == nil || !strings.Contains(err.Error(), "transaction_id:") {
		t.Errorf("validateEntityID must prefix errors with the field name, got %v", err)
	}
}

// FuzzValidateUUIDOrLookup asserts two invariants on arbitrary input:
//
//  1. The validator never panics. Any input is either accepted or
//     rejected with a normal error; no input can crash the process.
//  2. Every accepted (non-lookup) string round-trips through
//     url.PathEscape unchanged. This is the core property the validator
//     exists to protect: a string the validator says is safe must be
//     safe to drop into a URL path segment without escaping. If that
//     property ever breaks, the validator is lying and a future
//     validator revision is introducing escaping-sensitive characters.
//
// The fuzzer explores both the allowLookup=true and allowLookup=false
// branches; the lookup keywords are exempted from the round-trip check
// because they are not UUIDs by design.
func FuzzValidateUUIDOrLookup(f *testing.F) {
	// Seed with representative inputs.
	seeds := []struct {
		s           string
		allowLookup bool
	}{
		{"", false},
		{"default", true},
		{"last-used", true},
		{"default", false},
		{testPlanID, false},
		{testAccountID, false},
		{"11111111-1111-4111-8111-111111111111", true},
		{"not-a-uuid", false},
		{"////", false},
		{"\x00\x01\x02", false},
	}
	for _, s := range seeds {
		f.Add(s.s, s.allowLookup)
	}

	f.Fuzz(func(t *testing.T, s string, allowLookup bool) {
		err := validateUUIDOrLookup(s, allowLookup)
		if err != nil {
			return
		}
		// Accepted. If it is a lookup keyword, we exempt it from the
		// round-trip — lookup strings are not intended as URL path
		// segments.
		if allowLookup && (s == "default" || s == "last-used") {
			return
		}
		// Otherwise the accepted string MUST be a URL-safe path segment:
		// url.PathEscape of an accepted id returns the string unchanged.
		if escaped := url.PathEscape(s); escaped != s {
			t.Fatalf("validateUUIDOrLookup accepted %q but url.PathEscape escapes it to %q — validator is lying about URL safety", s, escaped)
		}
	})
}
