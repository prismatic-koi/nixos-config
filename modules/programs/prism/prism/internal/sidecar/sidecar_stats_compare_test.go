package sidecar

// Tests for the host-API GET /stats compare/abtest/abtest_list views
// added in #2098. These mirror the shape of sidecar_stats_test.go
// (existing view=summary / view=detail / view=doomloops coverage).
//
// Each test seeds rows directly into the test DB, invokes the
// hostAPIHandler() through the existing doHostAPI helper, and asserts
// either the response shape (StatsCompareResponseWire / StatsAbtestList
// ResponseWire) or the HTTP status code (404 / 400 / 409) for the
// negative paths.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedSidecarStatsSession inserts the minimum rows needed to make a
// session resolvable by view=compare/abtest. It mirrors
// cmd/stats_compare_test.go::seedCompareSession but stays inside the
// sidecar package (which cannot import cmd).
func seedSidecarStatsSession(t *testing.T, d *db.DB, sessionName string, startedAt time.Time, endState string) (instanceID string) {
	t.Helper()
	instanceID = uuid.New().String()
	state := "finished"
	if endState != "" {
		state = endState
	}
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, state, nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	if endState != "" {
		if err := d.UpdateSessionEnded(instanceID, endState); err != nil {
			t.Fatalf("UpdateSessionEnded %q: %v", sessionName, err)
		}
	}
	return instanceID
}

