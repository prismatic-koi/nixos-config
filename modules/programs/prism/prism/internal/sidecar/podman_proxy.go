// Podman proxy lifecycle wiring.
//
// This file plumbs the standalone internal/podmanproxy package into the
// per-session sidecar, gated on the agent_status.containers_enabled column.
// Policy and request inspection live in the proxy package; this file does
// NOT make policy decisions -- it only:
//
//   1. Discovers the upstream podman socket per platform.
//   2. Opens the per-session audit log file.
//   3. Builds podmanproxy.Config from sidecar.Config + the discovered upstream.
//   4. Runs the proxy's Serve loop in a goroutine tracked by notifyWG.
//
// All the security guarantees come from the proxy package's default-deny
// model. This wiring file MUST NOT relax any of those checks; if a request
// is rejected unexpectedly, the correct response is to escalate via
// `prism escalate` and either admit the new field upstream in the
// podmanproxy package or revisit the integration design, NOT to weaken the
// proxy here.

package sidecar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/podmanproxy"
)

// defaultPodmanMachineName is the Darwin podman-machine name probed when
// Config.PodmanMachineName is empty. This matches the default machine name
// used by `podman machine init` when no --name flag is passed, and is the
// only machine name we attempt to discover automatically. A per-spawn
// override arrives through the same Config.PodmanMachineName field.
const defaultPodmanMachineName = "podman-machine-default"

// runPodmanProxyIfEnabled starts the per-session filtering podman API socket
// proxy when the session's agent_status.containers_enabled gate is set. It
// is called from (*Sidecar).Run after instance_id and the FK-guard row have
// been settled -- the instance_id is needed to derive the audit-log path,
// and the row must exist before any audit writes occur.
//
// When containers_enabled is false (the default), this function returns
// without creating any listener, file, or goroutine.
//
// When containers_enabled is true:
//
//   - Upstream discovery is attempted per platform. Failure is NOT fatal:
//     the proxy is still started with the resolved (possibly non-existent)
//     upstream path, and the proxy package's friendly 503 envelope handles
//     every request until the upstream becomes reachable.
//   - The audit log is opened in append-only mode under
//     <XDG_STATE_HOME>/prism/sessions/<instanceID>/podman-proxy.log so log
//     entries survive sidecar restarts within a single session incarnation.
//   - The proxy's Serve goroutine is launched via goNotify so notifyWG tracks
//     it; ctx cancellation -- triggered by Shutdown() -- drains the accept
//     loop within the proxy's own shutdown budget.
//   - The audit file handle is stored on the Sidecar so Shutdown can close
//     it after the proxy goroutine exits.
//
// Errors during file/dir setup are logged but NOT propagated: a configured
// proxy that cannot start should not prevent the rest of the sidecar from
// coming up, because the agent's non-container surface (host-API, harness
// pipe, SSE) is independent of container access.
func (s *Sidecar) runPodmanProxyIfEnabled(ctx context.Context) {
	// Resolve the listener path. Empty means the caller did not wire a path
	// (spawns and tests that do not need the proxy). Skip silently.
	listenerPath := s.cfg.PodmanProxyListenerPath
	if listenerPath == "" {
		return
	}

	// Containers-enabled gate. The DB is the source of truth: spawn flags
	// in spawn_inputs are an audit row only; agent_status.containers_enabled
	// is the live runtime gate. A nil DB (some tests, ad-hoc bring-up) keeps
	// the proxy off -- there is no row to read.
	if s.cfg.DB == nil {
		return
	}
	status, err := s.cfg.DB.CurrentStatus(s.cfg.SessionName)
	if err != nil {
		s.logger().Printf("sidecar: podman-proxy: CurrentStatus: %v (proxy not started)", err)
		return
	}
	if status == nil || !status.ContainersEnabled {
		// Default-off path: no listener, no audit file, no goroutine.
		return
	}

	// At this point containers_enabled=1. Build the proxy config.
	upstream := s.resolvePodmanUpstreamPath()

	// Open the audit log. The directory is the per-session work dir, which
	// production code prepares via PrepareSessionWorkDir. We create it here
	// defensively so tests -- and the rare case where the proxy starts
	// before agent-run has prepared the directory -- do not lose audit
	// lines.
	auditFile, auditPath, auditErr := s.openPodmanProxyAuditFile()
	if auditErr != nil {
		// Audit-file open failure must not prevent the proxy from
		// starting. Log and run with a nil writer -- the proxy package
		// silently drops audit events when AuditWriter is nil.
		s.logger().Printf("sidecar: podman-proxy: audit log open: %v (proxy will run without audit)", auditErr)
		auditFile = nil
		auditPath = ""
	}

	allowed := s.allowedPodmanBindSources()
	cfg := podmanproxy.Config{
		ListenerPath:       listenerPath,
		UpstreamPath:       upstream,
		AllowedBindSources: allowed,
		// Per-session container name prefix. The proxy auto-injects this
		// prefix into containers/create requests with no Name field, and
		// rejects any explicit Name that does not start with it. Cleanup
		// (`cmd/cleanup.go`) sweeps any orphan container matching the
		// same prefix at session teardown.
		ContainerNamePrefix: "prism-" + s.cfg.SessionName + "-",
		// The default-deny policy applies: no AllowedCaps, no
		// AllowedSecurityOpts, no MaxMemoryBytes. Every escape vector is
		// rejected by default.
	}
	if auditFile != nil {
		cfg.AuditWriter = auditFile
	}

	proxy, err := podmanproxy.NewProxy(cfg)
	if err != nil {
		s.logger().Printf("sidecar: podman-proxy: NewProxy: %v (proxy not started)", err)
		if auditFile != nil {
			_ = auditFile.Close()
		}
		return
	}

	// Track the audit file on the Sidecar so Shutdown can close it once the
	// proxy goroutine has exited. We deliberately do NOT log the upstream
	// socket path verbatim -- it must not appear in the sidecar log. The
	// discovery-classification string ("env-derived",
	// "fallback", "machine-inspect", "override", "missing") is the
	// observability we expose instead.
	s.mu.Lock()
	s.podmanProxyAuditFile = auditFile
	s.podmanProxyAuditPath = auditPath
	s.mu.Unlock()
	s.logger().Printf("sidecar: podman-proxy: listening on %s (audit=%s, allowed_bind_sources=%d)",
		listenerPath, auditPath, len(allowed))

	// Run the proxy's Serve loop in a tracked goroutine. goNotify increments
	// notifyWG so test code can drain it, and Run() returns when ctx is
	// cancelled (i.e. when Shutdown is invoked). On ctx cancellation Serve
	// drains in-flight requests up to its own shutdownTimeout (2s) and
	// removes the listener socket file before returning.
	//
	// The audit file is closed inside this goroutine, AFTER Serve returns,
	// so the last audit line is guaranteed flushed before fclose. Shutdown
	// also calls closePodmanProxyAuditFile defensively in case Serve never
	// started (e.g. failure path before this branch was reached); both
	// callers are guarded by an s.mu nil-check so a double close is safe.
	s.goNotify(func() {
		if err := proxy.Serve(ctx); err != nil {
			s.logger().Printf("sidecar: podman-proxy: Serve: %v", err)
		}
		s.closePodmanProxyAuditFile()
	})
}

