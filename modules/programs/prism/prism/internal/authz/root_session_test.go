package authz

// root_session_test.go
//
// A non-worktree session such as `obsidian` has a bare name: no "@", so no
// branch, so no "@main" suffix to test. Without IsRootSession it is
// unreachable by `prism prompt` and invisible in `prism sessions list`, and a
// single wrong root_agent_name value in the DB makes that state permanent.
//
// This file pins the predicate and, more importantly, its limits. IsRootSession
// is a NARROWER grant than IsCoordinatorSession: it admits a bare name for
// prompt routing and for list visibility, and it grants nothing else. The
// tests below are written so that widening it — for example by returning
// IsCoordinatorSession(name) || !HasBranch(name) with no descendant guard —
// fails.
//
// Fixture naming follows a strict discipline: every name is prefixed
// `prism-test`, so no fixture can collide with a live session on a
// developer's host.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sessionname"
)

// openRootSessionTestDB returns an isolated DB in a temp dir.
func openRootSessionTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// rootFixture is one row of the shared fixture set. The same set drives the
// predicate table below and the Go/SQL parity test at the bottom of the file.
type rootFixture struct {
	session string
	repo    string
	// rootAgent is written to root_agent_name. "" leaves the column NULL,
	// which is the pre-migration shape.
	rootAgent string
	// wantRoot is the expected IsRootSession verdict.
	wantRoot bool
	// wantCoordinator is the expected IsCoordinatorSession verdict.
	wantCoordinator bool
	// why documents the arm of the rule the row exercises.
	why string
}

// rootFixtures covers every name shape named in the acceptance
// criteria: a bare name, <repo>@main, <repo>@branch, <bare>~investigate-<slug>
// and <repo>@branch~review-<n>-<role>, plus the meta-sessions and the
// corrupted-DB shapes that motivated the issue.
func rootFixtures() []rootFixture {
	return []rootFixture{
		// ── The reported defect ────────────────────────────────────────────
		{
			session: "prism-test-bare", repo: "prism-test-bare",
			// The `obsidian` row carried root_agent_name='review-goal'. The
			// value is wrong, and without this predicate nothing could override it,
			// because a bare name cannot satisfy the "@main" heuristic.
			rootAgent: "review-goal",
			wantRoot:  true, wantCoordinator: false,
			why: "bare name with a corrupted root_agent_name is still a root session",
		},
		{
			session: "prism-test-bare2", repo: "prism-test-bare2",
			rootAgent: "coordinator",
			wantRoot:  true, wantCoordinator: true,
			why: "bare name that IS a DB-backed coordinator",
		},

		// ── Descendants are never promoted ─────────────────────────────────
		{
			session: "prism-test-bare~investigate-v2", repo: "prism-test-bare",
			rootAgent: "worker",
			wantRoot:  false, wantCoordinator: false,
			why: "investigator of a bare-name parent",
		},
		{
			session: "prism-test-bare~investigate-evil", repo: "prism-test-bare",
			// The nastiest shape: a descendant whose root_agent_name has been
			// corrupted to 'coordinator'. The name guard must win.
			rootAgent: "coordinator",
			wantRoot:  false, wantCoordinator: false,
			why: "descendant with root_agent_name='coordinator' is still refused",
		},
		{
			session: "prism-test-wt@feature~review-1-review-goal", repo: "prism-test-wt",
			rootAgent: "review-goal",
			wantRoot:  false, wantCoordinator: false,
			why: "review agent of a worktree branch",
		},

		// ── Worktree sessions ──────────────────────────────────────
		{
			session: "prism-test-wt@main", repo: "prism-test-wt",
			rootAgent: "coordinator",
			wantRoot:  true, wantCoordinator: true,
			why: "<repo>@main coordinator",
		},
		{
			session: "prism-test-wt@feature", repo: "prism-test-wt",
			rootAgent: "worker",
			wantRoot:  false, wantCoordinator: false,
			why: "<repo>@branch worker",
		},
		{
			session: "prism-test-corrupt@main", repo: "prism-test-corrupt",
			// The @main heuristic is the defence against a wrong DB value and
			// wins on disagreement — the rule IsCoordinatorSession documents.
			rootAgent: "worker",
			wantRoot:  true, wantCoordinator: true,
			why: "<repo>@main with a corrupted root_agent_name — the heuristic wins",
		},
		{
			session: "prism-test-null@main", repo: "prism-test-null",
			rootAgent: "",
			wantRoot:  true, wantCoordinator: true,
			why: "pre-migration row (NULL root_agent_name) with an @main name",
		},
		{
			session: "prism-test-null@feature", repo: "prism-test-null",
			rootAgent: "",
			wantRoot:  false, wantCoordinator: false,
			why: "pre-migration row on a feature branch",
		},
		{
			// The repo column disagrees with the name prefix. The Go
			// heuristic reads the NAME, so the SQL must not read
			// `repo || '@main'`.
			session: "prism-test-mismatch@main", repo: "prism-test-other-repo",
			rootAgent: "worker",
			wantRoot:  true, wantCoordinator: true,
			why: "@main name whose repo column disagrees with the name prefix",
		},
		{
			session: "prism-test-dbcoord@feature", repo: "prism-test-dbcoord",
			rootAgent: "coordinator",
			wantRoot:  true, wantCoordinator: true,
			why: "DB-backed coordinator on a non-main branch",
		},

		// ── A repo directory whose own name holds "~" ──────────────────────
		// The descendant guard searches the BRANCH part, not the whole name.
		// A whole-name search would demote these two, which would take the
		// merge queue away from a working coordinator — a regression against
		// main, not a tightening.
		{
			session: "prism-test~odd@main", repo: "prism-test~odd",
			rootAgent: "coordinator",
			wantRoot:  true, wantCoordinator: true,
			why: "coordinator of a repo whose directory name holds a tilde",
		},
		{
			session: "prism-test~odd@feature", repo: "prism-test~odd",
			rootAgent: "worker",
			wantRoot:  false, wantCoordinator: false,
			why: "worker of a repo whose directory name holds a tilde",
		},
		{
			session: "prism-test~odd@feature~review-1-review-goal", repo: "prism-test~odd",
			rootAgent: "review-goal",
			wantRoot:  false, wantCoordinator: false,
			why: "review agent of a repo whose directory name holds a tilde — the branch part decides",
		},

		// ── Meta-sessions ──────────────────────────────────────────────────
		// Production never writes these rows: cmd/event.go refuses them at
		// the write, and TestMetaSessionsAreNotWrittenToAgentStatus in
		// cmd/ pins that. They are seeded here anyway, so the exclusion is
		// proved at the read as well as at the write.
		{
			session: sessionname.Scratchpad, repo: sessionname.Scratchpad,
			rootAgent: "",
			wantRoot:  false, wantCoordinator: false,
			why: "scratchpad meta-session",
		},
		{
			session: sessionname.Dashboard, repo: sessionname.Dashboard,
			rootAgent: "",
			wantRoot:  false, wantCoordinator: false,
			why: "prism-dashboard meta-session",
		},
	}
}

