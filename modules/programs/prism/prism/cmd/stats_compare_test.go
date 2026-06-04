package cmd

// Renderer-level tests for `prism stats compare` (issue #2102). These drive the
// real command runner (runStatsCompare → runComparison → renderCompareTable)
// against a seeded temp DB and assert on the captured table output:
//
//   - a terminal-state session that has NOT been cleaned up still renders full
//     aggregate axes (Layer 1, on-the-fly compute);
//   - an in-progress (active) session renders "—" for its aggregate axes
//     (negative / over-broad-fix guard);
//   - the Spawn Inputs block is populated from spawn_inputs rather than the old
//     "not yet populated … C.1" placeholder (Layer 2, renderer fix).

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// newCompareTestCmd builds a *cobra.Command with the same flag set the real
// `prism stats compare` registers, so runStatsCompare can read them.
func newCompareTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "compare", RunE: runStatsCompare}
	c.Flags().StringSlice("axes", nil, "")
	c.Flags().String("format", "table", "")
	c.Flags().Bool("diff-only", false, "")
	c.Flags().String("sort", "", "")
	c.Flags().Bool("include-inputs", false, "")
	c.Flags().Bool("include-rubric", false, "")
	return c
}

// seedCompareSession seeds a pre-cleanup session: sessions row (no end_state),
// token/tool/error events, a spawn_inputs row (profile + effective isolation),
// and an agent_status row in the given state linked by instance_id.
func seedCompareSession(t *testing.T, d *db.DB, state, profile, isolation string) string {
	t.Helper()
	iid := uuid.New().String()
	sessName := "prism-test@cmp-" + iid[:8]
	startedAt := time.Now().Add(-8 * time.Minute)

	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessName,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/" + iid[:8],
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	writeStatsEventIID(t, d, sessName, iid, "msg_assistant",
		`{"inputTokens":120,"outputTokens":60,"cacheReadTokens":10,"cacheWriteTokens":5,"cost":0.0015}`,
		startedAt.Add(1*time.Minute))
	writeStatsEventIID(t, d, sessName, iid, "tool_call", `{"name":"bash"}`, startedAt.Add(70*time.Second))
	writeStatsEventIID(t, d, sessName, iid, "tool_error", `{"name":"bash"}`, startedAt.Add(80*time.Second))

	si := db.SpawnInputs{InstanceID: iid, CreatedAt: startedAt.UnixMilli()}
	if profile != "" {
		si.ProfileName = &profile
	}
	if isolation != "" {
		si.IsolationFlag = &isolation
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	if err := d.UpsertStatus(sessName, "testrepo", "/code/testrepo/"+iid[:8], state, nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", state, err)
	}
	if err := d.SetInstanceID(sessName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	return iid
}

// writeStatsEventIID writes an instance_id-linked event.
func writeStatsEventIID(t *testing.T, d *db.DB, sess, iid, typ, payload string, ts time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sess,
		InstanceID:  &iid,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/wt",
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}); err != nil {
		t.Fatalf("WriteEvent(%s): %v", typ, err)
	}
}

// TestRunStatsCompare_TerminalSessionRendersAggregates verifies the headline
// Layer-1 outcome: two finished-but-not-cleaned-up sessions render populated
// aggregate axes in the table (not "—").
func TestRunStatsCompare_TerminalSessionRendersAggregates(t *testing.T) {
	d := openStatsTestDB(t)
	iidA := seedCompareSession(t, d, "finished", "anthropic", "bwrap")
	iidB := seedCompareSession(t, d, "finished", "gemini", "bwrap")

	cmd := newCompareTestCmd()
	out := captureStdout(t, func() {
		if err := runStatsCompare(cmd, []string{iidA, iidB}); err != nil {
			t.Fatalf("runStatsCompare: %v", err)
		}
	})

	// The per-axis aggregations section must carry real numbers. tool_call=1,
	// tool_error=1, msg_assistant=1 per leg; tokens populated.
	for _, want := range []string{
		"Per-axis aggregations",
		"tokens_input",
		"tokens_output",
		"cost_usd",
		"end_state",
		"finished",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compare output missing %q; got:\n%s", want, out)
		}
	}

	// A finished session must not render the whole aggregations block as dashes.
	// Find the tokens_input row and assert it isn't "—".
	if line := lineContaining(out, "tokens_input:"); line == "" {
		t.Fatalf("no tokens_input row in output:\n%s", out)
	} else if !strings.Contains(line, "120") {
		t.Errorf("tokens_input row should show 120 per leg, got: %q", line)
	}
}

// TestRunStatsCompare_ActiveSessionRendersDashes is the negative test: an
// in-progress session must render "—" for its aggregate axes, not stale data.
func TestRunStatsCompare_ActiveSessionRendersDashes(t *testing.T) {
	d := openStatsTestDB(t)
	iidA := seedCompareSession(t, d, "active", "anthropic", "bwrap")
	iidB := seedCompareSession(t, d, "active", "gemini", "bwrap")

	cmd := newCompareTestCmd()
	out := captureStdout(t, func() {
		if err := runStatsCompare(cmd, []string{iidA, iidB}); err != nil {
			t.Fatalf("runStatsCompare: %v", err)
		}
	})

	// tokens_input must be all dashes for active sessions.
	line := lineContaining(out, "tokens_input:")
	if line == "" {
		t.Fatalf("no tokens_input row in output:\n%s", out)
	}
	if strings.Contains(line, "120") {
		t.Errorf("active session leaked aggregate data into tokens_input: %q", line)
	}
	if !strings.Contains(line, "—") {
		t.Errorf("active session tokens_input should be '—', got: %q", line)
	}
}

// TestRunStatsCompare_SpawnInputsBlockPopulated verifies Layer 2: the Spawn
// Inputs block surfaces the captured spawn_inputs columns and no longer prints
// the "not yet populated … C.1" placeholder.
func TestRunStatsCompare_SpawnInputsBlockPopulated(t *testing.T) {
	d := openStatsTestDB(t)
	iidA := seedCompareSession(t, d, "finished", "anthropic", "bwrap")
	iidB := seedCompareSession(t, d, "finished", "gemini", "sandbox-exec")

	cmd := newCompareTestCmd()
	out := captureStdout(t, func() {
		if err := runStatsCompare(cmd, []string{iidA, iidB}); err != nil {
			t.Fatalf("runStatsCompare: %v", err)
		}
	})

	if strings.Contains(out, "not yet populated") || strings.Contains(out, "C.1") {
		t.Errorf("stale placeholder still present in Spawn Inputs block:\n%s", out)
	}
	for _, want := range []string{
		"Spawn Inputs",
		"profile_name",
		"anthropic",
		"gemini",
		"isolation_mode",
		"bwrap",
		"sandbox-exec",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Spawn Inputs block missing %q; got:\n%s", want, out)
		}
	}
}

// lineContaining returns the first line of s that contains sub, or "".
func lineContaining(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
