package sidecar

// Tests for the @-less session-name resolution path.
//
// # Isolation contract
//
// Every test in this file routes Sidecar construction through
// sidecartest.NewIsolated(t, ...) so that:
//   - XDG_STATE_HOME points at a t.TempDir(); host paths never escape the
//     test sandbox.
//   - PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 prevents promptdelivery from
//     dialling a real host socket.
//   - The DB is an isolated SQLite file in a private MkdirTemp dir.
//
// Fixture session names use this discipline:
//   - @-bearing sessions   → "prism-test@<descriptor>"
//   - @-less host-mode sessions → "prism-test-<descriptor>"
//   - investigator descendants → "prism-test-<descriptor>~investigate-<slug>"
//
// Crucially, NO test uses a real production session name (e.g.
// "nixos-config@main") or a real ProjectIsolationOverrides slug
// ("obsidian", "obsidian~investigate-apis"). The fixture shape mirrors
// production without colliding with any live coordinator on the developer's
// host — see the sidecartest package doc.
//
// # Why @-less fixtures still drive the regression test
//
// The production failure mode is name-shape, not name-value. A fixture named
// "prism-test-obsidianlike" lacks an "@" separator in the exact same way
// that "obsidian" does, and therefore trips a string-parser that assumes an
// "@" in the exact same way.
//
// # Negative-mutation guarantee
//
// Every test is constructed so that reverting helpers.go::repoFromSession to
// a string-only form that requires an "@" causes the test to FAIL:
//
//   - TestHostAPI_RepoFromSession_DBFallback:
//       Asserts repoFromSession("prism-test-obsidianlike", d) ==
//       "prism-test-obsidianlike". A string-only parser returns an error and
//       an empty string; the assertion fails.
//
//   - TestHostAPI_AtlessSession_Checkin_Resolves:
//       Asserts /checkin against an @-less target returns 200. A string-only
//       parser returns 400 ("contains no '@'"); the assertion fails.
//
//   - TestHostAPI_AtlessSession_Checkin_NotFound:
//       Asserts /checkin against an unknown @-less name returns 404 with a
//       "not found" body. A string-only parser returns 400 with "contains no
//       '@'"; the assertion fails on both status code and body content.
//
//   - TestHostAPI_AtlessSession_Prompt_Resolves:
//       Asserts /prompt with an @-less target succeeds. A string-only parser
//       returns 400 ("invalid target session name"); the assertion fails.
//
//   - TestHostAPI_AtlessSession_CrossRepoWorker_403:
//       Asserts that a coordinator in repo A cannot /checkin an @-less
//       worker in repo B (the cross-repo permission check still fires).
//       A string-only parser returns 400 before reaching the permission
//       check; the assertion that the code is 403 fails.
//
//   - TestHostAPI_AtlessSession_Spawn_Resolves:
//       Asserts /spawn succeeds when the sidecar's own session has an
//       @-less name. A string-only parser returns 500 ("cannot derive repo")
//       at the own-repo derivation step; the assertion fails.

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// ── Test helpers ─────────────────────────────────────────────────────────────

// newAtlessIsolatedSidecar constructs an isolated Sidecar+DB for an @-less
// session-name test. It routes through sidecartest.NewIsolated so that
// XDG_STATE_HOME is redirected to a tempdir and the
// PRISM_TEST_MODE_RESTRICT_HOSTAPI guard is set — closing both isolation
// gaps.
//
// The bus's DB is returned so the caller can seed any additional fixture
// rows (caller-coordinator, target-worker, etc.) needed by the test.
func newAtlessIsolatedSidecar(t *testing.T, sessionName, repo, role string) (*Sidecar, *db.DB) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	clk := newTestClock()
	cfg := Config{
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/tmp/" + sessionName,
		HarnessURL:  "http://localhost:14000",
		DB:          bus.DB,
		Clock:       clk,
		AgentRole:   role,
		Harness:     newSSEHarness(),
	}
	return New(cfg), bus.DB
}

// newAtlessIsolatedSidecarWithStub is the success-stub variant: it points
// PrismBinaryPath at a shell script that exits 0, so handlers that shell
// out (`/prompt`, `/spawn`) succeed past the permission gate without
// launching the real prism binary. Isolation contract identical to
// newAtlessIsolatedSidecar.
func newAtlessIsolatedSidecarWithStub(t *testing.T, sessionName, repo, role string) (*Sidecar, *db.DB) {
	t.Helper()
	stubPath := filepath.Join(t.TempDir(), "prism-stub-success")
	if err := os.WriteFile(stubPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write success stub: %v", err)
	}
	bus := sidecartest.NewIsolated(t, "")
	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        "/tmp/" + sessionName,
		HarnessURL:      "http://localhost:14000",
		DB:              bus.DB,
		Clock:           clk,
		AgentRole:       role,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	return New(cfg), bus.DB
}

