package db_test

// session_starts_3375_test.go — a stop that fails after the sidecar stop
// keeps the start record open (#3375).

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
)

// AC (review of #3375): the sidecar stop ends the sessions row of the
// incarnation (EndIncarnation, as Sidecar.Shutdown does). When the stop of
// the sandbox processes then fails, the cleanup keeps the agent_status row,
// and tmux no longer lists the session. prism confirm-bare must still
// refuse the root that the session started in, because a process of the
// session can still write it. After the session ends, it confirms.
func TestConfirmBare_RefusesWhileAStoppedSessionKeepsItsRow_3375(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	code := filepath.Join(t.TempDir(), "code")
	cfg, err := json.Marshal(map[string]any{"project_locations": []string{code}})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	root := filepath.Join(code, "proj")
	if out, err := exec.Command("git", "init", "-q", "--bare", filepath.Join(root, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(filepath.Join(state, "prism", "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	if err := d.InsertSession(db.Session{InstanceID: "i1", SessionName: "proj", Repo: "proj", Worktree: root}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.UpsertStatus("proj", "proj", root, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID("proj", "i1"); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.EndIncarnation("i1", "finished"); err != nil {
		t.Fatalf("EndIncarnation: %v", err)
	}
	tmuxGone := func(string) (bool, error) { return false, nil }

	_, err = git.ConfirmBare(root, tmuxGone)
	if err == nil || !strings.Contains(err.Error(), "a session record with no end time names the session proj") {
		t.Fatalf("ConfirmBare with the agent_status row of a refused stop = %v, want a refusal that names the session", err)
	}

	if err := d.SetEnded("proj"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if _, err := git.ConfirmBare(root, tmuxGone); err != nil {
		t.Errorf("ConfirmBare after the session ended: %v, want the root confirmed", err)
	}
}
