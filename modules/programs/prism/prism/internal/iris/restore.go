package iris

// restore.go — daemon-restart restore logic for iris (D-9, #1640).
//
// This file implements the three-step restore sequence that runs on iris daemon
// startup after the DB is opened but before any new pi child is spawned:
//
//  1. Orphan detection: for each session in "active" state, enumerate
//     tool_call events without a matching tool_result and write a synthetic
//     tool_result {success:false, isError:true, synthetic:true}.
//
//  2. Session re-spawn: for sessions in "active" state, locate the pi JSONL
//     session file, create a Supervisor with --session <path>, and start it.
//
//  3. Spawning reconciliation: sessions in "spawning" state at daemon-crash
//     time are stuck mid-creation; mark them error("daemon crashed during spawn").
//
// All active sessions are restored concurrently via a goroutine per session.
// Spawning sessions are reconciled in a tight loop before the concurrent phase.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	piharness "github.com/prismatic-koi/prism/internal/harness/pi"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
)

// RestoreConfig holds the parameters needed to run the restore sequence.
type RestoreConfig struct {
	// Database is the open iris DB.
	Database *db.DB
	// RunDir is the iris run directory (e.g. ~/.local/state/iris/run/).
	// Used to re-create harness sockets for restored sessions.
	RunDir string
	// PIAgentDir is the base directory where pi stores session files
	// (default: ~/.pi/agent/). Used to locate JSONL files at restore time.
	// When empty, defaults to ~/.pi/agent/.
	PIAgentDir string
	// SupervisorTemplate is the template SupervisorConfig used for all
	// re-spawned sessions. Per-session fields (SessionName, Worktree, Role,
	// SessionContinuePath) are filled in for each restored session.
	SupervisorTemplate SupervisorConfig
	// Publisher is the optional event publisher for client fan-out (D-6,
	// §4.3). When non-nil, synthetic tool_result events written during orphan
	// detection are also published to connected IPC subscribers in real time,
	// matching how real tool_result events flow through harness_socket.go.
	// Nil disables fan-out (acceptable for tests and non-daemon invocations).
	Publisher EventPublisher
}

// RestoreResult summarises what the restore sequence did.
type RestoreResult struct {
	// SpawningMarkError is the number of sessions marked error due to being
	// in spawning state at daemon-crash time.
	SpawningMarkError int
	// OrphansWritten is the total number of synthetic tool_result events written.
	OrphansWritten int
	// SessionsRestored is the number of sessions for which SpawnSession was called.
	SessionsRestored int
	// SessionsSkipped is the number of active sessions that could not be restored
	// (missing JSONL, missing worktree, etc.).
	SessionsSkipped int
	// Supervisors is the list of supervisor instances that RunRestore started
	// goroutines for (one per SessionsRestored increment). Callers that own
	// the supervisor lifecycle — the daemon's process loop, or tests that
	// must wait for shutdown before tearing down tempdirs / closing the DB —
	// should cancel the supervisor context and then wait on <-sup.Done() for
	// each entry to guarantee no late writes against torn-down state.
	//
	// This was added to fix issue #1705: supervisor goroutines spawned by
	// RunRestore outlived test t.Cleanup, racing against tempdir removal and
	// DB close under -race. Tests should use iristest.RunRestoreForTest which
	// wires up the shutdown wait automatically.
	Supervisors []*Supervisor
}

