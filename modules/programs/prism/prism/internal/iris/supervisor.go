package iris

// supervisor.go — pi child supervisor for the iris daemon.
//
// The Supervisor holds a single pi --mode rpc child and its associated harness
// socket server. It watches cmd.Wait() and applies the restart policy
// (§3.6.1 and §11.2 of the daemon-mode design doc):
//
//   exit 0 + session_shutdown  → StateFinished, no restart
//   exit 0 without shutdown    → StateFinished (log anomaly), no restart
//   non-zero exit, count < N   → StateError, backoff, restart
//   non-zero exit, count >= N  → StateError, circuit breaker, no restart
//   context cancelled          → send abort to pi, then SIGTERM/SIGKILL

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
)

// SupervisorConfig holds the spawn parameters for one pi session.
type SupervisorConfig struct {
	// SessionName is the logical session name (e.g. "nixos-config@my-branch").
	SessionName string
	// Worktree is the absolute path to the git worktree.
	Worktree string
	// Role is the agent role ("worker", "coordinator", etc.).
	Role string
	// BareRoot is the bare git repository root for this session. Used by the
	// bash sandbox to select the role-scoped GITHUB_TOKEN via the 4-PAT
	// architecture.  May be empty; falls back to host GITHUB_TOKEN.
	BareRoot string
	// ParentSession is the logical session_name of the iris session that
	// invoked `iris spawn` to create this one. Populated by the daemon from
	// the session_spawn frame's Parent field (which the CLI reads from
	// IRIS_SESSION_NAME). Empty means "no parent" (top-level spawn). Used by
	// the terminal-state notification path (#1700) to deliver an
	// "Agent <name> has finished" prompt back to the spawning session.
	ParentSession string
	// PIBinaryPath is the path to the pi binary. Falls back to "pi" on PATH.
	PIBinaryPath string
	// ExtensionPath is the absolute path to prism.ts.
	ExtensionPath string
	// RestartThreshold is the max consecutive failures before the circuit
	// breaker opens. 0 means use DefaultRestartThreshold.
	RestartThreshold int
	// ShutdownTimeout is how long to wait for pi to exit gracefully on abort.
	// 0 means use a default of 10 seconds.
	ShutdownTimeout time.Duration
	// RunDir is the iris run directory (e.g. ~/.local/state/iris/run/).
	RunDir string
	// LogDir is the iris per-session log directory (e.g.
	// ~/.local/state/iris/logs/). When non-empty, the supervisor opens a
	// per-session log file at <LogDir>/<session-name>.log and tees its
	// session-scoped log lines into it. The file is opened with O_APPEND
	// so restart and re-spawn append rather than truncate. Empty disables
	// per-session logging — in that case session log lines only go to the
	// global logger (stderr / journal).
	LogDir string
	// Database is the open iris DB (used for event writes).
	Database *db.DB
	// Publisher is the optional EventPublisher wired to the harness socket so
	// that every event written via the harness is also fanned out to client
	// subscribers (D-6). When nil, fan-out is disabled (harness-only mode).
	Publisher EventPublisher
	// PIAgentDir is the base directory where pi stores session files
	// (~/.pi/agent/). Used by SpawnSessionContinue to locate the JSONL file.
	// When empty, defaults to ~/.pi/agent/.
	PIAgentDir string
	// SessionContinuePath is the full path to an existing pi JSONL session
	// file to pass as --session <path> on the pi command line. When non-empty,
	// the pi child resumes conversation history from this file. Set by the
	// D-9 restore path when re-spawning after daemon restart.
	SessionContinuePath string
	// NotifyParent, when non-nil, is invoked from setState on a terminal
	// transition (StateFinished / StateError) when ParentSession is non-empty.
	// The callback is responsible for delivering an "Agent <name> has finished"
	// (or "has errored") prompt to the parent session via the daemon's
	// existing prompt-delivery path. Wired by the daemon at SupervisorConfig
	// construction so the supervisor does not need a direct reference to the
	// supervisor map. deliveryID is a freshly minted UUID per call (#1695
	// exactly-once-with-replay-marker contract). state is the supervisor's
	// terminal SessionState (StateFinished or StateError).
	//
	// NotifyParent is called from a goroutine — the supervisor lock is NOT
	// held when it runs (#1687).
	NotifyParent func(child, parent string, state SessionState, deliveryID string)
}

// Supervisor manages a single pi child process.
type Supervisor struct {
	cfg     SupervisorConfig
	sess    SessionRecord
	harness *HarnessSocketServer

	// stdinPipe is the write end of pi's stdin (for sending RPC commands).
	stdinPipe io.WriteCloser

	// process is the live pi *os.Process while pi is running, nil otherwise.
	// Used by Kill() to escalate to SIGKILL after a SIGTERM timeout. Guarded
	// by mu — same lock as stdinPipe and state.
	process *os.Process

	// sessionLog is the per-session logger that writes to both the global
	// log destination (stderr / journal) and the per-session log file under
	// LogDir. Always non-nil after construction; falls back to the stderr-
	// only global logger when LogDir is empty or the file cannot be opened.
	sessionLog *log.Logger
	// sessionLogFile is the per-session log file. May be nil when LogDir is
	// empty or the file could not be opened. Closed by Start() on terminal
	// state.
	sessionLogFile *os.File

	// cancel is the per-session context cancel function. It is set by Start()
	// at the top of the loop and used by Kill() to send SIGTERM via
	// exec.CommandContext. Guarded by mu.
	cancel context.CancelFunc
	// done is closed by Start() when the supervisor goroutine returns. Kill()
	// waits on it to determine when pi has fully exited. Initialised in
	// NewSupervisor so callers can wait even before Start() runs.
	done chan struct{}

	mu    sync.Mutex
	state SessionState
	// killReason, when non-empty, overrides the default termination reason in
	// the session_end event emitted on terminal state. Set by Kill() to
	// "killed_sigterm" or "killed_sigkill". Guarded by mu.
	killReason string
	// parentNotified is true once the parent-notification trigger has fired
	// for this supervisor. Defence-in-depth (#1700): the existing setState
	// dedup catches Finished→Finished and Error→Error sequences but not
	// Finished→Error transitions. If a future kill-path or restart-policy
	// change ever drives two terminal transitions in sequence with different
	// states (currently impossible by inspection — see the comment in
	// setState near this guard), the second notification must still not
	// fire. Guarded by mu.
	parentNotified bool
}

