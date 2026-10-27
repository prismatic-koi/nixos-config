package container

// project_trust_test.go — the project-trust flag of pi (issue #3216).
//
// PIInvocation (bwrap, sandbox-exec) must carry exactly one of --approve and
// --no-approve. The bwrap and sandbox-exec pane commands carry the decision
// of the host to `prism agent-run` as --approve-project-dir. No sandbox can
// reach project-trust.json.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// TestBwrapBuildArgs_ProjectTrustFileNotBound: a sandbox that can write
// project-trust.json can trust its own repo. The file is planted on the fake
// host home, so a widened mount finds a real file to bind.
// TestBwrapBuildArgs_PrismConfigDirNotBoundWholesale pins the directory half.
func TestBwrapBuildArgs_ProjectTrustFileNotBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	trustFile := filepath.Join(fakeHome, ".config", "prism", config.ProjectTrustFileName)
	if err := os.MkdirAll(filepath.Dir(trustFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustFile, []byte(`{"trusted_repos":["home-ops"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)
	for _, a := range redactedArgs(args) {
		if strings.Contains(a, config.ProjectTrustFileName) {
			t.Errorf("%s appears in the bwrap argv (%q) — it must stay host-side", config.ProjectTrustFileName, a)
		}
	}
}

// TestGenerateProfile_ProjectTrustFileNoAllow is the sandbox-exec half: under
// deny-default, a path that no rule names is unreadable and unwritable.
func TestGenerateProfile_ProjectTrustFileNoAllow(t *testing.T) {
	newFakeHome(t)
	profile := generateProfile(newSandboxExecManager(Config{SessionName: "repo@main"}))
	if strings.Contains(profile, config.ProjectTrustFileName) {
		t.Errorf("%s must not be named in the SBPL profile:\n%s", config.ProjectTrustFileName, profile)
	}
}

func TestPIInvocation_ProjectTrustFlag(t *testing.T) {
	for _, tc := range []struct {
		approve bool
		want    string
	}{
		{true, "--approve"},
		{false, "--no-approve"},
	} {
		// Prompts that look like the flags must not count: PIInvocation
		// adds a leading space to a prompt that starts with "-".
		for _, prompt := range []string{"", "fix it", "--approve"} {
			args := PIInvocation(Config{PIApproveProject: tc.approve, AgentRole: "worker", InitialPrompt: prompt})
			if got := countFlag(args, "--approve") + countFlag(args, "--no-approve"); got != 1 {
				t.Errorf("approve=%v prompt=%q: %d project-trust flags in %q, want exactly 1", tc.approve, prompt, got, args)
			}
			if countFlag(args, tc.want) != 1 {
				t.Errorf("approve=%v prompt=%q: argv lacks %s: %q", tc.approve, prompt, tc.want, args)
			}
		}
	}
}

// TestPIInvocation_ZeroConfigIsNoApprove: a Config that no caller filled in
// fails closed.
func TestPIInvocation_ZeroConfigIsNoApprove(t *testing.T) {
	if args := PIInvocation(Config{}); countFlag(args, "--no-approve") != 1 || countFlag(args, "--approve") != 0 {
		t.Errorf("PIInvocation(Config{}) = %q, want --no-approve and no --approve", args)
	}
}

func TestAgentPaneCmd_ApproveProjectDir(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/abcd-prism/bin/prism")
	for _, iso := range []Isolator{&bwrapIsolator{}, &sandboxExecIsolator{}} {
		got, err := iso.AgentPaneCmd(AgentPaneOpts{
			SessionName:       "prism-test@trust",
			ApproveProjectDir: "/home/u/code/home-ops/it's",
		})
		if err != nil {
			t.Fatalf("%T AgentPaneCmd: %v", iso, err)
		}
		if want := `--approve-project-dir '/home/u/code/home-ops/it'\''s'`; !strings.Contains(got, want) {
			t.Errorf("%T: pane command lacks %q: %q", iso, want, got)
		}

		got, err = iso.AgentPaneCmd(AgentPaneOpts{SessionName: "prism-test@trust"})
		if err != nil {
			t.Fatalf("%T AgentPaneCmd: %v", iso, err)
		}
		if strings.Contains(got, "--approve-project-dir") {
			t.Errorf("%T: empty ApproveProjectDir emitted the flag: %q", iso, got)
		}

		got, err = iso.AgentPaneCmd(AgentPaneOpts{
			SessionName:       "prism-test@trust",
			ApproveProjectDir: "/home/u/code/home-ops/main",
			HarnessName:       "not-pi",
		})
		if err != nil {
			t.Fatalf("%T AgentPaneCmd: %v", iso, err)
		}
		if strings.Contains(got, "--approve-project-dir") {
			t.Errorf("%T: non-pi harness got the flag: %q", iso, got)
		}
	}
}
