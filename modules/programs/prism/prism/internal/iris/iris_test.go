package iris_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris"
)

// TestZeroSharedState is the security integration test mandated by the D-2 AC:
//
//	"An integration test sets $HOME to a temp dir, runs the iris startup path,
//	 and asserts no file or directory was created under any path containing the
//	 substring 'prism'."
//
// The test also asserts that every artefact iris creates lives under the
// iris-prefixed paths from the §10.1 table.
func TestZeroSharedState(t *testing.T) {
	// Redirect all XDG directories and HOME to an isolated temp dir so that
	// the iris startup path cannot reach the real ~/.local/state/prism/ or
	// ~/.config/prism/ paths.
	tmp := t.TempDir()

	fakeHome := filepath.Join(tmp, "home")
	fakeStateHome := filepath.Join(tmp, "state")
	fakeConfigHome := filepath.Join(tmp, "config")

	for _, d := range []string{fakeHome, fakeStateHome, fakeConfigHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("setup: MkdirAll(%q): %v", d, err)
		}
	}

	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", fakeStateHome)
	t.Setenv("XDG_CONFIG_HOME", fakeConfigHome)

	// --- Exercise the iris startup path ---

	// 1. Resolve paths (pure computation, no I/O).
	p := iris.ResolvePaths()

	// 2. Load config (absent config file → defaults, no error).
	cfg, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// 3. Open the iris DB — this is the only step that creates files.
	irisDB, err := iris.OpenDB(p.DB)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer irisDB.Close()

	// --- Assertions ---

	// Collect every file and directory created under tmp.
	var created []string
	if err := filepath.Walk(tmp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		created = append(created, path)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// Security assertion: no path must contain the substring "prism".
	for _, p := range created {
		if strings.Contains(p, "prism") {
			t.Errorf("iris startup created a path containing 'prism': %q", p)
		}
	}

	// Path assertion: the iris DB must have been created.
	if _, err := os.Stat(p.DB); os.IsNotExist(err) {
		t.Errorf("iris.db was not created at expected path %q", p.DB)
	}

	// Path assertion: the iris state dir must be under fakeStateHome/iris/,
	// not under any prism-prefixed path.
	wantStatePrefix := filepath.Join(fakeStateHome, "iris")
	if !strings.HasPrefix(p.DB, wantStatePrefix) {
		t.Errorf("iris DB path %q does not start with expected prefix %q", p.DB, wantStatePrefix)
	}

	// Config assertion: defaults are applied when no config file exists.
	if cfg.LogLevel == "" {
		t.Errorf("LoadConfig: LogLevel should not be empty (expected default)")
	}
}

// TestConfigLoad verifies that present config files are read and merged with
// defaults, and that absent config files return defaults without error.
func TestConfigLoad(t *testing.T) {
	t.Run("absent file returns defaults", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := iris.LoadConfig(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.LogLevel == "" {
			t.Error("LogLevel should be set to default")
		}
	})

	t.Run("present file is applied", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.json")
		data, _ := json.Marshal(map[string]any{
			"log_level":          "debug",
			"allowed_extensions": []string{"ext1", "ext2"},
		})
		if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg, err := iris.LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.LogLevel != "debug" {
			t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
		}
		if len(cfg.AllowedExtensions) != 2 {
			t.Errorf("AllowedExtensions len = %d, want 2", len(cfg.AllowedExtensions))
		}
	})
}

// TestResolvePaths verifies that ResolvePaths returns iris-prefixed paths and
// honours XDG environment variables.
func TestResolvePaths(t *testing.T) {
	tmp := t.TempDir()
	fakeState := filepath.Join(tmp, "state")
	fakeConfig := filepath.Join(tmp, "config")
	fakeHome := filepath.Join(tmp, "home")

	t.Setenv("XDG_STATE_HOME", fakeState)
	t.Setenv("XDG_CONFIG_HOME", fakeConfig)
	t.Setenv("HOME", fakeHome)

	p := iris.ResolvePaths()

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"DB", p.DB, filepath.Join(fakeState, "iris", "iris.db")},
		{"Sock", p.Sock, filepath.Join(fakeState, "iris", "iris.sock")},
		{"RunDir", p.RunDir, filepath.Join(fakeState, "iris", "run")},
		{"LogDir", p.LogDir, filepath.Join(fakeState, "iris", "logs")},
		{"ConfigFile", p.ConfigFile, filepath.Join(fakeConfig, "iris", "config.json")},
		{"ArchiveRoot", p.ArchiveRoot, filepath.Join(fakeHome, "code", "archives", "iris")},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Paths.%s = %q, want %q", c.name, c.got, c.want)
		}
		// No path should contain "prism".
		if strings.Contains(c.got, "prism") {
			t.Errorf("Paths.%s = %q contains 'prism'", c.name, c.got)
		}
	}
}

// TestDBOpenIdempotent verifies that opening an already-existing iris.db
// does not corrupt the file or fail.
func TestDBOpenIdempotent(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris", "iris.db")

	// First open — creates the file.
	db1, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("first OpenDB: %v", err)
	}
	db1.Close()

	// Second open — file already exists.
	db2, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("second OpenDB: %v", err)
	}
	db2.Close()

	// File must still exist and be non-empty.
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat after second open: %v", err)
	}
	if info.Size() == 0 {
		t.Error("iris.db is empty after second open")
	}
}
