package db_test

// Tests for SetGeneratedTitle and title provenance (#2683).

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openTitleTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "titles.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// strPtr is declared in db_test.go, which is part of the same test package.

// TestSetGeneratedTitle_WritesTitleAndIssueRef is the happy path: both
// columns land, and provenance is recorded as "generated".
func TestSetGeneratedTitle_WritesTitleAndIssueRef(t *testing.T) {
	d := openTitleTestDB(t)
	const session = "prism-test@titlegen"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	written, err := d.SetGeneratedTitle(session, "Generate session titles", strPtr("#2683"))
	if err != nil {
		t.Fatalf("SetGeneratedTitle: %v", err)
	}
	if !written {
		t.Fatal("SetGeneratedTitle returned written=false on a titleless row")
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Generate session titles" {
		t.Errorf("title = %v, want %q", st.Title, "Generate session titles")
	}
	if st.TitleSource == nil || *st.TitleSource != "generated" {
		t.Errorf("title_source = %v, want \"generated\"", st.TitleSource)
	}
	if st.IssueRef == nil || *st.IssueRef != "#2683" {
		t.Errorf("issue_ref = %v, want %q", st.IssueRef, "#2683")
	}
}

// TestSetGeneratedTitle_NilIssueRefStaysNull covers the edge-case AC: source
// text with no issue or ticket reference leaves issue_ref NULL. NULL is the
// answer "the text names no issue", and must never be filled with a guess.
func TestSetGeneratedTitle_NilIssueRefStaysNull(t *testing.T) {
	d := openTitleTestDB(t)
	const session = "prism-test@noref"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if _, err := d.SetGeneratedTitle(session, "Refactor the login flow", nil); err != nil {
		t.Fatalf("SetGeneratedTitle: %v", err)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.IssueRef != nil {
		t.Errorf("issue_ref = %q, want NULL for source text with no reference", *st.IssueRef)
	}
	if st.Title == nil || *st.Title != "Refactor the login flow" {
		t.Errorf("title = %v, want the generated title to be written regardless", st.Title)
	}
}

// TestSetGeneratedTitle_NeverOverwritesHumanTitle is the AC that the guard
// exists for. A human rename is the operator saying what the session is; a
// model summary must never talk over it.
func TestSetGeneratedTitle_NeverOverwritesHumanTitle(t *testing.T) {
	d := openTitleTestDB(t)
	const session = "prism-test@human"

	// A harness-reported title is a human rename — pi emits info.title only
	// in response to an explicit user rename — so this stamps 'human'.
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", strPtr("Operator chose this"), nil); err != nil {
		t.Fatalf("UpsertStatus with title: %v", err)
	}
	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.TitleSource == nil || *st.TitleSource != "human" {
		t.Fatalf("title_source after a harness title = %v, want \"human\"", st.TitleSource)
	}

	written, err := d.SetGeneratedTitle(session, "A model wrote this", strPtr("#1"))
	if err != nil {
		t.Fatalf("SetGeneratedTitle: %v", err)
	}
	if written {
		t.Error("SetGeneratedTitle reported a write against a human-titled row")
	}

	st, err = d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus after refused write: %v", err)
	}
	if st.Title == nil || *st.Title != "Operator chose this" {
		t.Errorf("title = %v, want the human title untouched", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "human" {
		t.Errorf("title_source = %v, want \"human\" preserved", st.TitleSource)
	}
	if st.IssueRef != nil {
		t.Errorf("issue_ref = %q, want NULL — the refusal covers the whole statement", *st.IssueRef)
	}
}

// TestSetGeneratedTitle_OverwritesFallbackTitle — the fallback is the
// weakest provenance and a model summary is meant to supersede it.
func TestSetGeneratedTitle_OverwritesFallbackTitle(t *testing.T) {
	d := openTitleTestDB(t)
	const session = "prism-test@fallback"

	if err := d.UpsertStatusSeedRootAgentName(
		session, "prism-test", "/tmp/w", "idle",
		strPtr("Please implement GitHub issue #2683 in this repo (nixos"),
		nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.TitleSource == nil || *st.TitleSource != "fallback" {
		t.Fatalf("title_source after spawn seed = %v, want \"fallback\"", st.TitleSource)
	}

	written, err := d.SetGeneratedTitle(session, "Generate session titles", strPtr("#2683"))
	if err != nil {
		t.Fatalf("SetGeneratedTitle: %v", err)
	}
	if !written {
		t.Fatal("SetGeneratedTitle refused to overwrite a fallback title")
	}
	st, err = d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Generate session titles" {
		t.Errorf("title = %v, want the generated title", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "generated" {
		t.Errorf("title_source = %v, want \"generated\"", st.TitleSource)
	}
}

// TestSetGeneratedTitle_HumanRenameStillWinsAfterGeneration verifies the
// precedence runs the other way too: once a title is generated, an operator
// rename still takes over, and the row is then permanently protected.
func TestSetGeneratedTitle_HumanRenameStillWinsAfterGeneration(t *testing.T) {
	d := openTitleTestDB(t)
	const session = "prism-test@rename"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if _, err := d.SetGeneratedTitle(session, "Model title", nil); err != nil {
		t.Fatalf("SetGeneratedTitle: %v", err)
	}
	// The operator renames the session; the harness reports it.
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", strPtr("My name for it"), nil); err != nil {
		t.Fatalf("UpsertStatus rename: %v", err)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "My name for it" {
		t.Errorf("title = %v, want the rename to win over a generated title", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "human" {
		t.Errorf("title_source = %v, want \"human\"", st.TitleSource)
	}

	// And it is now protected for good.
	written, err := d.SetGeneratedTitle(session, "Another model title", nil)
	if err != nil {
		t.Fatalf("SetGeneratedTitle after rename: %v", err)
	}
	if written {
		t.Error("SetGeneratedTitle overwrote a title the operator had just set")
	}
}

// TestSetGeneratedTitle_UnknownSessionIsNotAnError — a row that does not
// exist is a refusal, not a failure, and must not insert a phantom row.
func TestSetGeneratedTitle_UnknownSessionIsNotAnError(t *testing.T) {
	d := openTitleTestDB(t)
	written, err := d.SetGeneratedTitle("prism-test@nonexistent", "A title", nil)
	if err != nil {
		t.Fatalf("SetGeneratedTitle on an unknown session: %v", err)
	}
	if written {
		t.Error("SetGeneratedTitle reported a write for a session with no row")
	}
	st, err := d.CurrentStatus("prism-test@nonexistent")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st != nil {
		t.Error("SetGeneratedTitle inserted a phantom agent_status row")
	}
}

// TestUpsertStatusSeedRootAgentName_NilTitleLeavesSourceAlone verifies the
// provenance stamp is tied to actually writing a title. The seeder passes
// nil whenever a title already exists (the respawn-after-cleanup path), and
// that must not overwrite the existing provenance with "fallback".
func TestUpsertStatusSeedRootAgentName_NilTitleLeavesSourceAlone(t *testing.T) {
	d := openTitleTestDB(t)
	const session = "prism-test@preserve"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", strPtr("Human title"), nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// Respawn with no title, exactly as SpawnSession does when a title
	// already exists on the row.
	if err := d.UpsertStatusSeedRootAgentName(
		session, "prism-test", "/tmp/w", "idle", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Human title" {
		t.Errorf("title = %v, want the human title preserved across respawn", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "human" {
		t.Errorf("title_source = %v, want \"human\" preserved (a nil title must not restamp it)", st.TitleSource)
	}
}

// TestDisplayTitle covers the shared renderer helper used by the tmux
// dashboard and `prism sessions list`.
func TestDisplayTitle(t *testing.T) {
	cases := []struct {
		name  string
		title *string
		ref   *string
		want  string
	}{
		{"both", strPtr("Generate session titles"), strPtr("#2683"), "#2683 · Generate session titles"},
		{"title only", strPtr("Generate session titles"), nil, "Generate session titles"},
		{"ref only", nil, strPtr("PLAT-123"), "PLAT-123"},
		{"neither", nil, nil, ""},
		{"empty title with ref", strPtr(""), strPtr("#7"), "#7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := db.Status{Title: tc.title, IssueRef: tc.ref}.DisplayTitle()
			if got != tc.want {
				t.Errorf("DisplayTitle() = %q, want %q", got, tc.want)
			}
		})
	}
}
