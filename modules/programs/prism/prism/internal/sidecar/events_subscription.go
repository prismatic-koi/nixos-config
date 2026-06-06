// Package sidecar — events_subscription.go
//
// This file implements the GET /events streaming endpoint on the host-API
// socket (issue #2155 — part of the prism-native multiplexer programme
// tracked by #2147). The mux process subscribes to this surface so the
// sidebar widget can repaint within one frame of a state transition,
// replacing the dashboard's pre-existing DB-polling loop.
//
// # Protocol
//
// `GET /events` opens a long-lived Server-Sent Events stream. Each frame is
// one SSE event whose `data:` line is a single JSON object:
//
//	{
//	  "type": "state_change",
//	  "session_name": "nixos-config@main",
//	  "payload": {"state":"active"},
//	  "created_at_ms": 1717635600000
//	}
//
// The SSE `event:` field is the same string as the JSON `type` so a
// consumer that filters by event type without parsing the JSON envelope
// can still do the right thing.
//
// ## Query parameters
//
//   - `session` (repeatable): restrict the stream to events whose
//     `session_name` matches one of the supplied values. Omitting the
//     parameter, or supplying the literal string `*`, streams every event
//     the sidecar emits.
//
// ## Snapshot on subscribe
//
// Before the live stream begins, the server emits one synthetic
// `state_snapshot` event per matching session, sourced from
// agent_status.CurrentStatus. The payload is the same shape as
// state_change, with an additional `"snapshot":true` flag. The snapshot
// lets a reconnecting client resync the current state before consuming
// further deltas (AC: "Subscription survives sidecar restarts ... resyncs
// current state before resuming the event stream").
//
// To avoid losing events that fire between the snapshot read and the
// start of live streaming, the server registers the broker subscriber
// *before* reading the snapshot. Live events that overlap the snapshot
// are delivered twice but state transitions are idempotent — the model
// converges on the same end state either way.
//
// ## Backpressure
//
// Each subscriber owns a bounded channel (eventSubscriberBufferSize). When
// the channel is full — the consumer has fallen behind — the broker drops
// the oldest pending event in favour of the newest. This matches the
// snapshot-then-deltas contract: a slow subscriber that misses an event
// will resync the next time it reconnects.
//
// ## Threading
//
// The broker uses a single sync.Mutex to guard its subscriber set. Publish
// is non-blocking — each subscriber's channel send is in default-drop mode
// — so the sidecar's HandleEvent path cannot stall on a slow consumer.
// Subscribe/Unsubscribe are O(N) under the lock.

package sidecar

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// eventSubscriberBufferSize is the per-subscriber outbound channel capacity.
// State transitions are bursty around session start/finish but the steady-
// state rate is one event per second or so; 256 is a comfortable margin
// without committing significant memory per consumer.
const eventSubscriberBufferSize = 256

// eventSubscriberPingInterval is the cadence at which the events handler
// writes an SSE comment frame (": ping") when no real events have arrived.
// Comment frames are silently ignored by SSE parsers per the spec but keep
// HTTP intermediaries (and the kernel's idle-connection reaper) from closing
// the stream while the agent is quiet.
const eventSubscriberPingInterval = 15 * time.Second

