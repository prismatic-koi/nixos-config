package git

// host_project_root_test.go — OpenBare and a project root that is not a prism
// project root in config.json (issue #3330).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectConfig writes a config.json with the project roots of prism,
// and points PRISM_CONFIG_FILE at it.
func writeProjectConfig(t *testing.T, locations, specific []string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"project_locations": locations,
		"project_specific":  specific,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

// newOrigin returns a bare origin repo with one commit on main.
func newOrigin(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	initRepo(t, src, "main")
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, origin).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	return origin
}

// TestOpenBare_RefusesRootOutsideProjectRoots is the second step of the path
// in issue #3330. A sandboxed session makes a .bare in a dir that it can write
// and that is in no prism project, for example under ~/.npm. The .bare holds
// a smudge filter that info/attributes selects, which the env of host git
// does not turn off. The control shows that a pinned worktree add in that
// root runs the payload. Then OpenBare, and each host git command that opens
// the root, must refuse it.
func TestOpenBare_RefusesRootOutsideProjectRoots(t *testing.T) {
	isolateHostGitConfig(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	obsidian := filepath.Join(base, "documents", "obsidian")
	for _, d := range []string{code, obsidian} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectConfig(t, []string{code}, []string{obsidian})

	origin := newOrigin(t)
	planted := filepath.Join(base, "npm", "x")
	bareDir := filepath.Join(planted, ".bare")
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("plant .bare: %v\n%s", err, out)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	if out, err := exec.Command("git", "config", "--file", filepath.Join(bareDir, "config"),
		"filter.evil.smudge", fmt.Sprintf("echo smudge >> '%s'; cat", marker)).CombinedOutput(); err != nil {
		t.Fatalf("plant filter: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(bareDir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareDir, "info", "attributes"), []byte("* filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	takeMarker := func() string {
		data, _ := os.ReadFile(marker)
		_ = os.Remove(marker)
		return strings.TrimSpace(string(data))
	}

	control := Repo{gitDir: bareDir, commonDir: bareDir, dir: bareDir}
	_, _ = control.CombinedOutput("worktree", "add", filepath.Join(planted, "control"), "main")
	if takeMarker() == "" {
		t.Fatal("control: a pinned worktree add in the planted root did not run the smudge filter; the plant does not work")
	}

	// A raw bare git dir is the other form of root that OpenBare takes.
	raw := filepath.Join(base, "npm", "raw.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, raw).CombinedOutput(); err != nil {
		t.Fatalf("plant raw bare dir: %v\n%s", err, out)
	}
	for _, root := range []string{planted, raw, filepath.Join(base, "npm")} {
		if _, err := OpenBare(root); !errors.Is(err, ErrNotProjectRoot) {
			t.Errorf("OpenBare(%s) = %v, want ErrNotProjectRoot", root, err)
		}
	}
	if _, err := OpenBare(planted); err == nil || !strings.Contains(err.Error(), planted) {
		t.Errorf("OpenBare error %v, want it to name %s", err, planted)
	}
	if err := FetchRemote(planted); err == nil {
		t.Error("FetchRemote: want a refusal")
	}
	if _, err := CreateWorktree(planted, "spawned"); err == nil {
		t.Error("CreateWorktree: want a refusal")
	}
	if wts := Worktrees(planted); wts != nil {
		t.Errorf("Worktrees = %v, want nil", wts)
	}
	if _, err := OpenDir(planted); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("OpenDir = %v, want ErrNotProjectRoot", err)
	}
	if _, err := EnvForDir(planted, nil); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("EnvForDir = %v, want ErrNotProjectRoot", err)
	}
	if got := takeMarker(); got != "" {
		t.Errorf("a host git command for the planted root ran the payload: %q", got)
	}
}

// TestOpenBare_AcceptsConfiguredProjectRoots: a dir in a project location
// and a project_specific path are project roots, also when the path goes
// through a symlink, and with another letter case on a case-insensitive
// volume. projecttrust accepts the same paths.
func TestOpenBare_AcceptsConfiguredProjectRoots(t *testing.T) {
	isolateHostGitConfig(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	specific := filepath.Join(base, "documents", "Specific")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(specific), 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, []string{specific})

	origin := newOrigin(t)
	inLocation := filepath.Join(code, "repo")
	for _, root := range []string{inLocation, specific} {
		if err := CloneWorktree(origin, root, func(string) {}); err != nil {
			t.Fatalf("CloneWorktree(%s): %v", root, err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}

	roots := []string{
		inLocation,
		specific,
		filepath.Join(alias, "code", "repo"),
		filepath.Join(alias, "documents", "Specific"),
	}
	upper := strings.ToUpper(base)
	if upper != base {
		if info, err := os.Stat(upper); err == nil && info.IsDir() {
			roots = append(roots,
				filepath.Join(upper, "CODE", "repo"),
				filepath.Join(upper, "DOCUMENTS", "specific"))
		} else {
			t.Logf("the volume of %s is case-sensitive: the letter-case rows do not run", base)
		}
	}
	for _, root := range roots {
		r, err := OpenBare(root)
		if err != nil {
			t.Errorf("OpenBare(%s): %v", root, err)
			continue
		}
		if branch, err := r.Output("symbolic-ref", "--short", "HEAD"); err != nil || branch != "main" {
			t.Errorf("git in OpenBare(%s): HEAD = %q, %v; want main", root, branch, err)
		}
	}
	if _, err := OpenWorktree(filepath.Join(alias, "code", "repo"), filepath.Join(alias, "code", "repo", "main")); err != nil {
		t.Errorf("OpenWorktree through the symlink: %v", err)
	}
}

// TestCloneWorktree_RefusesTargetOutsideProjectRoots: prism clone and the
// clone entry of prism switch refuse a target that OpenBare refuses, before
// the clone. A dir that the clone made is removed, and a dir that was there
// is kept.
func TestCloneWorktree_RefusesTargetOutsideProjectRoots(t *testing.T) {
	isolateHostGitConfig(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)

	outside := filepath.Join(base, "elsewhere", "repo")
	if err := CloneWorktree(origin, outside, func(string) {}); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("CloneWorktree(%s) = %v, want ErrNotProjectRoot", outside, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("the refused clone left %s: %v", outside, err)
	}

	existing := filepath.Join(base, "existing")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(existing, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CloneWorktree(origin, existing, func(string) {}); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("CloneWorktree(%s) = %v, want ErrNotProjectRoot", existing, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the refused clone removed a file that was there: %v", err)
	}

	inside := filepath.Join(code, "repo")
	if err := CloneWorktree(origin, inside, func(string) {}); err != nil {
		t.Errorf("CloneWorktree(%s): %v", inside, err)
	}
}

// TestConvertToBare_RefusesDirOutsideProjectRoots: prism convert refuses a
// repo that OpenBare would refuse after the conversion, and does not change
// it.
func TestConvertToBare_RefusesDirOutsideProjectRoots(t *testing.T) {
	isolateHostGitConfig(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)

	dir := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, dir, filepath.Join(t.TempDir(), "remote.git"), "main")
	if _, err := ConvertToBare(dir, func(string) {}); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("ConvertToBare = %v, want ErrNotProjectRoot", err)
	}
	if info, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !info.IsDir() {
		t.Errorf("the refused conversion changed .git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".bare")); !os.IsNotExist(err) {
		t.Errorf("the refused conversion made .bare: %v", err)
	}
}
