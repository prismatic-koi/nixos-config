// integration_test.go drives Subscriber.Run against a fake sidecar that
// emits a scripted SSE stream. The fake is a httptest.Server so the test
// is fully in-process and never touches a real Unix socket — a hard
// requirement under the nix-build sandbox where $HOME=/homeless-shelter
// (see modules/programs/prism/AGENTS.md § Prism for the sandbox-isolation
// convention).
//
// Coverage:
//
//   - "fake sidecar emits events; model state updates per event"
//     (AC: integration test).
//   - Snapshot-then-deltas ordering matches the wire contract.
//   - Reconnect after the fake closes the stream: a second event delivered
//     after reconnect lands in the Store.

package state

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// fakeSidecar serves GET /events with a scripted sequence of frames. The
// test plays each frame out via the Frames channel; the fake closes the
// connection cleanly after the last frame so the Subscriber's reconnect
// path is exercised.
type fakeSidecar struct {
	mu          sync.Mutex
	connections atomic.Int32
	srv         *httptest.Server
	frames      []string
	// onConnect optionally fires once per accepted connection. Used to
	// drive deterministic reconnect tests.
	onConnect func(connNum int)
}

func newFakeSidecar(t *testing.T, frames []string) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{frames: frames}
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		n := int(f.connections.Add(1))
		if f.onConnect != nil {
			f.onConnect(n)
		}
		// Honour the client's context: when ctx fires (test teardown),
		// stop writing rather than block on a dead socket.
		f.mu.Lock()
		framesCopy := append([]string{}, f.frames...)
		f.mu.Unlock()
		for _, frame := range framesCopy {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := fmt.Fprint(w, frame); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Close the connection cleanly after the scripted frames have
		// been delivered. The Subscriber's underlying sse.Client will
		// see EOF and (depending on the test) either exit or reconnect.
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// setFrames replaces the scripted frame list. Subsequent reconnects will
// observe the new frames.
func (f *fakeSidecar) setFrames(frames []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = frames
}

// connectionCount returns how many /events connections the fake has
// accepted so far.
func (f *fakeSidecar) connectionCount() int { return int(f.connections.Load()) }

// frameStateChange builds one SSE frame carrying a state_change event for
// the named session.
func frameStateChange(sessionName, state string) string {
	return fmt.Sprintf(
		"event: state_change\ndata: {\"type\":\"state_change\",\"session_name\":%q,\"payload\":{\"state\":%q},\"created_at_ms\":1}\n\n",
		sessionName, state,
	)
}

// frameSnapshot builds one SSE frame carrying a state_snapshot event for
// the named session.
func frameSnapshot(sessionName, state string) string {
	return fmt.Sprintf(
		"event: state_snapshot\ndata: {\"type\":\"state_snapshot\",\"session_name\":%q,\"payload\":{\"state\":%q},\"created_at_ms\":1,\"snapshot\":true}\n\n",
		sessionName, state,
	)
}

// waitForState polls store until SessionState(id) returns want, or fails
// after a generous timeout. Used after Run launches because the
// Subscriber goroutine applies events asynchronously.
func waitForState(t *testing.T, store *Store, id string, want agent.AgentState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := store.SessionState(id)
		if ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForState(%q) = (%q, %v), want %q", id, got, ok, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runSubscriberInBackground starts s.Run and registers a cleanup that
// cancels the context. Returns a wait function that the test can call to
// confirm Run returned cleanly.
func runSubscriberInBackground(t *testing.T, s *Subscriber) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Subscriber.Run returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("Subscriber.Run did not return after cancel")
		}
	})
	return func() {
		cancel()
		<-done
	}
}

