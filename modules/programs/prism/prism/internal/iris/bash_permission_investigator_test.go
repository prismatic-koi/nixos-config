package iris

// bash_permission_investigator_test.go — unit tests for the investigator
// branch of CheckBashPermission. The investigator deny list mirrors
// modules/programs/prism/agents/investigate.md verbatim; this test pins
// the canonical examples from that file so a future drift in either side
// is caught immediately.

import (
	"strings"
	"testing"
)

func TestCheckBashPermission_Investigator_DeniesMutations(t *testing.T) {
	denied := []struct {
		cmd     string
		mention string
	}{
		// Prism agent control — investigator may not spawn or steer agents.
		{"prism spawn --worktree /tmp/foo", "prism spawn"},
		{"prism review 1234", "prism review"},
		{"prism merge 1234", "prism merge"},
		{"prism merges", "prism merges"},

		// Iris agent control — same restriction on the iris side.
		{"iris spawn --worktree /tmp/foo --role worker", "iris spawn"},
		{"iris review 1234", "iris review"},
		{"iris merge 1234", "iris merge"},
		{"iris merges", "iris merges"},
		{"iris investigate --prompt foo", "iris investigate"},

		// GitHub mutations (issue/pr without a read-only subcommand).
		{"gh issue create --title foo", "gh issue"},
		{"gh issue edit 1 --title bar", "gh issue"},
		{"gh issue close 1", "gh issue"},
		{"gh issue comment 1 --body foo", "gh issue"},
		{"gh pr create --title foo", "gh pr"},
		{"gh pr edit 1 --title bar", "gh pr"},
		{"gh pr merge 1 --squash", "gh pr"},
		{"gh pr close 1", "gh pr"},
		{"gh pr review 1 --approve", "gh pr"},
		{"gh pr comment 1 --body foo", "gh pr"},

		// Git history mutation.
		{"git push origin main", "git push"},
		{"git commit -m foo", "git commit"},
		{"git add .", "git add"},
		{"git rebase -i HEAD~3", "git rebase"},
		{"git reset --hard HEAD~1", "git reset"},
	}
	for _, tc := range denied {
		tc := tc
		t.Run(tc.cmd, func(t *testing.T) {
			allowed, reason := CheckBashPermission("investigate", tc.cmd)
			if allowed {
				t.Fatalf("CheckBashPermission(investigate, %q) = allowed; expected denied", tc.cmd)
			}
			if !strings.Contains(reason, "read-only") {
				t.Errorf("reason = %q, want it to mention 'read-only'", reason)
			}
			if !strings.Contains(reason, tc.mention) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.mention)
			}
		})
	}
}

func TestCheckBashPermission_Investigator_AllowsReadOnly(t *testing.T) {
	allowed := []string{
		// Standard read-only utilities.
		"rg foo .",
		"grep -rn foo .",
		"find . -name '*.go'",
		"ls -la",
		"cat /etc/hostname",
		"head -n 20 README.md",
		"wc -l file.go",
		"diff a.txt b.txt",

		// Read-only git.
		"git log --oneline -10",
		"git diff HEAD",
		"git show HEAD",
		"git status",
		"git blame file.go",

		// Read-only GitHub (refinement: gh issue/pr view/list/diff).
		"gh issue view 1",
		"gh issue list",
		"gh pr view 1",
		"gh pr list",
		"gh pr diff 1",

		// Read-only prism introspection.
		"prism checkin nixos-config@main",
		"prism logs nixos-config@main",
		"prism sessions list",
	}
	for _, cmd := range allowed {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			ok, reason := CheckBashPermission("investigate", cmd)
			if !ok {
				t.Errorf("CheckBashPermission(investigate, %q) = denied (reason=%q); expected allowed", cmd, reason)
			}
		})
	}
}

// TestCheckBashPermission_NonInvestigatorRolesUnaffected asserts that the
// investigator deny list does NOT apply to worker / coordinator sessions.
// Workers can git push / git commit; coordinators can prism merge. The
// investigator branch must be role-keyed strictly to "investigate".
func TestCheckBashPermission_NonInvestigatorRolesUnaffected(t *testing.T) {
	cases := []struct {
		role, cmd string
		want      bool
	}{
		{"worker", "git push origin foo", true},
		{"worker", "git commit -m foo", true},
		{"worker", "gh pr create --title foo", true},
		{"coordinator", "git push origin main", true},
		{"coordinator", "prism merge 1234", true},
		// worker still denied prism merge (existing D-10 behaviour).
		{"worker", "prism merge 1234", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.role+":"+tc.cmd, func(t *testing.T) {
			got, _ := CheckBashPermission(tc.role, tc.cmd)
			if got != tc.want {
				t.Errorf("CheckBashPermission(%q, %q) = %v, want %v", tc.role, tc.cmd, got, tc.want)
			}
		})
	}
}
