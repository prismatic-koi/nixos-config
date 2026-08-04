package cmd

// checkin_permission_test.go — issue #2619.
//
// The DIRECT CLI route of `prism checkin`, which a `host`-mode session takes
// because it has no host-API socket. Before #2619 that route had no role
// check, no repo scope, and wrote no audit event; the three tiers of #2587
// gated the host-API route alone.
//
// These tests pin the direct route against the same tier table the host-API
// tests in internal/sidecar/checkin_permission_test.go pin the proxy route
// against. Keep the two in step: a tier that behaves differently depending on
// the caller's isolation mode is the defect this file exists to prevent.
//
// # Isolation contract
//
// Every test redirects both XDG roots. The DB goes to a t.TempDir() through
// SetTestDBPath, so no test touches the developer's real prism.db.
// XDG_CONFIG_HOME is redirected too, which matters more than it looks: the
// tier-3 repo list is read from $XDG_CONFIG_HOME/prism/
// checkin-privileged-repos.json, and the developer's own file names a real
// privileged repo. Without the redirect the tier-3 tests would pass or fail
// according to host configuration.
//
// Session names carry the "prism-test-checkin-cli" prefix so a leaked write
// cannot collide with a live session.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

// Session names used across the tiers. The worker owns one review round; the
// stranger sessions belong to other callers and must stay out of reach.
const (
	cliRepo    = "prism-test-checkin-cli"
	cliAltRepo = "prism-test-checkin-cli-other"

	cliWorker       = "prism-test-checkin-cli@feature"
	cliWorkerReview = "prism-test-checkin-cli@feature~review-1-review-goal"
	cliCoordinator  = "prism-test-checkin-cli@main"
	cliOtherWorker  = "prism-test-checkin-cli@other-feature"

	cliAltCoordinator = "prism-test-checkin-cli-other@main"
	cliAltWorker      = "prism-test-checkin-cli-other@feature"
)

// ── fixture ──────────────────────────────────────────────────────────────────

// directCheckinFixture is one isolated prism.db, wired so openDB() inside the
// gate resolves to it, plus an isolated XDG_CONFIG_HOME for the tier-3 list.
type directCheckinFixture struct {
	t         *testing.T
	DB        *db.DB
	configDir string
}

func newDirectCheckinFixture(t *testing.T) *directCheckinFixture {
	t.Helper()

	// The gate must never take the proxy branch in these tests.
	t.Setenv("PRISM_HOST_API", "")

	dbPath := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	// Redirect the tier-3 privilege list away from the developer's own
	// ~/.config/prism/. An absent file means "no repo is privileged", which is
	// the default the tier-1 and tier-2 tests need.
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	return &directCheckinFixture{t: t, DB: d, configDir: configDir}
}

// seedSession writes an agent_status row with an explicit root_agent_name.
func (f *directCheckinFixture) seedSession(sessionName, repo, rootAgentName string) {
	f.t.Helper()
	if err := f.DB.UpsertStatusSeedRootAgentName(
		sessionName, repo, "/tmp/"+repo, "active", nil, nil, rootAgentName, "", "",
	); err != nil {
		f.t.Fatalf("seed %q (root_agent_name=%q): %v", sessionName, rootAgentName, err)
	}
}

// seedReviewGroup registers a review group owned by parent and attaches each
// named member to it, exactly as `prism review` does at spawn time.
func (f *directCheckinFixture) seedReviewGroup(parent, repo string, round int, members ...string) {
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
}

// privilege writes checkin-privileged-repos.json naming the given repos, the
// same shape the prism NixOS module renders.
func (f *directCheckinFixture) privilege(repos ...string) {
	f.t.Helper()
	dir := filepath.Join(f.configDir, "prism")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", dir, err)
	}
	body, err := json.Marshal(map[string][]string{"privileged_repos": repos})
	if err != nil {
		f.t.Fatalf("marshal privileged repos: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkin-privileged-repos.json"), body, 0o644); err != nil {
		f.t.Fatalf("write privileged repos file: %v", err)
	}
}

