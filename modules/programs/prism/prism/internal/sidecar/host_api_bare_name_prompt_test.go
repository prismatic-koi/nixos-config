package sidecar

// host_api_bare_name_prompt_test.go — issue #2658.
//
// The cross-repo arm of the host-API /prompt gate used to ask
// isCoordinatorSession. A non-worktree session such as `obsidian` has a bare
// name — no "@", so no branch, so no "@main" suffix — and its DB row carried a
// wrong root_agent_name, so both halves of that predicate answered false. The
// prompt was refused with:
//
//	cross-repo prompts can only target coordinators (<repo>@main), got "obsidian"
//
// The gate now asks isRootSession, which admits a bare name and nothing else
// new. This file pins the admission AND the four refusals that must survive
// it.
//
// # Isolation contract
//
// Every test routes Sidecar construction through the newAtlessIsolated*
// helpers in host_api_atless_session_test.go, which use
// sidecartest.NewIsolated: XDG_STATE_HOME points at a t.TempDir(),
// PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 stops promptdelivery dialling a real host
// socket, and the DB is a private SQLite file. Fixture names are all
// `prism-test`-prefixed, so none can collide with a live session on the
// developer's host.
//
// # Negative-mutation guarantee
//
// Revert the gate to isCoordinatorSession and
// TestHostAPI_Prompt_BareNameRoot_Allowed fails with 403. Widen it to admit
// every cross-repo target and the three refusal tests fail with 200.

import (
	"net/http"
	"strings"
	"testing"
)

// bareNamePromptFixture seeds the shape from the issue: a coordinator in one
// repo, and in another repo a bare-name session whose root_agent_name is
// wrong. It returns the sidecar for the coordinator.
func bareNamePromptFixture(t *testing.T, callerRole string) *Sidecar {
	t.Helper()
	const (
		caller     = "prism-test@coord-bare"
		callerRepo = "prism-test-coord-bare"
		bare       = "prism-test-bareroot"
	)
	sc, d := newAtlessIsolatedSidecarWithStub(t, caller, callerRepo, callerRole)

	if err := d.UpsertStatusSeedRootAgentName(
		caller, callerRepo, "/tmp/"+caller, "active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// The reported row: a bare name whose root_agent_name is 'review-goal'.
	// The session is not a review agent; the value is simply wrong, and
	// before #2658 nothing could override it.
	if err := d.UpsertStatusSeedRootAgentName(
		bare, bare, "/tmp/"+bare, "active", nil, nil, "review-goal", "", "host",
	); err != nil {
		t.Fatalf("seed bare-name target: %v", err)
	}
	// A descendant of the bare-name session, attributed to the parent's repo
	// (which is what the repo-derivation fix now writes).
	if err := d.UpsertStatusSeedRootAgentName(
		bare+"~investigate-v2", bare, "/tmp/"+bare+"-inv", "active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed investigator: %v", err)
	}
	// A plain cross-repo worker, to prove the gate still refuses one.
	if err := d.UpsertStatusSeedRootAgentName(
		"prism-test-other@feature", "prism-test-other", "/tmp/other", "active", nil, nil, "worker", "", "",
	); err != nil {
		t.Fatalf("seed cross-repo worker: %v", err)
	}
	return sc
}

// TestHostAPI_Prompt_BareNameRoot_Allowed is the reported defect. A
// coordinator in one repo prompts the bare-name root session of another, and
// the call must succeed even though the target's root_agent_name is wrong.
func TestHostAPI_Prompt_BareNameRoot_Allowed(t *testing.T) {
	sc := bareNamePromptFixture(t, "coordinator")

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test-bareroot","prompt":"kia ora"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 — a bare-name root session must be reachable", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Prompt_DescendantOfBareName_Refused is the security half. The
// investigator shares the bare-name parent's repo, so the repo-attribution fix
// makes it cross-repo relative to the caller. It must still be refused: a name
// that carries "~" is never a root session, whatever its DB row says.
func TestHostAPI_Prompt_DescendantOfBareName_Refused(t *testing.T) {
	sc := bareNamePromptFixture(t, "coordinator")

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test-bareroot~investigate-v2","prompt":"kia ora"}`)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 — a `~` descendant must never be promoted to a root session", rr.Code, rr.Body.String())
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if !strings.Contains(errResp["error"], "cross-repo") {
		t.Errorf("error %q should mention \"cross-repo\"", errResp["error"])
	}
}

