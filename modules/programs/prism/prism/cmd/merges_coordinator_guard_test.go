package cmd

// Tests for the direct-CLI coordinator guard on `prism merges list` and
// `prism merges cancel`.
//
// The host API gates both verbs — `/merges` and `/merges/cancel` each call
// requireCoordinator — which covers every sandboxed caller, because a bwrap or
// sandbox-exec session always carries the host-API socket. A session in `host`
// isolation mode has no socket, so runMergesList / runMergesCancel skip the
// proxy branch and reach the direct DB path. Without a role check there, a
// host-mode worker reaches d.CancelMerge.
//
// These tests pin the guard on that path: refusal for every non-coordinator
// role, admission for a coordinator, explicit fail-closed behaviour on an
// unresolvable caller, and — the other half of the contract — the proxy route
// left untouched, so a sandboxed caller still routes to the host API and is
// refused there rather than locally.
//
// Every test drives runMergesList / runMergesCancel rather than the guard
// helper alone, so a guard that is present but unwired fails here. The
// mutation assertion (row still `watching`) is what proves the guard runs
// before d.CancelMerge and not merely before the success message.
//
// Isolation: openMergeTestDB points openDB() at a t.TempDir() database and
// clears PRISM_HOST_API; TestMain neutralises $TMUX / $TMUX_TMPDIR suite-wide
// (see tmux_isolation_test.go), so review.LookupParentSession cannot reach the
// developer's live tmux server. Session names carry a "prism-test" prefix so
// they can never collide with a live coordinator.

import (
	"strings"
	"testing"
)

const (
	mergesGuardRepo        = "prism-test-merges"
	mergesGuardCoordinator = "prism-test-merges@main"
	mergesGuardWorker      = "prism-test-merges@guard-worker"
	mergesGuardCoordInst   = "inst-coord-2608"
	mergesGuardWorkerInst  = "inst-worker-2608"
)

// seedMergesGuardSession inserts an agent_status row for the given session with
// an explicit root_agent_name, an instance_id, and isolation mode `host` — the
// mode that reaches the direct CLI path because it carries no host-API socket.
func seedMergesGuardSession(t *testing.T, sessionName, rootAgent, instanceID string) {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatalf("seedMergesGuardSession(%q): openDB: %v", sessionName, err)
	}
	defer d.Close()
	if err := d.UpsertStatusSeedRootAgentName(
		sessionName, mergesGuardRepo, "/worktree/"+rootAgent, "idle",
		nil, nil, rootAgent, "", "host",
	); err != nil {
		t.Fatalf("seedMergesGuardSession(%q): UpsertStatusSeedRootAgentName: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("seedMergesGuardSession(%q): SetInstanceID: %v", sessionName, err)
	}
}

// enqueueMergesGuardRow inserts a `watching` pending_merges row owned by the
// given session and instance_id.
func enqueueMergesGuardRow(t *testing.T, pr int, sessionName, instanceID string) {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatalf("enqueueMergesGuardRow(%d): openDB: %v", pr, err)
	}
	defer d.Close()
	if _, err := d.EnqueueMerge(pr, mergesGuardRepo, sessionName, instanceID, nil); err != nil {
		t.Fatalf("enqueueMergesGuardRow(%d): EnqueueMerge: %v", pr, err)
	}
}

// assertMergeStatus fails the test unless the pending_merges row for pr is
// present with the wanted status.
func assertMergeStatus(t *testing.T, pr int, want string) {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatalf("assertMergeStatus(%d): openDB: %v", pr, err)
	}
	defer d.Close()
	row, err := d.PendingMergeByPR(pr, mergesGuardRepo)
	if err != nil {
		t.Fatalf("assertMergeStatus(%d): PendingMergeByPR: %v", pr, err)
	}
	if row == nil {
		t.Fatalf("assertMergeStatus(%d): row is missing, want status %q", pr, want)
	}
	if row.Status != want {
		t.Errorf("pending_merges row for PR #%d has status %q, want %q", pr, row.Status, want)
	}
}

