package cmd

// agent_run_repo_skills_test.go — `prism agent-run` (bwrap and sandbox-exec)
// gives pi one --skill <path> for each repo skill of the session (issue
// #3463). It reads repo-skills.json on the host, and takes the repo from the
// session name and the worktree in agent_status.

import (
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/reposkills/reposkillstest"
	"github.com/prismatic-koi/prism/internal/session"
)

// piSkillValues returns the value of each --skill flag of a pi argv.
func piSkillValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--skill" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestPopulatePIConfig_RepoSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	isolateForPopulatePIConfig(t)
	l := projecttrusttest.New(t)
	writeProfilesWithEnv(t, l.ConfigDir, nil)
	store := reposkillstest.Store(t)
	theming := reposkillstest.Skill(t, store, "site-theming")
	nixos := projecttrusttest.Worktree(t, l.Code, "nixos-config", "feat")
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	reposkillstest.WriteFile(t, l.ConfigDir, map[string][]string{"nixos-config": {theming}})
	cfg := config.Config{
		PIExtensionDir:   "/nix/store/fake-ext",
		ProjectLocations: []string{l.Code},
		ProjectSpecific:  []string{l.Obsidian},
	}

	cases := []struct {
		name, session, role, worktree string
		want                          []string
	}{
		{"worker", "nixos-config@feat", "worker", nixos, []string{theming}},
		{"review agent", "nixos-config@feat~review-1-review-code", "review-code", nixos, []string{theming}},
		{"investigator", "nixos-config@feat~investigate-x", "investigate", nixos, []string{theming}},
		{"repo with no entry", "home-ops@main", "coordinator", homeOps, nil},
		{"agent_status worktree of another repo", "nixos-config@feat", "worker", homeOps, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrCfg := container.Config{Worktree: tc.worktree}
			if err := populatePIConfig(&ctrCfg, tc.session, tc.role, cfg, piOverrides{}); err != nil {
				t.Fatalf("populatePIConfig: %v", err)
			}
			args := container.PIInvocation(ctrCfg)
			if got := piSkillValues(args); !slices.Equal(got, tc.want) {
				t.Errorf("pi argv --skill values = %q, want %q", got, tc.want)
			}
			if got := piTrustFlags(args); len(got) != 1 || got[0] != "--no-approve" {
				t.Errorf("pi argv project-trust flags = %q, want exactly [--no-approve]", got)
			}
		})
	}
}

// TestPopulatePIConfig_RepoSkillsWarnings: pi starts with no --skill flag
// for each skill that prism cannot use, and agent-run writes a warning to
// the agent-run log of the session.
func TestPopulatePIConfig_RepoSkillsWarnings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	cases := []struct {
		name      string
		body      string // "" leaves the file absent
		wantGood  bool
		wantInLog string
	}{
		{"missing file", "", false, config.RepoSkillsFileName},
		{"malformed file", "{", false, config.RepoSkillsFileName},
		{"missing path", `{"nixos-config":["MISSING","GOOD"]}`, true, "0000-missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateForPopulatePIConfig(t)
			l := projecttrusttest.New(t)
			writeProfilesWithEnv(t, l.ConfigDir, nil)
			store := reposkillstest.Store(t)
			good := reposkillstest.Skill(t, store, "good")
			wt := projecttrusttest.Worktree(t, l.Code, "nixos-config", "feat")
			if tc.body != "" {
				body := strings.NewReplacer("MISSING", store+"/0000-missing", "GOOD", good).Replace(tc.body)
				reposkillstest.WriteRaw(t, l.ConfigDir, []byte(body))
			}
			const name = "nixos-config@feat"
			cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext", ProjectLocations: []string{l.Code}}
			ctrCfg := container.Config{Worktree: wt}
			if err := populatePIConfig(&ctrCfg, name, "worker", cfg, piOverrides{}); err != nil {
				t.Fatalf("populatePIConfig: %v", err)
			}
			var want []string
			if tc.wantGood {
				want = []string{good}
			}
			if got := piSkillValues(container.PIInvocation(ctrCfg)); !slices.Equal(got, want) {
				t.Errorf("pi argv --skill values = %q, want %q", got, want)
			}
			logPath, err := session.AgentRunLogPath(name)
			if err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read agent-run log: %v", err)
			}
			if !strings.Contains(string(log), "[agent-run] warning: ") || !strings.Contains(string(log), tc.wantInLog) {
				t.Errorf("agent-run log = %q, want a warning that names %s", log, tc.wantInLog)
			}
		})
	}
}
