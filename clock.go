// SPDX-License-Identifier: MIT
//
// clock.go centralizes the one package-level source of "now" so tests
// that need deterministic dates can override it. Every production code
// path in this binary that would otherwise call time.Now() should call
// nowUTC() instead.
//
// Review finding L14: before this, ~7 time.Now().UTC() calls were
// scattered across tools.go, tools_tasks.go, and tools_writes.go,
// making time-sensitive handlers hard to unit test deterministically.
// Now there's one var to override.
//
// Usage pattern in tests:
//
//	func TestThing(t *testing.T) {
//	    nowUTC = func() time.Time { return time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC) }
//	    t.Cleanup(func() { nowUTC = defaultNowUTC })
//	    // ... test body
//	}
//
// The overridable variable is intentionally NOT a goroutine-safe
// swap-via-atomic; tests that need determinism run sequentially via
// t.Cleanup, not in parallel with production code.

package main

import "time"

// nowUTC returns the current time in UTC. Production code calls this
// instead of time.Now().UTC() so tests can override for determinism.
var nowUTC = defaultNowUTC

// defaultNowUTC is the production implementation. Kept as a named
// symbol so tests can restore it via t.Cleanup without referencing the
// stdlib expression literal.
func defaultNowUTC() time.Time {
	return time.Now().UTC()
}
