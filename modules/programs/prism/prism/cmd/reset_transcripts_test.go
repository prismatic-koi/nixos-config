package cmd

// Tests for the `prism reset` transcript-removal code path.
//
// resetClearPiTranscripts deletes exactly the *_<harness_session_id>.jsonl
// files belonging to the snapshotted resume pointers from the shared host pi
// sessions root (~/.pi/agent/sessions/--<encoded-cwd>--/, or
// $PI_CODING_AGENT_DIR/sessions/... when the env var is set). The shared
// root is NEVER swept wholesale: transcripts the DB does not know about —
// other repos' sessions, non-prism pi conversations, sibling sessions on the
// same worktree — must survive a reset untouched.
//
// Test helpers (clearPICodingAgentDir, piEncodeCWD, writeFakePiResumeJSONL)
// are shared with the cleanup-side severance tests in
// cleanup_pi_resume_test.go.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResetClearPiTranscripts_RemovesExactlyTargetedJSONLs is the primary
// test: transcripts are planted for two distinct cwd roots,
// the reset covers only one of them, and the other root is untouched. A
// sibling transcript with a different harness_session_id in the SAME
// encoded-cwd directory must also survive — the removal is per-file, not
// per-directory (matching cleanup's sever granularity).
func TestResetClearPiTranscripts_RemovesExactlyTargetedJSONLs(t *testing.T) {
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	const (
		wtA      = "/home/user/code/repo-a/feature"
		wtB      = "/home/user/code/repo-b/main"
		sidA     = "019e00ed-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		sidB     = "019e00ed-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		sidOther = "019e00ed-cccc-cccc-cccc-cccccccccccc"
	)
	target := writeFakePiResumeJSONL(t, home, wtA, sidA)
	siblingSameCwd := writeFakePiResumeJSONL(t, home, wtA, sidOther)
	otherRoot := writeFakePiResumeJSONL(t, home, wtB, sidB)

	pointers := []piResumePointer{
		{sessionName: "repo-a@feature", worktree: wtA, harnessSessionID: sidA},
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	// The targeted transcript is gone.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("targeted transcript %s was not removed (stat err=%v)", target, err)
	}
	// The sibling transcript in the same cwd dir (different id) survives.
	if _, err := os.Stat(siblingSameCwd); err != nil {
		t.Errorf("sibling transcript %s was removed but must survive: %v", siblingSameCwd, err)
	}
	// The other cwd root is untouched.
	if _, err := os.Stat(otherRoot); err != nil {
		t.Errorf("other-root transcript %s was removed but must survive: %v", otherRoot, err)
	}
}

// TestResetClearPiTranscripts_MultiplePointersAllRemoved verifies that every
// snapshotted pointer is processed: transcripts across two distinct cwd
// roots are both removed when both sessions are being reset.
func TestResetClearPiTranscripts_MultiplePointersAllRemoved(t *testing.T) {
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	const (
		wtA  = "/home/user/code/repo-a/feature"
		wtB  = "/home/user/code/repo-b/main"
		sidA = "019e00ed-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		sidB = "019e00ed-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)
	transcriptA := writeFakePiResumeJSONL(t, home, wtA, sidA)
	transcriptB := writeFakePiResumeJSONL(t, home, wtB, sidB)

	pointers := []piResumePointer{
		{sessionName: "repo-a@feature", worktree: wtA, harnessSessionID: sidA},
		{sessionName: "repo-b@main", worktree: wtB, harnessSessionID: sidB},
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	for _, p := range []string{transcriptA, transcriptB} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("transcript %s was not removed (stat err=%v)", p, err)
		}
	}
}

// TestResetClearPiTranscripts_NoTranscriptsNoError is the edge case:
// `prism reset` on a host with no transcripts completes
// without error, and the sessions root is not materialised as a side
// effect.
func TestResetClearPiTranscripts_NoTranscriptsNoError(t *testing.T) {
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	pointers := []piResumePointer{
		{sessionName: "repo@gone", worktree: "/wt/gone", harnessSessionID: "019e00ed-dddd"},
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts with no transcripts on disk: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, ".pi")); !os.IsNotExist(err) {
		t.Errorf("~/.pi materialised by resetClearPiTranscripts (stat err=%v)", err)
	}
}

