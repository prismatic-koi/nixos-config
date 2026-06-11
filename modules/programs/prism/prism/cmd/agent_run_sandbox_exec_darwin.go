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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
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

	// Resolve the harness name from the DB status. Fall back to "pi"
	// for pre-registry rows that have a NULL harness column.
	sandboxHarnessName := "pi"
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

	// Carry the persisted harness session UUID through to ctrCfg so that
	// PIInvocation can append --session <id> for conversation resume on
	// restore (issue #1838). Empty when the harness never started.
	sandboxHarnessSessionID := ""
	if status.HarnessSessionID != nil {
		sandboxHarnessSessionID = *status.HarnessSessionID
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
		SshBin:            cfg.SshBin,
		GitHubTokenPath:   cfg.GitHubTokenPath,
		HostAPISockPath:   hostAPISockPath,
		InstanceID:        instanceID,
		RuntimeEnv:        sandboxRuntimeEnv,
		AgentEnvVars:      agentEnvVars,
		Harness:           sandboxHarnessName,
		HarnessSessionID:  sandboxHarnessSessionID,
	}

	// For socket-pipe harnesses (PI), the sidecar stores the TCP port it
	// allocated in harness_port. Propagate it so PRISM_HARNESS_PIPE is injected.
	if hShape, hOK := harness.ShapeOf(sandboxHarnessName); hOK && hShape == harness.TransportSocketPipe {
		if status.HarnessPort != nil {
			ctrCfg.HarnessPipeTCPPort = *status.HarnessPort
		}
	}

	// PI harness: populate PI-specific config fields (system-prompt, extension
	// dir, provider/model/thinking) from the active profile slot.
	// On Darwin sandbox-exec there are no bind-mounts, so the "sandbox paths"
	// are the same as the host paths — sandbox-exec shares the host filesystem.
	if sandboxHarnessName == "pi" {
		if piErr := populatePIConfig(&ctrCfg, sessionName, agentRole, cfg, loadAgentRunOverrides(sessionName)); piErr != nil {
			return fmt.Errorf("agent-run: %w", piErr)
		}
		// sandbox-exec shares the host filesystem, so the in-sandbox paths for
		// the PI agent config directory and extension directory are the same as
		// the host paths. Override the bwrap-oriented sandbox-path defaults so
		// that PIInvocation and PI_CODING_AGENT_DIR reference host-accessible
		// paths directly.
		ctrCfg.PIAgentConfigSandboxDir = ctrCfg.PIAgentConfigHostDir
		ctrCfg.PIExtensionSandboxDir = ctrCfg.PIExtensionHostDir
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
	if stagingHome, stagingErr := m.SandboxExecHomePath(); stagingErr == nil && stagingHome != "" {
		if _, statErr := os.Stat(stagingHome); statErr == nil {
			// Strip any existing HOME and XDG vars from the filtered env —
			// we replace them all with staging-HOME-relative paths below.
			xdgKeys := map[string]bool{
				"HOME":              true,
				"XDG_CACHE_HOME":    true,
				"XDG_DATA_HOME":     true,
				"XDG_CONFIG_HOME":   true,
				"XDG_STATE_HOME":    true,
				"CFFIXED_USER_HOME": true,
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
			// sessions share a single agent DB and state store.
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
				// XDG_CACHE_HOME and XDG_CONFIG_HOME point into the staging
				// HOME so the agent's module-load mkdir calls land inside
				// sandbox-allowed paths.
				"XDG_CACHE_HOME="+filepath.Join(stagingHome, ".cache"),
				"XDG_CONFIG_HOME="+filepath.Join(stagingHome, ".config"),
				// XDG_DATA_HOME and XDG_STATE_HOME point to the real host
				// paths so all sessions share a single agent DB and state
				// store (both of which ARE in the SBPL profile's RW block).
				"XDG_DATA_HOME="+filepath.Join(realHome, ".local", "share"),
				"XDG_STATE_HOME="+filepath.Join(realHome, ".local", "state"),
				// CFFIXED_USER_HOME redirects CoreFoundation's NSHomeDirectory()
				// to the staging HOME so chromium (Google Chrome for Testing,
				// invoked via playwright-cli) writes its crash database, code
				// cache, profile, and SingletonLock under
				//   <stagingHome>/Library/Application Support/Google/...
				// rather than under the real ~/Library/Application Support/...
				// which would either leak the daily-driver Chrome profile or
				// EPERM on every xattr write. Chromium uses NSHomeDirectory()
				// (CoreFoundation) and not getenv("HOME") for the user-data
				// directory root — setting HOME alone is insufficient. Issue
				// #2021.
				"CFFIXED_USER_HOME="+stagingHome,
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
	// For socket-pipe harnesses (PI) on Darwin, the sidecar allocates a TCP
	// port at startup and stores it in harness_port. Expose it here so the
	// harness can connect back to the sidecar's pipe listener.
	//
	// sandbox-exec runs directly on the host — there is no VM, no gvproxy,
	// and no synthetic hostname resolution. The sidecar listener and the
	// sandboxed extension are both on the host loopback, so 127.0.0.1 is the
	// correct address. host.containers.internal is a gvproxy container-VM
	// convention that does NOT resolve on bare macOS.
	if ctrCfg.HarnessPipeTCPPort != 0 {
		env = append(env, fmt.Sprintf("PRISM_HARNESS_PIPE=tcp://127.0.0.1:%d", ctrCfg.HarnessPipeTCPPort))
	}

	// For PI sessions, set PI_CODING_AGENT_DIR so PI discovers settings.json /
	// themes / AGENTS.md / skills from the shared host ~/.pi/agent directory.
	// The role system-prompt is injected at runtime by the prism PI extension,
	// not via this directory (design #2031). On Darwin (sandbox-exec) the
	// in-sandbox path is collapsed to the host path above (sandbox-exec shares
	// the host filesystem), so PI_CODING_AGENT_DIR ends up pointing at
	// ~/.pi/agent directly. The SBPL profile grants (subpath ~/.pi/agent) RW
	// for pi sessions which covers auth.json writes (OAuth token refresh) and
	// the proper-lockfile auth.json.lock mkdir.
	if ctrCfg.Harness == "pi" && ctrCfg.PIAgentConfigSandboxDir != "" {
		env = append(env, "PI_CODING_AGENT_DIR="+ctrCfg.PIAgentConfigSandboxDir)
	}

	// GIT_CONFIG_GLOBAL + GIT_SSH_COMMAND: point git and ssh at the generated
	// configs in the per-session work dir (issue #2213, Step 2 of #2132):
	//
	//   GIT_CONFIG_GLOBAL=<sessionDir>/gitconfig
	//   GIT_SSH_COMMAND="<sshBin> -F <sessionDir>/ssh-config"
	//
	// The work dir was created by PrepareSessionWorkDir() inside
	// PrepareSandboxExec() above. The embedded key paths are the stable sops
	// symlink paths (~/.ssh/<keyname>), so they survive secrets.d/<N>
	// rotation mid-session (#1410/#1573).
	//
	// Why -F with the Nix-built openssh binary (cfg.SshBin):
	//
	// 1. openssh resolves its default config via getpwuid() rather than
	//    $HOME, so without -F it always reads /Users/<user>/.ssh/config
	//    regardless of the HOME env var override. The generated ssh-config
	//    has the correct IdentityFile and StrictHostKeyChecking accept-new.
	//
	// 2. /usr/bin/ssh links against Apple's libnetwork.dylib, which reads
	//    /private/var/db/nsurlstoraged/dafsaData.bin (the DAFSA domain suffix
	//    database) during getaddrinfo(). Under deny-default the sandbox denies
	//    this read, causing getaddrinfo() to fail silently and SSH to call
	//    connect() with no resolved IP — returning "Undefined error: 0".
	//    The Nix-built openssh links against its own libresolv/libldns (Nix
	//    store paths under /nix, which are fully allowed), bypassing Apple's
	//    network stack entirely.
	//
	// Known gap, accepted in #2132 Step 2: libgit2/go-git-class tools ignore
	// GIT_CONFIG_GLOBAL; they fall back to $HOME-derived config, which does
	// not exist — benign for read-only operations (e.g. nix flake metadata),
	// which need no git identity.
	if sessionDir, workDirErr := m.SessionWorkDir(); workDirErr == nil && sessionDir != "" {
		env = append(env, container.SessionWorkDirGitEnv(sessionDir, ctrCfg.SshBin)...)
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

	// Layer 1 FD isolation (#2190): cap the sandbox child's RLIMIT_NOFILE so
	// a misbehaving agent cannot exhaust the host-wide FD pool (the #2180
	// failure class). The caps are applied to this process immediately before
	// Start() and restored immediately after — the child inherits them at
	// fork time, while the parent's own FD bookkeeping (stderr pipe, log
	// file, kqueue watcher) is unaffected. Warnings (host-hard clamping,
	// setrlimit failures) go to the agent-run log; failures never abort the
	// spawn. Mirrors the bwrap dispatch path in agent_run.go.
	restoreRlimit := container.ApplyAgentRlimitNofile(
		cfg.AgentMaxOpenFilesSoft, cfg.AgentMaxOpenFilesHard,
		func(format string, args ...any) { logAgentRunWarning(logFile, format, args...) })

	if err := sandboxCmd.Start(); err != nil {
		restoreRlimit()
		return fmt.Errorf("agent-run: start sandbox-exec: %w", err)
	}
	restoreRlimit()

	// Close the write end of the stderr pipe in the parent now that sandbox-exec
	// has inherited it. Required so reads from the read end return EOF when
	// sandbox-exec exits.
	if sandboxStderrW != nil {
		sandboxStderrW.Close()
	}

	// childExited is closed once sandboxCmd has been waited on. The
	// parent-death watcher consumes this channel to avoid sending spurious
	// signals after the child has already exited.
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

	// Supervise the child: foreground the pgid, forward SIGTERM/SIGINT/SIGHUP
	// (sandbox-exec deliberately does not subscribe SIGWINCH — see
	// SuperviseOpts.ForwardWinch godoc for the rationale), and wait for exit.
	// The shared SuperviseChild helper (supervise.go, A2.SUP) replaces the
	// previous open-coded tcsetpgrpForeground / per-mode signal forwarding /
	// cmd.Wait / tcsetpgrpRestore sequence — same behaviour, single
	// implementation across the bwrap and sandbox-exec dispatch paths.
	waitErr := SuperviseChild(sandboxCmd, int(os.Stdin.Fd()), SuperviseOpts{
		ForwardWinch: false,
		OnWinch:      nil,
	})

	// Signal the parent-death watcher that the child has exited.
	close(childExited)
	<-teeDone

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("agent-run: wait sandbox-exec: %w", waitErr)
	}
	return nil
}
