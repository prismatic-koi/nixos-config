package cmd

// Tests for the centralised spawn_inputs writer introduced in issue #2087.
//
// SpawnSession is the single chokepoint that writes spawn_inputs (see
// internal/session/spawn.go). The writer builds the row from
// session.SpawnOpts fields via session.SpawnInputsFromOpts. These tests
// exercise that mapping and the end-to-end insert path against an isolated
// DB so the schema, flag→column mapping, and abtest-pair propagation all
// land correctly without spinning up tmux / sidecar / a real agent.
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir() and
//     sets the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard, so no host DB / bus /
//     tmux state is touched.
//   - Session names use the "prism-test@" prefix.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedSessionForSpawnInputs inserts a minimal sessions row for the given instance + name so
// the spawn_inputs FK (spawn_inputs.instance_id → sessions.instance_id) is
// satisfied. Mirrors what SpawnSession does host-side before writing the
// audit row (#1507).
func seedSessionForSpawnInputs(t *testing.T, d *db.DB, instanceID, sessionName string) {
	t.Helper()
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "test-repo",
		Worktree:    "/tmp/worktree",
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("seedSessionForSpawnInputs: InsertSession: %v", err)
	}
}

// TestSpawnInputsFromOpts_FullFlagSet verifies that every flag-mirror field
// on session.SpawnOpts lands in the corresponding spawn_inputs column when
// the row is written via InsertSpawnInputs and read back via
// SpawnInputsByInstanceID.
//
// This is the core flag→column mapping test required by issue #2087:
// after a `prism spawn` with all flags set, the audit row reflects the
// invocation faithfully.
func TestSpawnInputsFromOpts_FullFlagSet(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-full-flags"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	modelsByRole := map[string]string{
		"coordinator":    "anthropic/claude-opus-4",
		"review-context": "google/gemini-2.5-pro",
	}
	opts := session.SpawnOpts{
		InstanceID:           instanceID,
		Prompt:               "do the mahi",
		PromptSource:         "cli-positional",
		PromptTemplateHash:   "tmpl-sha-abc",
		ProfileName:          "anthropic",
		ModelFlag:            "anthropic/claude-opus-4-8",
		VariantFlag:          "high",
		ProviderFlag:         "openrouter",
		AgentFlag:            "worker",
		HarnessFlag:          "pi",
		IsolationFlag:        "bwrap",
		HostModeFlag:         true,
		PRNumber:             1234,
		BranchFlag:           "feature/x",
		IgnoreConcurrencyCap: true,
		ModelsByRole:         modelsByRole,
		SkillsManifestHash:   "skills-sha-xyz",
		AgentPromptHash:      "agent-sha-def",
		AbtestPairID:         "abtest-pair-uuid-0001",
	}

	si := session.SpawnInputsFromOpts(opts)
	if si.InstanceID != instanceID {
		t.Errorf("InstanceID: got %q, want %q", si.InstanceID, instanceID)
	}
	if si.CreatedAt == 0 {
		t.Error("CreatedAt: got 0, want non-zero (mirrors sessions.started_at)")
	}
	// CreatedAt should be within a sensible window of now — guards against
	// time.Now() being read at the wrong point in the helper.
	if delta := time.Now().UnixMilli() - si.CreatedAt; delta < 0 || delta > 5_000 {
		t.Errorf("CreatedAt: %d ms away from now, want within 5s", delta)
	}

	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil, want non-nil row")
	}

	assertStringPtr(t, "ProfileName", got.ProfileName, "anthropic")
	assertStringPtr(t, "ModelFlag", got.ModelFlag, "anthropic/claude-opus-4-8")
	assertStringPtr(t, "VariantFlag", got.VariantFlag, "high")
	// provider_flag round trip (issue #2852).
	assertStringPtr(t, "ProviderFlag", got.ProviderFlag, "openrouter")
	assertStringPtr(t, "AgentFlag", got.AgentFlag, "worker")
	assertStringPtr(t, "HarnessFlag", got.HarnessFlag, "pi")
	assertStringPtr(t, "IsolationFlag", got.IsolationFlag, "bwrap")
	if !got.HostModeFlag {
		t.Error("HostModeFlag: got false, want true")
	}
	if got.PRNumber == nil || *got.PRNumber != 1234 {
		t.Errorf("PRNumber: got %v, want 1234", got.PRNumber)
	}
	assertStringPtr(t, "BranchFlag", got.BranchFlag, "feature/x")
	if !got.IgnoreConcurrencyCap {
		t.Error("IgnoreConcurrencyCap: got false, want true")
	}
	assertStringPtr(t, "SkillsManifestHash", got.SkillsManifestHash, "skills-sha-xyz")
	assertStringPtr(t, "PromptTemplateHash", got.PromptTemplateHash, "tmpl-sha-abc")
	assertStringPtr(t, "AgentPromptHash", got.AgentPromptHash, "agent-sha-def")
	assertStringPtr(t, "PromptText", got.PromptText, "do the mahi")
	assertStringPtr(t, "PromptSource", got.PromptSource, "cli-positional")
	assertStringPtr(t, "AbtestPairID", got.AbtestPairID, "abtest-pair-uuid-0001")

	// ModelVariantOverrides is the JSON encoding of ModelsByRole.
	if got.ModelVariantOverrides == nil {
		t.Fatal("ModelVariantOverrides: got nil, want JSON of ModelsByRole")
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(*got.ModelVariantOverrides), &decoded); err != nil {
		t.Fatalf("ModelVariantOverrides: unmarshal: %v (raw: %q)", err, *got.ModelVariantOverrides)
	}
	if len(decoded) != len(modelsByRole) {
		t.Errorf("ModelVariantOverrides: got %d entries, want %d", len(decoded), len(modelsByRole))
	}
	for role, model := range modelsByRole {
		if decoded[role] != model {
			t.Errorf("ModelVariantOverrides[%q]: got %q, want %q", role, decoded[role], model)
		}
	}
}

