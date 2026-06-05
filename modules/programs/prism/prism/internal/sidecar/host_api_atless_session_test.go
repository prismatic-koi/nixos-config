package sidecar

// Tests for the @-less session-name resolution path introduced by issue #2112.
//
// Pre-#2112, repoFromSession was a pure string parser that returned an error
// for any session name lacking an "@" separator. Host-mode non-git sessions
// configured via ProjectIsolationOverrides (e.g. "obsidian" against
// ~/Documents/obsidian) and their investigator children (e.g.
// "obsidian~investigate-apis") are legitimate @-less names with valid DB
// rows — but every host-API permission check that called repoFromSession
// rejected them with a "session name contains no '@' — cannot derive repo"
// parse error, making `prism checkin`, `prism prompt`, and related surfaces
// unusable against them.
//
// The fix layers a DB lookup (via db.CurrentStatus) in front of the name-
// parse path. When agent_status has a row for the session with a non-empty
// repo column, that value is returned authoritatively. The name-parse path
// remains as a fallback for pre-row state and pure-helper unit tests.
//
// # Negative-mutation guarantee
//
// Every test in this file is constructed so that reverting helpers.go::
// repoFromSession to its pre-#2112 string-only form causes the test to FAIL:
//
//   - TestHostAPI_RepoFromSession_DBFallback:
//       Asserts repoFromSession("obsidian", d) == "obsidian". Pre-fix code
//       returns an error and an empty string; the assertion fails.
//
//   - TestHostAPI_AtlessSession_Checkin_Resolves:
//       Asserts /checkin?session=obsidian returns 200. Pre-fix code returns
//       400 with the parse error; the assertion fails.
//
//   - TestHostAPI_AtlessSession_Checkin_NotFound:
//       Asserts /checkin against an unknown @-less name returns 404 with a
//       "not found" body. Pre-fix code returns 400 with "contains no '@'";
//       the assertion fails on both status code and body content.
//
//   - TestHostAPI_AtlessSession_Prompt_Resolves:
//       Asserts /prompt with an @-less target succeeds. Pre-fix code returns
//       400 ("invalid target session name"); the assertion fails.
//
//   - TestHostAPI_AtlessSession_CrossRepoWorker_403:
//       Asserts that a coordinator in repo A cannot /checkin an @-less
//       worker in repo B (the cross-repo permission check still fires).
//       Pre-fix code returns 400 before reaching the permission check;
//       the assertion that the code is 403 fails.

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// newSidecarWithSuccessStub constructs a Sidecar whose PrismBinaryPath points
// at a shell script that always exits 0. Used by /prompt and other host-API
// tests that need to pass through the permission gate without launching the
// real prism binary.
func newSidecarWithSuccessStub(t *testing.T, sessionName, repo, role string, d *db.DB) *Sidecar {
	t.Helper()
	stubPath := filepath.Join(t.TempDir(), "prism-stub-success")
	stubScript := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write success stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        "/tmp/" + sessionName,
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       role,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	return New(cfg)
}

// ── Helper-level: repoFromSession with DB fallback ────────────────────────────

