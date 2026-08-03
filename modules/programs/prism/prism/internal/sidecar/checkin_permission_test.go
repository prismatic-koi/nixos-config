package sidecar

// checkin_permission_test.go — issue #2587.
//
// The three-tier permission model for GET /checkin. Before #2587 the endpoint
// had one rule (requireCoordinator) and every worker got 403, including a
// worker reading the review agents it had just spawned for its own PR. These
// tests pin the replacement rule from the outside — through the HTTP handler,
// not through the predicate alone — so a regression in the wiring fails here
// as well as a regression in the logic.
//
// Tier 1 (worker):     the review agents of its own session, and nothing else.
// Tier 2 (coordinator): own repo plus cross-repo coordinators. Unchanged.
// Tier 3 (privileged):  any session in any repo, audited.
//
// # Isolation contract (#1608)
//
// Every sidecar here is built on sidecartest.NewIsolated(t, ""), so
// $XDG_STATE_HOME points at a t.TempDir(), PRISM_TEST_MODE_RESTRICT_HOSTAPI is
// set, and the DB is a test-scoped SQLite file opened through
// sidecartest.OpenDB. Session names carry the "prism-test-checkin" prefix so a
// leaked write cannot collide with a live session on the developer's host.
//
// Config.CheckinPrivilegedRepos is set explicitly per test. It is never loaded
// from disk here: the production loader reads $XDG_CONFIG_HOME, which this
// package does not redirect, so a disk read would make the tier-3 tests depend
// on the developer's own ~/.config/prism/.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// ── fixture ──────────────────────────────────────────────────────────────────

// checkinFixture is one isolated DB plus the helpers to populate it with the
// session shapes the permission tiers discriminate between.
type checkinFixture struct {
	t  *testing.T
	DB *db.DB
}

func newCheckinFixture(t *testing.T) *checkinFixture {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	return &checkinFixture{t: t, DB: bus.DB}
}

// seedSession writes an agent_status row with an explicit root_agent_name.
// Pass rootAgentName "" to seed a pre-migration row whose root_agent_name is
// NULL.
func (f *checkinFixture) seedSession(sessionName, repo, rootAgentName string) {
	f.t.Helper()
	if rootAgentName == "" {
		if err := f.DB.UpsertStatus(sessionName, repo, "/tmp/"+repo, "active", nil, nil); err != nil {
			f.t.Fatalf("seed %q: %v", sessionName, err)
		}
		return
	}
	if err := f.DB.UpsertStatusSeedRootAgentName(
		sessionName, repo, "/tmp/"+repo, "active", nil, nil, rootAgentName, "", "",
	); err != nil {
		f.t.Fatalf("seed %q (root_agent_name=%q): %v", sessionName, rootAgentName, err)
	}
}

// seedReviewGroup registers a review group owned by parent and attaches each
// named member to it, exactly as `prism review` does at spawn time
// (RegisterGroupWithPR followed by SetGroupID per member). Returns the
// group_id so a test can delete the row and exercise the orphan path.
func (f *checkinFixture) seedReviewGroup(parent, repo string, round int, members ...string) string {
	f.t.Helper()
	groupID, err := f.DB.RegisterGroupWithPR(parent, "42", round)
	if err != nil {
		f.t.Fatalf("RegisterGroupWithPR(%q): %v", parent, err)
	}
	for _, m := range members {
		f.seedSession(m, repo, "worker")
		if err := f.DB.SetGroupID(m, groupID); err != nil {
			f.t.Fatalf("SetGroupID(%q, %q): %v", m, groupID, err)
		}
	}
	return groupID
}

// sidecarFor builds a Sidecar for sessionName against the fixture DB.
// privilegedRepos is written to Config.CheckinPrivilegedRepos; pass nil for
// the default "no repo is privileged" configuration.
func (f *checkinFixture) sidecarFor(sessionName, repo, agentRole string, privilegedRepos []string) *Sidecar {
	f.t.Helper()
	return New(Config{
		SessionName:            sessionName,
		Repo:                   repo,
		Worktree:               f.t.TempDir(),
		HarnessURL:             "http://localhost:14000",
		DB:                     f.DB,
		Clock:                  newTestClock(),
		AgentRole:              agentRole,
		CheckinPrivilegedRepos: privilegedRepos,
		Harness:                newSSEHarness(),
		Logger:                 log.New(os.Stderr, "", 0),
	})
}

