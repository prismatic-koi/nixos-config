// Package configtest changes the prism config.json for a test.
package configtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// AllowProjectRoots makes each of roots a prism project root for the rest of
// t: a project_specific entry of config.json. git.OpenBare refuses a root
// that is not a prism project root.
//
// It copies the config file that is in effect (config.FilePath), adds the
// roots, writes the copy to t.TempDir(), and points PRISM_CONFIG_FILE at the
// copy with t.Setenv. So t must not be parallel. A relative root is made
// absolute. A second call adds to the roots of the first.
func AllowProjectRoots(t testing.TB, roots ...string) {
	t.Helper()
	cfg := map[string]any{}
	if path := config.FilePath(); path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("configtest: read %s: %v", path, err)
		}
		if err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("configtest: parse %s: %v", path, err)
			}
		}
	}
	specific, _ := cfg["project_specific"].([]any)
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			t.Fatalf("configtest: %v", err)
		}
		specific = append(specific, abs)
	}
	cfg["project_specific"] = specific

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("configtest: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("configtest: write %s: %v", path, err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}
