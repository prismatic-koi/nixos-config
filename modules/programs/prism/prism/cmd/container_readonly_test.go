package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// TestContainerRunHostMode_ReadOnlyWorktreeRefusesMountRW: the host-mode
// route reads agent_status.worktree_readonly, so a read-only session cannot
// mount its worktree read-write (#3079).
func TestContainerRunHostMode_ReadOnlyWorktreeRefusesMountRW(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))
	session := "prism-test@container-host-readonly"
	worktree := prismcontainertest.RealTempDir(t)
	t.Setenv("PRISM_SESSION_NAME", session)
	seedContainerRunSession(t, session, worktree)
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.SetWorktreeReadOnly(session, true); err != nil {
		t.Fatalf("SetWorktreeReadOnly: %v", err)
	}
	d.Close()

	f := &prismcontainertest.Fake{ImagePresent: true}
	installPrismContainerFake(t, f)
	res := runContainerHostMode(context.Background(), prismcontainer.RunRequest{Image: "alpine", Mount: prismcontainer.MountRW})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "read-only") || !strings.Contains(res.Message, worktree) {
		t.Errorf("result = %+v, want a refusal that names the read-only worktree %s", res, worktree)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("podman called: %q", f.Calls())
	}
}
