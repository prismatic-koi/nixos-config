package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

func TestShellCompletion_Wiring(t *testing.T) {
	for name, c := range map[string]*cobra.Command{
		"account use": accountUseCmd, "account rm": accountRmCmd, "account login": accountLoginCmd,
		"profile use": profileUseCmd, "profile show": profileShowCmd,
		"checkin": checkinCmd, "logs": logsCmd, "prompt": promptCmd, "stats": statsCmd, "archive": archiveCmd,
	} {
		if c.ValidArgsFunction == nil {
			t.Errorf("%s: no ValidArgsFunction", name)
		}
	}
	if _, ok := cleanupCmd.GetFlagCompletionFunc("session"); !ok {
		t.Error("cleanup --session: no flag completion function")
	}
}

func TestCompleteAccountNames_ListsNamesOnly(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := filepath.Join(cfg, "prism", "accounts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-ant-oat-SECRET-TOKEN"
	for _, n := range []string{"work", "personal"} {
		// Mode 0o000: the completion must list the name without reading the file.
		if err := os.WriteFile(filepath.Join(dir, n+".json"), []byte(secret), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, dir2 := completeAccountNames(accountUseCmd, nil, "")
	if want := []cobra.Completion{"personal", "work"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if dir2 != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v", dir2)
	}
	for _, c := range got {
		if strings.Contains(c, secret) {
			t.Fatalf("candidate %q contains account file content", c)
		}
	}

	got, _ = completeAccountNames(accountUseCmd, nil, "w")
	if want := []cobra.Completion{"work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prefix w: got %v, want %v", got, want)
	}
	if got, _ := completeAccountNames(accountUseCmd, []string{"work"}, ""); len(got) != 0 {
		t.Errorf("second argument: got %v, want none", got)
	}
}

func TestCompleteAccountNames_MissingDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	got, d := completeAccountNames(accountUseCmd, nil, "")
	if len(got) != 0 || d != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("got %v %v, want no candidates", got, d)
	}
	if _, err := os.Stat(filepath.Join(cfg, "prism")); !os.IsNotExist(err) {
		t.Errorf("completion created %s/prism: %v", cfg, err)
	}
}

func TestCompleteProfileNames(t *testing.T) {
	withProfileFixture(t, `{"profiles": {"heavy": {}, "light": {}}}`)
	got, _ := completeProfileNames(profileUseCmd, nil, "")
	if want := []cobra.Completion{"heavy", "light"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got, _ = completeProfileNames(profileUseCmd, nil, "l")
	if want := []cobra.Completion{"light"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prefix l: got %v, want %v", got, want)
	}
}

func TestCompleteProfileNames_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got, _ := completeProfileNames(profileUseCmd, nil, ""); len(got) != 0 {
		t.Fatalf("got %v, want no candidates", got)
	}
}

// createMinimalSessionsDB writes a database with only a sessions table, so
// a migration run by the completion shows up as a new table.
func createMinimalSessionsDB(t *testing.T, path string, names ...string) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TABLE sessions (instance_id TEXT PRIMARY KEY, session_name TEXT NOT NULL, started_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		if _, err := conn.Exec(`INSERT INTO sessions VALUES (?, ?, ?)`, n+string(rune('a'+i)), n, i); err != nil {
			t.Fatal(err)
		}
	}
}

func tableCount(t *testing.T, path string) int {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCompleteSessionNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	createMinimalSessionsDB(t, path, "repo@main", "repo@feat", "repo@feat", "other@main")
	before := tableCount(t, path)
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })

	got, d := completeSessionNames(checkinCmd, nil, "")
	if want := []cobra.Completion{"other@main", "repo@feat", "repo@main"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v", d)
	}
	got, _ = completeSessionFlag(cleanupCmd, nil, "repo@")
	if want := []cobra.Completion{"repo@feat", "repo@main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("flag prefix repo@: got %v, want %v", got, want)
	}
	if got, _ := completeSessionNames(logsCmd, []string{"x"}, ""); len(got) != 0 {
		t.Errorf("second argument: got %v, want none", got)
	}
	if after := tableCount(t, path); after != before {
		t.Errorf("sqlite_master entries %d -> %d: completion ran a migration", before, after)
	}
}

func TestCompleteSessionNames_MissingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })

	if got, _ := completeSessionNames(checkinCmd, nil, ""); len(got) != 0 {
		t.Fatalf("got %v, want no candidates", got)
	}
	if got, _ := completeSessionFlag(cleanupCmd, nil, ""); len(got) != 0 {
		t.Fatalf("flag: got %v, want no candidates", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("completion created %s: %v", path, err)
	}
}
