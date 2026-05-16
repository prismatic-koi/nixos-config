package main

// merges_test.go — unit + integration tests for `iris merges` (issue #1719).
//
// The unit tests assert byte-for-byte rendering parity with prism's
// cmd/merges.go: column widths, header shape, POS as 1-based rank,
// empty-state messages, and JSON envelope shape.
//
// The integration test exercises the cobra subcommands end-to-end against
// an iristest.NewIsolated DB: enqueue → list → cancel → list-after-cancel.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// strPtrMerges is a tiny helper so test rows can carry pointer-typed Title /
// Error fields without local declarations cluttering the assertions. Named
// to avoid collision with strPtr in db_test.go / archive_test.go.
func strPtrMerges(s string) *string { return &s }

// expectedIrisPosPrefix builds the expected POS-column prefix for a 1-based
// rank, padded to irisMergesWPos with %-*s. Uses the real constant so the
// test stays correct if the column width is ever changed.
func expectedIrisPosPrefix(rank int) string {
	return fmt.Sprintf("%-*s", irisMergesWPos, fmt.Sprintf("%d", rank))
}

// TestIrisFormatMergesRows_PosIsOneBasedRank verifies POS is the rank in
// the input slice, not the raw queue_position timestamp. Mirrors the
// equivalent prism unit test so any regression here also flags in the
// shared "POS == rank" contract.
func TestIrisFormatMergesRows_PosIsOneBasedRank(t *testing.T) {
	now := time.Date(2025, 4, 26, 12, 0, 0, 0, time.UTC)
	queuedAt := now.Add(-3 * time.Minute)

	merges := []db.PendingMerge{
		{PR: 1046, Status: "watching", QueuePosition: 1777154720435, Title: strPtrMerges("first"), QueuedAt: queuedAt},
		{PR: 1047, Status: "watching", QueuePosition: 1777154720436, Title: strPtrMerges("second"), QueuedAt: queuedAt},
		{PR: 1048, Status: "watching", QueuePosition: 1777154720437, Title: strPtrMerges("third"), QueuedAt: queuedAt},
	}

	_, rows := formatIrisMergesRows(merges, now)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, row := range rows {
		want := expectedIrisPosPrefix(i + 1)
		if !strings.HasPrefix(row, want) {
			t.Errorf("row %d: expected POS prefix %q, got row %q", i, want, row)
		}
		if strings.Contains(row, "17771") {
			t.Errorf("row %d: raw queue_position timestamp leaked into output: %q", i, row)
		}
	}
}

// TestIrisFormatMergesRows_HeaderShape asserts the header is identical to
// prism's. The header is the easiest byte-for-byte parity check.
func TestIrisFormatMergesRows_HeaderShape(t *testing.T) {
	now := time.Date(2025, 4, 26, 12, 0, 0, 0, time.UTC)
	header, _ := formatIrisMergesRows(nil, now)

	// Built from the same template as cmd/merges.go::formatMergesRows.
	want := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s",
		irisMergesWPos, "POS",
		irisMergesWPR, "PR",
		irisMergesWTitle, "TITLE",
		irisMergesWStatus, "STATUS",
		irisMergesWQueued, "QUEUED",
		irisMergesWChecked, "CHECKED",
		"ERROR",
	)
	if header != want {
		t.Errorf("header mismatch:\n got: %q\nwant: %q", header, want)
	}
}

// TestIrisFormatMergesRows_ColumnWidthsMatchPrism asserts the iris column
// constants match the prism ones byte-for-byte. The values are hard-coded
// here rather than imported because `cmd/merges.go` lives in the prism
// `cmd` package (not importable from `cmd/iris`); the assertion catches
// any future drift.
func TestIrisFormatMergesRows_ColumnWidthsMatchPrism(t *testing.T) {
	// Constants copied verbatim from cmd/merges.go for the parity check.
	const (
		wantPos     = 5
		wantPR      = 6
		wantTitle   = 40
		wantStatus  = 11
		wantQueued  = 10
		wantChecked = 10
		wantError   = 40
	)
	cases := []struct {
		name      string
		got, want int
	}{
		{"WPos", irisMergesWPos, wantPos},
		{"WPR", irisMergesWPR, wantPR},
		{"WTitle", irisMergesWTitle, wantTitle},
		{"WStatus", irisMergesWStatus, wantStatus},
		{"WQueued", irisMergesWQueued, wantQueued},
		{"WChecked", irisMergesWChecked, wantChecked},
		{"WError", irisMergesWError, wantError},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (must match prism cmd/merges.go for byte-for-byte parity)", c.name, c.got, c.want)
		}
	}
}