// envelope is the JSON shape emitted on the /events stream. Both live
// events (state_change, message.updated, etc.) and synthetic snapshot
// events share this envelope.
type envelope struct {
	Type        string          `json:"type"`
	SessionName string          `json:"session_name"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAtMs int64           `json:"created_at_ms"`
	// Snapshot is true for the synthetic state_snapshot events emitted
	// on subscribe. Live deltas omit the field.
	Snapshot bool `json:"snapshot,omitempty"`
}

// eventSubscriber is one active connection to GET /events. The broker fans
// publish() calls out to every subscriber whose filter matches.
type eventSubscriber struct {
	// sessions, when non-empty, restricts delivery to events whose
	// session_name is a key in this map. An empty map means "all
	// sessions" — equivalent to ?session=* or omitting the parameter.
	sessions map[string]struct{}
	ch       chan envelope
}

// matches reports whether sub wants to receive an event for the named
// session. Empty filter == accept all.
func (sub *eventSubscriber) matches(sessionName string) bool {
	if len(sub.sessions) == 0 {
		return true
	}
	_, ok := sub.sessions[sessionName]
	return ok
}

// eventBroker is the in-process fan-out. Owned by *Sidecar; constructed in
// New().
type eventBroker struct {
	mu          sync.Mutex
	subscribers map[*eventSubscriber]struct{}
}

func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: make(map[*eventSubscriber]struct{})}
}

// subscribe registers a new subscriber. sessions may be empty (meaning all
// sessions); otherwise the slice is converted into the filter set.
// Returns the subscriber so the caller can drain its channel.
func (b *eventBroker) subscribe(sessions []string) *eventSubscriber {
	sub := &eventSubscriber{
		sessions: make(map[string]struct{}, len(sessions)),
		ch:       make(chan envelope, eventSubscriberBufferSize),
	}
	for _, s := range sessions {
		if s == "" || s == "*" {
			// Wildcard wipes any specific filter — the empty map below
			// matches every session.
			sub.sessions = map[string]struct{}{}
			break
		}
		sub.sessions[s] = struct{}{}
	}
	b.mu.Lock()
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

// unsubscribe removes sub from the broker. Safe to call multiple times.
func (b *eventBroker) unsubscribe(sub *eventSubscriber) {
	b.mu.Lock()
	if _, ok := b.subscribers[sub]; ok {
		delete(b.subscribers, sub)
		close(sub.ch)
	}
	b.mu.Unlock()
}

// publish fans env out to every matching subscriber. Non-blocking: when a
// subscriber's channel is full, the oldest pending envelope is dropped and
// env is enqueued in its place. This trades best-effort delivery for the
// guarantee that the sidecar's HandleEvent loop never stalls on a slow
// consumer; clients recover by reconnecting and consuming the snapshot.
func (b *eventBroker) publish(env envelope) {
	b.mu.Lock()
	subs := make([]*eventSubscriber, 0, len(b.subscribers))
	for sub := range b.subscribers {
		if sub.matches(env.SessionName) {
			subs = append(subs, sub)
		}
	}
	b.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- env:
		default:
			// Channel full — drop the oldest, enqueue the newest.
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- env:
			default:
				// Another concurrent publish raced ahead; give up
				// on this delivery rather than block. The client
				// will resync on reconnect.
			}
		}
	}
}

// subscriberCount returns the current number of active subscribers.
// Exposed for tests; not part of the host-API surface.
func (b *eventBroker) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// publishEvent is the hook called from writeEvent in state.go on every
// agent_events row write. Safe to call before the broker is wired up (the
// nil check guards New() ordering and standalone tests that omit the
// broker).
func (s *Sidecar) publishEvent(eventType, sessionName string, payload []byte, createdAt time.Time) {
	if s.events == nil {
		return
	}
	s.events.publish(envelope{
		Type:        eventType,
		SessionName: sessionName,
		Payload:     append(json.RawMessage(nil), payload...),
		CreatedAtMs: createdAt.UnixMilli(),
	})
}

// registerEventsRoute installs the GET /events handler on mux. Called from
// hostAPIHandler so the route is part of the same listener used by every
// other host-API endpoint.
func (s *Sidecar) registerEventsRoute(mux *http.ServeMux) {
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
			return
		}

		filter := r.URL.Query()["session"]
		// SSE response headers — set BEFORE the first write so the client
		// observes a streaming response immediately.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// Register the broker subscriber BEFORE reading the snapshot so
		// any event written between the snapshot read and the start of
		// streaming is captured on the channel rather than dropped on
		// the floor. Duplicate state_change deltas are idempotent.
		sub := s.events.subscribe(filter)
		defer s.events.unsubscribe(sub)

		// Snapshot: emit one synthetic state_snapshot event per matching
		// session. For ?session=*, walk every session_name currently in
		// agent_status; for specific filters, look up each by name.
		snapshots := s.snapshotForFilter(filter)
		for _, snap := range snapshots {
			if err := writeSSEEvent(w, snap); err != nil {
				s.logger().Printf("sidecar: /events: write snapshot: %v", err)
				return
			}
		}
		flusher.Flush()

		ctx := r.Context()
		ping := time.NewTicker(eventSubscriberPingInterval)
		defer ping.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-sub.ch:
				if !ok {
					return
				}
				if err := writeSSEEvent(w, env); err != nil {
					s.logger().Printf("sidecar: /events: write delta: %v", err)
					return
				}
				flusher.Flush()
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					s.logger().Printf("sidecar: /events: ping: %v", err)
					return
				}
				flusher.Flush()
			}
		}
	})
}

// writeSSEEvent emits one envelope as a properly framed SSE event. The
// SSE event: field carries the agent-event type so a consumer that filters
// by event type without parsing JSON can still do the right thing; the
// data: field carries the full envelope JSON.
func writeSSEEvent(w http.ResponseWriter, env envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	// SSE forbids raw newlines inside a data: value; envelope JSON is
	// single-line by construction (encoding/json's Marshal never emits a
	// trailing newline), so a direct write is correct.
	eventName := strings.ReplaceAll(env.Type, "\n", "")
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, data); err != nil {
		return err
	}
	return nil
}

// snapshotForFilter reads agent_status rows from the DB matching the
// requested filter and returns one envelope per row. For an empty/wildcard
// filter, every session currently tracked in agent_status is included.
// Returns an empty slice (never nil) so the handler loop is uniform.
func (s *Sidecar) snapshotForFilter(filter []string) []envelope {
	out := []envelope{}

	wantAll := len(filter) == 0
	for _, f := range filter {
		if f == "" || f == "*" {
			wantAll = true
			break
		}
	}

	if wantAll {
		statuses, err := s.cfg.DB.AllActiveStatus()
		if err != nil {
			s.logger().Printf("sidecar: /events: snapshot AllActiveStatus: %v", err)
			return out
		}
		for _, st := range statuses {
			out = append(out, statusToEnvelope(st.SessionName, agent.AgentState(st.State), s.cfg.Clock.Now()))
		}
		return out
	}

	for _, name := range filter {
		st, err := s.cfg.DB.CurrentStatus(name)
		if err != nil {
			s.logger().Printf("sidecar: /events: snapshot CurrentStatus %q: %v", name, err)
			continue
		}
		if st == nil {
			continue
		}
		out = append(out, statusToEnvelope(name, agent.AgentState(st.State), s.cfg.Clock.Now()))
	}
	return out
}

// statusToEnvelope formats a CurrentStatus row as a synthetic state_snapshot
// envelope. The payload shape mirrors a state_change payload so consumers
// can apply both without branching.
func statusToEnvelope(sessionName string, state agent.AgentState, now time.Time) envelope {
	payload, _ := json.Marshal(map[string]string{"state": string(state)})
	return envelope{
		Type:        "state_snapshot",
		SessionName: sessionName,
		Payload:     payload,
		CreatedAtMs: now.UnixMilli(),
		Snapshot:    true,
	}
}
