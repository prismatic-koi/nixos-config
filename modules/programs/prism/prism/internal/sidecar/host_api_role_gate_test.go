package sidecar

// host_api_role_gate_test.go — issue #2588.
//
// The worker restriction on coordinator-only verbs (`prism merge`,
// `prism investigate`, …) is enforced at the host API by requireCoordinator.
// There is no bash-level deny list for those verbs, so this file is the
// regression guard for the boundary itself:
//
//   - /investigate refuses a worker with 403 and serves a coordinator
//     unchanged (the defect fixed in #2588: the handler decoded the body and
//     spawned an investigator for any caller).
//   - Every endpoint documented "coordinator only" in the hostAPIHandler
//     route list refuses a worker with 403.
//   - A caller whose role cannot be determined — no agent_status row, a
//     pre-migration row with NULL root_agent_name, or no DB handle at all —
//     is treated as a worker and refused.
//
// # Isolation contract (#1608)
//
// Every sidecar here is built on sidecartest.NewIsolated(t, ""), so
// $XDG_STATE_HOME points at a t.TempDir(), PRISM_TEST_MODE_RESTRICT_HOSTAPI
// is set, and the DB is a test-scoped SQLite file. Session names use the
// "prism-test" prefix so a leaked write cannot collide with a live session on
// the developer's host. Every sidecar also runs against a shell stub instead
// of the real prism binary: if a role gate ever regresses, the handler execs
// the stub rather than really spawning a session.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// failingStub exits 1 without touching the host. A handler that reaches the
// exec path with this stub returns 500, which is distinguishable from the 403
// the role gate must produce.
const failingStub = "#!/bin/sh\nexit 1\n"

// newRoleGateSidecar builds an isolated Sidecar for role-gate assertions.
//
// rootAgentName seeds agent_status.root_agent_name for the session:
//
//	"coordinator" / "worker" — the row is seeded with that value.
//	""                       — no row is seeded at all, so the DB lookup
//	                           finds nothing and the caller's role can only
//	                           come from the session-name heuristic.
//
// agentRole is written to Config.AgentRole. requireCoordinator does not read
// it — the field is set only so the sidecar is shaped like the real thing.
func newRoleGateSidecar(t *testing.T, sessionName, repo, rootAgentName, agentRole, stubBody string) *Sidecar {
	t.Helper()

	bus := sidecartest.NewIsolated(t, "")
	if rootAgentName != "" {
		if err := bus.DB.UpsertStatusSeedRootAgentName(
			sessionName, repo, "/tmp/"+repo, "active", nil, nil, rootAgentName, "", "",
		); err != nil {
			t.Fatalf("seed %s status: %v", rootAgentName, err)
		}
	}
	return newRoleGateSidecarWithDB(t, sessionName, repo, agentRole, stubBody, bus.DB)
}

// newRoleGateSidecarWithDB is newRoleGateSidecar with the DB handle supplied
// by the caller, so a test can pass nil to exercise the no-DB path.
func newRoleGateSidecarWithDB(t *testing.T, sessionName, repo, agentRole, stubBody string, d *db.DB) *Sidecar {
	t.Helper()

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return New(Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        t.TempDir(),
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       agentRole,
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
		Logger:          log.New(os.Stderr, "", 0),
	})
}

// ── /investigate ─────────────────────────────────────────────────────────────

// TestHostAPI_Investigate_DeniesWorker is the headline security assertion of
// #2588: a worker calling /investigate gets 403 and no child is executed.
// Before the fix the handler ran requirePost, decoded the body, and shelled
// out to `prism investigate` for any caller, so the restriction documented in
// coordinator.md and the prism skill had no enforcement point.
func TestHostAPI_Investigate_DeniesWorker(t *testing.T) {
	sc := newRoleGateSidecar(t,
		"prism-test-rolegate@investigate-worker", "prism-test-rolegate",
		"worker", "worker", failingStub)

	rr := doHostAPI(t, sc, http.MethodPost, "/investigate", `{"prompt":"look into this"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, rr.Body.String())
	}
	if !strings.Contains(resp["error"], "investigate") {
		t.Errorf("error = %q, want it to name the refused operation", resp["error"])
	}
}

// TestHostAPI_Investigate_AllowsCoordinator is the other half of the gate: a
// coordinator is unaffected and still receives the spawned session name. This
// is what stops the fix from being "deny everyone".
func TestHostAPI_Investigate_AllowsCoordinator(t *testing.T) {
	const wantSessionName = "prism-test-rolegate@main~investigate-abc123"
	stubBody := "#!/bin/sh\n" +
		"printf '%s' " + shellSingleQuote(wantSessionName+"\n") + "\n" +
		"exit 0\n"
	sc := newRoleGateSidecar(t,
		"prism-test-rolegate@main", "prism-test-rolegate",
		"coordinator", "coordinator", stubBody)

	rr := doHostAPI(t, sc, http.MethodPost, "/investigate", `{"prompt":"look into this"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, rr.Body.String())
	}
	if resp["session_name"] != wantSessionName {
		t.Errorf("session_name = %q, want %q", resp["session_name"], wantSessionName)
	}
}

