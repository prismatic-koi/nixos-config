package container

// pi_invocation_remove_resume_test.go — issue #2035.
//
// Verifies RemovePiResumeJSONL: the FS-side companion to
// db.ClearHarnessSessionID called from `prism cleanup` so that a re-spawn on
// the same branch name does not resume the cleaned session's pi conversation
// via a leftover JSONL transcript in ~/.pi/agent/sessions/<encoded-cwd>/.
//
// All tests use t.TempDir() + t.Setenv("HOME", ...) so they never touch the
// host's real ~/.pi/agent/sessions/.
//
// Post-#2185 piResumeSessionsRoot honours PI_CODING_AGENT_DIR; these tests
// clear that env var so they exercise the home-fallback branch
// deterministically (the developer host sets it system-wide).

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemovePiResumeJSONL_RemovesMatchingFile verifies the positive case:
// a JSONL file matching `*_<HarnessSessionID>.jsonl` under the worktree's
// encoded-cwd directory is removed.
func TestRemovePiResumeJSONL_RemovesMatchingFile(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"
	const harnessSessionID = "019e72d2-446a-712f-baea-7abc9e7ce7df"

	path := writeBwrapResumeSession(t, sessionName, worktree, harnessSessionID)

	// Pre-condition.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-condition: stat %s: %v", path, err)
	}

	cfg := bwrapResumeCfg(sessionName, worktree, harnessSessionID)
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Fatalf("RemovePiResumeJSONL: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file %s still exists after RemovePiResumeJSONL (err=%v); want removed", path, err)
	}
}

// TestRemovePiResumeJSONL_SandboxExec_RemovesFromHostRoot is the #2210
// regression guard for cleanup: for a sandbox-exec-shaped config, the
// transcript JSONL lives at the HOST sessions root (pi writes there because
// the dispatcher injects PI_CODING_AGENT_DIR into the sandbox env), and
// RemovePiResumeJSONL must remove it from there. Pre-#2210 the resolver
// pointed at the per-session staging HOME, making the removal a silent no-op
// that left dead transcripts accumulating under the host root.
func TestRemovePiResumeJSONL_SandboxExec_RemovesFromHostRoot(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@sbx-cleanup"
	const worktree = "/Users/user/code/myrepo/sbx-cleanup"
	const harnessSessionID = "019e2210-cccc-dddd-eeee-ffff00001111"
	const instanceID = "12342210-5678-90ab-cdef-001122334455"

	// Plant the transcript at the host root (~/.pi/agent/sessions/… with the
	// temp HOME) — where pi actually writes for sandbox-exec sessions.
	path := writeBwrapResumeSession(t, sessionName, worktree, harnessSessionID)

	// Pre-condition.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-condition: stat %s: %v", path, err)
	}

	cfg := sandboxExecResumeCfg(sessionName, worktree, harnessSessionID, instanceID)
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Fatalf("RemovePiResumeJSONL (sandbox-exec): %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("host-root transcript %s still exists after RemovePiResumeJSONL for a sandbox-exec config (err=%v); want removed (#2210)",
			path, err)
	}
}

// TestRemovePiResumeJSONL_LeavesSiblingTranscriptsAlone verifies the targeted
// suffix match: a JSONL belonging to a DIFFERENT harness_session_id under the
// same encoded-cwd directory must not be touched.
//
// This protects the legitimate-resume case (issue #1838) and any other
// session that happens to share a worktree path: cleanup must only delete
// the specific transcript bound to the cleaned session's harness_session_id.
func TestRemovePiResumeJSONL_LeavesSiblingTranscriptsAlone(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"
	const cleanedID = "019e0000-1111-1111-1111-111111111111"
	const survivorID = "019e0000-2222-2222-2222-222222222222"

	cleanedPath := writeBwrapResumeSession(t, sessionName, worktree, cleanedID)
	survivorPath := writeBwrapResumeSession(t, sessionName, worktree, survivorID)

	cfg := bwrapResumeCfg(sessionName, worktree, cleanedID)
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Fatalf("RemovePiResumeJSONL: %v", err)
	}

	if _, err := os.Stat(cleanedPath); !os.IsNotExist(err) {
		t.Errorf("cleaned file %s still exists; want removed", cleanedPath)
	}
	if _, err := os.Stat(survivorPath); err != nil {
		t.Errorf("survivor file %s missing (err=%v); want preserved", survivorPath, err)
	}
}

