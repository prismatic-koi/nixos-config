package cmd

// End-to-end seam test for the issue #1947 fix.
//
// Pins the post-reset invariant: given a DB row with a non-empty
// harness_session_id AND a transcript JSONL on disk (bwrap layout), after the
// `prism reset` DB + FS code paths run, neither half of pi_invocation.go's
// `--session <id>` injection trigger remains:
//
//   (1) DB:  CurrentStatus.HarnessSessionID is nil (so cmd/agent_run.go feeds
//       the empty string into container.Config.HarnessSessionID).
//   (2) FS:  the transcript JSONL is gone (so piResolveResumeSession returns
//       false even when called with a stale UUID).
//   (3) PIInvocation: with HarnessSessionID="" the helper short-circuits and
//       does not append --session at all (issue #1838 contract).
//
// Host-mode is pinned separately: it does not consume harness_session_id from
// the DB on the spawn/switch paths (internal/session/session.go:181-182), so
// the reset code path is a no-op for it w.r.t. the resume pointer \u2014 covered
// by TestResetClearPiTranscripts_NoSubtreeUnderHashIsNoOp (no bwrap subtree
// for a host-mode session means nothing is touched).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// TestReset_E2E_NoSessionFlagAfterReset is the AC8 end-to-end invariant:
// reset \u2192 (DB cleared + FS cleared) \u2192 PIInvocation does NOT emit --session.
func TestReset_E2E_NoSessionFlagAfterReset(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	// Some downstream helpers (piResumeSessionsRoot host fallback,
	// SandboxExecStagingHomePath) consult $HOME via os.UserHomeDir(). Pin
	// it to a tempdir too so we never touch the real home.
	t.Setenv("HOME", t.TempDir())

	const (
		sessionName      = "myrepo@feature"
		worktree         = "/home/user/code/myrepo/feature"
		harnessSessionID = "019e00ed-aaaa-bbbb-cccc-deadbeef1947"
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

	// ---- Seed FS (bwrap layout) ----
	// <stateHome>/prism/run/<sessionDirHash>/pi-agent/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl
	hash := prismSession.SessionDirName(sessionName)
	transcriptDir := filepath.Join(stateHome, "prism", "run", hash, "pi-agent", "sessions",
		"--home-user-code-myrepo-feature--")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatalf("mkdir transcriptDir: %v", err)
	}
	transcript := filepath.Join(transcriptDir, "2026-05-22T03-04-05-000Z_"+harnessSessionID+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"session"}`), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	// Plant a sibling file under the hashDir that must survive the reset.
	hashDir := filepath.Join(stateHome, "prism", "run", hash)
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
	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}
	if err := resetClearPiTranscripts(); err != nil {
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

	// (2) FS: transcript JSONL gone, hashDir + sibling preserved.
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Errorf("transcript still present (stat err=%v)", err)
	}
	if _, err := os.Stat(hashDir); err != nil {
		t.Errorf("hashDir removed: %v", err)
	}
	if _, err := os.Stat(siblingPath); err != nil {
		t.Errorf("sibling file removed: %v", err)
	}

	// (3) container.ResolvePIResumeSession returns false post-reset, even
	//     when invoked with the stale UUID. This proves the FS half of the
	//     guard: even if cmd/agent_run.go somehow forwarded a non-empty SID,
	//     PIInvocation would skip --session because the transcript is gone.
	if container.ResolvePIResumeSession(preCfg) {
		t.Errorf("post-reset: ResolvePIResumeSession = true, want false")
	}

	// (4) The realistic post-reset path \u2014 cmd/agent_run.go feeds
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

// TestReset_E2E_HostModeUnchanged pins AC4: host mode is incidentally safe.
// It does not rely on the reset code path because `prism switch` already
// leaves opts.HarnessSessionID empty for host mode (see
// internal/session/session.go:181-182). The test mirrors that contract by
// constructing a host-mode container.Config (no PIAgentConfig* dirs, no
// InstanceID) and verifying PIInvocation does not emit --session even when
// transcripts and a DB SID still exist on disk.
//
// In other words: the reset fix changes bwrap / sandbox-exec behaviour; it
// does NOT regress host mode, which was already correct.
func TestReset_E2E_HostModeUnchanged(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())

	const (
		sessionName      = "host@main"
		worktree         = "/home/user/host-repo/main"
		harnessSessionID = "019e00ed-1111-2222-3333-444444444444"
	)

	// Plant a host-mode transcript at ~/.pi/agent/sessions/<encoded-cwd>/...
	// (resetClearPiTranscripts intentionally does NOT touch this.)
	home := os.Getenv("HOME")
	hostTranscriptDir := filepath.Join(home, ".pi", "agent", "sessions", "--home-user-host-repo-main--")
	if err := os.MkdirAll(hostTranscriptDir, 0o700); err != nil {
		t.Fatalf("mkdir host transcript: %v", err)
	}
	hostTranscript := filepath.Join(hostTranscriptDir, "2026-05-22_"+harnessSessionID+".jsonl")
	if err := os.WriteFile(hostTranscript, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write host transcript: %v", err)
	}

	// Run the FS-side reset.
	if err := resetClearPiTranscripts(); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	// AC4(a): the host-mode transcript is preserved (out of scope).
	if _, err := os.Stat(hostTranscript); err != nil {
		t.Errorf("host-mode transcript was removed by reset; reset must not touch ~/.pi/agent/sessions/: %v", err)
	}

	// AC4(b): the host-mode invocation path that cmd does not feed
	// HarnessSessionID into (mirrors internal/session.buildDirectAgentCmd's
	// opts.HarnessSessionID="" contract). PIInvocation should not emit
	// --session for a Config with empty HarnessSessionID regardless of
	// what is on disk.
	hostCfg := container.Config{
		SessionName: sessionName,
		Worktree:    worktree,
		// HarnessSessionID intentionally empty \u2014 host-mode switch/spawn path.
	}
	args := container.PIInvocation(hostCfg)
	for i, a := range args {
		if a == "--session" {
			t.Errorf("PIInvocation host-mode emitted --session at args[%d]; full args=%v", i, args)
		}
	}
	// Even if a caller did populate the host config with a SID, host mode
	// resolves to ~/.pi/agent/sessions/, so the transcript is still there and
	// resume WOULD succeed (intentional \u2014 the host-mode resume path is the
	// regression guard described in AC5 / #1838).
	hostCfgWithSID := container.Config{
		SessionName:      sessionName,
		Worktree:         worktree,
		HarnessSessionID: harnessSessionID,
	}
	if !container.ResolvePIResumeSession(hostCfgWithSID) {
		t.Errorf("AC5 regression: host-mode resume should still work after reset (the transcript survives, the host path doesn't auto-fetch the SID from the DB)")
	}
	// And if it does get the SID, PIInvocation appends --session per #1838.
	argsWithSID := container.PIInvocation(hostCfgWithSID)
	foundSession := false
	for i, a := range argsWithSID {
		if a == "--session" && i+1 < len(argsWithSID) && argsWithSID[i+1] == harnessSessionID {
			foundSession = true
			break
		}
	}
	if !foundSession {
		t.Errorf("AC5 regression: PIInvocation should append --session %s when host-mode transcript exists; got %v",
			harnessSessionID, argsWithSID)
	}
}
