package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The persistent dashboard runs across a switch. If the lookup reads the
// cached config, a changed limit does not show until a restart, and this
// test fails.
func TestSpawnBudgetLookup_ReadsTheLimitOnEveryLookup(t *testing.T) {
	dir := t.TempDir()
	SetTestDBPath(filepath.Join(dir, "prism.db"))
	t.Cleanup(func() { SetTestDBPath("") })
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	for _, limit := range []int{80, 90} {
		if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"spawn_budget_limit": %d}`, limit)), 0o644); err != nil {
			t.Fatal(err)
		}
		r := spawnBudgetLookup()
		if r.Limit != limit {
			t.Errorf("Limit = %d after config.json set %d", r.Limit, limit)
		}
		if !r.Known || r.Fleet || r.Count != 0 {
			t.Errorf("result = %+v, want a known local count of 0 with no fleet query", r)
		}
	}
}
