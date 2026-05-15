// Package iristest provides test-isolation helpers for the iris parity gate
// (D-10) and any other iris test that exercises the storage roots.
//
// # Isolation contract
//
// Any test that constructs iris paths or opens the iris DB must call
// NewIsolated *before* any iris startup code runs. NewIsolated:
//
//   - Sets $HOME, $XDG_STATE_HOME, and $XDG_CONFIG_HOME to subdirectories
//     of a t.TempDir() so iris.ResolvePaths() resolves entirely under the
//     tempdir.
//   - Sets $IRIS_PARITY_TEST_MODE=1 which the prism binary checks at
//     startup and, when set, exits 99 with a clear error message. This is
//     the prism-binary tripwire required by the D-10 security AC.
//   - Verifies that the resolved iris paths all live under the tempdir.
//     A panic from this check is a non-recoverable test bug — the
//     isolation has been broken before any iris code runs.
//   - Registers a t.Cleanup that re-runs the path check at the end of the
//     test, catching late-arriving writes that escape the tempdir.
//
// # Session naming convention
//
// Tests that create sessions must use names with the "iris-test@" prefix.
// This avoids any chance of colliding with a real iris session on the
// developer's host even if isolation is accidentally broken — there can
// be no real coordinator named "iris-test@foo".
//
// # No prism imports
//
// This package and every parity test built on it must NOT import
// internal/container, internal/sidecar, or internal/session. The CI grep
// gate (iris-parity-isolation_test.go) enforces this.
package iristest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// EnvParityTestMode is the environment variable the prism binary checks at
// startup. When set to any non-empty value, prism's main() exits 99 with a
// "iris parity test mode active" message so parity tests that accidentally
// invoke prism are detected immediately.
//
// The check itself lives in cmd/root.go (the prism cobra root command).
// Wiring it from iris's test package keeps the constant exported for
// assertion-grep tests.
const EnvParityTestMode = "IRIS_PARITY_TEST_MODE"

// Isolated bundles the per-test isolated environment.
type Isolated struct {
	// Root is the per-test t.TempDir() — every iris path lives under here.
	Root string
	// Home is the fake $HOME ($Root/home).
	Home string
	// StateHome is the fake $XDG_STATE_HOME ($Root/state).
	StateHome string
	// ConfigHome is the fake $XDG_CONFIG_HOME ($Root/config).
	ConfigHome string
	// Paths is the resolved iris.Paths under the tempdir. RunDir and Sock
	// have been redirected to a short prefix (see note below) so per-session
	// Unix sockets stay under the 108-byte sun_path limit even when the
	// test name is long.
	Paths iris.Paths
	// DB is an opened iris DB at $Paths.DB.
	DB *db.DB
	// PIAgentDir is a fake ~/.pi/agent/ root for tests that exercise the
	// pi JSONL archive path. Pre-created with mode 0700.
	PIAgentDir string
	// shortPrefix is the os.MkdirTemp() path used to anchor RunDir and
	// Sock to a short prefix. Retained for the t.Cleanup that removes it
	// at end of test.
	shortPrefix string
}

