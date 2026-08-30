package cmd

// End-to-end seam test for the reset resume-pointer clear.
//
// Pins the post-reset invariant: given a DB row with a non-empty
// harness_session_id AND a transcript JSONL on disk, after the
// `prism reset` DB + FS code paths run, the `--session <id>` injection
// trigger in pi_invocation.go no longer fires — on BOTH surfaces.
//
// pi writes its transcripts to the shared host sessions root
// (~/.pi/agent/sessions/--<encoded-cwd>--/, or
// $PI_CODING_AGENT_DIR/sessions/...) in every isolation mode. `prism reset`
// snapshots the (worktree, harness_session_id) pairs from the DB before
// clearing them and deletes exactly those *_<id>.jsonl files from the shared
// root — per-file, never a wholesale sweep.
//
// Invariants pinned here:
//
//   (1) DB:  CurrentStatus.HarnessSessionID is nil (so cmd/agent_run.go feeds
//       the empty string into container.Config.HarnessSessionID).
//   (2) FS:  the transcript JSONL for the reset session is REMOVED from the
//       shared root ("reset means forget"); transcripts the DB does not know
//       about — sibling ids on the same worktree, other cwd roots — survive.
//   (3) PIInvocation: with HarnessSessionID="" the helper short-circuits and
//       does not append --session; and even with a
//       stale id, ResolvePIResumeSession finds no transcript, so the flag is
//       not emitted either — the reset guard is now double-layered.
//
// Transcripts with no corresponding DB resume pointer (e.g. non-prism pi
// conversations) are pinned separately by
// TestReset_E2E_UntrackedTranscriptsSurvive below.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// TestReset_E2E_NoSessionFlagAfterReset is the end-to-end invariant:
// reset → (DB cleared + targeted FS removal) → PIInvocation does NOT emit
// --session.
func TestReset_E2E_NoSessionFlagAfterReset(t *testing.T) {
	clearPICodingAgentDir(t)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	// Downstream helpers (piResumeSessionsRoot host fallback) consult $HOME
	// via os.UserHomeDir(). Pin it to a tempdir so we never touch the real
	// home.
	t.Setenv("HOME", t.TempDir())

	const (
		sessionName      = "myrepo@feature"
		worktree         = "/home/user/code/myrepo/feature"
		harnessSessionID = "019e00ed-aaaa-bbbb-cccc-deadbeef1947"
		siblingSessionID = "019e00ed-1111-2222-3333-aaaabbbbcccc"
		otherWorktree    = "/home/user/code/otherrepo/main"
		otherSessionID   = "019e00ed-4444-5555-6666-777788889999"
	)

	// ---- Seed DB ----
	dbFile := filepath.Join(stateHome, "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := d.UpsertStatus(sessionName, "myrepo", worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpdateHarnessSessionID(sessionName, harnessSessionID); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}
	// Pre-condition: DB has the SID.
	preStatus, _ := d.CurrentStatus(sessionName)
	if preStatus == nil || preStatus.HarnessSessionID == nil || *preStatus.HarnessSessionID != harnessSessionID {
		t.Fatalf("pre-condition: DB HarnessSessionID = %v, want %q", preStatus, harnessSessionID)
	}
	d.Close()

	// ---- Seed FS (shared host root: ~/.pi/agent/sessions/) ----
	home := os.Getenv("HOME")
	// The transcript belonging to the session being reset — must be removed.
	transcript := writeFakePiResumeJSONL(t, home, worktree, harnessSessionID)
	// A sibling transcript in the SAME encoded-cwd dir with a different id
	// (no DB row) — must survive: removal is per-file, not per-directory.
	sibling := writeFakePiResumeJSONL(t, home, worktree, siblingSessionID)
	// A transcript under a DIFFERENT cwd root with no DB row — must survive.
	otherRoot := writeFakePiResumeJSONL(t, home, otherWorktree, otherSessionID)

	// Plant a sibling file under the per-prism-session hash dir that must
	// survive the reset — the run/<hash>/ subtree holds agent-run.log,
	// hostapi.sock, sidecar.pid etc., and reset's transcript step no longer
	// walks the prism state root at all.
	hash := prismSession.SessionDirName(sessionName)
	hashDir := filepath.Join(stateHome, "prism", "run", hash)
	if err := os.MkdirAll(hashDir, 0o700); err != nil {
		t.Fatalf("mkdir hashDir: %v", err)
	}
	siblingPath := filepath.Join(hashDir, "agent-run.log")
	if err := os.WriteFile(siblingPath, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	// Pre-condition: piResolveResumeSession would succeed right now.
	preCfg := container.Config{
		SessionName:             sessionName,
		Worktree:                worktree,
		HarnessSessionID:        harnessSessionID,
		PIAgentConfigHostDir:    "/some/host/stage",
		PIAgentConfigSandboxDir: "/run/prism/pi-agent",
	}
	if !container.ResolvePIResumeSession(preCfg) {
		t.Fatalf("pre-condition: ResolvePIResumeSession = false, want true (transcript should be discoverable)")
	}

	// ---- Run the reset code paths ----
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	pointers, err := resetMarkDBEnded()
	if err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	// ---- Post-conditions ----

	// (1) DB: HarnessSessionID is now nil.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	postStatus, _ := d2.CurrentStatus(sessionName)
	if postStatus == nil {
		t.Fatalf("CurrentStatus post-reset: row missing")
	}
	if postStatus.HarnessSessionID != nil {
		t.Errorf("post-reset DB HarnessSessionID = %q, want nil",
			*postStatus.HarnessSessionID)
	}
	// And the row was marked ended.
	if postStatus.EndedAt == nil {
		t.Errorf("post-reset DB EndedAt = nil, want non-nil")
	}

	// (2) FS: the reset session's transcript is REMOVED from the shared root
	//     (reset means forget, scoped per-id). Transcripts the
	//     DB does not know about survive, as do the run/<hash>/ files.
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Errorf("reset session's transcript was not removed (stat err=%v)", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling transcript (different id, same cwd) was removed but must survive: %v", err)
	}
	if _, err := os.Stat(otherRoot); err != nil {
		t.Errorf("other-root transcript was removed but must survive: %v", err)
	}
	if _, err := os.Stat(hashDir); err != nil {
		t.Errorf("hashDir removed: %v", err)
	}
	if _, err := os.Stat(siblingPath); err != nil {
		t.Errorf("sibling file removed: %v", err)
	}

	// (3) Even a caller holding the STALE id can no longer resume: the
	//     transcript is gone, so ResolvePIResumeSession returns false and
	//     PIInvocation omits --session. This is the FS-side guard.
	if container.ResolvePIResumeSession(preCfg) {
		t.Errorf("post-reset: ResolvePIResumeSession = true, want false (transcript must be gone)")
	}
	staleCfg := preCfg
	staleCfg.InitialPrompt = "hello after reset"
	for i, a := range container.PIInvocation(staleCfg) {
		if a == "--session" {
			t.Errorf("PIInvocation with stale SID emitted --session at args[%d] post-reset", i)
		}
	}

	// (4) The realistic post-reset path — cmd/agent_run.go feeds
	//     status.HarnessSessionID into container.Config. With the DB row now
	//     carrying nil, that lowers to "". PIInvocation must NOT contain
	//     --session in its args.
	postCfg := container.Config{
		SessionName:             sessionName,
		Worktree:                worktree,
		HarnessSessionID:        "", // mirrors what cmd/agent_run.go would set
		PIAgentConfigHostDir:    "/some/host/stage",
		PIAgentConfigSandboxDir: "/run/prism/pi-agent",
		InitialPrompt:           "hello after reset",
	}
	args := container.PIInvocation(postCfg)
	for i, a := range args {
		if a == "--session" {
			t.Errorf("PIInvocation post-reset emitted --session at args[%d]; full args=%v", i, args)
		}
	}
	// Sanity: the InitialPrompt still rides as the final positional.
	if len(args) == 0 || args[len(args)-1] != "hello after reset" {
		t.Errorf("PIInvocation args missing trailing initial prompt; got %v", args)
	}
}

// TestReset_E2E_UntrackedTranscriptsSurvive pins the shared-root safety
// property: transcripts with NO corresponding resume
// pointer in prism's DB — non-prism pi conversations, other repos' sessions,
// sessions whose linkage was already severed at cleanup — survive a reset
// untouched. The removal is keyed off DB rows, so an empty DB means zero FS
// deletions.
//
// It also pins the resume regression guard: because the untracked
// transcript survives, a caller that (re)acquires the id can still resume —
// reset must not break the legitimate resume path for conversations it does
// not own.
func TestReset_E2E_UntrackedTranscriptsSurvive(t *testing.T) {
	clearPICodingAgentDir(t)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())

	const (
		sessionName      = "host@main"
		worktree         = "/home/user/host-repo/main"
		harnessSessionID = "019e00ed-1111-2222-3333-444444444444"
	)

	// Plant a transcript at the shared root with NO DB row referencing it
	// (e.g. a pi conversation started outside prism).
	home := os.Getenv("HOME")
	untracked := writeFakePiResumeJSONL(t, home, worktree, harnessSessionID)

	// Run both reset halves against an empty DB: the snapshot is empty, so
	// the FS half must be a no-op.
	dbFile := filepath.Join(stateHome, "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	pointers, err := resetMarkDBEnded()
	if err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}
	if len(pointers) != 0 {
		t.Fatalf("snapshot from empty DB has %d pointer(s), want 0: %+v", len(pointers), pointers)
	}
	if err := resetClearPiTranscripts(pointers); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	// The untracked transcript is preserved.
	if _, err := os.Stat(untracked); err != nil {
		t.Errorf("untracked transcript was removed by reset; reset must only delete transcripts it has resume pointers for: %v", err)
	}

	// Regression guard: the surviving transcript is still resumable by
	// a caller that holds the id — PIInvocation appends --session for it.
	cfgWithSID := container.Config{
		SessionName:      sessionName,
		Worktree:         worktree,
		HarnessSessionID: harnessSessionID,
	}
	if !container.ResolvePIResumeSession(cfgWithSID) {
		t.Errorf("resume should still work for an untracked transcript after reset")
	}
	argsWithSID := container.PIInvocation(cfgWithSID)
	foundSession := false
	for i, a := range argsWithSID {
		if a == "--session" && i+1 < len(argsWithSID) && argsWithSID[i+1] == harnessSessionID {
			foundSession = true
			break
		}
	}
	if !foundSession {
		t.Errorf("PIInvocation should append --session %s when the transcript exists; got %v",
			harnessSessionID, argsWithSID)
	}

	// And the empty-HarnessSessionID path (what `prism switch` produces for
	// host mode) still never emits --session regardless of what is on disk.
	hostCfg := container.Config{
		SessionName: sessionName,
		Worktree:    worktree,
	}
	for i, a := range container.PIInvocation(hostCfg) {
		if a == "--session" {
			t.Errorf("PIInvocation with empty HarnessSessionID emitted --session at args[%d]", i)
		}
	}
}
