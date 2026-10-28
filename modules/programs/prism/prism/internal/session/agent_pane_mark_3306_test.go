package session

import (
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// spyTmuxBinPaneID installs a fake tmux that writes one line per call to
// the returned file. For a call with -P it prints the pane ID %42, as
// `tmux new-window -P -F '#{pane_id}'` does.
func spyTmuxBinPaneID(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	callsFile := dir + "/tmux-calls"
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + callsFile + "\n" +
		"case \" $* \" in *' -P '*) echo '%42' ;; esac\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("write spy tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = dir + "/tmux"
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return callsFile
}

// requireAgentPaneMarked fails t unless the calls create the agent window
// with -P and then set tmux.AgentPaneOption on the pane that it printed.
// The pane-died hook acts only for a pane with that option (#3306).
func requireAgentPaneMarked(t *testing.T, callsFile string) {
	t.Helper()
	data, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatalf("read spy calls: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	created := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "new-window ") && strings.Contains(c, " -n agent ") {
			if !strings.Contains(c, " -P -F #{pane_id} ") {
				t.Errorf("agent new-window %q does not print the pane ID", c)
			}
			created = i
		}
	}
	if created < 0 {
		t.Fatalf("no agent new-window call in:\n%s", data)
	}
	want := "set-option -p -t %42 " + tmux.AgentPaneOption + " 1"
	for _, c := range calls[created+1:] {
		if c == want {
			return
		}
	}
	t.Errorf("no %q call after the agent new-window in:\n%s", want, data)
}

func TestSetupFullLayout_MarksAgentPane_3306(t *testing.T) {
	_, _ = openSpawnTestDB(t)
	callsFile := spyTmuxBinPaneID(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@mark-full"
	opts := Opts{
		SessionName:    sessionName,
		Layout:         LayoutFull,
		IsolationMode:  "bwrap",
		PIExtensionDir: testPIExtensionDir,
		ForceFresh:     true,
		SkipStatusSeed: true,
	}
	if err := Create(sessionName, t.TempDir(), opts); err != nil {
		t.Fatalf("Create: %v", err)
	}
	requireAgentPaneMarked(t, callsFile)
}

func TestSpawnAgentOnlyLayout_MarksAgentPane_3306(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	callsFile := spyTmuxBinPaneID(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	opts := SpawnOpts{
		SessionName:    "myrepo@branch~review-1-review-qa",
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-qa",
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	}
	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	requireAgentPaneMarked(t, callsFile)
}
