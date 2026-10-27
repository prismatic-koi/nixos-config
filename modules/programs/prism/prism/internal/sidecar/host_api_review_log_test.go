package sidecar

// host_api_review_log_test.go — GET /groups/monitor-log, the host-API half
// of `prism reviews log` (issue #3277).
//
// The access rule is authz.AuthorizeReviewLog. These tests drive the real
// handler of a worker sidecar and of a coordinator sidecar, and check:
//
//   - a worker reads each round of its own session;
//   - a worker is refused another session, and the response holds no log
//     content;
//   - a coordinator reads a session of its own repo, and is refused a
//     session of another repo;
//   - the caller is Config.SessionName and the role is the start config,
//     whatever the request and the DB row say;
//   - a round or a session that holds ".." or "/" is refused;
//   - a round with no log gives 404.
//
// Every sidecar is built through newRoleGateSidecarWithDB on a
// sidecartest.NewIsolated bus, so $XDG_STATE_HOME is a t.TempDir() and the
// review-monitor directory is under it.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

const (
	rlRepo      = "prism-test-rlog"
	rlOtherRepo = "prism-test-rlog-other"
	rlWorker    = "prism-test-rlog@feature"
	rlSibling   = "prism-test-rlog@sibling"
	rlCoord     = "prism-test-rlog@main"
	rlForeign   = "prism-test-rlog-other@feature"
	rlPR        = "3277"
)

type reviewLogFixture struct {
	d *db.DB
	// content maps "<parent>/<round>" to the log content of that round.
	content map[string]string
}

// newReviewLogFixture seeds a worker, a sibling worker, and a coordinator of
// one repo, and a worker of another repo. The worker has rounds 1 and 2, and
// each other worker has round 1. Every round has a log with a unique marker.
func newReviewLogFixture(t *testing.T) *reviewLogFixture {
	t.Helper()
	return newReviewLogFixtureRoles(t, "worker", "coordinator")
}

// newReviewLogFixtureRoles is newReviewLogFixture with the root_agent_name
// rows of the worker and the coordinator given by the caller. One repo can
// have one active coordinator row only.
func newReviewLogFixtureRoles(t *testing.T, workerRow, coordRow string) *reviewLogFixture {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	f := &reviewLogFixture{d: bus.DB, content: map[string]string{}}
	for _, row := range []struct{ name, repo, role string }{
		{rlWorker, rlRepo, workerRow},
		{rlSibling, rlRepo, "worker"},
		{rlCoord, rlRepo, coordRow},
		{rlForeign, rlOtherRepo, "worker"},
	} {
		if err := bus.DB.UpsertStatusSeedRootAgentName(row.name, row.repo, "/tmp/"+row.repo, "active", nil, nil, row.role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", row.name, err)
		}
	}
	for _, r := range []struct {
		parent string
		round  int
	}{{rlWorker, 1}, {rlWorker, 2}, {rlSibling, 1}, {rlForeign, 1}} {
		f.addRound(t, r.parent, r.round, true)
	}
	return f
}

// addRound registers a review group and, when withLog is true, writes its
// monitor log.
func (f *reviewLogFixture) addRound(t *testing.T, parent string, round int, withLog bool) {
	t.Helper()
	groupID, err := f.d.RegisterGroupWithPR(parent, rlPR, round)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR(%q, %d): %v", parent, round, err)
	}
	if !withLog {
		return
	}
	content := "MONITOR-LOG-MARKER " + parent + " round " + strconv.Itoa(round) + "\n"
	path := review.MonitorLogPath(groupID, rlPR, round)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.content[parent+"/"+strconv.Itoa(round)] = content
}

func (f *reviewLogFixture) sidecar(t *testing.T, caller, repo, agentRole string) *Sidecar {
	t.Helper()
	return newRoleGateSidecarWithDB(t, caller, repo, agentRole, failingStub, f.d)
}

func getReviewLog(t *testing.T, sc *Sidecar, params url.Values) (int, string) {
	t.Helper()
	rr := doHostAPI(t, sc, http.MethodGet, "/groups/monitor-log?"+params.Encode(), "")
	return rr.Code, rr.Body.String()
}

