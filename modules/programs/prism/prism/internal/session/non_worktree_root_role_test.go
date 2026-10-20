package session

// non_worktree_root_role_test.go — the start role of the root session of a
// non-worktree project (issue #3179).
//
// The root-session start path gives that session no role. A legacy
// root_agent_name ("worker" from before #954, or "review-goal" as in #2658)
// must not become the start role on restore, and the start path clears it so
// that agent_status agrees with the start config of the sidecar.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openNWRTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// nwrBareWorktree returns a worktree inside a fake bare+worktree layout.
func nwrBareWorktree(t *testing.T, branch string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".bare"), 0o755); err != nil {
		t.Fatalf("mkdir .bare: %v", err)
	}
	wt := filepath.Join(root, branch)
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	return wt
}

func TestDefaultAgentForSession_NonWorktreeRootIgnoresStaleRole(t *testing.T) {
	d := openNWRTestDB(t)
	plain := t.TempDir()
	wt := nwrBareWorktree(t, "feature")

	seed := func(session, dir, role string) {
		t.Helper()
		if err := d.UpsertStatusSeedRootAgentName(session, "x", dir, "idle", nil, nil, role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", session, err)
		}
	}
	seed("prism-test-root", plain, "worker")
	seed("prism-test-review", plain, "review-goal")
	seed("prism-test-root~investigate-v2", plain, "investigate")
	seed("prism-test-wt", wt, "worker")

	cases := []struct {
		session, dir, want, why string
	}{
		{"prism-test-root", plain, "", "a stale worker role must not become the start role of a non-worktree root"},
		{"prism-test-review", plain, "", "a stale review-goal role must not become the start role of a non-worktree root"},
		{"prism-test-root~investigate-v2", plain, "investigate", "a descendant keeps its recorded role"},
		{"prism-test-wt", wt, "worker", "a session in a worktree keeps its recorded role"},
	}
	for _, tc := range cases {
		if got := DefaultAgentForSession(tc.session, tc.dir, "", d); got != tc.want {
			t.Errorf("DefaultAgentForSession(%q) = %q, want %q — %s", tc.session, got, tc.want, tc.why)
		}
	}

	if got := DefaultAgentForSession("prism-test-root", plain, "worker", d); got != "worker" {
		t.Errorf("explicit role = %q, want %q (an explicit role still wins)", got, "worker")
	}
}

func TestClearStaleRootAgentName(t *testing.T) {
	plain := t.TempDir()
	wt := nwrBareWorktree(t, "feature")

	cases := []struct {
		name      string
		session   string
		dir       string
		agent     string
		wantClear bool
	}{
		{"role-less non-worktree root: cleared", "prism-test-root", plain, "", true},
		{"explicit role: kept", "prism-test-root", plain, "worker", false},
		{"descendant: kept", "prism-test-root~investigate-v2", plain, "", false},
		{"worktree session: kept", "prism-test-wt", wt, "", false},
		{"worktree-shaped name: kept", "prism-test@feature", plain, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openNWRTestDB(t)
			if err := d.UpsertStatusSeedRootAgentName(tc.session, "x", tc.dir, "idle", nil, nil, "worker", "", ""); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := clearStaleRootAgentName(d, tc.session, tc.dir, tc.agent); err != nil {
				t.Fatalf("clearStaleRootAgentName: %v", err)
			}
			name, _, err := d.RootAgentName(tc.session)
			if err != nil {
				t.Fatalf("RootAgentName: %v", err)
			}
			if tc.wantClear && name != "" {
				t.Errorf("root_agent_name = %q, want NULL", name)
			}
			if !tc.wantClear && name != "worker" {
				t.Errorf("root_agent_name = %q, want %q (unchanged)", name, "worker")
			}
		})
	}
}
