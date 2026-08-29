package sidecar

// Tests for the repo scoping on `GET /merges/by-pr`.
//
// The endpoint accepts an optional `repo` query parameter; when omitted,
// the sidecar substitutes its own repo. Both behaviours must be exercised
// so that:
//
//   - proxyWaitProbe (which always passes repo) is repo-scoped end-to-end,
//     and
//   - ad-hoc callers / older clients that omit the parameter still cannot
//     cross-repo through this endpoint (the sidecar's own repo is a safe
//     default because the sidecar is bound to one repo).

import (
	"net/http"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestHostAPI_MergesByPR_RepoScoping_SidecarDefault verifies that a request
// without an explicit `repo=` query parameter is scoped to the sidecar's
// own repo. Even if a row with the same PR number exists for a DIFFERENT
// repo, the endpoint must return 404, not the foreign row.
func TestHostAPI_MergesByPR_RepoScoping_SidecarDefault(t *testing.T) {
	d := openTestDB(t)
	// Seed a row belonging to a foreign repo.
	if _, err := d.EnqueueMerge(42, "foreign-repo", "foreign-repo@main", "inst-foreign", nil); err != nil {
		t.Fatalf("EnqueueMerge foreign: %v", err)
	}
	if err := d.TerminateMerge(42, "foreign-repo", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge foreign: %v", err)
	}

	// The sidecar is bound to a different repo ("this-repo").
	sc := newSidecarWithRole(t, "this-repo@main", "this-repo", "coordinator", d)

	// No repo query parameter — the sidecar substitutes its own repo,
	// which is "this-repo". The foreign row must NOT be returned.
	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr?pr=42", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (foreign-repo row must be invisible without an explicit repo query); body = %s", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_MergesByPR_RepoScoping_ExplicitRepoQuery verifies that the
// `repo=` query parameter takes precedence over the sidecar's own repo.
// This is the path proxyWaitProbe (from cmd/wait_probe.go) uses: it
// always passes the caller's repo explicitly.
func TestHostAPI_MergesByPR_RepoScoping_ExplicitRepoQuery(t *testing.T) {
	d := openTestDB(t)
	// Seed a row in "repo-a".
	if _, err := d.EnqueueMerge(101, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	// The sidecar's own repo is unrelated.
	sc := newSidecarWithRole(t, "repo-b@main", "repo-b", "coordinator", d)

	// Explicit repo=repo-a — endpoint must return the repo-a row even
	// though the sidecar's own repo is repo-b.
	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr?pr=101&repo=repo-a", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var row db.PendingMerge
	decodeJSONBody(t, rr, &row)
	if row.PR != 101 {
		t.Errorf("PR: got %d, want 101", row.PR)
	}
	if row.Repo != "repo-a" {
		t.Errorf("repo: got %q, want repo-a", row.Repo)
	}
}

// TestHostAPI_MergesByPR_CrossRepoCollision_ReturnsCorrectRow reproduces
// the incident shape at the host-API level: two rows with the same PR
// number but different repos. The endpoint must return the row matching
// the explicit repo query, never the foreign one.
func TestHostAPI_MergesByPR_CrossRepoCollision_ReturnsCorrectRow(t *testing.T) {
	d := openTestDB(t)
	// Repo A: terminal merged row (the github-actions@main / dependabot
	// equivalent from the 2026-07-06 incident).
	titleA := "chore: bump deps"
	if _, err := d.EnqueueMerge(47, "repo-a", "repo-a@main", "inst-a", &titleA); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if err := d.TerminateMerge(47, "repo-a", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge repo-a: %v", err)
	}
	// Repo B: fresh watching row for its own (unrelated) PR #47.
	titleB := "feat: real work"
	if _, err := d.EnqueueMerge(47, "repo-b", "repo-b@main", "inst-b", &titleB); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	// The sidecar for repo-b calls /merges/by-pr with explicit repo=repo-b.
	sc := newSidecarWithRole(t, "repo-b@main", "repo-b", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr?pr=47&repo=repo-b", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var row db.PendingMerge
	decodeJSONBody(t, rr, &row)
	if row.Repo != "repo-b" {
		t.Errorf("repo: got %q, want repo-b (the endpoint returned the foreign repo-a row \u2014 the incident of 2026-07-06 has regressed)", row.Repo)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (foreign repo-a's terminal row is bleeding through)", row.Status)
	}
	if row.Title == nil || *row.Title != titleB {
		got := "<nil>"
		if row.Title != nil {
			got = *row.Title
		}
		t.Errorf("title: got %q, want %q", got, titleB)
	}
}