// auditEvents returns the audit events recorded against a session.
func (f *directCheckinFixture) auditEvents(session string) []payload.Audit {
	f.t.Helper()
	events, err := f.DB.QueryEvents(session, 0, nil, nil, []string{"audit"})
	if err != nil {
		f.t.Fatalf("QueryEvents(audit) for %q: %v", session, err)
	}
	out := make([]payload.Audit, 0, len(events))
	for _, e := range events {
		var a payload.Audit
		if err := json.Unmarshal([]byte(e.Payload), &a); err != nil {
			f.t.Fatalf("unmarshal audit payload %q: %v", e.Payload, err)
		}
		out = append(out, a)
	}
	return out
}

// ── helper for the rendering tests ───────────────────────────────────────────

// grantCheckinCallerIdentity gives the calling test a caller identity that the
// #2619 gate admits for targetSession: the coordinator of the target's own
// repo, which tier 2 allows.
//
// Tests that exercise checkin RENDERING rather than checkin PERMISSION need
// this. runCheckinSession now gates the direct route, so a test that calls it
// with no caller identity is refused before any rendering happens. That
// refusal is the gate working, not a rendering regression — the fix is to give
// the test a caller, not to weaken the gate.
//
// It also redirects XDG_CONFIG_HOME, so the tier-3 repo list can never come
// from the developer's own ~/.config/prism/.
func grantCheckinCallerIdentity(t *testing.T, targetSession string) {
	t.Helper()
	repo, _, found := strings.Cut(targetSession, "@")
	if !found || repo == "" {
		t.Fatalf("grantCheckinCallerIdentity: %q has no <repo>@<branch> shape, so no coordinator name can be derived", targetSession)
	}
	t.Setenv("PRISM_SESSION_NAME", repo+"@main")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// ── tier 1: worker ───────────────────────────────────────────────────────────

// TestDirectCheckin_Worker_AllowsOwnReviewAgent is the motivating case of
// #2587, now on the route a host-mode worker actually takes: the worker reads
// the review agent it spawned for its own PR.
func TestDirectCheckin_Worker_AllowsOwnReviewAgent(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)

	if err := authorizeDirectCheckinFor(cliWorker, cliWorkerReview); err != nil {
		t.Fatalf("worker checking in on its own review agent was refused: %v", err)
	}
}

// TestDirectCheckin_Worker_DeniesEverythingElse is the headline defect of
// #2619: before the fix, every one of these reads succeeded on this route.
func TestDirectCheckin_Worker_DeniesEverythingElse(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)

	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliOtherWorker, cliRepo, "worker")
	f.seedSession(cliAltCoordinator, cliAltRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")

	cases := []struct {
		name   string
		target string
	}{
		{"its own session", cliWorker},
		{"another worker in the same repo", cliOtherWorker},
		{"its own coordinator", cliCoordinator},
		{"a coordinator in another repo", cliAltCoordinator},
		{"a worker in another repo", cliAltWorker},
		{"a session that does not exist", "prism-test-checkin-cli@nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := authorizeDirectCheckinFor(cliWorker, tc.target); err == nil {
				t.Errorf("worker reading %s (%q) was permitted — want a refusal", tc.name, tc.target)
			}
		})
	}
}

// TestDirectCheckin_Worker_DeniesReviewAgentOfAnotherSession pins the DB-backed
// half of the tier-1 scope: membership resolves through
// session_groups.parent_session, not through the "<parent>~review-N-<agent>"
// name shape.
func TestDirectCheckin_Worker_DeniesReviewAgentOfAnotherSession(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedSession(cliOtherWorker, cliRepo, "worker")
	const strangersReviewAgent = "prism-test-checkin-cli@other-feature~review-1-review-code"
	f.seedReviewGroup(cliOtherWorker, cliRepo, 1, strangersReviewAgent)

	if err := authorizeDirectCheckinFor(cliWorker, strangersReviewAgent); err == nil {
		t.Fatal("worker read another session's review agent — want a refusal")
	}
}

// ── tier 2: coordinator ──────────────────────────────────────────────────────

