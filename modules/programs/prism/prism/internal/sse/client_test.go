package sse

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseServer creates an httptest.Server that writes the given SSE payload and
// then blocks until the client disconnects. This prevents the client from
// seeing a connection close and attempting to reconnect during tests that don't
// exercise reconnection.
func sseServer(payload string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		fmt.Fprint(w, payload)
		flusher.Flush()

		// Block until the client disconnects to avoid triggering reconnection.
		<-r.Context().Done()
	}))
}

// collectEvents reads up to n events from ch, with a timeout per event.
func collectEvents(t *testing.T, ch <-chan Event, n int) []Event {
	t.Helper()
	var events []Event
	for range n {
		select {
		case evt, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, evt)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event (got %d of %d)", len(events), n)
		}
	}
	return events
}

func TestParseSimpleEvent(t *testing.T) {
	srv := sseServer("event: greeting\ndata: hello\n\n")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "greeting" {
		t.Errorf("Type = %q, want %q", events[0].Type, "greeting")
	}
	if events[0].Data != "hello" {
		t.Errorf("Data = %q, want %q", events[0].Data, "hello")
	}
}

func TestMultipleEvents(t *testing.T) {
	payload := strings.Join([]string{
		"event: first",
		"data: one",
		"",
		"event: second",
		"data: two",
		"",
		"",
	}, "\n")

	srv := sseServer(payload)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 2)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != "first" || events[0].Data != "one" {
		t.Errorf("event[0] = %+v", events[0])
	}
	if events[1].Type != "second" || events[1].Data != "two" {
		t.Errorf("event[1] = %+v", events[1])
	}
}

func TestMultiLineData(t *testing.T) {
	payload := "event: multi\ndata: line1\ndata: line2\ndata: line3\n\n"

	srv := sseServer(payload)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	want := "line1\nline2\nline3"
	if events[0].Data != want {
		t.Errorf("Data = %q, want %q", events[0].Data, want)
	}
}

func TestDefaultEventType(t *testing.T) {
	// No "event:" field — should default to "message".
	srv := sseServer("data: no-type\n\n")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "message" {
		t.Errorf("Type = %q, want %q", events[0].Type, "message")
	}
}

func TestCommentsIgnored(t *testing.T) {
	payload := ": this is a comment\nevent: real\ndata: value\n: another comment\n\n"

	srv := sseServer(payload)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "real" || events[0].Data != "value" {
		t.Errorf("event = %+v", events[0])
	}
}

func TestContextCancellation(t *testing.T) {
	// Server that blocks until the request context is done.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}

		// Block until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{}

	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Cancel the context and verify the channel closes.
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after cancel, but got an event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel was not closed within timeout after cancel")
	}
}

func TestReconnection(t *testing.T) {
	var mu sync.Mutex
	attempt := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := attempt
		attempt++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// First connection: send one event then close.
		// Second connection: send another event then close.
		evt := fmt.Sprintf("event: attempt\ndata: %d\n\n", current)
		fmt.Fprint(w, evt)
		flusher.Flush()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &Client{
		InitialRetryDelay: 50 * time.Millisecond,
		MaxRetryDelay:     200 * time.Millisecond,
	}

	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// We should get events from at least 2 connections.
	events := collectEvents(t, ch, 2)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events across reconnections, got %d", len(events))
	}
	if events[0].Data != "0" {
		t.Errorf("first event data = %q, want %q", events[0].Data, "0")
	}
	if events[1].Data != "1" {
		t.Errorf("second event data = %q, want %q", events[1].Data, "1")
	}
}

func TestMidEventDisconnect(t *testing.T) {
	// Server sends an incomplete event (no trailing blank line) then closes.
	// Followed by a proper event on reconnect.
	var mu sync.Mutex
	attempt := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := attempt
		attempt++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		if current == 0 {
			// Incomplete event — no trailing empty line.
			fmt.Fprint(w, "event: broken\ndata: partial")
			flusher.Flush()
			return
		}

		// Complete event on second connection.
		fmt.Fprint(w, "event: recovered\ndata: complete\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &Client{
		InitialRetryDelay: 50 * time.Millisecond,
		MaxRetryDelay:     200 * time.Millisecond,
	}

	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The incomplete event should be discarded; we should only get the
	// recovered event from the second connection.
	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "recovered" {
		t.Errorf("Type = %q, want %q", events[0].Type, "recovered")
	}
}

