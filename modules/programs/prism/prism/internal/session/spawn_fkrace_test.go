package session

// Regression tests for issue #1507 — concurrent `prism spawn` invocations
// produced two distinct races in the spawn pipeline. This file covers the
// FK-constraint race ("Symptom 1" in the issue):
//
//   The sidecar mints an instance_id, immediately starts processing inbound
//   frames, and writes events with that instance_id — but the corresponding
//   row in the `sessions` table has not been inserted yet. Every
//   agent_events insert fails with FOREIGN KEY constraint failed (787),
//   the agent runs for ~30s with all events dropped, and the sidecar
//   eventually dies on extension reconnect-timeout.
//
// The fix lives in SpawnSession: the host-side spawn driver now mints
// instance_id and inserts the sessions row synchronously, before tmux/
// sidecar are kicked off. The sidecar's first writeEvent therefore always
// finds the FK target in place.
//
// These tests reproduce the failure mode deterministically without spinning
// up a real sidecar — they exercise the same DB-state invariants the sidecar
// relies on. They fail on the pre-#1507 code (no sessions row pre-insert)
// and pass after the fix.

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestSpawnSession_FullLayout_PreInsertsSessionsRow is the primary deterministic
// regression test for the FK-constraint race (#1507 Symptom 1). It exercises
// the LayoutFull spawn path — the path used by `prism spawn` — and verifies
// the post-condition the sidecar relies on: by the time SpawnSession returns
// (or, more precisely, by the time SpawnSession kicks off the sidecar), a
// `sessions` row exists keyed on the same instance_id that ends up on
// agent_status.
//
// On the pre-#1507 code path, only LayoutAgentOnly pre-inserted the sessions
// row, and even then only when the caller pre-populated InstanceID. LayoutFull
// deferred the insert to the tmux-session-start hook, which races with the
// sidecar. This test would fail (no sessions row → next assertion that we can
// write an event without FK error fails) on that code.
func TestSpawnSession_FullLayout_PreInsertsSessionsRow(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@fk-race-fullspawn"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-fk",
		AgentRole:     "worker",
		Prompt:        "go",
		Layout:        LayoutFull,
		IsolationMode: "host",
		HarnessName:   "pi",
		// ReadinessTimeout=0: skip the readiness gate. We are testing the
		// pre-spawn DB-state invariants, not the readiness path.,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	// Post-condition 1: agent_status.instance_id is set (host-side mint).
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil || st.InstanceID == nil || *st.InstanceID == "" {
		t.Fatalf("agent_status row for %q has no instance_id (got %+v) — host-side mint did not run", sessionName, st)
	}
	instanceID := *st.InstanceID

	// Post-condition 2: sessions row exists for that instance_id.
	// On the pre-fix code path this row would not exist yet (it would be
	// created later by the tmux-session-start hook racing with the sidecar).
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID(%s): %v", instanceID, err)
	}
	if sess == nil {
		t.Fatalf("sessions row missing for instance_id %s — FK race fix did not run", instanceID)
	}
	if sess.SessionName != sessionName {
		t.Errorf("sessions.session_name = %q, want %q", sess.SessionName, sessionName)
	}

	// Post-condition 3: agent_events insert keyed on instance_id succeeds.
	// This is the failure the sidecar logged hundreds of times in the issue
	// repro: "WriteEvent(state_change) failed: ... FOREIGN KEY constraint
	// failed (787)". After the fix it must succeed.
	iid := instanceID
	evt := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-fk",
		InstanceID:  &iid,
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
	}
	if err := d.WriteEvent(evt); err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
			t.Fatalf("WriteEvent FK-failed for instance_id %s — this is exactly the #1507 Symptom 1 race: %v", instanceID, err)
		}
		t.Fatalf("WriteEvent: %v", err)
	}
}