// seedRootFixtures writes every fixture row into d.
func seedRootFixtures(t *testing.T, d *db.DB, fixtures []rootFixture) {
	t.Helper()
	for _, f := range fixtures {
		var err error
		if f.rootAgent == "" {
			// UpsertStatus never writes root_agent_name, so the column stays
			// NULL — the pre-migration shape.
			err = d.UpsertStatus(f.session, f.repo, "/tmp/"+f.session, "active", nil, nil)
		} else {
			err = d.UpsertStatusSeedRootAgentName(f.session, f.repo, "/tmp/"+f.session, "active", nil, nil, f.rootAgent, "pi", "host")
		}
		if err != nil {
			t.Fatalf("seed %q: %v", f.session, err)
		}
	}
}

// TestIsRootSession is the predicate table. It is the answer to the AC
// "a `~`-suffixed session is never classified as a coordinator or as a root
// session" and to "every `<repo>@main` session is still classified as a
// coordinator".
func TestIsRootSession(t *testing.T) {
	d := openRootSessionTestDB(t)
	fixtures := rootFixtures()
	seedRootFixtures(t, d, fixtures)

	for _, f := range fixtures {
		t.Run(f.session, func(t *testing.T) {
			if got := IsRootSession(f.session, d, quietLogger()); got != f.wantRoot {
				t.Errorf("IsRootSession(%q) = %v, want %v (%s)", f.session, got, f.wantRoot, f.why)
			}
			if got := IsCoordinatorSession(f.session, d, quietLogger()); got != f.wantCoordinator {
				t.Errorf("IsCoordinatorSession(%q) = %v, want %v (%s)", f.session, got, f.wantCoordinator, f.why)
			}
		})
	}
}

