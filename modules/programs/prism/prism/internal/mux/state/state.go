// Package state is the multiplexer-side consumer of the sidecar's GET
// /events stream (issue #2155 — part of the prism-native multiplexer
// programme tracked by #2147).
//
// The package has two responsibilities:
//
//  1. Maintain a Store that maps each session ID to its current
//     agent.AgentState. The render layer (#2152) reads from the Store to
//     drive the sidebar's per-session glyph and colour, per the §3.1
//     state → glyph + colour table.
//
//  2. Run one Subscriber per sidecar, each of which holds a long-lived SSE
//     connection to the sidecar's host-API socket and applies incoming
//     events to the Store. Subscriptions survive sidecar restarts via
//     exponential-backoff reconnect; on every reconnect the sidecar
//     emits a fresh snapshot, so the Store resyncs to authoritative state
//     before consuming further deltas.
//
// # Threading
//
// The Store is goroutine-safe — every read and write acquires its mutex.
// Subscribers run in their own goroutine and write to the Store directly;
// the render layer reads via SessionState / Snapshot.
//
// The Store does NOT own a *pane.SessionTree, but it can be configured
// with one so that ActivateSession events emitted by the sidecar can be
// translated into pane.SessionTree.ActivateSession calls. Translating
// state_change events into pane operations is out of scope for this
// package — only state colours flow into the Store.
//
// # Reconnect strategy
//
// On each connection attempt:
//
//   - On success, reset the backoff to InitialRetryDelay and consume the
//     stream until it errors or the context is cancelled.
//   - On error, sleep for the current backoff then double it (capped at
//     MaxRetryDelay) before retrying.
//
// The strategy mirrors internal/sse.Client.reconnect — we use that
// package directly via its public Connect API so the wire-level details
// stay in one place.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/mux/pane"
	"github.com/prismatic-koi/prism/internal/sse"
)