// ── refusal on the direct path ────────────────────────────────────────────────

// TestRunMergesCancel_HostModeWorker_Refused is the headline security
// assertion: a worker in host isolation mode — the exact session shape that
// carries no host-API socket and therefore never meets the host-API gate —
// is refused on the direct path.
//
// The queued row is deliberately owned by the WORKER's own instance_id, not
// the coordinator's. Instance scoping keeps the severity low: CancelMerge
// filters on instance_id, so a foreign session's cancel is normally a no-op.
// Seeding the row under the caller's own instance_id removes that mitigation,
// so the surviving `watching` status proves the guard runs before
// d.CancelMerge rather than relying on the scope filter to absorb the call.
func TestRunMergesCancel_HostModeWorker_Refused(t *testing.T) {
	openMergeTestDB(t)

	seedMergesGuardSession(t, mergesGuardWorker, "worker", mergesGuardWorkerInst)
	enqueueMergesGuardRow(t, 4242, mergesGuardWorker, mergesGuardWorkerInst)

	t.Setenv("PRISM_SESSION_NAME", mergesGuardWorker)
	t.Setenv("TMUX", "")

	err := runMergesCancel(mergesCancelCmd, []string{"4242"})
	if err == nil {
		t.Fatal("runMergesCancel returned nil for a host-mode worker, want a refusal (issue #2608)")
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("error = %q, want it to name the role requirement (\"coordinator sessions only\")", err.Error())
	}
	if !strings.Contains(err.Error(), mergesGuardWorker) {
		t.Errorf("error = %q, want it to name the refused caller %q", err.Error(), mergesGuardWorker)
	}

	// The row must be untouched: the guard precedes d.CancelMerge.
	assertMergeStatus(t, 4242, "watching")
}

// TestRunMergesList_HostModeWorker_Refused pins the same refusal on the
// read-only verb. The direct path is guarded there too so the role boundary of
// the verb does not depend on the caller's isolation mode — a bwrap worker
// already receives HTTP 403 from `/merges`.
func TestRunMergesList_HostModeWorker_Refused(t *testing.T) {
	openMergeTestDB(t)

	seedMergesGuardSession(t, mergesGuardWorker, "worker", mergesGuardWorkerInst)

	t.Setenv("PRISM_SESSION_NAME", mergesGuardWorker)
	t.Setenv("TMUX", "")

	err := runMergesList(mergesListCmd, nil)
	if err == nil {
		t.Fatal("runMergesList returned nil for a host-mode worker, want a refusal (issue #2608)")
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("error = %q, want it to name the role requirement (\"coordinator sessions only\")", err.Error())
	}
	if !strings.Contains(err.Error(), mergesGuardWorker) {
		t.Errorf("error = %q, want it to name the refused caller %q", err.Error(), mergesGuardWorker)
	}
}

// TestRunMergesGuard_NonCoordinatorRoles_Refused widens the refusal to the
// other non-coordinator roles reachable in host mode: a review agent and an
// investigator. Both verbs must refuse both roles.
func TestRunMergesGuard_NonCoordinatorRoles_Refused(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		rootAgent string
	}{
		{
			name:      "review agent",
			session:   "prism-test-merges@main~review-1-review-code",
			rootAgent: "review-code",
		},
		{
			name:      "investigator",
			session:   "prism-test-merges@main~investigate-merge-queue",
			rootAgent: "investigate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openMergeTestDB(t)
			seedMergesGuardSession(t, tc.session, tc.rootAgent, "inst-"+tc.rootAgent)
			enqueueMergesGuardRow(t, 4343, tc.session, "inst-"+tc.rootAgent)

			t.Setenv("PRISM_SESSION_NAME", tc.session)
			t.Setenv("TMUX", "")

			listErr := runMergesList(mergesListCmd, nil)
			if listErr == nil {
				t.Errorf("runMergesList(%q) returned nil, want a refusal (issue #2608)", tc.session)
			} else if !strings.Contains(listErr.Error(), "coordinator sessions only") {
				t.Errorf("runMergesList error = %q, want it to name the role requirement", listErr.Error())
			}

			cancelErr := runMergesCancel(mergesCancelCmd, []string{"4343"})
			if cancelErr == nil {
				t.Errorf("runMergesCancel(%q) returned nil, want a refusal (issue #2608)", tc.session)
			} else if !strings.Contains(cancelErr.Error(), "coordinator sessions only") {
				t.Errorf("runMergesCancel error = %q, want it to name the role requirement", cancelErr.Error())
			}
			assertMergeStatus(t, 4343, "watching")
		})
	}
}

