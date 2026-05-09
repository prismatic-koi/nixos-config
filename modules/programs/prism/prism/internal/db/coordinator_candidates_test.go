package db

import (
	"path/filepath"
	"testing"
)

// TestCoordinatorCandidatesForRepo_Single covers the discovery primitive used
// by `prism escalate` for the auto-discovery path: exactly one same-repo
// active coordinator returned.
func TestCoordinatorCandidatesForRepo_Single(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	role := "coordinator"
	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/wt", "active", nil, nil, &role, nil); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	// Add an unrelated worker in the same repo — should NOT be returned.
	worker := "worker"
	if err := d.UpsertStatusWithRootAgent("repo@feature", "repo", "/wt2", "active", nil, nil, &worker, nil); err != nil {
		t.Fatalf("seed worker: %v", err)
	}

	got, err := d.CoordinatorCandidatesForRepo("repo")
	if err != nil {
		t.Fatalf("CoordinatorCandidatesForRepo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(got))
	}
	if got[0].SessionName != "repo@main" {
		t.Errorf("candidate session = %q, want %q", got[0].SessionName, "repo@main")
	}
}

// TestCoordinatorCandidatesForRepo_LegacyAtMainRow covers the fallback path:
// a pre-migration row named <repo>@main with NULL root_agent_name is still
// treated as a candidate.
func TestCoordinatorCandidatesForRepo_LegacyAtMainRow(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	// nil rootAgent → root_agent_name remains NULL (pre-migration shape).
	if err := d.UpsertStatus("repo@main", "repo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	got, err := d.CoordinatorCandidatesForRepo("repo")
	if err != nil {
		t.Fatalf("CoordinatorCandidatesForRepo: %v", err)
	}
	if len(got) != 1 || got[0].SessionName != "repo@main" {
		t.Fatalf("legacy candidate not returned: got=%+v", got)
	}
}

// TestCoordinatorCandidatesForRepo_Zero covers the no-coordinator branch.
func TestCoordinatorCandidatesForRepo_Zero(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	worker := "worker"
	if err := d.UpsertStatusWithRootAgent("repo@feature", "repo", "/wt", "active", nil, nil, &worker, nil); err != nil {
		t.Fatalf("seed worker: %v", err)
	}

	got, err := d.CoordinatorCandidatesForRepo("repo")
	if err != nil {
		t.Fatalf("CoordinatorCandidatesForRepo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("candidate count = %d, want 0", len(got))
	}
}

// TestCoordinatorCandidatesForRepo_Multiple covers the ambiguous branch: a
// legacy <repo>@main row alongside an explicit coordinator on a different
// branch (the unique index permits this combination because it filters on
// root_agent_name='coordinator' only).
func TestCoordinatorCandidatesForRepo_Multiple(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := d.UpsertStatus("repo@main", "repo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	role := "coordinator"
	if err := d.UpsertStatusWithRootAgent("repo@coord-2", "repo", "/wt2", "active", nil, nil, &role, nil); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}

	got, err := d.CoordinatorCandidatesForRepo("repo")
	if err != nil {
		t.Fatalf("CoordinatorCandidatesForRepo: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("candidate count = %d, want 2", len(got))
	}
}

// TestCoordinatorCandidatesForRepo_ExcludesOtherRepo verifies that candidates
// from other repos are not surfaced (the discovery is repo-scoped per the
// out-of-scope note about cross-repo escalation).
func TestCoordinatorCandidatesForRepo_ExcludesOtherRepo(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	role := "coordinator"
	if err := d.UpsertStatusWithRootAgent("repo-a@main", "repo-a", "/wt", "active", nil, nil, &role, nil); err != nil {
		t.Fatalf("seed coordinator a: %v", err)
	}
	if err := d.UpsertStatusWithRootAgent("repo-b@main", "repo-b", "/wt", "active", nil, nil, &role, nil); err != nil {
		t.Fatalf("seed coordinator b: %v", err)
	}

	got, err := d.CoordinatorCandidatesForRepo("repo-a")
	if err != nil {
		t.Fatalf("CoordinatorCandidatesForRepo: %v", err)
	}
	if len(got) != 1 || got[0].SessionName != "repo-a@main" {
		t.Errorf("got=%+v, want only repo-a@main", got)
	}
}
