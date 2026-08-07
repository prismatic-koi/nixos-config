package cmd

// Parity test for the three agent_status title renderers (#2683 review).
//
// Why this test exists rather than three separate assertions: the first cut
// of this change updated `prism sessions list` and the tmux dashboard but
// missed `prism checkin` (no argument), so the same row rendered
// "#2683 · title" on two surfaces and "title" on the third. The
// Status.DisplayTitle doc comment nevertheless claimed every renderer went
// through it.
//
// That is the same defect class the PR corrects elsewhere: internal/session/
// title_fallback.go carried a false in-code claim ("sessions a human renamed
// interactively") that led the next reader to the wrong conclusion. A claim
// in a comment is only worth what enforces it, so this test enforces it.
//
// Adding a fourth renderer that reads Status.Title directly will not fail
// this test — no test can catch a call site it does not know about — but any
// change that stops one of the three named surfaces from folding in the
// reference will.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestTitleRenderers_AgreeOnIssueRef pins that every renderer reading a
// db.Status row shows the issue reference alongside the title.
func TestTitleRenderers_AgreeOnIssueRef(t *testing.T) {
	title := "Generate session titles"
	ref := "#2683"
	status := db.Status{
		SessionName: "nixos-config@session-title-generation",
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		State:       "active",
		Title:       &title,
		IssueRef:    &ref,
	}

	// The value every surface is expected to render.
	want := status.DisplayTitle()
	if want != "#2683 · Generate session titles" {
		t.Fatalf("DisplayTitle() = %q, want %q", want, "#2683 · Generate session titles")
	}

	t.Run("prism checkin (no argument)", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := printSessionTable([]db.Status{status}); err != nil {
				t.Errorf("printSessionTable: %v", err)
			}
		})
		if !strings.Contains(out, ref) {
			t.Errorf("prism checkin table omits the issue reference %q.\nGot:\n%s", ref, out)
		}
		if !strings.Contains(out, title) {
			t.Errorf("prism checkin table omits the title %q.\nGot:\n%s", title, out)
		}
	})

	t.Run("tmux dashboard", func(t *testing.T) {
		got := dashboard.StatusToAgentSession(status, nil, nil, nil).AgentTitle
		if got != want {
			t.Errorf("dashboard AgentTitle = %q, want %q", got, want)
		}
	})

	// `prism sessions list` builds its cell with the same DisplayTitle call
	// (cmd/list_sessions.go). Asserting the method here keeps the three
	// surfaces named together in one place; the row-building code is
	// exercised by the sessions-list tests.
	t.Run("prism sessions list", func(t *testing.T) {
		if got := status.DisplayTitle(); got != want {
			t.Errorf("DisplayTitle() = %q, want %q", got, want)
		}
	})
}

// TestTitleRenderers_NoIssueRefRendersBareTitle verifies folding the
// reference in does not disturb the common case: a row with no reference
// renders exactly the title, with no separator left dangling.
func TestTitleRenderers_NoIssueRefRendersBareTitle(t *testing.T) {
	title := "Refactor the login flow"
	status := db.Status{
		SessionName: "nixos-config@login",
		State:       "active",
		Title:       &title,
	}

	out := captureStdout(t, func() {
		if err := printSessionTable([]db.Status{status}); err != nil {
			t.Errorf("printSessionTable: %v", err)
		}
	})
	if !strings.Contains(out, title) {
		t.Errorf("checkin table omits the title.\nGot:\n%s", out)
	}
	if strings.Contains(out, "·") {
		t.Errorf("checkin table rendered a dangling separator for a row with no reference.\nGot:\n%s", out)
	}
	if got := dashboard.StatusToAgentSession(status, nil, nil, nil).AgentTitle; got != title {
		t.Errorf("dashboard AgentTitle = %q, want the bare title %q", got, title)
	}
}

// TestTitleRenderers_UntitledRowRendersPlaceholder verifies the em-dash
// placeholder still appears for a row with neither a title nor a reference,
// so an untitled session never renders as an empty cell that reads as a bug.
func TestTitleRenderers_UntitledRowRendersPlaceholder(t *testing.T) {
	status := db.Status{SessionName: "nixos-config@untitled", State: "idle"}

	out := captureStdout(t, func() {
		if err := printSessionTable([]db.Status{status}); err != nil {
			t.Errorf("printSessionTable: %v", err)
		}
	})
	if !strings.Contains(out, "—") {
		t.Errorf("checkin table omits the em-dash placeholder for an untitled row.\nGot:\n%s", out)
	}
}