// ── admission on the direct path ──────────────────────────────────────────────

// TestRunMergesList_HostModeCoordinator_Admitted is the other half of the
// gate: a coordinator in host isolation mode still lists the queue. This is
// what stops the guard from being "deny everyone".
func TestRunMergesList_HostModeCoordinator_Admitted(t *testing.T) {
	openMergeTestDB(t)

	seedMergesGuardSession(t, mergesGuardCoordinator, "coordinator", mergesGuardCoordInst)
	enqueueMergesGuardRow(t, 4444, mergesGuardCoordinator, mergesGuardCoordInst)

	t.Setenv("PRISM_SESSION_NAME", mergesGuardCoordinator)
	t.Setenv("TMUX", "")

	var listErr error
	out := captureStdout(t, func() {
		listErr = runMergesList(mergesListCmd, nil)
	})
	if listErr != nil {
		t.Fatalf("runMergesList for a host-mode coordinator: %v", listErr)
	}
	if !strings.Contains(out, "4444") {
		t.Errorf("stdout = %q, want it to list the coordinator's queued PR #4444", out)
	}
}

// TestRunMergesCancel_HostModeCoordinator_Cancels verifies that a coordinator
// in host isolation mode still cancels its own queued row: the guard admits
// the caller and the row reaches the terminal `cancelled` state.
func TestRunMergesCancel_HostModeCoordinator_Cancels(t *testing.T) {
	openMergeTestDB(t)

	seedMergesGuardSession(t, mergesGuardCoordinator, "coordinator", mergesGuardCoordInst)
	enqueueMergesGuardRow(t, 4545, mergesGuardCoordinator, mergesGuardCoordInst)

	t.Setenv("PRISM_SESSION_NAME", mergesGuardCoordinator)
	t.Setenv("TMUX", "")

	var cancelErr error
	out := captureStdout(t, func() {
		cancelErr = runMergesCancel(mergesCancelCmd, []string{"4545"})
	})
	if cancelErr != nil {
		t.Fatalf("runMergesCancel for a host-mode coordinator: %v", cancelErr)
	}
	if !strings.Contains(out, "removed from merge queue") {
		t.Errorf("stdout = %q, want the cancellation confirmation", out)
	}
	assertMergeStatus(t, 4545, "cancelled")
}

// TestRunMergesGuard_CoordinatorByRoleAlone_Admitted pins that admission does
// not depend on the "@main" name heuristic. The session name has a plain
// branch suffix; only the DB-backed root_agent_name = 'coordinator' admits it.
// A guard that keyed on the name alone would refuse a coordinator whose
// session is not named <repo>@main.
func TestRunMergesGuard_CoordinatorByRoleAlone_Admitted(t *testing.T) {
	openMergeTestDB(t)

	const coord = "prism-test-merges@coordinator-host-mode"
	seedMergesGuardSession(t, coord, "coordinator", "inst-coord-by-role")
	enqueueMergesGuardRow(t, 4646, coord, "inst-coord-by-role")

	t.Setenv("PRISM_SESSION_NAME", coord)
	t.Setenv("TMUX", "")

	var listErr, cancelErr error
	_ = captureStdout(t, func() {
		listErr = runMergesList(mergesListCmd, nil)
		cancelErr = runMergesCancel(mergesCancelCmd, []string{"4646"})
	})
	if listErr != nil {
		t.Errorf("runMergesList for a non-@main coordinator: %v", listErr)
	}
	if cancelErr != nil {
		t.Errorf("runMergesCancel for a non-@main coordinator: %v", cancelErr)
	}
	assertMergeStatus(t, 4646, "cancelled")
}