// closePodmanProxyAuditFile closes the audit log file handle if one was
// opened. Called from Shutdown after the proxy goroutine has exited so the
// last audit line has already been written.
func (s *Sidecar) closePodmanProxyAuditFile() {
	s.mu.Lock()
	f := s.podmanProxyAuditFile
	s.podmanProxyAuditFile = nil
	s.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// resolvePodmanUpstreamPath returns the absolute path of the upstream
// docker/podman Unix socket the proxy should forward to. The path does not
// have to exist -- if it doesn't, the proxy returns the friendly 503
// envelope for every request.
//
// Precedence:
//  1. Config.PodmanUpstreamPath override (test seam).
//  2. Platform-specific discovery (NixOS env, Darwin `podman machine inspect`).
//  3. A non-existent placeholder path as a final fallback -- the proxy
//     handles ENOENT/ECONNREFUSED at dial time and returns the friendly 503.
func (s *Sidecar) resolvePodmanUpstreamPath() string {
	if s.cfg.PodmanUpstreamPath != "" {
		return s.cfg.PodmanUpstreamPath
	}
	switch runtime.GOOS {
	case "linux":
		return s.resolvePodmanUpstreamLinux()
	case "darwin":
		return s.resolvePodmanUpstreamDarwin()
	default:
		// Unsupported platforms get a non-existent placeholder; the proxy
		// handles dial failure as "podman socket unavailable".
		return "/podman-unavailable.sock"
	}
}

// resolvePodmanUpstreamLinux applies the NixOS / Linux discovery rules:
//
//   - $XDG_RUNTIME_DIR/podman/podman.sock when XDG_RUNTIME_DIR is set.
//   - /run/user/<uid>/podman/podman.sock as the fallback when XDG_RUNTIME_DIR
//     is unset.
//
// Discovery never inspects the path's existence -- the proxy itself
// handles ENOENT at dial time.
func (s *Sidecar) resolvePodmanUpstreamLinux() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "podman", "podman.sock")
	}
	return fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
}