// stateNotifier is implemented by publishers that want to receive
// state-transition notifications. The supervisor calls PublishState on every
// transition so subscribed clients can drive UI updates (e.g. the TUI session
// table, or `iris logs --follow` exiting after terminal state).
//
// EventPublisher implementations may optionally satisfy this interface; the
// supervisor checks for it at the call site rather than embedding it in the
// EventPublisher interface so existing test publishers that only implement
// Publish continue to compile.
type stateNotifier interface {
	PublishState(sessionName, state string)
}

// openSessionLogFile opens (creating if needed) the per-session log file under
// logDir for the given session name. Returns (nil, nil) when logDir is empty
// — callers must tolerate a nil file. Errors are returned for logging by the
// caller but never made fatal: missing per-session logs do not block spawn.
func openSessionLogFile(logDir, sessionName string) (*os.File, error) {
	if logDir == "" || sessionName == "" {
		return nil, nil
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir log dir %q: %w", logDir, err)
	}
	path := (Paths{LogDir: logDir}).SessionLogPath(sessionName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session log %q: %w", path, err)
	}
	return f, nil
}

// newSessionLogger constructs a *log.Logger that writes to both the global
// stderr destination and the given per-session file. When f is nil, the
// returned logger writes only to stderr (matching the global log package).
// The prefix is "[iris:<session>] " so per-session files self-identify even
// after grepping across multiple logs.
func newSessionLogger(f *os.File, sessionName string) *log.Logger {
	prefix := fmt.Sprintf("[iris:%s] ", sessionName)
	flags := log.LstdFlags | log.Lmicroseconds
	if f == nil {
		return log.New(os.Stderr, prefix, flags)
	}
	return log.New(io.MultiWriter(os.Stderr, f), prefix, flags)
}

// NewSupervisor creates a Supervisor for the given config and begins the
// spawn sequence. It does NOT start the pi child — call Start().
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.RestartThreshold == 0 {
		cfg.RestartThreshold = DefaultRestartThreshold
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}

	instanceID := uuid.New().String()
	harnessSockPath := HarnessSockPath(cfg.RunDir, instanceID)

	sess := SessionRecord{
		InstanceID:       instanceID,
		SessionName:      cfg.SessionName,
		Worktree:         cfg.Worktree,
		Role:             cfg.Role,
		BareRoot:         cfg.BareRoot,
		ParentSession:    cfg.ParentSession,
		State:            StateSpawning,
		HarnessSockPath:  harnessSockPath,
		RestartCount:     0,
		RestartThreshold: cfg.RestartThreshold,
		StartedAt:        time.Now(),
	}

	// Ensure the session run directory exists before we create the socket.
	if _, err := EnsureSessionDir(cfg.RunDir, instanceID); err != nil {
		return nil, err
	}

	harness, err := NewHarnessSocketServer(&sess, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("iris: create harness server: %w", err)
	}
	if err := harness.Listen(); err != nil {
		return nil, err
	}

	// Wire the client-socket publisher (D-6 fan-out). This must happen before
	// AcceptOne is called so that every harness event is published from the
	// first frame onward.
	if cfg.Publisher != nil {
		harness.SetPublisher(cfg.Publisher)
	}

	// Insert the session record into the DB.
	if err := insertSessionRecord(cfg.Database, sess); err != nil {
		// Non-fatal: log and continue.
		log.Printf("[iris] supervisor: failed to insert session record: %v", err)
	}
	// Write initial iris_state=spawning to the DB so the restore path can
	// distinguish sessions that crashed during creation from active ones.
	if err := cfg.Database.IrisUpdateSessionState(instanceID, string(StateSpawning)); err != nil {
		log.Printf("[iris] supervisor: failed to set initial iris_state: %v", err)
	}

	logFile, logErr := openSessionLogFile(cfg.LogDir, cfg.SessionName)
	if logErr != nil {
		log.Printf("[iris] supervisor: open per-session log: %v (continuing without per-session log)", logErr)
	}
	sessionLog := newSessionLogger(logFile, cfg.SessionName)

	sup := &Supervisor{
		cfg:            cfg,
		sess:           sess,
		harness:        harness,
		state:          StateSpawning,
		sessionLog:     sessionLog,
		sessionLogFile: logFile,
		done:           make(chan struct{}),
	}
	// Wire the session_status handler so the harness socket can update the
	// in-memory SessionRecord.PiSessionPath as soon as pi delivers its
	// session UUID (issue #1682). Without this, the in-memory record would
	// stay empty for the entire lifetime of every live session even though
	// the DB row is correct.
	harness.SetSessionStatusHandler(sup.handleSessionStatus)
	// Wire the state_change handler so the harness socket can drive the
	// in-memory session state machine when the extension emits a
	// state_change frame (issue #1701). The handler runs *after* the
	// agent_events row has been written by writeObservationEvent, preserving
	// the PR #1657 event-row-before-status-row ordering.
	harness.SetStateChangeHandler(sup.handleStateChange)
	return sup, nil
}

