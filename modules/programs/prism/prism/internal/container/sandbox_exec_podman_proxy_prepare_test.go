package container

// sandbox_exec_podman_proxy_prepare_test.go — unit tests for the
// containers-enabled prep block in sandboxExecIsolator.Prepare. This block
// is symmetric to the bwrap-side containers-enabled prep block and exercises
// the same validation order + scratch-dir mkdir against the sandbox-exec
// isolator.
//
// What's covered here:
//
//   - sandboxExecIsolator.Prepare creates <sessionDir>/container-scratch
//     on disk when ContainersEnabled=true; the SBPL profile generator's
//     literal RW allow on PodmanProxySockPath is exercised via the sibling
//     sandbox_exec_podman_proxy_test.go unit tests, and end-to-end via
//     internal/integration/sandbox_exec_podman_proxy_darwin_test.go.
//
//   - The scratch dir is NOT created when ContainersEnabled=false — the
//     default-off path is a clean no-op.
//
//   - Prepare hard-fails when PodmanProxySockPath is empty on a
//     ContainersEnabled=true session.
//
//   - Prepare hard-fails when the per-session run dir (parent of
//     PodmanProxySockPath) does not exist on disk, or exists but is not a
//     directory.
//
//   - The hard-fail gate is engaged ONLY when ContainersEnabled=true. A
//     misconfigured PodmanProxySockPath on a containers_enabled=false
//     session must NOT fail Prepare, because no allow is emitted in that
//     case.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxExecPrepare_CreatesContainerScratchWhenEnabled verifies that
// <sessionDir>/container-scratch/ is created on disk when
// ContainersEnabled=true. The proxy's allowedPodmanBindSources includes
// this path; the create-time bind validation expects it to exist.
//
// The mkdir lives in sandboxExecIsolator.Prepare (not PrepareSessionWorkDir
// itself) so it is symmetric with the bwrap-side block and so non-containers
// sessions sharing PrepareSessionWorkDir see zero behaviour change.
func TestSandboxExecPrepare_CreatesContainerScratchWhenEnabled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	runDir := t.TempDir()
	proxyPath := filepath.Join(runDir, "podman.sock")

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:         "repo@main",
		InstanceID:          "test-containers-enabled",
		Worktree:            t.TempDir(),
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	if _, err := iso.Prepare(context.Background(), m); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	scratchDir := SessionWorkDirContainerScratchPath(sessionDir)
	info, statErr := os.Stat(scratchDir)
	if statErr != nil {
		t.Fatalf("container-scratch dir not created at %s: %v", scratchDir, statErr)
	}
	if !info.IsDir() {
		t.Errorf("container-scratch at %s is not a directory (mode=%v)", scratchDir, info.Mode())
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("container-scratch at %s has perm %#o, want 0700", scratchDir, info.Mode().Perm())
	}
}

// TestSandboxExecPrepare_NoContainerScratchWhenDisabled verifies that the
// scratch dir is NOT created when ContainersEnabled=false. The default-off
// path must be a clean no-op — sessions that did not opt in to containers
// see no new state on disk.
func TestSandboxExecPrepare_NoContainerScratchWhenDisabled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@main",
		InstanceID:  "test-containers-disabled",
		Worktree:    t.TempDir(),
		// ContainersEnabled deliberately omitted — default false.
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	if _, err := iso.Prepare(context.Background(), m); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	scratchDir := SessionWorkDirContainerScratchPath(sessionDir)
	if _, statErr := os.Stat(scratchDir); statErr == nil {
		t.Errorf("container-scratch dir was created at %s despite ContainersEnabled=false", scratchDir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) returned unexpected error: %v (want os.ErrNotExist)", scratchDir, statErr)
	}
}