// writeSidecarAssistantTurn writes a msg_assistant event tied to instanceID.
func writeSidecarAssistantTurn(t *testing.T, d *db.DB, sessionName, instanceID string, ts time.Time, inputTokens, outputTokens int) {
	t.Helper()
	payload := fmt.Sprintf(`{"messageId":%q,"text":"reply","agent":"pi","model":"anthropic/claude-sonnet-4-6","inputTokens":%d,"outputTokens":%d,"durationMs":5000}`,
		uuid.New().String(), inputTokens, outputTokens)
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &instanceID,
		Type:        "msg_assistant",
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// ── view=compare ──────────────────────────────────────────────────────────────

// TestHostAPI_Stats_Compare_HappyPath verifies the multi-id resolution
// returns one StatsCompareRunWire per id, in input order, with labels
// run-A and run-B.
func TestHostAPI_Stats_Compare_HappyPath(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

	iidA := seedSidecarStatsSession(t, d, "repo@compare-a", startedAt, "finished")
	iidB := seedSidecarStatsSession(t, d, "repo@compare-b", startedAt.Add(time.Second), "finished")

	writeSidecarAssistantTurn(t, d, "repo@compare-a", iidA, startedAt.Add(10*time.Second), 1500, 700)
	writeSidecarAssistantTurn(t, d, "repo@compare-b", iidB, startedAt.Add(20*time.Second), 2000, 900)

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	url := fmt.Sprintf("/stats?view=compare&ids=%s,%s", iidA, iidB)
	rr := doHostAPI(t, sc, http.MethodGet, url, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsCompareResponseWire
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
	if resp.Runs[0].Label != "run-A" || resp.Runs[1].Label != "run-B" {
		t.Errorf("labels = (%q, %q), want (run-A, run-B)", resp.Runs[0].Label, resp.Runs[1].Label)
	}
	if resp.Runs[0].Session == nil || resp.Runs[0].Session.InstanceID != iidA {
		t.Errorf("run-A session id mismatch: got %+v", resp.Runs[0].Session)
	}
	if resp.Runs[1].Session == nil || resp.Runs[1].Session.InstanceID != iidB {
		t.Errorf("run-B session id mismatch: got %+v", resp.Runs[1].Session)
	}
}

// TestHostAPI_Stats_Compare_TerminalFallbackToComputeSpawnOutcome verifies
// that a terminal session with no persisted spawn_outcome row still gets
// aggregates surfaced via on-the-fly ComputeSpawnOutcome. This is the
// server-side analogue of cmd/stats_compare_test.go's Layer-1 coverage —
// without this branch, proxy users would see "—" cells for sessions in
// the gap between terminal-state and `prism cleanup`.
func TestHostAPI_Stats_Compare_TerminalFallbackToComputeSpawnOutcome(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

	iidA := seedSidecarStatsSession(t, d, "repo@terminal-a", startedAt, "finished")
	iidB := seedSidecarStatsSession(t, d, "repo@terminal-b", startedAt.Add(time.Second), "finished")
	writeSidecarAssistantTurn(t, d, "repo@terminal-a", iidA, startedAt.Add(10*time.Second), 1500, 700)
	writeSidecarAssistantTurn(t, d, "repo@terminal-b", iidB, startedAt.Add(20*time.Second), 2000, 900)
	// Note: NO WriteSpawnOutcome call — outcomes must be computed on the fly.

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	url := fmt.Sprintf("/stats?view=compare&ids=%s,%s", iidA, iidB)
	rr := doHostAPI(t, sc, http.MethodGet, url, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsCompareResponseWire
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
	if resp.Runs[0].Outcome == nil || resp.Runs[0].Outcome.TokensInputTotal != 1500 {
		t.Errorf("run-A outcome missing or wrong tokens_input: %+v", resp.Runs[0].Outcome)
	}
	if resp.Runs[1].Outcome == nil || resp.Runs[1].Outcome.TokensInputTotal != 2000 {
		t.Errorf("run-B outcome missing or wrong tokens_input: %+v", resp.Runs[1].Outcome)
	}
}

// TestHostAPI_Stats_Compare_LiveSessionOmitsOutcome verifies that a live
// (non-terminal) session has Outcome == nil in the response — matching the
// cmd-side renderer's "—" cells. This is the over-broad-fix guard for the
// "terminal-only compute" branch.
func TestHostAPI_Stats_Compare_LiveSessionOmitsOutcome(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

	// Seed one terminal session and one live session.
	iidA := seedSidecarStatsSession(t, d, "repo@terminal-a", startedAt, "finished")
	iidB := seedSidecarStatsSession(t, d, "repo@live-b", startedAt.Add(time.Second), "" /* no end state */)
	writeSidecarAssistantTurn(t, d, "repo@terminal-a", iidA, startedAt.Add(10*time.Second), 1500, 700)
	writeSidecarAssistantTurn(t, d, "repo@live-b", iidB, startedAt.Add(20*time.Second), 2000, 900)
	// Reset the live session's status row to a non-terminal state so the
	// terminal gate returns false.
	if err := d.UpsertStatus("repo@live-b", "repo", "/wt/repo@live-b", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus active: %v", err)
	}

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	url := fmt.Sprintf("/stats?view=compare&ids=%s,%s", iidA, iidB)
	rr := doHostAPI(t, sc, http.MethodGet, url, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsCompareResponseWire
	decodeJSONBody(t, rr, &resp)
	if resp.Runs[0].Outcome == nil {
		t.Errorf("terminal session A should have outcome populated, got nil")
	}
	if resp.Runs[1].Outcome != nil {
		t.Errorf("live session B should have outcome == nil; got %+v", resp.Runs[1].Outcome)
	}
}

// TestHostAPI_Stats_Compare_MissingIdReturns404 verifies that an unresolvable
// id surfaces as HTTP 404 with the "instance %q not found" message — the same
// shape the cmd-side resolveSessionArg emits on the direct-DB path.
func TestHostAPI_Stats_Compare_MissingIdReturns404(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	iidA := seedSidecarStatsSession(t, d, "repo@compare-a", startedAt, "finished")

	ghost := "aaaaaaaa-1111-2222-3333-444444444444"
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	url := fmt.Sprintf("/stats?view=compare&ids=%s,%s", iidA, ghost)
	rr := doHostAPI(t, sc, http.MethodGet, url, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ghost) {
		t.Errorf("404 body does not mention %q: %s", ghost, rr.Body.String())
	}
}

// TestHostAPI_Stats_Compare_RequiresIds verifies the empty-ids 400.
func TestHostAPI_Stats_Compare_RequiresIds(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Compare_RequiresAtLeastTwoIds guards against a single-id
// proxy call slipping through (the comparison engine needs >= 2 runs).
func TestHostAPI_Stats_Compare_RequiresAtLeastTwoIds(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	iid := seedSidecarStatsSession(t, d, "repo@compare-a", startedAt, "finished")

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare&ids="+iid, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Compare_ByName verifies that ids can also be raw
// session_names (not just instance IDs), matching the cmd-side
// resolveSessionArg precedence.
func TestHostAPI_Stats_Compare_ByName(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	_ = seedSidecarStatsSession(t, d, "repo@by-name-a", startedAt, "finished")
	_ = seedSidecarStatsSession(t, d, "repo@by-name-b", startedAt.Add(time.Second), "finished")

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare&ids=repo@by-name-a,repo@by-name-b", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsCompareResponseWire
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
}

// ── view=abtest ───────────────────────────────────────────────────────────────

// TestHostAPI_Stats_Abtest_HappyPath verifies that GET /stats?view=abtest
// resolves group members, sorts by session_name, and returns runs.
func TestHostAPI_Stats_Abtest_HappyPath(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

	const nameA = "repo@group-aa"
	const nameB = "repo@group-bb"
	iidA := seedSidecarStatsSession(t, d, nameA, startedAt, "finished")
	iidB := seedSidecarStatsSession(t, d, nameB, startedAt.Add(time.Second), "finished")
	_ = iidA
	_ = iidB

	groupID, err := d.RegisterGroup("repo@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.SetGroupID(nameA, groupID); err != nil {
		t.Fatalf("SetGroupID A: %v", err)
	}
	if err := d.SetGroupID(nameB, groupID); err != nil {
		t.Fatalf("SetGroupID B: %v", err)
	}

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	url := "/stats?view=abtest&group_id=" + groupID
	rr := doHostAPI(t, sc, http.MethodGet, url, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsCompareResponseWire
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
	// Sorted by session_name: group-aa comes before group-bb.
	if resp.Runs[0].Session.SessionName != nameA || resp.Runs[1].Session.SessionName != nameB {
		t.Errorf("sort order wrong: got (%q, %q), want (%q, %q)",
			resp.Runs[0].Session.SessionName, resp.Runs[1].Session.SessionName, nameA, nameB)
	}
}

// TestHostAPI_Stats_Abtest_MissingGroupReturns404 verifies that an unknown
// group_id surfaces as 404 with a clear message.
func TestHostAPI_Stats_Abtest_MissingGroupReturns404(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest&group_id=does-not-exist", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Abtest_RequiresGroupID verifies the missing-param 400.
func TestHostAPI_Stats_Abtest_RequiresGroupID(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// ── view=abtest_list ──────────────────────────────────────────────────────────

// TestHostAPI_Stats_AbtestList_Empty verifies that an empty DB returns the
// envelope with an empty pairs array (not null) so the CLI's empty-set
// hint path is exercised consistently.
func TestHostAPI_Stats_AbtestList_Empty(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest_list", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsAbtestListResponseWire
	decodeJSONBody(t, rr, &resp)
	if resp.Pairs == nil {
		t.Errorf("Pairs is nil; want empty slice for stable JSON roundtrip")
	}
	if len(resp.Pairs) != 0 {
		t.Errorf("got %d pairs, want 0", len(resp.Pairs))
	}
	// Verify the JSON envelope itself contains "pairs":[] not "pairs":null.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	if got := strings.TrimSpace(string(raw["pairs"])); got != "[]" {
		t.Errorf(`pairs raw JSON = %q, want "[]"`, got)
	}
}

// TestHostAPI_Stats_AbtestList_HappyPath seeds two abtest-paired sessions
// and verifies they appear in the listing.
func TestHostAPI_Stats_AbtestList_HappyPath(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

	const (
		nameA = "repo@pair-a"
		nameB = "repo@pair-b"
	)
	iidA := seedSidecarStatsSession(t, d, nameA, startedAt, "finished")
	iidB := seedSidecarStatsSession(t, d, nameB, startedAt.Add(time.Second), "finished")

	pairID := uuid.New().String()
	profA := "anthropic-opus-max-4-7"
	profB := "anthropic-opus-max-4-8"
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:   iidA,
		ProfileName:  &profA,
		AbtestPairID: &pairID,
		CreatedAt:    startedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs A: %v", err)
	}
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:   iidB,
		ProfileName:  &profB,
		AbtestPairID: &pairID,
		CreatedAt:    startedAt.Add(time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs B: %v", err)
	}

	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest_list", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp StatsAbtestListResponseWire
	decodeJSONBody(t, rr, &resp)
	if len(resp.Pairs) != 1 {
		t.Fatalf("got %d pairs, want 1; body = %s", len(resp.Pairs), rr.Body.String())
	}
	if resp.Pairs[0].PairID != pairID {
		t.Errorf("pair_id = %q, want %q", resp.Pairs[0].PairID, pairID)
	}
	if resp.Pairs[0].SessionNameA != nameA || resp.Pairs[0].SessionNameB != nameB {
		t.Errorf("session names = (%q, %q), want (%q, %q)",
			resp.Pairs[0].SessionNameA, resp.Pairs[0].SessionNameB, nameA, nameB)
	}
}

// ── privilege-boundary regression (PR #2107 review-security) ─────────────────

// TestHostAPI_Stats_Compare_DoesNotLeakPromptText is the regression guard
// for the review-security finding on PR #2107: the all-roles /stats
// endpoint must NOT serialise db.SpawnInputs.PromptText (or any of the
// other row-level conversation-content fields — PromptSource,
// ModelVariantOverrides, Extras, SkillsManifestHash, AgentPromptHash) to
// callers. The restricted StatsCompareInputsWire struct holds only the six
// render-relevant fields; this test asserts that contract by inserting a
// distinctive prompt body, issuing a worker-role request, and confirming
// the secret is absent from both the parsed envelope and the raw response
// bytes.
//
// AgentRole = "worker" exercises the path most likely to leak: a
// container worker session with PRISM_HOST_API set, talking to its own
// sidecar's /stats handler. The same data shape goes out for coordinator
// callers as well, but the worker case is the boundary that matters.
func TestHostAPI_Stats_Compare_DoesNotLeakPromptText(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)

	iidA := seedSidecarStatsSession(t, d, "repo@secret-a", startedAt, "finished")
	iidB := seedSidecarStatsSession(t, d, "repo@secret-b", startedAt.Add(time.Second), "finished")

	profile := "anthropic-opus-max-4-7"
	// Distinctive sentinel strings so a substring match is unambiguous.
	promptText := "SECRET-PROMPT-do-not-leak-9d8f7a6b"
	promptSource := "SECRET-SOURCE-also-do-not-leak"
	modelOverrides := `{"SECRET-OVERRIDES":"do-not-leak"}`
	extras := `{"SECRET-EXTRAS":"do-not-leak"}`
	skillsHash := "SECRET-SKILLS-HASH"
	agentHash := "SECRET-AGENT-PROMPT-HASH"
	promptTemplateHash := "SECRET-PROMPT-TEMPLATE-HASH"
	modelFlag := "SECRET-MODEL-FLAG"
	variantFlag := "SECRET-VARIANT-FLAG"

	for _, iid := range []string{iidA, iidB} {
		if err := d.InsertSpawnInputs(db.SpawnInputs{
			InstanceID:            iid,
			ProfileName:           &profile,
			PromptText:            &promptText,
			PromptSource:          &promptSource,
			ModelVariantOverrides: &modelOverrides,
			Extras:                &extras,
			SkillsManifestHash:    &skillsHash,
			AgentPromptHash:       &agentHash,
			PromptTemplateHash:    &promptTemplateHash,
			ModelFlag:             &modelFlag,
			VariantFlag:           &variantFlag,
			CreatedAt:             startedAt.UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertSpawnInputs %s: %v", iid, err)
		}
	}

	sc := newSidecarWithRole(t, "repo@secret-worker", "repo", "worker", d)
	url := fmt.Sprintf("/stats?view=compare&ids=%s,%s", iidA, iidB)
	rr := doHostAPI(t, sc, http.MethodGet, url, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	// Raw-byte check first: this is the strongest assertion because it
	// catches a serialised field even if the wire struct gains a typo
	// later (e.g. "prompttext" instead of "prompt_text").
	body := rr.Body.String()
	secrets := []string{
		promptText, promptSource, modelOverrides, extras,
		skillsHash, agentHash, promptTemplateHash,
		modelFlag, variantFlag,
	}
	for _, secret := range secrets {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaks %q to worker caller; full body:\n%s", secret, body)
		}
	}
	// And the JSON field names themselves — belt and braces, so a future
	// refactor that switches to a different secret value still trips this.
	forbiddenFields := []string{
		"prompt_text", "PromptText",
		"prompt_source", "PromptSource",
		"model_variant_overrides", "ModelVariantOverrides",
		"extras", "Extras",
		"skills_manifest_hash", "SkillsManifestHash",
		"agent_prompt_hash", "AgentPromptHash",
		"prompt_template_hash", "PromptTemplateHash",
		"model_flag", "ModelFlag",
		"variant_flag", "VariantFlag",
	}
	for _, field := range forbiddenFields {
		if strings.Contains(body, field) {
			t.Errorf("response body includes forbidden field key %q; full body:\n%s", field, body)
		}
	}

	// Parsed-envelope check: the six render-relevant fields must still be
	// present so the renderer works. ProfileName is the easiest to assert
	// because the test seeds it on every run.
	var resp StatsCompareResponseWire
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
	for i, run := range resp.Runs {
		if run.Inputs == nil {
			t.Errorf("run %d: Inputs is nil; want populated render-only subset", i)
			continue
		}
		if run.Inputs.ProfileName == nil || *run.Inputs.ProfileName != profile {
			t.Errorf("run %d: ProfileName = %v, want %q", i, run.Inputs.ProfileName, profile)
		}
	}
}

// ── unknown view ──────────────────────────────────────────────────────────────

// TestHostAPI_Stats_UnknownViewIncludesNewViews verifies the dispatcher's
// "unknown view" 400 message names the new views in the allow-list,
// guarding against drift between the doc comment and the actual switch.
func TestHostAPI_Stats_UnknownViewIncludesNewViews(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=bogus", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, view := range []string{"compare", "abtest", "abtest_list"} {
		if !strings.Contains(body, view) {
			t.Errorf("unknown-view error %q does not mention %q", body, view)
		}
	}
}
