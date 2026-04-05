// SPDX-License-Identifier: MIT
//
// delta.go implements in-process delta-sync caching for YNAB's
// last_knowledge_of_server pattern. It is purely a bandwidth/latency
// optimization — not a correctness requirement. When a read handler has
// a cache entry for (plan_id, endpoint), it passes last_knowledge_of_server
// on subsequent calls and receives only the entities that have changed
// since the last fetch. The handler then merges the deltas into the
// cached set and returns the merged view to the caller.
//
// Scope (per v0.2 brief, A4 decision):
//
//   - Delta sync applies ONLY to unfiltered read endpoints:
//     list_accounts, list_categories, list_plans, get_month, and
//     list_transactions with no filter arguments.
//   - Filtered list_transactions (scoped by account/category/payee, or
//     filtered by since_date/type) always does a full fetch. YNAB's
//     documentation does not clearly specify delta semantics on filtered
//     endpoints, and shipping wrong behavior is worse than shipping
//     less-optimal behavior.
//
// Lifetime: in-memory only. The cache is tied to the process; it dies
// when the MCP server child process exits. Nothing persists to disk.
//
// Memory bound (review finding L3): each per-plan entry is capped at
// maxItemsPerPlanEntry entities. When a merge would push the count above
// the cap, the cache flushes the entry (knowledge reset to 0) rather
// than evicting individual items. The next read does a full refetch and
// starts a fresh delta chain. A conservative cap (20,000) means this
// only fires on pathological plans or very long-lived sessions and even
// then the worst case is one extra full fetch.
//
// Security: the cache holds YNAB wire-entity data that was already
// returned to the MCP tool layer. It never holds secrets, tokens, or
// anything the MCP client has not already seen. Entries are keyed by
// plan_id which is a YNAB UUID, not PII.

package main

import (
	"sync"
)

// deltaCache is a generic, thread-safe delta-sync cache for a single YNAB
// entity type T. One deltaCache is created per entity type on the Client
// (e.g., one for wireAccount, one for wireTransaction). Entries within a
// cache are keyed by plan_id.
//
// A nil *deltaCache is safe to call: all methods degrade to "no caching"
// when the receiver is nil, so read handlers can invoke cache operations
// unconditionally without nil checks. Tests that don't want caching
// construct Clients with nil caches.
type deltaCache[T any] struct {
	mu    sync.Mutex
	plans map[string]*deltaPlanState[T]
}

// deltaPlanState holds the cached state for a single YNAB plan: the
// latest server_knowledge we saw, and the full set of entities keyed by
// their YNAB id.
type deltaPlanState[T any] struct {
	knowledge int64
	items     map[string]T
}

// maxItemsPerPlanEntry is the per-(plan, entity-type) cache size cap.
// At roughly 500 bytes per entity, 20,000 items per entry × two entries
// per plan (accounts + transactions) × a small number of plans stays
// under ~20 MB total, which is acceptable for a session-scoped MCP
// process. When the cap fires, the affected entry is flushed and the
// next read starts a fresh delta chain from server_knowledge=0.
const maxItemsPerPlanEntry = 20_000

// newDeltaCache constructs an empty cache.
func newDeltaCache[T any]() *deltaCache[T] {
	return &deltaCache[T]{
		plans: make(map[string]*deltaPlanState[T]),
	}
}

// knowledge returns the cached YNAB server_knowledge value for planID.
// Returns 0 when the receiver is nil, or when the plan has no cache
// entry yet (a first-time call). Handlers use this to decide whether to
// pass last_knowledge_of_server on the outgoing request.
func (c *deltaCache[T]) knowledge(planID string) int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.plans[planID]; ok {
		return s.knowledge
	}
	return 0
}

// merge applies a delta response to the cache. deltas is the entity slice
// returned by YNAB (when last_knowledge_of_server was passed, this slice
// contains only changed entities; on a first call with knowledge=0, it
// contains everything). idFn extracts the entity id; deletedFn reports
// whether the entity was deleted in this delta (should be removed from
// the cache).
//
// Returns the complete merged item set as a new slice. Callers should
// use the returned slice as the canonical entity list for the handler
// response; the original `deltas` arg only contains the deltas and is
// not authoritative.
//
// If the receiver is nil (cache disabled), merge returns deltas as-is —
// in that case the caller is expected to treat deltas as the complete
// set (which is true when last_knowledge_of_server was NOT passed).
func (c *deltaCache[T]) merge(
	planID string,
	newKnowledge int64,
	deltas []T,
	idFn func(T) string,
	deletedFn func(T) bool,
) []T {
	if c == nil {
		return deltas
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	s, ok := c.plans[planID]
	if !ok {
		s = &deltaPlanState[T]{items: make(map[string]T)}
		c.plans[planID] = s
	}

	// Monotonicity guard: if YNAB ever returns a smaller server_knowledge
	// than we have cached (e.g. a retry of a cached upstream response or
	// a backend anomaly), refuse to regress. Returning the existing full
	// set without applying the out-of-order deltas is safer than
	// re-emitting stale entities as "new" deltas. Review finding M2.
	if newKnowledge > 0 && newKnowledge < s.knowledge {
		out := make([]T, 0, len(s.items))
		for _, item := range s.items {
			out = append(out, item)
		}
		return out
	}

	for _, item := range deltas {
		id := idFn(item)
		if id == "" {
			// Defensive: skip entities with no id. This should never happen
			// with real YNAB data, but we don't want a malformed response
			// to corrupt the cache via empty-string keys.
			continue
		}
		if deletedFn(item) {
			delete(s.items, id)
		} else {
			s.items[id] = item
		}
	}

	// Size cap: if the post-merge size would exceed the bound, return the
	// merged set to this caller (so they get a correct full view instead
	// of a mere delta slice) and then flush the cache entry so the NEXT
	// call starts a fresh delta chain from knowledge=0. Previously this
	// returned the raw deltas, which would silently give the caller only
	// the incremental slice when a subsequent call had passed
	// last_knowledge_of_server. Review findings M1 and L3.
	if len(s.items) > maxItemsPerPlanEntry {
		out := make([]T, 0, len(s.items))
		for _, item := range s.items {
			out = append(out, item)
		}
		delete(c.plans, planID)
		return out
	}

	s.knowledge = newKnowledge

	// Return the full merged set as a slice. Order is not preserved
	// (Go map iteration is randomized); callers that need ordered
	// output should sort at the tool-output boundary.
	out := make([]T, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out
}

// size returns the current entity count cached for planID. Used by tests
// and potentially by future observability endpoints. Returns 0 when the
// receiver is nil.
func (c *deltaCache[T]) size(planID string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.plans[planID]; ok {
		return len(s.items)
	}
	return 0
}
