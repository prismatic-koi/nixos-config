package container

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
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
//
// Post A1.L1 (issue #1140): the per-mode logic moved into the registered
// Isolator's EnsureRemoved method. Manager.EnsureRemoved is a thin
// dispatcher that also unlinks the per-session temp files unconditionally
// (the legacy "defensive cleanup" behaviour, mode-agnostic) so that a stale
// session's mode-mismatched temp files do not survive the cleanup pass.
func (m *Manager) EnsureRemoved(ctx context.Context) {
	// Mode-agnostic temp-file cleanup: always remove every per-session
	// artefact regardless of which isolator the Manager was constructed
	// for. This preserves the pre-refactor "defensive cleanup" behaviour
	// where Manager.EnsureRemoved unlinked all temp files, including those
	// belonging to other modes (so a session that switched modes between
	// runs leaves no orphan files on disk).
	_ = os.Remove(m.gitdirFilePath())
	_ = os.Remove(m.worktreeGitdirFilePath())
	_ = os.Remove(m.sshConfigFilePath())
	_ = os.Remove(m.gitconfigFilePath())
	_ = os.Remove(m.allowedSignersFilePath())
	_ = os.Remove(m.opencodeConfigFilePath())
	_ = os.Remove(m.claudeCredentialsFilePath())
	_ = os.Remove(m.sandboxExecProfilePath())
	if stagingHome, err := m.sandboxExecHomePath(); err == nil {
		_ = os.RemoveAll(stagingHome)
	}

	// Per-mode lifecycle cleanup (podman stop/rm for the podman isolator;
	// no-op for the others) lives on the registered Isolator. See
	// lifecycle_dispatch.go (issue #1140 A1.L1).
	m.isolator.EnsureRemoved(ctx, m)
}

// Create creates and starts the podman container for this session.
// It first calls EnsureRemoved to handle any stale container with the same name.
// Returns an error if podman create or start fails.
//
// Post A1.L5 (issue #1140): the per-mode session-start logic moved into the
// registered Isolator's Create method. Manager.Create is a thin dispatcher.
func (m *Manager) Create(ctx context.Context) error {
	return m.isolator.Create(ctx, m)
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