// checkin issues GET /checkin?session=<target> against sc.
func checkin(t *testing.T, sc *Sidecar, target string) (int, string) {
	t.Helper()
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+target, "")
	return rr.Code, rr.Body.String()
}

// errorField returns the "error" field of a JSON error envelope.
func errorField(t *testing.T, body string) string {
	t.Helper()
	var resp map[string]string
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal error envelope %q: %v", body, err)
	}
	return resp["error"]
}

// Session names used across the tiers. The worker owns two review rounds; the
// stranger sessions belong to other callers and must stay out of reach.
const (
	ckRepo    = "prism-test-checkin"
	ckAltRepo = "prism-test-checkin-other"

	ckWorker       = "prism-test-checkin@feature"
	ckWorkerReview = "prism-test-checkin@feature~review-2-review-context"
	ckWorkerOldRev = "prism-test-checkin@feature~review-1-review-goal"
	ckCoordinator  = "prism-test-checkin@main"

	ckOtherWorker       = "prism-test-checkin@other-feature"
	ckOtherWorkerReview = "prism-test-checkin@other-feature~review-1-review-code"

	ckAltCoordinator  = "prism-test-checkin-other@main"
	ckAltWorker       = "prism-test-checkin-other@feature"
	ckAltWorkerReview = "prism-test-checkin-other@feature~review-1-review-qa"
)

// ── tier 1: worker ───────────────────────────────────────────────────────────

// TestCheckin_Worker_AllowsOwnReviewAgent is the motivating case of #2587: a
// worker reads the review agent it spawned for its own PR and receives the
// conversation history. The worker already receives that agent's verdict and
// blocking findings through the review-complete prompt; only the non-blocking
// detail was out of reach.
func TestCheckin_Worker_AllowsOwnReviewAgent(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedReviewGroup(ckWorker, ckRepo, 2, ckWorkerReview)

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	code, body := checkin(t, sc, ckWorkerReview)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", code, body)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, body)
	}
	if resp["session"] != ckWorkerReview {
		t.Errorf("session = %v, want %q", resp["session"], ckWorkerReview)
	}
	if _, ok := resp["events"]; !ok {
		t.Errorf("response has no events field — the worker must receive conversation history, not a stub: %q", body)
	}
}

// TestCheckin_Worker_AllowsEarlierRoundReviewAgent covers the edge-case AC: a
// review agent from an EARLIER round of the same session is still in scope.
// Each round registers its own session_groups row, and every one of those rows
// carries the same parent_session, so the scope check admits all of them.
func TestCheckin_Worker_AllowsEarlierRoundReviewAgent(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedReviewGroup(ckWorker, ckRepo, 1, ckWorkerOldRev)
	f.seedReviewGroup(ckWorker, ckRepo, 2, ckWorkerReview)

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	for _, target := range []string{ckWorkerOldRev, ckWorkerReview} {
		if code, body := checkin(t, sc, target); code != http.StatusOK {
			t.Errorf("checkin %q: status = %d, body = %q, want 200", target, code, body)
		}
	}
}

// TestCheckin_Worker_DeniesEverythingElse covers three security ACs at once: a
// worker calling /checkin for a session that is not a review agent of its own
// session gets 403 — its own session, another worker, another worker's review
// agent, a coordinator, and any session in another repo.
func TestCheckin_Worker_DeniesEverythingElse(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedReviewGroup(ckWorker, ckRepo, 2, ckWorkerReview)

	// Sessions the worker must not reach.
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckOtherWorker, ckRepo, "worker")
	f.seedReviewGroup(ckOtherWorker, ckRepo, 1, ckOtherWorkerReview)
	f.seedSession(ckAltCoordinator, ckAltRepo, "coordinator")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")
	f.seedReviewGroup(ckAltWorker, ckAltRepo, 1, ckAltWorkerReview)

	cases := []struct {
		name   string
		target string
	}{
		{"own session", ckWorker},
		{"own coordinator", ckCoordinator},
		{"another worker in the same repo", ckOtherWorker},
		{"another worker's review agent in the same repo", ckOtherWorkerReview},
		{"a coordinator in another repo", ckAltCoordinator},
		{"a worker in another repo", ckAltWorker},
		{"a review agent in another repo", ckAltWorkerReview},
	}

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := checkin(t, sc, tc.target)
			if code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %q, want 403", code, truncateForLog(body, 200))
			}
			if errorField(t, body) == "" {
				t.Error("403 response carries no error field")
			}
		})
	}
}

