package container

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnsureRemoved stops and removes any existing container with the same name,
// and cleans up any temp files created by Create. It is safe to call when no
// such container exists — errors for "no such container" are silently ignored.
//
// When the Manager was created with a non-empty Config.InstanceID, EnsureRemoved
// first inspects the container's "prism.instance-id" label. If the label exists
// and does not match the current InstanceID, a warning is logged indicating
// that the container belongs to a different session instance. Removal still
// proceeds regardless — the new session needs the container name.
func (m *Manager) EnsureRemoved(ctx context.Context) {
	// Clean up temp files created by Create.
	_ = os.Remove(m.gitdirFilePath())
	_ = os.Remove(m.worktreeGitdirFilePath())
	_ = os.Remove(m.sshConfigFilePath())
	_ = os.Remove(m.gitconfigFilePath())
	_ = os.Remove(m.allowedSignersFilePath())
	_ = os.Remove(m.opencodeConfigFilePath())
	_ = os.Remove(m.claudeCredentialsFilePath())
	_ = os.Remove(m.sandboxExecProfilePath())
	// Remove the per-session sandbox-exec staging HOME directory tree.
	if stagingHome, err := m.sandboxExecHomePath(); err == nil {
		_ = os.RemoveAll(stagingHome)
	}

	// Check the container's instance label when we have our own InstanceID.
	// This detects ownership mismatches where a container from a previous
	// session incarnation is being cleaned up by a new one.
	if m.cfg.InstanceID != "" {
		inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
		out, inspectErr := exec.CommandContext(inspectCtx, "podman", "inspect",
			"--format", `{{index .Config.Labels "prism.instance-id"}}`,
			m.name,
		).Output()
		inspectCancel()
		if inspectErr == nil {
			containerInstanceID := strings.TrimSpace(string(out))
			if containerInstanceID != "" && containerInstanceID != m.cfg.InstanceID {
				log.Printf("container: warning: container %q has instance-id %q but current session has %q — removing anyway",
					m.name, containerInstanceID, m.cfg.InstanceID)
			}
		}
	}

	// Stop the container (ignore errors — may not be running).
	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", m.name)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		// Only log if it looks like a real error (not "no such container").
		if !IsNoSuchContainerError(string(out)) {
			log.Printf("container: stop existing %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
		}
	}

	// Remove the container (ignore errors — may not exist).
	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", m.name)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		if !IsNoSuchContainerError(string(out)) {
			log.Printf("container: rm existing %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
		}
	}
}

// Create creates and starts the podman container for this session.
// It first calls EnsureRemoved to handle any stale container with the same name.
// Returns an error if podman create or start fails.
func (m *Manager) Create(ctx context.Context) error {
	createStart := time.Now()

	// Remove any stale container first (AC-15).
	m.EnsureRemoved(ctx)
	log.Printf("[timing] EnsureRemoved: %s", time.Since(createStart).Round(time.Millisecond))

	// Write corrected git pointer files for the container.
	if m.cfg.BareRoot != "" && m.cfg.WorktreeGitDir != "" {
		branch := filepath.Base(m.cfg.WorktreeGitDir)

		// Forward pointer: /workspace/.git → /prism-git/worktrees/<branch> (#492).
		// The host's .git file contains an absolute host path that doesn't exist
		// inside the container.
		gitdirContent := "gitdir: /prism-git/worktrees/" + branch + "\n"
		if err := os.WriteFile(m.gitdirFilePath(), []byte(gitdirContent), 0o644); err != nil {
			return fmt.Errorf("container: write gitdir file: %w", err)
		}

		// Back-pointer: worktrees/<branch>/gitdir → /workspace/.git
		// This file is the reverse pointer from git metadata back to the
		// worktree checkout. On the host it contains the host worktree path
		// (e.g. /home/user/code/repo/branch/.git) which doesn't exist in the
		// container. nix/libgit2 follows this pointer when resolving the
		// working tree, so it must point to the container-internal path.
		wtGitdirContent := "/workspace/.git\n"
		if err := os.WriteFile(m.worktreeGitdirFilePath(), []byte(wtGitdirContent), 0o644); err != nil {
			return fmt.Errorf("container: write worktree gitdir file: %w", err)
		}
	}

	// Write a minimal SSH config for the container. The host's ~/.ssh/config
	// is a nix store symlink with wrong ownership that SSH rejects.
	if err := m.writeSshConfig(isolationPodman); err != nil {
		return err
	}

	// Write a minimal .gitconfig for the container. The host's git config lives
	// in ~/.config/git/config (managed by home-manager) and is not mounted into
	// containers. We generate a minimal config with identity, signing, and
	// convenience settings.
	t0 := time.Now()
	if err := m.writeGitconfig(isolationPodman); err != nil {
		return fmt.Errorf("container: write gitconfig: %w", err)
	}
	log.Printf("[timing] writeGitconfig: %s", time.Since(t0).Round(time.Millisecond))

	// Write the opencode config file for the container. This replaces the
	// previous OPENCODE_CONFIG_CONTENT env var approach so that relative plugin
	// paths (e.g. "./plugins/my-plugin") resolve correctly from the config
	// file's location at /root/.config/opencode/opencode.json.
	if m.cfg.ConfigContent != "" {
		if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o644); err != nil {
			return fmt.Errorf("container: write opencode config: %w", err)
		}
	}

	// On Darwin, extract Claude Code credentials from the macOS Keychain and
	// write them to a temp file so the container can find them. On Linux,
	// ~/.claude/.credentials.json is already inside the claudeMount directory.
	m.writeClaudeCredentials()

	// Pre-create directories that buildRunArgs() will reference as volume mount
	// sources. podman validates all bind-mount source paths before starting the
	// container and exits 125 if any are absent, so we create them eagerly here.
	// Failures are logged and non-fatal — podman will surface the real error if
	// the directory is still absent after this attempt.
	if err := m.prepareVolumeDirs(true); err != nil {
		// prepareVolumeDirs logs individual failures; a non-nil error means
		// multiple dirs failed and is logged here for observability only.
		log.Printf("container: prepareVolumeDirs partial failure: %v", err)
	}

	// Build the podman run arguments.
	t0 = time.Now()
	args := m.buildRunArgs()
	log.Printf("[timing] buildRunArgs: %s", time.Since(t0).Round(time.Millisecond))

	log.Printf("container: creating %q: podman %s", m.name, strings.Join(redactArgs(args), " "))

	podmanStart := time.Now()
	if err := m.isolator.Run(ctx, args); err != nil {
		return err
	}
	log.Printf("[timing] podman run: %s (total to here: %s)",
		time.Since(podmanStart).Round(time.Millisecond),
		time.Since(createStart).Round(time.Millisecond))
	log.Printf("[timing] Create total: %s", time.Since(createStart).Round(time.Millisecond))

	return nil
}

