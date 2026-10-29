package cmd

// agent_run_shared_state_test.go — prism agent-run (bwrap and sandbox-exec)
// makes the shared lock dir and the Notion store dir before pi starts, moves
// the Notion store into its dir, and records the paths for the isolators
// (#3289, #3290). It records the Notion store dir only for a session in
// NOTION_MCP_REPOS (#3319).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

// writeProfilesWithEnv writes a profiles.json under configHome/prism/ with
// a slot for each role that the tests below start, and env as
// agent_env_vars.
func writeProfilesWithEnv(t *testing.T, configHome string, env map[string]string) {
	t.Helper()
	slot := config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-opus-4-7", Thinking: "medium"}
	pf := config.ProfilesFile{
		Default:      "test",
		AgentEnvVars: env,
		Profiles: map[string]config.ProfileEntry{
			"test": {"coordinator": slot, "worker": slot, "investigate": slot, "review-code": slot},
		},
	}
	b, err := json.Marshal(&pf)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(configHome, "prism"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "prism", "profiles.json"), b, 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
}

// A session that is not in NOTION_MCP_REPOS gets the lock dir and no Notion
// store dir (#3319). prism still makes the store dir and moves the old store
// out of the agent dir.
func TestPopulatePIConfig_SetsSharedStateDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	home := os.Getenv("HOME")
	agentDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const tokens = `{"accessToken":"a","refreshToken":"r","expiresAt":1,"clientId":"c"}`
	if err := os.WriteFile(filepath.Join(agentDir, "notion-mcp-oauth.json"), []byte(tokens), 0o600); err != nil {
		t.Fatal(err)
	}

	ctrCfg := container.Config{Worktree: filepath.Join(t.TempDir(), "wt")}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", config.Config{PIExtensionDir: "/nix/store/fake-ext"}, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	wantLock, wantNotion := container.PISharedStateDirs(home)
	if ctrCfg.PILockHostDir != wantLock || ctrCfg.PINotionHostDir != "" {
		t.Fatalf("shared dirs = %q, %q, want %q, \"\"", ctrCfg.PILockHostDir, ctrCfg.PINotionHostDir, wantLock)
	}
	for _, d := range []string{wantLock, wantNotion} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s = (%v, %v), want a dir", d, fi, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(wantNotion, "notion-mcp-oauth.json")); err != nil || string(got) != tokens {
		t.Errorf("Notion token file in the store dir = (%q, %v), want the moved file", got, err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("Notion token file still in the agent dir: %v", err)
	}
}

// populatePIConfig records the Notion store dir only for a session whose
// repo identity on the host is in NOTION_MCP_REPOS (#3319). A review agent
// and an investigator of such a session get the store too. A name alone, a
// directory alone, and a session outside the list do not.
func TestPopulatePIConfig_NotionStoreOnlyForNotionRepos(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	isolateForPopulatePIConfig(t)
	l := projecttrusttest.New(t)
	nixos := projecttrusttest.Worktree(t, l.Code, "nixos-config", "feat")
	notionWorktree := projecttrusttest.Worktree(t, l.Code, "notes", "main")
	forged := filepath.Join(l.Writable, "obsidian")
	if err := os.MkdirAll(forged, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		PIExtensionDir:   "/nix/store/fake-ext",
		ProjectLocations: []string{l.Code},
		ProjectSpecific:  []string{l.Obsidian},
	}
	_, wantNotion := container.PISharedStateDirs(os.Getenv("HOME"))
	repos := l.Obsidian + ":" + filepath.Join(l.Code, "notes") + ":" + forged

	cases := []struct {
		name    string
		repos   string
		session string
		role    string
		dir     string
		want    bool
	}{
		{"obsidian root", repos, "obsidian", "", l.Obsidian, true},
		{"obsidian investigator", repos, "obsidian~investigate-x", "investigate", l.Obsidian, true},
		{"obsidian review agent", repos, "obsidian~review-1-review-code", "review-code", l.Obsidian, true},
		{"worktree below an entry", repos, "notes@main", "worker", notionWorktree, true},

		{"repo not in the list", repos, "nixos-config@feat", "worker", nixos, false},
		{"other repo name in a listed dir", repos, "nixos-config", "", l.Obsidian, false},
		{"listed dir that is not a project root", repos, "obsidian", "", forged, false},
		{"listed name in an unlisted worktree", repos, "obsidian", "", nixos, false},
		{"no NOTION_MCP_REPOS", "", "obsidian", "", l.Obsidian, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.repos != "" {
				env[container.PINotionReposEnvVar] = tc.repos
			}
			writeProfilesWithEnv(t, l.ConfigDir, env)
			ctrCfg := container.Config{Worktree: tc.dir}
			if err := populatePIConfig(&ctrCfg, tc.session, tc.role, cfg, piOverrides{}); err != nil {
				t.Fatalf("populatePIConfig: %v", err)
			}
			want := ""
			if tc.want {
				want = wantNotion
			}
			if ctrCfg.PINotionHostDir != want {
				t.Errorf("PINotionHostDir = %q, want %q", ctrCfg.PINotionHostDir, want)
			}
			if ctrCfg.PILockHostDir == "" {
				t.Error("PILockHostDir is empty, want the lock dir for every session")
			}
		})
	}
}
