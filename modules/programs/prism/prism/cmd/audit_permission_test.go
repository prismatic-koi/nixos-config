package cmd

// Tests for the direct-CLI coordinator guard on `prism audit` (issue #2627).
//
// The host API gates the proxy route — GET /audit calls requireCoordinator —
// which covers every sandboxed caller. A session in `host` isolation mode has
// no socket, so fetchAuditEvents skips the proxy branch and reaches
// fetchAuditEventsLocal. Before this fix that path had no role check at all,
// so a host-mode worker read every session's audit rows.
//
// These tests mirror cmd/merges_coordinator_guard_test.go: refusal for every
// non-coordinator role, admission for a coordinator (including the
// --days/--pattern/--limit/--json/session-argument/no-results surface), and
// explicit fail-closed behaviour on an unresolvable caller. The proxy-route
// helpers (openStatsTestDB, newAuditFlagsCmd, startAuditProxyServer) come
// from cmd/stats_test.go and cmd/audit_proxy_test.go.
//
// Isolation: TestMain neutralises $TMUX / $TMUX_TMPDIR suite-wide, so
// review.LookupParentSession cannot reach the developer's live tmux server.
// Session names carry a "prism-test" prefix.

import (
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

const (
	auditGuardRepo        = "prism-test-audit"
	auditGuardCoordinator = "prism-test-audit@main"
	auditGuardWorker      = "prism-test-audit@guard-worker"
	auditGuardCoordInst   = "inst-coord-2627"
	auditGuardWorkerInst  = "inst-worker-2627"
)

// seedAuditGuardSession inserts an agent_status row for the given session
// against d, with an explicit root_agent_name, an instance_id, and isolation
// mode `host` — the mode that reaches the direct CLI path because it carries
// no host-API socket.
func seedAuditGuardSession(t *testing.T, d *db.DB, sessionName, rootAgent, instanceID string) {
	t.Helper()
	if err := d.UpsertStatusSeedRootAgentName(
		sessionName, auditGuardRepo, "/worktree/"+rootAgent, "idle",
		nil, nil, rootAgent, "", "host",
	); err != nil {
		t.Fatalf("seedAuditGuardSession(%q): UpsertStatusSeedRootAgentName: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("seedAuditGuardSession(%q): SetInstanceID: %v", sessionName, err)
	}
}

// ── refusal on the direct path ────────────────────────────────────────────────

// TestRunAudit_HostModeWorker_Refused is the headline assertion of #2627: a
// worker in host isolation mode — the exact session shape that carries no
// host-API socket and therefore never meets the host-API gate — is refused
// on the direct path.
//
// Before the fix this call returned nil and printed the audit table.
func TestRunAudit_HostModeWorker_Refused(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditGuardSession(t, d, auditGuardWorker, "worker", auditGuardWorkerInst)

	t.Setenv("PRISM_SESSION_NAME", auditGuardWorker)
	t.Setenv("TMUX", "")

	c := newAuditFlagsCmd(t)
	err := runAudit(c, nil)
	if err == nil {
		t.Fatal("runAudit returned nil for a host-mode worker, want a refusal (issue #2627)")
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("error = %q, want it to name the role requirement (\"coordinator sessions only\")", err.Error())
	}
	if !strings.Contains(err.Error(), auditGuardWorker) {
		t.Errorf("error = %q, want it to name the refused caller %q", err.Error(), auditGuardWorker)
	}
}

// TestRunAudit_NonCoordinatorRoles_Refused widens the refusal to the other
// non-coordinator roles reachable in host mode: a review agent and an
// investigator.
func TestRunAudit_NonCoordinatorRoles_Refused(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		rootAgent string
	}{
		{
			name:      "review agent",
			session:   "prism-test-audit@main~review-1-review-code",
			rootAgent: "review-code",
		},
		{
			name:      "investigator",
			session:   "prism-test-audit@main~investigate-audit",
			rootAgent: "investigate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openStatsTestDB(t)
			seedAuditGuardSession(t, d, tc.session, tc.rootAgent, "inst-"+tc.rootAgent)

			t.Setenv("PRISM_SESSION_NAME", tc.session)
			t.Setenv("TMUX", "")

			c := newAuditFlagsCmd(t)
			err := runAudit(c, nil)
			if err == nil {
				t.Errorf("runAudit(%q) returned nil, want a refusal (issue #2627)", tc.session)
			} else if !strings.Contains(err.Error(), "coordinator sessions only") {
				t.Errorf("runAudit error = %q, want it to name the role requirement", err.Error())
			}
		})
	}
}

// ── admission on the direct path ──────────────────────────────────────────────

