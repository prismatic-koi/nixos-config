package cmd

// pr_branch_trust_3287_test.go — `prism pr` and `prism spawn --pr` write the
// PR-branch record before the agent starts, so pi gets --no-approve in a
// trusted repo (issue #3287).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

const prTrustBranch = "renovate-x"

// trustedPRRepo is a bare+worktree layout of the trusted repo home-ops with a
// PR branch, a fake gh that resolves every PR to that branch, and a test DB
// that both cmd and internal/session read.
type trustedPRRepo struct {
	bareRoot string
	worktree string // where CreateWorktree checks out the PR branch
	session  string
	d        *db.DB
}

func setupTrustedPRRepo(t *testing.T) trustedPRRepo {
	t.Helper()
	dbFile := setupIsolatedSpawn(t)
	l := projecttrusttest.New(t, "home-ops")
	bareRoot := initBareRepoWithMainWorktree(t, l.Code, "home-ops")
	bareDir := filepath.Join(bareRoot, ".bare")
	for _, args := range [][]string{
		{"--git-dir", bareDir, "branch", prTrustBranch, "main"},
		{"--git-dir", bareDir, "remote", "add", "origin", bareDir},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\necho "+prTrustBranch+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PRISM_BARE_ROOT", bareRoot)

	d := openTestDBAt(t, dbFile)
	session.SetTestDBPath(dbFile)
	t.Cleanup(func() { session.SetTestDBPath("") })
	return trustedPRRepo{
		bareRoot: bareRoot,
		worktree: filepath.Join(bareRoot, prTrustBranch),
		session:  "home-ops@" + prTrustBranch,
		d:        d,
	}
}

// agentApproves builds the agent command of opts in mode and reports whether
// it gives pi --approve.
func agentApproves(t *testing.T, opts session.Opts, mode string) bool {
	t.Helper()
	opts.IsolationMode = mode
	opts.HarnessName = "pi"
	cmd, err := session.BuildAgentCmd(opts)
	if err != nil {
		t.Fatalf("BuildAgentCmd(%s): %v", mode, err)
	}
	return commandApproves(t, cmd, mode, opts.Worktree)
}

func commandApproves(t *testing.T, cmd, mode, worktree string) bool {
	t.Helper()
	if mode != "host" {
		return strings.Contains(cmd, "--approve-project-dir '"+worktree+"'")
	}
	var flags []string
	for _, f := range strings.Fields(cmd) {
		// A tmux pane_start_command quotes the whole `sh -c` argument.
		f = strings.Trim(f, `"'`)
		if f == "--approve" || f == "--no-approve" {
			flags = append(flags, f)
		}
	}
	if len(flags) != 1 {
		t.Fatalf("host agent command has project-trust flags %q, want exactly one: %s", flags, cmd)
	}
	return flags[0] == "--approve"
}

func newPRCmdForTest() *cobra.Command {
	cmd := &cobra.Command{Use: "pr"}
	cmd.Flags().String("repo", "", "")
	addPromptFlags(cmd)
	cmd.Flags().String("agent", "", "")
	cmd.Flags().Bool("attach", false, "")
	cmd.Flags().Bool("ignore-concurrency-cap", false, "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	cmd.Flags().StringArray("model-override", nil, "")
	cmd.Flags().String("harness", "", "")
	cmd.Flags().String("isolation", "", "")
	return cmd
}

// TestPrCmd_RecordsPRBranchBeforeTheAgentStarts: when `prism pr` starts the
// session, the record is in prism.db, and the agent command gives pi
// --no-approve in every isolation mode. With the record removed, the same
// command gives --approve, so the layout is trusted and the record alone
// changes the result.
func TestPrCmd_RecordsPRBranchBeforeTheAgentStarts(t *testing.T) {
	r := setupTrustedPRRepo(t)

	started := false
	prev := prEnsureAndSwitch
	t.Cleanup(func() { prEnsureAndSwitch = prev })
	prEnsureAndSwitch = func(path, projectRoot string, opts session.Opts) error {
		started = true
		opts.SessionName = mustNameFor(t, path, projectRoot)
		opts.Worktree = path
		if opts.SessionName != r.session || path != r.worktree {
			t.Fatalf("prism pr starts %q in %q, want %q in %q", opts.SessionName, path, r.session, r.worktree)
		}
		records, err := r.d.PRBranchSessions()
		if err != nil || len(records) != 1 || records[0].SessionName != r.session || records[0].Worktree != r.worktree || records[0].PR != "12" {
			t.Fatalf("PR-branch records at the start of the agent = (%+v, %v), want the record of %s", records, err, r.session)
		}
		for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
			if agentApproves(t, opts, mode) {
				t.Errorf("%s: the agent command of the prism pr session gives --approve, want --no-approve", mode)
			}
		}
		if err := r.d.ClearPRBranchSession(r.session); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
			if !agentApproves(t, opts, mode) {
				t.Errorf("%s: with no record, the agent command gives --no-approve; the test layout is not trusted", mode)
			}
		}
		return nil
	}

	cmd := newPRCmdForTest()
	_ = cmd.Flags().Set("isolation", "host")
	if err := prCmd.RunE(cmd, []string{"12"}); err != nil {
		t.Fatalf("prism pr: %v", err)
	}
	if !started {
		t.Fatal("prism pr did not start the session")
	}
}

