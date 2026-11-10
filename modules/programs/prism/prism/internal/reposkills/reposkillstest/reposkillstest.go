// Package reposkillstest makes a fake nix store and a repo-skills.json for
// tests of the repo skills (internal/reposkills).
package reposkillstest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/reposkills"
)

// Store makes a temporary dir the nix store of reposkills until the test
// ends, and returns the dir with its symlinks resolved. On Darwin, t.TempDir
// is below a symlink, and reposkills compares resolved paths with the store.
func Store(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve store dir: %v", err)
	}
	t.Cleanup(reposkills.SetStoreDirForTest(dir))
	return dir
}

// Skill makes the skill dir <store>/0000-<name> with a SKILL.md, and returns
// its path.
func Skill(t *testing.T, store, name string) string {
	t.Helper()
	dir := filepath.Join(store, "0000-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "---\nname: " + name + "\ndescription: A test skill.\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

// WriteFile writes byRepo as repo-skills.json under configDir/prism, in the
// shape that the NixOS module renders.
func WriteFile(t *testing.T, configDir string, byRepo map[string][]string) {
	t.Helper()
	b, err := json.Marshal(byRepo)
	if err != nil {
		t.Fatalf("marshal repo skills: %v", err)
	}
	WriteRaw(t, configDir, b)
}

// WriteRaw writes body as repo-skills.json under configDir/prism.
func WriteRaw(t *testing.T, configDir string, body []byte) {
	t.Helper()
	dir := filepath.Join(configDir, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.RepoSkillsFileName), body, 0o600); err != nil {
		t.Fatalf("write %s: %v", config.RepoSkillsFileName, err)
	}
}
