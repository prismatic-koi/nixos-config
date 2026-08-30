package cmd

// Tests for agent_status.repo attribution.
//
// Several code paths write agent_status.repo. Each must derive the repo via
// the shared rule (sessionname.Repo). A path that splits on "@" alone
// attributes a descendant of a NON-worktree parent to a repo of its own:
// `obsidian~investigate-v2` instead of `obsidian`.
//
// The write in cmd/sidecar.go is the most damaging. `db.UpsertStatus` sets
// `repo = excluded.repo` with no COALESCE, and internal/sidecar/state.go
// re-writes the column from sidecar.Config.Repo on EVERY state transition, so
// the sidecar is the continuous LAST writer. internal/review can seed the
// correct value and the sidecar overwrites it milliseconds later. A test that
// only exercises the pure derivation cannot see that. These tests read the
// STORED value back.
//
// Two guards work together here, because neither is sufficient alone:
//
//   - TestStoredRepoSurvivesTheSidecarUpsert is behavioural. It fails when the
//     shared rule (sessionname.Repo) is wrong.
//   - TestNoPrivateRepoGrammarInAWriter is structural. It fails when a writer
//     stops calling the shared rule and grows its own again. A behavioural
//     test cannot catch that, because a private copy does not call the shared
//     rule.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/sessionname"
)

// TestStoredRepoSurvivesTheSidecarUpsert verifies that the stored repo, after
// the sidecar has written it, still attributes a descendant to its parent's
// repo.
//
// Negative-mutation guard: change sessionname.Repo to split on "@" alone and
// the three descendant cases fail with the parent-plus-suffix name.
func TestStoredRepoSurvivesTheSidecarUpsert(t *testing.T) {
	d := openStatsTestDB(t)

	cases := []struct {
		session string
		want    string
	}{
		// The problem shapes: a non-worktree parent and its descendants.
		{"bare-project", "bare-project"},
		{"bare-project~investigate-v2", "bare-project"},
		{"bare-project~review-1-review-goal", "bare-project"},
		// Worktree shapes were already correct under the old rule; they must
		// stay correct.
		{"wt-project@main", "wt-project"},
		{"wt-project@feature", "wt-project"},
		{"wt-project@feature~review-1-review-goal", "wt-project"},
		// A repo directory whose own name holds "~". The repo part is kept
		// whole.
		{"odd~name@main", "odd~name"},
		{"odd~name@feature~review-1-review-goal", "odd~name"},
	}

	for _, tc := range cases {
		t.Run(tc.session, func(t *testing.T) {
			// 1. The spawn/review path seeds the row with the correct repo.
			if err := d.UpsertStatusSeedRootAgentName(
				tc.session, tc.want, "/tmp/"+tc.session, "active", nil, nil, "worker", "pi", "host",
			); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// 2. `prism sidecar` starts and derives its own repo from the
			//    session name, then upserts. This is the call cmd/sidecar.go
			//    makes; TestNoPrivateRepoGrammarInAWriter is what keeps that
			//    call site from drifting away from this rule again.
			if err := d.UpsertStatus(tc.session, sessionname.Repo(tc.session), "/tmp/"+tc.session, "idle", nil, nil); err != nil {
				t.Fatalf("sidecar upsert: %v", err)
			}

			// 3. Read the column back — this is what every repo-scoped query
			//    and authz.RepoFromSession will see.
			status, err := d.CurrentStatus(tc.session)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			if status == nil {
				t.Fatalf("no row for %q", tc.session)
			}
			if status.Repo != tc.want {
				t.Errorf("stored repo for %q = %q, want %q — the sidecar upsert overwrote the seeded value",
					tc.session, status.Repo, tc.want)
			}
		})
	}
}

// TestNoPrivateRepoGrammarInAWriter fails when a file that WRITES
// agent_status.repo re-derives the repo from a session name itself, instead of
// calling sessionname.Repo.
//
// This is the defect class the test closes: a writer that re-derives the repo
// from a session name and silently overwrites the correct value at runtime.
//
// # Scope
//
// The file set is DERIVED, not hardcoded: every non-test file in this package
// that calls UpsertStatus. That is precisely the set that can persist a wrong
// repo, so there is no allowlist to rot. Files that split a session name for
// other reasons are not scanned and are unaffected — extracting the BRANCH
// (cmd/close.go, cmd/cleanup.go), building a display sort key
// (cmd/list_sessions.go), and scoping a read (cmd/merge.go) are all legitimate
// and out of scope here.
//
// The match is deliberately literal: `strings.Index` / `strings.Split` /
// `strings.Cut` against "@" on one line. It catches a copy-paste of the
// deleted expression, which is how every instance so far has arisen. It is not
// a proof that no other derivation is possible.
func TestNoPrivateRepoGrammarInAWriter(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(data)
		// Only a writer of the column can persist a wrong repo.
		if !strings.Contains(src, "UpsertStatus") {
			continue
		}
		scanned++
		for i, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, `"@"`) {
				continue
			}
			if strings.Contains(line, "strings.Index") ||
				strings.Contains(line, "strings.Split") ||
				strings.Contains(line, "strings.Cut") {
				t.Errorf("%s:%d writes agent_status.repo and also splits a session name on \"@\":\n\t%s\n"+
					"Call sessionname.Repo instead. A private copy of the grammar overwrites the stored "+
					"repo for a non-worktree session's descendant (#2658).",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}

	// The set is derived, so an empty set would pass silently. Pin that it is
	// non-empty and that the files the review named are still inside it.
	if scanned < 2 {
		t.Fatalf("scanned %d writer files — the guard is vacuous; expected at least sidecar.go and switch.go", scanned)
	}
	for _, must := range []string{"sidecar.go", "switch.go", "event.go"} {
		data, err := os.ReadFile(filepath.Clean(must))
		if err != nil {
			t.Fatalf("read %s: %v", must, err)
		}
		if !strings.Contains(string(data), "UpsertStatus") {
			t.Errorf("%s no longer calls UpsertStatus, so it dropped out of this guard's scope — "+
				"confirm it no longer writes agent_status.repo", must)
		}
	}
}