// TestIsRootSession_IsNarrowerThanCoordinator states the design decision
// as an assertion rather than as prose: the two predicates differ on
// exactly one shape — the bare name — and the root predicate never refuses a
// session the coordinator predicate admits.
//
// If someone later collapses IsRootSession into IsCoordinatorSession (option A
// of the issue), the first loop fails. If someone widens
// IsCoordinatorSession to accept a bare name, the second loop fails.
func TestIsRootSession_IsNarrowerThanCoordinator(t *testing.T) {
	d := openRootSessionTestDB(t)
	fixtures := rootFixtures()
	seedRootFixtures(t, d, fixtures)

	sawBareOnlyRoot := false
	for _, f := range fixtures {
		root := IsRootSession(f.session, d, quietLogger())
		coord := IsCoordinatorSession(f.session, d, quietLogger())

		// Root must be a superset of coordinator: a coordinator is always a
		// root session.
		if coord && !root {
			t.Errorf("%q is a coordinator but not a root session — the root predicate must not refuse a coordinator", f.session)
		}
		// The only way to be a root session without being a coordinator is to
		// carry a bare name.
		if root && !coord {
			if sessionname.HasBranch(f.session) {
				t.Errorf("%q is a root session but not a coordinator, yet it carries a branch — the extra grant must be limited to bare names", f.session)
			}
			sawBareOnlyRoot = true
		}
	}
	if !sawBareOnlyRoot {
		t.Fatal("no fixture exercised the bare-name-only root grant — the test is vacuous")
	}
}

// TestIsRootSession_NilDBAndNilRows covers the fallback paths. A predicate
// that is consulted on an authorization path must give a defined answer with
// no DB handle, and it must not widen when the DB is absent.
func TestIsRootSession_NilDB(t *testing.T) {
	tests := []struct {
		session string
		want    bool
	}{
		{"prism-test-bare", true},                           // bare name — name alone decides
		{"prism-test-wt@main", true},                        // @main heuristic
		{"prism-test-wt@feature", false},                    // worktree, not main
		{"prism-test-bare~investigate-v2", false},           // descendant
		{"prism-test-wt@main~review-1-review-goal", false},  // descendant of a coordinator
		{"prism-test~odd@main", true},                       // tilde in the REPO name, not the branch
		{"prism-test~odd@main~review-1-review-goal", false}, // tilde in both — branch part decides
		{sessionname.Scratchpad, false},                     // meta
		{sessionname.Dashboard, false},                      // meta
		{"", false},                                         // empty
		{"@main", false},                                    // no repo part
		{"~investigate-x", false},                           // no repo part, descendant
	}
	for _, tc := range tests {
		if got := IsRootSession(tc.session, nil, quietLogger()); got != tc.want {
			t.Errorf("IsRootSession(%q, nil) = %v, want %v", tc.session, got, tc.want)
		}
	}
}