// TestHostAPI_RepoFromSession_DBFallback covers AC: "@-less session with DB row
// → resolves correctly". The helper is exercised directly so the assertion
// pins the new behaviour at the smallest possible surface.
//
// Coverage matrix:
//
//	(a) @-less name + DB row with non-empty repo → returns DB repo.
//	(b) @-less name + DB row with empty repo     → falls through to name parse,
//	                                                which also fails → error.
//	(c) @-less name + no DB row                  → name-parse fallback fires;
//	                                                no '@' → error.
//	(d) <repo>@<branch> name + no DB row         → name-parse fallback wins.
//	(e) <repo>@<branch> name + DB row with diff  → DB authoritative; row wins.
//	(f) nil DB                                   → pure name parse (legacy).
func TestHostAPI_RepoFromSession_DBFallback(t *testing.T) {
	d := openTestDB(t)

	// Seed an @-less host-mode session: name "obsidian", repo "obsidian"
	// (matching the production shape from ~/Documents/obsidian +
	// ProjectIsolationOverrides).
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian", "obsidian", "/home/test/obsidian", "active",
		nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed obsidian row: %v", err)
	}

	// Seed an @-bearing session whose DB repo intentionally differs from
	// the name prefix, to prove the DB is authoritative.
	if err := d.UpsertStatus(
		"alias@main", "real-repo", "/home/test/real", "active", nil, nil,
	); err != nil {
		t.Fatalf("seed alias row: %v", err)
	}

	// (a) @-less name with valid DB row → DB wins.
	got, err := repoFromSession("obsidian", d)
	if err != nil {
		t.Errorf("repoFromSession(\"obsidian\", d): unexpected error: %v", err)
	}
	if got != "obsidian" {
		t.Errorf("repoFromSession(\"obsidian\", d) = %q, want \"obsidian\"", got)
	}

	// (b) @-less name with empty DB repo → fallback → error.
	if err := d.UpsertStatusSeedRootAgentName(
		"empty-repo", "", "/tmp/empty", "active",
		nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed empty-repo row: %v", err)
	}
	if _, err := repoFromSession("empty-repo", d); err == nil {
		t.Errorf("repoFromSession(\"empty-repo\", d): expected error, got nil")
	}

	// (c) @-less name with no DB row → error with "not found".
	_, err = repoFromSession("ghost", d)
	if err == nil {
		t.Errorf("repoFromSession(\"ghost\", d): expected error, got nil")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("repoFromSession(\"ghost\", d): error %q should mention \"not found\"", err.Error())
	}

	// (d) <repo>@<branch> name with no DB row → name parse wins.
	got, err = repoFromSession("brand-new@feature", d)
	if err != nil {
		t.Errorf("repoFromSession(\"brand-new@feature\", d): unexpected error: %v", err)
	}
	if got != "brand-new" {
		t.Errorf("repoFromSession(\"brand-new@feature\", d) = %q, want \"brand-new\"", got)
	}

	// (e) <repo>@<branch> name with DB row whose repo differs → DB wins.
	got, err = repoFromSession("alias@main", d)
	if err != nil {
		t.Errorf("repoFromSession(\"alias@main\", d): unexpected error: %v", err)
	}
	if got != "real-repo" {
		t.Errorf("repoFromSession(\"alias@main\", d) = %q, want \"real-repo\" (DB authoritative)", got)
	}

	// (f) nil DB → pure name parse.
	got, err = repoFromSession("myrepo@main", nil)
	if err != nil {
		t.Errorf("repoFromSession(\"myrepo@main\", nil): unexpected error: %v", err)
	}
	if got != "myrepo" {
		t.Errorf("repoFromSession(\"myrepo@main\", nil) = %q, want \"myrepo\"", got)
	}
	if _, err := repoFromSession("obsidian", nil); err == nil {
		t.Errorf("repoFromSession(\"obsidian\", nil): expected error (no DB, no '@'), got nil")
	}

	// Sanity: isCoordinatorSession still recognises the @-less obsidian row
	// as a coordinator via root_agent_name. Pre-#2112 this could only be
	// reached if the caller manually checked the DB; the @-less target check
	// now folds in transparently.
	if !isCoordinatorSession("obsidian", d, log.Default()) {
		t.Error("isCoordinatorSession(\"obsidian\", d): got false, want true (root_agent_name=coordinator)")
	}
}

// ── End-to-end: host-API surface ──────────────────────────────────────────────