// handleStateChange is the callback the HarnessSocketServer invokes when a
// state_change frame arrives from the extension. It maps the wire state
// string to a SessionState and drives setState under the supervisor's lock
// (PR #1687 pattern). Unknown wire states are logged and ignored — the
// extension is forward-compatible per the wire spec.
//
// Issue #1701: this is the production path that lands a session in
// StateWaiting. Without it the iris prompt waiting-state guard (#1689) is
// unreachable.
func (s *Supervisor) handleStateChange(state string) {
	// Terminal states are owned by the supervisor lifecycle (clean exit,
	// non-zero exit, kill). Don't let the extension drive us out of a
	// terminal state — once finished/error, the supervisor goroutine has
	// already returned or is about to, and re-asserting active/waiting here
	// would publish a misleading transition.
	s.mu.Lock()
	current := s.state
	killing := s.killReason != ""
	s.mu.Unlock()
	if current == StateFinished || current == StateError || killing {
		s.logf("supervisor: ignoring state_change=%q while terminal/killing (current=%s)", state, current)
		return
	}

	var next SessionState
	switch state {
	case "waiting":
		next = StateWaiting
	case "active":
		next = StateActive
	default:
		// Other wire states (finished, interrupted, etc.) are not driven
		// through this path — terminal transitions are owned by the
		// supervisor loop. Log and ignore so future wire-protocol additions
		// are forward-compatible.
		s.logf("supervisor: state_change=%q ignored (not driven from harness)", state)
		return
	}

	if current == next {
		return
	}
	s.setState(next)
}

// handleSessionStatus is the callback the HarnessSocketServer invokes when
// a session_status frame delivers pi's session UUID. It resolves the JSONL
// file path the same way the D-9 restore path does and stores it on the
// in-memory SessionRecord under the supervisor's lock.
//
// If the JSONL file cannot be located (a transient condition at the moment
// session_status arrives — pi may not yet have flushed the first chunk),
// the UUID itself is stored as the path. This keeps harness_session_id
// non-empty for the live-session AC; the next daemon-restart restore will
// upgrade it to a full path via findPiSessionJSONL.
func (s *Supervisor) handleSessionStatus(sessionID string) {
	if sessionID == "" {
		return
	}
	path := s.resolvePiSessionPath(sessionID)
	s.SetPiSessionPath(s.sess.InstanceID, path)
}

// resolvePiSessionPath finds the pi JSONL file for the given session UUID by
// scanning the per-cwd pi sessions directory. Mirrors findPiSessionJSONL
// (used by the restore path) so live and restored sessions converge on the
// same path format.
func (s *Supervisor) resolvePiSessionPath(sessionID string) string {
	piAgentDir := s.cfg.PIAgentDir
	if piAgentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Cannot resolve — fall back to the bare UUID so clients still
			// see a non-empty harness_session_id.
			return sessionID
		}
		piAgentDir = filepath.Join(home, ".pi", "agent")
	}
	encodedCWD := piharness.EncodePiCWD(s.sess.Worktree)
	cwdDir := filepath.Join(piAgentDir, "sessions", encodedCWD)
	entries, err := os.ReadDir(cwdDir)
	if err != nil {
		return sessionID
	}
	suffix := "_" + sessionID + ".jsonl"
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(cwdDir, e.Name())
		}
	}
	return sessionID
}

// InstanceID returns the session instance ID. The instance ID is immutable
// after NewSupervisor returns so this is safe without holding the lock.
func (s *Supervisor) InstanceID() string { return s.sess.InstanceID }

// SessionRecord returns a copy of the session record under the supervisor's
// lock so callers see a consistent snapshot of fields that may be mutated
// concurrently (State, PiSessionPath, RestartCount).
func (s *Supervisor) SessionRecord() SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess
}

// SetPiSessionPath updates the in-memory SessionRecord's PiSessionPath field
// under the supervisor's lock. Called by the harness socket session_status
// handler immediately after the DB write so that daemonState.activeSessions()
// (which reads from the in-memory record, not the DB) reports the correct
// harness_session_id for live sessions.
//
// The instanceID argument is checked against this supervisor's instance ID;
// a mismatch is a no-op (defensive — the caller should already be scoped
// to this supervisor's session).
func (s *Supervisor) SetPiSessionPath(instanceID, path string) {
	if instanceID != s.sess.InstanceID {
		return
	}
	s.mu.Lock()
	s.sess.PiSessionPath = path
	s.mu.Unlock()
}

// SetPublisher wires an EventPublisher to this supervisor's harness socket.
// It may be called at any time after NewSupervisor returns and before or
// after Start(). The publisher receives a Publish call for every event written
// to the DB by the harness (D-6 fan-out).
func (s *Supervisor) SetPublisher(p EventPublisher) {
	s.harness.SetPublisher(p)
}

