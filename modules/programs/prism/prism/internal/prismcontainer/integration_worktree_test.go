package prismcontainer_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// integrationWorktree returns the worktree of an integration test. On
// Linux, podman mounts any path, so it is a temp dir. On macOS, podman
// runs in a machine that mounts only some Mac paths, and the worktree must
// be inside one of them. The helper reads the mounts the way the build
// check of prism does. It uses the temp dir when a mount holds it, else a
// new dir inside a writable mount, and else it skips the test.
func integrationWorktree(t *testing.T) string {
	t.Helper()
	tmp := prismcontainertest.RealTempDir(t)
	if runtime.GOOS != "darwin" {
		return tmp
	}
	name, mounts, err := prismcontainer.MachineMounts(context.Background(), prismcontainer.ExecRunner{}, os.ReadFile)
	if err != nil {
		// The mounts are unknown. Use the temp dir: a real fault then
		// shows as a failure, not as a skip.
		t.Logf("cannot read the podman machine mounts: %v", err)
		return tmp
	}
	var roots []string
	for _, m := range mounts {
		// Podman passes the Mac path to the machine unchanged, so only
		// a mount at the same path can hold a worktree.
		if m.ReadOnly || m.Source != m.Target {
			continue
		}
		root, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			continue
		}
		roots = append(roots, root)
		if inside(tmp, root) {
			return tmp
		}
	}
	for _, root := range roots {
		dir, err := os.MkdirTemp(root, ".prism-it-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	var sources []string
	for _, m := range mounts {
		sources = append(sources, m.Source)
	}
	t.Skipf("podman machine %q mounts %q, and none of them holds a writable worktree: the worktree-mount tests need a machine mount at the same path on the Mac", name, strings.Join(sources, ", "))
	return ""
}

func inside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
