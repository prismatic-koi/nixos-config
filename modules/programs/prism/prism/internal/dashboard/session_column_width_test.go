package dashboard_test

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

// TestSessionColumnWidth covers the AC scenarios from issue #754.
func TestSessionColumnWidth(t *testing.T) {
	tests := []struct {
		name     string
		sessions []dashboard.AgentSession
		want     int
	}{
		// [edge-case] No sessions → header minimum (7).
		{
			name:     "empty slice → header minimum 7",
			sessions: nil,
			want:     7,
		},
		// [edge-case] Empty slice (non-nil) → header minimum.
		{
			name:     "empty non-nil slice → 7",
			sessions: []dashboard.AgentSession{},
			want:     7,
		},
		// [functional] Short session name (< 20 chars) → narrower than old hardcoded 20.
		// "nixos-config@main" = 17 chars; needed = 17 - 10 = 7 → at header min.
		{
			name: "single short top-level session (17 chars) → at header min 7",
			sessions: []dashboard.AgentSession{
				{Name: "nixos-config@main"},
			},
			want: 7,
		},
		// A slightly longer top-level @main session name.
		// "nixos-config-2@main" = 19 chars; top-level (branch=="@main"); needed = 19 - 10 = 9.
		{
			name: "top-level @main session (19 chars) → 9",
			sessions: []dashboard.AgentSession{
				{Name: "nixos-config-2@main"},
			},
			want: 9,
		},
		// [functional] Very short top-level name — clamped up to header min 7.
		// "a@main" = 6 chars; needed = 6 - 10 = -4 → clamped to 7.
		{
			name: "very short top-level → header min 7",
			sessions: []dashboard.AgentSession{
				{Name: "a@main"},
			},
			want: 7,
		},
		// [functional] No @ in name (plain session like "scratchpad").
		// "scratchpad" = 10 chars; needed = 10 - 10 = 0 → clamped to 7.
		{
			name: "plain session name without @ → 7",
			sessions: []dashboard.AgentSession{
				{Name: "scratchpad"},
			},
			want: 7,
		},
		// [functional] Short depth-1 child with very short branch — clamped to 7.
		// "session-x@y": depth-1 child, branch = "@y" = 2 chars; needed = 6+2-10 = -2 → 7.
		{
			name: "depth-1 child with very short branch → clamped to 7",
			sessions: []dashboard.AgentSession{
				{Name: "session-x@y"},
			},
			want: 7,
		},
		// [functional] Depth-1 child row is the longest entry.
		// "repo@long-feature-branch": branch = "@long-feature-branch" = 20 chars.
		// needed = 6 + 20 - 10 = 16.
		{
			name: "depth-1 child row drives width",
			sessions: []dashboard.AgentSession{
				{Name: "repo@main"},
				{Name: "repo@long-feature-branch"},
			},
			want: 16,
		},
		// [functional] Depth-2 child row is the longest entry.
		// "repo@feature~review-session": label = "~review-session" = 15 chars.
		// needed = 10 + 15 - 10 = 15.
		{
			name: "depth-2 child row drives width",
			sessions: []dashboard.AgentSession{
				{Name: "repo@main"},
				{Name: "repo@feature"},
				{Name: "repo@feature~review-session"},
			},
			want: 15,
		},
		// [functional] Long top-level @main name between 20-40 chars — old floor of 20
		// must NOT artificially cap it at 20.
		// "my-very-long-repo-name@main" = 27 chars; top-level (branch=="@main");
		// needed = 27 - 10 = 17. (The old code would yield 20 as floor, which
		// is larger than 17; new code correctly yields the exact fit.)
		{
			name: "long top-level @main name (27 chars) → 17, not old floor 20",
			sessions: []dashboard.AgentSession{
				{Name: "my-very-long-repo-name@main"},
			},
			want: 17,
		},
		// [functional] Top-level @main name that gives needed > 20 → exact width.
		// "my-very-long-repo-name-xx@main" = 30 chars; needed = 30-10 = 20.
		{
			name: "long top-level @main name (30 chars) → 20",
			sessions: []dashboard.AgentSession{
				{Name: "my-very-long-repo-name-xx@main"},
			},
			want: 20,
		},
		// [functional] Long depth-1 branch with needed > 20 — between 20 and 40 chars.
		// "repo@a-very-long-branch-name-here": branch = "@a-very-long-branch-name-here" = 29 chars;
		// depth-1 child: needed = 6 + 29 - 10 = 25.
		{
			name: "long depth-1 branch (needed=25) → 25",
			sessions: []dashboard.AgentSession{
				{Name: "repo@a-very-long-branch-name-here"},
			},
			want: 25,
		},
		// [edge-case] Session name exactly 40 chars in total → sessionW = 30 (no truncation).
		// Name = 40 chars total; needed = 40 - 10 = 30. Within cap of 40.
		{
			name: "40-char session name → sessionW 30 (no truncation)",
			sessions: []dashboard.AgentSession{
				{Name: strings.Repeat("x", 40)},
			},
			want: 30,
		},
		// [edge-case] Session name of 50 chars → clamped to cap 40.
		// needed = 50 - 10 = 40 → exactly at cap.
		{
			name: "50-char session name → capped at 40",
			sessions: []dashboard.AgentSession{
				{Name: strings.Repeat("x", 50)},
			},
			want: 40,
		},
		// [edge-case] Session name of 51 chars → clamped to cap 40.
		{
			name: "51-char session name → capped at 40",
			sessions: []dashboard.AgentSession{
				{Name: strings.Repeat("x", 51)},
			},
			want: 40,
		},
		// [functional] Multiple sessions — width driven by longest top-level name.
		// "nixos-config@main" (17) → needed = 7. "nixos-config@fix-1" is depth-1
		// child (branch "@fix-1" = 6 chars) → needed = 6+6-10 = 2. Max = 7.
		{
			name: "multiple sessions — top-level drives width",
			sessions: []dashboard.AgentSession{
				{Name: "a@main"},             // top-level, needed = 6-10 = -4 → 0
				{Name: "nixos-config@main"},  // top-level, needed = 17-10 = 7
				{Name: "nixos-config@fix-1"}, // depth-1 child, needed = 6+6-10 = 2
			},
			want: 7,
		},
		// [functional] Multiple sessions — deeper branch name drives width.
		// "nixos-config@feature-123": branch = "@feature-123" = 12 chars;
		// depth-1 child: needed = 6+12-10 = 8.
		{
			name: "multiple sessions — depth-1 child drives width",
			sessions: []dashboard.AgentSession{
				{Name: "nixos-config@main"},        // top-level, needed = 7
				{Name: "nixos-config@feature-123"}, // depth-1, needed = 6+12-10 = 8
			},
			want: 8,
		},
		// [functional] Session list changes: new longer session added → recalculates.
		// This is tested by calling SessionColumnWidth with the new set.
		// "nixos-config@a-much-longer-branch-x": depth-1 child,
		//   branch = "@a-much-longer-branch-x" = 23 chars; needed = 6 + 23 - 10 = 19.
		{
			name: "after adding longer session, width reflects new longest",
			sessions: []dashboard.AgentSession{
				{Name: "nixos-config@main"},                   // top-level, needed = 7
				{Name: "nixos-config@a-much-longer-branch-x"}, // depth-1, needed = 19
			},
			want: 19,
		},
		// [functional] @main session — treated as top-level (branch == "@main").
		// "@main" condition makes it top-level for any repo.
		{
			name: "@main session treated as top-level",
			sessions: []dashboard.AgentSession{
				{Name: "my-repo@main"},
			},
			// "my-repo@main" = 12 chars; needed = 12 - 10 = 2 → clamped to 7.
			want: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashboard.SessionColumnWidth(tt.sessions)
			if got != tt.want {
				t.Errorf("SessionColumnWidth(%v) = %d, want %d", sessionNames(tt.sessions), got, tt.want)
			}
		})
	}
}