// TestDirectCheckin_Coordinator_ReachesOwnRepoAndCrossRepoCoordinators covers
// the unprivileged coordinator scope on this route: every session in its own
// repo, plus a cross-repo target that is itself a coordinator — and nothing
// else.
func TestDirectCheckin_Coordinator_ReachesOwnRepoAndCrossRepoCoordinators(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)
	f.seedSession(cliAltCoordinator, cliAltRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")

	allowed := []struct {
		name   string
		target string
	}{
		{"a worker in its own repo", cliWorker},
		{"a review agent in its own repo", cliWorkerReview},
		{"a coordinator in another repo", cliAltCoordinator},
	}
	for _, tc := range allowed {
		t.Run("allows "+tc.name, func(t *testing.T) {
			if err := authorizeDirectCheckinFor(cliCoordinator, tc.target); err != nil {
				t.Errorf("coordinator reading %s (%q) was refused: %v", tc.name, tc.target, err)
			}
		})
	}

	t.Run("denies a worker in another repo", func(t *testing.T) {
		if err := authorizeDirectCheckinFor(cliCoordinator, cliAltWorker); err == nil {
			t.Error("unprivileged coordinator read a worker in another repo — want a refusal")
		}
	})
}

// TestDirectCheckin_Coordinator_WritesNoAuditEvent pins the audit boundary from
// the other side: tier 2 is ordinary access and must not fill the trail.
func TestDirectCheckin_Coordinator_WritesNoAuditEvent(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliWorker, cliRepo, "worker")

	if err := authorizeDirectCheckinFor(cliCoordinator, cliWorker); err != nil {
		t.Fatalf("coordinator reading its own repo was refused: %v", err)
	}
	if got := f.auditEvents(cliCoordinator); len(got) != 0 {
		t.Errorf("tier-2 access wrote %d audit event(s), want 0: %+v", len(got), got)
	}
}

// ── tier 3: privileged coordinator ───────────────────────────────────────────

// TestDirectCheckin_PrivilegedCoordinator_ReachesAnySession covers the tier-3
// widening on this route.
func TestDirectCheckin_PrivilegedCoordinator_ReachesAnySession(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.privilege(cliRepo)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")
	const altReviewAgent = "prism-test-checkin-cli-other@feature~review-1-review-qa"
	f.seedReviewGroup(cliAltWorker, cliAltRepo, 1, altReviewAgent)

	for _, target := range []string{cliAltWorker, altReviewAgent} {
		if err := authorizeDirectCheckinFor(cliCoordinator, target); err != nil {
			t.Errorf("privileged coordinator reading %q was refused: %v", target, err)
		}
	}
}

// TestDirectCheckin_PrivilegedCoordinator_WritesAuditEvent is the audit-
// completeness AC of #2619: a tier-3 read through the CLI route leaves the same
// record as one through the host-API route, carrying the shared grant label.
func TestDirectCheckin_PrivilegedCoordinator_WritesAuditEvent(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.privilege(cliRepo)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")

	if err := authorizeDirectCheckinFor(cliCoordinator, cliAltWorker); err != nil {
		t.Fatalf("privileged coordinator was refused: %v", err)
	}

	events := f.auditEvents(cliCoordinator)
	if len(events) != 1 {
		t.Fatalf("tier-3 access wrote %d audit event(s), want exactly 1: %+v", len(events), events)
	}
	a := events[0]
	if a.Grant != authz.CheckinPrivilegeGrantName {
		t.Errorf("audit Grant = %q, want %q — the grant label must match the host-API route", a.Grant, authz.CheckinPrivilegeGrantName)
	}
	if a.SessionName != cliCoordinator {
		t.Errorf("audit SessionName = %q, want the caller %q", a.SessionName, cliCoordinator)
	}
	if a.Target != cliAltWorker {
		t.Errorf("audit Target = %q, want the session that was read %q", a.Target, cliAltWorker)
	}
	if a.Command != "prism checkin "+cliAltWorker {
		t.Errorf("audit Command = %q, want %q", a.Command, "prism checkin "+cliAltWorker)
	}
	if a.Tool != directCheckinAuditTool {
		t.Errorf("audit Tool = %q, want %q — the trail must record which route the read came through", a.Tool, directCheckinAuditTool)
	}
}