// TestHostAPI_Investigate_DeniesUndeterminableRole covers the fail-closed
// property: when the DB cannot answer "is this session a coordinator?", the
// caller is treated as a worker. Each case uses a session name that does not
// end in @main, so the name heuristic cannot rescue the lookup either.
func TestHostAPI_Investigate_DeniesUndeterminableRole(t *testing.T) {
	cases := []struct {
		name string
		// build returns a sidecar whose role cannot be resolved.
		build func(t *testing.T) *Sidecar
	}{
		{
			// No agent_status row for the calling session at all.
			name: "no status row",
			build: func(t *testing.T) *Sidecar {
				return newRoleGateSidecar(t,
					"prism-test-rolegate@no-row", "prism-test-rolegate",
					"", "", failingStub)
			},
		},
		{
			// Pre-migration row: present, but root_agent_name is NULL.
			name: "null root_agent_name",
			build: func(t *testing.T) *Sidecar {
				bus := sidecartest.NewIsolated(t, "")
				const sess = "prism-test-rolegate@null-root"
				if err := bus.DB.UpsertStatus(sess, "prism-test-rolegate",
					"/tmp/prism-test-rolegate", "active", nil, nil); err != nil {
					t.Fatalf("seed pre-migration status: %v", err)
				}
				return newRoleGateSidecarWithDB(t, sess, "prism-test-rolegate",
					"", failingStub, bus.DB)
			},
		},
		{
			// No DB handle: the lookup cannot run at all.
			name: "no db handle",
			build: func(t *testing.T) *Sidecar {
				return newRoleGateSidecarWithDB(t,
					"prism-test-rolegate@no-db", "prism-test-rolegate",
					"", failingStub, nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := tc.build(t)
			rr := doHostAPI(t, sc, http.MethodPost, "/investigate", `{"prompt":"look into this"}`)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
			}
		})
	}
}

// ── the whole coordinator-only surface ───────────────────────────────────────

// TestHostAPI_CoordinatorOnly_DeniesWorker pins the "coordinator only" half of
// the route list in the hostAPIHandler doc comment: every endpoint labelled
// that way must refuse a worker with 403. An endpoint added to the list
// without a requireCoordinator call fails here.
//
// /checkin is deliberately absent. Issue #2587 replaced its single
// coordinator-only rule with a three-tier model, so it is documented
// "role-scoped" rather than "coordinator only" and does not belong in this
// list. Its gate is pinned in checkin_permission_test.go.
func TestHostAPI_CoordinatorOnly_DeniesWorker(t *testing.T) {
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/spawn", `{"branch":"feature-x","prompt":"do the thing"}`},
		{http.MethodPost, "/cleanup", `{"session":"prism-test-rolegate@feature-x","yes":true}`},
		{http.MethodPost, "/close", `{"session":"prism-test-rolegate@feature-x"}`},
		{http.MethodPost, "/merge", `{"pr":1}`},
		{http.MethodGet, "/merges", ""},
		{http.MethodPost, "/merges/cancel", `{"pr":1}`},
		{http.MethodPost, "/investigate", `{"prompt":"look into this"}`},
		{http.MethodGet, "/logs?session=prism-test-rolegate@feature-x", ""},
		{http.MethodGet, "/db/query?sql=SELECT+1", ""},
		{http.MethodGet, "/db/schema", ""},
		{http.MethodGet, "/db/tables", ""},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			sc := newRoleGateSidecar(t,
				"prism-test-rolegate@feature-x", "prism-test-rolegate",
				"worker", "worker", failingStub)

			rr := doHostAPI(t, sc, tc.method, tc.path, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s %s: status = %d, body = %q, want 403",
					tc.method, tc.path, rr.Code, truncateForLog(rr.Body.String(), 200))
			}
		})
	}
}
