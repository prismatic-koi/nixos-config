package authz

// non_worktree_root_test.go — IsNonWorktreeRoot and IsNonWorktreeRootFromDB
// (issue #3179).
//
// The predicate gives the coordinator role to the root session of a
// non-worktree project. The grant comes from the start record (the start role
// and the start directory), never from the shape of the name. Each test below
// pins one arm. If a change makes the predicate answer from the name alone,
// the worker rows fail.

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// nonWorktreeDir returns a directory outside every bare+worktree layout.
func nonWorktreeDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// worktreeDir returns a worktree directory of a fake bare+worktree layout: a
// project root that holds ".bare", with the worktree as its child. This is
// the shape that git.BareRoot detects.
func worktreeDir(t *testing.T, branch string) string {
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

func TestIsNonWorktreeRoot(t *testing.T) {
	plain := nonWorktreeDir(t)
	wtFeature := worktreeDir(t, "feature")

	cases := []struct {
		name      string
		session   string
		startRole string
		startDir  string
		want      bool
	}{
		{"root start: bare name, no role, non-worktree dir", "obsidian", "", plain, true},

		// A worker with a name that looks like a root session stays a worker.
		{"bare-named worker: explicit worker role", "obsidian", "worker", plain, false},
		{"bare-named session started in a worktree", "obsidian", "", wtFeature, false},
		{"explicit coordinator role is not this arm", "obsidian", "coordinator", plain, false},

		// A descendant never gets the coordinator role.
		{"investigator of a bare-name root", "obsidian~investigate-v2", "", plain, false},
		{"review agent of a bare-name root", "obsidian~review-1-review-goal", "", plain, false},
		{"review agent of a worktree session", "repo@feat~review-1-review-code", "", plain, false},

		{"meta-session scratchpad", "scratchpad", "", plain, false},
		{"meta-session dashboard", "prism-dashboard", "", plain, false},
		{"worktree-shaped name", "repo@feature", "", plain, false},
		{"coordinator-shaped name", "repo@main", "", plain, false},
		{"no start directory", "obsidian", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNonWorktreeRoot(tc.session, tc.startRole, tc.startDir); got != tc.want {
				t.Errorf("IsNonWorktreeRoot(%q, %q, %q) = %v, want %v",
					tc.session, tc.startRole, tc.startDir, got, tc.want)
			}
		})
	}
}

func TestIsNonWorktreeRootFromDB(t *testing.T) {
	d := openRootSessionTestDB(t)
	plain := nonWorktreeDir(t)
	wtFeature := worktreeDir(t, "feature")

	seed := func(session, worktree, role string) {
		t.Helper()
		if err := d.UpsertStatusSeedRootAgentName(session, "x", worktree, "idle", nil, nil, role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", session, err)
		}
	}
	seed("prism-test-root", plain, "")
	seed("prism-test-bareworker", plain, "worker")
	seed("prism-test-stale", plain, "review-goal")
	seed("prism-test-inwt", wtFeature, "")
	seed("prism-test-root~investigate-v2", plain, "")

	cases := []struct {
		session string
		want    bool
		why     string
	}{
		{"prism-test-root", true, "NULL role and a non-worktree worktree is a root start"},
		{"prism-test-bareworker", false, "a recorded worker role stays a worker"},
		{"prism-test-stale", false, "a stale role is not a root start until the start path clears it"},
		{"prism-test-inwt", false, "a worktree inside a bare+worktree layout is not a root start"},
		{"prism-test-root~investigate-v2", false, "a descendant is never a root"},
		{"prism-test-ghost", false, "no row fails closed"},
	}
	for _, tc := range cases {
		if got := IsNonWorktreeRootFromDB(tc.session, d); got != tc.want {
			t.Errorf("IsNonWorktreeRootFromDB(%q) = %v, want %v — %s", tc.session, got, tc.want, tc.why)
		}
	}

	if IsNonWorktreeRootFromDB("prism-test-root", nil) {
		t.Error("IsNonWorktreeRootFromDB with a nil DB = true, want false (fail closed)")
	}

	if err := d.ClearRootAgentName("prism-test-stale"); err != nil {
		t.Fatalf("ClearRootAgentName: %v", err)
	}
	if !IsNonWorktreeRootFromDB("prism-test-stale", d) {
		t.Error("after ClearRootAgentName, the stale root row is still not a root start")
	}
}

// TestAuthorizeCheckin_NonWorktreeRootFlagCannotPromoteADescendant pins the
// defence-in-depth guard in callerIsCoordinator and privilegeGranted. A route
// that sets CallerIsNonWorktreeRoot for a descendant by mistake must still
// get tier 1, and must not get the tier-3 privilege.
func TestAuthorizeCheckin_NonWorktreeRootFlagCannotPromoteADescendant(t *testing.T) {
	d := openRootSessionTestDB(t)
	const caller = "prism-test-root~investigate-v2"
	if err := d.UpsertStatusSeedRootAgentName(caller, "prism-test-root", nonWorktreeDir(t), "idle", nil, nil, "", "", ""); err != nil {
		t.Fatalf("seed caller: %v", err)
	}

	// The coordinator target is in reach at tier 2, so it pins the guard in
	// callerIsCoordinator. The worker target is in reach only at tier 3, so it
	// pins the guard in privilegeGranted.
	targets := map[string]string{
		"prism-test-other@main":    "coordinator",
		"prism-test-other@feature": "worker",
	}
	for target, role := range targets {
		if err := d.UpsertStatusSeedRootAgentName(target, "prism-test-other", "/tmp/"+target, "idle", nil, nil, role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", target, err)
		}
	}

	for target := range targets {
		got := AuthorizeCheckin(CheckinRequest{
			Caller:                  caller,
			Target:                  target,
			DB:                      d,
			PrivilegedRepos:         []string{"prism-test-root"},
			CallerIsNonWorktreeRoot: true,
			Logger:                  quietLogger(),
		})
		if got.Allow {
			t.Errorf("target %q: descendant caller admitted at tier %d — CallerIsNonWorktreeRoot must not promote a \"~\" session", target, got.Tier)
			continue
		}
		if got.Status != http.StatusForbidden {
			t.Errorf("target %q: status = %d, want %d", target, got.Status, http.StatusForbidden)
		}
	}
}
