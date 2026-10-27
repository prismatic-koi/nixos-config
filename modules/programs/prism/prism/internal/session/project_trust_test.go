package session

// project_trust_test.go — the project-trust flag on every pi launch command
// that BuildAgentCmd builds (issue #3216).
//
// host mode: buildDirectAgentCmd emits exactly one of --approve and
// --no-approve. bwrap and sandbox-exec: the pane command carries the decision
// to `prism agent-run` as --approve-project-dir <dir>, and agent-run turns it
// into the pi flag (cmd/agent_run_project_trust_test.go).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

// trustFlags returns the project-trust tokens of a launch command.
func trustFlags(cmd string) []string {
	var out []string
	for _, f := range strings.Fields(cmd) {
		if f == "--approve" || f == "--no-approve" {
			out = append(out, f)
		}
	}
	return out
}

// assertTrust checks the launch command of one mode against want.
func assertTrust(t *testing.T, mode, cmd, dir string, want bool) {
	t.Helper()
	if mode == "host" {
		flags := trustFlags(cmd)
		wantFlag := "--no-approve"
		if want {
			wantFlag = "--approve"
		}
		if len(flags) != 1 || flags[0] != wantFlag {
			t.Errorf("host command has project-trust flags %q, want exactly [%s]: %q", flags, wantFlag, cmd)
		}
		return
	}
	if flags := trustFlags(cmd); len(flags) != 0 {
		t.Errorf("%s pane command has pi flags %q; agent-run must add them: %q", mode, flags, cmd)
	}
	has := strings.Contains(cmd, "--approve-project-dir '"+filepath.Clean(dir)+"'")
	if has != want {
		t.Errorf("%s pane command: --approve-project-dir present = %v, want %v: %q", mode, has, want, cmd)
	}
	if !want && strings.Contains(cmd, "--approve-project-dir") {
		t.Errorf("%s pane command carries --approve-project-dir for a session that is not trusted: %q", mode, cmd)
	}
}

func TestBuildAgentCmd_ProjectTrust(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops", "obsidian")
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	nixos := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")

	cases := []struct {
		name     string
		session  string
		agent    string
		worktree string
		want     bool
	}{
		{"trusted repo", "home-ops@main", "coordinator", homeOps, true},
		{"non-worktree trusted repo with a bare name", "obsidian", "", l.Obsidian, true},
		{"repo not in the list", "nixos-config@main", "coordinator", nixos, false},
		{"directory outside every project", "obsidian", "", l.Writable, false},
		{"empty Worktree", "home-ops@main", "coordinator", "", false},
	}
	for _, tc := range cases {
		for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				cmd, err := BuildAgentCmd(Opts{
					SessionName:   tc.session,
					Agent:         tc.agent,
					Worktree:      tc.worktree,
					IsolationMode: mode,
					HarnessName:   "pi",
				})
				if err != nil {
					t.Fatalf("BuildAgentCmd: %v", err)
				}
				assertTrust(t, mode, cmd, tc.worktree, tc.want)
			})
		}
	}
}

// TestSpawnLayouts_ForwardWorktreeToTheTrustDecision: both SpawnOpts → Opts
// builders must forward Worktree. If one drops it, a spawned worker, a review
// agent, or an investigator of a trusted repo starts with --no-approve.
func TestSpawnLayouts_ForwardWorktreeToTheTrustDecision(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "feat")

	for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
		t.Run("full/"+mode, func(t *testing.T) {
			opts := buildOptsForLayout(SpawnOpts{
				SessionName:   "home-ops@feat",
				Worktree:      wt,
				AgentRole:     "worker",
				IsolationMode: mode,
				HarnessName:   "pi",
			}, 14000, "")
			cmd, err := BuildAgentCmd(opts)
			if err != nil {
				t.Fatalf("BuildAgentCmd: %v", err)
			}
			assertTrust(t, mode, cmd, wt, true)
		})
		t.Run("agent-only/"+mode, func(t *testing.T) {
			opts := buildOptsForAgentOnlyLayout(SpawnOpts{
				SessionName:  "home-ops@feat~review-1-review-goal",
				Worktree:     wt,
				AgentRole:    "review-goal",
				HarnessName:  "pi",
				AgentEnvVars: map[string]string{},
			}, 14000, mode)
			cmd, err := BuildAgentCmd(opts)
			if err != nil {
				t.Fatalf("BuildAgentCmd: %v", err)
			}
			assertTrust(t, mode, cmd, wt, true)
		})
	}
}

// TestBuildAgentCmd_TrustJSONDoesNotChangeTheDecision: an entry in
// ~/.pi/agent/trust.json does not reach the decision, and pi gets an explicit
// --no-approve, which wins over trust.json in pi.
func TestBuildAgentCmd_TrustJSONDoesNotChangeTheDecision(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	nixos := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".pi", "agent")
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	trust := `{"` + nixos + `": true, "` + l.Writable + `": true}`
	if err := os.WriteFile(filepath.Join(agentDir, "trust.json"), []byte(trust), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ session, dir string }{
		{"nixos-config@main", nixos},
		{"obsidian", l.Writable},
	} {
		cmd, err := BuildAgentCmd(Opts{SessionName: tc.session, Worktree: tc.dir, IsolationMode: "host", HarnessName: "pi"})
		if err != nil {
			t.Fatalf("BuildAgentCmd: %v", err)
		}
		assertTrust(t, "host", cmd, tc.dir, false)
	}
}
