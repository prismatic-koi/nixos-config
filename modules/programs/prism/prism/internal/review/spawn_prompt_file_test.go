package review_test

// spawn_prompt_file_test.go — integration test for the review-agent prompt-file
// delivery path (issue #1195 / regression of #1092).
//
// These tests synthesise a >100 KiB review context (the prompt that Run/RunAsync
// builds for each review agent) and verify that session.SpawnSession:
//
//  (a) succeeds — the launch command is within HostLaunchCmdSafeBound (16 KiB)
//  (b) the constructed tmux new-window argv does NOT contain the prompt body
//      inline (no -e PRISM_INITIAL_PROMPT=<huge>) — the file-based path is used
//  (c) the agent receives the full prompt content via the initial-prompt file
//
// Test structure:
//   - A fake tmux binary (shell script) that records its argv to a file is
//     installed for the duration of each test via tmux.TmuxBin.
//   - PRISM_TEST_SUBPROCESS=1 makes the StartSidecar call use the test binary
//     as a stub sidecar (exits after 50ms — see TestMain in the session package
//     which owns the PRISM_TEST_SUBPROCESS handling).
//   - XDG_STATE_HOME is redirected to a temp dir so no real state files are
//     written to the user's home directory.
//
// AC: [functional] An integration test under internal/integration/ or
// internal/review/ synthesises a >100 KiB review context, invokes the spawn
// path, and asserts (a) the spawn succeeds, (b) the constructed launch command
// is < 16 KiB, (c) the agent receives the full prompt content via the file.
// — issue #1195

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// spawnSpyTmuxBin installs a fake tmux binary for the test, recording each
// argv element on a separate line to argsFile. Returns the path to argsFile.
// Restores the original TmuxBin in t.Cleanup. Call only from non-parallel tests.
func spawnSpyTmuxBin(t *testing.T) string {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "tmux-args")
	wrapperPath := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write spy tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return argsFile
}

// spawnReadSpyArgs reads the space-separated argv elements recorded by the spy.
func spawnReadSpyArgs(argsFile string) []string {
	data, err := os.ReadFile(argsFile)
	if err != nil {
		return nil
	}
	var args []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

// openSpawnIntegTestDB opens a fresh temp SQLite DB and registers cleanup.
func openSpawnIntegTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	session.SetTestDBPath(dbFile)
	t.Cleanup(func() {
		d.Close()
		session.SetTestDBPath("")
	})
	return d, dbFile
}

