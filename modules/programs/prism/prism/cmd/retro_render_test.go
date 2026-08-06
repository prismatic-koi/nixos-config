package cmd

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestFormatTokensGrouped covers the AC: token counts render as plain integers
// with thousands separators, never in scientific notation.
func TestFormatTokensGrouped(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{100000, "100,000"},
		{1000000, "1,000,000"},
		{11297191, "11,297,191"},
		{5521483, "5,521,483"},
		{-2500, "-2,500"},
	}
	for _, c := range cases {
		got := formatTokensGrouped(c.in)
		if got != c.want {
			t.Errorf("formatTokensGrouped(%d) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "eE") {
			t.Errorf("formatTokensGrouped(%d) = %q contains scientific notation", c.in, got)
		}
	}
}

// TestFormatRetroCost covers the subscription-profile case: a zero cost renders
// as an explicit $0.00, not "<$0.01".
func TestFormatRetroCost(t *testing.T) {
	if got := formatRetroCost(0); got != "$0.00" {
		t.Errorf("formatRetroCost(0) = %q, want $0.00", got)
	}
	if got := formatRetroCost(12.5); got != "$12.50" {
		t.Errorf("formatRetroCost(12.5) = %q, want $12.50", got)
	}
}

// TestRenderRetro_EmptyWindow verifies the empty-window path prints the
// explicit message and does not panic.
func TestRenderRetro_EmptyWindow(t *testing.T) {
	r := &db.RetroReport{
		Repo:   "retrorepo",
		Since:  "2026-08-05T00:00:00Z",
		Until:  "2026-08-06T00:00:00Z",
		Trains: []db.RetroTrain{},
	}
	out := captureStdout(t, func() { renderRetro(r) })
	if !strings.Contains(out, "no sessions in this window") {
		t.Errorf("empty-window render missing message; got:\n%s", out)
	}
}

// TestRenderRetro_WasteUnavailable verifies the waste section renders
// "unavailable" distinctly from a recorded zero.
func TestRenderRetro_WasteUnavailable(t *testing.T) {
	r := &db.RetroReport{
		Repo:   "retrorepo",
		Since:  "2026-08-05T00:00:00Z",
		Until:  "2026-08-06T00:00:00Z",
		Trains: []db.RetroTrain{{Root: "retrorepo@a", Kind: "worker", TotalTokens: 100}},
		WindowTotals: db.RetroWindowTotals{
			TotalTokens:  100,
			SessionCount: 1,
		},
		WasteSignals: db.RetroWasteSignals{Available: false},
	}
	out := captureStdout(t, func() { renderRetro(r) })
	if !strings.Contains(out, "unavailable") {
		t.Errorf("waste section should render 'unavailable'; got:\n%s", out)
	}
}
