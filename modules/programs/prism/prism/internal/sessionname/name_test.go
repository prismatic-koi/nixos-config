package sessionname

import "testing"

// TestRepo pins the repo-attribution rule: Repo derivation returns `obsidian`
// for both `obsidian` and `obsidian~investigate-v2`.
//
// A rule that split on "@" alone returns a name with no "@" whole, so a
// non-worktree session's descendants each become their own repo. The cases
// marked "was broken" below fail against such a rule.
//
// This file uses literal names (`obsidian`, `obsidian~investigate-v2`) rather
// than `prism-test`-prefixed fixtures. That is safe here and only here: Repo
// is a pure string function. It opens no database, starts no tmux session, and
// reads no host state, so no name in this file can collide with a live
// session. Every test in this package holds that property. The tests that DO
// touch a DB — internal/authz/root_session_test.go,
// internal/sidecar/host_api_bare_name_prompt_test.go — use `prism-test`
// fixtures, per the naming discipline.
func TestRepo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Worktree shapes.
		{"coordinator", "nixos-config@main", "nixos-config"},
		{"branch worker", "nixos-config@feature-x", "nixos-config"},
		{"review agent of a branch", "nixos-config@feature-x~review-1-review-goal", "nixos-config"},
		{"investigator of a coordinator", "nixos-config@main~investigate-flake", "nixos-config"},

		// Non-worktree shapes.
		{"bare name", "obsidian", "obsidian"},
		{"investigator of a bare name (was broken)", "obsidian~investigate-v2", "obsidian"},
		{"review agent of a bare name (was broken)", "obsidian~review-1-review-goal", "obsidian"},

		// "@" is tested before "~" so a repo directory whose own name holds
		// "~" is not shortened. A shorter repo could make a cross-repo target
		// look same-repo, which would widen a permission check.
		{"tilde inside the repo name", "weird~repo@main", "weird~repo"},

		// Degenerate input.
		{"empty", "", ""},
		{"leading @", "@main", ""},
		{"leading ~", "~investigate-x", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Repo(tc.in); got != tc.want {
				t.Errorf("Repo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsDescendant pins the guard that keeps a review agent or an investigator
// from ever being promoted to a coordinator or a root session.
func TestIsDescendant(t *testing.T) {
	descendants := []string{
		"obsidian~investigate-v2",
		"nixos-config@feature~review-1-review-goal",
		"nixos-config@main~investigate-flake",
		"~leading",
		// The tilde is in the branch part as well as the repo part, so this
		// IS a descendant.
		"weird~repo@main~review-1-review-goal",
	}
	roots := []string{
		"obsidian",
		"nixos-config@main",
		"nixos-config@feature",
		"",
		// The tilde is in the REPO part only. Searching the whole name would
		// wrongly demote this coordinator and take its merge queue away.
		"weird~repo@main",
		"weird~repo@feature",
	}
	for _, n := range descendants {
		if !IsDescendant(n) {
			t.Errorf("IsDescendant(%q) = false, want true", n)
		}
	}
	for _, n := range roots {
		if IsDescendant(n) {
			t.Errorf("IsDescendant(%q) = true, want false", n)
		}
	}
}

// TestIsMetaAndMetaNames pins that IsMeta and MetaNames agree. The SQL in
// db.AllActiveStatusForRepoAndOtherRootSessions binds MetaNames(), so a name
// added to one and not the other would let a meta-session appear in a
// cross-repo listing.
func TestIsMetaAndMetaNames(t *testing.T) {
	for _, n := range MetaNames() {
		if !IsMeta(n) {
			t.Errorf("MetaNames() lists %q but IsMeta(%q) = false", n, n)
		}
	}
	if len(MetaNames()) != 2 {
		t.Errorf("MetaNames() = %v, want exactly the two known meta-sessions", MetaNames())
	}
	for _, n := range []string{"obsidian", "nixos-config@main", "scratchpad-like", ""} {
		if IsMeta(n) {
			t.Errorf("IsMeta(%q) = true, want false", n)
		}
	}
}

// TestHasBranchAndCoordinatorSuffix pins the two name heuristics the
// coordinator and root predicates read.
func TestHasBranchAndCoordinatorSuffix(t *testing.T) {
	tests := []struct {
		in         string
		hasBranch  bool
		coordSuffx bool
	}{
		{"nixos-config@main", true, true},
		{"nixos-config@feature", true, false},
		{"obsidian", false, false},
		{"obsidian~investigate-v2", false, false},
		{"weird~repo@main", true, true},
		{"", false, false},
	}
	for _, tc := range tests {
		if got := HasBranch(tc.in); got != tc.hasBranch {
			t.Errorf("HasBranch(%q) = %v, want %v", tc.in, got, tc.hasBranch)
		}
		if got := HasCoordinatorSuffix(tc.in); got != tc.coordSuffx {
			t.Errorf("HasCoordinatorSuffix(%q) = %v, want %v", tc.in, got, tc.coordSuffx)
		}
	}
}