// TestSandboxExecPrepare_ContainerScratchIdempotent verifies that calling
// Prepare twice with ContainersEnabled=true is idempotent — the second
// call must succeed even though the scratch dir already exists, and any
// content placed inside between calls must survive (the mkdir is
// MkdirAll, not a wipe-and-recreate).
func TestSandboxExecPrepare_ContainerScratchIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	runDir := t.TempDir()
	proxyPath := filepath.Join(runDir, "podman.sock")

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:         "repo@main",
		InstanceID:          "test-containers-idempotent",
		Worktree:            t.TempDir(),
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	if _, err := iso.Prepare(context.Background(), m); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	scratchDir := SessionWorkDirContainerScratchPath(sessionDir)
	sentinel := filepath.Join(scratchDir, "preserve-across-prepare")
	if err := os.WriteFile(sentinel, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write sentinel into scratch dir: %v", err)
	}

	if _, err := iso.Prepare(context.Background(), m); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("sentinel %s removed by second Prepare: %v", sentinel, statErr)
	}
}

// TestSandboxExecPrepare_ErrorsWhenSockPathEmpty verifies that
// ContainersEnabled=true with an empty PodmanProxySockPath is rejected at
// Prepare time. This is the defence-in-depth call-site contract: the
// per-mode dispatcher must populate both fields or neither. Without this
// check we'd silently generate an SBPL with no proxy allow and the agent
// would see EPERM at connect(2) time with no operator signal as to why.
func TestSandboxExecPrepare_ErrorsWhenSockPathEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:       "repo@main",
		InstanceID:        "test-sockpath-empty",
		Worktree:          t.TempDir(),
		ContainersEnabled: true,
		// PodmanProxySockPath deliberately empty.
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	_, err := iso.Prepare(context.Background(), m)
	if err == nil {
		t.Fatalf("Prepare succeeded with empty PodmanProxySockPath; want hard-fail")
	}
	if !strings.Contains(err.Error(), "PodmanProxySockPath is empty") {
		t.Errorf("error message %q does not mention the empty PodmanProxySockPath", err.Error())
	}
	if _, statErr := os.Stat(m.sandboxExecProfilePath()); statErr == nil {
		t.Errorf("Prepare wrote a profile despite the empty-SockPath hard-fail")
	}
}

// TestSandboxExecPrepare_ErrorsWhenRunDirMissing verifies the edge case:
// when ContainersEnabled=true but the per-session run dir
// (parent of PodmanProxySockPath) does not exist on disk, Prepare returns
// an error rather than rendering an SBPL with an allow for a literal that
// doesn't exist. The integration tests live in
// internal/integration/sandbox_exec_podman_proxy_darwin_test.go.
func TestSandboxExecPrepare_ErrorsWhenRunDirMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Point PodmanProxySockPath at a directory we never create. Note that
	// the SESSION work dir (under XDG_STATE_HOME) is created by Prepare
	// itself via PrepareSessionWorkDir; only the run dir (PodmanProxySockPath's
	// parent) is expected to pre-exist, populated by the sidecar.
	missingRunDir := filepath.Join(t.TempDir(), "never-mkdir-this")
	proxyPath := filepath.Join(missingRunDir, "podman.sock")

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:         "repo@main",
		InstanceID:          "test-rundir-missing",
		Worktree:            t.TempDir(),
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	_, err := iso.Prepare(context.Background(), m)
	if err == nil {
		t.Fatalf("Prepare succeeded despite missing run dir %s; want hard-fail error", missingRunDir)
	}
	if !strings.Contains(err.Error(), "containers_enabled=true") {
		t.Errorf("error message %q does not mention containers_enabled=true (operator must be able to grep for the cause)", err.Error())
	}
	if !strings.Contains(err.Error(), missingRunDir) {
		t.Errorf("error message %q does not include the missing run dir path %q", err.Error(), missingRunDir)
	}

	// No SBPL profile must have been materialised — a missing-run-dir
	// session must not leave any rendered allow on disk.
	if _, statErr := os.Stat(m.sandboxExecProfilePath()); statErr == nil {
		t.Errorf("Prepare wrote a profile despite the hard-fail at %s", m.sandboxExecProfilePath())
	}
}