// TestRunAudit_HostModeCoordinator_Admitted is the other half of the gate: a
// coordinator in host isolation mode still reads the audit trail. This is
// what stops the fix from being "deny everyone", and exercises --days,
// --pattern, --limit, --json, a session-name argument, and the no-results
// output, per the #2627 acceptance criteria.
func TestRunAudit_HostModeCoordinator_Admitted(t *testing.T) {
	d := openStatsTestDB(t)
	seedAuditGuardSession(t, d, auditGuardCoordinator, "coordinator", auditGuardCoordInst)

	t.Setenv("PRISM_SESSION_NAME", auditGuardCoordinator)
	t.Setenv("TMUX", "")

	t.Run("no-results output", func(t *testing.T) {
		c := newAuditFlagsCmd(t)
		out := captureStdout(t, func() {
			if err := runAudit(c, nil); err != nil {
				t.Fatalf("runAudit for a host-mode coordinator: %v", err)
			}
		})
		if !strings.Contains(out, "no audit events found") {
			t.Errorf("stdout = %q, want the no-results message", out)
		}
	})

	writeAuditTestEvent(t, d, auditGuardCoordinator, "audit",
		`{"command":"git push","sessionName":"`+auditGuardCoordinator+`"}`, time.Now())

	t.Run("flags and session argument", func(t *testing.T) {
		c := newAuditFlagsCmd(t)
		setAuditFlags(t, c, map[string]string{
			"days":    "7",
			"pattern": "git push",
			"limit":   "5",
			"json":    "true",
		})
		out := captureStdout(t, func() {
			if err := runAudit(c, []string{auditGuardCoordinator}); err != nil {
				t.Fatalf("runAudit with flags for a host-mode coordinator: %v", err)
			}
		})
		if !strings.Contains(out, `"events"`) {
			t.Errorf("stdout = %q, want a JSON events envelope", out)
		}
		if !strings.Contains(out, "git push") {
			t.Errorf("stdout = %q, want the seeded event's command", out)
		}
	})
}

// ── unresolvable caller ───────────────────────────────────────────────────────

// TestRunAudit_UnresolvableCaller_FailsClosed makes the unresolvable-caller
// behaviour explicit, per the #2627 acceptance criteria: a non-zero exit,
// naming PRISM_SESSION_NAME, before QueryAuditEvents is ever reached.
//
// PRISM_SESSION_NAME is empty and the suite-wide tmux neutralisation makes
// tmux.CurrentSession() fail, so review.LookupParentSession returns "".
func TestRunAudit_UnresolvableCaller_FailsClosed(t *testing.T) {
	openStatsTestDB(t)

	t.Setenv("PRISM_SESSION_NAME", "")
	t.Setenv("TMUX", "")

	c := newAuditFlagsCmd(t)
	err := runAudit(c, nil)
	if err == nil {
		t.Fatal("runAudit returned nil for an unresolvable caller, want a non-zero exit")
	}
	if !strings.Contains(err.Error(), "PRISM_SESSION_NAME") {
		t.Errorf("error = %q, want it to name PRISM_SESSION_NAME", err.Error())
	}
	if !strings.Contains(err.Error(), "cannot determine caller session") {
		t.Errorf("error = %q, want the identity error (\"cannot determine caller session\")", err.Error())
	}
}

// ── the proxy route stays unchanged ───────────────────────────────────────────

// TestRunAudit_SandboxedWorker_StillProxies is the edge case that keeps the
// fix from changing the sandboxed contract: when the host-API socket is set,
// the command must still route to GET /audit rather than being refused
// locally by the new guard. The caller here is a worker — exactly the role
// the guard refuses on the direct path — so a guard placed before the proxy
// branch would fail this test.
//
// The refusal for that route belongs to the host API, which answers HTTP 403
// — pinned separately in internal/sidecar/host_api_audit_test.go. The fake
// server here (startAuditProxyServer, from audit_proxy_test.go) delegates to
// db.QueryAuditEvents with no role check of its own, so a request reaching it
// at all is the evidence that the CLI guard did not preempt the proxy branch.
func TestRunAudit_SandboxedWorker_StillProxies(t *testing.T) {
	d := openStatsTestDB(t)
	srv, apiURL := startAuditProxyServer(t, d)

	seedAuditGuardSession(t, d, auditGuardWorker, "worker", auditGuardWorkerInst)
	t.Setenv("PRISM_SESSION_NAME", auditGuardWorker)
	t.Setenv("TMUX", "")
	t.Setenv("PRISM_HOST_API", apiURL)

	c := newAuditFlagsCmd(t)
	var runErr error
	_ = captureStdout(t, func() { runErr = runAudit(c, nil) })
	if runErr != nil {
		t.Fatalf("runAudit over the proxy route: %v — the CLI guard must not preempt the proxy branch", runErr)
	}

	if srv.requests == 0 {
		t.Fatal("server received no requests — the CLI guard preempted the proxy route")
	}
	if srv.capturedPath != "/audit" {
		t.Errorf("first request path = %q, want /audit", srv.capturedPath)
	}
}

// ── the guard helper in isolation ─────────────────────────────────────────────

// TestRequireAuditCoordinator_KeysOnCallerSession pins the unit-level
// contract of the guard helper: the verdict is a function of the caller
// session name and the DB, matching requireMergesCoordinator.
func TestRequireAuditCoordinator_KeysOnCallerSession(t *testing.T) {
	cases := []struct {
		caller    string
		wantAdmit bool
	}{
		{"prism-test-audit@main", true},
		{"prism-test-audit@worker-branch", false},
		{"prism-test-audit@main~investigate-audit", false},
		{"prism-test-audit@main~review-1-review-code", false},
		{"", false},
	}
	for _, tc := range cases {
		name := tc.caller
		if name == "" {
			name = "empty caller"
		}
		t.Run(name, func(t *testing.T) {
			err := requireAuditCoordinator(tc.caller, nil)
			if tc.wantAdmit && err != nil {
				t.Errorf("requireAuditCoordinator(%q, nil) = %v, want nil (admit)", tc.caller, err)
			}
			if !tc.wantAdmit && err == nil {
				t.Errorf("requireAuditCoordinator(%q, nil) = nil, want a refusal", tc.caller)
			}
		})
	}
}