// TestSpawnInputsFromOpts_EmptyFlagsRoundTripAsNull verifies that unset
// flag fields land as NULL (nil pointer) in the DB row — the
// "minimum-viable spawn" case for `prism spawn` with only a prompt.
//
// This case matters because `prism stats --group-by model` queries
// IS NOT NULL on model_flag; if an unset flag round-tripped as the empty
// string instead of NULL, the group-by would mis-classify rows.
func TestSpawnInputsFromOpts_EmptyFlagsRoundTripAsNull(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-empty-flags"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	// Only the bare minimum: instance id + a prompt. Everything else is
	// the zero value of SpawnOpts.
	opts := session.SpawnOpts{
		InstanceID:   instanceID,
		Prompt:       "minimal prompt",
		PromptSource: "cli-positional",
	}
	si := session.SpawnInputsFromOpts(opts)
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil, want non-nil row")
	}

	// Unset flag-mirror fields must be nil so downstream IS NULL queries
	// classify them correctly.
	for name, field := range map[string]*string{
		"ProfileName":           got.ProfileName,
		"ModelFlag":             got.ModelFlag,
		"VariantFlag":           got.VariantFlag,
		"ProviderFlag":          got.ProviderFlag,
		"AgentFlag":             got.AgentFlag,
		"HarnessFlag":           got.HarnessFlag,
		"IsolationFlag":         got.IsolationFlag,
		"BranchFlag":            got.BranchFlag,
		"SkillsManifestHash":    got.SkillsManifestHash,
		"PromptTemplateHash":    got.PromptTemplateHash,
		"AgentPromptHash":       got.AgentPromptHash,
		"AbtestPairID":          got.AbtestPairID,
		"ModelVariantOverrides": got.ModelVariantOverrides,
	} {
		if field != nil {
			t.Errorf("%s: got %q, want nil (flag was not passed)", name, *field)
		}
	}
	if got.PRNumber != nil {
		t.Errorf("PRNumber: got %d, want nil (flag was not passed)", *got.PRNumber)
	}
	if got.HostModeFlag {
		t.Error("HostModeFlag: got true, want false (default)")
	}
	if got.IgnoreConcurrencyCap {
		t.Error("IgnoreConcurrencyCap: got true, want false (default)")
	}

	// Prompt-related fields that were set must survive the round trip.
	assertStringPtr(t, "PromptText", got.PromptText, "minimal prompt")
	assertStringPtr(t, "PromptSource", got.PromptSource, "cli-positional")
}