// TestRepoFromSession_BareNameStillNeedsARow pins the boundary that keeps an
// unknown bare name from being admitted by the new bare-name arm.
//
// IsRootSession admits any bare name on the name alone. That is safe only
// because /prompt resolves the target through RepoFromSession first, and
// RepoFromSession refuses a name with no "@" and no agent_status row. If this
// test ever fails, `prism prompt <typo>` stops returning 404 and starts
// returning an opaque delivery failure instead — the confusion this predicate prevents.
func TestRepoFromSession_BareNameStillNeedsARow(t *testing.T) {
	d := openRootSessionTestDB(t)
	if err := d.UpsertStatusSeedRootAgentName(
		"prism-test-bare", "prism-test-bare", "/tmp/prism-test-bare",
		"active", nil, nil, "review-goal", "pi", "host",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Known bare name → resolves from the row.
	got, err := RepoFromSession("prism-test-bare", d)
	if err != nil {
		t.Fatalf("RepoFromSession(known bare name): %v", err)
	}
	if got != "prism-test-bare" {
		t.Errorf("RepoFromSession(%q) = %q, want %q", "prism-test-bare", got, "prism-test-bare")
	}

	// Unknown bare name → error, so the caller can return 404.
	if _, err := RepoFromSession("prism-test-ghost", d); err == nil {
		t.Error("RepoFromSession(unknown bare name) returned no error — the caller can no longer answer 404")
	}

	// An @-bearing name with no row still resolves from the name.
	got, err = RepoFromSession("prism-test-new@brand-new-branch", d)
	if err != nil {
		t.Fatalf("RepoFromSession(unknown @-bearing name): %v", err)
	}
	if got != "prism-test-new" {
		t.Errorf("RepoFromSession(%q) = %q, want %q", "prism-test-new@brand-new-branch", got, "prism-test-new")
	}
}

// TestRepoFromSessionName_DescendantOfABareName is the AC "repo derivation
// returns `obsidian` for both `obsidian` and `obsidian~investigate-v2`" at the
// authz surface. internal/sessionname holds the exhaustive table; this pins
// that authz reads the same rule.
func TestRepoFromSessionName_DescendantOfABareName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"prism-test-bare", "prism-test-bare"},
		{"prism-test-bare~investigate-v2", "prism-test-bare"},
		{"prism-test-wt@feature~review-1-review-goal", "prism-test-wt"},
	} {
		if got := RepoFromSessionName(tc.in); got != tc.want {
			t.Errorf("RepoFromSessionName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRootSessionParity_GoAndSQL is the AC: "for a fixture set that covers a
// bare name, <repo>@main, <repo>@branch, <bare>~investigate-<slug>, and
// <repo>@branch~review-<n>-<role>, the classification used by /prompt and the
// classification used by `prism sessions list` return the same answer for
// every entry."
//
// The two classifications are written in two languages. /prompt reads the Go
// predicate authz.IsRootSession. `prism sessions list` reads the SQL in
// db.AllActiveStatusForRepoAndOtherRootSessions. There is no way to share one
// implementation across the process boundary, so the agreement is pinned by
// running both over the same rows.
//
// The query runs from a viewer repo that owns none of the fixtures, so every
// fixture row takes the cross-repo arm — the arm the two surfaces must agree
// on. Own-repo rows are always visible and are not a classification question.
func TestRootSessionParity_GoAndSQL(t *testing.T) {
	const viewerRepo = "prism-test-viewer"

	d := openRootSessionTestDB(t)
	fixtures := rootFixtures()
	seedRootFixtures(t, d, fixtures)

	// Guard the premise: no fixture may belong to the viewer's repo, or its
	// visibility would be decided by the same-repo arm and prove nothing.
	for _, f := range fixtures {
		if f.repo == viewerRepo {
			t.Fatalf("fixture %q belongs to the viewer repo %q — it would not exercise the cross-repo arm", f.session, viewerRepo)
		}
	}

	rows, err := d.AllActiveStatusForRepoAndOtherRootSessions(viewerRepo)
	if err != nil {
		t.Fatalf("AllActiveStatusForRepoAndOtherRootSessions: %v", err)
	}
	visible := make(map[string]bool, len(rows))
	for _, r := range rows {
		visible[r.SessionName] = true
	}

	for _, f := range fixtures {
		goVerdict := IsRootSession(f.session, d, quietLogger())
		sqlVerdict := visible[f.session]

		if goVerdict != sqlVerdict {
			t.Errorf("%q: authz.IsRootSession = %v but the sessions-list SQL says %v (%s)",
				f.session, goVerdict, sqlVerdict, f.why)
		}
		// Pin the expected value too. Without this, the two could agree by
		// both being wrong.
		if goVerdict != f.wantRoot {
			t.Errorf("%q: IsRootSession = %v, want %v (%s)", f.session, goVerdict, f.wantRoot, f.why)
		}
	}
}

// TestRootSessionParity_OwnRepoIsAlwaysVisible pins the other half of the
// listing contract: the root-session filter applies to OTHER repos only. A
// worker, a review agent and an investigator in the viewer's own repo are all
// still listed.
func TestRootSessionParity_OwnRepoIsAlwaysVisible(t *testing.T) {
	const ownRepo = "prism-test-own"

	d := openRootSessionTestDB(t)
	own := []string{
		"prism-test-own@main",
		"prism-test-own@feature",
		"prism-test-own@feature~review-1-review-goal",
	}
	for _, s := range own {
		if err := d.UpsertStatusSeedRootAgentName(s, ownRepo, "/tmp/"+s, "active", nil, nil, "worker", "pi", ""); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	rows, err := d.AllActiveStatusForRepoAndOtherRootSessions(ownRepo)
	if err != nil {
		t.Fatalf("AllActiveStatusForRepoAndOtherRootSessions: %v", err)
	}
	visible := make(map[string]bool, len(rows))
	for _, r := range rows {
		visible[r.SessionName] = true
	}
	for _, s := range own {
		if !visible[s] {
			t.Errorf("own-repo session %q is missing from the listing — the root filter must apply to other repos only", s)
		}
	}
}
