package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config/configtest"
)

// AC (#3361): before the conversion, the root session of a plain clone can
// plant a symlink to .bare at the worktree path and an untracked config in
// the root. ConvertToBare refuses that worktree path before it changes the
// clone, so the planted config does not become .bare/config.
func TestConvertToBare_RefusesPlantAtWorktreePath_3361(t *testing.T) {
	isolateHostGitConfig(t)
	for _, tc := range []struct {
		name, branch, plant string
	}{
		{"symlink at the branch dir", "main", "main"},
		{"symlink at the first dir of a slash branch", "feat/plant", "feat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configtest.AllowProjectRoots(t, dir)
			initRepoWithRemote(t, dir, t.TempDir(), "main")
			if tc.branch != "main" {
				runGitIn(t, dir, "checkout", "-q", "-b", tc.branch)
				runGitIn(t, dir, "push", "-q", "origin", tc.branch)
			}
			if err := os.Symlink(".bare", filepath.Join(dir, tc.plant)); err != nil {
				t.Fatal(err)
			}
			const payload = "[core]\n\tfsmonitor = \"echo planted-3361\"\n"
			if err := os.WriteFile(filepath.Join(dir, "config"), []byte(payload), 0o644); err != nil {
				t.Fatal(err)
			}

			_, convErr := ConvertToBare(dir, func(string) {})
			if cfg, err := os.ReadFile(filepath.Join(dir, ".bare", "config")); err == nil && strings.Contains(string(cfg), "planted-3361") {
				t.Fatal("the planted config became .bare/config")
			}
			if convErr == nil || !strings.Contains(convErr.Error(), "exists") {
				t.Fatalf("ConvertToBare = %v, want a refusal of the planted worktree path", convErr)
			}
			if _, err := os.Lstat(filepath.Join(dir, ".bare")); !os.IsNotExist(err) {
				t.Errorf(".bare: %v, want none: the refusal comes before the clone", err)
			}
			if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
				t.Errorf(".git: %v, want the .git dir of the clone, unchanged", err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "config")); err != nil || string(got) != payload {
				t.Errorf("the untracked config moved or changed: %q, %v", got, err)
			}
		})
	}
}
