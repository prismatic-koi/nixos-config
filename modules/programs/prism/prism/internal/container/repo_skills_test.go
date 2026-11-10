package container

// repo_skills_test.go — the repo skills of pi (issue #3463).
//
// PIInvocation (bwrap, sandbox-exec) passes one --skill <path> for each
// entry of Config.PISkillPaths, and no other --skill flag. No sandbox can
// reach repo-skills.json.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// skillValues returns the value of each --skill flag of a pi argv.
func skillValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--skill" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestPIInvocation_SkillFlags(t *testing.T) {
	paths := []string{"/nix/store/0000-site-theming", "/nix/store/1111-other"}
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"no repo skills", nil},
		{"two repo skills", paths},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A prompt that looks like the flag must not count: PIInvocation
			// adds a leading space to a prompt that starts with "-".
			args := PIInvocation(Config{PISkillPaths: tc.paths, AgentRole: "worker", InitialPrompt: "--skill /tmp/x"})
			if got := skillValues(args); !slices.Equal(got, tc.paths) {
				t.Errorf("--skill values = %q, want %q (argv %q)", got, tc.paths, args)
			}
			if countFlag(args, "--no-approve") != 1 {
				t.Errorf("argv lacks --no-approve: %q", args)
			}
		})
	}
}

// TestBwrapBuildArgs_RepoSkillsFileNotBound: a sandbox that can write
// repo-skills.json can give pi a skill from its own worktree. The file is
// planted on the fake host home, so a widened mount finds a real file to
// bind. TestBwrapBuildArgs_PrismConfigDirNotBoundWholesale pins the
// directory half.
func TestBwrapBuildArgs_RepoSkillsFileNotBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	file := filepath.Join(fakeHome, ".config", "prism", config.RepoSkillsFileName)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"repo":["/nix/store/0000-x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)
	for _, a := range redactedArgs(args) {
		if strings.Contains(a, config.RepoSkillsFileName) {
			t.Errorf("%s appears in the bwrap argv (%q) — it must stay host-side", config.RepoSkillsFileName, a)
		}
	}
}

// TestGenerateProfile_RepoSkillsFileNoAllow is the sandbox-exec half: under
// deny-default, a path that no rule names is unreadable and unwritable.
func TestGenerateProfile_RepoSkillsFileNoAllow(t *testing.T) {
	newFakeHome(t)
	profile := generateProfile(newSandboxExecManager(Config{SessionName: "repo@main"}))
	if strings.Contains(profile, config.RepoSkillsFileName) {
		t.Errorf("%s must not be named in the SBPL profile:\n%s", config.RepoSkillsFileName, profile)
	}
}
