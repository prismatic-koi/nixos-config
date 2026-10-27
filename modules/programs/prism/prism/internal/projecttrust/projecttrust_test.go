package projecttrust

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

// TestApprove is the decision table of issue #3216. Each "true" row is a
// session of a trusted repo in a worktree root or in the project directory.
// Each "false" row breaks exactly one condition of Approve.
func TestApprove(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops", "obsidian")
	homeOpsMain := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	homeOpsNested := projecttrusttest.Worktree(t, l.Code, "home-ops", "feat/x")
	nixosMain := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
	// A layout that copies home-ops in a directory that a sandbox can write.
	forged := projecttrusttest.Worktree(t, l.Writable, "home-ops", "main")
	forgedFlat := filepath.Join(l.Writable, "obsidian")
	if err := os.MkdirAll(forgedFlat, 0o700); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(homeOpsMain, "sub")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A worktree whose .git file points outside its own bare repo.
	stray := filepath.Join(l.Code, "home-ops", "stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, ".git"), []byte("gitdir: "+filepath.Join(l.Code, "nixos-config", ".bare", "worktrees", "main")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := Load()
	cases := []struct {
		name    string
		session string
		dir     string
		want    bool
	}{
		{"trusted coordinator worktree", "home-ops@main", homeOpsMain, true},
		{"trusted worker in a nested worktree", "home-ops@feat--x", homeOpsNested, true},
		{"review agent of a trusted worker", "home-ops@feat--x~review-1-review-goal", homeOpsNested, true},
		{"non-worktree project with a bare session name", "obsidian", l.Obsidian, true},
		{"investigator of a non-worktree project", "obsidian~investigate-v2", l.Obsidian, true},
		{"trailing slash is cleaned", "home-ops@main", homeOpsMain + "/", true},

		{"repo not in the list", "nixos-config@main", nixosMain, false},
		{"forged layout under a writable directory", "home-ops@main", forged, false},
		{"bare name in a writable directory", "obsidian", forgedFlat, false},
		{"project root that holds .bare", "home-ops@main", filepath.Join(l.Code, "home-ops"), false},
		{"bare name in the project root that holds .bare", "home-ops", filepath.Join(l.Code, "home-ops"), false},
		{"subdirectory of a worktree", "home-ops@main", subdir, false},
		{".git points into another bare repo", "home-ops@stray", stray, false},
		{"bare name in a worktree", "home-ops", homeOpsMain, false},
		{"branch name in a non-worktree project", "obsidian@x", l.Obsidian, false},
		{"name of another repo", "nixos-config@main", homeOpsMain, false},
		{"name of a trusted repo in an untrusted worktree", "home-ops@main", nixosMain, false},
		{"relative directory", "home-ops@main", "code/home-ops/main", false},
		{"empty directory", "home-ops@main", "", false},
		{"missing directory", "home-ops@gone", filepath.Join(l.Code, "home-ops", "gone"), false},
		{"meta-session", "scratchpad", l.Obsidian, false},
		{"empty session name", "", l.Obsidian, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Approve(tc.session, tc.dir); got != tc.want {
				t.Errorf("Approve(%q, %q) = %v, want %v", tc.session, tc.dir, got, tc.want)
			}
		})
	}
}

// TestApprove_EmptyListTrustsNoRepo: with no project-trust.json, every pi
// starts with --no-approve.
func TestApprove_EmptyListTrustsNoRepo(t *testing.T) {
	l := projecttrusttest.New(t)
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	if err := os.Remove(config.ProjectTrustPath()); err != nil {
		t.Fatal(err)
	}
	p := Load()
	if len(p.TrustedRepos) != 0 {
		t.Fatalf("TrustedRepos = %v, want empty", p.TrustedRepos)
	}
	if p.Approve("home-ops@main", wt) || p.Approve("obsidian", l.Obsidian) {
		t.Error("Approve = true with no project-trust.json, want false")
	}
}

// TestLoad_MalformedFileTrustsNoRepo: an unreadable list fails closed.
func TestLoad_MalformedFileTrustsNoRepo(t *testing.T) {
	l := projecttrusttest.New(t, "obsidian")
	if err := os.WriteFile(config.ProjectTrustPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := Load(); p.Approve("obsidian", l.Obsidian) {
		t.Errorf("Approve = true with a malformed project-trust.json (TrustedRepos %v), want false", p.TrustedRepos)
	}
}

// TestLoad_ReadsBothFiles: Load takes the repo list from project-trust.json
// and the project roots from config.json.
func TestLoad_ReadsBothFiles(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops", "obsidian")
	p := Load()
	if !slices.Equal(p.TrustedRepos, []string{"home-ops", "obsidian"}) {
		t.Errorf("TrustedRepos = %v", p.TrustedRepos)
	}
	if !slices.Equal(p.ProjectLocations, []string{l.Code}) || !slices.Equal(p.ProjectSpecific, []string{l.Obsidian}) {
		t.Errorf("project roots = %v / %v, want %v / %v", p.ProjectLocations, p.ProjectSpecific, l.Code, l.Obsidian)
	}
}

// TestApprove_TildeProjectRoots: config.json holds "~/code" and
// "~/documents/obsidian" as Nix renders them.
func TestApprove_TildeProjectRoots(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops", "obsidian")
	t.Setenv("HOME", l.Root)
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	p := Policy{
		TrustedRepos:     []string{"home-ops", "obsidian"},
		ProjectLocations: []string{"~/code"},
		ProjectSpecific:  []string{"~/documents/obsidian"},
	}
	if !p.Approve("home-ops@main", wt) {
		t.Error("Approve(home-ops@main) = false with ~/code, want true")
	}
	if !p.Approve("obsidian", l.Obsidian) {
		t.Error("Approve(obsidian) = false with ~/documents/obsidian, want true")
	}
}

// TestApprove_SymlinkedProjectRootMatchesByIdentity: the project root
// compares by file identity, so a different path to the same directory
// matches (a case-insensitive macOS volume is the same class).
func TestApprove_SymlinkedProjectRootMatchesByIdentity(t *testing.T) {
	l := projecttrusttest.New(t, "obsidian")
	alias := filepath.Join(l.Root, "Documents")
	if err := os.Symlink(filepath.Join(l.Root, "documents"), alias); err != nil {
		t.Fatal(err)
	}
	if !Load().Approve("obsidian", filepath.Join(alias, "obsidian")) {
		t.Error("Approve(obsidian, <alias>/obsidian) = false, want true")
	}
}

// TestApprove_DotInRepoDirectory: session.NameFor replaces "." with "_", so
// the session of a directory "my.repo" is "my_repo@main". The list holds
// the directory name.
func TestApprove_DotInRepoDirectory(t *testing.T) {
	l := projecttrusttest.New(t, "my.repo")
	wt := projecttrusttest.Worktree(t, l.Code, "my.repo", "main")
	p := Load()
	if !p.Approve("my_repo@main", wt) {
		t.Error("Approve(my_repo@main) = false, want true")
	}
	if p.Approve("my.repo@main", wt) {
		t.Error("Approve(my.repo@main) = true; prism never gives that name")
	}
}