// TestIrisEmptyMergesMessage covers the empty-state messages for each
// filter. Strings must match prism's emptyMergesMessage byte-for-byte.
func TestIrisEmptyMergesMessage(t *testing.T) {
	cases := map[string]string{
		"":          "merge queue is empty",
		"failed":    "no failed merge queue entries",
		"abandoned": "no abandoned merge queue entries from previous coordinator sessions",
		"all":       "no merge queue entries in the last 7 days",
	}
	for filter, want := range cases {
		got := emptyIrisMergesMessage(filter)
		if got != want {
			t.Errorf("emptyIrisMergesMessage(%q) = %q, want %q", filter, got, want)
		}
	}
}

// TestIrisRenderMergesList_EmptyQueueWrites the canonical empty-state line
// when the slice is empty.
func TestIrisRenderMergesList_EmptyQueue(t *testing.T) {
	var buf bytes.Buffer
	if err := renderIrisMergesList(&buf, nil, ""); err != nil {
		t.Fatalf("renderIrisMergesList: %v", err)
	}
	got := strings.TrimRight(buf.String(), "\n")
	if got != "merge queue is empty" {
		t.Errorf("got %q, want %q", got, "merge queue is empty")
	}
}

// TestIrisRenderMergesListJSON_EmptySliceShape asserts the JSON shape for
// the empty case: an empty array under `merges` and `truncated:false`.
func TestIrisRenderMergesListJSON_EmptySliceShape(t *testing.T) {
	var buf bytes.Buffer
	if err := renderIrisMergesListJSON(&buf, nil); err != nil {
		t.Fatalf("renderIrisMergesListJSON: %v", err)
	}
	var got struct {
		Merges    []irisMergeJSONRow `json:"merges"`
		Truncated bool               `json:"truncated"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, buf.String())
	}
	if got.Merges == nil {
		t.Errorf("merges is nil, want empty array")
	}
	if len(got.Merges) != 0 {
		t.Errorf("len(merges) = %d, want 0", len(got.Merges))
	}
	if got.Truncated {
		t.Errorf("truncated = true, want false")
	}
}

// TestIrisRenderMergesListJSON_RoundTrip writes a small slice and asserts
// the timestamps and pointer fields round-trip the way the contract claims.
func TestIrisRenderMergesListJSON_RoundTrip(t *testing.T) {
	queuedAt := time.Date(2025, 4, 26, 12, 0, 0, 0, time.UTC)
	checkedAt := queuedAt.Add(30 * time.Second)
	endedAt := queuedAt.Add(60 * time.Second)
	in := []db.PendingMerge{
		{
			PR:            1046,
			SessionName:   "iris-test@coord",
			InstanceID:    "iris-test-instance-merge-json",
			QueuePosition: 1777154720435,
			Status:        "merged",
			Title:         strPtrMerges("first"),
			QueuedAt:      queuedAt,
			LastCheckedAt: &checkedAt,
			MergedAt:      &endedAt,
			EndedAt:       &endedAt,
		},
	}
	var buf bytes.Buffer
	if err := renderIrisMergesListJSON(&buf, in); err != nil {
		t.Fatalf("renderIrisMergesListJSON: %v", err)
	}
	var out struct {
		Merges []irisMergeJSONRow `json:"merges"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, buf.String())
	}
	if len(out.Merges) != 1 {
		t.Fatalf("len(merges) = %d, want 1", len(out.Merges))
	}
	row := out.Merges[0]
	if row.PR != 1046 {
		t.Errorf("pr = %d, want 1046", row.PR)
	}
	if row.Status != "merged" {
		t.Errorf("status = %q, want merged", row.Status)
	}
	if row.CoordinatorSession != "iris-test@coord" {
		t.Errorf("coordinator_session = %q, want iris-test@coord", row.CoordinatorSession)
	}
	if row.InstanceID != "iris-test-instance-merge-json" {
		t.Errorf("instance_id = %q, want iris-test-instance-merge-json", row.InstanceID)
	}
	if row.Title == nil || *row.Title != "first" {
		t.Errorf("title = %v, want pointer to %q", row.Title, "first")
	}
	wantQueued := queuedAt.UTC().Format(time.RFC3339)
	if row.EnqueuedAt != wantQueued {
		t.Errorf("enqueued_at = %q, want %q", row.EnqueuedAt, wantQueued)
	}
	if row.LastCheckedAt == nil || *row.LastCheckedAt != checkedAt.UTC().Format(time.RFC3339) {
		t.Errorf("last_checked_at = %v, want pointer to %q", row.LastCheckedAt, checkedAt.UTC().Format(time.RFC3339))
	}
	if row.MergedAt == nil || *row.MergedAt != endedAt.UTC().Format(time.RFC3339) {
		t.Errorf("merged_at = %v, want pointer to %q", row.MergedAt, endedAt.UTC().Format(time.RFC3339))
	}
	if row.EndedAt == nil {
		t.Errorf("ended_at is nil, want set")
	}
}