// WaitHealthy polls the container's HTTP endpoint until it responds with a
// successful status code (2xx) or the timeout expires. Returns nil when the
// container is healthy.
func (m *Manager) WaitHealthy(ctx context.Context) error {
	timeout := m.cfg.HealthCheckTimeout
	if timeout == 0 {
		timeout = DefaultHealthCheckTimeout
	}

	waitStart := time.Now()
	probeCount := 0
	deadline := waitStart.Add(timeout)
	for {
		if time.Now().After(deadline) {
			m.dumpLogs()
			return fmt.Errorf("container: health check timed out after %s for %q", timeout, m.name)
		}
		if ctx.Err() != nil {
			m.dumpLogs()
			return ctx.Err()
		}

		probeCount++
		healthy := m.isHealthy(ctx)
		elapsed := time.Since(waitStart).Round(time.Millisecond)
		if healthy {
			log.Printf("[timing] WaitHealthy probe %d: %s elapsed — ok", probeCount, elapsed)
			log.Printf("[timing] WaitHealthy: %s (%d probes)", time.Since(waitStart).Round(time.Millisecond), probeCount)
			return nil
		}
		log.Printf("[timing] WaitHealthy probe %d: %s elapsed — fail", probeCount, elapsed)

		// Check if the container exited early — no point polling a dead
		// container until the timeout expires.
		if exited, code := m.hasExited(); exited {
			m.dumpLogs()
			return fmt.Errorf("container: %q exited with code %d before becoming healthy", m.name, code)
		}

		select {
		case <-ctx.Done():
			m.dumpLogs()
			return ctx.Err()
		case <-time.After(healthCheckInterval):
		}
	}
}

// dumpLogs writes the container's stdout/stderr to the sidecar log so that
// startup failures are visible without needing to race `podman logs` before
// the container is removed by Shutdown.
func (m *Manager) dumpLogs() {
	m.isolator.DumpLogs()
}

// hasExited checks whether the container has already stopped. Returns true and
// the exit code if the container is in an exited state.
func (m *Manager) hasExited() (bool, int) {
	return m.isolator.HasExited()
}

// isHealthy performs a single HTTP probe and returns true on success.
func (m *Manager) isHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.healthCheckURL, nil)
	if err != nil {
		return false
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Shutdown stops and removes the container. Intended to be called on SIGTERM.
// It delegates the podman stop/rm calls to the Isolator and then cleans up
// the temp files created by Create.
func (m *Manager) Shutdown() {
	log.Printf("container: shutting down %q", m.name)

	m.isolator.Shutdown()

	// Clean up temp files created by Create.
	_ = os.Remove(m.gitdirFilePath())
	_ = os.Remove(m.worktreeGitdirFilePath())
	_ = os.Remove(m.sshConfigFilePath())
	_ = os.Remove(m.gitconfigFilePath())
	_ = os.Remove(m.allowedSignersFilePath())
	_ = os.Remove(m.opencodeConfigFilePath())
	_ = os.Remove(m.claudeCredentialsFilePath())
	_ = os.Remove(m.sandboxExecProfilePath())
	// Remove the per-session sandbox-exec staging HOME directory tree.
	if stagingHome, err := m.sandboxExecHomePath(); err == nil {
		_ = os.RemoveAll(stagingHome)
	}

	log.Printf("container: %q removed", m.name)
}
