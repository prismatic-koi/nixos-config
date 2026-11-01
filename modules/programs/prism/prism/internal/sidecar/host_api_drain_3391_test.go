package sidecar

// Tests for #3391: CloseHostAPI closes the host API and waits for each
// handler that runs, so that the sidecar does not remove its PID file while
// a handler can still start a podman client. The end-to-end test of the
// clean-exit path is in cmd/sidecar_clean_exit_3391_test.go.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/hostapiauth"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// blockingRun is a fake podman whose container command waits until the
// test releases it. It does not stop when its context ends.
type blockingRun struct {
	fake        *prismcontainertest.Fake
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingRun(t *testing.T) *blockingRun {
	b := &blockingRun{
		fake:    &prismcontainertest.Fake{ImagePresent: true},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	b.fake.OnRun = func(context.Context, io.Writer, io.Writer, []string) (int, error) {
		b.startOnce.Do(func() { close(b.started) })
		<-b.release
		return 0, nil
	}
	t.Cleanup(b.letGo)
	return b
}

func (b *blockingRun) letGo() { b.releaseOnce.Do(func() { close(b.release) }) }

// newDrainTestSidecar returns a sidecar with a database, a host-API secret,
// and the fake podman of b. It serves no listener: the tests call
// hostAPIHandler.
func newDrainTestSidecar(t *testing.T, b *blockingRun, bound time.Duration) (*Sidecar, string) {
	t.Helper()
	sc := newContainerTestSidecar(t, "worker", b.fake)
	sc.cfg.DB = openTestDB(t)
	sc.cfg.HostAPIDrainTimeout = bound
	secret := "drain-test-secret-3391"
	sc.hostAPISecret.Store(&secret)
	return sc, secret
}

// serveContainerRun sends POST /container/run through hostAPIHandler, the
// handler of each host-API listener, and returns the recorder when the
// handler returns.
func serveContainerRun(sc *Sidecar, secret string) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/container/run", strings.NewReader(`{"image":"alpine","command":["true"]}`))
		req.Header.Set(hostapiauth.Header, secret)
		rr := httptest.NewRecorder()
		sc.hostAPIHandler().ServeHTTP(rr, req)
		out <- rr
	}()
	return out
}

func waitDrain[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
	var zero T
	return zero
}

// AC: the sidecar waits for each host-API handler that runs before it
// exits. CloseHostAPI does not return while a container request runs, and
// returns nil after the request ends.
func TestCloseHostAPI_WaitsForTheHandlerThatRuns_3391(t *testing.T) {
	b := newBlockingRun(t)
	sc, secret := newDrainTestSidecar(t, b, 0)
	resp := serveContainerRun(sc, secret)
	waitDrain(t, b.started, "the podman client of the request did not start")

	closed := make(chan error, 1)
	go func() { closed <- sc.CloseHostAPI() }()
	select {
	case err := <-closed:
		t.Fatalf("CloseHostAPI returned %v while a container request ran", err)
	case <-time.After(300 * time.Millisecond):
	}

	b.letGo()
	if err := waitDrain(t, closed, "CloseHostAPI did not return after the request ended"); err != nil {
		t.Errorf("CloseHostAPI = %v, want nil after the request ended", err)
	}
	if rr := waitDrain(t, resp, "the request did not end"); rr.Code != http.StatusOK {
		t.Errorf("status = %d, body %s, want 200", rr.Code, rr.Body.String())
	}
}

// AC: when a handler does not end in the bound, CloseHostAPI returns an
// error that names the handler. A later call checks again, and returns nil
// after the handler ended.
func TestCloseHostAPI_ReportsTheHandlerPastTheBound_3391(t *testing.T) {
	b := newBlockingRun(t)
	sc, secret := newDrainTestSidecar(t, b, 200*time.Millisecond)
	resp := serveContainerRun(sc, secret)
	waitDrain(t, b.started, "the podman client of the request did not start")

	start := time.Now()
	err := sc.CloseHostAPI()
	var drainErr *HostAPIDrainError
	if !errors.As(err, &drainErr) {
		t.Fatalf("CloseHostAPI = %v, want a *HostAPIDrainError", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("CloseHostAPI took %s, want about the bound of 200ms", took)
	}
	if len(drainErr.Handlers) != 1 || !strings.HasPrefix(drainErr.Handlers[0], "POST /container/run (runs for ") {
		t.Errorf("handlers = %q, want the one POST /container/run", drainErr.Handlers)
	}
	if !strings.Contains(err.Error(), `"prism-test@container"`) {
		t.Errorf("error %q does not name the session", err)
	}
	if err := sc.CloseHostAPI(); err == nil {
		t.Error("a later CloseHostAPI = nil while the handler still runs")
	}

	b.letGo()
	waitDrain(t, resp, "the request did not end")
	if err := sc.CloseHostAPI(); err != nil {
		t.Errorf("CloseHostAPI after the handler ended = %v, want nil", err)
	}
}

// AC: no host-API handler starts after CloseHostAPI. A request that reaches
// the handler then, for example on a connection that the server accepted
// before the close, gets 503, and podman does not run.
func TestCloseHostAPI_NoHandlerStartsAfterTheClose_3391(t *testing.T) {
	b := newBlockingRun(t)
	b.letGo()
	sc, secret := newDrainTestSidecar(t, b, 0)
	if rr := waitDrain(t, serveContainerRun(sc, secret), "the request before the close did not end"); rr.Code != http.StatusOK {
		t.Fatalf("request before the close: status %d, body %s; want 200", rr.Code, rr.Body.String())
	}
	callsBefore := len(b.fake.Calls())

	if err := sc.CloseHostAPI(); err != nil {
		t.Fatalf("CloseHostAPI = %v, want nil", err)
	}
	rr := waitDrain(t, serveContainerRun(sc, secret), "the request after the close did not end")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), hostAPIStoppingMessage) {
		t.Errorf("request after the close: status %d, body %s; want 503 and the stopping message", rr.Code, rr.Body.String())
	}
	if calls := b.fake.Calls(); len(calls) != callsBefore {
		t.Errorf("podman calls after the close: %q, want none", calls[callsBefore:])
	}
}

// AC: the SIGTERM exit path (Shutdown) also closes the host API and waits
// for the handler that runs.
func TestShutdown_WaitsForTheHostAPIHandler_3391(t *testing.T) {
	b := newBlockingRun(t)
	sc, secret := newDrainTestSidecar(t, b, 0)
	resp := serveContainerRun(sc, secret)
	waitDrain(t, b.started, "the podman client of the request did not start")

	done := make(chan struct{})
	go func() {
		sc.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a container request ran")
	case <-time.After(300 * time.Millisecond):
	}

	b.letGo()
	waitDrain(t, done, "Shutdown did not return after the request ended")
	waitDrain(t, resp, "the request did not end")
	if err := sc.CloseHostAPI(); err != nil {
		t.Errorf("CloseHostAPI after Shutdown = %v, want nil", err)
	}
}