// TestSpawnInputsFromOpts_AbtestPair verifies that both legs of an --abtest
// pair land with the same abtest_pair_id, matching the front-door contract
// (cmd/spawn.go's runAbtestSpawn mints one pairID and passes it to both
// spawnOneAbtest invocations). This is the writer-level contract: given the
// same SpawnOpts.AbtestPairID on two SpawnOpts, the two rows share the value.
func TestSpawnInputsFromOpts_AbtestPair(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	pairID := uuid.New().String()

	for _, leg := range []struct{ name, profile string }{
		{"prism-test@worker-abtest-a", "profileA"},
		{"prism-test@worker-abtest-b", "profileB"},
	} {
		instanceID := uuid.New().String()
		seedSessionForSpawnInputs(t, d, instanceID, leg.name)
		si := session.SpawnInputsFromOpts(session.SpawnOpts{
			InstanceID:   instanceID,
			Prompt:       "abtest leg prompt",
			PromptSource: "cli-positional",
			ProfileName:  leg.profile,
			AbtestPairID: pairID,
		})
		if err := d.InsertSpawnInputs(si); err != nil {
			t.Fatalf("InsertSpawnInputs leg %q: %v", leg.name, err)
		}
		got, err := d.SpawnInputsByInstanceID(instanceID)
		if err != nil {
			t.Fatalf("SpawnInputsByInstanceID leg %q: %v", leg.name, err)
		}
		if got == nil {
			t.Fatalf("leg %q: got nil row", leg.name)
		}
		assertStringPtr(t, "AbtestPairID/"+leg.name, got.AbtestPairID, pairID)
		assertStringPtr(t, "ProfileName/"+leg.name, got.ProfileName, leg.profile)
	}
}

// TestSpawnInputsFromOpts_ModelOverrideJSON verifies the JSON shape written
// to model_variant_overrides for --model-override flag repeats. Downstream
// readers parse this back into a map, so the round trip must be lossless.
func TestSpawnInputsFromOpts_ModelOverrideJSON(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-model-override"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	overrides := map[string]string{
		"review-context": "google/gemini-2.5-pro",
		"review-code":    "anthropic/claude-sonnet-4-7",
	}
	si := session.SpawnInputsFromOpts(session.SpawnOpts{
		InstanceID:   instanceID,
		Prompt:       "with overrides",
		PromptSource: "cli-positional",
		ModelsByRole: overrides,
	})
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil || got.ModelVariantOverrides == nil {
		t.Fatalf("ModelVariantOverrides: got nil row or nil column, want JSON")
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(*got.ModelVariantOverrides), &decoded); err != nil {
		t.Fatalf("decode model_variant_overrides JSON: %v (raw: %q)", err, *got.ModelVariantOverrides)
	}
	for role, model := range overrides {
		if decoded[role] != model {
			t.Errorf("ModelVariantOverrides[%q]: got %q, want %q", role, decoded[role], model)
		}
	}
}

// TestSpawnInputsFromOpts_InvestigateMinimalRow verifies that the minimum
// audit row required by the issue ACs lands for an investigate-style spawn:
// instance_id + created_at + agent role/flag. cmd/investigate.go sets
// SpawnOpts.AgentFlag = "investigate" so the row classifies under
// `prism stats --group-by agent` alongside spawn / pr rows.
func TestSpawnInputsFromOpts_InvestigateMinimalRow(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@coord~investigate-slug"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	si := session.SpawnInputsFromOpts(session.SpawnOpts{
		InstanceID:   instanceID,
		Prompt:       "investigation prompt",
		PromptSource: "cli-positional",
		AgentFlag:    "investigate",
		HarnessFlag:  "pi",
	})
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want non-nil row")
	}
	if got.InstanceID != instanceID {
		t.Errorf("InstanceID: got %q, want %q", got.InstanceID, instanceID)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt: got 0, want non-zero")
	}
	assertStringPtr(t, "AgentFlag", got.AgentFlag, "investigate")
	assertStringPtr(t, "HarnessFlag", got.HarnessFlag, "pi")
}

// TestSpawnInputsFromOpts_AbtestPairCarriesRendererFields is the issue #2102
// Layer 2 AC at the writer level: every --abtest leg must persist the columns
// that `prism stats compare`'s Spawn Inputs block actually reads —
// profile_name, harness_flag, isolation_flag, agent_flag — with non-empty
// values. Without these, the renderer collapses each leg to "—" and the
// A/B-test merge-decision workflow loses its leg-discriminating data.
func TestSpawnInputsFromOpts_AbtestPairCarriesRendererFields(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	pairID := uuid.New().String()
	legs := []struct {
		name      string
		profile   string
		isolation string
		agent     string
	}{
		{"prism-test@worker-abtest-renderer-a", "anthropic", "bwrap", "worker"},
		{"prism-test@worker-abtest-renderer-b", "google-gemini", "bwrap", "worker"},
	}
	for _, leg := range legs {
		iid := uuid.New().String()
		seedSessionForSpawnInputs(t, d, iid, leg.name)
		si := session.SpawnInputsFromOpts(session.SpawnOpts{
			InstanceID:    iid,
			Prompt:        "abtest leg prompt",
			PromptSource:  "cli-positional",
			ProfileName:   leg.profile,
			HarnessFlag:   "pi",
			IsolationFlag: leg.isolation,
			AgentFlag:     leg.agent,
			AbtestPairID:  pairID,
		})
		if err := d.InsertSpawnInputs(si); err != nil {
			t.Fatalf("InsertSpawnInputs leg %q: %v", leg.name, err)
		}
		got, err := d.SpawnInputsByInstanceID(iid)
		if err != nil {
			t.Fatalf("SpawnInputsByInstanceID leg %q: %v", leg.name, err)
		}
		if got == nil {
			t.Fatalf("leg %q: got nil row", leg.name)
		}
		// Every column the `prism stats compare` Spawn Inputs renderer reads
		// must be populated. Missing fields would collapse the leg to "—".
		assertStringPtr(t, "ProfileName/"+leg.name, got.ProfileName, leg.profile)
		assertStringPtr(t, "HarnessFlag/"+leg.name, got.HarnessFlag, "pi")
		assertStringPtr(t, "IsolationFlag/"+leg.name, got.IsolationFlag, leg.isolation)
		assertStringPtr(t, "AgentFlag/"+leg.name, got.AgentFlag, leg.agent)
		assertStringPtr(t, "AbtestPairID/"+leg.name, got.AbtestPairID, pairID)
	}
}

