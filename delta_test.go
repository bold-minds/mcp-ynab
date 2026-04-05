// SPDX-License-Identifier: MIT
package main

import (
	"sort"
	"sync"
	"testing"
)

// testEntity is a minimal struct used only in delta cache tests — having
// our own type lets us exercise generics without depending on the YNAB
// wire types.
type testEntity struct {
	ID      string
	Name    string
	Deleted bool
}

func testID(e testEntity) string      { return e.ID }
func testDeleted(e testEntity) bool   { return e.Deleted }
func sortByID(es []testEntity)        { sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID }) }

func TestDeltaCache_NilReceiver_NoPanic(t *testing.T) {
	t.Parallel()
	var c *deltaCache[testEntity] // nil
	if k := c.knowledge("plan-1"); k != 0 {
		t.Errorf("nil knowledge() should return 0, got %d", k)
	}
	if n := c.size("plan-1"); n != 0 {
		t.Errorf("nil size() should return 0, got %d", n)
	}
	// merge on nil returns deltas unchanged.
	in := []testEntity{{ID: "a", Name: "A"}}
	out := c.merge("plan-1", 42, in, testID, testDeleted)
	if len(out) != 1 || out[0].ID != "a" {
		t.Errorf("nil merge should passthrough deltas, got %+v", out)
	}
}

func TestDeltaCache_FirstCall_StoresKnowledgeAndEntities(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	items := []testEntity{
		{ID: "a", Name: "Alpha"},
		{ID: "b", Name: "Bravo"},
	}
	out := c.merge("plan-1", 100, items, testID, testDeleted)
	sortByID(out)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Errorf("unexpected merge output: %+v", out)
	}
	if k := c.knowledge("plan-1"); k != 100 {
		t.Errorf("knowledge not stored, got %d", k)
	}
	if n := c.size("plan-1"); n != 2 {
		t.Errorf("size wrong, got %d", n)
	}
}

func TestDeltaCache_SecondCall_MergesDeltas(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	// First call: populate with 3 entities.
	c.merge("plan-1", 100, []testEntity{
		{ID: "a", Name: "Alpha"},
		{ID: "b", Name: "Bravo"},
		{ID: "c", Name: "Charlie"},
	}, testID, testDeleted)

	// Second call: delta response contains 1 update, 1 new, 1 deletion.
	out := c.merge("plan-1", 200, []testEntity{
		{ID: "b", Name: "Bravo-UPDATED"},
		{ID: "d", Name: "Delta"},
		{ID: "a", Name: "", Deleted: true},
	}, testID, testDeleted)

	sortByID(out)
	if len(out) != 3 {
		t.Fatalf("expected 3 entities after merge, got %d: %+v", len(out), out)
	}
	// "a" was deleted; "b" was updated; "c" was unchanged (still present
	// from first call); "d" is new.
	wantIDs := []string{"b", "c", "d"}
	for i, w := range wantIDs {
		if out[i].ID != w {
			t.Errorf("pos %d: got %q want %q", i, out[i].ID, w)
		}
	}
	// Verify update took effect.
	for _, e := range out {
		if e.ID == "b" && e.Name != "Bravo-UPDATED" {
			t.Errorf("update did not replace entity: %+v", e)
		}
	}
	// Knowledge updated.
	if c.knowledge("plan-1") != 200 {
		t.Errorf("knowledge not updated, got %d", c.knowledge("plan-1"))
	}
}

func TestDeltaCache_DifferentPlansIsolated(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	c.merge("plan-1", 100, []testEntity{{ID: "a", Name: "PlanOne-A"}}, testID, testDeleted)
	c.merge("plan-2", 200, []testEntity{{ID: "a", Name: "PlanTwo-A"}}, testID, testDeleted)

	if c.knowledge("plan-1") != 100 || c.knowledge("plan-2") != 200 {
		t.Error("plan knowledge mixed across plan_ids")
	}
	if c.size("plan-1") != 1 || c.size("plan-2") != 1 {
		t.Error("plan sizes wrong")
	}
}

