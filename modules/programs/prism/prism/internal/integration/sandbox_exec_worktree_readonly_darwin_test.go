//go:build darwin

package integration_test

// sandbox_exec_worktree_readonly_darwin_test.go — the read-only worktree
// deny of the sandbox-exec profile (section 21d of generateProfile, #3079).
//
// A review agent and an investigator run with Config.WorktreeReadOnly set.
// The positive test runs writes under the production profile of such a
// session and expects each one to fail with a permission error, while a
// read still works. The worker test runs the same writes under the profile
// of a worker and expects them to work. The negative test removes the deny
// from the read-only profile and expects the writes to work, which proves
// that the deny is what stops them.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// newWorktreeReadOnlyManager builds a manager in the production layout: the
// worktree is a direct child of BareRoot, two levels under HOME. The
// worktree holds one file, main.go.
func newWorktreeReadOnlyManager(t *testing.T, readOnly bool) (*container.Manager, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-3079-worktree-readonly-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot := filepath.Join(wrap, "repo")
	worktree := filepath.Join(bareRoot, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(worktree): %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go): %v", err)
	}
	return container.New(container.Config{
		SessionName:      "integ-worktree-readonly",
		InstanceID:       "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:         worktree,
		BareRoot:         bareRoot,
		WorktreeReadOnly: readOnly,
		GitUserName:      "test-user",
		GitUserEmail:     "test@example.com",
	}), worktree
}

// worktreeWriteScript changes main.go and creates new.go in $WT. It exits
// 0 only when both writes work.
const worktreeWriteScript = `echo changed > "$WT/main.go" || exit 10
echo created > "$WT/new.go" || exit 11
exit 0`

// TestSandboxExecWorktreeReadOnly_WriteDenied: under the profile of a
// read-only session, a read of a worktree file works, and a change, a
// create, and a remove each fail with "Operation not permitted".
func TestSandboxExecWorktreeReadOnly_WriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	m, worktree := newWorktreeReadOnlyManager(t, true)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	if out, err := runWorktreeScript(t, profilePath, worktree, `cat "$WT/main.go"`); err != nil || !strings.Contains(string(out), "package main") {
		t.Fatalf("read of a worktree file failed under the read-only profile: %v\nOutput: %s\nProfile: %s", err, out, profilePath)
	}
	for _, script := range []string{
		`echo changed > "$WT/main.go"`,
		`touch "$WT/new.go"`,
		`rm "$WT/main.go"`,
	} {
		out, err := runWorktreeScript(t, profilePath, worktree, script)
		if err == nil {
			t.Errorf("%q succeeded under the read-only profile.\nOutput: %s\nProfile: %s", script, out, profilePath)
			continue
		}
		if !strings.Contains(string(out), "Operation not permitted") {
			t.Errorf("%q failed for a reason other than the sandbox: %v\nOutput: %s", script, err, out)
		}
	}

	if got, err := os.ReadFile(filepath.Join(worktree, "main.go")); err != nil || string(got) != "package main\n" {
		t.Errorf("main.go on the host after the write probes = (%q, %v), want it unchanged", got, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "new.go")); err == nil {
		t.Error("new.go exists on the host: the sandbox wrote into the read-only worktree")
	}
}

// TestSandboxExecWorktreeReadOnly_WorkerCanWrite: under the profile of a
// worker, the same writes work.
func TestSandboxExecWorktreeReadOnly_WorkerCanWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	m, worktree := newWorktreeReadOnlyManager(t, false)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	if out, err := runWorktreeScript(t, profilePath, worktree, worktreeWriteScript); err != nil {
		t.Fatalf("a write to the worktree of a worker failed (exit 10: change, 11: create).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "new.go")); err != nil || string(got) != "created\n" {
		t.Errorf("new.go on the host = (%q, %v), want %q", got, err, "created\n")
	}
}

// TestSandboxExecWorktreeReadOnly_WritableWithoutDeny is the paired
// negative test: with the section-21d deny removed from the read-only
// profile, the same writes work.
func TestSandboxExecWorktreeReadOnly_WritableWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	m, worktree := newWorktreeReadOnlyManager(t, true)
	deny := regexp.MustCompile(`\(deny file-write\*\n  \(subpath "` + regexp.QuoteMeta(worktree) + `"\)\)\n`)
	profilePath := withMutatedProfile(t, m, func(p string) string { return deny.ReplaceAllString(p, "") })

	if out, err := runWorktreeScript(t, profilePath, worktree, worktreeWriteScript); err != nil {
		t.Fatalf("without the deny a write to the worktree still failed, so the positive test proves nothing (exit 10: change, 11: create).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
}
