package sidecar

// bounded_map_test.go — unit tests for the generic insertion-order LRU
// map used to cap the per-message tracking structures on Sidecar
// (writtenMessages / textByMessage / msgCreatedAtMs / ttftByMessage).
// Issue #1846.

import (
	"strconv"
	"testing"
)

func TestBoundedMap_SetAndGet(t *testing.T) {
	b := newBoundedMap[string](8)
	b.set("a", "alpha")
	if v, ok := b.get("a"); !ok || v != "alpha" {
		t.Errorf("get(\"a\") = (%q, %v), want (\"alpha\", true)", v, ok)
	}
	if v, ok := b.get("missing"); ok || v != "" {
		t.Errorf("get(\"missing\") = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestBoundedMap_HasAndLen(t *testing.T) {
	b := newBoundedMap[int](8)
	if b.has("a") {
		t.Errorf("has on empty map = true, want false")
	}
	if b.len() != 0 {
		t.Errorf("len on empty map = %d, want 0", b.len())
	}
	b.set("a", 1)
	b.set("b", 2)
	if !b.has("a") || !b.has("b") {
		t.Errorf("has(a)=%v has(b)=%v, want both true", b.has("a"), b.has("b"))
	}
	if b.len() != 2 {
		t.Errorf("len after 2 sets = %d, want 2", b.len())
	}
}

func TestBoundedMap_Delete(t *testing.T) {
	b := newBoundedMap[int](8)
	b.set("a", 1)
	b.set("b", 2)
	b.del("a")
	if b.has("a") {
		t.Errorf("has(\"a\") after del = true, want false")
	}
	if b.len() != 1 {
		t.Errorf("len after del = %d, want 1", b.len())
	}
	// del on missing key is a no-op.
	b.del("missing")
	if b.len() != 1 {
		t.Errorf("len after del missing = %d, want 1", b.len())
	}
}

func TestBoundedMap_UpdateInPlaceDoesNotChangeLenOrPosition(t *testing.T) {
	// Setting an existing key updates the value but does NOT refresh its
	// position in the eviction order — this is the documented behaviour
	// for streaming text-part updates where re-setting the same in-flight
	// message-id should not prolong its retention relative to others.
	b := newBoundedMap[string](3)
	b.set("a", "1")
	b.set("b", "2")
	b.set("c", "3")
	// Overwrite "a" — len must not grow, value must update.
	b.set("a", "updated")
	if b.len() != 3 {
		t.Errorf("len after update = %d, want 3", b.len())
	}
	if v, _ := b.get("a"); v != "updated" {
		t.Errorf("get(\"a\") after update = %q, want \"updated\"", v)
	}
	// Adding "d" must still evict "a" (because position was not refreshed).
	b.set("d", "4")
	if b.has("a") {
		t.Errorf("after update-a + add-d, has(\"a\") = true, want false (position should not have been refreshed)")
	}
	if !b.has("b") || !b.has("c") || !b.has("d") {
		t.Errorf("has(b,c,d) = (%v,%v,%v), all want true", b.has("b"), b.has("c"), b.has("d"))
	}
}

func TestBoundedMap_EvictsOldestWhenAtCapacity(t *testing.T) {
	b := newBoundedMap[int](3)
	b.set("a", 1)
	b.set("b", 2)
	b.set("c", 3)
	if b.len() != 3 {
		t.Fatalf("len after 3 sets = %d, want 3", b.len())
	}
	b.set("d", 4)
	if b.len() != 3 {
		t.Errorf("len after eviction = %d, want 3", b.len())
	}
	if b.has("a") {
		t.Errorf("has(\"a\") after eviction = true, want false")
	}
	if !b.has("b") || !b.has("c") || !b.has("d") {
		t.Errorf("has(b,c,d) = (%v,%v,%v), all want true", b.has("b"), b.has("c"), b.has("d"))
	}
}

// TestBoundedMap_NeverExceedsCapacity is the headline AC #1 check: after
// many inserts the map size must never exceed the bound.
func TestBoundedMap_NeverExceedsCapacity(t *testing.T) {
	const cap = 64
	b := newBoundedMap[int](cap)
	for i := 0; i < cap*100; i++ {
		b.set("k-"+strconv.Itoa(i), i)
		if got := b.len(); got > cap {
			t.Fatalf("len = %d after %d inserts, exceeds cap %d", got, i+1, cap)
		}
	}
	if got := b.len(); got != cap {
		t.Errorf("final len = %d, want %d", got, cap)
	}
}

func TestBoundedMap_NonPositiveCapacityIsUnbounded(t *testing.T) {
	// cap <= 0 disables eviction. Test-only convenience.
	b := newBoundedMap[int](0)
	for i := 0; i < 1000; i++ {
		b.set("k-"+strconv.Itoa(i), i)
	}
	if b.len() != 1000 {
		t.Errorf("len with cap=0 after 1000 inserts = %d, want 1000", b.len())
	}
}

func TestBoundedMap_DeleteAllowsReinsertionWithoutEviction(t *testing.T) {
	// del must remove the key from both the map AND the list; otherwise a
	// subsequent re-set of the same key would either double-count or fail
	// to evict on overflow.
	b := newBoundedMap[int](3)
	b.set("a", 1)
	b.set("b", 2)
	b.set("c", 3)
	b.del("b")
	if b.len() != 2 {
		t.Fatalf("len after del = %d, want 2", b.len())
	}
	b.set("d", 4)
	if b.len() != 3 {
		t.Errorf("len after re-fill = %d, want 3", b.len())
	}
	// "a" must still be present — del("b") opened a slot so "d" should
	// not have triggered eviction of "a".
	if !b.has("a") {
		t.Errorf("has(\"a\") after del(b) + set(d) = false, want true (no eviction expected)")
	}
}
