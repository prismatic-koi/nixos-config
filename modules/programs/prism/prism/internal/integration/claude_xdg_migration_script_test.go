package integration_test

// claude_xdg_migration_script_test.go — unit coverage for the one-time
// claude-code state migration script:
// modules/programs/prism/claude-xdg-migrate.sh, invoked by the home-manager
// activation script in claude-code.nix.
//
// The script lives OUTSIDE the Go module (it is a nix-module artefact, not
// part of the prism binary), so these tests resolve it via a relative path
// from this package directory. Two execution contexts:
//
//   - `go test ./...` from modules/programs/prism/prism/ (developer runs and
//     the go-tests CI job): the full repo checkout is present, the script is
//     found, and the tests run for real.
//   - The nix-sandboxed build (runChecks = true, the homeless-shelter CI
//     job): pkgs/prism.nix sets src = modules/programs/prism/prism only, so
//     the script is absent and the tests skip with an explicit message —
//     same explicit-skip discipline as the PTY and sandbox-exec gates.
//
// Everything runs against t.TempDir() paths — no real $HOME state is ever
// read or written (homeless-shelter-safe by construction).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// claudeMigrateEntries mirrors the entries=() list in the script.
var claudeMigrateEntries = []string{"history.jsonl", "projects", "plugins", "telemetry", "backups"}

// requireClaudeMigrateScript resolves the migration script relative to this
// package dir and skips with an explicit message when it is not present
// (the nix build sandbox strips the repo tree outside the Go module).
func requireClaudeMigrateScript(t *testing.T) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "claude-xdg-migrate.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, statErr := os.Stat(script); statErr != nil {
		t.Skipf("claude-xdg-migrate.sh not found at %s — running outside the full repo checkout (e.g. the nix build sandbox, whose src is the Go module only); the go-tests CI job covers this test", script)
	}
	if _, lookErr := exec.LookPath("bash"); lookErr != nil {
		t.Skipf("bash not found in PATH: %v", lookErr)
	}
	return script
}

// runClaudeMigrate invokes the script with the given src dir, src json, and
// dst dir, returning combined output and error.
func runClaudeMigrate(t *testing.T, script, srcDir, srcJSON, dstDir string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", script, srcDir, srcJSON, dstDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// migrationFixture creates a fake old-layout claude state: <root>/.claude
// with every migratable entry plus <root>/.claude.json. Returns
// (srcDir, srcJSON, dstDir). dstDir is not created.
func migrationFixture(t *testing.T) (srcDir, srcJSON, dstDir string) {
	t.Helper()
	root := t.TempDir()
	srcDir = filepath.Join(root, ".claude")
	srcJSON = filepath.Join(root, ".claude.json")
	dstDir = filepath.Join(root, ".config", "claude")

	for _, entry := range claudeMigrateEntries {
		p := filepath.Join(srcDir, entry)
		if strings.Contains(entry, ".") {
			// File entry (history.jsonl).
			if err := os.MkdirAll(srcDir, 0o700); err != nil {
				t.Fatalf("MkdirAll %s: %v", srcDir, err)
			}
			if err := os.WriteFile(p, []byte("src-"+entry), 0o600); err != nil {
				t.Fatalf("WriteFile %s: %v", p, err)
			}
		} else {
			// Directory entry with a sentinel file inside.
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatalf("MkdirAll %s: %v", p, err)
			}
			if err := os.WriteFile(filepath.Join(p, "sentinel"), []byte("src-"+entry), 0o600); err != nil {
				t.Fatalf("WriteFile sentinel in %s: %v", p, err)
			}
		}
	}
	if err := os.WriteFile(srcJSON, []byte(`{"src":"claude.json"}`), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", srcJSON, err)
	}
	return srcDir, srcJSON, dstDir
}

// TestClaudeXdgMigrate_MovesAllState verifies the fresh-migration path:
// every entry and ~/.claude.json move to the destination, and the sources
// are gone afterwards.
func TestClaudeXdgMigrate_MovesAllState(t *testing.T) {
	script := requireClaudeMigrateScript(t)
	srcDir, srcJSON, dstDir := migrationFixture(t)

	out, err := runClaudeMigrate(t, script, srcDir, srcJSON, dstDir)
	if err != nil {
		t.Fatalf("migration failed: %v\noutput: %s", err, out)
	}

	for _, entry := range claudeMigrateEntries {
		if _, statErr := os.Lstat(filepath.Join(dstDir, entry)); statErr != nil {
			t.Errorf("destination missing %s after migration: %v", entry, statErr)
		}
		if _, statErr := os.Lstat(filepath.Join(srcDir, entry)); statErr == nil {
			t.Errorf("source still has %s after migration — should have moved", entry)
		}
	}
	// Directory contents travelled with the move.
	got, readErr := os.ReadFile(filepath.Join(dstDir, "projects", "sentinel"))
	if readErr != nil || string(got) != "src-projects" {
		t.Errorf("projects/sentinel content after migration = %q, %v; want %q", got, readErr, "src-projects")
	}
	// .claude.json relocated into the dst dir.
	if _, statErr := os.Lstat(filepath.Join(dstDir, ".claude.json")); statErr != nil {
		t.Errorf("destination missing .claude.json: %v", statErr)
	}
	if _, statErr := os.Lstat(srcJSON); statErr == nil {
		t.Errorf("source .claude.json still present after migration — should have moved")
	}
}

