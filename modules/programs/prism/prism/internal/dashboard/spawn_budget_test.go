package dashboard_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

func sharedWithBudget(r spawnbudget.Result) dashboard.Shared {
	return dashboard.Shared{SpawnBudget: r, SpawnBudgetLoaded: true, Width: 120}
}

func TestSpawnBudgetStat_ShowsCountLimitAndLevelMark(t *testing.T) {
	cases := []struct {
		name    string
		d       dashboard.Shared
		want    []string
		mustNot []string
	}{
		{
			name:    "loading",
			d:       dashboard.Shared{},
			want:    []string{"7d spawns …"},
			mustNot: []string{dashboard.SpawnBudgetNearMark, dashboard.SpawnBudgetOverMark},
		},
		{
			name:    "under 95%",
			d:       sharedWithBudget(spawnbudget.Result{Count: 113, Limit: 120, Known: true, Fleet: true}),
			want:    []string{"7d spawns 113/120"},
			mustNot: []string{dashboard.SpawnBudgetNearMark, dashboard.SpawnBudgetOverMark, "this host"},
		},
		{
			name:    "at 95%",
			d:       sharedWithBudget(spawnbudget.Result{Count: 114, Limit: 120, Known: true, Fleet: true}),
			want:    []string{"7d spawns 114/120", dashboard.SpawnBudgetNearMark},
			mustNot: []string{dashboard.SpawnBudgetOverMark},
		},
		{
			name:    "at 100%",
			d:       sharedWithBudget(spawnbudget.Result{Count: 120, Limit: 120, Known: true, Fleet: true}),
			want:    []string{"7d spawns 120/120", dashboard.SpawnBudgetOverMark},
			mustNot: []string{dashboard.SpawnBudgetNearMark},
		},
		{
			name: "this host only",
			d:    sharedWithBudget(spawnbudget.Result{Count: 40, Limit: 120, Known: true, LocalOnlyReason: "cannot reach Prometheus"}),
			want: []string{"7d spawns 40/120 (this host)"},
		},
		{
			name: "the count is not available",
			d:    sharedWithBudget(spawnbudget.Result{Limit: 120}),
			want: []string{"7d spawns ?/120"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dashboard.SpawnBudgetStat(tc.d)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("stat %q does not contain %q", got, w)
				}
			}
			for _, w := range tc.mustNot {
				if strings.Contains(got, w) {
					t.Errorf("stat %q contains %q", got, w)
				}
			}
		})
	}
}

func TestRenderHeader_ShowsSpawnBudgetInFullAndCompactModes(t *testing.T) {
	over := sharedWithBudget(spawnbudget.Result{Count: 196, Limit: 120, Known: true, Fleet: true})
	dim := lipgloss.NewStyle()

	full := over
	full.Height = 0 // no height limit: full header
	if got := dashboard.RenderHeader(full, dim); !strings.Contains(got, "7d spawns 196/120 "+dashboard.SpawnBudgetOverMark) {
		t.Errorf("full header does not show the budget:\n%s", got)
	}

	compact := over
	compact.Height = 3 // too short for the art block: compact header
	got := dashboard.RenderHeader(compact, dim)
	if !strings.Contains(got, "7d spawns 196/120 "+dashboard.SpawnBudgetOverMark) {
		t.Errorf("compact header does not show the budget:\n%s", got)
	}
	if strings.Count(got, "\n") != 2 {
		t.Errorf("compact header has %d lines, want 2:\n%s", strings.Count(got, "\n"), got)
	}
}

func TestApplySpawnBudgetMsg_FailedLookupKeepsLastCount(t *testing.T) {
	var d dashboard.Shared
	d = d.ApplySpawnBudgetMsg(dashboard.SpawnBudgetMsg{Result: spawnbudget.Result{Count: 50, Limit: 120, Known: true, Fleet: true}})
	if !d.SpawnBudgetLoaded || d.SpawnBudget.Count != 50 {
		t.Fatalf("after a known result: loaded=%v count=%d, want true/50", d.SpawnBudgetLoaded, d.SpawnBudget.Count)
	}
	d = d.ApplySpawnBudgetMsg(dashboard.SpawnBudgetMsg{Result: spawnbudget.Result{Limit: 120, LocalErr: errors.New("locked")}})
	if !d.SpawnBudget.Known || d.SpawnBudget.Count != 50 {
		t.Errorf("a failed lookup replaced the last count: %+v", d.SpawnBudget)
	}
	d = d.ApplySpawnBudgetMsg(dashboard.SpawnBudgetMsg{Result: spawnbudget.Result{Count: 51, Limit: 120, Known: true}})
	if d.SpawnBudget.Count != 51 || d.SpawnBudget.Fleet {
		t.Errorf("a new known result did not replace the count: %+v", d.SpawnBudget)
	}
}

func TestDashboardModels_StoreSpawnBudgetMsg(t *testing.T) {
	msg := dashboard.SpawnBudgetMsg{Result: spawnbudget.Result{Count: 115, Limit: 120, Known: true, Fleet: true}}

	popup, _ := dashboard.NewPopupModel("", "").Update(msg)
	if p := popup.(dashboard.PopupModel); !p.SpawnBudgetLoaded || p.SpawnBudget.Count != 115 {
		t.Errorf("popup did not store the budget: %+v", p.SpawnBudget)
	}

	persistent, _ := dashboard.NewPersistentModel("", "").Update(msg)
	if p := persistent.(dashboard.PersistentModel); !p.SpawnBudgetLoaded || p.SpawnBudget.Count != 115 {
		t.Errorf("persistent dashboard did not store the budget: %+v", p.SpawnBudget)
	}
}