func TestBufferDropsEvents(t *testing.T) {
	// Generate more events than the buffer can hold.
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprintf("event: flood\ndata: %d\n", i))
	}
	payload := strings.Join(lines, "\n") + "\n"

	srv := sseServer(payload)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{
		BufferSize: 2, // Tiny buffer to force drops.
	}

	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Give the producer time to fill and overflow the buffer.
	time.Sleep(200 * time.Millisecond)

	// Drain whatever made it through.
	var got int
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto done
			}
			got++
		case <-time.After(500 * time.Millisecond):
			goto done
		}
	}
done:

	// We should have gotten some events but not necessarily all 10.
	if got == 0 {
		t.Error("expected at least some events to be delivered")
	}
	// The key assertion is that the client did not panic or deadlock.
}

func TestInitialConnectionError(t *testing.T) {
	// Connect to a URL where nothing is listening. Connect now retries with
	// backoff, so we cancel the context quickly to get a clean error return.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	client := &Client{
		InitialRetryDelay: 50 * time.Millisecond,
		MaxRetryDelay:     100 * time.Millisecond,
	}

	_, err := client.Connect(ctx, "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error when context cancelled with no server, got nil")
	}
}

// TestConnectRetryOnStartup verifies that Connect retries the initial
// connection with backoff when the server is not yet ready, and succeeds once
// the server starts accepting connections.
func TestConnectRetryOnStartup(t *testing.T) {
	var mu sync.Mutex
	ready := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isReady := ready
		mu.Unlock()

		if !isReady {
			// Simulate server not ready yet — refuse with 503.
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, "event: ready\ndata: ok\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{
		InitialRetryDelay: 50 * time.Millisecond,
		MaxRetryDelay:     200 * time.Millisecond,
	}

	// Start the connect attempt before the server is ready.
	done := make(chan struct{})
	var ch <-chan Event
	var connectErr error
	go func() {
		defer close(done)
		ch, connectErr = client.Connect(ctx, srv.URL)
	}()

	// Let it attempt and fail at least once, then flip the server to ready.
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	ready = true
	mu.Unlock()

	// Wait for Connect to return.
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Connect did not return within timeout after server became ready")
	}

	if connectErr != nil {
		t.Fatalf("Connect returned error after server became ready: %v", connectErr)
	}

	// Expect the "ready" event delivered via the channel.
	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "ready" || events[0].Data != "ok" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestDataWithColonNoSpace(t *testing.T) {
	// "data:value" (no space after colon) should still parse correctly.
	srv := sseServer("data:hello\n\n")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "hello" {
		t.Errorf("Data = %q, want %q", events[0].Data, "hello")
	}
}

func TestServerConnectedEvent(t *testing.T) {
	// Simulate opencode's initial server.connected event.
	srv := sseServer("event: server.connected\ndata: {\"version\":\"1.0\"}\n\n")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "server.connected" {
		t.Errorf("Type = %q, want %q", events[0].Type, "server.connected")
	}
	if events[0].Data != `{"version":"1.0"}` {
		t.Errorf("Data = %q", events[0].Data)
	}
}

// TestLargeDataLine verifies that a single SSE data: line larger than the
// default bufio.MaxScanTokenSize (64 KiB) is parsed without triggering a
// reconnect. This exercises the root-cause fix for the reconnect storm:
// opencode sends message.part.updated events with full LLM text in the data
// field, which can exceed 64 KiB for long responses.
func TestLargeDataLine(t *testing.T) {
	// Generate a payload larger than the default 64 KiB scanner limit.
	const size = 128 * 1024 // 128 KiB
	largeData := strings.Repeat("x", size)

	// Server sends one event with a large data line, then blocks.
	var connectionCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectionCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		fmt.Fprintf(w, "event: large\ndata: %s\n\n", largeData)
		flusher.Flush()

		// Block until the client disconnects to prevent triggering reconnection.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &Client{}
	ch, err := client.Connect(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	events := collectEvents(t, ch, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "large" {
		t.Errorf("Type = %q, want %q", events[0].Type, "large")
	}
	if events[0].Data != largeData {
		t.Errorf("Data length = %d, want %d (first 20 chars: %q)", len(events[0].Data), size, events[0].Data[:20])
	}

	// Exactly one connection should have been made — no spurious reconnect.
	if connectionCount != 1 {
		t.Errorf("connection count = %d, want 1 (large payload should not cause reconnect)", connectionCount)
	}
}
