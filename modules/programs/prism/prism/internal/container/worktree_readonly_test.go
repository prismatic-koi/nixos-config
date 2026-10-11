package container

// worktree_readonly_test.go — argv and profile coverage for
// Config.WorktreeReadOnly (#3079). The live bwrap tests are in
// worktree_readonly_bwrap_test.go. The live sandbox-exec tests are in
// internal/integration/sandbox_exec_worktree_readonly_darwin_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeBinds returns every bind triple in args whose destination is the
// worktree or an ancestor of it. A bind on an ancestor that follows the
// worktree bind hides it.
func worktreeBinds(args []string, worktree string) [][3]string {
	var out [][3]string
	for i := 0; i+2 < len(args); i++ {
		switch args[i] {
		case "--bind", "--ro-bind", "--bind-try", "--ro-bind-try", "--dev-bind", "--dev-bind-try":
		default:
			continue
		}
		dst := filepath.Clean(args[i+2])
		if dst == worktree || strings.HasPrefix(worktree, dst+string(filepath.Separator)) || dst == "/" {
			out = append(out, [3]string{args[i], args[i+1], args[i+2]})
		}
	}
	return out
}

func TestBwrapBuildArgs_WorktreeReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		wantFlag string
	}{
		{"review agent", true, "--ro-bind"},
		{"worker", false, "--bind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree := t.TempDir()
			m, _, cleanup := bwrapFixture(t, Config{
				SessionName:      "repo@main",
				Worktree:         worktree,
				AllocatedPort:    14010,
				WorktreeReadOnly: tc.readOnly,
			})
			defer cleanup()

			args := (&bwrapIsolator{name: m.name}).BuildArgs(m)
			got := worktreeBinds(args, worktree)
			want := [3]string{tc.wantFlag, worktree, worktree}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("binds on the worktree or an ancestor = %q, want exactly %q", got, want)
			}
		})
	}
}

// TestGenerateProfile_WorktreeReadOnlyDenyFollowsEveryAllow: for a
// read-only session, the profile denies file-write* on the worktree after
// the last allow. SBPL takes the later rule, so an allow after the deny
// gives the write back. The section-6 grants on the worktree and BareRoot
// stay, so the deny is what makes the worktree read-only.
func TestGenerateProfile_WorktreeReadOnlyDenyFollowsEveryAllow(t *testing.T) {
	m := newAuditLocationTestManager(t)
	m.cfg.WorktreeReadOnly = true
	profile := generateProfile(m)

	deny := "(deny file-write*\n  (subpath " + quoteSBPL(m.cfg.Worktree) + "))\n"
	denyAt := strings.Index(profile, deny)
	if denyAt < 0 {
		t.Fatalf("profile has no read-only worktree deny %q.\nprofile:\n%s", deny, profile)
	}
	if allowAt := strings.LastIndex(profile, "(allow "); allowAt > denyAt {
		t.Errorf("an allow clause (offset %d) follows the read-only worktree deny (offset %d); SBPL takes the later rule.\nprofile:\n%s",
			allowAt, denyAt, profile)
	}
	for _, grant := range []string{m.cfg.Worktree, m.cfg.BareRoot} {
		if !strings.Contains(profile, "  (subpath "+quoteSBPL(grant)+")\n") {
			t.Fatalf("fixture premise changed: the profile no longer grants (subpath %s)", grant)
		}
	}
}

// TestGenerateProfile_WorkerHasNoWorktreeReadOnlyDeny: a worker profile has
// no write deny on its worktree.
func TestGenerateProfile_WorkerHasNoWorktreeReadOnlyDeny(t *testing.T) {
	m := newAuditLocationTestManager(t)
	profile := generateProfile(m)
	if strings.Contains(profile, "(deny file-write*\n  (subpath "+quoteSBPL(m.cfg.Worktree)) {
		t.Fatalf("a worker profile denies writes to its worktree.\nprofile:\n%s", profile)
	}
}

// TestWorktreeReadOnlyDeny_ListsResolvedPath: SBPL matches the resolved
// path, so a worktree path with a symlink in it gets both paths.
func TestWorktreeReadOnlyDeny_ListsResolvedPath(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	real, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	clause := worktreeReadOnlyDeny(link)
	for _, p := range []string{link, real} {
		if !strings.Contains(clause, "(subpath "+quoteSBPL(p)+")") {
			t.Errorf("deny clause does not list %s:\n%s", p, clause)
		}
	}
}
