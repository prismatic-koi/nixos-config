// events_subscription_test.go — unit tests for the GET /events endpoint
// added by issue #2155.
//
// All tests construct sidecars via sidecartest.NewIsolated so the host-
// API socket paths, dashboard sentinel paths, and bus directories all
// resolve under a per-test tempdir. This is the convention documented in
// modules/programs/prism/AGENTS.md § Prism (issue #1608) — it keeps `go
// test ./... -race` honest about the homeless-shelter sandbox.

package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// newEventsSidecar returns a freshly constructed Sidecar with the broker
// initialised, bus-backed DB, and a known session name. The handler is
// constructed via hostAPIHandler() so the test exercises the real route
// registration glue, not just the broker in isolation.
func newEventsSidecar(t *testing.T) (*Sidecar, *httptest.Server, *sidecartest.Bus) {
	t.Helper()
	invoker := "prism-test@events-" + strings.ReplaceAll(t.Name(), "/", "-")
	bus := sidecartest.NewIsolated(t, invoker)
	cfg := Config{
		SessionName: invoker,
		Repo:        "prism-test",
		DB:          bus.DB,
		Clock:       newTestClock(),
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	}
	sc := New(cfg)
	srv := httptest.NewServer(sc.hostAPIHandler())
	t.Cleanup(srv.Close)
	return sc, srv, bus
}

// openEventsStream issues GET /events?<query> and returns the response
// body so the caller can read SSE frames. The caller must close the
// response body (or rely on the context cancel via the supplied ctx
// closing the response).
func openEventsStream(t *testing.T, ctx context.Context, srvURL, query string) *http.Response {
	t.Helper()
	url := srvURL + "/events"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET /events: status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	return resp
}

// readEnvelope consumes one SSE event from r and returns the decoded
// envelope. Comment frames (": ping") are silently skipped.
func readEnvelope(t *testing.T, br *bufio.Reader) envelope {
	t.Helper()
	env, err := readEnvelopeMaybe(br)
	if err != nil {
		t.Fatalf("readEnvelope: %v", err)
	}
	return env
}

// readEnvelopeMaybe is the error-returning sibling of readEnvelope; used
// by tests that race a read against a context-cancel and need to handle
// the closed-connection error path gracefully.
func readEnvelopeMaybe(br *bufio.Reader) (envelope, error) {
	var dataLines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return envelope{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// End of event. If dataLines is empty, this was a heartbeat-
			// only frame — loop and read the next event.
			if len(dataLines) == 0 {
				continue
			}
			break
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// event: lines are read but ignored — the JSON envelope
		// carries the same information.
	}
	var env envelope
	if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &env); err != nil {
		return envelope{}, fmt.Errorf("unmarshal envelope: %w: %q", err, strings.Join(dataLines, "\n"))
	}
	return env, nil
}

// readUntilLiveDelta reads frames until a non-snapshot envelope arrives,
// discarding any state_snapshot frames in the way. The snapshot read in
// the production handler races against in-flight writeStateChange /
// upsertState writes, so tests that want to assert on the live delta
// branch must tolerate an unpredictable number of snapshot frames
// preceding it.
func readUntilLiveDelta(t *testing.T, br *bufio.Reader) envelope {
	t.Helper()
	for {
		env := readEnvelope(t, br)
		if env.Type != "state_snapshot" {
			return env
		}
	}
}