// RunRestore executes the daemon-restart restore sequence. It is called once
// at daemon startup, after the DB is opened, before any new sessions are spawned.
//
// RunRestore blocks until orphan detection and spawning reconciliation are
// complete, and until all re-spawned session goroutines have been started
// (though the goroutines themselves run asynchronously).
//
// ctx is used only for the re-spawned supervisor goroutines; it is NOT
// expected to be cancelled during RunRestore itself (callers should pass the
// daemon's lifecycle context).
func RunRestore(ctx context.Context, cfg RestoreConfig) (*RestoreResult, error) {
	sessions, err := cfg.Database.IrisSessionsToRestore()
	if err != nil {
		return nil, fmt.Errorf("iris: restore: enumerate sessions: %w", err)
	}

	if len(sessions) == 0 {
		log.Printf("[iris] restore: no sessions to restore")
		return &RestoreResult{}, nil
	}
	log.Printf("[iris] restore: %d session(s) to restore", len(sessions))

	result := &RestoreResult{}

	// Step 1: Reconcile sessions that were in "spawning" state at crash time.
	// These cannot be safely re-spawned; mark them error immediately.
	for _, sess := range sessions {
		if sess.IrisState != string(StateSpawning) {
			continue
		}
		log.Printf("[iris] restore: session %s (%s) was in spawning state at crash — marking error",
			sess.InstanceID, sess.SessionName)
		if err := markSessionErrorWithReason(cfg.Database, sess.InstanceID, "daemon crashed during spawn"); err != nil {
			log.Printf("[iris] restore: failed to mark spawning session %s as error: %v", sess.InstanceID, err)
		}
		result.SpawningMarkError++
	}

	// Step 2: Restore active and waiting sessions concurrently. "waiting" is
	// treated the same as "active" for restore purposes (issue #1701): pi is
	// re-spawned with the previous JSONL session file, and the extension's
	// next state_change frame (whether "waiting" or "active") re-asserts the
	// correct state on the restored supervisor.
	var wg sync.WaitGroup
	var mu sync.Mutex // guards result counters

	for _, sess := range sessions {
		if sess.IrisState != string(StateActive) && sess.IrisState != string(StateWaiting) {
			continue
		}
		sess := sess // capture for goroutine

		// Step 2a: Orphan detection — synchronous per-session (fast DB query).
		orphanCount, err := detectAndWriteOrphans(cfg.Database, cfg.Publisher, sess.InstanceID, sess.SessionName, sess.Worktree)
		if err != nil {
			log.Printf("[iris] restore: orphan detection for session %s failed: %v", sess.InstanceID, err)
		}
		mu.Lock()
		result.OrphansWritten += orphanCount
		mu.Unlock()

		// Step 2b: Re-spawn the session concurrently.
		wg.Add(1)
		go func() {
			defer wg.Done()
			sup, restored := restoreActiveSession(ctx, cfg, sess)
			mu.Lock()
			if restored {
				result.SessionsRestored++
				if sup != nil {
					result.Supervisors = append(result.Supervisors, sup)
				}
			} else {
				result.SessionsSkipped++
			}
			mu.Unlock()
		}()
	}

	wg.Wait()
	log.Printf("[iris] restore: complete — spawning_errors=%d orphans_written=%d restored=%d skipped=%d",
		result.SpawningMarkError, result.OrphansWritten, result.SessionsRestored, result.SessionsSkipped)
	return result, nil
}

// detectAndWriteOrphans finds tool_call events for the session without a
// matching tool_result and writes a synthetic tool_result for each. Returns
// the number of synthetic events written. If pub is non-nil, each synthetic
// event is also published to the client fan-out (D-6 §4.3).
func detectAndWriteOrphans(database *db.DB, pub EventPublisher, instanceID, sessionName, worktree string) (int, error) {
	orphans, err := database.IrisOrphanedToolCalls(instanceID)
	if err != nil {
		return 0, fmt.Errorf("query orphans: %w", err)
	}

	count := 0
	for _, orphan := range orphans {
		log.Printf("[iris] restore: writing synthetic tool_result for orphaned call %q (session %s)",
			orphan.ToolCallID, instanceID)
		rowID, payload, err := database.IrisSyntheticToolResult(sessionName, worktree, orphan.ToolCallID, instanceID)
		if err != nil {
			log.Printf("[iris] restore: failed to write synthetic tool_result for %q: %v", orphan.ToolCallID, err)
			continue
		}
		if pub != nil {
			pub.Publish(EventPublication{
				SessionName: sessionName,
				RowID:       rowID,
				EventType:   "tool_result",
				Payload:     payload,
			})
		}
		count++
	}
	return count, nil
}