// TestResetClearPiTranscripts_EmptyPointerListIsNoOp verifies that with no
// resume pointers recorded in the DB, the FS half does nothing: no error,
// and existing transcripts on the shared root are untouched.
func TestResetClearPiTranscripts_EmptyPointerListIsNoOp(t *testing.T) {
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	untracked := writeFakePiResumeJSONL(t, home, "/wt/untracked", "019e00ed-eeee")

	if err := resetClearPiTranscripts(nil); err != nil {
		t.Fatalf("resetClearPiTranscripts(nil): %v", err)
	}

	if _, err := os.Stat(untracked); err != nil {
		t.Errorf("untracked transcript %s was removed by an empty-pointer reset: %v", untracked, err)
	}
}

// TestResetClearPiTranscripts_BlankPointerFieldsSkipped is defence-in-depth
// for the container.RemovePiResumeJSONL caller contract: pointers with an
// empty worktree or empty harness_session_id are silently skipped (nothing
// to scope to), without error and without touching unrelated transcripts.
// resetMarkDBEnded filters such rows out of the snapshot, so this guards the
// underlying helper's behaviour should that filter ever regress.
func TestResetClearPiTranscripts_BlankPointerFieldsSkipped(t *testing.T) {
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	bystander := writeFakePiResumeJSONL(t, home, "/wt/bystander", "019e00ed-ffff")

	pointers := []piResumePointer{
		{sessionName: "repo@no-worktree", worktree: "", harnessSessionID: "019e00ed-1111"},
		{sessionName: "repo@no-sid", worktree: "/wt/bystander", harnessSessionID: ""},
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts with blank-field pointers: %v", err)
	}

	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("bystander transcript %s was removed: %v", bystander, err)
	}
}

// TestResetClearPiTranscripts_DeadLayoutsNotSwept pins that reset does NOT
// sweep the dead-layout trees: $XDG_STATE_HOME/prism/run/<hash>/pi-agent/
// sessions/ (an old bwrap layout) and
// $XDG_STATE_HOME/prism/sessions/<instanceID>/home/.pi/agent/sessions/
// (a sandbox-exec staging HOME). Both trees are planted here and must survive
// a reset that DOES remove a targeted shared-root transcript — proving the
// test is not vacuous and that reset does not walk the prism state root at
// all.
func TestResetClearPiTranscripts_DeadLayoutsNotSwept(t *testing.T) {
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	// Dead layout 1: an old bwrap run-dir layout.
	bwrapLegacy := filepath.Join(stateHome, "prism", "run", "abc123def456", "pi-agent", "sessions", "--wt--")
	if err := os.MkdirAll(bwrapLegacy, 0o700); err != nil {
		t.Fatalf("mkdir bwrap legacy: %v", err)
	}
	bwrapLegacyFile := filepath.Join(bwrapLegacy, "2026-01-02_legacy.jsonl")
	if err := os.WriteFile(bwrapLegacyFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write bwrap legacy: %v", err)
	}

	// Dead layout 2: sandbox-exec staging HOME.
	stagingLegacy := filepath.Join(stateHome, "prism", "sessions", "11111111-2222-3333-4444-555555555555",
		"home", ".pi", "agent", "sessions", "--wt--")
	if err := os.MkdirAll(stagingLegacy, 0o700); err != nil {
		t.Fatalf("mkdir staging legacy: %v", err)
	}
	stagingLegacyFile := filepath.Join(stagingLegacy, "2026-01-02_legacy.jsonl")
	if err := os.WriteFile(stagingLegacyFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write staging legacy: %v", err)
	}

	// A live shared-root transcript targeted by the reset (non-vacuity
	// anchor: the function must do real work in this run).
	const (
		wt  = "/home/user/code/repo/main"
		sid = "019e00ed-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	)
	target := writeFakePiResumeJSONL(t, home, wt, sid)

	pointers := []piResumePointer{
		{sessionName: "repo@main", worktree: wt, harnessSessionID: sid},
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	// The shared-root target is gone (the function ran for real)…
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("targeted transcript %s was not removed (stat err=%v)", target, err)
	}
	// …and both dead-layout trees are untouched.
	if _, err := os.Stat(bwrapLegacyFile); err != nil {
		t.Errorf("pre-#1985 bwrap layout was swept but the sweep should be gone: %v", err)
	}
	if _, err := os.Stat(stagingLegacyFile); err != nil {
		t.Errorf("sandbox-exec staging HOME layout was swept but the sweep should be gone: %v", err)
	}
}