// TestCheckin_Worker_DeniesSelfWithActionableMessage covers the settled scope
// decision on #2587: `prism checkin <self>` is NOT granted. The refusal must
// say what the grant does cover, because worker.md previously told the worker
// this call worked.
func TestCheckin_Worker_DeniesSelfWithActionableMessage(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	code, body := checkin(t, sc, ckWorker)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", code, body)
	}
	msg := errorField(t, body)
	if !strings.Contains(msg, "own session") {
		t.Errorf("error %q must name the refused case (own session)", msg)
	}
	if !strings.Contains(msg, "review") {
		t.Errorf("error %q must name the scope that IS granted (review agents)", msg)
	}
}

// TestCheckin_Worker_DeletedGroupRowIs403NotPanic covers the edge-case AC: a
// review agent whose session_groups row was deleted yields a clear 403, never
// a 500. The name still looks like a review agent of this worker
// ("<parent>~review-<N>-<agent>"), so a name-heuristic scope check would admit
// it. The DB-backed check refuses it, and says why.
func TestCheckin_Worker_DeletedGroupRowIs403NotPanic(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	groupID := f.seedReviewGroup(ckWorker, ckRepo, 2, ckWorkerReview)

	// Delete the group row. agent_status.group_id is ON DELETE SET NULL, so
	// the member row survives with a NULL group_id — an orphaned review agent.
	if err := f.DB.QueryRow(
		"DELETE FROM session_groups WHERE group_id = ? RETURNING group_id", groupID,
	).Scan(new(string)); err != nil {
		t.Fatalf("delete session_groups row: %v", err)
	}

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	code, body := checkin(t, sc, ckWorkerReview)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403 (never 500)", code, truncateForLog(body, 200))
	}
	if msg := errorField(t, body); !strings.Contains(msg, "review-group row") {
		t.Errorf("error %q must explain that the review-group row is missing", msg)
	}
}

// TestCheckin_UndeterminableRole_TakesWorkerTier covers the fail-closed
// direction of the role split. A caller whose role cannot be determined — no
// agent_status row, a pre-migration row with NULL root_agent_name, or no DB
// handle at all — must land on tier 1, the narrowest tier, not on tier 2.
func TestCheckin_UndeterminableRole_TakesWorkerTier(t *testing.T) {
	t.Run("no status row", func(t *testing.T) {
		f := newCheckinFixture(t)
		f.seedSession(ckOtherWorker, ckRepo, "worker")

		sc := f.sidecarFor("prism-test-checkin@no-row", ckRepo, "", nil)
		if code, body := checkin(t, sc, ckOtherWorker); code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %q, want 403", code, truncateForLog(body, 200))
		}
	})

	t.Run("null root_agent_name", func(t *testing.T) {
		f := newCheckinFixture(t)
		const sess = "prism-test-checkin@null-root"
		f.seedSession(sess, ckRepo, "")
		f.seedSession(ckOtherWorker, ckRepo, "worker")

		sc := f.sidecarFor(sess, ckRepo, "", nil)
		if code, body := checkin(t, sc, ckOtherWorker); code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %q, want 403", code, truncateForLog(body, 200))
		}
	})

	t.Run("no db handle", func(t *testing.T) {
		sc := New(Config{
			SessionName: "prism-test-checkin@no-db",
			Repo:        ckRepo,
			Worktree:    t.TempDir(),
			HarnessURL:  "http://localhost:14000",
			Clock:       newTestClock(),
			Harness:     newSSEHarness(),
			Logger:      log.New(os.Stderr, "", 0),
		})
		if code, body := checkin(t, sc, ckOtherWorker); code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %q, want 403", code, truncateForLog(body, 200))
		}
	})
}

// ── tier 2: coordinator ──────────────────────────────────────────────────────

