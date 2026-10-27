package container

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// piSessionHeaderLine returns the first line of a pi session file: a header
// with id and cwd, in the shape that pi 1.4 writes.
func piSessionHeaderLine(t *testing.T, id, cwd string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":      "session",
		"version":   3,
		"id":        id,
		"timestamp": "2026-01-02T03:04:05.000Z",
		"cwd":       cwd,
	})
	if err != nil {
		t.Fatalf("marshal session header: %v", err)
	}
	return append(b, '\n')
}

// resumeCwdFixture makes a sessions root under PI_CODING_AGENT_DIR and
// returns the sessions dir of worktree.
func resumeCwdFixture(t *testing.T, worktree string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", root)
	dir := filepath.Join(root, "sessions", encodePiCWD(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	return dir
}

func writeSessionFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const (
	resumeCwdWorktree = "/home/user/code/myrepo/feature"
	resumeCwdID       = "019e3284-1111-2222-3333-444444444444"
)

func resumeCwdCfg() Config {
	return Config{SessionName: "myrepo@feature", Worktree: resumeCwdWorktree, HarnessSessionID: resumeCwdID}
}

func TestCheckPIResumeCwd_HeaderCwdIsWorktree(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	content := append(piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree), []byte(`{"type":"message"}`+"\n")...)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", content)
	// A session of another id in the same dir has no effect.
	writeSessionFile(t, dir, "2026-01-02T03-04-06-000Z_other.jsonl", piSessionHeaderLine(t, "other", "/elsewhere"))

	if err := CheckPIResumeCwd(resumeCwdCfg()); err != nil {
		t.Fatalf("CheckPIResumeCwd = %v, want nil", err)
	}
}

func TestCheckPIResumeCwd_RefusesForeignCwd(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	path := writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl",
		piSessionHeaderLine(t, resumeCwdID, "/home/user/.npm/planted"))

	err := CheckPIResumeCwd(resumeCwdCfg())
	if err == nil {
		t.Fatal("CheckPIResumeCwd = nil for a header cwd outside the worktree, want an error")
	}
	for _, want := range []string{path, `"/home/user/.npm/planted"`, resumeCwdWorktree} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// pi opens the first file whose header id is the id, in directory order,
// and ignores the file name. A second file with the same header id and a
// foreign cwd must make the check fail, whatever its name.
func TestCheckPIResumeCwd_RefusesSecondFileWithSameHeaderID(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl",
		piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree))
	writeSessionFile(t, dir, "0000_planted.jsonl", piSessionHeaderLine(t, resumeCwdID, "/tmp/planted"))

	if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil || !strings.Contains(err.Error(), "/tmp/planted") {
		t.Fatalf("CheckPIResumeCwd = %v, want an error that names /tmp/planted", err)
	}
}

// pi falls back to a prefix match on the header id.
func TestCheckPIResumeCwd_RefusesPrefixMatchWithForeignCwd(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl",
		piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree))
	writeSessionFile(t, dir, "x.jsonl", piSessionHeaderLine(t, resumeCwdID+"9", "/tmp/planted"))

	if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil {
		t.Fatal("CheckPIResumeCwd = nil with a prefix-match file outside the worktree, want an error")
	}
}

// pi skips blank lines, lines that are not JSON, and JSON null before the
// header. pi never writes such lines, so the check refuses them.
func TestCheckPIResumeCwd_RefusesSkippedLinesBeforeHeader(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	content := append([]byte("\n   \nnot json\nnull\n"), piSessionHeaderLine(t, resumeCwdID, "/tmp/planted")...)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", content)

	if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil {
		t.Fatal("CheckPIResumeCwd = nil for a header after skipped lines, want an error")
	}
}

func TestCheckPIResumeCwd_RefusesMissingOrNonStringCwd(t *testing.T) {
	for name, header := range map[string]string{
		"missing":  `{"type":"session","id":"` + resumeCwdID + `"}`,
		"number":   `{"type":"session","id":"` + resumeCwdID + `","cwd":1}`,
		"relative": `{"type":"session","id":"` + resumeCwdID + `","cwd":"feature"}`,
		"tilde":    `{"type":"session","id":"` + resumeCwdID + `","cwd":"~/code/myrepo/feature"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := resumeCwdFixture(t, resumeCwdWorktree)
			writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", []byte(header+"\n"))
			if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil {
				t.Fatalf("CheckPIResumeCwd = nil for header %s, want an error", header)
			}
		})
	}
}

// A file name that carries the id is not sufficient: pi selects by the
// header id.
func TestCheckPIResumeCwd_RefusesWhenNoHeaderHasTheID(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl", piSessionHeaderLine(t, "other", resumeCwdWorktree))

	if err := CheckPIResumeCwd(resumeCwdCfg()); err == nil || !strings.Contains(err.Error(), "has the id") {
		t.Fatalf("CheckPIResumeCwd = %v, want the no-header-id error", err)
	}
}

// pi reads a --session value with '/' or a ".jsonl" suffix as a file path.
func TestCheckPIResumeCwd_RefusesIDThatPiReadsAsAPath(t *testing.T) {
	resumeCwdFixture(t, resumeCwdWorktree)
	for _, id := range []string{"x.jsonl", "../../x", "a/b", "", "-a"} {
		cfg := resumeCwdCfg()
		cfg.HarnessSessionID = id
		if err := CheckPIResumeCwd(cfg); err == nil {
			t.Errorf("CheckPIResumeCwd(id %q) = nil, want an error", id)
		}
	}
}

// An open of a FIFO blocks, in pi too. The check refuses the resume, and
// it must not block, or a planted FIFO stops `prism restore`.
func TestCheckPIResumeCwd_RefusesFIFOWithoutBlocking(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl",
		piSessionHeaderLine(t, resumeCwdID, resumeCwdWorktree))
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.jsonl"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- CheckPIResumeCwd(resumeCwdCfg()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("CheckPIResumeCwd = %v, want the not-a-regular-file error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CheckPIResumeCwd blocked on a FIFO")
	}
}

func TestCheckPIResumeCwd_MissingDirIsNotExist(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	err := CheckPIResumeCwd(resumeCwdCfg())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("CheckPIResumeCwd = %v, want an error that wraps fs.ErrNotExist", err)
	}
}

// PIInvocation must not append --session for a header cwd outside the
// worktree, and must log the reason to the agent-run log.
func TestPIInvocation_Resume_OmitsSessionForForeignHeaderCwd(t *testing.T) {
	dir := resumeCwdFixture(t, resumeCwdWorktree)
	stateHome := os.Getenv("XDG_STATE_HOME")
	writeSessionFile(t, dir, "2026-01-02T03-04-05-000Z_"+resumeCwdID+".jsonl",
		piSessionHeaderLine(t, resumeCwdID, "/tmp/planted"))

	cfg := bwrapResumeCfg("myrepo@feature", resumeCwdWorktree, resumeCwdID)
	args := PIInvocation(cfg)
	if hasArg(args, "--session") {
		t.Fatalf("PIInvocation appended --session for a foreign header cwd: %v", redactedArgs(args))
	}

	logPath := filepath.Join(stateHome, "prism", "run", sessionDirName("myrepo@feature"), "agent-run.log")
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent-run.log: %v", err)
	}
	if !strings.Contains(string(body), "not resuming pi session "+resumeCwdID) || !strings.Contains(string(body), "/tmp/planted") {
		t.Errorf("agent-run.log = %q, want the refusal and the header cwd", body)
	}
}