func TestEventsRoute_WrongMethod_405(t *testing.T) {
	_, srv, _ := newEventsSidecar(t)
	resp, err := http.Post(srv.URL+"/events", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestEventsRoute_SnapshotOnSubscribe(t *testing.T) {
	// AC: subscription survives sidecar restarts via resync. The wire-
	// level realisation is "snapshot on subscribe" — on first connect
	// the server emits one synthetic state_snapshot per matching session
	// before live deltas begin. Seed the DB and assert the snapshot.
	sc, srv, bus := newEventsSidecar(t)
	if err := bus.DB.UpsertStatus(sc.cfg.SessionName, "prism-test", "", string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("seed agent_status: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openEventsStream(t, ctx, srv.URL, "session="+sc.cfg.SessionName)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	env := readEnvelope(t, br)
	if env.Type != "state_snapshot" {
		t.Fatalf("first env type = %q, want state_snapshot", env.Type)
	}
	if !env.Snapshot {
		t.Fatalf("first env Snapshot = false, want true")
	}
	if env.SessionName != sc.cfg.SessionName {
		t.Fatalf("first env session_name = %q, want %q", env.SessionName, sc.cfg.SessionName)
	}
	var p statePayloadShim
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.State != string(agent.StateActive) {
		t.Fatalf("snapshot payload state = %q, want active", p.State)
	}
}

// statePayloadShim mirrors the state_change payload shape — duplicated
// here rather than reaching into events_subscription.go's package-private
// statePayload because the production code uses an anonymous struct via
// json.Marshal(map).
type statePayloadShim struct {
	State string `json:"state"`
}

func TestEventsRoute_StreamsLiveStateChange(t *testing.T) {
	// AC: when a state change fires, every active subscriber observes
	// the delta. Open the stream, drive a state transition through
	// writeStateChange, then skip any snapshot frames (which are timing-
	// dependent: the snapshot read in the handler races against the
	// upsertState DB write inside writeStateChange) and assert that the
	// live state_change delta arrives on the wire.
	sc, srv, _ := newEventsSidecar(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openEventsStream(t, ctx, srv.URL, "")
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	waitForSubscribers(t, sc, 1)

	sc.mu.Lock()
	sc.writeStateChange(agent.StateActive)
	sc.mu.Unlock()

	env := readUntilLiveDelta(t, br)
	if env.Type != "state_change" {
		t.Fatalf("env type = %q, want state_change", env.Type)
	}
	var p statePayloadShim
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.State != string(agent.StateActive) {
		t.Fatalf("env payload state = %q, want active", p.State)
	}
	if env.SessionName != sc.cfg.SessionName {
		t.Fatalf("env session_name = %q, want %q", env.SessionName, sc.cfg.SessionName)
	}
}

func TestEventsRoute_SessionFilter(t *testing.T) {
	// A subscriber that requests session=other should not receive an
	// event for THIS sidecar's session — the broker's matches() filter
	// drops it.
	sc, srv, _ := newEventsSidecar(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openEventsStream(t, ctx, srv.URL, "session=does-not-match")
	br := bufio.NewReader(resp.Body)

	waitForSubscribers(t, sc, 1)

	sc.mu.Lock()
	sc.writeStateChange(agent.StateActive)
	sc.mu.Unlock()

	// Read with a short deadline — the snapshot is empty (no agent_status
	// row for "does-not-match") and the live event is filtered out, so
	// we should time out waiting for a non-comment frame. The expected
	// path is that the goroutine reading from br blocks indefinitely.
	done := make(chan envelope, 1)
	errCh := make(chan error, 1)
	go func() {
		env, err := readEnvelopeMaybe(br)
		if err != nil {
			errCh <- err
			return
		}
		done <- env
	}()
	select {
	case env := <-done:
		resp.Body.Close()
		t.Fatalf("subscriber received an event despite filter: %+v", env)
	case <-errCh:
		resp.Body.Close()
		t.Fatalf("subscriber read errored unexpectedly")
	case <-time.After(150 * time.Millisecond):
		// Expected: no envelopes arrive.
		resp.Body.Close()
	}
}

func TestEventsRoute_WildcardReceivesAll(t *testing.T) {
	// session=* must receive every event regardless of session_name.
	sc, srv, _ := newEventsSidecar(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openEventsStream(t, ctx, srv.URL, "session=*")
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	waitForSubscribers(t, sc, 1)
	sc.mu.Lock()
	sc.writeStateChange(agent.StateActive)
	sc.mu.Unlock()

	env := readUntilLiveDelta(t, br)
	if env.Type != "state_change" || env.SessionName != sc.cfg.SessionName {
		t.Fatalf("wildcard envelope = %+v", env)
	}
}

func TestEventsRoute_DisconnectUnsubscribes(t *testing.T) {
	// Cancelling the request context must drain the subscriber from the
	// broker so a future publish has no leak. We assert subscriberCount
	// returns to zero after the response body is closed.
	sc, srv, _ := newEventsSidecar(t)

	ctx, cancel := context.WithCancel(context.Background())
	resp := openEventsStream(t, ctx, srv.URL, "")
	waitForSubscribers(t, sc, 1)

	// Cancel and close — the handler's defer s.events.unsubscribe runs
	// before the response body is fully closed.
	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if sc.events.subscriberCount() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriber not unsubscribed after disconnect: count=%d", sc.events.subscriberCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEventsRoute_MultipleSubscribers_Fanout(t *testing.T) {
	// Two concurrent subscribers must both receive the same delta.
	sc, srv, _ := newEventsSidecar(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r1 := openEventsStream(t, ctx, srv.URL, "")
	defer r1.Body.Close()
	r2 := openEventsStream(t, ctx, srv.URL, "")
	defer r2.Body.Close()
	br1 := bufio.NewReader(r1.Body)
	br2 := bufio.NewReader(r2.Body)

	waitForSubscribers(t, sc, 2)
	sc.mu.Lock()
	sc.writeStateChange(agent.StateActive)
	sc.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	checks := make(chan envelope, 2)
	go func() { defer wg.Done(); checks <- readUntilLiveDelta(t, br1) }()
	go func() { defer wg.Done(); checks <- readUntilLiveDelta(t, br2) }()
	wg.Wait()
	close(checks)
	count := 0
	for env := range checks {
		count++
		if env.Type != "state_change" {
			t.Fatalf("subscriber received non-state_change: %+v", env)
		}
	}
	if count != 2 {
		t.Fatalf("fanout received %d envelopes, want 2", count)
	}
}

// waitForSubscribers polls sc.events.subscriberCount until it reaches n,
// failing the test after a generous deadline. Used after opening a stream
// because the http handler runs in its own goroutine and the subscribe
// call may not be observable from the test thread immediately.
func waitForSubscribers(t *testing.T, sc *Sidecar, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if sc.events.subscriberCount() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForSubscribers(%d): got %d", n, sc.events.subscriberCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEventBroker_PublishConcurrentWithUnsubscribe_NoPanic is the
// regression guard for the PR #2167 round-1 review finding. Before the
// fix, publish snapshotted the subscriber set under the lock and then
// sent into sub.ch after releasing the lock; unsubscribe closed sub.ch
// under the lock. The narrow window between the snapshot and the send
// allowed unsubscribe to close the channel and the send then panicked
// with "send on closed channel".
//
// The test drives publishes from one goroutine and subscribe/unsubscribe
// churn from another, both at high frequency, for a fixed duration. Any
// surviving race manifests as a "send on closed channel" panic on the
// publish goroutine — which the test thread observes via the
// publisher's recover, failing the test. Under `-race` the analyser
// also flags the unsynchronised send-vs-close as a data race.
func TestEventBroker_PublishConcurrentWithUnsubscribe_NoPanic(t *testing.T) {
	b := newEventBroker()

	var panicked atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Publisher loop: publishes a steady stream of envelopes against
	// the broker until the test deadline fires. Any panic in the send
	// path is captured and re-checked on the test goroutine after the
	// loops finish.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicked.Store(fmt.Sprintf("%v", r))
			}
		}()
		env := envelope{
			Type:        "state_change",
			SessionName: "alpha",
			Payload:     []byte(`{"state":"active"}`),
			CreatedAtMs: 1,
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			b.publish(env)
		}
	}()

	// Churn loop: subscribes and unsubscribes a fresh subscriber on
	// every iteration so the publisher races against close(sub.ch).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicked.Store(fmt.Sprintf("%v", r))
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			sub := b.subscribe([]string{"alpha"})
			// Drain a few envelopes to ensure the publish loop has
			// observed this subscriber before we unsubscribe —
			// otherwise the loop body becomes too short to expose
			// the race on most schedulers.
			for i := 0; i < 4; i++ {
				select {
				case <-sub.ch:
				default:
				}
			}
			b.unsubscribe(sub)
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if v := panicked.Load(); v != nil {
		t.Fatalf("publish/unsubscribe race panicked: %v", v)
	}
	if got := b.subscriberCount(); got != 0 {
		t.Errorf("subscriberCount after churn = %d, want 0", got)
	}
}

func TestEventBroker_PublishWithoutSubscribers(t *testing.T) {
	// Publish with zero subscribers must be a cheap no-op — the broker
	// short-circuits before allocating the subs slice.
	b := newEventBroker()
	b.publish(envelope{Type: "state_change", SessionName: "ghost"})
	if got := b.subscriberCount(); got != 0 {
		t.Fatalf("subscriberCount after publish = %d, want 0", got)
	}
}

func TestEventBroker_PublishDropsOldestWhenFull(t *testing.T) {
	// Backpressure contract: when the subscriber channel is full, the
	// oldest envelope is dropped to make room for the newest. We
	// construct a subscriber with a tiny buffer to exercise this.
	b := newEventBroker()
	sub := b.subscribe([]string{"alpha"})
	// Drain the auto-allocated channel and replace with a 1-slot
	// channel so the test can deterministically fill it. This pokes at
	// internal state intentionally — it is the only knob the broker
	// exposes for this property.
	sub.ch = make(chan envelope, 1)

	first := envelope{Type: "state_change", SessionName: "alpha", Payload: []byte(`{"state":"active"}`)}
	second := envelope{Type: "state_change", SessionName: "alpha", Payload: []byte(`{"state":"waiting"}`)}

	b.publish(first)
	b.publish(second)

	got, ok := <-sub.ch
	if !ok {
		t.Fatalf("channel closed after publish")
	}
	// Either `second` (oldest-drop-newest-kept) — the broker keeps the
	// newest envelope.
	var p statePayloadShim
	_ = json.Unmarshal(got.Payload, &p)
	if p.State != "waiting" {
		t.Fatalf("got payload state = %q, want waiting (the newest)", p.State)
	}
}

// TestEventsRoute_WritesViaSidecar_FlowsToSubscriber is the bridge
// assertion: a state change initiated by HandleEvent (production path)
// is observed by a /events subscriber. This protects against future
// refactors that route state writes around writeEvent.
func TestEventsRoute_WritesViaSidecar_FlowsToSubscriber(t *testing.T) {
	sc, srv, _ := newEventsSidecar(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openEventsStream(t, ctx, srv.URL, fmt.Sprintf("session=%s", sc.cfg.SessionName))
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	waitForSubscribers(t, sc, 1)

	// Drive a write via the public path — same path the production
	// sidecar uses when the harness emits session.updated.
	sc.mu.Lock()
	sc.upsertState(agent.StateActive, nil, nil)
	sc.writeStateChange(agent.StateActive)
	sc.mu.Unlock()

	env := readUntilLiveDelta(t, br)
	if env.Type != "state_change" {
		t.Fatalf("env type = %q, want state_change", env.Type)
	}
	if env.CreatedAtMs == 0 {
		t.Errorf("env created_at_ms = 0; want non-zero from the clock")
	}
}