// newAtlessIsolatedSidecarWithSpawnStub returns a Sidecar whose stub binary
// echoes a synthetic `session "<name>" created` line so /spawn's session-name
// parser produces a deterministic value. Used by the /spawn happy-path test.
func newAtlessIsolatedSidecarWithSpawnStub(t *testing.T, sessionName, repo, role, spawnedName string) (*Sidecar, *db.DB) {
	t.Helper()
	stubPath := filepath.Join(t.TempDir(), "prism-stub-spawn")
	script := "#!/bin/sh\necho 'session \"" + spawnedName + "\" created'\n"
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write spawn stub: %v", err)
	}
	bus := sidecartest.NewIsolated(t, "")
	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        "/tmp/" + sessionName,
		HarnessURL:      "http://localhost:14000",
		DB:              bus.DB,
		Clock:           clk,
		AgentRole:       role,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	return New(cfg), bus.DB
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
	// Even the helper-level test routes through sidecartest.NewIsolated so
	// the isolation contract is enforced uniformly. No Sidecar is needed
	// here — only the bus's isolated DB.
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	// Seed an @-less host-mode session: name "prism-test-obsidianlike",
	// repo "prism-test-obsidianlike" (mirrors the production shape from
	// `obsidian` against ~/Documents/obsidian without using the literal
	// production name).
	if err := d.UpsertStatusSeedRootAgentName(
		"prism-test-obsidianlike", "prism-test-obsidianlike",
		"/tmp/prism-test-obsidianlike", "active",
		nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed obsidian-like row: %v", err)
	}

	// Seed an @-bearing session whose DB repo intentionally differs from
	// the name prefix, to prove the DB is authoritative.
	if err := d.UpsertStatus(
		"prism-test@alias-main", "prism-test-real-repo",
		"/tmp/prism-test-real", "active", nil, nil,
	); err != nil {
		t.Fatalf("seed alias row: %v", err)
	}

	// (a) @-less name with valid DB row → DB wins.
	got, err := repoFromSession("prism-test-obsidianlike", d)
	if err != nil {
		t.Errorf("repoFromSession(\"prism-test-obsidianlike\", d): unexpected error: %v", err)
	}
	if got != "prism-test-obsidianlike" {
		t.Errorf("repoFromSession(\"prism-test-obsidianlike\", d) = %q, want \"prism-test-obsidianlike\"", got)
	}

	// (b) @-less name with empty DB repo → fallback → error.
	if err := d.UpsertStatusSeedRootAgentName(
		"prism-test-empty-repo", "", "/tmp/prism-test-empty", "active",
		nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed empty-repo row: %v", err)
	}
	if _, err := repoFromSession("prism-test-empty-repo", d); err == nil {
		t.Errorf("repoFromSession(\"prism-test-empty-repo\", d): expected error, got nil")
	}

	// (c) @-less name with no DB row → error with "not found".
	_, err = repoFromSession("prism-test-ghost", d)
	if err == nil {
		t.Errorf("repoFromSession(\"prism-test-ghost\", d): expected error, got nil")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("repoFromSession(\"prism-test-ghost\", d): error %q should mention \"not found\"", err.Error())
	}

	// (d) <repo>@<branch> name with no DB row → name parse wins.
	got, err = repoFromSession("prism-test@brand-new-feature", d)
	if err != nil {
		t.Errorf("repoFromSession(\"prism-test@brand-new-feature\", d): unexpected error: %v", err)
	}
	if got != "prism-test" {
		t.Errorf("repoFromSession(\"prism-test@brand-new-feature\", d) = %q, want \"prism-test\"", got)
	}

	// (e) <repo>@<branch> name with DB row whose repo differs → DB wins.
	got, err = repoFromSession("prism-test@alias-main", d)
	if err != nil {
		t.Errorf("repoFromSession(\"prism-test@alias-main\", d): unexpected error: %v", err)
	}
	if got != "prism-test-real-repo" {
		t.Errorf("repoFromSession(\"prism-test@alias-main\", d) = %q, want \"prism-test-real-repo\" (DB authoritative)", got)
	}

	// (f) nil DB → pure name parse.
	got, err = repoFromSession("prism-test@myrepo-main", nil)
	if err != nil {
		t.Errorf("repoFromSession(\"prism-test@myrepo-main\", nil): unexpected error: %v", err)
	}
	if got != "prism-test" {
		t.Errorf("repoFromSession(\"prism-test@myrepo-main\", nil) = %q, want \"prism-test\"", got)
	}
	if _, err := repoFromSession("prism-test-obsidianlike", nil); err == nil {
		t.Errorf("repoFromSession(\"prism-test-obsidianlike\", nil): expected error (no DB, no '@'), got nil")
	}

	// Sanity: isCoordinatorSession still recognises the @-less row as a
	// coordinator via root_agent_name. The cross-repo permission check
	// path depends on this composition working.
	if !isCoordinatorSession("prism-test-obsidianlike", d, log.Default()) {
		t.Error("isCoordinatorSession(\"prism-test-obsidianlike\", d): got false, want true (root_agent_name=coordinator)")
	}
}

