package cmd

// Tests for prism agent-context (issue #1498).
//
// Verifies:
//  1. TestAgentContextCoversAllCommands — every non-hidden top-level command
//     registered in the cobra tree has an entry in the JSON output.
//  2. TestAgentContextSchemaValidation — the emitted JSON unmarshals cleanly
//     into the typed AgentContextDocument struct so future flag additions
//     can't silently break the shape.
//  3. TestAgentContextIsolationFlagIsEnum — the spawn --isolation flag has
//     type "enum" and a non-empty values array.
//  4. TestAgentContextMissingProfilesFile — when profiles.json is absent,
//     available_profiles is [] (not null, not an error).
//  5. TestAgentContextPrecedenceKeys — the precedence map includes at least
//     the "profile" and "isolation" keys.
//  6. TestAgentContextHiddenExcluded — hidden commands are absent by default
//     and present with includeHidden=true.
//  7. TestAgentContextOutputOnStdout — the command writes exclusively to
//     stdout (not stderr) and exits 0.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// TestAgentContextCoversAllCommands asserts that every non-hidden top-level
// cobra command has a corresponding entry in the agent-context JSON output.
//
// This is the AC stated in the issue: "A test walks RootCmd.Commands() and
// asserts every non-hidden command has an entry."
func TestAgentContextCoversAllCommands(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext: %v", err)
		}
	})

	var doc AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	// Walk the actual cobra tree.
	for _, sub := range rootCmd.Commands() {
		if sub.Hidden {
			continue
		}
		if _, ok := doc.Commands[sub.Name()]; !ok {
			t.Errorf("command %q is registered in cobra but missing from agent-context JSON", sub.Name())
		}
	}
}

// TestAgentContextSchemaValidation verifies the emitted JSON unmarshals
// cleanly into the typed AgentContextDocument struct. This ensures future
// flag additions can't silently break the document shape by producing keys
// that don't round-trip through the struct.
func TestAgentContextSchemaValidation(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext: %v", err)
		}
	})

	var doc AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	// schema_version must be the string "1".
	if doc.SchemaVersion != agentContextSchemaVersion {
		t.Errorf("schema_version = %q, want %q", doc.SchemaVersion, agentContextSchemaVersion)
	}

	// commands must be a non-nil map.
	if doc.Commands == nil {
		t.Error("commands must be a non-nil map")
	}

	// available_profiles must be a non-nil slice (may be empty).
	if doc.AvailableProfiles == nil {
		t.Error("available_profiles must be a non-nil slice, not null")
	}

	// precedence must be a non-nil map.
	if doc.Precedence == nil {
		t.Error("precedence must be a non-nil map")
	}

	// Every command entry must have a non-empty description.
	for name, meta := range doc.Commands {
		if meta.Description == "" {
			t.Errorf("command %q has an empty description", name)
		}
		// Every flag must have a non-empty type.
		for flagName, fm := range meta.Flags {
			if fm.Type == "" {
				t.Errorf("command %q flag %q has empty type", name, flagName)
			}
			validTypes := map[string]bool{
				"bool": true, "string": true, "int": true,
				"duration": true, "stringArray": true, "enum": true,
			}
			if !validTypes[fm.Type] {
				t.Errorf("command %q flag %q has invalid type %q", name, flagName, fm.Type)
			}
			// enum flags must have a non-empty values array.
			if fm.Type == "enum" && len(fm.Values) == 0 {
				t.Errorf("command %q flag %q has type enum but empty values array", name, flagName)
			}
		}
	}
}

// TestAgentContextIsolationFlagIsEnum verifies that spawn's --isolation flag
// is documented as type "enum" with the exact values from config.ValidIsolationModes.
func TestAgentContextIsolationFlagIsEnum(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext: %v", err)
		}
	})

	var doc AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	spawnMeta, ok := doc.Commands["spawn"]
	if !ok {
		t.Fatal("spawn command missing from output")
	}

	isolationFlag, ok := spawnMeta.Flags["--isolation"]
	if !ok {
		t.Fatal("spawn --isolation flag missing from output")
	}

	if isolationFlag.Type != "enum" {
		t.Errorf("spawn --isolation type = %q, want %q", isolationFlag.Type, "enum")
	}

	if len(isolationFlag.Values) == 0 {
		t.Fatal("spawn --isolation values must not be empty")
	}

	// Verify the values exactly match config.ValidIsolationModes.
	want := make([]string, len(config.ValidIsolationModes))
	for i, m := range config.ValidIsolationModes {
		want[i] = string(m)
	}
	got := isolationFlag.Values
	if len(got) != len(want) {
		t.Errorf("spawn --isolation values: got %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("spawn --isolation values[%d]: got %q, want %q", i, got[i], want[i])
			}
		}
	}

	// The default_source should reference the config file.
	if isolationFlag.DefaultSource == "" {
		t.Error("spawn --isolation default_source should be set")
	}
}

