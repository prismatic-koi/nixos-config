package cmd

// reviews_log_test.go — `prism reviews log` on both routes (issue #3277).
//
//   - Direct route: a `host`-mode session has no PRISM_HOST_API. The CLI
//     opens prism.db and applies authz.AuthorizeReviewLog with the caller
//     from PRISM_SESSION_NAME.
//   - Proxy route: the real host-API handler of a worker sidecar, served on
//     a Unix socket. The client sets PRISM_SESSION_NAME to the coordinator,
//     and the host still decides as the worker.
//
// The host-API rule table is in
// internal/sidecar/host_api_review_log_test.go. This file checks that each
// route applies the rule and that the CLI output and exit status are right.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sidecar"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const (
	rlCmdRepo    = "prism-test-rlogcmd"
	rlCmdWorker  = "prism-test-rlogcmd@feature"
	rlCmdSibling = "prism-test-rlogcmd@sibling"
	rlCmdCoord   = "prism-test-rlogcmd@main"
	rlCmdForeign = "prism-test-rlogcmd-other@feature"
	rlCmdPR      = "3277"
)

// seedReviewLogSessions seeds the sessions in `host` isolation mode, and
// round 1 with a log for each worker. It returns the log content by parent.
func seedReviewLogSessions(t *testing.T, d *db.DB) map[string]string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, row := range []struct{ name, repo, role string }{
		{rlCmdWorker, rlCmdRepo, "worker"},
		{rlCmdSibling, rlCmdRepo, "worker"},
		{rlCmdCoord, rlCmdRepo, "coordinator"},
		{rlCmdForeign, rlCmdRepo + "-other", "worker"},
	} {
		if err := d.UpsertStatusSeedRootAgentName(row.name, row.repo, "/worktree/"+row.name, "idle", nil, nil, row.role, "", "host"); err != nil {
			t.Fatalf("seed %q: %v", row.name, err)
		}
	}
	content := map[string]string{}
	for _, parent := range []string{rlCmdWorker, rlCmdSibling, rlCmdForeign} {
		groupID, err := d.RegisterGroupWithPR(parent, rlCmdPR, 1)
		if err != nil {
			t.Fatalf("RegisterGroupWithPR(%q): %v", parent, err)
		}
		content[parent] = "MONITOR-LOG-MARKER " + parent + "\n"
		path := review.MonitorLogPath(groupID, rlCmdPR, 1)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content[parent]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return content
}