// TestSpawnInputsFromOpts_IsolationModeDefaultsWhenFlagOmitted is the
// issue #2105 writer-level AC. When --isolation is omitted (the common
// case where the user relies on the resolved default), the writer must
// still populate spawn_inputs.isolation_mode with the resolved effective
// mode the session is about to run under. isolation_flag stays NULL
// because the raw flag was not passed — that is the audit trail.
//
// Without this, `prism stats compare`'s Spawn Inputs block shows "—"
// for the isolation_mode axis on nearly every session (every session
// where the user did not pass --isolation), defeating the
// A/B-comparison workflow's ability to confirm both legs ran under the
// same sandbox shape.
func TestSpawnInputsFromOpts_IsolationModeDefaultsWhenFlagOmitted(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-iso-mode-default"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	// SpawnOpts mimics the shape `prism spawn` builds when the user runs
	// `prism spawn "do the mahi"` without --isolation: IsolationFlag is
	// empty (no raw flag), IsolationMode holds the resolved effective
	// mode the resolver picked (here "sandbox-exec" for the test host).
	const resolvedMode = "sandbox-exec"
	opts := session.SpawnOpts{
		InstanceID:    instanceID,
		Prompt:        "do the mahi",
		PromptSource:  "cli-positional",
		IsolationMode: resolvedMode,
		// IsolationFlag deliberately empty — the user did not pass --isolation.
	}

	si := session.SpawnInputsFromOpts(opts)
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil row, want non-nil")
	}

	// isolation_mode must carry the resolved effective mode — the bug
	// fix in #2105 hinges on this not being NULL.
	assertStringPtr(t, "IsolationMode", got.IsolationMode, resolvedMode)

	// isolation_flag must stay NULL because no raw --isolation flag was
	// passed. This is the audit trail: it records that the user relied
	// on the default rather than naming a specific mode.
	if got.IsolationFlag != nil {
		t.Errorf("IsolationFlag: got %q, want nil (raw flag was not passed)",
			*got.IsolationFlag)
	}
}

// TestSpawnInputsFromOpts_IsolationModeMatchesFlagWhenPassed verifies that
// when --isolation is explicitly passed, BOTH spawn_inputs.isolation_mode
// (the resolved effective mode) and spawn_inputs.isolation_flag (the raw
// flag value as audit trail) are populated with that value. The two
// columns serve different roles — mode is what the renderer reads, flag
// is what the audit trail preserves — but in this case they agree because
// the explicit flag IS the resolved mode.
func TestSpawnInputsFromOpts_IsolationModeMatchesFlagWhenPassed(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-iso-mode-explicit"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	const mode = "bwrap"
	opts := session.SpawnOpts{
		InstanceID:    instanceID,
		Prompt:        "do the mahi",
		PromptSource:  "cli-positional",
		IsolationFlag: mode, // user passed --isolation bwrap
		IsolationMode: mode, // resolver agrees with the flag
	}

	si := session.SpawnInputsFromOpts(opts)
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil row, want non-nil")
	}

	assertStringPtr(t, "IsolationMode", got.IsolationMode, mode)
	assertStringPtr(t, "IsolationFlag", got.IsolationFlag, mode)
}

// assertStringPtr fails the test if got is nil or *got != want. Used to keep
// the per-field assertions in the tests above concise.
func assertStringPtr(t *testing.T, name string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: got nil, want %q", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s: got %q, want %q", name, *got, want)
	}
}