// TestClaudeXdgMigrate_SecondRunIsNoOp verifies idempotency: running the
// script twice leaves the destination identical and exits 0.
func TestClaudeXdgMigrate_SecondRunIsNoOp(t *testing.T) {
	script := requireClaudeMigrateScript(t)
	srcDir, srcJSON, dstDir := migrationFixture(t)

	if out, err := runClaudeMigrate(t, script, srcDir, srcJSON, dstDir); err != nil {
		t.Fatalf("first run failed: %v\noutput: %s", err, out)
	}
	out, err := runClaudeMigrate(t, script, srcDir, srcJSON, dstDir)
	if err != nil {
		t.Fatalf("second run failed (must be a no-op): %v\noutput: %s", err, out)
	}
	if strings.Contains(out, "moved") {
		t.Errorf("second run moved something — not a no-op:\n%s", out)
	}
	got, readErr := os.ReadFile(filepath.Join(dstDir, "history.jsonl"))
	if readErr != nil || string(got) != "src-history.jsonl" {
		t.Errorf("history.jsonl after second run = %q, %v; want unchanged %q", got, readErr, "src-history.jsonl")
	}
}

// TestClaudeXdgMigrate_AbsentSourceIsNoOp verifies that a host with no old
// claude state exits 0 without creating the destination dir.
func TestClaudeXdgMigrate_AbsentSourceIsNoOp(t *testing.T) {
	script := requireClaudeMigrateScript(t)
	root := t.TempDir()
	srcDir := filepath.Join(root, ".claude")           // does not exist
	srcJSON := filepath.Join(root, ".claude.json")     // does not exist
	dstDir := filepath.Join(root, ".config", "claude") // must not be created

	out, err := runClaudeMigrate(t, script, srcDir, srcJSON, dstDir)
	if err != nil {
		t.Fatalf("absent-source run failed (must be a no-op): %v\noutput: %s", err, out)
	}
	if _, statErr := os.Lstat(dstDir); statErr == nil {
		t.Errorf("destination dir was created on an absent-source run — must stay a strict no-op")
	}
}

// TestClaudeXdgMigrate_NeverClobbersDestination verifies the skip-and-warn
// behaviour: a pre-existing destination entry is never overwritten, the
// warning names the skipped entry, the source entry stays in place, and the
// other entries still migrate.
func TestClaudeXdgMigrate_NeverClobbersDestination(t *testing.T) {
	script := requireClaudeMigrateScript(t)
	srcDir, srcJSON, dstDir := migrationFixture(t)

	// Pre-existing destination entries with distinct content: one file, one dir.
	if err := os.MkdirAll(filepath.Join(dstDir, "projects"), 0o700); err != nil {
		t.Fatalf("MkdirAll dst projects: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "projects", "sentinel"), []byte("dst-projects"), 0o600); err != nil {
		t.Fatalf("WriteFile dst projects sentinel: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "history.jsonl"), []byte("dst-history"), 0o600); err != nil {
		t.Fatalf("WriteFile dst history.jsonl: %v", err)
	}

	out, err := runClaudeMigrate(t, script, srcDir, srcJSON, dstDir)
	if err != nil {
		t.Fatalf("migration failed: %v\noutput: %s", err, out)
	}

	// The pre-existing destination entries are untouched.
	for name, want := range map[string]string{
		filepath.Join(dstDir, "history.jsonl"):        "dst-history",
		filepath.Join(dstDir, "projects", "sentinel"): "dst-projects",
	} {
		got, readErr := os.ReadFile(name)
		if readErr != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want untouched %q", name, got, readErr, want)
		}
	}
	// The colliding source entries stay in place (not silently dropped).
	for _, entry := range []string{"history.jsonl", "projects"} {
		if _, statErr := os.Lstat(filepath.Join(srcDir, entry)); statErr != nil {
			t.Errorf("source %s removed despite destination collision: %v", entry, statErr)
		}
		if !strings.Contains(out, "SKIP "+filepath.Join(srcDir, entry)) {
			t.Errorf("output lacks skip warning for %s:\n%s", entry, out)
		}
	}
	// Non-colliding entries still migrated.
	for _, entry := range []string{"plugins", "telemetry", "backups"} {
		if _, statErr := os.Lstat(filepath.Join(dstDir, entry)); statErr != nil {
			t.Errorf("non-colliding entry %s did not migrate: %v", entry, statErr)
		}
	}
	if _, statErr := os.Lstat(filepath.Join(dstDir, ".claude.json")); statErr != nil {
		t.Errorf(".claude.json did not migrate: %v", statErr)
	}
	if _, statErr := os.Lstat(srcJSON); statErr == nil {
		t.Errorf("source .claude.json still present — should have moved")
	}
}

// TestClaudeXdgMigrate_JsonOnlyMigrates covers the partial-state case: only
// ~/.claude.json exists (no ~/.claude dir). The json must still relocate —
// CLAUDE_CONFIG_DIR moves .claude.json into the config dir too (claude-code
// 2.1.161 behaviour).
func TestClaudeXdgMigrate_JsonOnlyMigrates(t *testing.T) {
	script := requireClaudeMigrateScript(t)
	root := t.TempDir()
	srcDir := filepath.Join(root, ".claude") // does not exist
	srcJSON := filepath.Join(root, ".claude.json")
	dstDir := filepath.Join(root, ".config", "claude")
	if err := os.WriteFile(srcJSON, []byte(`{"only":"json"}`), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", srcJSON, err)
	}

	out, err := runClaudeMigrate(t, script, srcDir, srcJSON, dstDir)
	if err != nil {
		t.Fatalf("json-only migration failed: %v\noutput: %s", err, out)
	}
	got, readErr := os.ReadFile(filepath.Join(dstDir, ".claude.json"))
	if readErr != nil || string(got) != `{"only":"json"}` {
		t.Errorf("migrated .claude.json = %q, %v; want %q", got, readErr, `{"only":"json"}`)
	}
	if _, statErr := os.Lstat(srcJSON); statErr == nil {
		t.Errorf("source .claude.json still present — should have moved")
	}
}