// restoreActiveSession attempts to re-spawn a single active session. Returns
// the started Supervisor and true when a Supervisor was started, or (nil,
// false) otherwise. The returned supervisor is owned by the caller — its
// goroutine runs asynchronously and the caller is responsible for waiting
// on <-sup.Done() before tearing down any shared state (tempdir, DB).
func restoreActiveSession(ctx context.Context, cfg RestoreConfig, sess db.IrisSessionRow) (*Supervisor, bool) {
	// Check whether the worktree still exists.
	if _, err := os.Stat(sess.Worktree); os.IsNotExist(err) {
		log.Printf("[iris] restore: worktree %q for session %s no longer exists — marking error",
			sess.Worktree, sess.InstanceID)
		if merr := markSessionErrorWithReason(cfg.Database, sess.InstanceID, "worktree missing"); merr != nil {
			log.Printf("[iris] restore: failed to mark session %s error: %v", sess.InstanceID, merr)
		}
		return nil, false
	}

	// Locate the pi JSONL session file.
	jsonlPath, err := findPiSessionJSONL(cfg, sess)
	if err != nil {
		log.Printf("[iris] restore: cannot locate JSONL for session %s (harness_session_id=%q worktree=%q): %v",
			sess.InstanceID, sess.HarnessSessionID, sess.Worktree, err)
		if merr := markSessionErrorWithReason(cfg.Database, sess.InstanceID, "session file missing"); merr != nil {
			log.Printf("[iris] restore: failed to mark session %s error: %v", sess.InstanceID, merr)
		}
		return nil, false
	}

	log.Printf("[iris] restore: re-spawning session %s (%s) with JSONL %q",
		sess.InstanceID, sess.SessionName, jsonlPath)

	// Build per-session config from the template.
	superCfg := cfg.SupervisorTemplate
	superCfg.SessionName = sess.SessionName
	superCfg.Worktree = sess.Worktree
	superCfg.Role = sess.Role
	// Derive the bare repo root from the worktree for 4-PAT GITHUB_TOKEN
	// selection in the bash sandbox (D-5). Mirrors the pattern used in
	// cmd/iris/main.go for the spawn and daemon paths.
	superCfg.BareRoot = git.BareRoot(sess.Worktree)
	superCfg.SessionContinuePath = jsonlPath
	superCfg.Database = cfg.Database
	superCfg.RunDir = cfg.RunDir

	// Create a Supervisor that reuses the existing instance ID.
	sup, err := newRestoreSupervisor(superCfg, sess)
	if err != nil {
		log.Printf("[iris] restore: failed to create supervisor for %s: %v", sess.InstanceID, err)
		if merr := markSessionErrorWithReason(cfg.Database, sess.InstanceID, "supervisor creation failed"); merr != nil {
			log.Printf("[iris] restore: failed to mark session %s error: %v", sess.InstanceID, merr)
		}
		return nil, false
	}

	go sup.Start(ctx)
	return sup, true
}

// findPiSessionJSONL locates the pi JSONL session file for the given session
// row using the same path-encoding logic as internal/harness/pi/archive.go.
//
// The algorithm:
//  1. Determine the pi agent base dir: cfg.PIAgentDir or ~/.pi/agent/.
//  2. Encode the worktree path using EncodePiCWD.
//  3. Scan ~/.pi/agent/sessions/<encodedCWD>/ for a file matching
//     *_<HarnessSessionID>.jsonl.
//
// Returns an error when the JSONL file cannot be found.
func findPiSessionJSONL(cfg RestoreConfig, sess db.IrisSessionRow) (string, error) {
	if sess.HarnessSessionID == "" {
		return "", fmt.Errorf("harness_session_id is empty; cannot locate pi JSONL")
	}

	piAgentDir := cfg.PIAgentDir
	if piAgentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		piAgentDir = filepath.Join(home, ".pi", "agent")
	}

	encodedCWD := piharness.EncodePiCWD(sess.Worktree)
	cwdDir := filepath.Join(piAgentDir, "sessions", encodedCWD)

	entries, err := os.ReadDir(cwdDir)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("pi sessions dir %q does not exist", cwdDir)
	}
	if err != nil {
		return "", fmt.Errorf("scan pi sessions dir %q: %w", cwdDir, err)
	}

	suffix := "_" + sess.HarnessSessionID + ".jsonl"
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(cwdDir, e.Name()), nil
		}
	}

	return "", fmt.Errorf("no JSONL file matching *%s found in %q", suffix, cwdDir)
}