// TestCheckin_Coordinator_Tier2Unchanged pins the AC "coordinator access is
// unchanged for own-repo sessions and for cross-repo <repo>@main targets".
// The privileged list is empty throughout, which is the configuration every
// non-privileged coordinator runs under.
func TestCheckin_Coordinator_Tier2Unchanged(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedReviewGroup(ckWorker, ckRepo, 1, ckWorkerReview)
	f.seedSession(ckAltCoordinator, ckAltRepo, "coordinator")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", nil)

	allowed := []struct {
		name   string
		target string
	}{
		{"own session", ckCoordinator},
		{"own-repo worker", ckWorker},
		{"own-repo review agent", ckWorkerReview},
		{"cross-repo coordinator", ckAltCoordinator},
	}
	for _, tc := range allowed {
		t.Run("allows "+tc.name, func(t *testing.T) {
			if code, body := checkin(t, sc, tc.target); code != http.StatusOK {
				t.Fatalf("status = %d, body = %q, want 200", code, truncateForLog(body, 200))
			}
		})
	}

	t.Run("denies cross-repo worker", func(t *testing.T) {
		code, body := checkin(t, sc, ckAltWorker)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %q, want 403", code, truncateForLog(body, 200))
		}
		if msg := errorField(t, body); !strings.Contains(msg, "cross-repo") {
			t.Errorf("error %q should name the cross-repo rule", msg)
		}
	})
}

// TestCheckin_EmptyPrivilegedList_MatchesTodaysBehaviour covers the AC "an
// empty or missing privileged-repo list grants the privilege to nobody, and
// behaviour matches today's". Both spellings of "no list" are exercised
// against the one target that the privilege would otherwise unlock.
func TestCheckin_EmptyPrivilegedList_MatchesTodaysBehaviour(t *testing.T) {
	cases := []struct {
		name  string
		repos []string
	}{
		{"nil list (file absent)", nil},
		{"empty list (file present, list empty)", []string{}},
		{"blank entry only", []string{""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckinFixture(t)
			f.seedSession(ckCoordinator, ckRepo, "coordinator")
			f.seedSession(ckAltWorker, ckAltRepo, "worker")

			sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", tc.repos)
			if code, body := checkin(t, sc, ckAltWorker); code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %q, want 403", code, truncateForLog(body, 200))
			}
			if got := auditEventsFor(t, f.DB, ckCoordinator); len(got) != 0 {
				t.Errorf("audit events = %d, want 0 — a refused request must not be recorded as a grant", len(got))
			}
		})
	}
}

// ── tier 3: privileged coordinator ───────────────────────────────────────────

// TestCheckin_PrivilegedCoordinator_ReachesAnySession covers the AC "a
// coordinator of a repo named in the privileged list can check in on any
// session in any repo, including another coordinator's workers and review
// agents".
func TestCheckin_PrivilegedCoordinator_ReachesAnySession(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckAltCoordinator, ckAltRepo, "coordinator")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")
	f.seedReviewGroup(ckAltWorker, ckAltRepo, 1, ckAltWorkerReview)

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", []string{ckRepo})

	for _, target := range []string{ckAltCoordinator, ckAltWorker, ckAltWorkerReview} {
		if code, body := checkin(t, sc, target); code != http.StatusOK {
			t.Errorf("checkin %q: status = %d, body = %q, want 200", target, code, truncateForLog(body, 200))
		}
	}
}

// TestCheckin_PrivilegedCoordinator_EmitsAuditEvent covers the AC "each access
// granted by tier 3 emits an audit event recording the caller, the target, and
// the time". The row must also be reachable through the `prism audit` query
// path, which is the surface an operator uses.
func TestCheckin_PrivilegedCoordinator_EmitsAuditEvent(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", []string{ckRepo})
	if code, body := checkin(t, sc, ckAltWorker); code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", code, truncateForLog(body, 200))
	}

	events := auditEventsFor(t, f.DB, ckCoordinator)
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1", len(events))
	}
	e := events[0]
	if e.SessionName != ckCoordinator {
		t.Errorf("event.SessionName = %q, want the caller %q", e.SessionName, ckCoordinator)
	}
	if e.CreatedAt.IsZero() {
		t.Error("event.CreatedAt is zero — the audit row must record the time")
	}

	var p payload.Audit
	if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
		t.Fatalf("unmarshal audit payload %q: %v", e.Payload, err)
	}
	if p.SessionName != ckCoordinator {
		t.Errorf("payload.SessionName = %q, want the caller %q", p.SessionName, ckCoordinator)
	}
	if p.Target != ckAltWorker {
		t.Errorf("payload.Target = %q, want the target %q", p.Target, ckAltWorker)
	}
	if p.Grant != checkinPrivilegeGrantName {
		t.Errorf("payload.Grant = %q, want %q", p.Grant, checkinPrivilegeGrantName)
	}
	if !strings.Contains(p.Command, ckAltWorker) {
		t.Errorf("payload.Command = %q, want it to name the target", p.Command)
	}
}