// ── unresolvable caller ───────────────────────────────────────────────────────

// TestRunMergesGuard_UnresolvableCaller_FailsClosed makes the unresolvable-
// caller behaviour explicit for each guarded verb: the cmd/merge.go shape
// fails closed on an unknown caller.
//
// Both verbs fail closed with a non-zero exit and no DB write. The message is
// the identity error, not the role error: "run from inside a prism session" is
// the actionable instruction for an operator with no session, and it is the
// message both verbs return from resolveCallerIdentity, so the guard does not
// change the error for this case.
//
// PRISM_SESSION_NAME is empty and the suite-wide tmux neutralisation makes
// tmux.CurrentSession() fail, so review.LookupParentSession returns "".
func TestRunMergesGuard_UnresolvableCaller_FailsClosed(t *testing.T) {
	openMergeTestDB(t)

	seedMergesGuardSession(t, mergesGuardCoordinator, "coordinator", mergesGuardCoordInst)
	enqueueMergesGuardRow(t, 4747, mergesGuardCoordinator, mergesGuardCoordInst)

	t.Setenv("PRISM_SESSION_NAME", "")
	t.Setenv("TMUX", "")

	t.Run("merges list", func(t *testing.T) {
		err := runMergesList(mergesListCmd, nil)
		if err == nil {
			t.Fatal("runMergesList returned nil for an unresolvable caller, want a non-zero exit")
		}
		if !strings.Contains(err.Error(), "cannot determine caller session") {
			t.Errorf("error = %q, want the identity error (\"cannot determine caller session\")", err.Error())
		}
		if !strings.Contains(err.Error(), "prism merges list") {
			t.Errorf("error = %q, want it to name the verb", err.Error())
		}
	})

	t.Run("merges cancel", func(t *testing.T) {
		err := runMergesCancel(mergesCancelCmd, []string{"4747"})
		if err == nil {
			t.Fatal("runMergesCancel returned nil for an unresolvable caller, want a non-zero exit")
		}
		if !strings.Contains(err.Error(), "cannot determine caller session") {
			t.Errorf("error = %q, want the identity error (\"cannot determine caller session\")", err.Error())
		}
		if !strings.Contains(err.Error(), "prism merges cancel") {
			t.Errorf("error = %q, want it to name the verb", err.Error())
		}
	})

	// No write reached the queue on either path.
	assertMergeStatus(t, 4747, "watching")
}

// ── the proxy route stays unchanged ───────────────────────────────────────────

