package main

// reviews_test.go — unit + integration tests for `iris reviews` and
// `iris reviews show` (#1722).
//
// All tests run under iristest.NewIsolated so HOME / XDG_STATE_HOME are
// redirected to a per-test tempdir. No host state is touched and no
// notification can escape to a live coordinator (#1608).
//
// Seeding strategy:
//
//   We exercise the read surface directly against iristest's iso.DB,
//   inserting session_groups rows via RegisterGroup and agent_status
//   rows via UpsertStatusWithRootAgent + SetGroupID. This is the same
//   shape that iris's review orchestrator would write — but it avoids
//   spinning up the full daemon-side review handler (which has its own
//   integration test, TestReview_FullCycle in review_integration_test.go).
//
// The CLI is driven through the global rootCmd via runReviewsCommand,
// the reviews-package analogue of db_test.go::runCommand. Flag state on
// reviewsCmd / reviewsShowCmd is reset between invocations so cobra/pflag
// does not leak values across test functions.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	_ "modernc.org/sqlite"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// runReviewsCommand drives the global rootCmd with the given args and
// returns captured stdout + the cobra error. Resets reviewsCmd flags
// before and after so test order does not matter.
func runReviewsCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetReviewsFlags()
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		rootCmd.SetIn(nil)
		resetReviewsFlags()
	})
	err := rootCmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

func resetReviewsFlags() {
	visit := func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	visit(reviewsCmd)
	for _, sub := range reviewsCmd.Commands() {
		visit(sub)
	}
}

// seedReviewGroup registers a fresh session_groups row with parentSession
// as the parent and seeds memberRoles as agent_status rows joined to that
// group. The members are upserted in `state` (e.g. "active" or "finished")
// so the group's rolled-up state matches what the test wants to assert.
//
// Returns (groupID, memberSessionNames).
func seedReviewGroup(t *testing.T, iso *iristest.Isolated, parentSession string, memberRoles []string, state string) (string, []string) {
	t.Helper()
	groupID, err := iso.DB.RegisterGroup(parentSession)
	if err != nil {
		t.Fatalf("RegisterGroup(%s): %v", parentSession, err)
	}
	members := make([]string, 0, len(memberRoles))
	for _, role := range memberRoles {
		role := role
		sess := parentSession + "~review-1-" + role
		if err := iso.DB.UpsertStatusWithRootAgent(sess, "iris-test", iso.Root, state, nil, nil, &role, nil); err != nil {
			t.Fatalf("UpsertStatusWithRootAgent(%s): %v", sess, err)
		}
		if err := iso.DB.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%s, %s): %v", sess, groupID, err)
		}
		members = append(members, sess)
	}
	return groupID, members
}

// ─────────────────────────────────────────────────────────────────────────────
// iris reviews (list)
// ─────────────────────────────────────────────────────────────────────────────

// TestReviewsList_Empty asserts that `iris reviews` against a fresh DB
// (no session_groups rows) prints the "no reviews recorded" sentinel and
// exits 0.
func TestReviewsList_Empty(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runReviewsCommand(t, "reviews")
	if err != nil {
		t.Fatalf("iris reviews: %v", err)
	}
	if !strings.Contains(out, "no reviews recorded") {
		t.Errorf("expected 'no reviews recorded', got:\n%s", out)
	}
}

// TestReviewsList_ShowsRecentGroups seeds two groups (one in-progress,
// one completed) and asserts both appear in the list output with the
// correct rolled-up state.
func TestReviewsList_ShowsRecentGroups(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parentA := iristest.SessionName("worker-a")
	parentB := iristest.SessionName("worker-b")
	_, _ = seedReviewGroup(t, iso, parentA, []string{"review-goal", "review-code"}, "active")
	groupB, _ := seedReviewGroup(t, iso, parentB, []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}, "finished")

	out, err := runReviewsCommand(t, "reviews")
	if err != nil {
		t.Fatalf("iris reviews: %v", err)
	}
	// Both parents should appear (truncated column may strip the prefix
	// but the branch suffix uniquely identifies each row).
	for _, want := range []string{"worker-a", "worker-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in list output:\n%s", want, out)
		}
	}
	// The completed group should be tagged as such.
	if !strings.Contains(out, "completed") {
		t.Errorf("expected 'completed' state tag for group %s in:\n%s", groupB, out)
	}
	if !strings.Contains(out, "in-progress") {
		t.Errorf("expected 'in-progress' state tag in:\n%s", out)
	}
}