// TestHostAPI_Prompt_CrossRepoWorker_StillRefused pins that the ordinary
// cross-repo refusal is unchanged. This is the case the gate has always
// refused, and widening the predicate must not have relaxed it.
func TestHostAPI_Prompt_CrossRepoWorker_StillRefused(t *testing.T) {
	sc := bareNamePromptFixture(t, "coordinator")

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test-other@feature","prompt":"kia ora"}`)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 for a cross-repo worker target", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Prompt_UnknownBareName_Is404 pins the boundary that makes the
// bare-name arm safe. IsRootSession admits any bare name on its name alone;
// the gate is only sound because repoFromSession refuses a name with no "@"
// and no agent_status row, one step earlier.
//
// If this ever regresses to 403 or 200, `prism prompt <typo>` stops saying
// "not found" and starts failing opaquely at delivery — the second symptom
// reported in #2658.
func TestHostAPI_Prompt_UnknownBareName_Is404(t *testing.T) {
	sc := bareNamePromptFixture(t, "coordinator")

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test-ghost","prompt":"kia ora"}`)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404 for an unknown bare name", rr.Code, rr.Body.String())
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if !strings.Contains(errResp["error"], "not found") {
		t.Errorf("error %q should mention \"not found\"", errResp["error"])
	}
}

// TestHostAPI_Prompt_WorkerBranchUnchanged pins the AC "a worker-role session
// still cannot prompt any session other than its own coordinator. The worker
// branch of the /prompt gate is unchanged."
//
// The worker branch never consults isCoordinatorSession or isRootSession — it
// compares the target against the one coordinator of the worker's own repo —
// so widening the coordinator arm must leave it untouched. A bare-name root
// session in another repo is exactly the kind of new target that must NOT
// become reachable.
func TestHostAPI_Prompt_WorkerBranchUnchanged(t *testing.T) {
	const (
		worker     = "prism-test-w@feature"
		workerRepo = "prism-test-w"
		bare       = "prism-test-bareroot"
	)
	sc, d := newAtlessIsolatedSidecarWithStub(t, worker, workerRepo, "worker")

	if err := d.UpsertStatusSeedRootAgentName(
		worker, workerRepo, "/tmp/"+worker, "active", nil, nil, "worker", "", "",
	); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(
		workerRepo+"@main", workerRepo, "/tmp/"+workerRepo, "active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed own coordinator: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(
		bare, bare, "/tmp/"+bare, "active", nil, nil, "review-goal", "", "host",
	); err != nil {
		t.Fatalf("seed bare-name target: %v", err)
	}

	// Refused: a bare-name root session in another repo.
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"`+bare+`","prompt":"kia ora"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 — a worker must not reach a bare-name root session", rr.Code, rr.Body.String())
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if !strings.Contains(errResp["error"], "workers can only prompt their own coordinator") {
		t.Errorf("error %q should be the unchanged worker refusal", errResp["error"])
	}

	// Allowed: the worker's own coordinator. This proves the worker branch is
	// still functional, not merely denying everything.
	rr = doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"`+workerRepo+`@main","prompt":"kia ora"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 — a worker must still reach its own coordinator", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Prompt_RefusalNamesTheNewRule pins the operator-facing half of
// the fix. The old message named `<repo>@main` as the only reachable shape,
// which is what led an operator to guess `obsidian@main` and hit a second,
// less clear error. The message must now describe both shapes.
func TestHostAPI_Prompt_RefusalNamesTheNewRule(t *testing.T) {
	sc := bareNamePromptFixture(t, "coordinator")

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test-other@feature","prompt":"kia ora"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	msg := errResp["error"]
	if !strings.Contains(msg, "root sessions") {
		t.Errorf("error %q should name the rule as \"root sessions\"", msg)
	}
	if !strings.Contains(msg, "bare name") {
		t.Errorf("error %q should tell the operator that a bare-name session is reachable too", msg)
	}
}
