package rundir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// plantFixture makes a run dir and a target file outside it with the given
// content, as the files of another session or of the user.
func plantFixture(t *testing.T) (runDir, target string) {
	t.Helper()
	root := t.TempDir()
	runDir = filepath.Join(root, "run", "abc")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	return runDir, target
}

// symlinkedRunDir makes a run dir path that is a symlink to another dir, and
// returns both.
func symlinkedRunDir(t *testing.T) (runDir, other string) {
	t.Helper()
	root := t.TempDir()
	other = filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	runDir = filepath.Join(root, "own")
	if err := os.Symlink(other, runDir); err != nil {
		t.Fatal(err)
	}
	return runDir, other
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s holds %q, want %q", path, b, want)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("%s holds %d entries after a refused write, want none", dir, len(entries))
	}
}

func TestEnsureDir_MakesAndAcceptsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "abc")
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir of a missing dir: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("EnsureDir made %v (%v), want a dir with mode 0700", info, err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Errorf("EnsureDir of an existing dir: %v", err)
	}
}

func TestEnsureDir_RefusesSymlinkAndFile(t *testing.T) {
	runDir, _ := symlinkedRunDir(t)
	if err := EnsureDir(runDir); !errors.Is(err, ErrSymlinkDir) {
		t.Errorf("EnsureDir of a symlinked dir: %v, want ErrSymlinkDir", err)
	}

	dangling := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dangling); !errors.Is(err, ErrSymlinkDir) {
		t.Errorf("EnsureDir of a dangling symlink: %v, want ErrSymlinkDir", err)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(file); err == nil {
		t.Error("EnsureDir of a file returned no error")
	}
}

func TestOpenAppend_CreatesAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "abc", "agent-run.log")
	for _, line := range []string{"one\n", "two\n"} {
		f, err := OpenAppend(path)
		if err != nil {
			t.Fatalf("OpenAppend: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertContent(t, path, "one\ntwo\n")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode of %s: %v (%v), want 0600", path, info.Mode().Perm(), err)
	}
}

func TestOpenAppend_RefusesSymlinkedFile(t *testing.T) {
	runDir, target := plantFixture(t)
	path := filepath.Join(runDir, "agent-run.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	f, err := OpenAppend(path)
	if err == nil {
		_, _ = f.WriteString("planted\n")
		_ = f.Close()
		t.Fatal("OpenAppend opened a symlink")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("OpenAppend of a symlink: %v, want ELOOP", err)
	}
	assertContent(t, target, "keep")
}

func TestOpenAppend_RefusesSymlinkedDir(t *testing.T) {
	runDir, other := symlinkedRunDir(t)
	if f, err := OpenAppend(filepath.Join(runDir, "agent-run.log")); !errors.Is(err, ErrSymlinkDir) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("OpenAppend in a symlinked run dir: %v, want ErrSymlinkDir", err)
	}
	assertEmptyDir(t, other)
}

func TestOpenAppend_RefusesFIFOWithoutWait(t *testing.T) {
	runDir, _ := plantFixture(t)
	path := filepath.Join(runDir, "agent-run.log")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := OpenAppend(path)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("OpenAppend opened a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenAppend waits on a FIFO with no reader")
	}
}

func TestOpenRead_ReadsAndRefusesSymlink(t *testing.T) {
	runDir, target := plantFixture(t)
	path := filepath.Join(runDir, "agent-run.log")
	if err := os.WriteFile(path, []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead of a regular file: %v", err)
	}
	_ = f.Close()

	link := filepath.Join(runDir, "agent-startup.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRead(link); !errors.Is(err, syscall.ELOOP) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("OpenRead of a symlink: %v, want ELOOP", err)
	}
}

func TestOpenRead_RefusesFIFOAndDir(t *testing.T) {
	runDir, _ := plantFixture(t)
	fifo := filepath.Join(runDir, "agent-run.log")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	for _, path := range []string{fifo, runDir} {
		if f, err := OpenRead(path); !errors.Is(err, ErrNotRegular) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("OpenRead(%s): %v, want ErrNotRegular", path, err)
		}
	}
}

func TestWriteFile_WritesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "abc", "initial-prompt.txt")
	for _, content := range []string{"first", "second"} {
		if err := WriteFile(path, []byte(content)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		assertContent(t, path, content)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode of %s: %v (%v), want 0600", path, info.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the dir holds %d entries, want only the file (no temp file left)", len(entries))
	}
}

func TestWriteFile_ReplacesSymlinkWithoutFollow(t *testing.T) {
	runDir, target := plantFixture(t)
	path := filepath.Join(runDir, "initial-prompt.txt")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("prompt")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	assertContent(t, target, "keep")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s is %v (%v) after WriteFile, want a regular file", path, info, err)
	}
	assertContent(t, path, "prompt")
}

func TestWriteFile_RefusesSymlinkedDir(t *testing.T) {
	runDir, other := symlinkedRunDir(t)
	if err := WriteFile(filepath.Join(runDir, "initial-prompt.txt"), []byte("prompt")); !errors.Is(err, ErrSymlinkDir) {
		t.Errorf("WriteFile in a symlinked run dir: %v, want ErrSymlinkDir", err)
	}
	assertEmptyDir(t, other)
}

func TestEnsureDir_KeepsModeOfExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("mode of %s after EnsureDir: %v (%v), want 0755", dir, info.Mode().Perm(), err)
	}
}

func TestWriteFile_RefusesDirAtPath(t *testing.T) {
	runDir, _ := plantFixture(t)
	path := filepath.Join(runDir, "initial-prompt.txt")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("prompt")); err == nil {
		t.Error("WriteFile replaced a dir at the path")
	}
	entries, _ := os.ReadDir(runDir)
	if len(entries) != 1 {
		t.Errorf("the run dir holds %d entries after the refused write, want only the dir (no temp file left)", len(entries))
	}
}

func TestRefuseSymlink_RefusesSymlinkAndNamesPath(t *testing.T) {
	runDir, target := plantFixture(t)
	sock := filepath.Join(runDir, "hostapi.sock")
	if err := os.Symlink(target, sock); err != nil {
		t.Fatal(err)
	}
	err := RefuseSymlink(sock)
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("RefuseSymlink of a symlink: %v, want ErrSymlink", err)
	}
	if !strings.Contains(err.Error(), sock) {
		t.Errorf("the error %q does not name the path %s", err, sock)
	}
}

func TestRefuseSymlink_RefusesSymlinkedDir(t *testing.T) {
	runDir, _ := symlinkedRunDir(t)
	if err := RefuseSymlink(filepath.Join(runDir, "hostapi.sock")); !errors.Is(err, ErrSymlinkDir) {
		t.Errorf("RefuseSymlink in a symlinked dir: %v, want ErrSymlinkDir", err)
	}
}

func TestRefuseSymlink_AcceptsFileAndMissingPath(t *testing.T) {
	runDir, _ := plantFixture(t)
	file := filepath.Join(runDir, "hostapi.sock")
	if err := RefuseSymlink(file); err != nil {
		t.Errorf("RefuseSymlink of a missing path: %v", err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefuseSymlink(file); err != nil {
		t.Errorf("RefuseSymlink of a regular file: %v", err)
	}
}
