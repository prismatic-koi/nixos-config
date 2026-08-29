package sidecar

// bounded_map.go — bounded insertion-order LRU map that caps the
// per-message tracking structures on Sidecar (writtenMessages,
// textByMessage, msgCreatedAtMs, ttftByMessage).
//
// Coordinator sidecars run for days or weeks. Without a bound, the four
// message-tracking maps grow for every message ID observed, for the
// lifetime of the process. Abandoned messages (tool-only turns, agent
// interruptions) are never cleaned up, so on a busy coordinator the maps
// accrete hundreds of entries per day — linear growth on a long-lived
// process.
//
// The pattern mirrors `deliveryDedup` (delivery_dedup.go): a `map` for O(1)
// lookup combined with a doubly-linked `list` recording insertion order so
// the oldest key can be evicted in O(1) when the bound is hit. Unlike
// `deliveryDedup`, callers may overwrite an existing key with a new value
// without refreshing its position — `set` on an existing key keeps the
// original insertion-order slot. This matches the call-site semantics: a
// streaming `textByMessage` update for an in-flight message must not
// prolong the entry's lifetime relative to other in-flight messages.
//
// boundedMap is NOT safe for concurrent use. All call sites in this
// package access the message-tracking maps under `Sidecar.mu`, so the
// outer lock is sufficient — a second mutex only complicates the code
// without buying anything.

import "container/list"

// boundedMap is a generic insertion-order LRU map with a fixed capacity.
// V is the value type; keys are always strings (the message ID).
//
// Zero value is not usable; construct with newBoundedMap. When the map is
// at capacity and a new key is set, the oldest key (by insertion order) is
// evicted. Setting an existing key updates the value in place without
// changing its position in the eviction order.
//
// Eviction is O(1) amortised per insert.
type boundedMap[V any] struct {
	cap  int
	m    map[string]*list.Element
	keys *list.List // front=oldest, back=newest
}

// boundedMapEntry is the value stored in each list element. We keep the
// key alongside the value so eviction (which pops the front of the list)
// can remove the corresponding map entry without a reverse lookup.
type boundedMapEntry[V any] struct {
	key string
	val V
}

// newBoundedMap constructs an empty bounded map with the given capacity.
// A non-positive capacity disables eviction (the map grows without bound)
// and is intended for tests only — production callers should always pass
// a positive bound.
func newBoundedMap[V any](capacity int) *boundedMap[V] {
	return &boundedMap[V]{
		cap:  capacity,
		m:    make(map[string]*list.Element),
		keys: list.New(),
	}
}

// get returns the value for key and whether it was present. The zero value
// is returned when the key is absent.
func (b *boundedMap[V]) get(key string) (V, bool) {
	if el, ok := b.m[key]; ok {
		return el.Value.(*boundedMapEntry[V]).val, true
	}
	var zero V
	return zero, false
}

// has reports whether key is present.
func (b *boundedMap[V]) has(key string) bool {
	_, ok := b.m[key]
	return ok
}

// set inserts or updates key. On a fresh insert that exceeds the
// capacity, the oldest key is evicted first. On an update to an existing
// key, the value is replaced in place — position in the eviction order is
// not refreshed.
func (b *boundedMap[V]) set(key string, val V) {
	if el, ok := b.m[key]; ok {
		el.Value.(*boundedMapEntry[V]).val = val
		return
	}
	if b.cap > 0 && b.keys.Len() >= b.cap {
		oldest := b.keys.Front()
		if oldest != nil {
			entry := oldest.Value.(*boundedMapEntry[V])
			b.keys.Remove(oldest)
			delete(b.m, entry.key)
		}
	}
	entry := &boundedMapEntry[V]{key: key, val: val}
	b.m[key] = b.keys.PushBack(entry)
}

// del removes key. No-op if absent.
func (b *boundedMap[V]) del(key string) {
	if el, ok := b.m[key]; ok {
		b.keys.Remove(el)
		delete(b.m, key)
	}
}

// len returns the number of entries currently tracked.
func (b *boundedMap[V]) len() int {
	return b.keys.Len()
}

// messageTrackingCap is the per-map capacity for the four message-tracking
// maps on Sidecar (writtenMessages, textByMessage, msgCreatedAtMs,
// ttftByMessage). Sized generously at 4096 — well above any realistic
// short-conversation working set, so eviction does not perturb normal
// behaviour, while keeping the worst-case footprint bounded to a few MiB
// per map even with full-text payloads.
const messageTrackingCap = 4096
