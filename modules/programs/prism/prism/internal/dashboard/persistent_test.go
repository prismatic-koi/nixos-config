package dashboard_test

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/dashboard"
)

// TestPersistentInit_SchedulesSessionSyncTick verifies that PersistentModel.Init
// returns a non-nil command batch that includes at least 5 commands:
// FetchSessionsFromDB, FetchGitHubStats, GhTick, GitStatTick, SessionSyncTick.
func TestPersistentInit_SchedulesSessionSyncTick(t *testing.T) {
	m := dashboard.NewPersistentModel("", "")
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned nil cmd; expected a batch of commands")
	}

	// Execute the cmd to get the batch message.
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() cmd produced %T, want tea.BatchMsg", msg)
	}

	// The batch must contain at least 5 commands:
	//   FetchSessionsFromDB, FetchGitHubStats, GhTick, GitStatTick, SessionSyncTick
	if len(batch) < 5 {
		t.Errorf("Init() batch has %d cmds, want >= 5 (including SessionSyncTick)", len(batch))
	}

	for _, c := range batch {
		if c == nil {
			t.Error("Init() batch contains a nil cmd")
		}
	}
}

// TestPersistentUpdate_SessionSyncTickMsg_HandledGracefully verifies that
// receiving a SessionSyncTickMsg causes Update to return a non-nil batch cmd
// with at least two commands (FetchSessionsFromDB + the next SessionSyncTick),
// and that one of those cmds produces a SessionsMsg when executed.
//
// The DB is pointed at a nonexistent path so FetchSessionsFromDB returns
// SessionsMsg{} without needing a real database.
func TestPersistentUpdate_SessionSyncTickMsg_HandledGracefully(t *testing.T) {
	// Use a fake DB path so openDB fails cleanly — FetchSessionsFromDB will
	// return an empty SessionsMsg rather than querying a real DB.
	dashboard.SetTestDBPath("/nonexistent/path/prism.db")
	t.Cleanup(func() { dashboard.SetTestDBPath("") })

	m := dashboard.NewPersistentModel("", "")

	// Send a SessionSyncTickMsg to Update.
	updatedModel, cmd := m.Update(dashboard.SessionSyncTickMsg(time.Now()))

	if updatedModel == nil {
		t.Fatal("Update returned nil model")
	}
	if cmd == nil {
		t.Fatal("Update(SessionSyncTickMsg) returned nil cmd; expected a batch to re-fetch and re-schedule")
	}

	// Execute the returned cmd — it should be a batch (FetchSessionsFromDB + SessionSyncTick).
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Update(SessionSyncTickMsg) cmd produced %T, want tea.BatchMsg", msg)
	}
	if len(batch) < 2 {
		t.Errorf("SessionSyncTickMsg handler batch has %d cmds, want >= 2 (FetchSessionsFromDB + SessionSyncTick)", len(batch))
	}

	// Execute only the non-tick cmds looking for a SessionsMsg.
	// We identify the DB-fetch cmd by running each cmd with a very short
	// deadline: the tick cmd will block for 10s, so we skip any cmd that
	// doesn't return quickly by only running cmds that we know are the DB fetch.
	// The safe approach: run all cmds, but only the one that returns
	// synchronously (FetchSessionsFromDB) produces a SessionsMsg immediately.
	// We collect results using a channel with a short timeout.
	var gotSessionsMsg bool
	resultCh := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		if c == nil {
			t.Error("SessionSyncTickMsg handler batch contains a nil cmd")
			continue
		}
		go func(fn tea.Cmd) {
			resultCh <- fn()
		}(c)
	}

	// Collect results with a 2-second timeout (FetchSessionsFromDB returns quickly;
	// the tick cmd blocks for 10s and we don't wait for it).
	deadline := time.After(2 * time.Second)
	for i := 0; i < len(batch); i++ {
		select {
		case msg := <-resultCh:
			if _, isSessionsMsg := msg.(dashboard.SessionsMsg); isSessionsMsg {
				gotSessionsMsg = true
			}
		case <-deadline:
			// Timeout: remaining cmds are slow tickers; stop collecting.
			i = len(batch) // break loop
		}
	}

	if !gotSessionsMsg {
		t.Error("SessionSyncTickMsg handler did not produce a SessionsMsg from FetchSessionsFromDB")
	}
}

// TestPersistentUpdate_SessionSyncTickMsg_RetainsSessionsOnDBError verifies
// that when the DB is unreachable during a session sync tick, the model retains
// its last-known sessions rather than clearing them. FetchSessionsFromDB returns
// SessionsMsg{} with nil Sessions on error; ApplySessionsMsg guards nil so the
// existing session list is preserved.
func TestPersistentUpdate_SessionSyncTickMsg_RetainsSessionsOnDBError(t *testing.T) {
	// Point the DB at a nonexistent path so FetchSessionsFromDB always errors.
	dashboard.SetTestDBPath("/nonexistent/path/prism.db")
	t.Cleanup(func() { dashboard.SetTestDBPath("") })

	m := dashboard.NewPersistentModel("", "")

	// Simulate a prior successful DB fetch by applying a SessionsMsg with known sessions.
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@main"},
		{Name: "nixos-config@feature"},
	}
	m2, _ := m.Update(dashboard.SessionsMsg{Sessions: sessions})
	pm, ok := m2.(dashboard.PersistentModel)
	if !ok {
		t.Fatalf("Update returned %T, want dashboard.PersistentModel", m2)
	}
	if len(pm.Sessions) != 2 {
		t.Fatalf("pre-condition: expected 2 sessions, got %d", len(pm.Sessions))
	}

	// Fire a SessionSyncTickMsg — the DB query will fail (nonexistent path).
	_, cmd := pm.Update(dashboard.SessionSyncTickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("Update(SessionSyncTickMsg) returned nil cmd")
	}

	// Execute the batch and collect results with a timeout to avoid blocking on
	// the next-tick timer cmd.
	batchMsg := cmd()
	batch, ok := batchMsg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected tea.BatchMsg, got %T", batchMsg)
	}

	resultCh := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		if c == nil {
			continue
		}
		go func(fn tea.Cmd) {
			resultCh <- fn()
		}(c)
	}

	deadline := time.After(2 * time.Second)
	for i := 0; i < len(batch); i++ {
		select {
		case innerMsg := <-resultCh:
			if sm, isSM := innerMsg.(dashboard.SessionsMsg); isSM {
				// Apply the (empty) SessionsMsg to the model.
				m3, _ := pm.Update(sm)
				pm2, ok2 := m3.(dashboard.PersistentModel)
				if !ok2 {
					t.Fatalf("Update returned %T, want PersistentModel", m3)
				}
				// Sessions must not have been cleared — ApplySessionsMsg guards nil.
				if len(pm2.Sessions) != 2 {
					t.Errorf("DB error on sync tick cleared sessions: got %d, want 2", len(pm2.Sessions))
				}
			}
		case <-deadline:
			i = len(batch) // break loop
		}
	}
}