// TestCheckin_PrivilegedCoordinator_NoAuditOnTier2Access keeps the audit log
// readable: the privilege is consulted only where tier 2 refuses, so an
// own-repo read by a privileged coordinator writes no row. Without this, every
// routine checkin by the privileged coordinator would be recorded as a
// privileged access and the genuinely privileged reads would be lost in the
// noise.
func TestCheckin_PrivilegedCoordinator_NoAuditOnTier2Access(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedSession(ckAltCoordinator, ckAltRepo, "coordinator")

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", []string{ckRepo})

	// Both of these are tier-2 grants: an own-repo worker and a cross-repo
	// coordinator. Neither needs the privilege.
	for _, target := range []string{ckWorker, ckAltCoordinator} {
		if code, body := checkin(t, sc, target); code != http.StatusOK {
			t.Fatalf("checkin %q: status = %d, body = %q, want 200", target, code, truncateForLog(body, 200))
		}
	}
	if got := auditEventsFor(t, f.DB, ckCoordinator); len(got) != 0 {
		t.Errorf("audit events = %d, want 0 — tier-2 access must not be recorded as a privileged grant", len(got))
	}
}

// TestCheckin_PrivilegedRepo_RequiresDBBackedRootAgentName covers the AC "the
// privilege check uses the DB-backed root_agent_name where the row exists,
// rather than the @main name heuristic alone".
//
// isCoordinatorSession returns dbBased || nameBased and lets the "@main"
// suffix win on disagreement, so each caller below is admitted there as a
// coordinator and reaches tier 2. The tier-3 check must still refuse them:
// none carries a DB row that says root_agent_name = 'coordinator'.
func TestCheckin_PrivilegedRepo_RequiresDBBackedRootAgentName(t *testing.T) {
	cases := []struct {
		name string
		// seed writes the caller's agent_status row, or writes nothing.
		seed func(f *checkinFixture)
	}{
		{
			name: "no agent_status row for the caller",
			seed: func(f *checkinFixture) {},
		},
		{
			name: "pre-migration row with NULL root_agent_name",
			seed: func(f *checkinFixture) { f.seedSession(ckCoordinator, ckRepo, "") },
		},
		{
			name: "row says worker on an @main session",
			seed: func(f *checkinFixture) { f.seedSession(ckCoordinator, ckRepo, "worker") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckinFixture(t)
			tc.seed(f)
			f.seedSession(ckAltWorker, ckAltRepo, "worker")

			sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", []string{ckRepo})

			// Tier 2 still admits the caller for an own-repo target — the
			// name heuristic carries it that far, exactly as before #2587.
			if code, body := checkin(t, sc, ckCoordinator); code != http.StatusOK {
				t.Fatalf("own-session checkin: status = %d, body = %q, want 200", code, truncateForLog(body, 200))
			}

			// Tier 3 must refuse: the privilege needs DB evidence.
			if code, body := checkin(t, sc, ckAltWorker); code != http.StatusForbidden {
				t.Fatalf("cross-repo worker checkin: status = %d, body = %q, want 403", code, truncateForLog(body, 200))
			}
			if got := auditEventsFor(t, f.DB, ckCoordinator); len(got) != 0 {
				t.Errorf("audit events = %d, want 0", len(got))
			}
		})
	}
}

// TestCheckin_PrivilegedRepo_DoesNotGrantWorkers pins the tier boundary: the
// privilege attaches to the COORDINATOR of a named repo, not to every session
// in it. A worker in the privileged repo keeps the tier-1 scope.
func TestCheckin_PrivilegedRepo_DoesNotGrantWorkers(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")
	f.seedSession(ckOtherWorker, ckRepo, "worker")

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", []string{ckRepo})

	for _, target := range []string{ckAltWorker, ckOtherWorker} {
		if code, body := checkin(t, sc, target); code != http.StatusForbidden {
			t.Errorf("checkin %q: status = %d, body = %q, want 403", target, code, truncateForLog(body, 200))
		}
	}
}

