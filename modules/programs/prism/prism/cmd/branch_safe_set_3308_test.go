package cmd

// branch_safe_set_3308_test.go — `prism pr`, `prism spawn --pr`, and
// `prism spawn --branch` refuse a branch name outside the safe set before
// any worktree or session exists (issue #3308).
//
// The author of a PR picks its head branch, and git accepts shell syntax in
// a branch name. The session name holds the branch, and tmux.nix puts the
// session name into shell commands. So the branch must be refused before
// prism makes the worktree, the tmux session, or the agent_status row.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// unsafePRBranch is a branch name that git accepts and a shell runs.
const unsafePRBranch = "x$(id)"

// setupUnsafePRRepo makes a bare+worktree layout with the branch
// unsafePRBranch, a fake gh that resolves every PR to that branch, a test
// DB, and a tmux spy. It returns the bare root, the DB, and the file where
// the spy records each tmux argument.
func setupUnsafePRRepo(t *testing.T) (string, *db.DB, string) {
	t.Helper()
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	dbFile := setupIsolatedSpawn(t)
	bareRoot := initBareRepoWithMainWorktree(t, t.TempDir(), "prism-test-unsafe")
	bareDir := filepath.Join(bareRoot, ".bare")
	for _, args := range [][]string{
		{"--git-dir", bareDir, "branch", unsafePRBranch, "main"},
		{"--git-dir", bareDir, "remote", "add", "origin", bareDir},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	binDir := t.TempDir()
	branchFile := filepath.Join(binDir, "branch")
	if err := os.WriteFile(branchFile, []byte(unsafePRBranch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\ncat '"+branchFile+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PRISM_BARE_ROOT", bareRoot)

	d := openTestDBAt(t, dbFile)
	session.SetTestDBPath(dbFile)
	t.Cleanup(func() { session.SetTestDBPath("") })
	return bareRoot, d, spyTmuxForTrust(t)
}

// assertNothingMade checks that the refused command made no worktree for
// branch, no tmux session, and no agent_status row.
func assertNothingMade(t *testing.T, bareRoot, branch string, d *db.DB, tmuxArgsFile string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(bareRoot, branch)); !os.IsNotExist(err) {
		t.Errorf("worktree %s: %v, want none", filepath.Join(bareRoot, branch), err)
	}
	if args, err := os.ReadFile(tmuxArgsFile); err == nil && strings.Contains(string(args), "new-session") {
		t.Errorf("tmux got new-session, want no session:\n%s", args)
	}
	rows, err := d.AllActiveStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("agent_status rows = %+v, want none", rows)
	}
}

func assertNamesUnsafeChars(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error, want a refusal of the branch")
	}
	for _, want := range []string{prefix, `"$"`, `"("`, `")"`, "safe set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to hold %s", err, want)
		}
	}
}

func TestPrCmd_RefusesUnsafeHeadBranch(t *testing.T) {
	bareRoot, d, tmuxArgs := setupUnsafePRRepo(t)
	prev := prEnsureAndSwitch
	t.Cleanup(func() { prEnsureAndSwitch = prev })
	prEnsureAndSwitch = func(path, _ string, _ session.Opts) error {
		t.Fatalf("prism pr started a session in %q, want a refusal first", path)
		return nil
	}

	cmd := newPRCmdForTest()
	_ = cmd.Flags().Set("isolation", "host")
	err := prCmd.RunE(cmd, []string{"12"})
	assertNamesUnsafeChars(t, err, "head branch of PR #12")
	assertNothingMade(t, bareRoot, unsafePRBranch, d, tmuxArgs)
}

func TestRunSpawn_RefusesUnsafeBranch(t *testing.T) {
	for _, tc := range []struct {
		name, flag, value, prefix string
	}{
		{"--pr", "pr", "12", "head branch of PR #12"},
		{"--branch", "branch", unsafePRBranch, "--branch:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bareRoot, d, tmuxArgs := setupUnsafePRRepo(t)
			prev := spawnSessionFn
			t.Cleanup(func() { spawnSessionFn = prev })
			spawnSessionFn = func(_ *db.DB, opts session.SpawnOpts) error {
				t.Fatalf("prism spawn started session %q, want a refusal first", opts.SessionName)
				return nil
			}

			cmd := buildSpawnCmdForMainReuseTest(t)
			_ = cmd.Flags().Set("isolation", "host")
			_ = cmd.Flags().Set("prompt", "do the thing")
			_ = cmd.Flags().Set(tc.flag, tc.value)
			err := runSpawn(cmd, nil)
			assertNamesUnsafeChars(t, err, tc.prefix)
			assertNothingMade(t, bareRoot, unsafePRBranch, d, tmuxArgs)
		})
	}
}

// TestResolveBranch_BranchesInUse pins the branch shapes in use today. Each
// one passes the check, and SanitiseBranch gives the same result as before.
func TestResolveBranch_BranchesInUse(t *testing.T) {
	for flag, want := range map[string]string{
		"tmux-name-injection":                      "tmux-name-injection",
		"bump-pi-0.77.0":                           "bump-pi-0.77.0",
		"20260531T1213":                            "20260531t1213",
		"prismatic-bot/kube-prometheus-stack-89.x": "prismatic-bot-kube-prometheus-stack-89.x",
		"snake_case":                               "snake-case",
	} {
		got, err := resolveBranch(t.TempDir(), flag, "")
		if err != nil || got != want {
			t.Errorf("resolveBranch(--branch %q) = (%q, %v), want (%q, nil)", flag, got, err, want)
		}
	}
}
