//go:build darwin

package cmd

// agent_run_sandbox_exec_darwin.go — Darwin implementation of runAgentRunSandboxExec.
//
// This file contains the sandbox-exec dispatch path for the agent-run command.
// It switches from the syscall.Exec model (used in PRs #1016 and #1017) to a
// supervised child process model (os/exec.Command) so that:
//
//  1. A kqueue-based parent-death watcher can be installed (see lifecycle_darwin.go).
//  2. SIGTERM/SIGINT/SIGHUP are forwarded cleanly to the sandbox-exec child.
//  3. A stderr tee can capture sandbox-exec startup errors to the agent-run log.
//
// The non-Darwin stub (agent_run_sandbox_exec_other.go) returns an error
// immediately. The dispatch in runAgentRun (agent_run.go) already guards against
// non-Darwin via a runtime.GOOS check, so the stub is unreachable in practice.

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
)

// runAgentRunSandboxExec is the sandbox-exec dispatch path. It reconstructs
// the container.Config from the session's DB status, calls
// Manager.PrepareSandboxExec to materialise the SBPL profile, and then runs
// sandbox-exec as a supervised child process (using os/exec.Command).
//
// PR 4 (#1018) switches from syscall.Exec to os/exec.Command + manual signal
// forwarding so that a kqueue-based parent-death watcher can be installed. The
// watcher watches the parent PID (the tmux pane process, captured via
// os.Getppid() BEFORE launching the child) and, on parent exit, sends SIGTERM
// then SIGKILL (3-second grace) to the sandbox-exec child.
//
// Security: parentPID is read once from os.Getppid() at function entry and is
// never sourced from environment-supplied or user-supplied values.
//
// agentRunStart and logFile are passed through from runAgentRun so that the
// sandbox-exec path can emit `[timing]` markers symmetric to the bwrap path
// (#1052). All durations are relative to agentRunStart.
func runAgentRunSandboxExec(sessionName string, status *db.Status, agentRunStart time.Time, logFile *os.File) error {
	// Capture the parent PID immediately — this is the tmux pane process.
	// os.Getppid() returns the real parent PID of this process (agent-run),
	// which is the shell/tmux process managing the pane. We capture it here,
	// before any goroutines or child processes are involved, so there is no
	// window for it to be altered by environment variables or other input.
	//
	// Security AC: parentPID is never sourced from environment variables or
	// user-controlled input — os.Getppid() is read from the kernel directly.
	parentPID := os.Getppid()

	// Load prism config for git identity and SSH key names. Mirrors the
	// bwrap path so future PRs in this design can extend the Manager.Config
	// uniformly.
	cfg := config.Load()

	worktree := status.Worktree
	if worktree == "" {
		return fmt.Errorf("agent-run: session %q has no recorded worktree", sessionName)
	}
	bareRoot := git.BareRoot(worktree)
	var worktreeGitDir string
	if bareRoot != "" {
		worktreeGitDir = filepath.Join(bareRoot, ".bare", "worktrees", filepath.Base(worktree))
	}

	port := 0
	if status.HarnessPort != nil {
		port = *status.HarnessPort
	}

	agentRole := ""
	if status.RootAgentName != nil {
		agentRole = *status.RootAgentName
	}
	if agentRole == "" {
		agentRole = session.DefaultAgent(worktree, "")
	}

	hostAPISockPath := ""
	if sockPath, sockErr := session.SidecarHostAPIPath(sessionName); sockErr == nil {
		hostAPISockPath = sockPath
	}

	var agentEnvVars map[string]string
	if pf, pfErr := config.LoadProfiles(); pfErr == nil && pf != nil {
		agentEnvVars = pf.AgentEnvVars
	}

	// Resolve the harness name from the DB status. Fall back to "opencode"
	// for pre-registry rows that have a NULL harness column.
	sandboxHarnessName := "opencode"
	if status.Harness != nil && *status.Harness != "" {
		sandboxHarnessName = *status.Harness
	}

	sandboxHarness, _ := harness.New(sandboxHarnessName, "", nil, "", "")
	var sandboxRuntimeEnv map[string]string
	if sandboxHarness != nil {
		sandboxRuntimeEnv = sandboxHarness.RuntimeEnv()
	}
	// Resolve instance ID from DB status. Required so the staging HOME is
	// namespaced by the same instance_id that prism cleanup uses.
	instanceID := ""
	if status.InstanceID != nil {
		instanceID = *status.InstanceID
	}

	ctrCfg := container.Config{
		SessionName:       sessionName,
		Worktree:          worktree,
		BareRoot:          bareRoot,
		WorktreeGitDir:    worktreeGitDir,
		AllocatedPort:     port,
		AgentRole:         agentRole,
		GitUserName:       cfg.GitUserName,
		GitUserEmail:      cfg.GitUserEmail,
		SshAccessKeyName:  cfg.SshAccessKeyName,
		SshSigningKeyName: cfg.SshSigningKeyName,
		HostAPISockPath:   hostAPISockPath,
		InstanceID:        instanceID,
		RuntimeEnv:        sandboxRuntimeEnv,
		AgentEnvVars:      agentEnvVars,
	}

	applyInitialPromptEnvVar(&ctrCfg)

	m := container.New(ctrCfg)

	// `[timing] pre-exec`: covers DB lookup, config load, profile lookup,
	// Manager construction. Symmetric to the bwrap path's pre-exec marker
	// (see runAgentRun in agent_run.go).
	logTimingTo(logFile, "pre-exec", time.Since(agentRunStart))

	argsBuildStart := time.Now()
	args, err := m.PrepareSandboxExec()
	if err != nil {
		return fmt.Errorf("agent-run: prepare sandbox-exec args: %w", err)
	}
	// `[timing] sandbox-exec args build`: time spent writing the SBPL profile
	// and assembling the sandbox-exec argv. Mirrors `[timing] bwrap-args build`.
	logTimingTo(logFile, "sandbox-exec args build", time.Since(argsBuildStart))

	if err := validateSandboxExecArgs(args); err != nil {
		return err
	}

	// Write Claude Code credentials into the staging HOME.
	//
	// In bwrap/podman mode, writeClaudeCredentials() writes a temp file that
	// is bind-mounted at /root/.claude/.credentials.json inside the container.
	// sandbox-exec has no bind-mount mechanism, so we write the credentials
	// directly to $STAGING_HOME/.claude/.credentials.json instead. The staging
	// HOME's .claude/ is a symlink to the real ~/.claude/, so the write lands
	// at the host path — which is in the SBPL profile's RW allow set for the
	// .claude symlink target. On macOS, credentials live in the Keychain and
	// ~/.claude/.credentials.json is absent or empty; opencode-claude-auth
	// reads it at startup and fails silently if it is missing.
	if stagingHomeCreds, credErr := m.SandboxExecHomePath(); credErr == nil && stagingHomeCreds != "" {
		credsDst := filepath.Join(stagingHomeCreds, ".claude", ".credentials.json")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, keychainErr := exec.CommandContext(ctx, "security", "find-generic-password",
			"-l", "Claude Code-credentials", "-w").Output()
		if keychainErr == nil {
			creds := strings.TrimSpace(string(out))
			if creds != "" {
				if writeErr := os.WriteFile(credsDst, []byte(creds), 0o600); writeErr != nil {
					log.Printf("agent-run: sandbox-exec: write claude credentials: %v", writeErr)
				}
			}
		}
	}

	// Build the env for sandbox-exec. The slice is K=V pairs (os/exec uses the
	// same format as syscall.Exec).
	//
	//  1. Start with the filtered minimal allow-list (PATH, HOME, USER, …).
	//  2. Override HOME with the staging HOME path.
	//  3. Inject credential env vars (LLM API keys, GITHUB_TOKEN).
	//  4. Inject harness/profile runtime env vars and prism context vars.
	env := container.MinimalIsolatedExecEnv(os.Environ())

	// Override HOME → staging HOME so the sandbox sees the staged layout.
	// The staging HOME was created by PrepareSandboxExecHome() inside
	// PrepareSandboxExec(). Only override when the staging dir exists on disk.
	//
	// Also inject OPENCODE_TEST_HOME with the same staging path. opencode's
	// global.ts resolves Path.home via os.homedir() which on macOS calls
	// NSHomeDirectory()/getpwuid() — both ignore the HOME env var and return
	// the real home from the directory services database. OPENCODE_TEST_HOME
	// is the official override hook in global.ts:
	//   get home() { return process.env.OPENCODE_TEST_HOME ?? os.homedir() }
	// Without it, opencode probes (and tries to mkdir) paths under the real
	// $HOME even when HOME is overridden, causing EPERM under deny-default.
	if stagingHome, stagingErr := m.SandboxExecHomePath(); stagingErr == nil && stagingHome != "" {
		if _, statErr := os.Stat(stagingHome); statErr == nil {
			// Strip any existing HOME and XDG vars from the filtered env —
			// we replace them all with staging-HOME-relative paths below.
			xdgKeys := map[string]bool{
				"HOME":              true,
				"XDG_CACHE_HOME":   true,
				"XDG_DATA_HOME":    true,
				"XDG_CONFIG_HOME":  true,
				"XDG_STATE_HOME":   true,
				"OPENCODE_TEST_HOME": true,
			}
			filtered := make([]string, 0, len(env))
			for _, kv := range env {
				eq := -1
				for i := 0; i < len(kv); i++ {
					if kv[i] == '=' {
						eq = i
						break
					}
				}
				if eq > 0 && xdgKeys[kv[:eq]] {
					continue
				}
				filtered = append(filtered, kv)
			}
			// Resolve the real home directory for XDG_DATA_HOME and
			// XDG_STATE_HOME. These must point to the host paths so all
			// sessions share a single opencode DB and state store.
			realHome, realHomeErr := os.UserHomeDir()
			if realHomeErr != nil {
				// os.UserHomeDir() failed: fall back to the staging HOME for
				// XDG_DATA_HOME/XDG_STATE_HOME rather than emitting invalid
				// paths like "/.local/state" that would write into a
				// root-owned directory. This is extremely unlikely on macOS
				// (the stagingHome derivation above also calls UserHomeDir and
				// succeeded, so this path should be unreachable in practice).
				realHome = stagingHome
			}
			env = append(filtered,
				"HOME="+stagingHome,
				// OPENCODE_TEST_HOME overrides os.homedir() inside opencode.
				// opencode's global.ts uses os.homedir() (which on macOS calls
				// NSHomeDirectory()/getpwuid() and ignores $HOME) for Path.home.
				// OPENCODE_TEST_HOME is the official escape hatch:
				//   get home() { return process.env.OPENCODE_TEST_HOME ?? os.homedir() }
				"OPENCODE_TEST_HOME="+stagingHome,
				// XDG_CACHE_HOME and XDG_CONFIG_HOME point into the staging
				// HOME so opencode's module-load mkdir calls land inside
				// sandbox-allowed paths.
				"XDG_CACHE_HOME="+filepath.Join(stagingHome, ".cache"),
				"XDG_CONFIG_HOME="+filepath.Join(stagingHome, ".config"),
				// XDG_DATA_HOME and XDG_STATE_HOME must be set explicitly to
				// the real host paths. Without them, opencode derives these
				// from OPENCODE_TEST_HOME (the staging HOME) and tries to
				// mkdir ~/.local/state inside the staging directory — a path
				// that is not in the SBPL profile's RW allow set. Setting them
				// explicitly ensures opencode writes its DB and state to the
				// shared host locations (both of which ARE in the profile's RW
				// block).
				"XDG_DATA_HOME="+filepath.Join(realHome, ".local", "share"),
				"XDG_STATE_HOME="+filepath.Join(realHome, ".local", "state"),
			)
		}
	}

	// Inject credential env vars (LLM API keys, GITHUB_TOKEN).
	env = append(env, m.CredentialEnvVars()...)

	// Inject harness-specific runtime env vars and profile AgentEnvVars.
	env = container.AppendSandboxEnvVarsKV(env, ctrCfg)

	// Inject prism context vars.
	if ctrCfg.Worktree != "" {
		env = append(env, "PRISM_SPAWN_PATH="+ctrCfg.Worktree)
	}
	if ctrCfg.BareRoot != "" {
		env = append(env, "PRISM_BARE_ROOT="+ctrCfg.BareRoot)
	}
	env = append(env, "PRISM_SESSION_NAME="+ctrCfg.SessionName)
	if ctrCfg.HostAPISockPath != "" {
		env = append(env, "PRISM_HOST_API=unix://"+ctrCfg.HostAPISockPath)
	}

	// argv[0] is "sandbox-exec" (from BuildArgs); the well-known binary path
	// on macOS is /usr/bin/sandbox-exec.
	const sandboxExecBinary = "/usr/bin/sandbox-exec"

	// Build the supervised child command. We pass args[1:] because exec.Command
	// prepends argv[0] (the binary path) automatically. args[0] is "sandbox-exec"
	// (BuildArgs convention matching the bwrap args[0]="bwrap" convention).
	sandboxCmd := exec.Command(sandboxExecBinary, args[1:]...)
	sandboxCmd.Env = env

	// Inherit stdin/stdout from agent-run so the sandbox sees the tmux pane's
	// terminal. stderr is piped so startup errors can be tee'd to the log.
	sandboxCmd.Stdin = os.Stdin
	sandboxCmd.Stdout = os.Stdout

	var sandboxStderrR, sandboxStderrW *os.File
	if pipeErr := func() error {
		var e error
		sandboxStderrR, sandboxStderrW, e = os.Pipe()
		return e
	}(); pipeErr != nil {
		fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot create sandbox-exec stderr pipe: %v — stderr will not be logged\n", pipeErr)
	}

	if sandboxStderrW != nil {
		sandboxCmd.Stderr = sandboxStderrW
	} else {
		sandboxCmd.Stderr = os.Stderr
	}

	// Place sandbox-exec in its own process group for clean signal forwarding.
	sandboxCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// `[timing] sandbox-exec exec`: total wall time from agent-run entry to
	// the moment of cmd.Start (fork+exec). Mirrors `[timing] bwrap exec`.
	logTimingTo(logFile, "sandbox-exec exec", time.Since(agentRunStart))

	if err := sandboxCmd.Start(); err != nil {
		return fmt.Errorf("agent-run: start sandbox-exec: %w", err)
	}

	// Close the write end of the stderr pipe in the parent now that sandbox-exec
	// has inherited it. Required so reads from the read end return EOF when
	// sandbox-exec exits.
	if sandboxStderrW != nil {
		sandboxStderrW.Close()
	}

	// Give the terminal foreground to sandbox-exec's process group so that
	// the host PTY routes keypresses to sandbox-exec/opencode, not agent-run.
	origPgid := tcsetpgrpForeground(int(os.Stdin.Fd()), sandboxCmd.Process.Pid)

	// childExited is closed when cmd.Wait() returns — used by the parent-death
	// watcher to avoid sending spurious signals after the child has already exited.
	childExited := make(chan struct{})

	// Install the parent-death watcher in a goroutine. parentPID was captured
	// via os.Getppid() at the start of this function — before any forking —
	// and is never sourced from environment variables or user input.
	//
	// The watcher kills the child when parentPID exits: SIGTERM, then SIGKILL
	// after a 3-second grace. If kqueue setup fails, a warning is logged and
	// the 1-second heartbeat fallback is used instead (see lifecycle_darwin.go).
	const parentDeathGrace = 3 * time.Second
	go watchParentDeathAndKill(parentPID, sandboxCmd.Process, childExited, parentDeathGrace)

	// Forward SIGTERM, SIGINT, and SIGHUP to the sandbox-exec process group.
	stopForward := make(chan struct{})
	go forwardSignalsToSandboxExec(sandboxCmd.Process, stopForward)

	// Tee sandbox-exec's stderr to both the pane and the log file.
	teeDone := make(chan struct{})
	if sandboxStderrR != nil {
		go func() {
			defer close(teeDone)
			var logWriter io.Writer
			if logFile != nil {
				logWriter = logFile
			}
			teePipe(sandboxStderrR, os.Stderr, logWriter)
			sandboxStderrR.Close()
		}()
	} else {
		close(teeDone)
	}

	waitErr := sandboxCmd.Wait()

	// Signal goroutines that the child has exited.
	close(childExited)
	close(stopForward)
	<-teeDone

	// Restore the original foreground process group.
	if origPgid > 0 {
		_ = tcsetpgrpRestore(int(os.Stdin.Fd()), origPgid)
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("agent-run: wait sandbox-exec: %w", waitErr)
	}
	return nil
}
