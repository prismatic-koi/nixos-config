package cmd

// event_proxy_caller_test.go — the /event proxy from end to end (#3207).
//
// The sandboxed half is the real CLI client: `prism event <kind>` with
// PRISM_HOST_API set, run in this process. The host half is the real
// host-API handler of a worker sidecar, served on a Unix socket, which runs
// a real prism binary (isolatedPrismBinary) against the test DB. So each
// test covers the client, the caller check, the arg allowlist, and the host
// write together.
//
// The client opens a shadow DB (SetTestDBPath), not the host DB. If the
// client does not proxy, its write goes to the shadow DB, and the host-DB
// assertions fail.

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const (
	epRepo    = "prism-test-evtproxy"
	epWorker  = "prism-test-evtproxy@feature"
	epSibling = "prism-test-evtproxy@sibling"
	epCoord   = "prism-test-evtproxy@main"
	// epPlanted is a bare name outside every bare+worktree layout: the row
	// that `prism restore` starts as the root session of a non-worktree
	// project, with the coordinator role.
	epPlanted = "prism-test-evtproxy"
)

type eventProxyFixture struct {
	d        *db.DB
	dbPath   string
	sc       *sidecar.Sidecar
	worktree string // start worktree of the worker
	coordWT  string
	chosen   string // a directory that the worker controls
}

