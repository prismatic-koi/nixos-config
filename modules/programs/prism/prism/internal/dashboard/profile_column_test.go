package dashboard_test

// profile_column_test.go — tests for the profile-tier column between state
// and title (issue #2640).

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

// TestDashView_ProfileColumn_ShowsTierAndHeader verifies the profile column
// header appears between state and title, and that a session's profile tier
// is rendered.
func TestDashView_ProfileColumn_ShowsTierAndHeader(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature", AgentState: "active", AgentName: "worker", ProfileName: "heavy"},
	}
	d := dashboard.Shared{Width: 120, Sessions: sessions}
	d = dashboard.RefilterShared(d)

	output := dashboard.DashView(d, "", false)
	plain := stripANSI(output)

	headerIdx := strings.Index(plain, "state")
	profileHeaderIdx := strings.Index(plain, "profile")
	titleHeaderIdx := strings.Index(plain, "title")
	if headerIdx < 0 || profileHeaderIdx < 0 || titleHeaderIdx < 0 {
		t.Fatalf("expected state, profile, and title headers in output, got:\n%s", plain)
	}
	if !(headerIdx < profileHeaderIdx && profileHeaderIdx < titleHeaderIdx) {
		t.Errorf("expected header order state < profile < title, got indices state=%d profile=%d title=%d\n%s",
			headerIdx, profileHeaderIdx, titleHeaderIdx, plain)
	}

	if !strings.Contains(plain, "heavy") {
		t.Errorf("expected session's profile tier %q in output, got:\n%s", "heavy", plain)
	}
}

// TestDashView_ProfileColumn_NullProfileRendersPlaceholder verifies that a
// session with an empty ProfileName (NULL spawn_inputs.profile_name) renders
// an explicit short placeholder rather than an empty cell.
func TestDashView_ProfileColumn_NullProfileRendersPlaceholder(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature", AgentState: "active", AgentName: "worker", ProfileName: ""},
	}
	d := dashboard.Shared{Width: 120, Sessions: sessions}
	d = dashboard.RefilterShared(d)

	output := dashboard.DashView(d, "", false)
	plain := stripANSI(output)

	if !strings.Contains(plain, "-") {
		t.Errorf("expected an explicit placeholder for a NULL profile, got:\n%s", plain)
	}

	// Locate the data row (contains the session name) and confirm it is not
	// blank between state and title — i.e. some non-space content sits in
	// the profile slot.
	lines := strings.Split(plain, "\n")
	var dataLine string
	for _, l := range lines {
		if strings.Contains(l, "nixos-config@feature") {
			dataLine = l
			break
		}
	}
	if dataLine == "" {
		t.Fatalf("could not find data row in output:\n%s", plain)
	}
	idx := strings.Index(dataLine, "active")
	if idx < 0 {
		t.Fatalf("could not find state cell in data row: %q", dataLine)
	}
	afterState := dataLine[idx+len("active"):]
	trimmed := strings.TrimSpace(afterState)
	if trimmed == "" {
		t.Errorf("expected non-blank content after the state cell (profile placeholder), got row: %q", dataLine)
	}
}

// TestDashView_NarrowTerminal_DropsProfileAndTitle_KeepsStateIntact verifies
// that at a width too narrow for the profile+title columns, the dashboard
// falls back to session + state only, and the state label is never
// truncated.
func TestDashView_NarrowTerminal_DropsProfileAndTitle_KeepsStateIntact(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature", AgentState: "active", AgentName: "worker", ProfileName: "standard"},
	}
	// Width chosen to be comfortably above the skeleton-view cutoff but below
	// the room needed for the profile column (profileW=8 + its 2-space gap)
	// on top of the fixed session+state core.
	d := dashboard.Shared{Width: 30, Sessions: sessions}
	d = dashboard.RefilterShared(d)

	output := dashboard.DashView(d, "", false)
	plain := stripANSI(output)

	if strings.Contains(plain, "profile") {
		t.Errorf("expected the profile header to be dropped at narrow width, got:\n%s", plain)
	}
	if strings.Contains(plain, "standard") {
		t.Errorf("expected the profile value to be dropped at narrow width, got:\n%s", plain)
	}
	if !strings.Contains(plain, "active") {
		t.Errorf("expected the state label 'active' to render intact at narrow width, got:\n%s", plain)
	}
}
