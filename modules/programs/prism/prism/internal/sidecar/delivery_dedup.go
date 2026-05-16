package sidecar

// delivery_dedup.go — bounded in-memory dedup set for /prompt deliveries.
//
// Background: issue #1685 — a single `prism escalate` invocation was observed
// to deliver the same prompt body to the coordinator's harness four times.
// The Go-side delivery path is single-shot end-to-end, so the duplication
// arises further down (PI runtime's followUp queue, an upstream retry layer,
// or some future change that adds replay-on-reconnect). Rather than relying
// on every downstream consumer behaving correctly, the sidecar's /prompt
// handler is now idempotent at the bus boundary: each delivery carries a
// `delivery_id` (a UUID minted by the sender), and the receiving sidecar
// drops repeats whose ID it has seen recently.
//
// Capacity is bounded so the dedup set cannot grow without limit. The bound
// is generous (256) — 256 distinct deliveries per session within the dedup
// window is well above any realistic workload — but small enough that the
// memory cost is trivial.

import (
	"container/list"
	"sync"
)

// deliveryDedup is a bounded LRU set of delivery IDs. Adding an existing ID
// is a no-op (does not refresh its position); the goal is to drop repeats,
// not to maintain access ordering. Eviction is strictly first-inserted-first.
//
// All methods are safe for concurrent use.
type deliveryDedup struct {
	mu   sync.Mutex
	cap  int
	set  map[string]*list.Element
	keys *list.List // front=oldest, back=newest
}

// newDeliveryDedup constructs an empty dedup set with the given capacity.
// A non-positive capacity disables eviction (set grows without bound) and
// is intended for tests only — production callers should always pass a
// positive bound.
func newDeliveryDedup(capacity int) *deliveryDedup {
	return &deliveryDedup{
		cap:  capacity,
		set:  make(map[string]*list.Element),
		keys: list.New(),
	}
}

// markSeen records id as seen and returns true if it had already been seen
// (i.e. this is a repeat delivery). An empty id is treated as "do not
// dedup" — markSeen returns false and the set is not modified.
//
// When the set is at capacity and id is new, the oldest entry is evicted
// before id is added. Eviction is strict insertion-order; markSeen does not
// refresh the position of an already-seen id.
func (d *deliveryDedup) markSeen(id string) (repeat bool) {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.set[id]; ok {
		return true
	}
	if d.cap > 0 && d.keys.Len() >= d.cap {
		oldest := d.keys.Front()
		if oldest != nil {
			oldestID, _ := oldest.Value.(string)
			d.keys.Remove(oldest)
			delete(d.set, oldestID)
		}
	}
	d.set[id] = d.keys.PushBack(id)
	return false
}

// has reports whether id is currently in the set without recording it.
// Test-facing.
func (d *deliveryDedup) has(id string) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.set[id]
	return ok
}

// len returns the number of IDs currently tracked. Test-facing.
func (d *deliveryDedup) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.keys.Len()
}

// deliveryDedupCapacity is the default bound for the per-sidecar /prompt
// dedup set. Sized generously (256) — well above any realistic burst of
// distinct deliveries — while keeping the memory cost trivial (each entry
// is a UUID string + list-element pointer overhead).
const deliveryDedupCapacity = 256

// pendingReplayCapacity bounds the per-sidecar replay buffer used to hold
// /prompt deliveries that arrive while the PI extension is disconnected.
// Sized small (16) on the theory that any real reconnect-driven backlog is
// a handful of escalations / follow-ups, not hundreds: if a coordinator is
// disconnected long enough to accumulate more than 16 deliveries, the human
// operator is already involved and dropping the oldest is the right move.
const pendingReplayCapacity = 16

// pendingReplayDelivery records one /prompt that arrived while the PI
// extension was disconnected. On the next successful handshake, the
// reconnect loop drains the slice in arrival order and enqueues each entry
// with `replay: true` set on the prompt frame so the receiver can identify
// it as a replayed (not fresh) delivery. Issue #1685 AC #7.
type pendingReplayDelivery struct {
	DeliveryID string
	Text       string
	DeliverAs  string
}