// TestSpawnReviewAgent_LargePrompt_UsesPromptFile is the regression test for
// issue #1195. It verifies that session.SpawnSession (the shared primitive used
// by review.Run and review.RunAsync) routes a >100 KiB review prompt through
// the initial-prompt-file mechanism rather than inlining it in the tmux argv.
//
// The test synthesises a PRContext whose diff section alone exceeds 100 KiB,
// then builds the review prompt via the exported BuildReviewPromptForTest shim.
// It then calls SpawnSession in bwrap mode (the mode that triggered the original
// failure in #1092 and the reported regression in #1195) and verifies:
//
//   (a) The spawn succeeds — HostLaunchCmdSafeBound (16 KiB) is NOT exceeded.
//   (b) The tmux argv does NOT contain the prompt body inline.
//       PRISM_INITIAL_PROMPT_FILE is present; PRISM_INITIAL_PROMPT is absent.
//   (c) The initial-prompt file on disk contains the full prompt content.
//
// This test must NOT call t.Parallel(): it rewrites the global tmux.TmuxBin.
func TestSpawnReviewAgent_LargePrompt_UsesPromptFile(t *testing.T) {
	d, _ := openSpawnIntegTestDB(t)
	argsFile := spawnSpyTmuxBin(t)

	// Stub the sidecar so StartSidecarWithOpts does not try to exec a real
	// prism binary. The session package's TestMain handles PRISM_TEST_SUBPROCESS.
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")

	// Route all XDG state writes to a temp dir.
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// Build a >100 KiB review context. The diff section of a typical PR
	// review prompt is the largest contributor; replicate a realistic diff
	// body of ~120 KB.
	largeDiff := strings.Repeat("+// review-agent-prompt-file-test regression-#1195\n", 3000) // ~120 KB
	if len(largeDiff) < 100*1024 {
		t.Fatalf("test setup: largeDiff is only %d bytes, need ≥ 100 KiB", len(largeDiff))
	}

	prCtx := &review.PRContext{
		PRNumber:      "1195",
		Title:         "prism review: review-agent spawn inlines PRISM_INITIAL_PROMPT",
		Body:          "Closes #1195\n\nSee issue for root cause and proposed fix.",
		HeadRefName:   "review-agent-prompt-file",
		HeadRefOid:    "abc1234",
		BaseRefName:   "main",
		BaseRefOid:    "def5678",
		Additions:     3000,
		Deletions:     0,
		ChangedFiles:  4,
		RecentCommits: "abc1234 prism: fix review agent prompt delivery\ndef5678 main\n",
		BranchCommits: "abc1234 prism: fix review agent prompt delivery\n",
		Diff:          largeDiff,
		DiffLines:     3000,
		DiffBytes:     len(largeDiff),
		LinkedIssues: map[string]string{
			"1195": "title: prism review: launch-cmd size cap\nstate: OPEN\n",
		},
		WorktreePath: "/worktrees/nixos-config/review-agent-prompt-file",
	}

	prompt := review.BuildReviewPromptForTest("1195", prCtx)
	if len(prompt) < 100*1024 {
		t.Fatalf("test setup: prompt is only %d bytes, need ≥ 100 KiB for a meaningful regression test", len(prompt))
	}
	t.Logf("synthesised review prompt: %d bytes (%.1f KiB)", len(prompt), float64(len(prompt))/1024)

	const sessionName = "nixos-config@review-agent-prompt-file~review-1-review-context"
	opts := session.SpawnOpts{
		SessionName:   sessionName,
		Repo:          "nixos-config",
		Worktree:      "/worktrees/nixos-config/review-agent-prompt-file",
		AgentRole:     "review-context",
		Prompt:        prompt,
		Layout:        session.LayoutAgentOnly,
		IsolationMode: "bwrap",
	}

	// (a) Spawn must succeed. Before the #1092 / #1195 fix, this call would
	// fail with HostLaunchCmdTooLargeError because the full prompt was inlined
	// into the tmux new-window argv as -e PRISM_INITIAL_PROMPT=<120KB>.
	if err := session.SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession with >100 KiB prompt: %v — bwrap review-agent spawn must not exceed HostLaunchCmdSafeBound (regression of #1092, #1195)", err)
	}

	// (b) Inspect the tmux argv recorded by the spy.
	args := spawnReadSpyArgs(argsFile)
	joined := strings.Join(args, " ")

	// PRISM_INITIAL_PROMPT_FILE must be present (file-based delivery).
	promptFilePath, pathErr := session.InitialPromptPath(sessionName)
	if pathErr != nil {
		t.Fatalf("InitialPromptPath: %v", pathErr)
	}
	found := false
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && args[i+1] == "PRISM_INITIAL_PROMPT_FILE="+promptFilePath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("tmux argv does not contain [-e PRISM_INITIAL_PROMPT_FILE=%s] — bwrap review-agent must use file-based prompt delivery, not inline env var (#1195)\nfull argv: %v",
			promptFilePath, args)
	}

	// PRISM_INITIAL_PROMPT must NOT appear (the pre-fix inline path).
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "PRISM_INITIAL_PROMPT=") {
			t.Errorf("tmux argv contains inline PRISM_INITIAL_PROMPT — would re-introduce #1092 / #1195 launch-cmd size failure:\n  arg: %s", args[i+1])
			break
		}
	}

	// Defence-in-depth: the prompt body must not appear anywhere in the tmux argv.
	// Even a partial match (first 200 chars of the diff) would indicate regression.
	bodySnippet := largeDiff[:200]
	if strings.Contains(joined, bodySnippet) {
		t.Errorf("tmux argv contains prompt body inline (snippet: %.50q…) — file-based delivery is broken (#1195)", bodySnippet)
	}

	// Launch-command size bound: the entire tmux new-window argv approximation
	// must stay well below HostLaunchCmdSafeBound (16 KiB). The argv from the
	// spy is larger than what tmux sees (shell quoting is not stripped), but
	// the agent-only command ("prism agent-run --session <name>") + the file
	// path env var together are a few hundred bytes at most.
	argsByteCount := 0
	for _, a := range args {
		argsByteCount += len(a) + 4 // approximate '-e KEY=VALUE' overhead
	}
	const safebound = 16 * 1024
	if argsByteCount > safebound {
		t.Errorf("tmux argv total size %d bytes exceeds HostLaunchCmdSafeBound (%d) — prompt-file delivery is not keeping launch command O(1) in prompt size (#1195)",
			argsByteCount, safebound)
	}
	t.Logf("tmux argv total size: %d bytes (safe bound: %d)", argsByteCount, safebound)

	// (c) The initial-prompt file must exist and contain the full prompt.
	body, readErr := os.ReadFile(promptFilePath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v — initial-prompt file must exist after SpawnSession with bwrap mode", promptFilePath, readErr)
	}
	if string(body) != prompt {
		t.Errorf("initial-prompt file content mismatch: file has %d bytes, prompt has %d bytes — the full review context must be preserved verbatim",
			len(body), len(prompt))
	}
	t.Logf("initial-prompt file verified: %d bytes at %s", len(body), promptFilePath)
}