func TestSubscriber_AppliesScriptedSequence(t *testing.T) {
	// AC: spawn a fake sidecar emitting a scripted sequence of events;
	// assert that the model state updates per event in the correct
	// order. This is the canonical integration assertion.
	frames := []string{
		frameSnapshot("alpha", "idle"),
		frameStateChange("alpha", "active"),
		frameStateChange("alpha", "waiting"),
		frameStateChange("alpha", "active"),
		frameStateChange("alpha", "reviewing"),
		frameStateChange("alpha", "finished"),
	}
	fake := newFakeSidecar(t, frames)

	store := New(nil)

	// Listener captures the observed state sequence — we assert on this
	// sequence rather than just the final state so a reordered apply
	// path is caught.
	var (
		seenMu sync.Mutex
		seen   []agent.AgentState
	)
	store.AddListener(func() {
		seenMu.Lock()
		defer seenMu.Unlock()
		if v, ok := store.SessionState("alpha"); ok {
			seen = append(seen, v)
		}
	})

	sub := &Subscriber{
		Store: store,
		// Skip the host_api Unix socket; talk to the fake httptest
		// server directly.
		httpClient:        fake.srv.Client(),
		baseURL:           fake.srv.URL,
		InitialRetryDelay: 5 * time.Millisecond,
		MaxRetryDelay:     20 * time.Millisecond,
	}
	runSubscriberInBackground(t, sub)

	waitForState(t, store, "alpha", agent.StateFinished)

	want := []agent.AgentState{
		agent.StateIdle,
		agent.StateActive,
		agent.StateWaiting,
		agent.StateActive,
		agent.StateReviewing,
		agent.StateFinished,
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) < len(want) {
		t.Fatalf("captured %d states, want at least %d: %v", len(seen), len(want), seen)
	}
	// The observed sequence must contain `want` as a contiguous prefix
	// after reconnect dedup; allow trailing duplicates (a no-op apply
	// still fires the listener? — no, ApplyEvent returns false and does
	// not call SetSessionState. So the prefix is exact).
	for i, w := range want {
		if seen[i] != w {
			t.Fatalf("seen[%d] = %q, want %q (full: %v)", i, seen[i], w, seen)
		}
	}
}

func TestSubscriber_ReconnectsAndResyncs(t *testing.T) {
	// The fake delivers a first batch on connection 1, closes the
	// stream, then delivers a different batch on connection 2. The
	// Subscriber's reconnect path must pick up the second batch without
	// the test having to recreate it.
	first := []string{
		frameSnapshot("alpha", "idle"),
		frameStateChange("alpha", "active"),
	}
	second := []string{
		// Reconnect resync — the snapshot reports the authoritative
		// state at reconnect time.
		frameSnapshot("alpha", "active"),
		frameStateChange("alpha", "reviewing"),
		frameStateChange("alpha", "finished"),
	}
	fake := newFakeSidecar(t, first)

	// On the second connect, swap the frames to the second batch so
	// the reconnect path observes them.
	fake.onConnect = func(n int) {
		if n == 2 {
			fake.setFrames(second)
		}
	}

	store := New(nil)
	sub := &Subscriber{
		Store:             store,
		httpClient:        fake.srv.Client(),
		baseURL:           fake.srv.URL,
		InitialRetryDelay: 5 * time.Millisecond,
		MaxRetryDelay:     20 * time.Millisecond,
	}
	runSubscriberInBackground(t, sub)

	// After the full sequence (across both connections), the store must
	// settle on finished. waitForState polls and is the canonical
	// "drainage" check.
	waitForState(t, store, "alpha", agent.StateFinished)

	if got := fake.connectionCount(); got < 2 {
		t.Fatalf("connectionCount = %d, want >= 2 (reconnect path not exercised)", got)
	}
}

func TestSubscriber_FilterFiltersAtTheWire(t *testing.T) {
	// When Sessions is non-empty the Subscriber must encode session= query
	// parameters into the URL. The fake captures the URL of the first
	// request to assert on.
	var (
		capturedMu sync.Mutex
		captured   string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		capturedMu.Lock()
		captured = r.URL.RawQuery
		capturedMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, frameSnapshot("alpha", "active"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	store := New(nil)
	sub := &Subscriber{
		Store:             store,
		Sessions:          []string{"alpha", "beta"},
		httpClient:        srv.Client(),
		baseURL:           srv.URL,
		InitialRetryDelay: 5 * time.Millisecond,
		MaxRetryDelay:     20 * time.Millisecond,
	}
	runSubscriberInBackground(t, sub)
	waitForState(t, store, "alpha", agent.StateActive)

	capturedMu.Lock()
	defer capturedMu.Unlock()
	if captured == "" {
		t.Fatalf("captured query = empty; want session= params")
	}
	// url.Values.Encode sorts keys but preserves slice order — there's
	// only one key here so either order is acceptable. Just make sure
	// both names appear.
	if !contains(captured, "session=alpha") || !contains(captured, "session=beta") {
		t.Fatalf("captured query %q does not include both session filters", captured)
	}
}

func TestSubscriber_RequiresStore(t *testing.T) {
	sub := &Subscriber{SockPath: "/tmp/anything.sock"}
	err := sub.Run(context.Background())
	if err == nil {
		t.Fatalf("Run with nil Store returned nil; want error")
	}
}

func TestSubscriber_RequiresSockPathOrClient(t *testing.T) {
	sub := &Subscriber{Store: New(nil)}
	err := sub.Run(context.Background())
	if err == nil {
		t.Fatalf("Run with no SockPath or httpClient returned nil; want error")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
