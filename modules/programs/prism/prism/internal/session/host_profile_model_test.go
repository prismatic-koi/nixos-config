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