// runReviewsLogAs runs `prism reviews log <args>` with PRISM_SESSION_NAME
// set to caller, and returns the output and the error.
func runReviewsLogAs(t *testing.T, caller string, session string, round string) (string, error) {
	t.Helper()
	t.Setenv("PRISM_SESSION_NAME", caller)
	t.Setenv("TMUX", "")
	c := &cobra.Command{Use: "log"}
	c.Flags().String("session", "", "")
	if session != "" {
		if err := c.Flags().Set("session", session); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	c.SetOut(&out)
	err := runReviewsLog(c, []string{round})
	return out.String(), err
}

func TestReviewsLog_Direct_WorkerOwnSessionOnly(t *testing.T) {
	d := openStatsTestDB(t)
	content := seedReviewLogSessions(t, d)

	for _, session := range []string{"", rlCmdWorker} {
		out, err := runReviewsLogAs(t, rlCmdWorker, session, "1")
		if err != nil {
			t.Fatalf("--session=%q: %v", session, err)
		}
		if out != content[rlCmdWorker] {
			t.Errorf("--session=%q: output = %q, want %q", session, out, content[rlCmdWorker])
		}
	}

	for _, target := range []string{rlCmdSibling, rlCmdCoord, rlCmdForeign} {
		out, err := runReviewsLogAs(t, rlCmdWorker, target, "1")
		if err == nil || !strings.Contains(err.Error(), "its own session") {
			t.Errorf("--session=%s: err = %v, want a refusal", target, err)
		}
		if out != "" {
			t.Errorf("--session=%s: output = %q, want none", target, out)
		}
	}
}

func TestReviewsLog_Direct_CoordinatorOwnRepoOnly(t *testing.T) {
	d := openStatsTestDB(t)
	content := seedReviewLogSessions(t, d)

	for _, target := range []string{rlCmdWorker, rlCmdSibling} {
		out, err := runReviewsLogAs(t, rlCmdCoord, target, "1")
		if err != nil {
			t.Fatalf("--session=%s: %v", target, err)
		}
		if out != content[target] {
			t.Errorf("--session=%s: output = %q, want %q", target, out, content[target])
		}
	}

	out, err := runReviewsLogAs(t, rlCmdCoord, rlCmdForeign, "1")
	if err == nil || !strings.Contains(err.Error(), "own repo") || out != "" {
		t.Errorf("other repo: output = %q, err = %v, want a refusal and no output", out, err)
	}
}

func TestReviewsLog_Direct_UnresolvableCallerRefused(t *testing.T) {
	d := openStatsTestDB(t)
	seedReviewLogSessions(t, d)

	out, err := runReviewsLogAs(t, "", "", "1")
	if err == nil || !strings.Contains(err.Error(), "PRISM_SESSION_NAME") || out != "" {
		t.Errorf("output = %q, err = %v, want a refusal that names PRISM_SESSION_NAME", out, err)
	}
}

func TestReviewsLog_NoLogAndBadInput(t *testing.T) {
	d := openStatsTestDB(t)
	seedReviewLogSessions(t, d)
	if _, err := d.RegisterGroupWithPR(rlCmdWorker, rlCmdPR, 2); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ session, round, want string }{
		{"", "2", "has no monitor log"},
		{"", "5", "has no review round 5"},
		{"", "0", "positive integer"},
		{"", "../1", "positive integer"},
		{"", "1/2", "positive integer"},
		{"../" + rlCmdWorker, "1", "must not contain"},
		{"prism-test-rlogcmd@a/b", "1", "must not contain"},
	} {
		out, err := runReviewsLogAs(t, rlCmdWorker, tc.session, tc.round)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--session=%q round %q: err = %v, want it to contain %q", tc.session, tc.round, err, tc.want)
		}
		if out != "" {
			t.Errorf("--session=%q round %q: output = %q, want none", tc.session, tc.round, out)
		}
	}
}

// startReviewLogHostAPI serves the host-API handler of a worker sidecar on a
// Unix socket and points PRISM_HOST_API at it. The client gets an empty
// shadow DB, so a client that does not proxy finds no round.
func startReviewLogHostAPI(t *testing.T, d *db.DB) {
	t.Helper()
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	sc := sidecar.New(sidecar.Config{
		SessionName: rlCmdWorker,
		Repo:        rlCmdRepo,
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       sidecar.RealClock(),
		AgentRole:   "worker",
		Harness:     &harness.FakeHarness{},
		Logger:      log.New(io.Discard, "", 0),
	})
	sock := filepath.Join(stateguardtest.ShortTempDir(t, "rlg"), "host.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := &http.Server{Handler: sc.HostAPIHandlerForTest()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	t.Setenv("PRISM_HOST_API", "unix://"+sock)
	SetTestDBPath(filepath.Join(t.TempDir(), "shadow.db"))
	t.Cleanup(func() { SetTestDBPath("") })
}

// The client claims to be the coordinator with PRISM_SESSION_NAME. The host
// takes the caller from its own sidecar, so the claim widens nothing.
func TestReviewsLog_Proxy_HostDecidesCaller(t *testing.T) {
	hostDB, err := db.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hostDB.Close() })
	content := seedReviewLogSessions(t, hostDB)
	startReviewLogHostAPI(t, hostDB)

	out, err := runReviewsLogAs(t, rlCmdCoord, "", "1")
	if err != nil {
		t.Fatalf("own round: %v", err)
	}
	if out != content[rlCmdWorker] {
		t.Errorf("own round: output = %q, want the log of the sidecar session %q", out, content[rlCmdWorker])
	}

	out, err = runReviewsLogAs(t, rlCmdCoord, rlCmdSibling, "1")
	if err == nil || !strings.Contains(err.Error(), "its own session") {
		t.Errorf("sibling round: err = %v, want a refusal", err)
	}
	if out != "" || strings.Contains(errString(err), "MONITOR-LOG-MARKER") {
		t.Errorf("sibling round: output = %q, err = %v, want no log content", out, err)
	}

	_, err = runReviewsLogAs(t, rlCmdWorker, "", "4")
	if err == nil || !strings.Contains(err.Error(), "has no review round 4") {
		t.Errorf("missing round: err = %v, want a clear error", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
