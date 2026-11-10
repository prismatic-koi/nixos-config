package reposkills_test

// reposkills_test.go — the --skill paths of a session come only from
// repo-skills.json, for the repo that SessionProject gives, and each path
// is in the nix store (issue #3463).

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/reposkills"
	"github.com/prismatic-koi/prism/internal/reposkills/reposkillstest"
)

// collect returns a warn function and the slice that it appends to.
func collect() (func(string), *[]string) {
	var got []string
	return func(msg string) { got = append(got, msg) }, &got
}

func TestPaths_ByRepoIdentity(t *testing.T) {
	l := projecttrusttest.New(t)
	store := reposkillstest.Store(t)
	theming := reposkillstest.Skill(t, store, "site-theming")
	other := reposkillstest.Skill(t, store, "other")
	notes := reposkillstest.Skill(t, store, "notes")
	nixosMain := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
	nixosFeat := projecttrusttest.Worktree(t, l.Code, "nixos-config", "feat")
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	forged := filepath.Join(l.Writable, "nixos-config")
	subdir := filepath.Join(nixosMain, "modules")
	for _, d := range []string{forged, subdir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reposkillstest.WriteFile(t, l.ConfigDir, map[string][]string{
		"nixos-config": {theming, other},
		"obsidian":     {notes},
	})
	cfg := config.Config{ProjectLocations: []string{l.Code}, ProjectSpecific: []string{l.Obsidian}}
	both := []string{theming, other}

	cases := []struct {
		name, session, dir string
		want               []string
	}{
		{"coordinator", "nixos-config@main", nixosMain, both},
		{"worker", "nixos-config@feat", nixosFeat, both},
		{"review agent", "nixos-config@feat~review-1-review-goal", nixosFeat, both},
		{"fallback reviewer", "nixos-config@feat~review-2-review-qa-fallback-1", nixosFeat, both},
		{"investigator", "nixos-config@feat~investigate-x", nixosFeat, both},
		{"review agent of an investigator", "nixos-config@feat~investigate-x~review-1-review-qa", nixosFeat, both},
		{"non-worktree project", "obsidian", l.Obsidian, []string{notes}},

		{"repo with no entry", "home-ops@main", homeOps, nil},
		{"name of a repo with skills in a worktree of another repo", "nixos-config@main", homeOps, nil},
		{"name of a repo with skills in a dir that is not a project root", "nixos-config", forged, nil},
		{"subdirectory of a worktree", "nixos-config@main", subdir, nil},
		{"empty dir", "nixos-config@main", "", nil},
		{"relative dir", "nixos-config@main", "nixos-config/main", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, warnings := collect()
			got := reposkills.Paths(tc.session, tc.dir, cfg, warn)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Paths(%q, %q) = %q, want %q", tc.session, tc.dir, got, tc.want)
			}
			if len(*warnings) != 0 {
				t.Errorf("warnings = %q, want none", *warnings)
			}
		})
	}
}

// A file that prism cannot use gives no --skill path and one warning that
// names the file.
func TestPaths_UnusableFile(t *testing.T) {
	cases := []struct {
		name string
		body []byte // nil: no file
	}{
		{"missing file", nil},
		{"malformed JSON", []byte(`{"nixos-config": [`)},
		{"wrong shape", []byte(`{"nixos-config": "/nix/store/0000-x"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := projecttrusttest.New(t)
			wt := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
			if tc.body != nil {
				reposkillstest.WriteRaw(t, l.ConfigDir, tc.body)
			}
			warn, warnings := collect()
			got := reposkills.Paths("nixos-config@main", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
			if got != nil {
				t.Errorf("Paths = %q, want nil", got)
			}
			if len(*warnings) != 1 || !strings.Contains((*warnings)[0], config.RepoSkillsFileName) {
				t.Errorf("warnings = %q, want one that names %s", *warnings, config.RepoSkillsFileName)
			}
		})
	}
}

// A path that is not in the nix store, or that does not exist, is dropped
// with a warning. The other paths of the repo stay, in order.
func TestPaths_DropsPathsOutsideTheStore(t *testing.T) {
	l := projecttrusttest.New(t)
	store := reposkillstest.Store(t)
	first := reposkillstest.Skill(t, store, "first")
	last := reposkillstest.Skill(t, store, "last")
	wt := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
	inWorktree := filepath.Join(wt, "modules", "programs", "prism", "repo-skills", "site-theming")
	if err := os.MkdirAll(inWorktree, 0o700); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(store, "0000-escape")
	if err := os.Symlink(inWorktree, escape); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		inWorktree,
		filepath.Join(store, "0000-missing"),
		escape,
		store + "/../" + filepath.Base(store) + "/0000-first",
		"0000-first",
		store,
	}
	entries := append(append([]string{first}, bad...), last)
	reposkillstest.WriteFile(t, l.ConfigDir, map[string][]string{"nixos-config": entries})

	warn, warnings := collect()
	got := reposkills.Paths("nixos-config@main", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
	if want := []string{first, last}; !slices.Equal(got, want) {
		t.Errorf("Paths = %q, want %q", got, want)
	}
	if len(*warnings) != len(bad) {
		t.Fatalf("warnings = %q, want one for each of %q", *warnings, bad)
	}
	for i, p := range bad {
		if !strings.Contains((*warnings)[i], p) {
			t.Errorf("warning %d = %q, want it to name %s", i, (*warnings)[i], p)
		}
	}
}
