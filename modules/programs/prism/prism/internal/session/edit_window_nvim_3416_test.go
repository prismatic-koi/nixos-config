package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNvimCmd_Cases_3416(t *testing.T) {
	oneFile := t.TempDir()
	oneFilePath := filepath.Join(oneFile, "notes.md")
	if err := os.WriteFile(oneFilePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	obsidian := filepath.Join(t.TempDir(), "obsidian")
	if err := os.MkdirAll(obsidian, 0o755); err != nil {
		t.Fatal(err)
	}

	readmeDir := t.TempDir()
	readmePath := filepath.Join(readmeDir, "README.md")
	for _, f := range []string{readmePath, filepath.Join(readmeDir, "go.mod")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name, dir, want string
	}{
		{"one file", oneFile, "nvim '" + oneFilePath + "'"},
		{"obsidian", obsidian, "nvim +'Obsidian today'"},
		{"readme", readmeDir, "nvim '" + readmePath + "'"},
		{"empty", t.TempDir(), "nvim"},
	}
	for _, c := range cases {
		if got := NvimCmd(c.dir); got != c.want {
			t.Errorf("%s: NvimCmd(%q) = %q, want %q", c.name, c.dir, got, c.want)
		}
	}
}

// TestSetupFullLayout_EditWindowKeysHaveLeadingSpace_3416 checks that the
// edit-window command starts with exactly one space, so that zsh
// HIST_IGNORE_SPACE does not save it in the history file.
func TestSetupFullLayout_EditWindowKeysHaveLeadingSpace_3416(t *testing.T) {
	_, _ = openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	const sessionName = "prism-test@edit-window-space"
	opts := Opts{
		SessionName:    sessionName,
		Layout:         LayoutFull,
		IsolationMode:  "bwrap",
		PIExtensionDir: testPIExtensionDir,
		ForceFresh:     true,
		Port:           14998,
	}
	if err := Create(sessionName, dir, opts); err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []string{"send-keys", "-t", sessionName + ":0", " " + NvimCmd(dir), "C-m"}
	if args := readSpyArgs(argsFile); !containsSeq(args, want) {
		t.Errorf("tmux args do not contain %q:\n%q", want, args)
	}
}