// TestReviewsList_JSON asserts that `--json` emits a parseable JSON array
// whose entries carry the expected fields.
func TestReviewsList_JSON(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parent := iristest.SessionName("worker-json")
	groupID, members := seedReviewGroup(t, iso, parent, []string{"review-goal", "review-code"}, "finished")

	out, err := runReviewsCommand(t, "reviews", "--json")
	if err != nil {
		t.Fatalf("iris reviews --json: %v", err)
	}

	var rows []reviewsJSONRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("expected JSON array, got: %s\nerr: %v", out, err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %s", len(rows), out)
	}
	row := rows[0]
	if row.GroupID != groupID {
		t.Errorf("GroupID = %q, want %q", row.GroupID, groupID)
	}
	if row.ParentSession != parent {
		t.Errorf("ParentSession = %q, want %q", row.ParentSession, parent)
	}
	if row.GroupState != "completed" {
		t.Errorf("GroupState = %q, want 'completed'", row.GroupState)
	}
	if row.MemberCount != len(members) {
		t.Errorf("MemberCount = %d, want %d", row.MemberCount, len(members))
	}
	if len(row.AgentSessions) != len(members) {
		t.Errorf("AgentSessions = %v, want %d entries", row.AgentSessions, len(members))
	}
	if row.Round == nil || *row.Round != 1 {
		t.Errorf("Round = %v, want 1 (from session-name suffix)", row.Round)
	}
	if row.StartedAt == "" {
		t.Errorf("StartedAt is empty")
	}
	if _, err := time.Parse(time.RFC3339, row.StartedAt); err != nil {
		t.Errorf("StartedAt = %q is not RFC3339: %v", row.StartedAt, err)
	}
}

// TestReviewsList_DaysFilter asserts that `--days N` filters out groups
// older than N days. We achieve a deterministic "old" group by
// back-dating session_groups.created_at via a direct UPDATE — the only
// way to drive the filter without sleeping in tests.
func TestReviewsList_DaysFilter(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parentRecent := iristest.SessionName("worker-recent")
	parentOld := iristest.SessionName("worker-old")
	_, _ = seedReviewGroup(t, iso, parentRecent, []string{"review-goal"}, "active")
	oldGroupID, _ := seedReviewGroup(t, iso, parentOld, []string{"review-goal"}, "finished")

	// Back-date the "old" group to 10 days ago. The *db.DB type does not
	// expose Exec (intentionally — write surfaces are routed through
	// helpers), so we open a sibling sqlite handle for this one-line
	// fixture write. This is a test-only hack scoped to the iris reviews
	// suite; production code never touches session_groups.created_at.
	backdateCreatedAt(t, iso.Paths.DB, oldGroupID, time.Now().Add(-10*24*time.Hour))

	// With --days 7 the old group is filtered out, the recent one stays.
	out, err := runReviewsCommand(t, "reviews", "--days", "7")
	if err != nil {
		t.Fatalf("iris reviews --days 7: %v", err)
	}
	if !strings.Contains(out, "worker-recent") {
		t.Errorf("expected 'worker-recent' in --days 7 output:\n%s", out)
	}
	if strings.Contains(out, "worker-old") {
		t.Errorf("'worker-old' should be filtered out at --days 7:\n%s", out)
	}

	// With no filter both should appear.
	outAll, err := runReviewsCommand(t, "reviews")
	if err != nil {
		t.Fatalf("iris reviews (no filter): %v", err)
	}
	if !strings.Contains(outAll, "worker-recent") || !strings.Contains(outAll, "worker-old") {
		t.Errorf("expected both groups in unfiltered output:\n%s", outAll)
	}
}