// resolvePodmanUpstreamDarwin shells out to `podman machine inspect <name>
// --format '{{.ConnectionInfo.PodmanSocket.Path}}'` and returns the trimmed
// stdout. When the command is missing, exits non-zero, or returns empty
// output, a non-existent placeholder is returned so the proxy hits its
// friendly 503 path. Discovery is best-effort by design: a stopped machine
// is a normal failure mode.
func (s *Sidecar) resolvePodmanUpstreamDarwin() string {
	machine := s.cfg.PodmanMachineName
	if machine == "" {
		machine = defaultPodmanMachineName
	}
	cmd := exec.Command("podman", "machine", "inspect", machine,
		"--format", "{{.ConnectionInfo.PodmanSocket.Path}}")
	out, err := cmd.Output()
	if err != nil {
		// Use a stable classification token instead of the raw err so we
		// do not leak the upstream socket path if it appears in stderr.
		s.logger().Printf("sidecar: podman-proxy: machine-inspect failed (%s); proxy will return 503 envelope", classifyDiscoveryError(err))
		return "/podman-machine-unavailable.sock"
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		s.logger().Printf("sidecar: podman-proxy: machine-inspect returned empty path; proxy will return 503 envelope")
		return "/podman-machine-unavailable.sock"
	}
	// IMPORTANT: do NOT log `path` here. The upstream socket path must not
	// appear in the sidecar log.
	return path
}

// classifyDiscoveryError produces a short, redaction-safe token describing
// why upstream discovery failed. Never include err.Error() verbatim in the
// sidecar log -- the upstream socket path may appear in stderr output and
// must not reach the sidecar log.
func classifyDiscoveryError(err error) string {
	if err == nil {
		return "ok"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("exit-status-%d", exitErr.ExitCode())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "binary-not-found"
	}
	return "io-error"
}

// allowedPodmanBindSources returns the set of host path prefixes that may
// appear as the source side of HostConfig.Binds / Mounts entries on a
// containers/create request.
//
// The set always includes:
//   - The session's worktree (cfg.Worktree).
//   - The bare repo root (cfg.BareRoot) when non-empty.
//   - <sessionDir>/container-scratch/ when the instance ID is known.
//
// Empty fields are skipped. The scratch directory may not exist yet at this
// point -- that's fine, the proxy treats the AllowedBindSources entries as
// path prefixes, not file references.
func (s *Sidecar) allowedPodmanBindSources() []string {
	var allowed []string
	if s.cfg.Worktree != "" {
		allowed = append(allowed, s.cfg.Worktree)
	}
	if s.cfg.BareRoot != "" {
		allowed = append(allowed, s.cfg.BareRoot)
	}
	if s.cfg.InstanceID != "" {
		if sessionDir, err := container.SessionWorkDirPath(s.cfg.InstanceID); err == nil {
			allowed = append(allowed, filepath.Join(sessionDir, "container-scratch"))
		}
	}
	return allowed
}

// openPodmanProxyAuditFile opens the per-session audit log file in
// append-only mode and returns the file handle plus the absolute path. The
// audit log lives under the per-session work dir so RemoveSessionWorkDir
// wipes it on cleanup, alongside the rest of the session's transient state.
//
// The directory is created with 0o700 to match the rest of the per-session
// state; the file is opened 0o600 so only the sidecar owner can read it.
func (s *Sidecar) openPodmanProxyAuditFile() (*os.File, string, error) {
	if s.cfg.InstanceID == "" {
		return nil, "", fmt.Errorf("instance ID is empty")
	}
	sessionDir, err := container.SessionWorkDirPath(s.cfg.InstanceID)
	if err != nil {
		return nil, "", fmt.Errorf("session work dir: %w", err)
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("mkdir session dir: %w", err)
	}
	auditPath := filepath.Join(sessionDir, "podman-proxy.log")
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("open audit log: %w", err)
	}
	return f, auditPath, nil
}