// TestSandboxExecPrepare_ErrorsWhenRunDirIsRegularFile verifies the
// is-directory branch of the edge-case check: a regular file at the
// runDir path is a configuration error (not just an absent dir), and
// must also fail loudly.
func TestSandboxExecPrepare_ErrorsWhenRunDirIsRegularFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Plant a regular file at the runDir path so Stat succeeds but
	// IsDir() returns false.
	parent := t.TempDir()
	regularFileAsRunDir := filepath.Join(parent, "this-is-a-file-not-a-dir")
	if err := os.WriteFile(regularFileAsRunDir, []byte("oops"), 0o600); err != nil {
		t.Fatalf("plant regular file at run dir path: %v", err)
	}
	proxyPath := filepath.Join(regularFileAsRunDir, "podman.sock")

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:         "repo@main",
		InstanceID:          "test-rundir-regular-file",
		Worktree:            t.TempDir(),
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	_, err := iso.Prepare(context.Background(), m)
	if err == nil {
		t.Fatalf("Prepare succeeded with a regular file at runDir; want hard-fail")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error message %q does not mention \"not a directory\"", err.Error())
	}
}

// TestSandboxExecPrepare_NoErrorWhenContainersDisabled verifies the gate:
// a missing PodmanProxySockPath dir must NOT fail Prepare on a
// containers_enabled=false session, because the SBPL emits no allow for
// the proxy in that case anyway. This protects existing sessions from
// being broken by the new edge-case check.
func TestSandboxExecPrepare_NoErrorWhenContainersDisabled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	missingRunDir := filepath.Join(t.TempDir(), "never-mkdir-this-disabled")
	proxyPath := filepath.Join(missingRunDir, "podman.sock")

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@main",
		InstanceID:  "test-rundir-disabled",
		Worktree:    t.TempDir(),
		// ContainersEnabled deliberately omitted — default false.
		PodmanProxySockPath: proxyPath, // populated but ignored
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	args, err := iso.Prepare(context.Background(), m)
	if err != nil {
		t.Fatalf("Prepare returned error on containers_enabled=false session: %v", err)
	}
	if len(args) == 0 {
		t.Errorf("Prepare returned empty args slice")
	}
}

// TestSandboxExecPrepare_NoErrorWhenRunDirExists verifies the happy path:
// when ContainersEnabled=true and the run dir is on disk (the production
// posture, where the sidecar created it before agent-run was dispatched),
// Prepare succeeds and writes the SBPL containing the literal allow.
func TestSandboxExecPrepare_NoErrorWhenRunDirExists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	runDir := t.TempDir()
	proxyPath := filepath.Join(runDir, "podman.sock")

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:         "repo@main",
		InstanceID:          "test-rundir-exists",
		Worktree:            t.TempDir(),
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	iso := newSandboxExecIsolator(m.name)
	args, err := iso.Prepare(context.Background(), m)
	if err != nil {
		t.Fatalf("Prepare returned error on existing run dir: %v", err)
	}
	if len(args) < 3 || args[1] != "-f" {
		t.Fatalf("unexpected args shape: %v", redactedArgs(args))
	}

	// Verify the rendered profile contains the literal allow for the
	// proxy path — i.e. Prepare didn't just succeed, it actually emitted
	// the grant the integration tests exercise.
	content, readErr := os.ReadFile(args[2])
	if readErr != nil {
		t.Fatalf("read rendered profile: %v", readErr)
	}
	want := "(literal " + quoteSBPL(proxyPath) + "))"
	if !strings.Contains(string(content), want) {
		t.Errorf("rendered profile missing literal allow %q; full profile:\n%s", want, string(content))
	}

	// Confirm the scratch dir was also mkdir'd as part of the same Prepare
	// call (so the integration test's stub-server setup doesn't need to
	// pre-create it). This is the symmetric containers-enabled side
	// effect to the bwrap-side mkdir in Step 4.
	sessionDir, dirErr := m.sessionWorkDirPath()
	if dirErr != nil {
		t.Fatalf("sessionWorkDirPath: %v", dirErr)
	}
	scratchDir := SessionWorkDirContainerScratchPath(sessionDir)
	if info, statErr := os.Stat(scratchDir); statErr != nil {
		t.Errorf("container-scratch dir not created at %s after happy-path Prepare: %v", scratchDir, statErr)
	} else if !info.IsDir() {
		t.Errorf("container-scratch at %s is not a directory", scratchDir)
	}
}