// spyTmuxForTrust replaces tmux with a script that records its arguments.
func spyTmuxForTrust(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = filepath.Join(dir, "tmux")
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return argsFile
}

// TestRunSpawn_PRFlagRecordsPRBranchBeforeTheAgentStarts: `prism spawn --pr`
// passes the raw --pr value to SpawnSession, which writes the record before
// it builds the agent command. A URL is a valid --pr value, and its PR
// number is 0, so the record must not depend on the number. A spawn with
// --branch and no --pr gives --approve.
func TestRunSpawn_PRFlagRecordsPRBranchBeforeTheAgentStarts(t *testing.T) {
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	for _, tc := range []struct {
		name, pr, branch string
		wantApprove      bool
	}{
		{"PR number", "12", "", false},
		{"PR URL", "https://github.com/prismatic-koi/home-ops/pull/12", "", false},
		{"branch, no --pr", "", prTrustBranch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupTrustedPRRepo(t)
			argsFile := spyTmuxForTrust(t)

			var got session.SpawnOpts
			prev := spawnSessionFn
			t.Cleanup(func() { spawnSessionFn = prev })
			spawnSessionFn = func(d *db.DB, opts session.SpawnOpts) error {
				got = opts
				opts.ReadinessTimeout = 0
				opts.PIExtensionDir = "/test/prism-pi-extension"
				return session.SpawnSession(d, opts)
			}

			cmd := buildSpawnCmdForMainReuseTest(t)
			_ = cmd.Flags().Set("isolation", "host")
			_ = cmd.Flags().Set("prompt", "review the PR")
			if tc.pr != "" {
				_ = cmd.Flags().Set("pr", tc.pr)
			}
			if tc.branch != "" {
				_ = cmd.Flags().Set("branch", tc.branch)
			}
			if err := runSpawn(cmd, nil); err != nil {
				t.Fatalf("prism spawn: %v", err)
			}
			t.Cleanup(func() { session.KillSidecar(r.session) })

			if got.SessionName != r.session || got.PRRef != tc.pr {
				t.Errorf("SpawnOpts = {SessionName: %q, PRRef: %q}, want {%q, %q}", got.SessionName, got.PRRef, r.session, tc.pr)
			}
			records, err := r.d.PRBranchSessions()
			if err != nil {
				t.Fatal(err)
			}
			if hasRecord := len(records) == 1 && records[0].SessionName == r.session; hasRecord == tc.wantApprove {
				t.Errorf("PR-branch records = %+v, want a record = %v", records, !tc.wantApprove)
			}

			args, err := os.ReadFile(argsFile)
			if err != nil || !strings.Contains(string(args), "new-window") {
				t.Fatalf("SpawnSession created no agent window (%v): %s", err, args)
			}
			if approves := commandApproves(t, strings.ReplaceAll(string(args), "\n", " "), "host", r.worktree); approves != tc.wantApprove {
				t.Errorf("agent command gives --approve = %v, want %v", approves, tc.wantApprove)
			}
		})
	}
}