// Event is the JSON envelope emitted by the sidecar GET /events endpoint.
// The shape is duplicated here (rather than imported from internal/sidecar)
// because the mux process must not pull the sidecar package's transitive
// dependencies (DB, harness, container) into its own binary.
//
// Both live deltas and snapshot events use this envelope. Snapshot events
// have Snapshot=true and Type="state_snapshot"; live events use the
// agent_events row type ("state_change", "message_updated", etc.).
type Event struct {
	Type        string          `json:"type"`
	SessionName string          `json:"session_name"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAtMs int64           `json:"created_at_ms"`
	Snapshot    bool            `json:"snapshot,omitempty"`
}

// statePayload is the payload shape for state_change and state_snapshot
// events. The sidecar emits {"state":"<value>"} for both; using a typed
// payload keeps the per-event branch readable without a map[string]any.
type statePayload struct {
	State string `json:"state"`
}

// Store holds per-session agent.AgentState keyed by session ID. The render
// layer reads via SessionState; the Subscriber writes via SetSessionState
// and ApplyEvent.
//
// The zero value is NOT ready for use — call New(). The constructor exists
// so the listener slice and the inner map are non-nil from the start.
type Store struct {
	mu        sync.RWMutex
	states    map[string]agent.AgentState
	tree      *pane.SessionTree
	listeners []func()
}

// New returns an empty Store. tree may be nil; when non-nil, the Store
// holds a reference but does not mutate it from inside SetSessionState
// or ApplyEvent (the consumer does that explicitly via the public
// SessionTree API). Holding the reference lets the Subscriber discover
// which sessions to filter for in a single read.
func New(tree *pane.SessionTree) *Store {
	return &Store{
		states: make(map[string]agent.AgentState),
		tree:   tree,
	}
}

// SessionTree returns the *pane.SessionTree this Store was constructed
// with, or nil if none was supplied. Useful for the Subscriber so it can
// compute its session filter from the tree without holding a separate
// reference.
func (s *Store) SessionTree() *pane.SessionTree { return s.tree }

// SessionState returns the most recently observed agent.AgentState for
// the named session ID, or ("", false) if the session has never had a
// state event applied.
func (s *Store) SessionState(id string) (agent.AgentState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.states[id]
	return st, ok
}

// Snapshot returns a copy of every session-state mapping currently in the
// Store. The returned map is owned by the caller and may be mutated
// without affecting the Store. Order is unspecified.
func (s *Store) Snapshot() map[string]agent.AgentState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]agent.AgentState, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out
}

// SetSessionState records st as the current state for the named session.
// Fires every registered listener after the write (outside the lock) so a
// render goroutine subscribed via AddListener can repaint.
//
// Empty st is treated as a clear: the session entry is deleted.
func (s *Store) SetSessionState(id string, st agent.AgentState) {
	s.mu.Lock()
	if st == "" {
		delete(s.states, id)
	} else {
		s.states[id] = st
	}
	listeners := make([]func(), len(s.listeners))
	copy(listeners, s.listeners)
	s.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

// AddListener registers fn to be called every time SetSessionState (or
// ApplyEvent, indirectly) records a transition. Listeners are called
// outside the lock so they may call back into the Store without
// deadlocking.
//
// There is no Remove — listeners live for the lifetime of the Store. The
// render layer's update loop is the only intended consumer; multiple
// repaints in quick succession are harmless.
func (s *Store) AddListener(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

// ApplyEvent applies one Event to the Store. state_change and
// state_snapshot events update the Store; every other type is ignored
// (the package is intentionally narrow — broader event-driven model
// updates belong to other packages so the consumer surface stays
// auditable).
//
// Returns true if the event resulted in a state change, false otherwise.
// The boolean lets callers (tests, debug tooling) gate work on whether
// the event was a no-op.
func (s *Store) ApplyEvent(evt Event) bool {
	switch evt.Type {
	case "state_change", "state_snapshot":
		// fall through
	default:
		return false
	}
	var p statePayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		return false
	}
	if p.State == "" {
		return false
	}
	current, ok := s.SessionState(evt.SessionName)
	st := agent.AgentState(p.State)
	if ok && current == st {
		return false
	}
	s.SetSessionState(evt.SessionName, st)
	return true
}

// Subscriber maintains a long-lived SSE subscription to a single
// sidecar's GET /events endpoint and applies incoming events to a Store.
//
// Run blocks until ctx is cancelled. The Subscriber is intended to be
// driven by one goroutine per sidecar; the model's mutex (held inside the
// Store) serialises all writes.
type Subscriber struct {
	// SockPath is the Unix-socket path of the sidecar's host-API. Required.
	SockPath string

	// Sessions filters the subscription. An empty slice means "every
	// session this sidecar can see" — the wire-level wildcard.
	Sessions []string

	// Store receives applied events. Required.
	Store *Store

	// Logger is the logger used for connection / error messages. If nil,
	// log.Default() is used.
	Logger *log.Logger

	// InitialRetryDelay and MaxRetryDelay control the exponential
	// backoff between reconnect attempts. When zero, the defaults from
	// internal/sse (1s and 30s) apply.
	InitialRetryDelay time.Duration
	MaxRetryDelay     time.Duration

	// httpClient is overridable for tests that need to drive the
	// subscriber against an httptest.Server. When nil, Run constructs a
	// Unix-socket-dialling http.Client from SockPath.
	httpClient *http.Client

	// baseURL is overridable for tests. When non-empty, it replaces the
	// default "http://prism-sidecar" base. Required when httpClient is
	// set (so the test points the request at the httptest.Server).
	baseURL string
}

// SetHTTPClient overrides the default Unix-socket-dialling http.Client.
// Intended for tests that drive the Subscriber against an httptest.Server;
// pair with SetBaseURL so the request resolves to the test server.
func (s *Subscriber) SetHTTPClient(c *http.Client) { s.httpClient = c }

// SetBaseURL overrides the default "http://prism-sidecar" base used to
// construct the events URL. Intended for tests; production callers leave
// this unset and Run derives the URL from SockPath.
func (s *Subscriber) SetBaseURL(u string) { s.baseURL = u }

func (s *Subscriber) logger() *log.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return log.Default()
}

// Run blocks until ctx is cancelled, maintaining a long-lived subscription
// to the configured sidecar. Reconnection is handled internally by the
// underlying sse.Client; this method orchestrates one outer pass per
// connection so a fatal initial-connect error can still surface to the
// caller via ctx-aware shutdown.
//
// Returns nil when ctx is cancelled normally; returns a wrapped error
// only when configuration is invalid (missing SockPath, nil Store).
func (s *Subscriber) Run(ctx context.Context) error {
	if s.Store == nil {
		return errors.New("state: Subscriber.Run: Store is required")
	}
	if s.SockPath == "" && s.httpClient == nil {
		return errors.New("state: Subscriber.Run: SockPath or httpClient is required")
	}

	client := s.httpClient
	if client == nil {
		client = newUnixSocketClient(s.SockPath)
	}
	base := s.baseURL
	if base == "" {
		base = "http://prism-sidecar"
	}

	streamURL := base + "/events"
	if q := buildQuery(s.Sessions); q != "" {
		streamURL += "?" + q
	}

	sseClient := &sse.Client{
		HTTPClient:        client,
		InitialRetryDelay: s.InitialRetryDelay,
		MaxRetryDelay:     s.MaxRetryDelay,
	}

	events, err := sseClient.Connect(ctx, streamURL)
	if err != nil {
		// Connect only returns an error when ctx fires before the first
		// connect succeeds; treat as a clean shutdown.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("state: Subscriber.Run: sse connect: %w", err)
	}

	for evt := range events {
		// The sidecar serialises every envelope as a single data: line.
		var env Event
		if err := json.Unmarshal([]byte(evt.Data), &env); err != nil {
			s.logger().Printf("state: Subscriber: unmarshal event: %v: %s", err, evt.Data)
			continue
		}
		if env.Type == "" && evt.Type != "" {
			// The sidecar always sets `event: <type>`; treat the SSE
			// event field as a fallback for clients that buffer
			// across reconnects.
			env.Type = evt.Type
		}
		s.Store.ApplyEvent(env)
	}
	return nil
}

// buildQuery returns the URL-encoded ?session=... portion of the
// subscription URL. Empty sessions means "no filter" (the wildcard) and
// returns an empty string so the URL is just /events.
func buildQuery(sessions []string) string {
	if len(sessions) == 0 {
		return ""
	}
	vals := url.Values{}
	for _, s := range sessions {
		if s == "" {
			continue
		}
		vals.Add("session", s)
	}
	enc := vals.Encode()
	return enc
}

// newUnixSocketClient returns an http.Client whose Transport dials sockPath
// for every request. Used for production sidecar subscriptions; tests
// override via Subscriber.SetHTTPClient.
//
// The client carries no overall timeout because GET /events is a long-
// lived stream — a Client.Timeout would terminate the read after its
// deadline regardless of whether events were still flowing.
func newUnixSocketClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
}
