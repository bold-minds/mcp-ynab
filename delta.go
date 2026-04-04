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
// No TTL, no eviction, no size cap in v0.2.0-rc.1 — for a single-session
// MCP process this is acceptable. Future versions may add bounded
// caches if the memory footprint becomes an issue for long-running
// sessions.
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
