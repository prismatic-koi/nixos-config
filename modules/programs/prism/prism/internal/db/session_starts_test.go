package db_test

// session_starts_test.go — the bare repo record of internal/git starts from
// the session start records (#3364).

import (
	"database/sql"
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
// each pair of session name and start dir. Open is true when the
// agent_status row of the name has no end time and names the instance of a
// row of the pair, or names no instance while a row of the pair has no end
// time. With no DB file, it gives no record and makes no file.
//
// The cases:
//   - repo@main: one ended row and one open row that agent_status names.
//   - plain: an ended row.
//   - orphan: an open row that agent_status no longer names (#3027).
//   - pruned: an open row with no agent_status row.
//   - agended: an open row whose agent_status row has an end time.
//   - starting: an open row, and an agent_status row with no instance.
//   - shutdown: agent_status names the row, and a sidecar stop ended the
//     row (EndIncarnation): the stop of a session_shutdown frame, or a
//     session stop that failed after the sidecar stop (#3375). The
//     agent_status row has no end time, so Open is true.
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
		{InstanceID: "i4", SessionName: "orphan", Repo: "orphan", Worktree: "/x/orphan"},
		{InstanceID: "i5", SessionName: "pruned", Repo: "pruned", Worktree: "/x/pruned"},
		{InstanceID: "i6", SessionName: "agended", Repo: "agended", Worktree: "/x/agended"},
		{InstanceID: "i7", SessionName: "starting", Repo: "starting", Worktree: "/x/starting"},
		{InstanceID: "i8", SessionName: "shutdown", Repo: "shutdown", Worktree: "/x/shutdown"},
	} {
		if err := d.InsertSession(s); err != nil {
			t.Fatalf("InsertSession: %v", err)
		}
	}
	for name, instance := range map[string]string{
		"repo@main": "i2",
		"orphan":    "i4-next",
		"agended":   "i6",
		"starting":  "",
		"shutdown":  "i8",
	} {
		if err := d.UpsertStatus(name, name, "/x/"+name, "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus: %v", err)
		}
		if instance != "" {
			if err := d.SetInstanceID(name, instance); err != nil {
				t.Fatalf("SetInstanceID: %v", err)
			}
		}
	}
	for _, id := range []string{"i1", "i3"} {
		if err := d.UpdateSessionEnded(id, "done"); err != nil {
			t.Fatalf("UpdateSessionEnded: %v", err)
		}
	}
	if err := d.EndIncarnation("i8", "finished"); err != nil {
		t.Fatalf("EndIncarnation: %v", err)
	}
	d.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE agent_status SET ended_at = 1 WHERE session_name = 'agended'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

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
		{Name: "orphan", Worktree: "/x/orphan", Open: false},
		{Name: "pruned", Worktree: "/x/pruned", Open: false},
		{Name: "agended", Worktree: "/x/agended", Open: false},
		{Name: "starting", Worktree: "/x/starting", Open: true},
		{Name: "shutdown", Worktree: "/x/shutdown", Open: true},
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