// TestSpawnSession_FullLayout_HonoursPreSetInstanceID verifies that when the
// caller pre-populates opts.InstanceID, SpawnSession threads that value
// through unchanged — i.e. it does not mint a fresh UUID and clobber the
// caller's value. This guards against a regression where the host-side mint
// short-circuits the existing pre-mint plumbing in test code (#1496/#1506)
// or in any future caller that wants to know the instance_id up-front.
func TestSpawnSession_FullLayout_HonoursPreSetInstanceID(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	preMinted := uuid.New().String()
	const sessionName = "myrepo@fk-race-preset"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-preset",
		AgentRole:     "worker",
		Prompt:        "go",
		InstanceID:    preMinted,
		Layout:        LayoutFull,
		IsolationMode: "host",
		HarnessName:   "pi",
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	st, _ := d.CurrentStatus(sessionName)
	if st == nil || st.InstanceID == nil {
		t.Fatalf("agent_status missing or has no instance_id: %+v", st)
	}
	if *st.InstanceID != preMinted {
		t.Errorf("agent_status.instance_id = %q, want pre-minted %q", *st.InstanceID, preMinted)
	}
	sess, _ := d.SessionByInstanceID(preMinted)
	if sess == nil {
		t.Fatalf("sessions row missing for pre-minted instance_id %s", preMinted)
	}
}

// TestSpawnSession_FullLayout_ConcurrentSpawnsAllPreSeeded is the high-stakes
// regression test for the Symptom 1 trigger condition: concurrent spawns
// from the same coordinator produced FK errors because each sidecar's
// instance_id mint raced against the tmux-session-start hook. With the fix,
// every concurrent SpawnSession invocation owns its instance_id + sessions
// row insert synchronously, so all N spawns can write events keyed on their
// respective instance_id without FK failures.
//
// AC-1 ("≥5 concurrent spawns produce no FK errors") is exercised here at
// the SpawnSession layer rather than at the host-API layer; the race the
// issue describes lives in SpawnSession's sequencing, so reproducing it at
// this level is sufficient and avoids the need to spin up a real host-API
// server in unit tests.
func TestSpawnSession_FullLayout_ConcurrentSpawnsAllPreSeeded(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const N = 6 // covers AC-1's "≥5"
	var wg sync.WaitGroup
	errs := make([]error, N)
	names := make([]string, N)
	for i := 0; i < N; i++ {
		i := i
		names[i] = "myrepo@fk-race-conc-" + uuid.New().String()[:8]
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := SpawnOpts{
				SessionName:   names[i],
				Repo:          "myrepo",
				Worktree:      "/worktrees/myrepo-conc-" + names[i],
				AgentRole:     "worker",
				Prompt:        "go",
				Layout:        LayoutFull,
				IsolationMode: "host",
				HarnessName:   "pi",
				PIExtensionDir: testPIExtensionDir,
			}
			errs[i] = SpawnSession(d, opts)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("SpawnSession[%d] (%s): %v", i, names[i], err)
		}
	}
	if t.Failed() {
		return
	}

	// Every session must have a sessions row pre-inserted, and an event
	// write keyed on its instance_id must succeed (no FK failure).
	for _, name := range names {
		st, err := d.CurrentStatus(name)
		if err != nil || st == nil || st.InstanceID == nil {
			t.Errorf("post-spawn: missing instance_id for %q (status=%+v err=%v)", name, st, err)
			continue
		}
		iid := *st.InstanceID
		sess, _ := d.SessionByInstanceID(iid)
		if sess == nil {
			t.Errorf("post-spawn: missing sessions row for %q (instance_id=%s)", name, iid)
			continue
		}
		evt := db.Event{
			ID:          uuid.New().String(),
			SessionName: name,
			Repo:        "myrepo",
			Worktree:    "/worktrees/myrepo-conc-" + name,
			InstanceID:  &iid,
			Type:        "state_change",
			Payload:     `{"state":"active"}`,
		}
		if err := d.WriteEvent(evt); err != nil {
			t.Errorf("WriteEvent for %q (instance_id=%s) failed: %v", name, iid, err)
		}
	}
}