// TestCheckin_PrivilegedRepo_UnknownRepoChangesNothing covers the edge-case AC:
// a privileged-repo entry naming a repo that has no running coordinator changes
// no behaviour and causes no error.
func TestCheckin_PrivilegedRepo_UnknownRepoChangesNothing(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator",
		[]string{"prism-test-checkin-repo-that-does-not-exist"})

	// Own-repo access is unaffected.
	if code, body := checkin(t, sc, ckWorker); code != http.StatusOK {
		t.Fatalf("own-repo checkin: status = %d, body = %q, want 200", code, truncateForLog(body, 200))
	}
	// Tier 2 still refuses the cross-repo worker: this coordinator's repo is
	// not the one named in the list.
	if code, body := checkin(t, sc, ckAltWorker); code != http.StatusForbidden {
		t.Fatalf("cross-repo checkin: status = %d, body = %q, want 403", code, truncateForLog(body, 200))
	}
}

// TestCheckin_PrivilegedRepo_UnknownTargetStill404 keeps the #2112 behaviour
// for the privileged coordinator: an unresolvable target name is a 404 "not
// found", not an empty 200. The privilege widens WHO may be read, not whether
// a missing session is reported.
func TestCheckin_PrivilegedRepo_UnknownTargetStill404(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", []string{ckRepo})
	code, body := checkin(t, sc, "ghost-session-with-no-at-sign")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", code, truncateForLog(body, 200))
	}
}

// ── the rest of the surface must not move ────────────────────────────────────

// TestCheckin_PrivilegeIsCheckinOnly covers the AC "the privilege grants
// /checkin only, and no other endpoint changes behaviour". A worker in the
// privileged repo is still refused on every coordinator-only endpoint, and the
// privileged coordinator's /db/query — the endpoint that exposes a strict
// superset of /checkin's data — is not widened to other repos by the grant.
func TestCheckin_PrivilegeIsCheckinOnly(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")

	// A worker in the privileged repo stays a worker everywhere else.
	sc := f.sidecarFor(ckWorker, ckRepo, "worker", []string{ckRepo})
	coordinatorOnly := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/spawn", `{"branch":"feature-x","prompt":"do the thing"}`},
		{http.MethodPost, "/merge", `{"pr":1}`},
		{http.MethodGet, "/merges", ""},
		{http.MethodPost, "/investigate", `{"prompt":"look into this"}`},
		{http.MethodGet, "/logs?session=" + ckWorkerReview, ""},
		{http.MethodGet, "/db/query?sql=SELECT+1", ""},
		{http.MethodGet, "/db/schema", ""},
		{http.MethodGet, "/db/tables", ""},
	}
	for _, tc := range coordinatorOnly {
		t.Run("worker "+tc.method+" "+tc.path, func(t *testing.T) {
			rr := doHostAPI(t, sc, tc.method, tc.path, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %q, want 403", rr.Code, truncateForLog(rr.Body.String(), 200))
			}
		})
	}
}

// TestCheckin_MissingSessionParamIsBadRequest pins the one ordering change the
// gate introduced: the target is required before the tier check, because every
// tier scopes on the target. A request with no target is a 400 for every role,
// and nothing is read on that path.
func TestCheckin_MissingSessionParamIsBadRequest(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedSession(ckCoordinator, ckRepo, "coordinator")

	for _, caller := range []struct {
		name    string
		session string
		role    string
	}{
		{"worker", ckWorker, "worker"},
		{"coordinator", ckCoordinator, "coordinator"},
	} {
		t.Run(caller.name, func(t *testing.T) {
			sc := f.sidecarFor(caller.session, ckRepo, caller.role, nil)
			rr := doHostAPI(t, sc, http.MethodGet, "/checkin", "")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// auditEventsFor returns the audit rows recorded against sessionName.
func auditEventsFor(t *testing.T, d *db.DB, sessionName string) []db.Event {
	t.Helper()
	events, err := d.QueryAuditEvents(sessionName, 0, "", 0)
	if err != nil {
		t.Fatalf("QueryAuditEvents(%q): %v", sessionName, err)
	}
	return events
}