// TestDirectCheckin_Privilege_RequiresDBBackedCoordinatorRole pins the
// condition #2587 recorded as the answer to its own caution: the tier-3 grant
// does not rest on the "@main" name suffix.
func TestDirectCheckin_Privilege_RequiresDBBackedCoordinatorRole(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.privilege(cliRepo)
	// root_agent_name = "worker" on an @main-shaped name. isCoordinatorSession
	// admits it as a coordinator on the name, so it reaches tier 2 — but the
	// tier-3 check reads root_agent_name directly and must refuse.
	f.seedSession(cliCoordinator, cliRepo, "worker")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")

	if err := authorizeDirectCheckinFor(cliCoordinator, cliAltWorker); err == nil {
		t.Fatal("tier-3 privilege was granted on the @main name alone — want a refusal")
	}
	if got := f.auditEvents(cliCoordinator); len(got) != 0 {
		t.Errorf("a refused access wrote %d audit event(s), want 0: %+v", len(got), got)
	}
}

// ── caller identity ──────────────────────────────────────────────────────────

// TestDirectCheckin_UnresolvableCaller_FailsClosed pins the decision recorded
// on #2619: a caller whose session cannot be determined is refused, and the
// error names PRISM_SESSION_NAME, matching requireMergesCoordinator. Bare-shell
// `prism checkin` from a plain terminal outside tmux is refused. No carve-out.
func TestDirectCheckin_UnresolvableCaller_FailsClosed(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")

	err := authorizeDirectCheckinFor("", cliWorker)
	if err == nil {
		t.Fatal("checkin with an unresolvable caller was permitted — want a refusal")
	}
	if !strings.Contains(err.Error(), "PRISM_SESSION_NAME") {
		t.Errorf("error does not name PRISM_SESSION_NAME, so the caller is not told the remedy: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot determine caller session") {
		t.Errorf("error text drifted from requireMergesCoordinator's: %v", err)
	}
}

// TestDirectCheckin_ResolvesCallerFromEnvironment covers the wrapper that the
// production path calls: the caller comes from PRISM_SESSION_NAME, the same
// resolution `prism merges` uses.
func TestDirectCheckin_ResolvesCallerFromEnvironment(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedSession(cliOtherWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)

	t.Setenv("PRISM_SESSION_NAME", cliWorker)

	if err := authorizeDirectCheckin(cliWorkerReview); err != nil {
		t.Errorf("worker reading its own review agent was refused: %v", err)
	}
	if err := authorizeDirectCheckin(cliOtherWorker); err == nil {
		t.Error("worker reading another worker was permitted — want a refusal")
	}
}

// ── the ~review aggregate, verbose mode ────────────────────────────────────────
//
// `prism checkin <parent>~review --verbose` renders each group member through
// runCheckinSession (cmd/checkin_review.go), so the gate applies once per
// member. The non-verbose summary branch reads d.QueryEvents inline and never
// reaches the gate.
//
// Round 1 of the review of PR #2625 caught this: the original scope claim said
// the aggregate was ungated, and no test covered verbose=true. These tests
// exist so the claim in prism/docs/invariants/session-lifecycle.md is
// falsifiable rather than asserted.

// TestCheckinReviewAggregate_VerboseIsGatedPerMember covers both directions of
// the per-member gate, including the degradation contract: a refused member
// does not abort the command, it prints an error line and the loop continues.
func TestCheckinReviewAggregate_VerboseIsGatedPerMember(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedSession(cliOtherWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)
	// The member needs an assistant turn, or the allowed path falls through to
	// the tmux screen-scrape and prints the same error line as a refusal.
	writeEvent(t, f.DB, "agg-evt-1", cliWorkerReview, "msg_assistant",
		`{"messageId":"agg-m1","text":"review agent verdict text"}`, time.Now().Add(-time.Minute))

	t.Run("the owning worker is admitted for each member", func(t *testing.T) {
		t.Setenv("PRISM_SESSION_NAME", cliWorker)
		stdout, stderr := captureStdoutStderr(t, func() {
			if err := runCheckinReviewRoundsByGroup(cliWorker, true); err != nil {
				t.Errorf("runCheckinReviewRoundsByGroup: %v", err)
			}
		})
		if !strings.Contains(stdout, "review agent verdict text") {
			t.Errorf("the owning worker did not get its own review agent's conversation:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
		if strings.Contains(stderr, "error reading session") {
			t.Errorf("a member the worker owns was refused:\n%s", stderr)
		}
	})

	t.Run("a caller outside its tier is refused for each member", func(t *testing.T) {
		t.Setenv("PRISM_SESSION_NAME", cliOtherWorker)
		var returned error
		stdout, stderr := captureStdoutStderr(t, func() {
			returned = runCheckinReviewRoundsByGroup(cliWorker, true)
		})
		if strings.Contains(stdout, "review agent verdict text") {
			t.Errorf("a worker read another session's review agent through the aggregate:\n%s", stdout)
		}
		if !strings.Contains(stderr, "error reading session") {
			t.Errorf("the refusal was not reported on stderr:\n%s", stderr)
		}
		// The degradation contract: one refused member must not abort the run.
		if returned != nil {
			t.Errorf("a refused member aborted the command with %v; it must print the error line and continue", returned)
		}
	})
}

// TestCheckinReviewAggregate_VerboseAuditsEachMember pins the audit
// consequence stated in the invariant: a tier-3 read through the aggregate
// writes one event per member, not one per command, because the audit write
// lives inside runCheckinSession.
func TestCheckinReviewAggregate_VerboseAuditsEachMember(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.privilege(cliRepo)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")

	// Two members in another repo, so every read is a tier-3 cross-repo grant.
	const (
		altReviewA = "prism-test-checkin-cli-other@feature~review-1-review-code"
		altReviewB = "prism-test-checkin-cli-other@feature~review-1-review-qa"
	)
	f.seedReviewGroup(cliAltWorker, cliAltRepo, 1, altReviewA, altReviewB)
	for i, m := range []string{altReviewA, altReviewB} {
		writeEvent(t, f.DB, fmt.Sprintf("agg-audit-evt-%d", i), m, "msg_assistant",
			`{"messageId":"agg-audit-m","text":"member output"}`, time.Now().Add(-time.Minute))
	}

	t.Setenv("PRISM_SESSION_NAME", cliCoordinator)
	captureStdoutStderr(t, func() {
		if err := runCheckinReviewRoundsByGroup(cliAltWorker, true); err != nil {
			t.Errorf("runCheckinReviewRoundsByGroup: %v", err)
		}
	})

	events := f.auditEvents(cliCoordinator)
	if len(events) != 2 {
		t.Fatalf("tier-3 aggregate read wrote %d audit event(s), want one per member (2): %+v", len(events), events)
	}
	got := map[string]bool{}
	for _, a := range events {
		if a.Grant != authz.CheckinPrivilegeGrantName {
			t.Errorf("audit Grant = %q, want %q", a.Grant, authz.CheckinPrivilegeGrantName)
		}
		got[a.Target] = true
	}
	for _, want := range []string{altReviewA, altReviewB} {
		if !got[want] {
			t.Errorf("no audit event names member %q as its target: %+v", want, events)
		}
	}
}

// ── route wiring ─────────────────────────────────────────────────────────────

// TestRunCheckinSession_DirectRouteIsGated proves the gate is wired into the
// command path, not merely present as a function. A predicate nothing calls is
// exactly the state #2619 found this route in.
func TestRunCheckinSession_DirectRouteIsGated(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedSession(cliOtherWorker, cliRepo, "worker")
	writeEvent(t, f.DB, "cli-gate-evt-1", cliOtherWorker, "msg_assistant",
		`{"messageId":"m1","text":"secret"}`, time.Now().Add(-time.Minute))

	t.Setenv("PRISM_SESSION_NAME", cliWorker)

	out := captureStdout(t, func() {
		err := runCheckinSession(cliOtherWorker, 10, nil, nil, nil, false, false)
		if err == nil {
			t.Error("runCheckinSession returned nil for a refused target — the direct route is ungated")
		}
	})
	if strings.Contains(out, "secret") {
		t.Errorf("refused checkin still rendered the target's conversation:\n%s", out)
	}
}