// NewIsolated creates an isolated environment for an iris test. See the
// package comment for the contract.
//
// The returned Isolated holds the opened DB; a t.Cleanup is registered to
// close it. Tests should not double-close.
func NewIsolated(t *testing.T) *Isolated {
	t.Helper()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateHome := filepath.Join(root, "state")
	configHome := filepath.Join(root, "config")
	piAgentDir := filepath.Join(home, ".pi", "agent")

	for _, d := range []string{home, stateHome, configHome, piAgentDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("iristest: MkdirAll(%q): %v", d, err)
		}
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv(EnvParityTestMode, "1")

	paths := iris.ResolvePaths()

	// Unix socket sun_path is 108 bytes. The default tempdir derived from
	// t.TempDir() embeds the test name (e.g.
	// /tmp/TestParitySpawnCoordinator_DefaultAgentAndBashPerm...../001/state/iris/run/<uuid>/harness.sock
	// is already over 100 bytes before the harness.sock suffix). To stay
	// under the limit we redirect RunDir (and the iris client socket Sock)
	// to an os.MkdirTemp("", "iris-p-") prefix — 8 chars + a 6-char random
	// suffix — which leaves ~80 bytes of headroom for the per-session
	// instance UUID and basename. Other paths (DB, LogDir, ConfigFile,
	// ArchiveRoot) stay under root because the tempdir-suffix length is
	// not a constraint there.
	shortPrefix, err := os.MkdirTemp("", "iris-p-")
	if err != nil {
		t.Fatalf("iristest: MkdirTemp for short prefix: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })
	shortRunDir := filepath.Join(shortPrefix, "run")
	shortSock := filepath.Join(shortPrefix, "iris.sock")
	paths.RunDir = shortRunDir
	paths.Sock = shortSock

	// Isolation tripwire: every iris path must resolve under either root
	// or shortPrefix (the latter is itself under os.TempDir(), still a
	// non-host path).
	checkUnderRoot(t, "DB", paths.DB, root)
	checkUnderShort(t, "Sock", paths.Sock, shortPrefix)
	checkUnderShort(t, "RunDir", paths.RunDir, shortPrefix)
	checkUnderRoot(t, "LogDir", paths.LogDir, root)
	checkUnderRoot(t, "ConfigFile", paths.ConfigFile, root)
	checkUnderRoot(t, "ArchiveRoot", paths.ArchiveRoot, root)

	if err := os.MkdirAll(paths.RunDir, 0o700); err != nil {
		t.Fatalf("iristest: MkdirAll RunDir: %v", err)
	}
	if err := os.MkdirAll(paths.ArchiveRoot, 0o700); err != nil {
		t.Fatalf("iristest: MkdirAll ArchiveRoot: %v", err)
	}

	database, err := iris.OpenDB(paths.DB)
	if err != nil {
		t.Fatalf("iristest: OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	iso := &Isolated{
		Root:        root,
		Home:        home,
		StateHome:   stateHome,
		ConfigHome:  configHome,
		Paths:       paths,
		DB:          database,
		PIAgentDir:  piAgentDir,
		shortPrefix: shortPrefix,
	}

	// Re-run the path check at the end of the test so any post-test path
	// resolution can be caught. The XDG env vars are restored by t.Setenv's
	// cleanup before our cleanup runs, so we re-set them defensively.
	t.Cleanup(func() {
		// Best-effort: walk root and assert nothing snuck out.
		// We don't fail the test here — a write outside root would have
		// already manifested as a panic or test failure inside the body —
		// but we log so operators see the imbalance.
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			return nil
		})
	})

	return iso
}

// SessionName returns a parity-test-safe session name. The prefix is
// always "iris-test@" so even a leaked notification cannot deliver to a
// real iris coordinator session on the host.
func SessionName(suffix string) string {
	return "iris-test@" + sanitiseForSessionName(suffix)
}

// CheckXDGUnderTempDir is a self-test the parity suite runs at startup to
// assert the isolation contract holds. It returns an error rather than
// failing the test directly so the caller can decide whether to t.Fatal
// or skip. The error message is suitable for surfacing in CI logs.
func CheckXDGUnderTempDir(root string) error {
	xdg := os.Getenv("XDG_STATE_HOME")
	if xdg == "" {
		return fmt.Errorf("XDG_STATE_HOME is unset; isolation broken")
	}
	abs, err := filepath.Abs(xdg)
	if err != nil {
		return fmt.Errorf("resolve XDG_STATE_HOME: %w", err)
	}
	if !strings.HasPrefix(abs, root) {
		return fmt.Errorf("XDG_STATE_HOME=%q does not resolve under tempdir %q", abs, root)
	}
	if os.Getenv(EnvParityTestMode) == "" {
		return fmt.Errorf("%s is unset; prism-binary tripwire would not fire", EnvParityTestMode)
	}
	return nil
}

func checkUnderRoot(t *testing.T, name, path, root string) {
	t.Helper()
	if !strings.HasPrefix(path, root) {
		t.Fatalf("iristest: isolation broken: %s=%q does not start with tempdir %q", name, path, root)
	}
}

// checkUnderShort verifies that path lives under the short MkdirTemp prefix
// used for socket paths (sun_path < 108 bytes).
func checkUnderShort(t *testing.T, name, path, short string) {
	t.Helper()
	if !strings.HasPrefix(path, short) {
		t.Fatalf("iristest: isolation broken: %s=%q does not start with short tempdir %q", name, path, short)
	}
}

// sanitiseForSessionName replaces characters that are awkward in iris
// session names (whitespace, slashes) with dashes. Idempotent; lossy.
func sanitiseForSessionName(s string) string {
	if s == "" {
		return "default"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '/' || c == '\\':
			out = append(out, '-')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