// ── End-to-end: host-API surface ──────────────────────────────────────────────

// TestHostAPI_AtlessSession_Checkin_Resolves covers AC: "prism checkin <name>
// succeeds for an @-less session name when a row exists". Driven through the
// public mux so that the call-site update in host_api.go is also pinned (a
// regression that touched only helpers.go but missed the call-site sweep
// would still fail here because the handler would still reject the target
// via the per-handler repoFromSession parse error).
func TestHostAPI_AtlessSession_Checkin_Resolves(t *testing.T) {
	const (
		caller = "prism-test@coord-checkin"
		target = "prism-test-obsidianlike"
	)
	sc, d := newAtlessIsolatedSidecar(t, caller, "prism-test-coord-checkin", "coordinator")

	// Seed the caller as a coordinator (DB-backed coordinator check uses
	// root_agent_name).
	if err := d.UpsertStatusSeedRootAgentName(
		caller, "prism-test-coord-checkin", "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// Seed the @-less target as a coordinator in its own repo.
	if err := d.UpsertStatusSeedRootAgentName(
		target, target, "/tmp/"+target,
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+target, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for @-less target with DB row", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	decodeJSONBody(t, rr, &resp)
	if resp["session"] != target {
		t.Errorf("response session = %v, want %q", resp["session"], target)
	}
	if _, ok := resp["events"]; !ok {
		t.Errorf("response missing \"events\" key: %v", resp)
	}
}

// TestHostAPI_AtlessSession_Checkin_InvestigatorShape covers the descendant
// naming pattern from the issue: `<host-mode-parent>~investigate-<slug>`.
// The DB row is what makes this resolvable; the name shape alone is
// ambiguous (no '@', not a literal repo name).
func TestHostAPI_AtlessSession_Checkin_InvestigatorShape(t *testing.T) {
	const (
		caller       = "prism-test-obsidianlike"
		investigator = "prism-test-obsidianlike~investigate-apis"
		repo         = "prism-test-obsidianlike"
	)
	// Caller is the @-less coordinator itself — only same-repo callers
	// can reach a worker target (the cross-repo permission check forbids
	// non-coordinator cross-repo targets).
	sc, d := newAtlessIsolatedSidecar(t, caller, repo, "coordinator")

	if err := d.UpsertStatusSeedRootAgentName(
		caller, repo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// Investigator inherits the @-less parent's shape. The DB row keeps
	// repo=<parent-repo> even though the session name embeds a
	// "~investigate-" suffix. A string-only parser fails here too.
	if err := d.UpsertStatusSeedRootAgentName(
		investigator, repo, "/tmp/"+investigator,
		"active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed investigator: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+investigator, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for @-less investigator target", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_AtlessSession_Checkin_NotFound covers AC: "prism checkin
// <unknown-name> (no DB row, name has no '@') returns a clear error naming
// the unknown session, not a 'cannot derive repo' parse error".
//
// The handler now returns 404 ("target session ... not found") for this
// case so the CLI can surface the canonical not-found error. The body
// substring assertions pin the new error-message shape and would fail
// against the pre-fix "contains no '@'" message even if the status code
// were lenient.
func TestHostAPI_AtlessSession_Checkin_NotFound(t *testing.T) {
	const (
		caller = "prism-test@coord-notfound"
		repo   = "prism-test-notfound"
		ghost  = "prism-test-ghost"
	)
	sc, d := newAtlessIsolatedSidecar(t, caller, repo, "coordinator")
	if err := d.UpsertStatusSeedRootAgentName(
		caller, repo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+ghost, "")
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
	if !strings.Contains(msg, ghost) {
		t.Errorf("error %q should name the unknown session %q", msg, ghost)
	}
}

// TestHostAPI_AtlessSession_Prompt_Resolves covers AC: "prism prompt <name>
// succeeds for an @-less session name with a DB row". The /prompt handler is
// fan-tested through a success stub so the cross-session permission and
// resolution paths are exercised without launching the real prism binary.
func TestHostAPI_AtlessSession_Prompt_Resolves(t *testing.T) {
	const (
		caller = "prism-test@coord-prompt"
		repo   = "prism-test-coord-prompt"
		target = "prism-test-obsidianlike"
	)
	sc, d := newAtlessIsolatedSidecarWithStub(t, caller, repo, "coordinator")

	if err := d.UpsertStatusSeedRootAgentName(
		caller, repo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// @-less target as a coordinator in its own repo — coordinators can
	// cross-repo prompt other coordinators, so this is the permission shape
	// to exercise.
	if err := d.UpsertStatusSeedRootAgentName(
		target, target, "/tmp/"+target,
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"`+target+`","prompt":"hello"}`)

	// Pre-fix: 400 ("invalid target session name: ... contains no '@'").
	// Post-fix: 200 (prism prompt stub succeeds; the cross-repo @-less
	// coordinator target is allowed).
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
//   - Post-fix code without the cross-repo permission check (a regression in
//     the new code path) would return 200 → also fails this assertion.
//
// Both regressions are caught by the same test.
func TestHostAPI_AtlessSession_CrossRepoWorker_403(t *testing.T) {
	const (
		caller       = "prism-test@coord-crossrepo"
		callerRepo   = "prism-test-coord-crossrepo"
		targetWorker = "prism-test-otherrepolike-bg-task"
		targetRepo   = "prism-test-otherrepolike"
	)
	sc, d := newAtlessIsolatedSidecar(t, caller, callerRepo, "coordinator")

	if err := d.UpsertStatusSeedRootAgentName(
		caller, callerRepo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	// Target: @-less worker in a different repo (not a coordinator). The
	// DB-fallback path resolves its repo via agent_status.repo, the cross-
	// repo check fires, and isCoordinatorSession returns false
	// (root_agent_name="worker" and the name doesn't end in "@main") → 403.
	if err := d.UpsertStatusSeedRootAgentName(
		targetWorker, targetRepo, "/tmp/"+targetWorker,
		"active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+targetWorker, "")
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
	const (
		caller = "prism-test-obsidianlike"
		repo   = "prism-test-obsidianlike"
		target = "prism-test-obsidianlike~investigate-apis"
	)
	sc, d := newAtlessIsolatedSidecar(t, caller, repo, "coordinator")

	if err := d.UpsertStatusSeedRootAgentName(
		caller, repo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(
		target, repo, "/tmp/"+target,
		"active", nil, nil, "worker", "", "host",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+target, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for same-repo @-less worker target", rr.Code)
	}
}

// TestHostAPI_AtlessSession_BackwardsCompat covers AC: "prism checkin <name>
// for a <repo>@<branch> session continues to work unchanged — no regression
// in the existing happy path".
func TestHostAPI_AtlessSession_BackwardsCompat(t *testing.T) {
	const (
		caller = "prism-test@coord-backcompat"
		repo   = "prism-test-backcompat"
		target = "prism-test@worker-backcompat"
	)
	sc, d := newAtlessIsolatedSidecar(t, caller, repo, "coordinator")

	if err := d.UpsertStatusSeedRootAgentName(
		caller, repo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(
		target, repo, "/tmp/"+target,
		"active", nil, nil, "worker", "", "",
	); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/checkin?session="+target, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for same-repo @-bearing worker target", rr.Code)
	}
}

// TestHostAPI_AtlessSession_Spawn_Resolves covers the /spawn handler's
// own-repo derivation path for an @-less sidecar. The DB-fallback resolves
// the repo from agent_status, the spawn proceeds, and the stub binary's
// `session "<name>" created` line is parsed back as the response.
func TestHostAPI_AtlessSession_Spawn_Resolves(t *testing.T) {
	const (
		caller      = "prism-test-obsidianlike"
		repo        = "prism-test-obsidianlike"
		spawnedName = "prism-test-obsidianlike@some-branch"
	)
	sc, d := newAtlessIsolatedSidecarWithSpawnStub(t, caller, repo, "coordinator", spawnedName)
	if err := d.UpsertStatusSeedRootAgentName(
		caller, repo, "/tmp/"+caller,
		"active", nil, nil, "coordinator", "", "host",
	); err != nil {
		t.Fatalf("seed caller: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn",
		`{"branch":"some-branch","prompt":"hi"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for @-less sidecar /spawn", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	decodeJSONBody(t, rr, &resp)
	if resp["session_name"] != spawnedName {
		t.Errorf("response session_name = %q, want %q", resp["session_name"], spawnedName)
	}
}
