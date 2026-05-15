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
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// SupervisorConfig holds the spawn parameters for one pi session.
type SupervisorConfig struct {
	// SessionName is the logical session name (e.g. "nixos-config@my-branch").
	SessionName string
	// Worktree is the absolute path to the git worktree.
	Worktree string
	// Role is the agent role ("worker", "coordinator", etc.).
	Role string
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
	// Database is the open iris DB (used for event writes).
	Database *db.DB
}

// Supervisor manages a single pi child process.
type Supervisor struct {
	cfg      SupervisorConfig
	sess     SessionRecord
	harness  *HarnessSocketServer

	// stdinPipe is the write end of pi's stdin (for sending RPC commands).
	stdinPipe io.WriteCloser

	mu    sync.Mutex
	state SessionState
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

	// Insert the session record into the DB.
	if err := insertSessionRecord(cfg.Database, sess); err != nil {
		// Non-fatal: log and continue.
		log.Printf("[iris] supervisor: failed to insert session record: %v", err)
	}

	return &Supervisor{
		cfg:     cfg,
		sess:    sess,
		harness: harness,
		state:   StateSpawning,
	}, nil
}

// InstanceID returns the session instance ID.
func (s *Supervisor) InstanceID() string { return s.sess.InstanceID }

// SessionRecord returns a copy of the session record.
func (s *Supervisor) SessionRecord() SessionRecord { return s.sess }

// Start spawns the pi child and runs the supervisor loop. It blocks until the
// session reaches a terminal state (finished or error) or ctx is cancelled.
// Start is intended to be called in its own goroutine.
func (s *Supervisor) Start(ctx context.Context) {
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
				log.Printf("[iris] supervisor: session %s: pi exited 0 without session_shutdown (anomaly)", s.sess.InstanceID)
			}
			s.setState(StateFinished)
			return
		}

		// Non-zero exit.
		s.sess.RestartCount++
		if s.sess.RestartCount >= s.cfg.RestartThreshold {
			log.Printf("[iris] supervisor: session %s: circuit breaker opened after %d failures", s.sess.InstanceID, s.sess.RestartCount)
			s.setState(StateError)
			return
		}

		backoff := RestartBackoff(s.sess.RestartCount)
		log.Printf("[iris] supervisor: session %s: pi exited non-zero (attempt %d/%d), restarting in %v",
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
			log.Printf("[iris] supervisor: pi not found on PATH: %v", err)
			return 1
		}
	}

	// Write a per-session pi config that loads the prism extension.
	piConfigDir, err := s.writePerSessionPIConfig()
	if err != nil {
		log.Printf("[iris] supervisor: write pi config: %v", err)
		return 1
	}

	// Build the pi command.
	args := []string{"--mode", "rpc"}
	if s.cfg.ExtensionPath != "" {
		args = append(args, "--extension", s.cfg.ExtensionPath)
	}

	cmd := exec.CommandContext(ctx, piBin, args...)
	cmd.Dir = s.cfg.Worktree
	cmd.Env = s.buildEnv(piConfigDir)

	// Wire stdin/stdout pipes.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[iris] supervisor: stdin pipe: %v", err)
		return 1
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[iris] supervisor: stdout pipe: %v", err)
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
		log.Printf("[iris] supervisor: start pi: %v", err)
		return 1
	}

	s.mu.Lock()
	s.stdinPipe = stdinPipe
	s.mu.Unlock()

	log.Printf("[iris] supervisor: spawned pi pid=%d (session %s)", cmd.Process.Pid, s.sess.InstanceID)
	s.setState(StateActive)

	// Run the harness socket acceptor in a goroutine.
	harnessCtx, harnessCancel := context.WithCancel(ctx)
	defer harnessCancel()
	go func() {
		if err := s.harness.AcceptOne(harnessCtx); err != nil && harnessCtx.Err() == nil {
			log.Printf("[iris] supervisor: harness accept error (session %s): %v", s.sess.InstanceID, err)
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
		log.Printf("[iris] supervisor: RPC parse error: %v", err)
		return
	}

	switch generic.Type {
	case "agent_start":
		log.Printf("[iris] supervisor: session %s: agent_start", s.sess.InstanceID)
	case "agent_end":
		log.Printf("[iris] supervisor: session %s: agent_end", s.sess.InstanceID)
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
		log.Printf("[iris] supervisor: open log file %q: %v (stderr will be discarded)", logPath, err)
		return nil
	}
	return f
}

// setState updates the in-memory and DB state of the session.
func (s *Supervisor) setState(newState SessionState) {
	s.mu.Lock()
	s.state = newState
	s.sess.State = newState
	s.mu.Unlock()

	// Update the sessions DB row end state for terminal states.
	switch newState {
	case StateFinished:
		if err := s.cfg.Database.UpdateSessionEnded(s.sess.InstanceID, "finished"); err != nil {
			log.Printf("[iris] supervisor: update session ended: %v", err)
		}
	case StateError:
		if err := s.cfg.Database.UpdateSessionEnded(s.sess.InstanceID, "error"); err != nil {
			log.Printf("[iris] supervisor: update session ended: %v", err)
		}
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

// GenerateSessionName generates a default session name from a worktree path
// and role. The format is "iris-<role>@<basename>" where basename is the
// last path component of the worktree. This is used by the daemon when a
// client sends a session_spawn frame without specifying a session name.
//
// Examples:
//
//	GenerateSessionName("/home/user/code/my-project", "worker")
//	  → "iris-worker@my-project"
func GenerateSessionName(worktree, role string) string {
	base := filepath.Base(worktree)
	if base == "" || base == "." || base == "/" {
		base = "session"
	}
	return fmt.Sprintf("iris-%s@%s", role, base)
}
