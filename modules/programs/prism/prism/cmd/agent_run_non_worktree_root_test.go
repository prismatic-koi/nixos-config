package cmd

// agent_run_non_worktree_root_test.go — the profile slot that `prism
// agent-run` gives the root session of a non-worktree project (issue #3179).
//
// The start path clears root_agent_name for that session, so agent-run finds
// NULL and session.DefaultAgent returns "" for its directory. No profile
// defines a "" slot. Without profileSlotRole, populatePIConfig refuses the
// session and the agent pane of `obsidian` stops in bwrap and sandbox-exec
// mode. The session must read the coordinator slot and still start pi with
// no --agent flag.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

const (
	nwrCoordinatorModel = "anthropic/claude-opus-4-7"
	nwrWorkerModel      = "anthropic/claude-sonnet-4-6"
)

// writeCoordinatorAndWorkerProfiles writes a profile "test" with a
// coordinator slot and a worker slot that use different models, so the test
// can tell which slot agent-run read.
func writeCoordinatorAndWorkerProfiles(t *testing.T, configHome string) {
	t.Helper()
	dir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir prism config dir: %v", err)
	}
	pf := config.ProfilesFile{
		Default: "test",
		Profiles: map[string]config.ProfileEntry{
			"test": {
				"coordinator": config.RoleSlot{Provider: "anthropic", Model: nwrCoordinatorModel, Thinking: "medium"},
				"worker":      config.RoleSlot{Provider: "anthropic", Model: nwrWorkerModel, Thinking: "low"},
			},
		},
	}
	b, err := json.Marshal(&pf)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles.json"), b, 0o600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
}

func TestAgentRun_NonWorktreeRootReadsCoordinatorSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeCoordinatorAndWorkerProfiles(t, configHome)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	cases := []struct {
		name      string
		session   string
		role      string // root_agent_name; "" = NULL
		worktree  string
		wantModel string
		wantAgent string // "" = no --agent flag
	}{
		{"root session of a non-worktree project", nwrCLIRoot, "", t.TempDir(), nwrCoordinatorModel, ""},
		{"worker keeps its own slot and flag", "prism-test-nwcli-other@feature", "worker", nwrCLIWorktree(t), nwrWorkerModel, "worker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := d.UpsertStatusSeedRootAgentName(tc.session, "x", tc.worktree, "idle", nil, nil, tc.role, "", "bwrap"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			status, err := d.CurrentStatus(tc.session)
			if err != nil || status == nil {
				t.Fatalf("CurrentStatus: status=%v err=%v", status, err)
			}

			role := agentRunRole(status, status.Worktree)
			ctrCfg := container.Config{AgentRole: role}
			cfg := config.Config{PIExtensionDir: "/nix/store/fake-ext"}
			if err := populatePIConfig(&ctrCfg, tc.session, role, cfg, piOverrides{}); err != nil {
				t.Fatalf("populatePIConfig(role %q): %v — agent-run cannot start this session", role, err)
			}

			if ctrCfg.PIModel != tc.wantModel {
				t.Errorf("PIModel = %q, want %q", ctrCfg.PIModel, tc.wantModel)
			}
			if ctrCfg.AgentRole != tc.wantAgent {
				t.Errorf("Config.AgentRole = %q, want %q", ctrCfg.AgentRole, tc.wantAgent)
			}
			args := container.PIInvocation(ctrCfg)
			hasAgent := slices.Contains(args, "--agent")
			if tc.wantAgent == "" && hasAgent {
				t.Errorf("pi argv has --agent for a role-less root session: %v", args)
			}
			if tc.wantAgent != "" && !argvHasPair(args, "--agent", tc.wantAgent) {
				t.Errorf("pi argv lacks --agent %s: %v", tc.wantAgent, args)
			}
		})
	}
}