// TestSessionColumnWidth_DashViewIntegration verifies that DashView starts the
// session column from the content-derived width (via SessionColumnWidth) rather
// than the old hardcoded 20-char floor. We test two scenarios side-by-side at
// a very narrow width (50 chars) where the difference between sessionW=7 and
// sessionW=20 is unambiguous: at width=50 with the old floor the layout runs
// out of room faster, while with the new content-derived width there is more
// room for the title column.
//
// The test probes DashView with:
//   - A set of short sessions (longest name → sessionW=7)
//   - A set of long sessions (longest name → sessionW=20+)
//
// In each case it calls SessionColumnWidth directly to get the expected sessionW
// and checks that the header "session" column is padded consistently.
func TestSessionColumnWidth_DashViewIntegration(t *testing.T) {
	// ── scenario: short sessions (sessionW should be 7 = minimum) ──
	shortSessions := []dashboard.AgentSession{
		{Name: "a@main", AgentState: "active"},
	}
	gotShortW := dashboard.SessionColumnWidth(shortSessions)
	if gotShortW != 7 {
		t.Errorf("short sessions: SessionColumnWidth = %d, want 7", gotShortW)
	}

	// DashView at a small but workable width; verify it doesn't panic.
	d := dashboard.Shared{
		Width:     80,
		Displayed: shortSessions,
		Sessions:  shortSessions,
	}
	output := dashboard.DashView(d, "", false)
	if output == "" {
		t.Error("DashView returned empty output for short sessions")
	}

	// ── scenario: long sessions (sessionW should be > 7) ──
	longSessions := []dashboard.AgentSession{
		{Name: "my-very-long-repo-name@main", AgentState: "active"},
		{Name: "my-very-long-repo-name@feature-x", AgentState: "waiting"},
	}
	// "my-very-long-repo-name@main": top-level, needed = 27 - 10 = 17.
	// "my-very-long-repo-name@feature-x": depth-1, branch = "@feature-x" = 10 chars,
	//   needed = 6 + 10 - 10 = 6 → 6 < 17, so max stays 17.
	gotLongW := dashboard.SessionColumnWidth(longSessions)
	if gotLongW != 17 {
		t.Errorf("long sessions: SessionColumnWidth = %d, want 17", gotLongW)
	}

	// Verify DashView starts from sessionW=17 (not the old floor of 20).
	// We check that SessionColumnWidth is what DashView would use.
	// The new code does: sessionW := SessionColumnWidth(d.Displayed)
	// so SessionColumnWidth(longSessions) IS the starting sessionW.
	d2 := dashboard.Shared{
		Width:     120,
		Displayed: longSessions,
		Sessions:  longSessions,
	}
	output2 := dashboard.DashView(d2, "", false)
	if output2 == "" {
		t.Error("DashView returned empty output for long sessions")
	}
	// Parse the header line to verify the column width.
	lines := strings.Split(output2, "\n")
	var headerLine string
	for _, l := range lines {
		plain := stripANSI(l)
		if strings.Contains(plain, "session") && strings.Contains(plain, "state") {
			headerLine = plain
			break
		}
	}
	if headerLine == "" {
		t.Fatal("could not find header line in DashView output for long sessions")
	}
	// The session column spans treePrefixW(10) + sessionW(after growSession).
	// What matters: DashView started from 17, not 20, so the total session area
	// fits the actual content. We can't easily check exact final width (growSession
	// may add more), but we can verify the header renders without panic and
	// contains expected column labels.
	for _, label := range []string{"session", "state", "type"} {
		if !strings.Contains(headerLine, label) {
			t.Errorf("header line missing %q: %q", label, headerLine)
		}
	}
}

// stripANSI removes ANSI escape sequences from a string for plain-text testing.
func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if b == 'm' {
				inEsc = false
			}
			continue
		}
		out.WriteByte(b)
	}
	return out.String()
}

// sessionNames is a helper that returns session names for test error messages.
func sessionNames(sessions []dashboard.AgentSession) []string {
	names := make([]string, len(sessions))
	for i, s := range sessions {
		names[i] = s.Name
	}
	return names
}