// newEventProxyFixture builds the prism binary, a test DB with a worker, a
// sibling worker, and a coordinator of one repo, and the sidecar of the
// worker. The sidecar serves its host-API handler on a Unix socket, and
// PRISM_HOST_API points at the socket.
func newEventProxyFixture(t *testing.T) *eventProxyFixture {
	t.Helper()
	bin, root := isolatedPrismBinary(t)

	// The host sidecar has no PRISM_HOST_API. This process does, because it
	// also runs the sandboxed client, so the host command must not inherit
	// it.
	hostPrism := filepath.Join(root, "bin", "host-prism")
	wrapper := "#!/bin/sh\nunset PRISM_HOST_API\nexec '" + bin + "' \"$@\"\n"
	if err := os.WriteFile(hostPrism, []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write host prism wrapper: %v", err)
	}

	layout := filepath.Join(root, "work", epRepo)
	f := &eventProxyFixture{
		dbPath:   rootDBPath(root),
		worktree: filepath.Join(layout, "feature"),
		coordWT:  filepath.Join(layout, "main"),
		chosen:   filepath.Join(root, "work", "chosen"),
	}
	for _, dir := range []string{filepath.Join(layout, ".bare"), f.worktree, f.coordWT, filepath.Join(layout, "sibling"), f.chosen, filepath.Dir(f.dbPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	d, err := db.Open(f.dbPath)
	if err != nil {
		t.Fatalf("open the host DB: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	f.d = d
	for _, row := range []struct{ name, wt, role string }{
		{epWorker, f.worktree, "worker"},
		{epSibling, filepath.Join(layout, "sibling"), "worker"},
		{epCoord, f.coordWT, "coordinator"},
	} {
		if err := d.UpsertStatusSeedRootAgentName(row.name, epRepo, row.wt, "active", nil, nil, row.role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", row.name, err)
		}
	}

	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	f.sc = sidecar.New(sidecar.Config{
		SessionName:     epWorker,
		Repo:            epRepo,
		Worktree:        f.worktree,
		DB:              d,
		Clock:           sidecar.RealClock(),
		AgentRole:       "worker",
		PrismBinaryPath: hostPrism,
		Harness:         &harness.FakeHarness{},
		Logger:          log.New(io.Discard, "", 0),
	})

	sock := filepath.Join(stateguardtest.ShortTempDir(t, "epc"), "host.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := &http.Server{Handler: f.sc.HostAPIHandlerForTest()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	t.Setenv("PRISM_HOST_API", "unix://"+sock)

	SetTestDBPath(filepath.Join(t.TempDir(), "shadow.db"))
	t.Cleanup(func() { SetTestDBPath("") })
	return f
}

// runSandboxed runs `prism <args>` in this process as the sandboxed client.
func runSandboxed(t *testing.T, args ...string) error {
	t.Helper()
	resetRootCmdFlags(t)
	rootCmd.SetArgs(args)
	return rootCmd.Execute()
}

func (f *eventProxyFixture) status(t *testing.T, name string) *db.Status {
	t.Helper()
	st, err := f.d.CurrentStatus(name)
	if err != nil {
		t.Fatalf("CurrentStatus(%q): %v", name, err)
	}
	return st
}

func (f *eventProxyFixture) eventCount(t *testing.T, name, eventType string) int {
	t.Helper()
	events, err := f.d.QueryEvents(name, 1000, nil, nil, []string{eventType})
	if err != nil {
		t.Fatalf("QueryEvents(%q, %q): %v", name, eventType, err)
	}
	return len(events)
}

func (f *eventProxyFixture) rootAgentName(t *testing.T, name string) string {
	t.Helper()
	role, _, err := f.d.RootAgentName(name)
	if err != nil {
		t.Fatalf("RootAgentName(%q): %v", name, err)
	}
	return role
}

// TestEventProxy_EndToEnd_OwnSessionEventsReachHostDB pins the functional
// half: each event that a worker emits on its own session through the
// proxy is still written to the host DB. The steps run in one order, because
// each one moves the state of the same row.
func TestEventProxy_EndToEnd_OwnSessionEventsReachHostDB(t *testing.T) {
	f := newEventProxyFixture(t)

	steps := []struct {
		name      string
		args      []string
		eventType string
		wantState string
	}{
		{"state-change", []string{"event", "state-change", "--session", epWorker, "--state", "active", "--worktree", f.worktree}, "state_change", "active"},
		{"compaction", []string{"event", "compaction", "--session", epWorker}, "compaction", "compacting"},
		{"doom-loop-detected", []string{"event", "doom-loop-detected", "--session", epWorker, "--tool", "bash", "--pattern", "gh issue view", "--count", "5"}, "doom_loop_detected", "compacting"},
		{"pane-died", []string{"event", "pane-died", "--session", epWorker, "--window", "agent", "--exit-code", "1"}, "state_change", "interrupted"},
		{"tmux-session-start without a role", []string{"event", "tmux-session-start", "--session", epWorker, "--worktree", f.worktree}, "tmux_session_start", "idle"},
		{"error", []string{"event", "error", "--session", epWorker, "--message", "it broke"}, "error", "error"},
	}
	for _, step := range steps {
		before := f.eventCount(t, epWorker, step.eventType)
		if err := runSandboxed(t, step.args...); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := f.eventCount(t, epWorker, step.eventType); got != before+1 {
			t.Errorf("%s: %s events = %d, want %d", step.name, step.eventType, got, before+1)
		}
		st := f.status(t, epWorker)
		if st.State != step.wantState {
			t.Errorf("%s: state = %q, want %q", step.name, st.State, step.wantState)
		}
		if st.Worktree != f.worktree || st.Repo != epRepo {
			t.Errorf("%s: worktree = %q, repo = %q, want %q, %q", step.name, st.Worktree, st.Repo, f.worktree, epRepo)
		}
		if role := f.rootAgentName(t, epWorker); role != "worker" {
			t.Errorf("%s: root_agent_name = %q, want worker", step.name, role)
		}
	}

	t.Run("tmux-session-end", func(t *testing.T) {
		// The host run asks tmux whether the session is still live, and
		// keeps the row when tmux cannot answer.
		if _, err := exec.LookPath("tmux"); err != nil {
			t.Skip("tmux not found in PATH")
		}
		if err := runSandboxed(t, "event", "tmux-session-end", "--session", epWorker); err != nil {
			t.Fatalf("tmux-session-end: %v", err)
		}
		if got := f.eventCount(t, epWorker, "tmux_session_end"); got != 1 {
			t.Errorf("tmux_session_end events = %d, want 1", got)
		}
		if st := f.status(t, epWorker); st.EndedAt == nil {
			t.Errorf("ended_at is not set")
		}
	})
}

// TestEventProxy_EndToEnd_WorkerCannotForgeRoleOrWriteOtherRows pins the
// security half with the real client:
//
//   - `prism event tmux-session-start --session <self> --agent-role
//     coordinator` is refused, root_agent_name stays "worker", and
//     requireCoordinator and /escalate for another session still refuse the
//     worker.
//   - An event for another session is refused, and no row changes or
//     appears.
//   - A worktree that is not the start worktree is refused.
//   - `prism restore` then reads only the rows that prism wrote at start,
//     with the role and the worktree that prism wrote.
func TestEventProxy_EndToEnd_WorkerCannotForgeRoleOrWriteOtherRows(t *testing.T) {
	f := newEventProxyFixture(t)
	coordBefore := *f.status(t, epCoord)
	siblingBefore := *f.status(t, epSibling)

	refused := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"forged role", []string{"event", "tmux-session-start", "--session", epWorker, "--worktree", f.worktree, "--agent-role", "coordinator"}, `arg "agent-role" is not allowed`},
		{"planted root session", []string{"event", "tmux-session-start", "--session", epPlanted, "--worktree", f.chosen}, "only for itself"},
		{"planted root session with a role", []string{"event", "tmux-session-start", "--session", epPlanted, "--worktree", f.chosen, "--agent-role", "coordinator"}, "only for itself"},
		{"move the coordinator", []string{"event", "tmux-session-start", "--session", epCoord, "--worktree", f.chosen}, "only for itself"},
		{"state of the sibling", []string{"event", "state-change", "--session", epSibling, "--state", "error", "--worktree", f.chosen}, "only for itself"},
		{"end the coordinator", []string{"event", "tmux-session-end", "--session", epCoord}, "only for itself"},
		{"error on the sibling", []string{"event", "error", "--session", epSibling, "--message", "x"}, "only for itself"},
		{"move itself by start", []string{"event", "tmux-session-start", "--session", epWorker, "--worktree", f.chosen}, "only its own worktree"},
		{"move itself by state", []string{"event", "state-change", "--session", epWorker, "--state", "active", "--worktree", f.chosen}, "only its own worktree"},
	}
	for _, tc := range refused {
		err := runSandboxed(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want an error with %q", tc.name, err, tc.wantErr)
		}
	}

	if st := f.status(t, epPlanted); st != nil {
		t.Errorf("a row for %q exists: %+v", epPlanted, *st)
	}
	worker := f.status(t, epWorker)
	if worker.Worktree != f.worktree || worker.State != "active" || worker.EndedAt != nil {
		t.Errorf("worker row: worktree = %q, state = %q, ended = %v; want %q, active, not ended",
			worker.Worktree, worker.State, worker.EndedAt != nil, f.worktree)
	}
	if role := f.rootAgentName(t, epWorker); role != "worker" {
		t.Errorf("worker root_agent_name = %q, want worker", role)
	}
	for _, other := range []struct {
		before db.Status
		name   string
	}{{coordBefore, epCoord}, {siblingBefore, epSibling}} {
		after := f.status(t, other.name)
		if after.Worktree != other.before.Worktree || after.State != other.before.State || after.EndedAt != nil ||
			f.rootAgentName(t, other.name) != *other.before.RootAgentName {
			t.Errorf("%s row changed: before %+v, after %+v", other.name, other.before, *after)
		}
	}
	if got := f.eventCount(t, epWorker, "tmux_session_start"); got != 0 {
		t.Errorf("worker tmux_session_start events = %d, want 0", got)
	}

	handler := f.sc.HostAPIHandlerForTest()
	for _, gate := range []struct{ path, body string }{
		{"/spawn", `{"branch":"feature-x","prompt":"do the thing"}`},
		{"/investigate", `{"prompt":"look into this"}`},
		{"/escalate", `{"prompt":"help","from":"` + epSibling + `"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://prism-hostapi"+gate.path, strings.NewReader(gate.body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s: status = %d, body = %s, want 403", gate.path, rr.Code, rr.Body.String())
		}
	}

	// The input of `prism restore`: the open rows, and the role that it
	// starts each one with.
	SetTestDBPath(f.dbPath)
	out := captureStdout(t, func() {
		if err := Restore(true); err != nil {
			t.Fatalf("Restore(dryRun=true): %v", err)
		}
	})
	var restored []string
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(line, "would restore: "); ok {
			restored = append(restored, name)
		}
	}
	slices.Sort(restored)
	want := []string{
		epWorker + " (worktree=" + f.worktree + ")",
		epCoord + " (worktree=" + f.coordWT + ")",
		epSibling + " (worktree=" + siblingBefore.Worktree + ")",
	}
	slices.Sort(want)
	if !slices.Equal(restored, want) {
		t.Errorf("restore would restore %q, want %q", restored, want)
	}
	if role := session.DefaultAgentForSession(epWorker, f.worktree, "", f.d); role != "worker" {
		t.Errorf("restore role of the worker = %q, want worker", role)
	}
	if role := session.DefaultAgentForSession(epCoord, f.coordWT, "", f.d); role != "coordinator" {
		t.Errorf("restore role of the coordinator = %q, want coordinator", role)
	}
}