// logf writes a log line via the per-session logger (stderr + per-session
// file). All supervisor lines should use this rather than the global log
// package so per-session log files capture them.
func (s *Supervisor) logf(format string, args ...any) {
	if s.sessionLog != nil {
		s.sessionLog.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// closeSessionLogFile closes the per-session log file if open. Safe to call
// multiple times.
func (s *Supervisor) closeSessionLogFile() {
	if s.sessionLogFile != nil {
		_ = s.sessionLogFile.Close()
		s.sessionLogFile = nil
	}
}

// Start spawns the pi child and runs the supervisor loop. It blocks until the
// session reaches a terminal state (finished or error) or ctx is cancelled.
// Start is intended to be called in its own goroutine.
func (s *Supervisor) Start(ctx context.Context) {
	// Wrap the caller's context in a per-session cancel so Kill() can SIGTERM
	// this one pi child without disturbing other sessions sharing the daemon
	// context. The wrapped context is the one that flows to exec.CommandContext
	// in spawnAndRun — cancelling it triggers SIGTERM delivery to pi.
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer cancel()

	// Ensure the per-session log file is closed when Start returns. The
	// supervisor's lifetime is the goroutine that runs Start, so this is the
	// correct close site. Match the natural session-end teardown convention
	// here too: close the harness listener (which removes the Unix socket
	// inode on Linux per the D-5 cycle-4 fix) so the kill path and the
	// natural exit path leave the same observable filesystem state.
	//
	// Defer ordering is LIFO and load-bearing: `close(s.done)` is the
	// completion signal Kill() blocks on, and it MUST fire last so the
	// harness listener (and therefore the Unix socket inode) is fully torn
	// down before Kill returns to the caller. Reversing these defers makes
	// TestSupervisorKill_RemovesHarnessSocketFile flake on CI — the kill
	// path observes s.done closed before harness.Close has run.
	defer close(s.done)
	defer s.harness.Close()
	defer s.closeSessionLogFile()

	for {
		exitCode := s.spawnAndRun(ctx)

		// Determine whether to restart.
		cleanExit := s.harness.SessionShutdownReceived()

		if ctx.Err() != nil {
			// Daemon is shutting down — do not restart.
			s.setState(StateFinished)
			return
		}

		if exitCode == 0 {
			if !cleanExit {
				s.logf("supervisor: session %s: pi exited 0 without session_shutdown (anomaly)", s.sess.InstanceID)
			}
			s.setState(StateFinished)
			return
		}

		// Non-zero exit.
		s.sess.RestartCount++
		if s.sess.RestartCount >= s.cfg.RestartThreshold {
			s.logf("supervisor: session %s: circuit breaker opened after %d failures", s.sess.InstanceID, s.sess.RestartCount)
			s.setState(StateError)
			return
		}

		backoff := RestartBackoff(s.sess.RestartCount)
		s.logf("supervisor: session %s: pi exited non-zero (attempt %d/%d), restarting in %v",
			s.sess.InstanceID, s.sess.RestartCount, s.cfg.RestartThreshold-1, backoff)
		s.setState(StateError)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		s.setState(StateSpawning)
	}
}

// spawnAndRun starts the pi child, runs the harness socket acceptor in a
// goroutine, reads pi's RPC stdout for observation events, and waits for
// the child to exit. Returns pi's exit code (0 for clean exit).
func (s *Supervisor) spawnAndRun(ctx context.Context) int {
	// Derive the pi binary path.
	piBin := s.cfg.PIBinaryPath
	if piBin == "" {
		var err error
		piBin, err = exec.LookPath("pi")
		if err != nil {
			s.logf("supervisor: pi not found on PATH: %v", err)
			return 1
		}
	}

	// Write a per-session pi config that loads the prism extension.
	piConfigDir, err := s.writePerSessionPIConfig()
	if err != nil {
		s.logf("supervisor: write pi config: %v", err)
		return 1
	}

	// Build the pi command.
	args := []string{"--mode", "rpc"}
	if s.cfg.ExtensionPath != "" {
		args = append(args, "--extension", s.cfg.ExtensionPath)
	}
	// Pass --session <path> when resuming a previous conversation (D-9 restore).
	if s.cfg.SessionContinuePath != "" {
		args = append(args, "--session", s.cfg.SessionContinuePath)
	}

	cmd := exec.CommandContext(ctx, piBin, args...)
	cmd.Dir = s.cfg.Worktree
	cmd.Env = s.buildEnv(piConfigDir)
	// Override the default SIGKILL-on-cancel behaviour with SIGTERM so the
	// kill ladder (issue #1674) runs SIGTERM → 5s grace → SIGKILL. WaitDelay
	// is intentionally NOT set: Kill() owns the SIGKILL escalation via the
	// recorded *os.Process handle, and a WaitDelay set here would race with
	// the explicit escalation path.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}

	// Wire stdin/stdout pipes.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		s.logf("supervisor: stdin pipe: %v", err)
		return 1
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		s.logf("supervisor: stdout pipe: %v", err)
		stdinPipe.Close()
		return 1
	}

	// Capture stderr to a log file.
	logFile := s.openLogFile()
	if logFile != nil {
		cmd.Stderr = logFile
		defer logFile.Close()
	}

	if err := cmd.Start(); err != nil {
		s.logf("supervisor: start pi: %v", err)
		return 1
	}

	s.mu.Lock()
	s.stdinPipe = stdinPipe
	s.process = cmd.Process
	s.mu.Unlock()

	s.logf("supervisor: spawned pi pid=%d (session %s)", cmd.Process.Pid, s.sess.InstanceID)
	s.setState(StateActive)

	// Run the harness socket acceptor in a goroutine.
	harnessCtx, harnessCancel := context.WithCancel(ctx)
	defer harnessCancel()
	go func() {
		if err := s.harness.AcceptOne(harnessCtx); err != nil && harnessCtx.Err() == nil {
			s.logf("supervisor: harness accept error (session %s): %v", s.sess.InstanceID, err)
		}
	}()

	// Read pi's RPC stdout in a goroutine (observation only in D-3).
	go func() {
		scanner := bufio.NewReaderSize(stdoutPipe, 16*1024*1024)
		for {
			line, err := scanner.ReadBytes('\n')
			if len(line) > 0 {
				s.handleRPCEvent(line)
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for pi to exit.
	waitErr := cmd.Wait()

	s.mu.Lock()
	s.stdinPipe = nil
	s.process = nil
	s.mu.Unlock()

	if waitErr == nil {
		return 0
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

// handleRPCEvent processes a single RPC event line from pi's stdout.
// In D-3 this is observation-only: we log agent lifecycle events and write
// the raw event to the DB.
func (s *Supervisor) handleRPCEvent(line []byte) {
	// Strip trailing newline.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return
	}

	var generic GenericFrame
	if err := json.Unmarshal(line, &generic); err != nil {
		s.logf("supervisor: RPC parse error: %v", err)
		return
	}

	switch generic.Type {
	case "agent_start":
		s.logf("supervisor: session %s: agent_start", s.sess.InstanceID)
	case "agent_end":
		s.logf("supervisor: session %s: agent_end", s.sess.InstanceID)
	case "response":
		// Command acknowledgement — log at debug level (suppressed for now).
	}
}

// SendRPC sends a JSON-line command to pi's stdin.
func (s *Supervisor) SendRPC(cmd any) error {
	s.mu.Lock()
	pipe := s.stdinPipe
	s.mu.Unlock()
	if pipe == nil {
		return fmt.Errorf("iris: no active pi process")
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = pipe.Write(data)
	return err
}

// Abort sends the abort RPC command to pi.
func (s *Supervisor) Abort() error {
	return s.SendRPC(map[string]any{"type": "abort"})
}

// buildEnv constructs the environment for the pi child.
func (s *Supervisor) buildEnv(piConfigDir string) []string {
	// Start from the current process environment so pi has access to
	// ANTHROPIC_API_KEY etc. (pi reads LLM credentials from its own config
	// dir and env vars, not from iris injections).
	env := os.Environ()

	// Set IRIS_DAEMON_SOCK so the prism extension knows to register overrides.
	env = append(env, "IRIS_DAEMON_SOCK="+s.sess.HarnessSockPath)

	// Set IRIS_SESSION_NAME so in-session CLIs (`iris review` #1694,
	// `iris escalate` #1693, future `iris prompt` from within a session,
	// etc.) can identify their calling session without a CWD walk or tmux
	// lookup. This is the iris analogue of PRISM_SESSION_NAME in prism's
	// worker environment. Guarded against an empty session name so
	// downstream emptiness checks in the worker CLIs ("set but empty" vs
	// "unset") don't fire spuriously — pinned by
	// TestSupervisor_BuildEnv_EmptySessionNameOmitsVar.
	if s.sess.SessionName != "" {
		env = append(env, "IRIS_SESSION_NAME="+s.sess.SessionName)
	}

	// Set PI_CODING_AGENT_DIR to the per-session config dir so pi loads
	// the prism extension and any APPEND_SYSTEM.md we write there.
	if piConfigDir != "" {
		env = append(env, "PI_CODING_AGENT_DIR="+piConfigDir)
	}

	return env
}

// writePerSessionPIConfig writes a minimal pi agent config directory for this
// session. The key file is a pi config/settings that auto-loads the prism
// extension pointing to s.cfg.ExtensionPath.
//
// In D-3 this simply creates the directory; the extension is loaded via the
// --extension flag on the pi command line instead of via the agent config.
// A full per-session settings.json injection (for provider overrides etc.)
// is a later concern.
func (s *Supervisor) writePerSessionPIConfig() (string, error) {
	if s.cfg.ExtensionPath == "" {
		return "", nil
	}

	// The config dir lives inside the session run dir.
	sessionDir := filepath.Join(s.cfg.RunDir, s.sess.InstanceID)
	configDir := filepath.Join(sessionDir, "pi-agent")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", fmt.Errorf("iris: create pi config dir: %w", err)
	}

	// In D-3 the extension is passed via --extension; the config dir just needs
	// to exist so PI_CODING_AGENT_DIR can be set without error.
	// We write a minimal settings.json so pi doesn't prompt for model config.
	settingsPath := filepath.Join(configDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		settings := `{"model": "claude-sonnet-4-20250514", "provider": "anthropic"}`
		if err := os.WriteFile(settingsPath, []byte(settings+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("iris: write pi settings: %w", err)
		}
	}

	return configDir, nil
}

// openLogFile opens (or creates) the stderr log file for this session.
func (s *Supervisor) openLogFile() *os.File {
	logDir := filepath.Join(s.cfg.RunDir, s.sess.InstanceID)
	logPath := filepath.Join(logDir, "pi-stderr.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.logf("supervisor: open log file %q: %v (stderr will be discarded)", logPath, err)
		return nil
	}
	return f
}

// setState updates the in-memory and DB state of the session.
func (s *Supervisor) setState(newState SessionState) {
	s.mu.Lock()
	// Suppress redundant transitions — the kill path may re-assert
	// StateError after setState already drove the supervisor into a terminal
	// state via the normal restart-policy path. Without this guard, the
	// session_end event would be written twice for a single kill.
	if s.state == newState && (newState == StateFinished || newState == StateError) {
		s.mu.Unlock()
		return
	}
	// When Kill has set killReason, the supervisor loop's race with ctx
	// cancellation can briefly drive the state through StateFinished (the
	// ctx.Err()-check branch at the top of Start's for-loop maps cancelled
	// context to StateFinished). That is wrong for a kill: the canonical
	// outcome of session_kill is StateFinished only when pi exited cleanly
	// on SIGTERM, otherwise StateError. Suppress the intermediate
	// StateFinished from the loop so the kill path produces one terminal
	// transition with the correct reason — Kill itself drives the final
	// setState call.
	if s.killReason != "" && newState == StateFinished && s.state != StateFinished {
		// Re-map to StateError when SIGKILL was already needed; otherwise
		// leave the SIGTERM-clean path to set StateFinished explicitly.
		if s.killReason == "killed_sigkill" || s.killReason == "killed_no_process" {
			s.mu.Unlock()
			return
		}
	}
	s.state = newState
	s.sess.State = newState
	killReason := s.killReason
	s.mu.Unlock()

	// Log the transition into the per-session log so `iris logs <session>`
	// captures lifecycle events even when no harness traffic is in flight.
	s.logf("supervisor: session %s state=%s", s.sess.InstanceID, newState)

	// Always write iris_state to the DB so the restore path can read it.
	if err := s.cfg.Database.IrisUpdateSessionState(s.sess.InstanceID, string(newState)); err != nil {
		s.logf("supervisor: update iris_state: %v", err)
	}

	// Update the sessions DB row end state for terminal states. PR #1657
	// ordering: write the session_end event FIRST (into agent_events) so any
	// subscriber observing the event stream sees the termination reason
	// before the sessions.end_state column reflects it.
	switch newState {
	case StateFinished:
		reason := killReason
		if reason == "" {
			reason = "clean_exit"
		}
		s.writeSessionEndEvent(reason, string(newState))
		if err := s.cfg.Database.UpdateSessionEnded(s.sess.InstanceID, "finished"); err != nil {
			s.logf("supervisor: update session ended: %v", err)
		}
	case StateError:
		reason := killReason
		if reason == "" {
			reason = "error"
		}
		s.writeSessionEndEvent(reason, string(newState))
		if err := s.cfg.Database.UpdateSessionEnded(s.sess.InstanceID, "error"); err != nil {
			s.logf("supervisor: update session ended: %v", err)
		}
	}

	// Notify subscribers of state transitions via the EventPublisher when it
	// implements stateNotifier (the production ClientSocket does; test
	// publishers that only implement Publish are unaffected).
	if s.cfg.Publisher != nil {
		if n, ok := s.cfg.Publisher.(stateNotifier); ok {
			n.PublishState(s.sess.SessionName, string(newState))
		}
	}

	// Issue #1700: deliver a body-bearing prompt to the parent session on
	// terminal state. The notification fires AFTER the event row, the status
	// update, and the PublishState fan-out — it is a downstream effect of
	// the transition, not part of it. We invoke the callback in a goroutine
	// so a blocking socket dial in the prompt-delivery path does not stall
	// the supervisor loop (#1687: external I/O must not run with s.mu held;
	// we are outside the lock here but the goroutine also defends against a
	// future caller that holds an unrelated lock around setState).
	//
	// ParentSession is read from the in-memory SessionRecord which is
	// populated at spawn time from cfg.ParentSession. NULL/empty means
	// top-level spawn — no parent to notify, no-op.
	//
	// parentNotified is a once-per-supervisor latch. The existing setState
	// dedup at the top of this method already suppresses Finished→Finished
	// and Error→Error, and by inspection of Kill / the supervisor loop no
	// Finished→Error or Error→Finished cross-terminal sequence can fire
	// today (Kill returns at <-s.done before its final setState(StateError)
	// on the SIGTERM-clean path, and the SIGKILL path's loop setState is
	// suppressed by the killReason guard). The latch is defence-in-depth
	// against future regressions of those invariants.
	if s.cfg.NotifyParent != nil && s.sess.ParentSession != "" &&
		(newState == StateFinished || newState == StateError) {
		s.mu.Lock()
		alreadyNotified := s.parentNotified
		if !alreadyNotified {
			s.parentNotified = true
		}
		s.mu.Unlock()
		if !alreadyNotified {
			parent := s.sess.ParentSession
			child := s.sess.SessionName
			deliveryID := uuid.New().String()
			notifier := s.cfg.NotifyParent
			go notifier(child, parent, newState, deliveryID)
		}
	}
}

// writeSessionEndEvent writes a session_end row into agent_events with the
// termination reason and terminal state. Mirrors the harness writeEvent
// shape (uuid id, instance_id pointer, payload JSON-encoded). The publisher
// fan-out is invoked when configured so subscribed clients see the
// session_end event the same way they see a session_status or tool_call
// frame.
func (s *Supervisor) writeSessionEndEvent(reason, state string) {
	payload, err := json.Marshal(map[string]any{
		"type":   "session_end",
		"reason": reason,
		"state":  state,
	})
	if err != nil {
		s.logf("supervisor: marshal session_end payload: %v", err)
		return
	}
	iid := s.sess.InstanceID
	var iidPtr *string
	if iid != "" {
		iidPtr = &iid
	}
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: s.sess.SessionName,
		Worktree:    s.sess.Worktree,
		Type:        "session_end",
		Payload:     string(payload),
		CreatedAt:   time.Now(),
		InstanceID:  iidPtr,
	}
	rowID, err := s.cfg.Database.WriteEventReturningRowID(event)
	if err != nil {
		s.logf("supervisor: write session_end event: %v", err)
		return
	}
	if s.cfg.Publisher != nil {
		s.cfg.Publisher.Publish(EventPublication{
			SessionName: s.sess.SessionName,
			RowID:       rowID,
			EventType:   "session_end",
			Payload:     string(payload),
		})
	}
}

// insertSessionRecord writes the initial session row to the DB.
func insertSessionRecord(database *db.DB, sess SessionRecord) error {
	role := sess.Role
	dbSess := db.Session{
		InstanceID:  sess.InstanceID,
		SessionName: sess.SessionName,
		AgentRole:   &role,
		Repo:        "", // iris does not carry repo separately in D-3
		Worktree:    sess.Worktree,
		Harness:     "pi",
		StartedAt:   sess.StartedAt,
	}
	if sess.ParentSession != "" {
		ps := sess.ParentSession
		dbSess.ParentSession = &ps
	}
	return database.InsertSession(dbSess)
}

// sessionLogPrefix computes a log prefix for the given supervisor config.
func sessionLogPrefix(cfg SupervisorConfig) string {
	name := cfg.SessionName
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("[iris:%s]", name)
}

// formatDuration formats a duration for human-readable logging.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// rpcAbortCmd is the JSON payload for the pi abort RPC command.
var rpcAbortCmd = map[string]any{"type": "abort"}

// DefaultKillTimeout is the SIGTERM grace period applied by Kill when the
// caller passes 0. After this elapses without pi exiting cleanly, Kill
// escalates to SIGKILL on the underlying process.
const DefaultKillTimeout = 5 * time.Second

// State returns the supervisor's current session state under the lock.
func (s *Supervisor) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Done returns a channel that is closed when the supervisor's Start
// goroutine returns. Useful for callers that want to wait for full session
// teardown without polling SessionRecord().State.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// Kill terminates the pi child managed by this supervisor.
//
//  1. If the session is already terminal (finished/error) the call is a
//     no-op and returns (state, nil). This is the idempotent path — callers
//     that re-kill an already-dead session see success.
//  2. Otherwise the per-session context is cancelled; exec.CommandContext
//     delivers SIGTERM to pi. Kill then waits up to timeout (or
//     DefaultKillTimeout when timeout is 0) for the Start goroutine to
//     finish.
//  3. If pi has not exited by the deadline, Kill sends SIGKILL directly to
//     the recorded process handle and waits a further 2 seconds for the
//     goroutine to converge. The terminal state in that case is
//     StateError; the clean SIGTERM path produces StateFinished.
//
// Kill is safe to call concurrently from multiple goroutines; the lock
// protects the cancel/process handles and the Done channel disambiguates
// the convergence.
func (s *Supervisor) Kill(ctx context.Context, timeout time.Duration) (SessionState, error) {
	if timeout <= 0 {
		timeout = DefaultKillTimeout
	}

	s.mu.Lock()
	current := s.state
	s.mu.Unlock()
	if current == StateFinished || current == StateError {
		return current, nil
	}

	// Step 1: cancel the per-session context. spawnAndRun configures
	// cmd.Cancel to send SIGTERM (overriding the default SIGKILL) so this
	// triggers a graceful shutdown attempt rather than an immediate kill.
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		// Start() has not been entered yet — there is no pi child to signal.
		// Mark the session error and let the caller proceed.
		s.mu.Lock()
		s.killReason = "killed_no_process"
		s.mu.Unlock()
		s.setState(StateError)
		return StateError, fmt.Errorf("iris kill: supervisor for %q has no live context", s.sess.SessionName)
	}
	s.mu.Lock()
	// Default to the SIGTERM-clean reason; if we escalate to SIGKILL below
	// we'll overwrite it before setState runs.
	s.killReason = "killed_sigterm"
	s.mu.Unlock()
	s.logf("supervisor: session %s: kill requested, sending SIGTERM via ctx cancel", s.sess.InstanceID)
	cancel()

	// Step 2: wait up to timeout for Start to exit cleanly.
	select {
	case <-s.done:
		return s.State(), nil
	case <-time.After(timeout):
	case <-ctx.Done():
		return s.State(), ctx.Err()
	}

	// Step 3: SIGKILL escalation.
	s.mu.Lock()
	proc := s.process
	s.mu.Unlock()
	s.mu.Lock()
	s.killReason = "killed_sigkill"
	s.mu.Unlock()
	if proc != nil {
		s.logf("supervisor: session %s: SIGTERM grace expired after %s, sending SIGKILL", s.sess.InstanceID, timeout)
		if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			s.logf("supervisor: session %s: SIGKILL: %v", s.sess.InstanceID, err)
		}
	} else {
		s.logf("supervisor: session %s: SIGTERM grace expired with no live process handle", s.sess.InstanceID)
	}

	// Wait briefly for the supervisor goroutine to converge. We bound this
	// so a wedged Wait() does not hang the daemon — a SIGKILLed process
	// almost always reaps within milliseconds.
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		s.logf("supervisor: session %s: supervisor goroutine did not converge after SIGKILL", s.sess.InstanceID)
	case <-ctx.Done():
		return s.State(), ctx.Err()
	}

	// Force the terminal state to StateError so the SIGKILL outcome is
	// distinguishable from a clean SIGTERM exit — setState is idempotent on
	// the DB side, so re-asserting it here when the supervisor loop already
	// did is safe.
	s.setState(StateError)
	return StateError, nil
}

// Escalate transitions the session from StateActive to StateEscalated.
//
// Issue #1693: this is the worker-side of `iris escalate` — the daemon
// records that the worker has handed a question to the coordinator and is
// pausing until guidance arrives. The pi child is NOT stopped; only the
// surrounding state machine flips. Any subsequent prompt_deliver (from any
// source) calls Resume() to flip back to StateActive.
//
// Returns an error when the current state is not StateActive — escalating
// from spawning, finished, error, or already-escalated is a no-op for the
// already-escalated case and an error for the terminal/spawning cases. The
// already-escalated branch is idempotent so concurrent escalate calls don't
// produce spurious DB writes.
func (s *Supervisor) Escalate() error {
	s.mu.Lock()
	current := s.state
	s.mu.Unlock()
	switch current {
	case StateEscalated:
		return nil // idempotent — already escalated
	case StateActive:
		// proceed below
	default:
		return fmt.Errorf("iris escalate: session %q is in state %q; escalate requires active", s.sess.SessionName, current)
	}
	s.setState(StateEscalated)
	return nil
}

// Resume transitions the session from StateEscalated back to StateActive.
//
// Called by the client socket's prompt_deliver handler so the worker resumes
// as soon as ANY prompt arrives — from the coordinator's reply, a human
// typing via `iris prompt`, or any other source. Idempotent when the session
// is already in StateActive. No-op for terminal or spawning states (the
// session is past the point where escalated-resume makes sense).
func (s *Supervisor) Resume() {
	s.mu.Lock()
	current := s.state
	s.mu.Unlock()
	if current != StateEscalated {
		return
	}
	s.setState(StateActive)
}

// StopGracefully sends abort to pi, waits up to shutdownTimeout for it to exit.
// If it doesn't exit, SIGTERM is sent.
func (s *Supervisor) StopGracefully(ctx context.Context, _ *exec.Cmd) {
	_ = s.Abort()
	select {
	case <-ctx.Done():
	case <-time.After(s.cfg.ShutdownTimeout):
	}
	// The supervisor loop will send SIGKILL via exec.CommandContext on context
	// cancellation. We don't need to do it explicitly here — ctx cancel handles it.
}

// SessionDir returns the per-session run directory for this supervisor.
func (s *Supervisor) SessionDir() string {
	return filepath.Join(s.cfg.RunDir, s.sess.InstanceID)
}

// splitLines is a helper for the RPC observation goroutine.
func splitLines(r io.Reader) []string {
	scanner := bufio.NewScanner(r)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

// SpawnSession is the high-level entry point that creates a Supervisor,
// starts the pi child, and blocks until the session terminates.
//
// This function is the D-3 equivalent of the spawn path that will eventually
// be triggered by a client IPC session_spawn frame (D-6). For now it provides
// a direct programmatic spawn used by the iris startup command and tests.
func SpawnSession(ctx context.Context, cfg SupervisorConfig) (*Supervisor, error) {
	sup, err := NewSupervisor(cfg)
	if err != nil {
		return nil, fmt.Errorf("iris: spawn session: %w", err)
	}
	go sup.Start(ctx)
	return sup, nil
}

// PiSessionPathFromSessionID reconstructs the expected pi JSONL session file
// path from the encoded-cwd and a session UUID prefix. Implements the path
// format documented in pi-rpc-interface.md Q5.
//
// encodedCwd is the result of:
//
//	"--" + cwd.replace(/^[\/\\]/, "").replace(/[\/\\:]/g, "-") + "--"
//
// This function is provided for D-9 (daemon restart / orphan detection) which
// needs to pass --session <path> to the restarted pi child.
func PiSessionPathFromSessionID(piAgentDir, encodedCwd, sessionID string) (string, error) {
	sessionsDir := filepath.Join(piAgentDir, "sessions", encodedCwd)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "", fmt.Errorf("iris: list pi sessions for cwd %q: %w", encodedCwd, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Session filenames are <timestamp>_<uuid>.jsonl — match by UUID suffix.
		if strings.HasSuffix(name, ".jsonl") && strings.Contains(name, sessionID) {
			return filepath.Join(sessionsDir, name), nil
		}
	}
	return "", fmt.Errorf("iris: pi session %q not found under %q", sessionID, sessionsDir)
}

// GenerateSessionName generates a default session name from a worktree path.
// The format is "<repo>/<branch>" where:
//
//   - <branch> is the final path component of the worktree (in the standard
//     bare+worktree layout, the worktree directory is named after the branch);
//   - <repo>   is the parent directory's name (the repo's bare/worktree
//     container).
//
// This is used by the daemon when a client sends a session_spawn frame
// without specifying a session name. Including the repo in the default
// name prevents collisions across repos that share a branch name
// (e.g. `main` in `nixos-config` and `main` in `hass-config`) — see
// issue #1738.
//
// The role parameter is intentionally not part of the name: role is
// already a separate field on SessionSnapshot and surfaced in its own
// column. The historical "iris-<role>@" prefix was a tmux-coexistence
// holdover from when iris and prism shared a tmux server; iris no longer
// runs under tmux, so the prefix has been dropped.
//
// Slash in the returned name: callers that derive filesystem paths from
// the session name must decide how to handle the embedded '/'. iris
// currently makes two choices:
//
//   - Per-session log files (Paths.SessionLogPath) sanitise '/' to '_'
//     so the log file is flat (`<repo>_<branch>.log`).
//   - The archive layout accepts the slash as a real subdirectory, so
//     archives are naturally grouped by repo on disk
//     (`<archive-root>/<repo>/<branch>/<instance>/raw/session.jsonl`).
//
// Examples:
//
//	GenerateSessionName("/home/user/code/my-project/main")
//	  → "my-project/main"
//	GenerateSessionName("/home/user/code/hass-config/test")
//	  → "hass-config/test"
//	GenerateSessionName("/foo")
//	  → "session/foo"   (no parent directory; safe fallback)
func GenerateSessionName(worktree string) string {
	abs, err := filepath.Abs(worktree)
	if err != nil || abs == "" {
		abs = worktree
	}
	branch := filepath.Base(abs)
	if branch == "" || branch == "." || branch == "/" {
		branch = "default"
	}
	parent := filepath.Dir(abs)
	repo := filepath.Base(parent)
	if repo == "" || repo == "." || repo == "/" {
		repo = "session"
	}
	return fmt.Sprintf("%s/%s", repo, branch)
}