// TestHostAPI_AtlessSession_Checkin_Resolves covers AC: "prism checkin <name>
// succeeds for an @-less session name when a row exists". The handler is
// driven through the public mux so that the call-site update in host_api.go
// is also pinned (a regression that touched only helpers.go but missed the
// call-site sweep would still fail here because the handler would still
// reject the target via the per-handler repoFromSession parse error).
func TestHostAPI_AtlessSession_Checkin_Resolves(t *testing.T) {
	d := openTestDB(t)

	// Seed a coordinator caller in repo "nixos-config".
	if err := d.UpsertStatusSeedRootAgentName(
		"nixos-config@main", "nixos-config", "/tmp/nixos-config",
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// Seed the @-less obsidian target as a coordinator in its own repo.
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian", "obsidian", "/tmp/obsidian",
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed obsidian: %v", err)
	}

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session=obsidian", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for @-less target with DB row", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	decodeJSONBody(t, rr, &resp)
	if resp["session"] != "obsidian" {
		t.Errorf("response session = %v, want \"obsidian\"", resp["session"])
	}
	if _, ok := resp["events"]; !ok {
		t.Errorf("response missing \"events\" key: %v", resp)
	}
}

// TestHostAPI_AtlessSession_Checkin_InvestigatorShape covers the descendant
// naming pattern from the issue: `obsidian~investigate-apis`. The DB row is
// what makes this resolvable; the name shape alone is ambiguous (it has no
// '@' and isn't a literal repo name).
func TestHostAPI_AtlessSession_Checkin_InvestigatorShape(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatusSeedRootAgentName(
		"nixos-config@main", "nixos-config", "/tmp/nixos-config",
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// Investigator inherits the @-less parent's shape. The DB row keeps
	// repo=obsidian even though the session name embeds a "~investigate-"
	// suffix. The cross-repo permission check folds in via repoFromSession's
	// DB lookup, so this target counts as same-repo to a coordinator in
	// repo "obsidian" but cross-repo non-coordinator to other coordinators.
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian~investigate-apis", "obsidian", "/tmp/obsidian-inv",
		"active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed investigator: %v", err)
	}

	// Same-coordinator-repo caller: the obsidian coordinator can reach its
	// own investigator. Seed an obsidian coordinator and drive from there.
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian", "obsidian", "/tmp/obsidian",
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed obsidian: %v", err)
	}
	sc := newSidecarWithRole(t, "obsidian", "obsidian", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session=obsidian~investigate-apis", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for @-less investigator target", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_AtlessSession_Checkin_NotFound covers AC: "prism checkin
// <unknown-name> (no DB row, name has no '@') returns a clear error naming
// the unknown session, not a 'cannot derive repo' parse error".
//
// The handler now returns 404 ("not found") for this case so the CLI can
// surface the canonical not-found error. The body must contain "not found"
// — the substring check pins the new error-message shape and would fail
// against the pre-fix "contains no '@'" message even if the status code
// were lenient.
func TestHostAPI_AtlessSession_Checkin_NotFound(t *testing.T) {
	d := openTestDB(t)
	if err := d.UpsertStatusSeedRootAgentName(
		"nixos-config@main", "nixos-config", "/tmp/nixos-config",
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session=ghost-session", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown @-less target", rr.Code)
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	msg := errResp["error"]
	if !strings.Contains(msg, "not found") {
		t.Errorf("error %q should mention \"not found\" (canonical shape post-#2112)", msg)
	}
	// Negative-mutation guard: must NOT match the pre-fix error shape.
	if strings.Contains(msg, "cannot derive repo") || strings.Contains(msg, "contains no '@'") {
		t.Errorf("error %q must not contain the pre-#2112 parse-error phrase", msg)
	}
	// The error message must name the unknown session so the operator can act.
	if !strings.Contains(msg, "ghost-session") {
		t.Errorf("error %q should name the unknown session \"ghost-session\"", msg)
	}
}

// TestHostAPI_AtlessSession_Prompt_Resolves covers AC: "prism prompt <name>
// succeeds for an @-less session name with a DB row". The /prompt handler
// is fan-tested through a stub binary so the cross-session permission and
// resolution paths are exercised without a real prism binary.
func TestHostAPI_AtlessSession_Prompt_Resolves(t *testing.T) {
	d := openTestDB(t)
	if err := d.UpsertStatusSeedRootAgentName(
		"nixos-config@main", "nixos-config", "/tmp/nixos-config",
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// @-less target as a coordinator in its own repo — coordinators can
	// cross-repo prompt other coordinators, so this is the permission shape
	// to exercise.
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian", "obsidian", "/tmp/obsidian",
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed obsidian: %v", err)
	}

	sc := newSidecarWithSuccessStub(t, "nixos-config@main", "nixos-config", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"obsidian","prompt":"hello"}`)

	// Pre-fix: 400 ("invalid target session name: ... contains no '@'").
	// Post-fix: 200 (prism prompt stub succeeds; the cross-repo @-less
	// target is allowed because it is a coordinator).
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for @-less coordinator target", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_AtlessSession_CrossRepoWorker_403 covers AC: "an @-less worker
// session in another repo cannot be reached via prism checkin from a
// coordinator in this repo — the cross-repo permission check still blocks
// non-coordinator targets even when the target name lacks '@'".
//
// This is the security-property test. Pre-fix, the handler returned 400 at
// the repoFromSession parse step, never reaching the cross-repo branch. The
// test asserts 403 so:
//
//   - Pre-fix code (returns 400) fails this assertion → negative-mutation guard.
//   - Post-fix code without the cross-repo permission check (a bug in the new
//     code path) would return 200 → also fails this assertion.
//
// Both regressions are caught by the same test.
func TestHostAPI_AtlessSession_CrossRepoWorker_403(t *testing.T) {
	d := openTestDB(t)

	// Caller: coordinator in repo "nixos-config".
	if err := d.UpsertStatusSeedRootAgentName(
		"nixos-config@main", "nixos-config", "/tmp/nixos-config",
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// Target: @-less worker in repo "obsidian" (different repo, not a
	// coordinator). The DB-fallback path resolves its repo to "obsidian"
	// via agent_status.repo, the cross-repo check fires, and the
	// isCoordinatorSession check returns false (root_agent_name="worker"
	// and the name doesn't end in "@main") → 403.
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian-bg-task", "obsidian", "/tmp/obsidian-bg",
		"active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session=obsidian-bg-task", "")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for cross-repo @-less non-coordinator target (cross-repo permission check must fire)", rr.Code)
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if !strings.Contains(errResp["error"], "cross-repo") {
		t.Errorf("error %q should mention \"cross-repo\"", errResp["error"])
	}
}

// TestHostAPI_AtlessSession_OwnRepoWorker_OK covers the positive side of the
// same security property: a coordinator CAN reach an @-less worker in its
// OWN repo. This pins the same-repo branch of the permission decision.
func TestHostAPI_AtlessSession_OwnRepoWorker_OK(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian", "obsidian", "/tmp/obsidian",
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(
		"obsidian~investigate-apis", "obsidian", "/tmp/obsidian-inv",
		"active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	sc := newSidecarWithRole(t, "obsidian", "obsidian", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session=obsidian~investigate-apis", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for same-repo @-less worker target", rr.Code)
	}
}

// TestHostAPI_AtlessSession_BackwardsCompat covers AC: "prism checkin <name>
// for a <repo>@<branch> session continues to work unchanged — no regression
// in the existing happy path".
func TestHostAPI_AtlessSession_BackwardsCompat(t *testing.T) {
	d := openTestDB(t)
	if err := d.UpsertStatusSeedRootAgentName(
		"myrepo@main", "myrepo", "/tmp/myrepo",
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(
		"myrepo@feature", "myrepo", "/tmp/myrepo-feature",
		"active", nil, nil, "worker", "", "",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session=myrepo@feature", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for same-repo @-bearing worker target", rr.Code)
	}
}
