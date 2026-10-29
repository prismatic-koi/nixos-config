package cmd

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

// withFakeSpawnBudget replaces the budget lookup with one that returns r and
// counts its calls. Only call this from non-parallel tests.
func withFakeSpawnBudget(t *testing.T, r spawnbudget.Result) *int {
	t.Helper()
	calls := 0
	orig := spawnBudgetLookup
	spawnBudgetLookup = func(config.Config) spawnbudget.Result {
		calls++
		return r
	}
	t.Cleanup(func() { spawnBudgetLookup = orig })
	return &calls
}

func budgetLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, spawnbudget.LinePrefix) {
			out = append(out, line)
		}
	}
	return out
}

var overBudget = spawnbudget.Result{Count: 196, Limit: 120, Known: true, Fleet: true}

func TestPlannedSpawnRole(t *testing.T) {
	cases := []struct {
		agent, branch, want string
	}{
		{"", "main", "coordinator"},
		{"", "fix-login", "worker"},
		{"investigate", "fix-login", "investigate"},
		{"worker", "main", "worker"},
		{"plan", "x", "plan"},
	}
	for _, tc := range cases {
		if got := plannedSpawnRole(tc.agent, tc.branch); got != tc.want {
			t.Errorf("plannedSpawnRole(%q, %q) = %q, want %q", tc.agent, tc.branch, got, tc.want)
		}
	}
}

func TestPrintWorkerSpawnBudget_OnlyAWorkerPrintsALine(t *testing.T) {
	for _, role := range []string{"coordinator", "investigate", "review-code", "review-goal", "plan", ""} {
		calls := withFakeSpawnBudget(t, overBudget)
		var buf bytes.Buffer
		if r := printWorkerSpawnBudget(&buf, role, config.Config{}); r != nil {
			t.Errorf("role %q: result = %+v, want nil", role, r)
		}
		if buf.Len() != 0 || *calls != 0 {
			t.Errorf("role %q: printed %q after %d lookups, want nothing and no lookup", role, buf.String(), *calls)
		}
	}

	calls := withFakeSpawnBudget(t, overBudget)
	var buf bytes.Buffer
	r := printWorkerSpawnBudget(&buf, "worker", config.Config{})
	if r == nil || r.Count != 196 {
		t.Fatalf("worker: result = %+v, want the lookup result", r)
	}
	if *calls != 1 {
		t.Errorf("worker: %d lookups, want 1", *calls)
	}
	if got := buf.String(); got != overBudget.Line()+"\n" {
		t.Errorf("worker: printed %q, want exactly the one budget line", got)
	}
}

// The wiring in runSpawn: a worker spawn prints one budget line, even when a
// later step fails, and the over-limit result does not stop the spawn
// before its worktree step.
func TestRunSpawn_WorkerPrintsOneBudgetLine(t *testing.T) {
	dbFile := setupIsolatedSpawn(t)
	_ = openTestDBAt(t, dbFile)
	bareRoot := initBareRepoWithMainWorktree(t, t.TempDir(), "myrepo")
	t.Setenv("PRISM_BARE_ROOT", bareRoot)
	withAllFailTmux(t)
	calls := withFakeSpawnBudget(t, overBudget)

	cmd := buildSpawnCmdForMainReuseTest(t)
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	_ = cmd.Flags().Set("branch", "feat-budget")
	_ = cmd.Flags().Set("isolation", "host")
	_ = cmd.Flags().Set("prompt", "do the mahi")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn: expected the failing tmux stub to fail the spawn")
	}
	if strings.Contains(err.Error(), "spawn budget") || strings.Contains(err.Error(), "OVER") {
		t.Errorf("the spawn failed because of the budget: %v", err)
	}
	if *calls != 1 {
		t.Errorf("budget lookups = %d, want 1", *calls)
	}
	if lines := budgetLines(stderr.String()); len(lines) != 1 || lines[0] != overBudget.Line() {
		t.Errorf("budget lines on stderr = %q, want exactly [%q]", lines, overBudget.Line())
	}
}