// markSessionErrorWithReason sets iris_state to "error" and end_state to
// "error" on the sessions row for instanceID.
func markSessionErrorWithReason(database *db.DB, instanceID, reason string) error {
	log.Printf("[iris] restore: marking session %s as error(%s)", instanceID, reason)
	if err := database.IrisUpdateSessionState(instanceID, string(StateError)); err != nil {
		return err
	}
	return database.UpdateSessionEnded(instanceID, "error")
}

// newRestoreSupervisor creates a Supervisor that reuses an existing instanceID
// from a previous daemon incarnation. Unlike NewSupervisor, it does NOT mint
// a new UUID or insert a new sessions row. It does re-create the harness socket
// (the previous socket was cleaned up on daemon crash) so the restarted pi
// child can connect.
//
// This is used exclusively by the D-9 restore path.
func newRestoreSupervisor(cfg SupervisorConfig, sess db.IrisSessionRow) (*Supervisor, error) {
	if cfg.RestartThreshold == 0 {
		cfg.RestartThreshold = DefaultRestartThreshold
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}

	harnessSockPath := HarnessSockPath(cfg.RunDir, sess.InstanceID)

	record := SessionRecord{
		InstanceID:       sess.InstanceID,
		SessionName:      sess.SessionName,
		Worktree:         sess.Worktree,
		Role:             sess.Role,
		State:            StateSpawning, // Start will transition to active
		HarnessSockPath:  harnessSockPath,
		RestartCount:     0,
		RestartThreshold: cfg.RestartThreshold,
		StartedAt:        sess.StartedAt,
		PiSessionPath:    cfg.SessionContinuePath,
		// BareRoot must be populated here so the D-5/D-7 credential broker can
		// resolve the role-scoped GITHUB_TOKEN for bash subprocesses run by a
		// restored session. Without this, restored sessions would always fall
		// back to host GITHUB_TOKEN — a silent credential downgrade. Mirrors
		// the spawn and daemon paths in cmd/iris/main.go.
		BareRoot: cfg.BareRoot,
	}

	// Create session run directory (may already exist from previous incarnation).
	if _, err := EnsureSessionDir(cfg.RunDir, sess.InstanceID); err != nil {
		return nil, fmt.Errorf("iris: restore supervisor: ensure session dir: %w", err)
	}

	harness, err := NewHarnessSocketServer(&record, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("iris: restore supervisor: create harness server: %w", err)
	}
	if cfg.Publisher != nil {
		harness.SetPublisher(cfg.Publisher)
	}
	if err := harness.Listen(); err != nil {
		return nil, fmt.Errorf("iris: restore supervisor: listen: %w", err)
	}

	// Update the DB to reflect that we're re-spawning this session.
	// Reset iris_state to spawning so that another crash mid-restore is
	// handled correctly on the next startup.
	if err := cfg.Database.IrisUpdateSessionState(sess.InstanceID, string(StateSpawning)); err != nil {
		log.Printf("[iris] restore: failed to reset iris_state to spawning for %s: %v", sess.InstanceID, err)
	}

	logFile, logErr := openSessionLogFile(cfg.LogDir, cfg.SessionName)
	if logErr != nil {
		log.Printf("[iris] restore: open per-session log: %v (continuing without per-session log)", logErr)
	}
	sessionLog := newSessionLogger(logFile, cfg.SessionName)

	sup := &Supervisor{
		cfg:            cfg,
		sess:           record,
		harness:        harness,
		state:          StateSpawning,
		sessionLog:     sessionLog,
		sessionLogFile: logFile,
		done:           make(chan struct{}),
	}
	// Wire harness → supervisor callbacks so restored sessions get the same
	// in-memory updates as freshly-spawned ones. Without this, a restored
	// session's pi child could deliver session_status or state_change frames
	// that update the DB but leave the in-memory SessionRecord stale
	// (issues #1682 and #1701).
	harness.SetSessionStatusHandler(sup.handleSessionStatus)
	harness.SetStateChangeHandler(sup.handleStateChange)
	return sup, nil
}
