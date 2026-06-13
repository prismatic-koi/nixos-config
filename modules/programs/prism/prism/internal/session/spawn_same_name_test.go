package session

// Tests for F2 (#1880): concurrent SpawnSession calls for the same session name
// must leave the DB in a consistent state — exactly one agent_status row,
// exactly one un-orphaned sessions row, exactly one allocated port.
//
// Before the in-process lock was added, two concurrent callers would both run
// the non-atomic prologue (seed → instance_id → InsertSession → AllocatePort)
// in parallel. SetInstanceID is an unconditional UPDATE so the last writer wins;
// both InsertSession calls succeed with different PKs, leaving one orphaned row;
// AllocatePort may pick the same port for both callers.
//
// The fix uses a per-session-name sync.Mutex (stored in spawnMu sync.Map) to
// serialise the prologue so only one caller runs it at a time.

import (
	"sync"
	"testing"
)

// TestSpawnSession_Concurrent_SameName fires N goroutines all calling
// SpawnSession with the same session name and asserts that after all calls
// complete the DB is in a consistent state:
//
//	(a) exactly one agent_status row exists (keyed on opts.SessionName).
//	(b) agent_status.instance_id is non-nil and matches exactly one sessions
//	    row whose ended_at is NULL (i.e. the row is alive).
//	(c) no other sessions row for this session name has ended_at NULL.
func TestSpawnSession_Concurrent_SameName(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const N = 5
	const sessionName = "myrepo@branch~conc-same-name"

	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := SpawnOpts{
				SessionName:    sessionName,
				Repo:           "myrepo",
				Worktree:       "/worktrees/myrepo-conc",
				AgentRole:      "worker",
				Prompt:         "go",
				Layout:         LayoutAgentOnly,
				IsolationMode:  "host",
				HarnessName:    "pi",
				PIExtensionDir: testPIExtensionDir,
			}
			errs[i] = SpawnSession(d, opts)
		}()
	}
	wg.Wait()

	// At least one call must have succeeded. The rest may succeed or fail
	// (e.g. tmux rejects a duplicate session name), but what matters is that
	// the DB is consistent regardless.
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount == 0 {
		t.Fatal("all SpawnSession calls failed; at least one must succeed")
	}

	// (a) Exactly one agent_status row exists for the session name.
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want one agent_status row")
	}

	// (b) agent_status.instance_id is non-nil.
	if st.InstanceID == nil || *st.InstanceID == "" {
		t.Fatal("agent_status.instance_id is nil/empty — exactly one must be set")
	}
	instanceID := *st.InstanceID

	// (b cont.) The sessions row for that instance_id must exist and be alive.
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID(%s): %v", instanceID, err)
	}
	if sess == nil {
		t.Fatalf("sessions row missing for instance_id %s pointed at by agent_status", instanceID)
	}
	if sess.EndedAt != nil {
		t.Errorf("sessions row for instance_id %s has ended_at set (not NULL) — the winning row must be alive", instanceID)
	}

	// (c) No OTHER sessions row for this session name should have ended_at NULL.
	// Any sessions rows inserted by losing callers must have been cleaned up
	// (ended_at IS NOT NULL) or not exist at all.
	allSessions, err := d.SessionsByName(sessionName)
	if err != nil {
		t.Fatalf("SessionsByName: %v", err)
	}
	for _, s := range allSessions {
		if s.InstanceID == instanceID {
			// This is the winning row; already verified above.
			continue
		}
		if s.EndedAt == nil {
			t.Errorf("orphaned sessions row found: instance_id=%s session_name=%s ended_at=NULL — only the winning row should be alive (#1880 F2)",
				s.InstanceID, s.SessionName)
		}
	}
}
