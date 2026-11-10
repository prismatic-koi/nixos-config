package session

// repo_skills_test.go — the repo skills on every pi launch command that
// BuildAgentCmd builds (issue #3463).
//
// host mode: buildDirectAgentCmd emits one --skill <path> for each repo
// skill. bwrap and sandbox-exec: the pane command carries no --skill flag,
// because `prism agent-run` resolves the skills on the host
// (cmd/agent_run_repo_skills_test.go). The skills do not change the
// project-trust decision.

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/reposkills/reposkillstest"
)

// hostSkillValues returns the value of each --skill flag of a host launch
// command. shellQuote puts each value in single quotes.
func hostSkillValues(cmd string) []string {
	fields := strings.Fields(cmd)
	var out []string
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "--skill" {
			out = append(out, strings.Trim(fields[i+1], "'"))
		}
	}
	return out
}

// assertSkills checks the launch command of one mode against want.
func assertSkills(t *testing.T, mode, cmd string, want []string) {
	t.Helper()
	if mode != "host" {
		if strings.Contains(cmd, "--skill") {
			t.Errorf("%s pane command has a --skill flag; agent-run must add it: %q", mode, cmd)
		}
		return
	}
	if got := hostSkillValues(cmd); !slices.Equal(got, want) {
		t.Errorf("host command --skill values = %q, want %q: %q", got, want, cmd)
	}
}

func TestBuildAgentCmd_RepoSkills(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	store := reposkillstest.Store(t)
	theming := reposkillstest.Skill(t, store, "site-theming")
	nixosMain := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
	nixosFeat := projecttrusttest.Worktree(t, l.Code, "nixos-config", "feat")
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	reposkillstest.WriteFile(t, l.ConfigDir, map[string][]string{"nixos-config": {theming}})

	cases := []struct {
		name, session, agent, worktree string
		wantSkills                     []string
		wantApprove                    bool
	}{
		{"coordinator", "nixos-config@main", "coordinator", nixosMain, []string{theming}, false},
		{"worker", "nixos-config@feat", "worker", nixosFeat, []string{theming}, false},
		{"review agent", "nixos-config@feat~review-1-review-goal", "review-goal", nixosFeat, []string{theming}, false},
		{"investigator", "nixos-config@feat~investigate-x", "investigate", nixosFeat, []string{theming}, false},
		{"repo with no entry", "home-ops@main", "coordinator", homeOps, nil, true},
		{"empty Worktree", "nixos-config@main", "coordinator", "", nil, false},
	}
	for _, tc := range cases {
		for _, mode := range trustModes {
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
				assertSkills(t, mode, cmd, tc.wantSkills)
				assertTrust(t, mode, cmd, tc.worktree, tc.wantApprove)
			})
		}
	}
}

// TestBuildAgentCmd_RepoSkillsOnPRBranch: a PR-branch session and its
// descendants get --no-approve, and still get the repo skills. The skill
// content is in the nix store, not on the branch.
func TestBuildAgentCmd_RepoSkillsOnPRBranch(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	l := projecttrusttest.New(t, "nixos-config")
	store := reposkillstest.Store(t)
	theming := reposkillstest.Skill(t, store, "site-theming")
	wt := projecttrusttest.Worktree(t, l.Code, "nixos-config", "renovate-x")
	reposkillstest.WriteFile(t, l.ConfigDir, map[string][]string{"nixos-config": {theming}})
	const root = "nixos-config@renovate-x"
	if err := d.MarkPRBranchSession(root, wt, "12"); err != nil {
		t.Fatalf("MarkPRBranchSession: %v", err)
	}

	for _, name := range append([]string{root}, prDescendants(root)...) {
		for _, mode := range trustModes {
			t.Run(name+"/"+mode, func(t *testing.T) {
				cmd, err := BuildAgentCmd(Opts{SessionName: name, Worktree: wt, IsolationMode: mode, HarnessName: "pi"})
				if err != nil {
					t.Fatalf("BuildAgentCmd: %v", err)
				}
				assertSkills(t, mode, cmd, []string{theming})
				assertTrust(t, mode, cmd, wt, false)
			})
		}
	}
}

// TestBuildAgentCmd_RepoSkillsWarnings: a host-mode session starts pi with
// no --skill flag for each skill that prism cannot use, and the agent-run
// log of the session gets a warning.
func TestBuildAgentCmd_RepoSkillsWarnings(t *testing.T) {
	cases := []struct {
		name       string
		body       string // "" leaves the file absent
		wantSkills func(good string) []string
		wantInLog  string
	}{
		{"missing file", "", func(string) []string { return nil }, "repo-skills.json"},
		{"malformed file", "{", func(string) []string { return nil }, "repo-skills.json"},
		{"missing path", `{"nixos-config":["MISSING","GOOD"]}`, func(good string) []string { return []string{good} }, "0000-missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			l := projecttrusttest.New(t)
			store := reposkillstest.Store(t)
			good := reposkillstest.Skill(t, store, "good")
			wt := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
			if tc.body != "" {
				body := strings.NewReplacer("MISSING", store+"/0000-missing", "GOOD", good).Replace(tc.body)
				reposkillstest.WriteRaw(t, l.ConfigDir, []byte(body))
			}
			const name = "nixos-config@main"
			cmd, err := BuildAgentCmd(Opts{SessionName: name, Worktree: wt, IsolationMode: "host", HarnessName: "pi"})
			if err != nil {
				t.Fatalf("BuildAgentCmd: %v", err)
			}
			assertSkills(t, "host", cmd, tc.wantSkills(good))
			logPath, err := AgentRunLogPath(name)
			if err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read agent-run log: %v", err)
			}
			if !strings.Contains(string(log), "[prism] warning: ") || !strings.Contains(string(log), tc.wantInLog) {
				t.Errorf("agent-run log = %q, want a warning that names %s", log, tc.wantInLog)
			}
		})
	}
}
