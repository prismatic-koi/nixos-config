package sessionname

import "testing"

// TestRepo pins the repo-attribution rule (issue #2658, AC "Repo derivation
// returns `obsidian` for both `obsidian` and `obsidian~investigate-v2`").
//
// The pre-#2658 rule split on "@" alone. Every case marked "was broken" below
// FAILS against that rule: a name with no "@" was returned whole, so a
// non-worktree session's descendants each became their own repo.
func TestRepo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Worktree shapes — unchanged by #2658.
		{"coordinator", "nixos-config@main", "nixos-config"},
		{"branch worker", "nixos-config@feature-x", "nixos-config"},
		{"review agent of a branch", "nixos-config@feature-x~review-1-review-goal", "nixos-config"},
		{"investigator of a coordinator", "nixos-config@main~investigate-flake", "nixos-config"},

		// Non-worktree shapes — the #2658 repair.
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
	}
	roots := []string{
		"obsidian",
		"nixos-config@main",
		"nixos-config@feature",
		"",
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