func TestDeltaCache_EmptyIDEntitiesSkipped(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	out := c.merge("plan-1", 100, []testEntity{
		{ID: "a", Name: "Good"},
		{ID: "", Name: "Bad"},
	}, testID, testDeleted)
	if len(out) != 1 || out[0].ID != "a" {
		t.Errorf("empty-id entity not skipped: %+v", out)
	}
}

// TestDeltaCache_SizeCapFlushesEntry is the L3 regression. When a merge
// would push the per-plan entity count above maxItemsPerPlanEntry, the
// cache flushes the entry entirely and returns the raw deltas. The next
// call then starts from knowledge=0 with a fresh state.
func TestDeltaCache_SizeCapFlushesEntry(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	// Build one merge call with maxItemsPerPlanEntry + 1 unique items.
	// The merge should flush the entry.
	n := maxItemsPerPlanEntry + 1
	items := make([]testEntity, n)
	for i := 0; i < n; i++ {
		items[i] = testEntity{ID: "e" + itoa(int64(i)), Name: "x"}
	}
	out := c.merge("plan-big", 500, items, testID, testDeleted)

	// merge should have returned the raw deltas (passthrough after flush).
	if len(out) != n {
		t.Errorf("expected %d raw deltas returned on cap-triggered flush, got %d", n, len(out))
	}
	// After the flush, the cache should NOT hold this plan anymore.
	if c.knowledge("plan-big") != 0 {
		t.Errorf("expected cache knowledge reset to 0 after flush, got %d", c.knowledge("plan-big"))
	}
	if c.size("plan-big") != 0 {
		t.Errorf("expected empty cache after flush, got size %d", c.size("plan-big"))
	}
}

func TestDeltaCache_BelowCapDoesNotFlush(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	// Exactly at the cap should NOT flush.
	n := maxItemsPerPlanEntry
	items := make([]testEntity, n)
	for i := 0; i < n; i++ {
		items[i] = testEntity{ID: "e" + itoa(int64(i)), Name: "x"}
	}
	_ = c.merge("plan-max", 100, items, testID, testDeleted)
	if c.size("plan-max") != n {
		t.Errorf("expected size %d at cap, got %d (should not flush at exactly the cap)", n, c.size("plan-max"))
	}
	if c.knowledge("plan-max") != 100 {
		t.Errorf("expected knowledge preserved when not flushed, got %d", c.knowledge("plan-max"))
	}
}

func TestDeltaCache_ConcurrentMergeAndRead(t *testing.T) {
	t.Parallel()
	c := newDeltaCache[testEntity]()
	const planID = "plan-concurrent"
	var wg sync.WaitGroup

	// 10 writers each merging 20 entities.
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			items := make([]testEntity, 20)
			for i := 0; i < 20; i++ {
				// Explicit base-10 formatting: the prior "rune('0'+i)"
				// scheme produced non-alphanumeric runes for i>=10
				// (:, ;, <, ...), which worked but was confusing in
				// failure output. Review finding L12.
				items[i] = testEntity{
					ID:   "w" + itoa(int64(worker)) + "-" + itoa(int64(i)),
					Name: "item",
				}
			}
			// All workers use the same server_knowledge value so the
			// monotonicity guard does not skip a worker whose goroutine
			// happens to run after a higher-knowledge worker. Real
			// YNAB usage is sequential, not concurrent, so the
			// out-of-order case is a synthetic-test artifact.
			//
			// This test's correctness depends on the monotonicity
			// guard using STRICTLY LESS THAN (`newKnowledge < s.knowledge`)
			// rather than `<=`. With `<=`, a second writer arriving
			// with the same knowledge value would be rejected and its
			// 20 entities would never enter the cache, producing a
			// flaky size!=200 failure. The guard in delta.go is `<`;
			// do not change to `<=` without rewriting this test.
			// Review nit.
			c.merge(planID, 1000, items, testID, testDeleted)
		}(w)
	}
	// 5 readers calling knowledge/size in a loop.
	for r := 0; r < 5; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = c.knowledge(planID)
				_ = c.size(planID)
			}
		}()
	}
	wg.Wait()
	// 10 workers × 20 items = 200 entities, all with unique ids.
	if c.size(planID) != 200 {
		t.Errorf("expected 200 entities after concurrent merges, got %d", c.size(planID))
	}
}