// TestRunMergesGuard_SandboxedWorker_StillProxies is the edge case that keeps
// the guard from changing the sandboxed contract: when the host-API socket is
// set, both verbs must still route to `/merges` and `/merges/cancel` rather
// than being refused locally by the new guard. The caller here is a worker —
// exactly the role the guard refuses on the direct path — so a guard placed
// before the proxy branch would fail this test.
//
// The refusal for that route belongs to the host API, which answers HTTP 403;
// that half is pinned by TestHostAPI_CoordinatorOnly_DeniesWorker in
// internal/sidecar, which asserts 403 for both `/merges` and `/merges/cancel`.
// The fake server here is a transport stand-in only: it records the request so
// this test can assert the call left the CLI.
func TestRunMergesGuard_SandboxedWorker_StillProxies(t *testing.T) {
	t.Run("merges list", func(t *testing.T) {
		openMergeTestDB(t)
		server, apiURL := startFakeHostAPIServer(t)
		server.mu.Lock()
		server.merges = []map[string]any{}
		server.mu.Unlock()

		seedMergesGuardSession(t, mergesGuardWorker, "worker", mergesGuardWorkerInst)
		t.Setenv("PRISM_SESSION_NAME", mergesGuardWorker)
		t.Setenv("TMUX", "")
		t.Setenv("PRISM_HOST_API", apiURL)

		var listErr error
		_ = captureStdout(t, func() { listErr = runMergesList(mergesListCmd, nil) })
		if listErr != nil {
			t.Fatalf("runMergesList over the proxy route: %v — the CLI guard must not preempt the proxy branch", listErr)
		}

		server.mu.Lock()
		defer server.mu.Unlock()
		if len(server.requests) == 0 {
			t.Fatal("server received no requests — the CLI guard preempted the proxy route")
		}
		if server.requests[0].Path != "/merges" {
			t.Errorf("first request path = %q, want /merges", server.requests[0].Path)
		}
	})

	t.Run("merges cancel", func(t *testing.T) {
		openMergeTestDB(t)
		server, apiURL := startFakeHostAPIServer(t)
		server.mu.Lock()
		server.cancelOK = true
		server.mu.Unlock()

		seedMergesGuardSession(t, mergesGuardWorker, "worker", mergesGuardWorkerInst)
		t.Setenv("PRISM_SESSION_NAME", mergesGuardWorker)
		t.Setenv("TMUX", "")
		t.Setenv("PRISM_HOST_API", apiURL)

		var cancelErr error
		_ = captureStdout(t, func() { cancelErr = runMergesCancel(mergesCancelCmd, []string{"55"}) })
		if cancelErr != nil {
			t.Fatalf("runMergesCancel over the proxy route: %v — the CLI guard must not preempt the proxy branch", cancelErr)
		}

		server.mu.Lock()
		defer server.mu.Unlock()
		if len(server.requests) == 0 {
			t.Fatal("server received no requests — the CLI guard preempted the proxy route")
		}
		if server.requests[0].Path != "/merges/cancel" {
			t.Errorf("first request path = %q, want /merges/cancel", server.requests[0].Path)
		}
	})
}

// ── the guard helper in isolation ─────────────────────────────────────────────

// TestRequireMergesCoordinator_KeysOnCallerSession pins the unit-level
// contract of the guard helper: the verdict is a function of the caller
// session name and the DB, matching cmd/merge.go and
// requireInvestigateCoordinator. A nil DB exercises the name-heuristic
// fallback in isolation, which is the fail-closed path for an unknown role.
func TestRequireMergesCoordinator_KeysOnCallerSession(t *testing.T) {
	cases := []struct {
		caller    string
		wantAdmit bool
	}{
		{"prism-test-merges@main", true},
		{"prism-test-merges@worker-branch", false},
		{"prism-test-merges@main~investigate-merge-queue", false},
		{"prism-test-merges@main~review-1-review-code", false},
		{"", false},
	}
	for _, tc := range cases {
		name := tc.caller
		if name == "" {
			name = "empty caller"
		}
		t.Run(name, func(t *testing.T) {
			err := requireMergesCoordinator("prism merges list", "prism merges list", tc.caller, nil)
			if tc.wantAdmit && err != nil {
				t.Errorf("requireMergesCoordinator(%q, nil) = %v, want nil (admit)", tc.caller, err)
			}
			if !tc.wantAdmit && err == nil {
				t.Errorf("requireMergesCoordinator(%q, nil) = nil, want a refusal", tc.caller)
			}
		})
	}
}

// TestRequireMergesCoordinator_MessageNamesVerbAndSuggestion pins the two
// caller-supplied strings into the refusal text, so a `merges cancel` refusal
// tells the operator the exact command their coordinator must run.
func TestRequireMergesCoordinator_MessageNamesVerbAndSuggestion(t *testing.T) {
	err := requireMergesCoordinator("prism merges cancel", "prism merges cancel 77", mergesGuardWorker, nil)
	if err == nil {
		t.Fatal("requireMergesCoordinator returned nil for a worker caller, want a refusal")
	}
	for _, want := range []string{
		"prism merges cancel",
		"prism merges cancel 77",
		"coordinator sessions only",
		mergesGuardWorker,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}
