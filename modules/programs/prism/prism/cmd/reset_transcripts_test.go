package cmd

// Tests for the issue #1947 reset transcript-wipe code path.
//
// resetClearPiTranscripts removes the per-session pi-agent transcript JSONL
// subtree under each per-session run directory (bwrap layout) and under each
// sandbox-exec instance staging HOME. The enclosing per-session directory
// must be preserved \u2014 only the inner `.../sessions/` subtree is removed.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResetClearPiTranscripts_RemovesBwrapSubtreePreservesParent is the
// primary AC2 test for the bwrap layout:
//
//   <stateHome>/prism/run/<sessionDirHash>/pi-agent/sessions/...
//
// After resetClearPiTranscripts, the inner `pi-agent/sessions/` subtree is
// gone, but the enclosing <sessionDirHash>/ directory is preserved (other
// state like agent-run.log, *-sidecar.pid, hostapi.sock may live there).
func TestResetClearPiTranscripts_RemovesBwrapSubtreePreservesParent(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	// Synthesise two per-session dirs under prism/run/ with the canonical layout:
	//   <hash>/pi-agent/sessions/--<encoded-cwd>--/<ts>_<uuid>.jsonl
	// and one sibling file under <hash>/ that must be preserved.
	hashes := []string{"abc123def456", "feedfacecafe"}
	for _, h := range hashes {
		hashDir := filepath.Join(stateHome, "prism", "run", h)
		// Plant a sibling file that MUST be preserved.
		if err := os.MkdirAll(hashDir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", hashDir, err)
		}
		sibling := filepath.Join(hashDir, "agent-run.log")
		if err := os.WriteFile(sibling, []byte("log content"), 0o600); err != nil {
			t.Fatalf("write sibling: %v", err)
		}
		// Plant the transcript subtree to be removed.
		transcriptDir := filepath.Join(hashDir, "pi-agent", "sessions", "--home-u-repo--")
		if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
			t.Fatalf("mkdir transcripts %s: %v", transcriptDir, err)
		}
		transcript := filepath.Join(transcriptDir, "2026-05-22_019e00ed-1234.jsonl")
		if err := os.WriteFile(transcript, []byte(`{"type":"session"}`), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}

	if err := resetClearPiTranscripts(); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	for _, h := range hashes {
		hashDir := filepath.Join(stateHome, "prism", "run", h)

		// (a) The hashDir itself MUST still exist (other state may live here).
		if _, err := os.Stat(hashDir); err != nil {
			t.Errorf("sessionDirHash %q was removed but must be preserved: %v", h, err)
		}

		// (b) The sibling file under hashDir MUST still exist.
		sibling := filepath.Join(hashDir, "agent-run.log")
		if _, err := os.Stat(sibling); err != nil {
			t.Errorf("sibling file %s under %q was removed: %v", sibling, h, err)
		}

		// (c) The pi-agent/sessions/ subtree MUST be gone.
		subtree := filepath.Join(hashDir, "pi-agent", "sessions")
		if _, err := os.Stat(subtree); !os.IsNotExist(err) {
			t.Errorf("pi-agent/sessions/ subtree under %q was not removed (stat err=%v)", h, err)
		}
	}
}

// TestResetClearPiTranscripts_RemovesSandboxExecSubtree exercises the
// sandbox-exec layout:
//
//   <stateHome>/prism/sessions/<instanceID>/home/.pi/agent/sessions/...
//
// The `<instanceID>/home/` directory is preserved (the staging HOME root); only
// the `.pi/agent/sessions/` subtree under it is removed.
func TestResetClearPiTranscripts_RemovesSandboxExecSubtree(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	const instanceID = "11111111-2222-3333-4444-555555555555"
	homeDir := filepath.Join(stateHome, "prism", "sessions", instanceID, "home")
	sessionsDir := filepath.Join(homeDir, ".pi", "agent", "sessions", "--home-u-repo--")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatalf("mkdir sessionsDir: %v", err)
	}
	transcript := filepath.Join(sessionsDir, "2026-05-22_uuid.jsonl")
	if err := os.WriteFile(transcript, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	// Plant a sibling file in the staging HOME that MUST be preserved.
	sibling := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(sibling, []byte("[user]"), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	if err := resetClearPiTranscripts(); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	// The staging HOME root must remain.
	if _, err := os.Stat(homeDir); err != nil {
		t.Errorf("staging HOME %s removed but must be preserved: %v", homeDir, err)
	}
	// The sibling file must remain.
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling .gitconfig removed: %v", err)
	}
	// The .pi/agent/sessions subtree must be gone.
	wiped := filepath.Join(homeDir, ".pi", "agent", "sessions")
	if _, err := os.Stat(wiped); !os.IsNotExist(err) {
		t.Errorf(".pi/agent/sessions/ not removed (stat err=%v)", err)
	}
}

// TestResetClearPiTranscripts_MissingDirsAreNoOp verifies the defensive
// no-op contract: when neither prism/run/ nor prism/sessions/ exists,
// resetClearPiTranscripts returns nil and does not create them.
func TestResetClearPiTranscripts_MissingDirsAreNoOp(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	if err := resetClearPiTranscripts(); err != nil {
		t.Fatalf("resetClearPiTranscripts on empty stateHome: %v", err)
	}

	// Neither directory should have been created as a side effect.
	for _, d := range []string{
		filepath.Join(stateHome, "prism", "run"),
		filepath.Join(stateHome, "prism", "sessions"),
	} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("directory %s materialised by resetClearPiTranscripts (err=%v)", d, err)
		}
	}
}

// TestResetClearPiTranscripts_NoSubtreeUnderHashIsNoOp verifies that a
// <sessionDirHash> directory without a pi-agent/sessions/ subtree (e.g. a
// host-mode session, or a fresh dir with only agent-run.log) is left alone
// without error.
func TestResetClearPiTranscripts_NoSubtreeUnderHashIsNoOp(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	hashDir := filepath.Join(stateHome, "prism", "run", "deadbeefcafe")
	if err := os.MkdirAll(hashDir, 0o700); err != nil {
		t.Fatalf("mkdir hashDir: %v", err)
	}
	keep := filepath.Join(hashDir, "agent-run.log")
	if err := os.WriteFile(keep, []byte("log"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	if err := resetClearPiTranscripts(); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	if _, err := os.Stat(hashDir); err != nil {
		t.Errorf("hashDir removed: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("keep file removed: %v", err)
	}
}

// TestResetClearPiTranscripts_NonDirChildIsSkipped guards against a stray
// regular file under prism/run/ being interpreted as a session dir. The
// helper must filepath.Join(parent, ...) → stat → skip without erroring.
func TestResetClearPiTranscripts_NonDirChildIsSkipped(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	runDir := filepath.Join(stateHome, "prism", "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir runDir: %v", err)
	}
	stray := filepath.Join(runDir, "stray-file.txt")
	if err := os.WriteFile(stray, []byte("oops"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	if err := resetClearPiTranscripts(); err != nil {
		t.Fatalf("resetClearPiTranscripts: %v", err)
	}

	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file removed unexpectedly: %v", err)
	}
}