func reviewLogContents(t *testing.T, body string) []string {
	t.Helper()
	var resp struct {
		Logs []review.MonitorLog `json:"logs"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	var out []string
	for _, l := range resp.Logs {
		out = append(out, l.Content)
	}
	return out
}

func TestHostAPIReviewLog_WorkerReadsOwnRounds(t *testing.T) {
	f := newReviewLogFixture(t)
	sc := f.sidecar(t, rlWorker, rlRepo, "worker")

	for _, params := range []url.Values{
		{"round": {"1"}},
		{"round": {"2"}},
		{"round": {"1"}, "session": {rlWorker}},
	} {
		code, body := getReviewLog(t, sc, params)
		if code != http.StatusOK {
			t.Fatalf("%v: status = %d, body = %q, want 200", params, code, body)
		}
		want := f.content[rlWorker+"/"+params.Get("round")]
		if got := reviewLogContents(t, body); len(got) != 1 || got[0] != want {
			t.Errorf("%v: logs = %q, want [%q]", params, got, want)
		}
	}
}

// The worker sends the name of another session. The request is refused,
// and the body holds no log content of any session.
func TestHostAPIReviewLog_WorkerRefusedOtherSession(t *testing.T) {
	f := newReviewLogFixture(t)
	sc := f.sidecar(t, rlWorker, rlRepo, "worker")

	for _, target := range []string{rlSibling, rlCoord, rlForeign} {
		code, body := getReviewLog(t, sc, url.Values{"round": {"1"}, "session": {target}})
		if code != http.StatusForbidden {
			t.Errorf("session=%s: status = %d, body = %q, want 403", target, code, body)
		}
		if strings.Contains(body, "MONITOR-LOG-MARKER") {
			t.Errorf("session=%s: refusal holds log content: %q", target, body)
		}
	}
}

// The DB row of the worker says "coordinator". The start config says
// "worker", and the start config decides the role. The request carries no
// value that can change the caller.
func TestHostAPIReviewLog_CallerFromSidecarNotRequest(t *testing.T) {
	f := newReviewLogFixtureRoles(t, "coordinator", "worker")
	sc := f.sidecar(t, rlWorker, rlRepo, "worker")

	code, body := getReviewLog(t, sc, url.Values{
		"round":   {"1"},
		"session": {rlSibling},
		"caller":  {rlCoord},
		"from":    {rlCoord},
	})
	if code != http.StatusForbidden || strings.Contains(body, "MONITOR-LOG-MARKER") {
		t.Errorf("status = %d, body = %q, want 403 with no log content", code, body)
	}
}

func TestHostAPIReviewLog_CoordinatorOwnRepoOnly(t *testing.T) {
	f := newReviewLogFixture(t)
	sc := f.sidecar(t, rlCoord, rlRepo, "coordinator")

	for _, target := range []string{rlWorker, rlSibling} {
		code, body := getReviewLog(t, sc, url.Values{"round": {"1"}, "session": {target}})
		if code != http.StatusOK {
			t.Fatalf("session=%s: status = %d, body = %q, want 200", target, code, body)
		}
		if got := reviewLogContents(t, body); len(got) != 1 || got[0] != f.content[target+"/1"] {
			t.Errorf("session=%s: logs = %q, want [%q]", target, got, f.content[target+"/1"])
		}
	}

	code, body := getReviewLog(t, sc, url.Values{"round": {"1"}, "session": {rlForeign}})
	if code != http.StatusForbidden || strings.Contains(body, "MONITOR-LOG-MARKER") {
		t.Errorf("other repo: status = %d, body = %q, want 403 with no log content", code, body)
	}
}

func TestHostAPIReviewLog_RefusesPathShapes(t *testing.T) {
	f := newReviewLogFixture(t)
	for _, role := range []struct{ caller, agentRole string }{
		{rlWorker, "worker"},
		{rlCoord, "coordinator"},
	} {
		sc := f.sidecar(t, role.caller, rlRepo, role.agentRole)
		for _, params := range []url.Values{
			{"round": {"../1"}},
			{"round": {"1/.."}},
			{"round": {"1/2"}},
			{"round": {".."}},
			{"round": {"0"}},
			{"round": {"-1"}},
			{"round": {"one"}},
			{},
			{"round": {"1"}, "session": {"../" + rlWorker}},
			{"round": {"1"}, "session": {rlWorker + "/.."}},
			{"round": {"1"}, "session": {"prism-test-rlog@a/b"}},
			{"round": {"1"}, "session": {".."}},
		} {
			code, body := getReviewLog(t, sc, params)
			if code != http.StatusBadRequest {
				t.Errorf("%s %v: status = %d, body = %q, want 400", role.agentRole, params, code, body)
			}
			if strings.Contains(body, "MONITOR-LOG-MARKER") {
				t.Errorf("%s %v: refusal holds log content: %q", role.agentRole, params, body)
			}
		}
	}
}

func TestHostAPIReviewLog_NoLog404(t *testing.T) {
	f := newReviewLogFixture(t)
	f.addRound(t, rlWorker, 3, false)
	sc := f.sidecar(t, rlWorker, rlRepo, "worker")

	for round, want := range map[string]string{"3": "has no monitor log", "9": "has no review round 9"} {
		code, body := getReviewLog(t, sc, url.Values{"round": {round}})
		if code != http.StatusNotFound {
			t.Errorf("round %s: status = %d, body = %q, want 404", round, code, body)
		}
		if !strings.Contains(body, want) {
			t.Errorf("round %s: body = %q, want it to contain %q", round, body, want)
		}
	}
}

func TestHostAPIReviewLog_GetOnly(t *testing.T) {
	f := newReviewLogFixture(t)
	sc := f.sidecar(t, rlWorker, rlRepo, "worker")
	rr := doHostAPI(t, sc, http.MethodPost, "/groups/monitor-log?round=1", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", rr.Code)
	}
}