func TestRunSpawn_NonWorkerPrintsNoBudgetLine(t *testing.T) {
	cases := []struct {
		name, branch, agent string
	}{
		{name: "coordinator on main", branch: "main"},
		{name: "investigator", branch: "feat-inv", agent: "investigate"},
		{name: "review agent role", branch: "feat-rev", agent: "review-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbFile := setupIsolatedSpawn(t)
			_ = openTestDBAt(t, dbFile)
			bareRoot := initBareRepoWithMainWorktree(t, t.TempDir(), "myrepo")
			t.Setenv("PRISM_BARE_ROOT", bareRoot)
			withAllFailTmux(t)
			calls := withFakeSpawnBudget(t, overBudget)

			cmd := buildSpawnCmdForMainReuseTest(t)
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			_ = cmd.Flags().Set("branch", tc.branch)
			_ = cmd.Flags().Set("isolation", "host")
			_ = cmd.Flags().Set("prompt", "do the mahi")
			if tc.agent != "" {
				_ = cmd.Flags().Set("agent", tc.agent)
			}

			_ = runSpawn(cmd, nil) // the failing tmux stub fails the spawn; that is not the assertion
			if *calls != 0 {
				t.Errorf("budget lookups = %d, want 0", *calls)
			}
			if lines := budgetLines(stderr.String()); len(lines) != 0 {
				t.Errorf("budget lines on stderr = %q, want none", lines)
			}
		})
	}
}

// On the host-API path the sidecar returns the line in "spawn_budget", and
// the sandboxed caller prints it once.
func TestProxySpawn_PrintsRelayedBudgetLine(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"relayed", `{"session_name":"r@b","spawn_budget":"` + spawnbudget.LinePrefix + ` 5 of 120"}`, []string{spawnbudget.LinePrefix + " 5 of 120"}},
		{"absent", `{"session_name":"r@b"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			})
			t.Setenv("PRISM_HOST_API", srv.apiURL())

			cmd := newSpawnTestCmd()
			cmd.Flags().Bool("wait", false, "")
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			_ = cmd.Flags().Set("branch", "b")
			_ = cmd.Flags().Set("prompt", "hi")

			if err := proxySpawn(srv.apiURL(), cmd); err != nil {
				t.Fatalf("proxySpawn: %v", err)
			}
			got := budgetLines(stderr.String())
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("budget lines = %q, want %q", got, tc.want)
			}
		})
	}
}

func budgetCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "spawn"}
	c.Flags().String("over-budget", "", overBudgetHelp)
	var buf bytes.Buffer
	c.SetErr(&buf)
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnforceWorkerSpawnBudget(t *testing.T) {
	under := spawnbudget.Result{Count: 5, Limit: 120, Known: true}
	unknown := spawnbudget.Result{Limit: 120}
	bare := t.TempDir()
	cases := []struct {
		name    string
		role    string
		res     spawnbudget.Result
		args    []string
		wantErr string
	}{
		{"over refuses", "worker", overBudget, nil, "--over-budget"},
		{"over with reason starts", "worker", overBudget, []string{"--over-budget", "urgent"}, ""},
		{"empty reason refused", "worker", overBudget, []string{"--over-budget", ""}, "needs a reason"},
		{"empty reason under limit refused", "worker", under, []string{"--over-budget", " "}, "needs a reason"},
		{"under starts", "worker", under, nil, ""},
		{"unknown count starts", "worker", unknown, nil, ""},
		{"coordinator over starts", "coordinator", overBudget, nil, ""},
		{"review over starts", "review-code", overBudget, nil, ""},
		{"investigate over starts", "investigate", overBudget, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeSpawnBudget(t, tc.res)
			c := budgetCmd(t, tc.args...)
			err := enforceWorkerSpawnBudget(c, tc.role, config.Config{}, bare, "fix-x")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	t.Run("refusal message gives count and limit", func(t *testing.T) {
		withFakeSpawnBudget(t, overBudget)
		err := enforceWorkerSpawnBudget(budgetCmd(t), "worker", config.Config{}, bare, "x")
		if err == nil || !strings.Contains(err.Error(), "196 of 120") {
			t.Fatalf("err = %v, want count and limit", err)
		}
	})
}