// --- Integration tests against a real iris DB ---

// seedSessionStatus writes a synthetic agent_status row so
// resolveIrisMergesIdentity can resolve session → instance_id. Mirrors what
// the daemon's harness path would write at session startup, but with the
// minimum columns the merges path needs.
func seedSessionStatus(t *testing.T, database *db.DB, sessionName, instanceID string) {
	t.Helper()
	if err := database.UpsertStatus(sessionName, "iris-test", "/tmp/iris-test-merges", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := database.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
}

// newMergesTestCmds builds a fresh subcommand pair (root list flags, plus
// list and cancel) wired to runMergesList/runMergesCancel for tests. We
// don't reuse the package-level cobra.Commands because their flags are
// shared global state that would carry across tests.
func newMergesTestCmds(t *testing.T) (root, list, cancel *cobra.Command) {
	t.Helper()
	root = &cobra.Command{Use: "merges", RunE: runMergesList}
	list = &cobra.Command{Use: "list", RunE: runMergesList}
	cancel = &cobra.Command{Use: "cancel", Args: cobra.ExactArgs(1), RunE: runMergesCancel}
	for _, c := range []*cobra.Command{root, list} {
		c.Flags().Bool("failed", false, "")
		c.Flags().Bool("abandoned", false, "")
		c.Flags().Bool("all", false, "")
		c.Flags().Bool("json", false, "")
		c.Flags().String("session", "", "")
	}
	cancel.Flags().String("session", "", "")
	return root, list, cancel
}

// TestIrisMerges_Integration drives enqueue → list → cancel → list-after
// against an isolated iris DB. Asserts on the rendered stdout strings.
func TestIrisMerges_Integration(t *testing.T) {
	iso := iristest.NewIsolated(t)

	// Pin the DB path so resolveDBPath() inside the cobra RunE returns the
	// isolated DB instead of $XDG-derived path.
	SetTestDBPath(iso.Paths.DB)
	t.Cleanup(func() { SetTestDBPath("") })

	sessionName := iristest.SessionName("merges-int")
	instanceID := "iris-test-instance-merges-int-001"
	seedSessionStatus(t, iso.DB, sessionName, instanceID)

	// Enqueue two rows.
	if _, err := iris.EnqueueMerge(iso.DB, 2001, sessionName, instanceID, strPtrMerges("first")); err != nil {
		t.Fatalf("EnqueueMerge 2001: %v", err)
	}
	if _, err := iris.EnqueueMerge(iso.DB, 2002, sessionName, instanceID, strPtrMerges("second")); err != nil {
		t.Fatalf("EnqueueMerge 2002: %v", err)
	}

	_, listCmd, cancelCmd := newMergesTestCmds(t)

	// --- list ---
	var listBuf bytes.Buffer
	listCmd.SetOut(&listBuf)
	listCmd.SetErr(&listBuf)
	if err := listCmd.Flags().Set("session", sessionName); err != nil {
		t.Fatalf("set --session: %v", err)
	}
	if err := runMergesList(listCmd, nil); err != nil {
		t.Fatalf("runMergesList: %v", err)
	}
	listOut := listBuf.String()
	if !strings.Contains(listOut, "#2001") || !strings.Contains(listOut, "#2002") {
		t.Errorf("list output missing PR numbers: %q", listOut)
	}
	if !strings.Contains(listOut, "first") || !strings.Contains(listOut, "second") {
		t.Errorf("list output missing titles: %q", listOut)
	}

	// --- cancel ---
	var cancelBuf bytes.Buffer
	cancelCmd.SetOut(&cancelBuf)
	cancelCmd.SetErr(&cancelBuf)
	if err := cancelCmd.Flags().Set("session", sessionName); err != nil {
		t.Fatalf("set --session for cancel: %v", err)
	}
	if err := runMergesCancel(cancelCmd, []string{"2001"}); err != nil {
		t.Fatalf("runMergesCancel 2001: %v", err)
	}
	cancelOut := cancelBuf.String()
	if !strings.Contains(cancelOut, "PR #2001 removed from merge queue.") {
		t.Errorf("cancel output: %q (want 'PR #2001 removed from merge queue.')", cancelOut)
	}

	// --- DB-level assertion: 2001 cancelled, 2002 still watching ---
	row1, err := iso.DB.PendingMergeByPR(2001)
	if err != nil {
		t.Fatalf("PendingMergeByPR 2001: %v", err)
	}
	if row1 == nil || row1.Status != "cancelled" {
		t.Errorf("PR 2001 status = %v, want cancelled", row1)
	}
	row2, err := iso.DB.PendingMergeByPR(2002)
	if err != nil {
		t.Fatalf("PendingMergeByPR 2002: %v", err)
	}
	if row2 == nil || row2.Status != "watching" {
		t.Errorf("PR 2002 status = %v, want watching", row2)
	}

	// --- list (default = watching) shows only 2002 now ---
	listBuf.Reset()
	if err := runMergesList(listCmd, nil); err != nil {
		t.Fatalf("runMergesList after cancel: %v", err)
	}
	listOut2 := listBuf.String()
	if strings.Contains(listOut2, "#2001") {
		t.Errorf("list-after-cancel still mentions cancelled PR: %q", listOut2)
	}
	if !strings.Contains(listOut2, "#2002") {
		t.Errorf("list-after-cancel missing remaining watching PR: %q", listOut2)
	}

	// --- list --all surfaces the cancelled row too ---
	listBuf.Reset()
	if err := listCmd.Flags().Set("all", "true"); err != nil {
		t.Fatalf("set --all: %v", err)
	}
	if err := runMergesList(listCmd, nil); err != nil {
		t.Fatalf("runMergesList --all: %v", err)
	}
	listOutAll := listBuf.String()
	if !strings.Contains(listOutAll, "#2001") || !strings.Contains(listOutAll, "#2002") {
		t.Errorf("--all should show both PRs, got: %q", listOutAll)
	}
	// Reset --all for any subsequent flag manipulation.
	_ = listCmd.Flags().Set("all", "false")
}

// TestIrisMergesCancel_NotInQueue asserts the no-row branch prints the
// "not in the merge queue" message and exits 0.
func TestIrisMergesCancel_NotInQueue(t *testing.T) {
	iso := iristest.NewIsolated(t)
	SetTestDBPath(iso.Paths.DB)
	t.Cleanup(func() { SetTestDBPath("") })

	sessionName := iristest.SessionName("merges-noqueue")
	instanceID := "iris-test-instance-merges-noqueue"
	seedSessionStatus(t, iso.DB, sessionName, instanceID)

	_, _, cancelCmd := newMergesTestCmds(t)
	var buf bytes.Buffer
	cancelCmd.SetOut(&buf)
	cancelCmd.SetErr(&buf)
	_ = cancelCmd.Flags().Set("session", sessionName)
	if err := runMergesCancel(cancelCmd, []string{"9999"}); err != nil {
		t.Fatalf("runMergesCancel: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "PR #9999 is not in the merge queue.") {
		t.Errorf("got %q, want it to contain 'PR #9999 is not in the merge queue.'", got)
	}
}

// TestIrisMergesCancel_DifferentInstance asserts that attempting to cancel
// a row owned by another instance_id prints the canonical
// "different coordinator incarnation" message rather than a DB error.
// Matches prism cmd/merges.go behaviour.
func TestIrisMergesCancel_DifferentInstance(t *testing.T) {
	iso := iristest.NewIsolated(t)
	SetTestDBPath(iso.Paths.DB)
	t.Cleanup(func() { SetTestDBPath("") })

	callerSession := iristest.SessionName("merges-other")
	callerInstance := "iris-test-instance-CALLER"
	seedSessionStatus(t, iso.DB, callerSession, callerInstance)

	// Enqueue under a *different* instance_id with the same session name.
	otherInstance := "iris-test-instance-OTHER"
	if _, err := iris.EnqueueMerge(iso.DB, 4242, callerSession, otherInstance, strPtrMerges("not mine")); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	_, _, cancelCmd := newMergesTestCmds(t)
	var buf bytes.Buffer
	cancelCmd.SetOut(&buf)
	cancelCmd.SetErr(&buf)
	_ = cancelCmd.Flags().Set("session", callerSession)
	if err := runMergesCancel(cancelCmd, []string{"4242"}); err != nil {
		t.Fatalf("runMergesCancel: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "different coordinator incarnation") {
		t.Errorf("got %q, want it to mention 'different coordinator incarnation'", got)
	}

	// DB-level assertion: row must still be watching (not cancelled).
	row, err := iso.DB.PendingMergeByPR(4242)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row == nil || row.Status != "watching" {
		t.Errorf("row %v, want still watching", row)
	}
}

// TestIrisMergesCancel_AlreadyTerminal exercises the "row exists, owned by
// this instance, but already terminal" branch.
func TestIrisMergesCancel_AlreadyTerminal(t *testing.T) {
	iso := iristest.NewIsolated(t)
	SetTestDBPath(iso.Paths.DB)
	t.Cleanup(func() { SetTestDBPath("") })

	sessionName := iristest.SessionName("merges-terminal")
	instanceID := "iris-test-instance-merges-terminal"
	seedSessionStatus(t, iso.DB, sessionName, instanceID)

	if _, err := iris.EnqueueMerge(iso.DB, 5050, sessionName, instanceID, strPtrMerges("done")); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := iso.DB.TerminateMerge(5050, "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}

	_, _, cancelCmd := newMergesTestCmds(t)
	var buf bytes.Buffer
	cancelCmd.SetOut(&buf)
	cancelCmd.SetErr(&buf)
	_ = cancelCmd.Flags().Set("session", sessionName)
	if err := runMergesCancel(cancelCmd, []string{"5050"}); err != nil {
		t.Fatalf("runMergesCancel: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "already in terminal state") {
		t.Errorf("got %q, want it to mention 'already in terminal state'", got)
	}
}

// TestIrisMerges_DaemonDownError asserts that running against a non-existent
// DB file surfaces the canonical "systemctl --user start iris" hint
// (matches the rest of the iris CLI's daemon-down wording).
func TestIrisMerges_DaemonDownError(t *testing.T) {
	tmp := t.TempDir()
	missingDB := filepath.Join(tmp, "iris.db")

	SetTestDBPath(missingDB)
	t.Cleanup(func() { SetTestDBPath("") })

	_, listCmd, _ := newMergesTestCmds(t)
	var buf bytes.Buffer
	listCmd.SetOut(&buf)
	listCmd.SetErr(&buf)
	err := runMergesList(listCmd, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "iris database not found") {
		t.Errorf("error missing 'iris database not found' wording: %q", msg)
	}
	if !strings.Contains(msg, "systemctl --user start iris") {
		t.Errorf("error missing canonical 'systemctl --user start iris' hint: %q", msg)
	}
}

// TestIrisMergesList_InvalidSession asserts the "cannot determine calling
// session" error surfaces a clear hint pointing at --session / env var.
func TestIrisMergesList_InvalidSession(t *testing.T) {
	iso := iristest.NewIsolated(t)
	SetTestDBPath(iso.Paths.DB)
	t.Cleanup(func() { SetTestDBPath("") })

	// Clear env var so resolveIrisMergesCaller returns "".
	t.Setenv("IRIS_SESSION_NAME", "")
	t.Setenv("PRISM_SESSION_NAME", "")
	// Defensively clear tmux env so lookupIrisParentSession doesn't pick up
	// the host tmux session inside CI. Clearing $TMUX alone is insufficient
	// — `tmux display-message` still walks its socket directory and finds a
	// running server (#1733) — so we also redirect tmux.TmuxBin to a
	// non-existent path via isolateHostTmux.
	t.Setenv("TMUX", "")
	isolateHostTmux(t)

	_, listCmd, _ := newMergesTestCmds(t)
	var buf bytes.Buffer
	listCmd.SetOut(&buf)
	listCmd.SetErr(&buf)
	err := runMergesList(listCmd, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine calling session") {
		t.Errorf("error missing 'cannot determine calling session' wording: %q", err.Error())
	}
}

// TestIrisMergesCmd_RegisteredOnRoot asserts both `iris merges` and
// `iris merges list` / `iris merges cancel` are wired into the cobra tree
// at the right place. Cheap structural test that catches accidental
// reshuffles.
func TestIrisMergesCmd_RegisteredOnRoot(t *testing.T) {
	var found bool
	for _, c := range rootCmd.Commands() {
		if c.Use == "merges" {
			found = true
			// Subcommands too.
			subs := map[string]bool{}
			for _, sc := range c.Commands() {
				subs[strings.SplitN(sc.Use, " ", 2)[0]] = true
			}
			if !subs["list"] {
				t.Errorf("missing `list` subcommand on merges")
			}
			if !subs["cancel"] {
				t.Errorf("missing `cancel` subcommand on merges")
			}
			break
		}
	}
	if !found {
		t.Errorf("`merges` command not registered on rootCmd")
	}
}

// --- Negative tests on the DB-file-stat path ---

// TestIrisMerges_StatErrorIsWrapped covers the "stat returned non-ENOENT
// error" branch. We trigger it by pointing testDBPath at a path under an
// unreadable parent directory. Skipped when running as root (which can
// read past 0o000 mode bits).
func TestIrisMerges_StatErrorIsWrapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires non-root to exercise EACCES on stat")
	}
	parent := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(parent, 0o000); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	SetTestDBPath(filepath.Join(parent, "iris.db"))
	t.Cleanup(func() { SetTestDBPath("") })

	_, listCmd, _ := newMergesTestCmds(t)
	var buf bytes.Buffer
	listCmd.SetOut(&buf)
	listCmd.SetErr(&buf)
	err := runMergesList(listCmd, nil)
	if err == nil {
		t.Fatalf("expected stat error, got nil")
	}
	// We don't insist on the exact wording — only that we surface an
	// actionable, prefixed error (not a panic) and that ensureDBExists
	// wrapped it under the canonical 'iris merges list' command prefix.
	if !strings.Contains(err.Error(), "iris merges list") {
		t.Errorf("error missing 'iris merges list' prefix: %q", err.Error())
	}
}
