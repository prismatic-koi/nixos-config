package cmd

// Tests for `prism spawn --wait` (#1500).
//
// We test the wait loop directly (waitForSpawnTerminal) rather than going
// through runSpawn — runSpawn does substantial side-effecting work (worktree
// creation, tmux session creation) that is out of scope for the wait
// behaviour. The wait loop's contract is "given a session row in the DB,
// poll until it reaches a terminal state and emit the right summary".

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

func openSpawnWaitTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spawn_wait.db")
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })
	t.Setenv("PRISM_HOST_API", "")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestWaitForSpawnTerminal_FinishedExits0 — the documented happy path.
// Seed an agent_status row in "active" state, then flip to "finished" in a
// goroutine. The wait loop should observe the flip and exit 0.
func TestWaitForSpawnTerminal_FinishedExits0(t *testing.T) {
	d := openSpawnWaitTestDB(t)
	const sess = "repo@feature"
	if err := d.UpsertStatus(sess, "repo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		d2, dErr := openDB()
		if dErr != nil {
			t.Errorf("openDB in goroutine: %v", dErr)
			return
		}
		defer d2.Close()
		if err := d2.UpsertStatus(sess, "repo", "/wt", "finished", nil, nil); err != nil {
			t.Errorf("UpsertStatus finished: %v", err)
		}
	}()

	out := captureStdout(t, func() {
		if err := waitForSpawnTerminal(sess, false, 5*time.Second); err != nil {
			t.Errorf("waitForSpawnTerminal: expected nil on finished, got %v", err)
		}
	})
	if !strings.Contains(out, "finished") {
		t.Errorf("expected finished summary, got %q", out)
	}
}

// TestWaitForSpawnTerminal_ErrorReturnsTerminalFail — error state is a
// terminal that exits with the terminal-fail code (2), not 0.
func TestWaitForSpawnTerminal_ErrorReturnsTerminalFail(t *testing.T) {
	d := openSpawnWaitTestDB(t)
	const sess = "repo@bad"
	if err := d.UpsertStatus(sess, "repo", "/wt", "error", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	_ = captureStdout(t, func() {
		err := waitForSpawnTerminal(sess, false, 5*time.Second)
		if err == nil {
			t.Fatal("expected non-nil error on error terminal")
		}
		var ec *exitCodeError
		if !errors.As(err, &ec) {
			t.Fatalf("expected *exitCodeError, got %T: %v", err, err)
		}
		if ec.code != waitExitTerminalFail {
			t.Errorf("expected exit code %d, got %d", waitExitTerminalFail, ec.code)
		}
	})
}

// TestWaitForSpawnTerminal_TimeoutReturnsTimeoutCode — never-finished
// session yields the timeout exit code (3), not the terminal-fail code (2).
func TestWaitForSpawnTerminal_TimeoutReturnsTimeoutCode(t *testing.T) {
	d := openSpawnWaitTestDB(t)
	const sess = "repo@slow"
	if err := d.UpsertStatus(sess, "repo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	out := captureStdout(t, func() {
		err := waitForSpawnTerminal(sess, true /* json */, 100*time.Millisecond)
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if exitCodeOf(err) != waitExitTimeout {
			t.Errorf("expected timeout code %d, got %d", waitExitTimeout, exitCodeOf(err))
		}
	})
	var payload map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &payload); err == nil && payload["state"] == "timeout" {
			break
		}
	}
	if payload["state"] != "timeout" {
		t.Errorf("expected JSON state=timeout, got %v\nout: %s", payload, out)
	}
	if payload["session"] != "repo@slow" {
		t.Errorf("expected session=repo@slow, got %v", payload["session"])
	}
}

// TestEmitSpawnWaitTerminal_JSONShape exercises the JSON contract for
// `prism spawn --wait --json`: stable keys, single-line output, no textual
// chatter.
func TestEmitSpawnWaitTerminal_JSONShape(t *testing.T) {
	d := openSpawnWaitTestDB(t)
	const sess = "repo@done"
	if err := d.UpsertStatus(sess, "repo", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	out := captureStdout(t, func() {
		if err := emitSpawnWaitTerminal(sess, d, true); err != nil {
			t.Errorf("emitSpawnWaitTerminal: %v", err)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		t.Fatalf("not JSON: %v\nout: %s", err, out)
	}
	for _, k := range []string{"session", "state", "status"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("missing required key %q in JSON: %v", k, payload)
		}
	}
	if payload["state"] != "finished" {
		t.Errorf("state: want finished, got %v", payload["state"])
	}
}
