package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

const hostProfilesJSON = `{"default":"balanced","profiles":{
 "light":{"worker":{"provider":"anthropic","model":"anthropic/claude-sonnet-5-5","thinking":"low"}},
 "balanced":{"worker":{"model":"anthropic/claude-opus-5-5"}}}}`

func writeHostProfiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prism"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prism", "profiles.json"), []byte(hostProfilesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
}

// A host-mode command for a profiled session carries the profile slot.
func TestBuildDirectAgentCmd_HostModeProfileSlot(t *testing.T) {
	writeHostProfiles(t)
	cmd := buildDirectAgentCmd(Opts{Agent: "worker", HarnessName: "pi", ProfileName: "light", IsolationMode: "host"})
	for _, want := range []string{"--provider 'anthropic'", "--model 'anthropic/claude-sonnet-5-5'", "--thinking 'low'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in %q", want, cmd)
		}
	}
}

// Overrides beat the slot, as on bwrap.
func TestBuildDirectAgentCmd_HostModeOverridesBeatProfileSlot(t *testing.T) {
	writeHostProfiles(t)
	cmd := buildDirectAgentCmd(Opts{
		Agent: "worker", HarnessName: "pi", ProfileName: "light",
		Model: "m/session", Provider: "p2", ModelsByRole: map[string]string{"worker": "m/role"},
	})
	if !strings.Contains(cmd, "--model 'm/role'") || strings.Contains(cmd, "sonnet") {
		t.Errorf("per-role model should win: %q", cmd)
	}
	if !strings.Contains(cmd, "--provider 'p2'") || strings.Contains(cmd, "--provider 'anthropic'") {
		t.Errorf("provider override should win: %q", cmd)
	}
}

// Host and bwrap render the same flags from the same resolver.
func TestHostAndPIInvocationShareModelFlags(t *testing.T) {
	writeHostProfiles(t)
	pf, _ := config.LoadProfiles()
	slot, _ := config.SlotForRole(pf, "light", "worker")
	ov := container.PIOverrides{Model: "m/session", AgentModel: "m/role", Variant: "high"}
	axes := container.ResolvePIModelAxes(slot, ov)
	cfg := container.Config{PIProvider: axes.Provider, PIModel: axes.Model, PIThinking: axes.Thinking, PIBinaryPath: "pi"}
	want := strings.Join(container.PIInvocation(cfg)[1:7], " ")
	cmd := buildDirectAgentCmd(Opts{Agent: "worker", HarnessName: "pi", ProfileName: "light",
		Model: "m/session", Variant: "high", ModelsByRole: map[string]string{"worker": "m/role"}})
	got := strings.ReplaceAll(cmd, "'", "")
	if !strings.Contains(got, want) {
		t.Errorf("host %q lacks bwrap flags %q", got, want)
	}
}

// The agent-only layout (review fan-out) forwards ProfileName, so a host-mode
// reviewer runs on the spawn's profile slot.
func TestAgentOnlyLayout_HostModeProfileSlot(t *testing.T) {
	writeHostProfiles(t)
	o := buildOptsForAgentOnlyLayout(SpawnOpts{AgentRole: "worker", HarnessName: "pi", ProfileName: "light"}, 0, "host")
	if o.ProfileName != "light" {
		t.Fatalf("ProfileName not forwarded: %q", o.ProfileName)
	}
	cmd := buildDirectAgentCmd(o)
	if !strings.Contains(cmd, "--model 'anthropic/claude-sonnet-5-5'") {
		t.Errorf("agent-only host cmd lacks profile model: %q", cmd)
	}
}

// A host-mode launch with no resolvable model writes a warning that names the
// role to the agent-run log.
func TestBuildDirectAgentCmd_HostModeNoModelWarns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	buildDirectAgentCmd(Opts{Agent: "worker", HarnessName: "pi", SessionName: "prism-test@warn", IsolationMode: "host"})
	p, err := AgentRunLogPath("prism-test@warn")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `role "worker"`) || !strings.Contains(string(b), "default model") {
		t.Errorf("warning missing: %q", b)
	}
}