// TestSpawnReviewAgent_LargePrompt_SandboxExec mirrors the bwrap test for
// sandbox-exec mode. Both modes delegate prompt delivery to `prism agent-run`
// (which reads PRISM_INITIAL_PROMPT_FILE), so the file-based path must fire
// for both. Covers the Darwin-native isolation mode that macOS workers use.
//
// This test must NOT call t.Parallel(): it rewrites the global tmux.TmuxBin.
func TestSpawnReviewAgent_LargePrompt_SandboxExec(t *testing.T) {
	d, _ := openSpawnIntegTestDB(t)
	argsFile := spawnSpyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// Synthesise a >100 KiB diff section — same size as the bwrap test so
	// both modes are exercised against the same threshold.
	largeDiff := strings.Repeat("+// sandbox-exec regression test for #1195\n", 3000) // ~120 KB

	prCtx := &review.PRContext{
		PRNumber:    "1195",
		Title:       "review-agent-prompt-file sandbox-exec test",
		Body:        "Closes #1195",
		HeadRefName: "review-agent-prompt-file",
		HeadRefOid:  "abc1234",
		BaseRefName: "main",
		BaseRefOid:  "def5678",
		Diff:        largeDiff,
		DiffLines:   3000,
		DiffBytes:   len(largeDiff),
	}

	prompt := review.BuildReviewPromptForTest("1195", prCtx)
	if len(prompt) < 100*1024 {
		t.Fatalf("test setup: sandbox-exec prompt is only %d bytes, need ≥ 100 KiB", len(prompt))
	}

	const sessionName = "nixos-config@review-agent-prompt-file~review-1-review-qa"
	opts := session.SpawnOpts{
		SessionName:   sessionName,
		Repo:          "nixos-config",
		Worktree:      "/worktrees/nixos-config/review-agent-prompt-file",
		AgentRole:     "review-qa",
		Prompt:        prompt,
		Layout:        session.LayoutAgentOnly,
		IsolationMode: "sandbox-exec",
	}

	if err := session.SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession (sandbox-exec) with >100 KiB prompt: %v — sandbox-exec review-agent spawn must not exceed HostLaunchCmdSafeBound (#1195)", err)
	}

	args := spawnReadSpyArgs(argsFile)

	promptFilePath, pathErr := session.InitialPromptPath(sessionName)
	if pathErr != nil {
		t.Fatalf("InitialPromptPath: %v", pathErr)
	}

	// PRISM_INITIAL_PROMPT_FILE must appear.
	found := false
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && args[i+1] == "PRISM_INITIAL_PROMPT_FILE="+promptFilePath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("tmux argv (sandbox-exec) does not contain [-e PRISM_INITIAL_PROMPT_FILE=%s] — file-based delivery must work for sandbox-exec too (#1195)\nargs: %v",
			promptFilePath, args)
	}

	// Inline env var must NOT appear.
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "PRISM_INITIAL_PROMPT=") {
			t.Errorf("tmux argv (sandbox-exec) contains inline PRISM_INITIAL_PROMPT — would re-introduce #1195: %s", args[i+1])
			break
		}
	}

	// File must contain the full prompt.
	body, readErr := os.ReadFile(promptFilePath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v (sandbox-exec)", promptFilePath, readErr)
	}
	if string(body) != prompt {
		t.Errorf("sandbox-exec initial-prompt file: %d bytes, want %d", len(body), len(prompt))
	}
}
