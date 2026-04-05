// SPDX-License-Identifier: MIT
//
// clock.go centralizes the one package-level source of "now" so tests
// that need deterministic dates can override it. Every production code
// path in this binary that would otherwise call time.Now() should call
// nowUTC() instead.
//
// The clock function is stored in an atomic.Value so concurrent callers
// are race-safe under -race — a future test author who forgets the
// non-parallel constraint won't corrupt state or trip the race detector.
// Review finding L4.
//
// Usage pattern in tests:
//
//	func TestThing(t *testing.T) {
//	    setNowUTC(func() time.Time { return time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC) })
//	    t.Cleanup(resetNowUTC)
//	    // ... test body
//	}

package main

import (
	"sync/atomic"
	"time"
)

// clockFn is the concrete type stored inside clockHolder. atomic.Value
// requires every Store call to use the same concrete type, so we wrap
// the function in a named struct.
type clockFn struct {
	fn func() time.Time
}

var clockHolder atomic.Value

func init() {
	clockHolder.Store(clockFn{fn: defaultNowUTC})
}

// nowUTC returns the current time in UTC via whichever clock function
// is currently installed. Production code calls this instead of
// time.Now().UTC() so tests can override for determinism.
func nowUTC() time.Time {
	return clockHolder.Load().(clockFn).fn()
}

// setNowUTC installs a new clock function atomically. Safe to call
// concurrently with other clock readers.
func setNowUTC(fn func() time.Time) {
	clockHolder.Store(clockFn{fn: fn})
}

// resetNowUTC restores the production clock. Safe to call concurrently
// with other clock readers; tests use this as their t.Cleanup.
func resetNowUTC() {
	clockHolder.Store(clockFn{fn: defaultNowUTC})
}

// defaultNowUTC is the production implementation. Exposed as a named
// symbol so other code can reference it explicitly (e.g. for diagnostic
// logging or tests that want to restore it without going through
// resetNowUTC).
func defaultNowUTC() time.Time {
	return time.Now().UTC()
}
