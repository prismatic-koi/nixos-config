package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// The cleanup of a failed spawn stops the sidecar with the stop that
// reports a result, before it ends the row. If the stop fails, the row and
// the port stay reserved (#3168).
func TestCleanupFailedSpawn_StopsSidecarFirst_3168(t *testing.T) {
	noop := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(noop, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write noop tmux: %v", err)
	}
	prevTmux := tmux.TmuxBin
	tmux.TmuxBin = noop
	t.Cleanup(func() { tmux.TmuxBin = prevTmux })

	for _, stopOK := range []bool{true, false} {
		d := openReapTestDB(t)
		const name = "prism-test@3168~review-1-review-qa"
		if err := d.UpsertStatus(name, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus: %v", err)
		}
		if _, err := d.AllocatePort(name); err != nil {
			t.Fatalf("AllocatePort: %v", err)
		}
		var stops int
		prev := stopFailedSpawnSidecar
		stopFailedSpawnSidecar = func(s string) bool {
			stops++
			if st, err := d.CurrentStatus(s); err != nil || st == nil || st.EndedAt != nil {
				t.Errorf("row of %q ended or absent at the sidecar stop (status=%v, err=%v)", s, st, err)
			}
			return stopOK
		}

		_ = captureStderr(t, func() { cleanupFailedSpawn(d, SpawnOpts{SessionName: name}, db.ReapCauseSpawnFailure) })
		stopFailedSpawnSidecar = prev

		if stops != 1 {
			t.Errorf("stopOK=%v: %d sidecar stops, want 1", stopOK, stops)
		}
		st, err := d.CurrentStatus(name)
		if err != nil || st == nil {
			t.Fatalf("CurrentStatus = %v, %v", st, err)
		}
		if st.State != "error" {
			t.Errorf("stopOK=%v: state = %q, want error", stopOK, st.State)
		}
		if stopOK && (st.EndedAt == nil || st.HarnessPort != nil) {
			t.Errorf("stopped sidecar: ended_at=%v port=%v, want ended with no port", st.EndedAt, st.HarnessPort)
		}
		if !stopOK && (st.EndedAt != nil || st.HarnessPort == nil) {
			t.Errorf("failed stop: ended_at=%v port=%v, want open with its port", st.EndedAt, st.HarnessPort)
		}
	}
}
