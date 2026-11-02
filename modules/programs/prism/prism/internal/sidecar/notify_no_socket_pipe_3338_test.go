package sidecar

// A notification to a coordinator whose harness has no socket pipe fails
// with a logged error, and prism sends no HTTP request to the harness port of
// that coordinator (#3338, item 2).

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNotifyCoordinator_NoSocketPipeHarness_LoggedAndNoHTTP_3338(t *testing.T) {
	d := openTestDB(t)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	// The coordinator row names a harness with no socket pipe, and a harness
	// port that the server above listens on.
	seedCoordinator(t, d, "test-repo", "coord-sid-3338")
	if err := d.QueryRow(
		"UPDATE agent_status SET harness = '', harness_port = ? WHERE session_name = ? RETURNING session_name",
		port, "test-repo@main",
	).Scan(new(string)); err != nil {
		t.Fatalf("set harness and port of the coordinator: %v", err)
	}

	worker, _ := newWorkerSidecar(t, d)
	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "finished", nil, nil)
	var logBuf bytes.Buffer
	worker.cfg.Logger = log.New(&logBuf, "", 0)

	worker.notifyCoordinator("")

	logged := logBuf.String()
	if !strings.Contains(logged, "notifyCoordinator: FAILED") || !strings.Contains(logged, "has no socket pipe") {
		t.Errorf("log = %q, want a FAILED line that says the harness has no socket pipe", logged)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("HTTP requests to the harness port of the coordinator = %d, want 0", n)
	}
	if !waitForBusMessageFailed(t, d, "test-repo@main") {
		t.Error("no failed bus message to the coordinator, want one")
	}
}
