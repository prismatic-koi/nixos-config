package stateguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setRoot(t *testing.T, root string) {
	t.Helper()
	orig := Root
	Root = root
	t.Cleanup(func() { Root = orig })
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestCheck_NoRootAllowsAnyEnvironment(t *testing.T) {
	setRoot(t, "")
	env := envOf(map[string]string{"HOME": "/Users/someone", "XDG_STATE_HOME": "/Users/someone/.local/state"})
	if err := Check(env); err != nil {
		t.Fatalf("Check with no Root = %v, want nil", err)
	}
}

func TestCheck_AllPathsInsideRoot(t *testing.T) {
	root := t.TempDir()
	setRoot(t, root)
	env := envOf(map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"TMUX":            filepath.Join(root, "tmux", "tmux-501", "s") + ",123,0",
	})
	if err := Check(env); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
}

func TestCheck_XDGFallsBackToHomeInsideRoot(t *testing.T) {
	root := t.TempDir()
	setRoot(t, root)
	env := envOf(map[string]string{
		"HOME":        filepath.Join(root, "home"),
		"TMUX_TMPDIR": filepath.Join(root, "tmux"),
	})
	if err := Check(env); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
}

func TestCheck_RefusesPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	inside := map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"TMUX_TMPDIR":     filepath.Join(root, "tmux"),
	}
	cases := []struct {
		name     string
		override map[string]string
		wantName string
	}{
		{"state dir reset by a shell startup file", map[string]string{"XDG_STATE_HOME": "/Users/someone/.local/state"}, "XDG_STATE_HOME"},
		{"config dir reset by a shell startup file", map[string]string{"XDG_CONFIG_HOME": "/Users/someone/.config"}, "XDG_CONFIG_HOME"},
		{"real home", map[string]string{"HOME": "/Users/someone"}, "HOME"},
		{"empty home", map[string]string{"HOME": ""}, "HOME"},
		{"tmux socket of another server", map[string]string{"TMUX": "/private/tmp/tmux-501/default,99,0"}, "tmux socket"},
		{"default tmux server", map[string]string{"TMUX_TMPDIR": ""}, "tmux socket"},
		{"sibling with the root as a name prefix", map[string]string{"XDG_STATE_HOME": root + "-other/state"}, "XDG_STATE_HOME"},
		{"relative path", map[string]string{"XDG_STATE_HOME": "state"}, "XDG_STATE_HOME"},
		{"parent traversal", map[string]string{"XDG_STATE_HOME": filepath.Join(root, "..", "state")}, "XDG_STATE_HOME"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRoot(t, root)
			env := map[string]string{}
			for k, v := range inside {
				env[k] = v
			}
			for k, v := range tc.override {
				env[k] = v
			}
			err := Check(envOf(env))
			if err == nil {
				t.Fatalf("Check = nil, want an error that names %s", tc.wantName)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("Check error = %q, want it to name %s", err, tc.wantName)
			}
		})
	}
}

func TestCheck_AcceptsResolvedFormOfRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	setRoot(t, link)
	env := envOf(map[string]string{
		"HOME": filepath.Join(link, "home"),
		"TMUX": filepath.Join(resolved, "tmux", "tmux-501", "s") + ",1,0",
	})
	if err := Check(env); err != nil {
		t.Fatalf("Check = %v, want nil when TMUX holds the resolved form of Root", err)
	}
}