// TestAgentContextMissingProfilesFile verifies that when profiles.json is
// absent, available_profiles is [] (an empty array, not null) and the
// command exits without error.
//
// We test this by pointing XDG_CONFIG_HOME at an empty temp directory so
// that profiles.json does not exist.
func TestAgentContextMissingProfilesFile(t *testing.T) {
	// Point XDG_CONFIG_HOME at an empty temp dir so profiles.json is absent.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext: %v", err)
		}
	})

	var doc AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	// available_profiles must be a non-nil empty array (not null).
	if doc.AvailableProfiles == nil {
		t.Error("available_profiles must be [] when profiles.json is missing, not null")
	}
	if len(doc.AvailableProfiles) != 0 {
		t.Errorf("available_profiles must be empty when profiles.json is missing, got %v", doc.AvailableProfiles)
	}
}

// TestAgentContextPrecedenceKeys verifies that the precedence map includes
// at least the "profile" and "isolation" keys, each with a non-empty ordered
// list of strings.
func TestAgentContextPrecedenceKeys(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext: %v", err)
		}
	})

	var doc AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	// "model" and "provider" were added by issue #2852: `prism agent-context`
	// must document that the CLI flag beats the profile slot on both axes.
	for _, key := range []string{"profile", "isolation", "model", "provider"} {
		chain, ok := doc.Precedence[key]
		if !ok {
			t.Errorf("precedence map missing required key %q", key)
			continue
		}
		if len(chain) == 0 {
			t.Errorf("precedence[%q] must not be empty", key)
		}
	}

	// The provider chain must state both rungs in order: the CLI flag wins
	// over the profile slot's provider.
	providerChain := doc.Precedence["provider"]
	if len(providerChain) < 2 {
		t.Fatalf("precedence[\"provider\"] = %v, want at least two rungs", providerChain)
	}
	if !strings.Contains(providerChain[0], "--provider") {
		t.Errorf("precedence[\"provider\"][0] = %q, want it to name the --provider flag", providerChain[0])
	}
	if !strings.Contains(providerChain[len(providerChain)-1], "profile") {
		t.Errorf("precedence[\"provider\"] lowest rung = %q, want it to name the profile slot",
			providerChain[len(providerChain)-1])
	}

	// The model chain has THREE rungs. --model-override names a single role
	// and beats --model for that role (cmd/spawn.go's flag description; the
	// agentRole lookup in cmd/sidecar.go). Publishing --model as the highest
	// rung is a false statement to every agent that reads agent-context, so
	// the top rung is pinned here.
	modelChain := doc.Precedence["model"]
	if len(modelChain) < 3 {
		t.Fatalf("precedence[\"model\"] = %v, want at least three rungs", modelChain)
	}
	if !strings.Contains(modelChain[0], "--model-override") {
		t.Errorf("precedence[\"model\"][0] = %q, want it to name --model-override (it outranks --model)",
			modelChain[0])
	}
	if !strings.Contains(modelChain[1], "--model") || strings.Contains(modelChain[1], "--model-override") {
		t.Errorf("precedence[\"model\"][1] = %q, want the plain --model flag", modelChain[1])
	}
	if !strings.Contains(modelChain[len(modelChain)-1], "profile") {
		t.Errorf("precedence[\"model\"] lowest rung = %q, want it to name the profile slot",
			modelChain[len(modelChain)-1])
	}
}

// TestAgentContextHiddenExcluded verifies that hidden commands are absent
// from the default output but present when the includeHidden flag would be set.
//
// monitor-review is the only command currently marked Hidden: true in cobra.
func TestAgentContextHiddenExcluded(t *testing.T) {
	// Find a hidden command in the cobra tree.
	var hiddenName string
	for _, sub := range rootCmd.Commands() {
		if sub.Hidden {
			hiddenName = sub.Name()
			break
		}
	}
	if hiddenName == "" {
		t.Skip("no hidden commands registered — skipping hidden-exclusion test")
	}

	// Default run: hidden command should be absent.
	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext (default): %v", err)
		}
	})

	var docDefault AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &docDefault); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if _, ok := docDefault.Commands[hiddenName]; ok {
		t.Errorf("hidden command %q present in default output (should be omitted)", hiddenName)
	}

	// Run with includeHidden: hidden command should be present.
	// We call buildAgentContextDocument directly to avoid flag-parsing overhead.
	docWithHidden := buildAgentContextDocument(true)
	if _, ok := docWithHidden.Commands[hiddenName]; !ok {
		t.Errorf("hidden command %q absent from output with includeHidden=true", hiddenName)
	}
}

// TestAgentContextFlagSubcommandScope verifies that flags declared on a
// subcommand appear under the subcommand's entry, not under the parent.
func TestAgentContextFlagSubcommandScope(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runAgentContext(agentContextCmd, nil); err != nil {
			t.Fatalf("runAgentContext: %v", err)
		}
	})

	var doc AgentContextDocument
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	// profile list --json should appear under profile.subcommands.list, not profile.
	profileMeta, ok := doc.Commands["profile"]
	if !ok {
		t.Fatal("profile command missing")
	}

	listMeta, ok := profileMeta.Subcommands["list"]
	if !ok {
		t.Fatal("profile list subcommand missing")
	}

	if _, ok := listMeta.Flags["--json"]; !ok {
		t.Error("profile list --json flag missing from subcommand entry")
	}

	// --json should NOT appear on the parent profile command.
	if _, ok := profileMeta.Flags["--json"]; ok {
		t.Error("--json flag should be on profile list, not on parent profile command")
	}
}
