package cmd

// agent_run_project_trust_test.go — `prism agent-run` turns the project-trust
// decision of the host into the pi flag (issue #3216). agent-run does not
// decide: it passes --approve only when the pane command carries
// --approve-project-dir for the worktree that pi starts in.

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/session"
)

func TestPIApproveProject(t *testing.T) {
	cases := []struct {
		name               string
		approved, worktree string
		want               bool
	}{
		{"host approved this worktree", "/c/home-ops/main", "/c/home-ops/main", true},
		{"same directory after Clean", "/c/home-ops/main/", "/c/home-ops/main", true},
		{"no decision from the host", "", "/c/home-ops/main", false},
		{"agent_status worktree differs from the start directory", "/c/home-ops/main", "/npm/home-ops/main", false},
		{"no worktree", "/c/home-ops/main", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := piApproveProject(tc.approved, tc.worktree); got != tc.want {
				t.Errorf("piApproveProject(%q, %q) = %v, want %v", tc.approved, tc.worktree, got, tc.want)
			}
		})
	}
}

// piTrustFlags returns the project-trust flags of a pi argv.
func piTrustFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if a == "--approve" || a == "--no-approve" {
			out = append(out, a)
		}
	}
	return out
}

// splitPaneCommand splits a pane command that shellQuoteContainer built: it
// handles single-quoted words and a backslash escape outside quotes. It is
// enough for these tests.
func splitPaneCommand(t *testing.T, cmd string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case inQuote && c == '\'':
			inQuote = false
		case inQuote:
			cur.WriteByte(c)
		case c == '\'':
			inQuote, inWord = true, true
		case c == '\\' && i+1 < len(cmd):
			i++
			cur.WriteByte(cmd[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inQuote {
		t.Fatalf("unterminated quote in %q", cmd)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// TestAgentRun_PaneCommandCarriesTheHostDecision runs the bwrap path end to
// end: session.BuildAgentCmd on the host builds the pane command, the
// agent-run flag set parses it, and populatePIConfig and PIInvocation build
// the pi argv. If the flag name that the pane command emits and the flag
// name that agent-run reads drift apart, this test fails.
func TestAgentRun_PaneCommandCarriesTheHostDecision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	isolateForPopulatePIConfig(t)
	l := projecttrusttest.New(t, "home-ops")
	writeProfiles(t, l.ConfigDir, "test", "anthropic/claude-opus-4-7", "medium")
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "feat")
	nixos := projecttrusttest.Worktree(t, l.Code, "nixos-config", "feat")

	cases := []struct {
		name             string
		session, dir     string
		statusWorktree   string // the worktree that agent-run reads from agent_status
		wantFlag         string
		wantApproveInCmd bool
	}{
		{"trusted repo", "home-ops@feat", homeOps, homeOps, "--approve", true},
		{"repo not in the list", "nixos-config@feat", nixos, nixos, "--no-approve", false},
		{"agent_status worktree moved after the decision", "home-ops@feat", homeOps, nixos, "--no-approve", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paneCmd, err := session.BuildAgentCmd(session.Opts{
				SessionName:   tc.session,
				Agent:         "worker",
				Worktree:      tc.dir,
				IsolationMode: "bwrap",
				HarnessName:   "pi",
			})
			if err != nil {
				t.Fatalf("BuildAgentCmd: %v", err)
			}
			words := splitPaneCommand(t, paneCmd)
			i := slices.Index(words, "agent-run")
			if i < 0 {
				t.Fatalf("pane command has no agent-run: %q", paneCmd)
			}
			flags := agentRunCmd.Flags()
			t.Cleanup(func() {
				_ = flags.Set("approve-project-dir", "")
				_ = flags.Set("session", "")
			})
			if err := flags.Parse(words[i+1:]); err != nil {
				t.Fatalf("agent-run cannot parse the pane command %q: %v", paneCmd, err)
			}
			approved, _ := flags.GetString("approve-project-dir")
			if (approved != "") != tc.wantApproveInCmd {
				t.Fatalf("--approve-project-dir = %q, want present = %v", approved, tc.wantApproveInCmd)
			}

			ctrCfg := container.Config{Worktree: tc.statusWorktree}
			cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
			if err := populatePIConfig(&ctrCfg, tc.session, "worker", cfg, piOverrides{ApproveProjectDir: approved}); err != nil {
				t.Fatalf("populatePIConfig: %v", err)
			}
			got := piTrustFlags(container.PIInvocation(ctrCfg))
			if len(got) != 1 || got[0] != tc.wantFlag {
				t.Errorf("pi argv project-trust flags = %q, want exactly [%s]", got, tc.wantFlag)
			}
		})
	}
}

// TestPopulatePIConfig_NoDecisionIsNoApprove: with no --approve-project-dir
// (every pane command built before the decision existed, and every session of
// a repo not in the list) pi gets --no-approve.
func TestPopulatePIConfig_NoDecisionIsNoApprove(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")
	ctrCfg := container.Config{Worktree: filepath.Join(t.TempDir(), "wt")}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", config.Config{PIExtensionDir: "/nix/store/fake-ext"}, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}
	if ctrCfg.PIApproveProject {
		t.Error("PIApproveProject = true with no decision from the host")
	}
}
