package iris

// sandbox_file_tools_test.go — unit tests for validateToolPath and related helpers.
//
// These are plain Go unit tests (no build tags, no network, no subprocess).
// They exercise the path validator's normalisation, symlink resolution, and
// worktree-containment logic.

import (
	"os"
	"path/filepath"
	"testing"
)

// makeTree is a test helper that creates a directory structure under a temp
// dir and returns the temp dir path.
func makeTree(t *testing.T, entries map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range entries {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("makeTree MkdirAll %q: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("makeTree WriteFile %q: %v", abs, err)
		}
	}
	return root
}

func TestValidateToolPath_AcceptsWorktreeFile(t *testing.T) {
	worktree := makeTree(t, map[string]string{
		"README.md": "hello",
	})
	tmpDir := t.TempDir()

	resolved, err := validateToolPath(worktree, tmpDir, "README.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(worktree, "README.md")
	if resolved != want {
		t.Errorf("resolved = %q, want %q", resolved, want)
	}
}

func TestValidateToolPath_AcceptsAbsoluteWorktreePath(t *testing.T) {
	worktree := makeTree(t, map[string]string{
		"sub/file.txt": "content",
	})
	tmpDir := t.TempDir()

	abs := filepath.Join(worktree, "sub/file.txt")
	resolved, err := validateToolPath(worktree, tmpDir, abs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != abs {
		t.Errorf("resolved = %q, want %q", resolved, abs)
	}
}

func TestValidateToolPath_AcceptsTmpPath(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := makeTree(t, map[string]string{
		"scratch.txt": "data",
	})

	abs := filepath.Join(tmpDir, "scratch.txt")
	resolved, err := validateToolPath(worktree, tmpDir, abs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != abs {
		t.Errorf("resolved = %q, want %q", resolved, abs)
	}
}

func TestValidateToolPath_RejectsEtcPasswd(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	_, err := validateToolPath(worktree, tmpDir, "/etc/passwd")
	if err == nil {
		t.Error("expected error for /etc/passwd, got nil")
	}
}

func TestValidateToolPath_RejectsSSHKey(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	// Simulate a path like /home/user/.ssh/id_ed25519
	home, _ := os.UserHomeDir()
	sshKey := filepath.Join(home, ".ssh", "id_ed25519")

	_, err := validateToolPath(worktree, tmpDir, sshKey)
	if err == nil {
		t.Error("expected error for SSH key path, got nil")
	}
}

func TestValidateToolPath_RejectsDotDotTraversal(t *testing.T) {
	worktree := makeTree(t, map[string]string{
		"file.txt": "content",
	})
	tmpDir := t.TempDir()

	// Attempt ../../../etc/passwd relative to worktree.
	traversal := "../../etc/passwd"
	_, err := validateToolPath(worktree, tmpDir, traversal)
	if err == nil {
		t.Errorf("expected error for dot-dot traversal %q, got nil", traversal)
	}
}

func TestValidateToolPath_RejectsSymlinkTraversal(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	// Create a symlink inside the worktree that points at /etc/passwd.
	symlink := filepath.Join(worktree, "evil-link")
	if err := os.Symlink("/etc/passwd", symlink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := validateToolPath(worktree, tmpDir, symlink)
	if err == nil {
		t.Error("expected error for symlink pointing to /etc/passwd, got nil")
	}
}

func TestValidateToolPath_RejectsSymlinkInsideTmpPointingOutside(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	// Create a symlink inside tmpDir that points at /etc/passwd.
	symlink := filepath.Join(tmpDir, "link-to-etc")
	if err := os.Symlink("/etc/passwd", symlink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := validateToolPath(worktree, tmpDir, symlink)
	if err == nil {
		t.Error("expected error for symlink in tmpDir pointing to /etc/passwd, got nil")
	}
}

func TestValidateToolPath_AcceptsNonExistentFileUnderWorktree(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	// Path doesn't exist yet — should be accepted (write creates it).
	newFile := filepath.Join(worktree, "new-file.txt")
	resolved, err := validateToolPath(worktree, tmpDir, newFile)
	if err != nil {
		t.Fatalf("unexpected error for non-existent worktree file: %v", err)
	}
	if resolved != newFile {
		t.Errorf("resolved = %q, want %q", resolved, newFile)
	}
}

func TestValidateToolPath_AcceptsNonExistentFileUnderTmpDir(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	newFile := filepath.Join(tmpDir, "new-scratch.txt")
	resolved, err := validateToolPath(worktree, tmpDir, newFile)
	if err != nil {
		t.Fatalf("unexpected error for non-existent tmpDir file: %v", err)
	}
	if resolved != newFile {
		t.Errorf("resolved = %q, want %q", resolved, newFile)
	}
}

func TestValidateToolPath_RejectsWorktreeParent(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	// The parent of the worktree is outside the worktree.
	parent := filepath.Dir(worktree)
	_, err := validateToolPath(worktree, tmpDir, parent)
	if err == nil {
		t.Errorf("expected error for worktree parent %q, got nil", parent)
	}
}

func TestValidateToolPath_RejectsHomeDir(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	home, _ := os.UserHomeDir()
	_, err := validateToolPath(worktree, tmpDir, home)
	if err == nil {
		t.Error("expected error for home dir, got nil")
	}
}

func TestIsUnder(t *testing.T) {
	cases := []struct {
		child string
		base  string
		want  bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a/b", "/a/bc", false},
		{"/a", "/a/b", false},
		{"/", "/a", false},
	}
	for _, c := range cases {
		got := isUnder(c.child, c.base)
		if got != c.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", c.child, c.base, got, c.want)
		}
	}
}

func TestValidateToolPath_ErrorMessageNamesPath(t *testing.T) {
	worktree := t.TempDir()
	tmpDir := t.TempDir()

	_, err := validateToolPath(worktree, tmpDir, "/etc/hosts")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errMsg := err.Error()
	// The error message should name the rejected path.
	if !contains(errMsg, "/etc/hosts") && !contains(errMsg, "outside") {
		t.Errorf("error message does not name path or say 'outside': %q", errMsg)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