// TestRemovePiResumeJSONL_MissingDirIsNoOp verifies that calling against a
// worktree whose encoded-cwd directory does not exist (fresh-session-then-cleanup
// scenario) returns nil — cleanup must remain best-effort.
func TestRemovePiResumeJSONL_MissingDirIsNoOp(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	cfg := bwrapResumeCfg(
		"myrepo@never-ran",
		"/home/user/code/myrepo/never-ran",
		"019e0000-3333-3333-3333-333333333333",
	)
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Errorf("RemovePiResumeJSONL on missing dir: %v, want nil", err)
	}
}

// TestRemovePiResumeJSONL_EmptyHarnessSessionIDIsNoOp verifies the caller-
// contract guard: when HarnessSessionID is empty there is nothing to scope
// to, and the function returns nil without scanning the filesystem.
func TestRemovePiResumeJSONL_EmptyHarnessSessionIDIsNoOp(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"

	// Plant a decoy file that would match if the suffix matching were lax.
	decoyPath := writeBwrapResumeSession(t, sessionName, worktree, "019e0000-dddd-dddd-dddd-dddddddddddd")

	cfg := bwrapResumeCfg(sessionName, worktree, "")
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Errorf("RemovePiResumeJSONL with empty HarnessSessionID: %v, want nil", err)
	}
	if _, err := os.Stat(decoyPath); err != nil {
		t.Errorf("decoy file %s was removed when HarnessSessionID was empty; the guard failed", decoyPath)
	}
}

// TestRemovePiResumeJSONL_EmptyWorktreeIsNoOp verifies that an empty Worktree
// is also a silent no-op — the function cannot resolve an encoded-cwd dir
// without a worktree path.
func TestRemovePiResumeJSONL_EmptyWorktreeIsNoOp(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	cfg := Config{
		SessionName:             "myrepo@feature",
		Worktree:                "",
		HarnessSessionID:        "019e0000-eeee-eeee-eeee-eeeeeeeeeeee",
		PIAgentConfigHostDir:    "/tmp/host-stage",
		PIAgentConfigSandboxDir: "/run/prism/pi-agent",
	}
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Errorf("RemovePiResumeJSONL with empty Worktree: %v, want nil", err)
	}
}

// TestRemovePiResumeJSONL_DoesNotDescendIntoSubdirs verifies that a directory
// whose name happens to end in `_<HarnessSessionID>.jsonl` is not removed
// (the suffix match must be restricted to regular files).
func TestRemovePiResumeJSONL_DoesNotDescendIntoSubdirs(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	const worktree = "/home/user/code/myrepo/feature"
	const harnessSessionID = "019e0000-ffff-ffff-ffff-ffffffffffff"

	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", encodePiCWD(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pathological: a directory whose name happens to match the suffix.
	pathologicalDir := filepath.Join(dir, "trap_"+harnessSessionID+".jsonl")
	if err := os.MkdirAll(pathologicalDir, 0o700); err != nil {
		t.Fatalf("mkdir pathological: %v", err)
	}

	cfg := bwrapResumeCfg("myrepo@feature", worktree, harnessSessionID)
	if err := RemovePiResumeJSONL(cfg); err != nil {
		t.Errorf("RemovePiResumeJSONL: %v", err)
	}
	if _, err := os.Stat(pathologicalDir); err != nil {
		t.Errorf("pathological subdir %s was removed; want preserved (must skip non-regular entries)", pathologicalDir)
	}
}
