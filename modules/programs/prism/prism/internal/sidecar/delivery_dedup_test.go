package sidecar

// delivery_dedup_test.go — unit tests for the bounded LRU dedup set used
// by the /prompt handler to drop repeat deliveries.

import (
	"sync"
	"testing"
)

func TestDeliveryDedup_FirstSeenReturnsFalse(t *testing.T) {
	d := newDeliveryDedup(8)
	if repeat := d.markSeen("a"); repeat {
		t.Errorf("first markSeen(\"a\") = true, want false")
	}
	if !d.has("a") {
		t.Errorf("after markSeen(\"a\"), has(\"a\") = false, want true")
	}
}

func TestDeliveryDedup_RepeatReturnsTrue(t *testing.T) {
	d := newDeliveryDedup(8)
	d.markSeen("a")
	if repeat := d.markSeen("a"); !repeat {
		t.Errorf("second markSeen(\"a\") = false, want true")
	}
}

func TestDeliveryDedup_EmptyIDIsNotTracked(t *testing.T) {
	d := newDeliveryDedup(8)
	if repeat := d.markSeen(""); repeat {
		t.Errorf("markSeen(\"\") = true on first call, want false (empty id not tracked)")
	}
	if d.has("") {
		t.Errorf("has(\"\") = true, want false (empty id not tracked)")
	}
	if d.len() != 0 {
		t.Errorf("len = %d after markSeen(\"\"), want 0", d.len())
	}
}

func TestDeliveryDedup_EvictsOldestWhenAtCapacity(t *testing.T) {
	d := newDeliveryDedup(3)
	d.markSeen("a")
	d.markSeen("b")
	d.markSeen("c")
	if d.len() != 3 {
		t.Fatalf("len after 3 adds = %d, want 3", d.len())
	}
	// Adding a fourth must evict the oldest ("a").
	d.markSeen("d")
	if d.len() != 3 {
		t.Errorf("len after eviction = %d, want 3", d.len())
	}
	if d.has("a") {
		t.Errorf("has(\"a\") = true after eviction, want false")
	}
	if !d.has("b") || !d.has("c") || !d.has("d") {
		t.Errorf("has(b,c,d) = (%v,%v,%v), all want true", d.has("b"), d.has("c"), d.has("d"))
	}
}

func TestDeliveryDedup_RepeatDoesNotRefreshPosition(t *testing.T) {
	// markSeen on an existing id must NOT re-order it to the back. The set
	// is strict insertion-order LRU; refresh-on-hit would let a repeated
	// delivery prolong its own retention, which is not what we want.
	d := newDeliveryDedup(3)
	d.markSeen("a")
	d.markSeen("b")
	d.markSeen("c")
	// Repeat "a" — must return true and NOT refresh position.
	if !d.markSeen("a") {
		t.Errorf("markSeen(\"a\") on repeat = false, want true")
	}
	// Adding "d" should still evict "a" (because position was not refreshed).
	d.markSeen("d")
	if d.has("a") {
		t.Errorf("after repeat-a + add-d, has(\"a\") = true, want false (position should not have been refreshed)")
	}
}

func TestDeliveryDedup_ConcurrentMarkSeenIsRaceFree(t *testing.T) {
	// Smoke test: many goroutines hammering markSeen on overlapping ids
	// must not race. Run under `go test -race` to verify.
	d := newDeliveryDedup(64)
	var wg sync.WaitGroup
	const goroutines = 16
	const idsPerGoroutine = 100
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < idsPerGoroutine; i++ {
				// Some overlap across goroutines so we exercise both new
				// and repeat paths.
				_ = d.markSeen("shared")
				_ = d.markSeen("unique")
			}
		}(g)
	}
	wg.Wait()
	// After all goroutines, "shared" and "unique" must be present exactly
	// once each (subject to eviction; capacity 64 is well above 2).
	if !d.has("shared") || !d.has("unique") {
		t.Errorf("has(shared)=%v has(unique)=%v after concurrent markSeen", d.has("shared"), d.has("unique"))
	}
}

func TestDeliveryDedup_ZeroCapacityIsUnbounded(t *testing.T) {
	// cap <= 0 disables eviction. Useful for tests that want to verify
	// every ID is retained; should not be used in production.
	d := newDeliveryDedup(0)
	for i := 0; i < 1000; i++ {
		d.markSeen(uniqueID(i))
	}
	if d.len() != 1000 {
		t.Errorf("len with cap=0 after 1000 adds = %d, want 1000", d.len())
	}
}

// uniqueID generates a short deterministic ID for tests.
func uniqueID(i int) string {
	// Cheap reversible encoding: avoids strconv import here for the smoke
	// test. Anything stable and unique-per-i works.
	const alpha = "0123456789abcdef"
	if i == 0 {
		return "id-0"
	}
	out := []byte("id-")
	digits := []byte{}
	for i > 0 {
		digits = append(digits, alpha[i%16])
		i /= 16
	}
	// reverse
	for j := len(digits) - 1; j >= 0; j-- {
		out = append(out, digits[j])
	}
	return string(out)
}
