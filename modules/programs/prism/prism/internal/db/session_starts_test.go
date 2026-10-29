package db_test

// session_starts_test.go — the bare repo record of internal/git starts from
// the session start records (#3364).

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
)

// TestSessionStarts_ReadsTheSessionsTable: SessionStarts gives one record for
// each pair of session name and start dir. Open is true when a row of the
// pair has no end time. With no DB file, it gives no record and makes no
// file.
func TestSessionStarts_ReadsTheSessionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, s := range []db.Session{
		{InstanceID: "i1", SessionName: "repo@main", Repo: "repo", Worktree: "/x/repo/main"},
		{InstanceID: "i2", SessionName: "repo@main", Repo: "repo", Worktree: "/x/repo/main"},
		{InstanceID: "i3", SessionName: "plain", Repo: "plain", Worktree: "/x/plain"},
	} {
		if err := d.InsertSession(s); err != nil {
			t.Fatalf("InsertSession: %v", err)
		}
	}
	for _, id := range []string{"i1", "i3"} {
		if err := d.UpdateSessionEnded(id, "done"); err != nil {
			t.Fatalf("UpdateSessionEnded: %v", err)
		}
	}
	d.Close()

	starts, err := db.SessionStarts(path)
	if err != nil {
		t.Fatalf("SessionStarts: %v", err)
	}
	got := map[git.SessionStart]bool{}
	for _, s := range starts {
		got[s] = true
	}
	want := []git.SessionStart{
		{Name: "repo@main", Worktree: "/x/repo/main", Open: true},
		{Name: "plain", Worktree: "/x/plain", Open: false},
	}
	if len(starts) != len(want) {
		t.Errorf("SessionStarts = %v, want %v", starts, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("SessionStarts = %v, want %v in it", starts, w)
		}
	}

	missing := filepath.Join(t.TempDir(), "prism.db")
	if starts, err := db.SessionStarts(missing); err != nil || starts != nil {
		t.Errorf("SessionStarts(no file) = %v, %v; want no record and no error", starts, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("SessionStarts made %s: %v", missing, err)
	}
}

// TestSessionStarts_StartTheBareRecord: this package sets the reader of
// internal/git. On the host, OpenBare starts the record from the sessions
// table of prism.db in the state dir. A .bare whose root has a worktree
// session row goes in the record. A .bare whose root has no row does not.
func TestSessionStarts_StartTheBareRecord(t *testing.T) {
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

	used := filepath.Join(code, "used")
	idle := filepath.Join(code, "idle")
	for _, root := range []string{used, idle} {
		if out, err := exec.Command("git", "init", "-q", "--bare", filepath.Join(root, ".bare")).CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", root, err, out)
		}
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d, err := db.Open(filepath.Join(state, "prism", "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := d.InsertSession(db.Session{InstanceID: "i1", SessionName: "used@main", Repo: "used", Worktree: filepath.Join(used, "main")}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	d.Close()

	if _, err := git.OpenBare(used); err != nil {
		t.Errorf("OpenBare(%s), a root with a worktree session row: %v", used, err)
	}
	if _, err := git.OpenBare(idle); !errors.Is(err, git.ErrNotPrismBare) {
		t.Errorf("OpenBare(%s), a root with no row = %v, want ErrNotPrismBare", idle, err)
	}
}