// TestReviewsList_NegativeDaysRejected asserts that --days < 0 is a
// usage error (non-zero exit, descriptive message).
func TestReviewsList_NegativeDaysRejected(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runReviewsCommand(t, "reviews", "--days", "-1")
	if err == nil {
		t.Fatalf("expected error for --days -1")
	}
	if !strings.Contains(err.Error(), "--days") {
		t.Errorf("error should mention --days, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iris reviews show <group_id>
// ─────────────────────────────────────────────────────────────────────────────

// TestReviewsShow_ListsMembers seeds a group with the canonical 5 review
// agents and asserts `iris reviews show <id>` prints each member's
// session name and role.
func TestReviewsShow_ListsMembers(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parent := iristest.SessionName("worker-show")
	roles := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	groupID, members := seedReviewGroup(t, iso, parent, roles, "finished")

	out, err := runReviewsCommand(t, "reviews", "show", groupID)
	if err != nil {
		t.Fatalf("iris reviews show: %v\n%s", err, out)
	}

	// Group header fields.
	if !strings.Contains(out, groupID) {
		t.Errorf("output missing group_id %q:\n%s", groupID, out)
	}
	if !strings.Contains(out, parent) {
		t.Errorf("output missing parent session %q:\n%s", parent, out)
	}
	if !strings.Contains(out, "completed") {
		t.Errorf("output missing 'completed' state:\n%s", out)
	}

	// Each member's session name and role should appear.
	for i, role := range roles {
		if !strings.Contains(out, members[i]) {
			t.Errorf("output missing member %q:\n%s", members[i], out)
		}
		if !strings.Contains(out, role) {
			t.Errorf("output missing role %q:\n%s", role, out)
		}
	}
}

// TestReviewsShow_JSON asserts that --json emits a JSON object with a
// members array carrying state + role per member.
func TestReviewsShow_JSON(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parent := iristest.SessionName("worker-show-json")
	roles := []string{"review-goal", "review-code"}
	groupID, _ := seedReviewGroup(t, iso, parent, roles, "finished")

	out, err := runReviewsCommand(t, "reviews", "show", groupID, "--json")
	if err != nil {
		t.Fatalf("iris reviews show --json: %v\n%s", err, out)
	}

	var body reviewsShowJSON
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("expected JSON object, got: %s\nerr: %v", out, err)
	}
	if body.GroupID != groupID {
		t.Errorf("GroupID = %q, want %q", body.GroupID, groupID)
	}
	if body.ParentSession != parent {
		t.Errorf("ParentSession = %q, want %q", body.ParentSession, parent)
	}
	if body.GroupState != "completed" {
		t.Errorf("GroupState = %q, want 'completed'", body.GroupState)
	}
	if len(body.Members) != len(roles) {
		t.Fatalf("Members = %d, want %d", len(body.Members), len(roles))
	}
	gotRoles := map[string]bool{}
	for _, m := range body.Members {
		gotRoles[m.Role] = true
		if m.State != "finished" {
			t.Errorf("member %s state = %q, want 'finished'", m.SessionName, m.State)
		}
		if m.LastSeen == "" {
			t.Errorf("member %s has empty LastSeen", m.SessionName)
		}
	}
	for _, want := range roles {
		if !gotRoles[want] {
			t.Errorf("expected role %q in JSON members, got: %+v", want, body.Members)
		}
	}
}

// TestReviewsShow_NoSuchGroup asserts that an unknown group_id exits
// non-zero with a clear "no such review group" message.
func TestReviewsShow_NoSuchGroup(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runReviewsCommand(t, "reviews", "show", "deadbeef-not-a-group")
	if err == nil {
		t.Fatalf("expected error for missing group")
	}
	if !strings.Contains(err.Error(), "no such review group") {
		t.Errorf("error should mention 'no such review group', got: %v", err)
	}
}

// TestReviewsShow_EmptyGroup asserts that a group registered with no
// members renders cleanly (header + "(no members)" sentinel) rather
// than erroring.
func TestReviewsShow_EmptyGroup(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parent := iristest.SessionName("worker-empty")
	groupID, err := iso.DB.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	out, err := runReviewsCommand(t, "reviews", "show", groupID)
	if err != nil {
		t.Fatalf("iris reviews show (empty): %v\n%s", err, out)
	}
	if !strings.Contains(out, "(no members)") {
		t.Errorf("expected '(no members)' sentinel, got:\n%s", out)
	}
	if !strings.Contains(out, groupID) {
		t.Errorf("output missing group_id %q:\n%s", groupID, out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: full review-cycle → list → show round-trip.
// ─────────────────────────────────────────────────────────────────────────────

// TestReviews_FullCycle_ListShow is the post-#1707 integration test
// required by issue #1722's ACs: drive the review-spawn write path
// against the DB, then exercise `iris reviews` to list it and `iris
// reviews show <group>` to inspect its members. We register the group
// and members directly (the equivalent of what the daemon-side
// orchestrator writes) rather than spinning up the live ClientSocket —
// that path has its own dedicated test (TestReview_FullCycle).
func TestReviews_FullCycle_ListShow(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	parent := iristest.SessionName("review-parent-integration")
	roles := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	groupID, members := seedReviewGroup(t, iso, parent, roles, "finished")

	// Step 1: `iris reviews` lists the group.
	listOut, err := runReviewsCommand(t, "reviews")
	if err != nil {
		t.Fatalf("iris reviews: %v", err)
	}
	if !strings.Contains(listOut, parent) {
		t.Errorf("list output missing parent %q:\n%s", parent, listOut)
	}
	if !strings.Contains(listOut, "completed") {
		t.Errorf("list output missing 'completed' state:\n%s", listOut)
	}

	// Step 2: `iris reviews --json` returns the group_id.
	jsonOut, err := runReviewsCommand(t, "reviews", "--json")
	if err != nil {
		t.Fatalf("iris reviews --json: %v", err)
	}
	var rows []reviewsJSONRow
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("expected JSON array: %v\n%s", err, jsonOut)
	}
	found := false
	for _, r := range rows {
		if r.GroupID == groupID {
			found = true
			if r.MemberCount != len(roles) {
				t.Errorf("member_count = %d, want %d", r.MemberCount, len(roles))
			}
			break
		}
	}
	if !found {
		t.Errorf("group_id %q not found in --json output: %s", groupID, jsonOut)
	}

	// Step 3: `iris reviews show <group_id>` lists all 5 members.
	showOut, err := runReviewsCommand(t, "reviews", "show", groupID)
	if err != nil {
		t.Fatalf("iris reviews show: %v\n%s", err, showOut)
	}
	for i, role := range roles {
		if !strings.Contains(showOut, members[i]) {
			t.Errorf("show output missing member %q:\n%s", members[i], showOut)
		}
		if !strings.Contains(showOut, role) {
			t.Errorf("show output missing role %q:\n%s", role, showOut)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper-function unit tests
// ─────────────────────────────────────────────────────────────────────────────

// TestInferPRFromGroup covers the parent-branch heuristic for PR
// extraction. The list / show surfaces both rely on this so a wrong
// guess for one ripples to the other.
func TestInferPRFromGroup(t *testing.T) {
	cases := []struct {
		parent string
		want   int
	}{
		{"repo@pr-123", 123},
		{"repo@pr456", 456},
		{"repo@main", 0},
		{"repo@feature-x", 0},
		{"no-at-symbol", 0},
		{"repo@pr-", 0},
		{"repo@pr-0", 0},
	}
	for _, tc := range cases {
		g := makeSummary(tc.parent, nil)
		if got := inferPRFromGroup(g); got != tc.want {
			t.Errorf("inferPRFromGroup(%q) = %d, want %d", tc.parent, got, tc.want)
		}
	}
}

// TestInferRoundFromGroup covers the per-member round-number parse.
func TestInferRoundFromGroup(t *testing.T) {
	cases := []struct {
		members []string
		want    int
	}{
		{[]string{"repo@main~review-1-review-goal"}, 1},
		{[]string{"repo@main~review-3-review-code", "repo@main~review-3-review-qa"}, 3},
		{[]string{"repo@main~not-a-review-session"}, 0},
		{nil, 0},
	}
	for _, tc := range cases {
		g := makeSummary("repo@main", tc.members)
		if got := inferRoundFromGroup(g); got != tc.want {
			t.Errorf("inferRoundFromGroup(%v) = %d, want %d", tc.members, got, tc.want)
		}
	}
}

// TestFormatDuration covers the human-readable duration buckets.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "<1s"},
		{3 * time.Second, "3s"},
		{45 * time.Second, "45s"},
		{2 * time.Minute, "2m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{1*time.Hour + 30*time.Minute, "1h30m"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// backdateCreatedAt rewrites session_groups.created_at for the given
// group_id by opening a sibling read-write handle on the iris DB file.
//
// internal/db deliberately doesn't expose an Exec method, so to set up
// the --days-filter fixture we open a one-shot sqlite handle, run the
// UPDATE, and close immediately. WAL mode (set by iris.OpenDB on the
// iso.DB handle) lets the two handles coexist.
func backdateCreatedAt(t *testing.T, dbPath, groupID string, when time.Time) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("backdateCreatedAt: open: %v", err)
	}
	defer conn.Close()
	// The session_groups.created_at column is stored as the SQLite
	// CURRENT_TIMESTAMP textual form ("YYYY-MM-DD HH:MM:SS"). Use the
	// same shape so the column scans back into time.Time on read.
	ts := when.UTC().Format("2006-01-02 15:04:05")
	if _, err := conn.Exec(`UPDATE session_groups SET created_at = ? WHERE group_id = ?`, ts, groupID); err != nil {
		t.Fatalf("backdateCreatedAt: exec: %v", err)
	}
}

// makeSummary is a tiny helper for the inference-function tests so we
// don't allocate a full DB just to test pure string parsing.
func makeSummary(parent string, members []string) db.ReviewGroupSummary {
	return db.ReviewGroupSummary{ParentSession: parent, Members: members}
}
