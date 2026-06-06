package cmd

// PRISM_USE_MUX cutover plumbing.
//
// This file owns the per-command gate and the user-facing diagnostic
// for the prismd-mux-not-running case. The four entry points
// (`prism spawn`, `cleanup`, `switch`, `nav`) check muxCutoverEnabled
// at the top of their run handlers and route through internal/mux/client
// instead of internal/tmux when the gate is on.
//
// Design notes per issue #2158:
//
//   - Default off. The env var is the only signal; absent / set to
//     anything other than "1" leaves the tmux path unchanged.
//   - Never silently fall back. If the daemon is not reachable we
//     surface a structured error with the canonical recovery steps
//     (`prismd mux start`, the systemd / launchd commands) rather
//     than pretending tmux is fine. The whole point of the flag is
//     to exercise the mux path deterministically; a silent fallback
//     would contaminate Ben's phase-3 soak.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/client"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// Indirections used by switchClientOrMuxSession so unit tests can
// substitute the tmux side without standing up a real tmux server.
// Production code passes the real internal/tmux functions through;
// tests override via the package-private vars in mux_cutover_test.go.
var (
	tmuxCurrentClient       = tmux.CurrentClient
	tmuxCallerClient        = tmux.CallerClient
	tmuxSwitchClient        = tmux.SwitchClient
	tmuxSwitchClientCurrent = tmux.SwitchClientCurrent
)

// muxCutoverEnvVar is the sentinel the four CLI gates check. We test
// for the literal value "1" rather than a "truthy" parse so a typo
// like PRISM_USE_MUX=yes does not silently enable the new path.
const muxCutoverEnvVar = "PRISM_USE_MUX"

// muxClientTimeout bounds a single round-trip to the daemon. 5 s
// mirrors the host-API sidecar client and the mux client's own
// default (internal/mux/client.defaultTimeout); we name it here so
// the CLI cutover code reads as a self-contained block.
const muxClientTimeout = 5 * time.Second

// muxCutoverEnabled returns true when the four CLI gates should route
// through internal/mux/client instead of internal/tmux. The check is
// strict: PRISM_USE_MUX must be exactly "1".
//
// Exported as a function (rather than read inline) so a follow-up PR
// can add a config-file fallback or a per-machine override without
// touching every gate site.
func muxCutoverEnabled() bool {
	return os.Getenv(muxCutoverEnvVar) == "1"
}

// newMuxClient constructs a configured *client.Client for the cutover
// gates. The socket path falls back to the daemon's canonical
// $XDG_STATE_HOME/prism/run/<hash>/mux.sock; the per-request timeout
// is muxClientTimeout. Callers are responsible for Close()-ing the
// returned client when done with it.
//
// The function is deliberately tiny so the gates can call it without
// hiding the construction shape. Tests that need to inject a different
// socket path (e.g. against a t.TempDir() server) call client.New
// directly with WithSocketPath instead.
func newMuxClient() (*client.Client, error) {
	return client.New(client.WithTimeout(muxClientTimeout))
}

// daemonNotRunningError formats the canonical "prismd mux daemon is
// not running" diagnostic the four gates surface when client.New or
// the first request returns client.ErrServerUnavailable. The shape
// mirrors the issue text verbatim — Linux gets the systemd line,
// Darwin gets the launchctl line, both get the "or run it directly"
// fallback.
//
// cmdName is the calling subcommand name ("prism spawn", "prism
// cleanup", etc.) so the operator can copy-paste the error message
// straight into a bug report without losing context.
func daemonNotRunningError(cmdName string) error {
	platformHint := platformSupervisorHint()
	return fmt.Errorf(`%s: prismd mux daemon is not running

  %s

Or run it directly:

  prismd mux start`, cmdName, platformHint)
}

// platformSupervisorHint returns the OS-appropriate command for
// starting the user's mux supervisor on the current host. It is a
// thin wrapper over platformSupervisorHintFor(runtime.GOOS) — the
// split lets tests pin the hint text for both Linux and Darwin
// without requiring a cross-platform test runner.
func platformSupervisorHint() string {
	return platformSupervisorHintFor(runtime.GOOS)
}

// platformSupervisorHintFor returns the supervisor hint for the
// named GOOS value. Exposed (lower-case, package-private) so the
// test suite can assert both branches deterministically.
//
// Darwin path note: the plist filename must match what home-manager's
// launchd module actually installs. home-manager hard-codes
// `labelPrefix = "org.nix-community.home."` and combines it with the
// service name (`prismd-mux`), so the on-disk file is
// `~/Library/LaunchAgents/org.nix-community.home.prismd-mux.plist`.
// A previous version of this string dropped the `home.` segment and
// pointed Darwin operators at a non-existent file — see PR #2175
// review-context.
func platformSupervisorHintFor(goos string) string {
	switch goos {
	case "linux":
		return "systemctl --user start prismd-mux         # Linux"
	case "darwin":
		// Resolve $UID via the env (cheap and stable; getuid()
		// would require a syscall on every error path). Fallback to
		// a literal "<uid>" placeholder when the var is unset.
		uid := os.Getenv("UID")
		if uid == "" {
			uid = "<uid>"
		}
		return fmt.Sprintf("launchctl bootstrap user/%s ~/Library/LaunchAgents/org.nix-community.home.prismd-mux.plist   # Darwin", uid)
	default:
		return "# (no supervisor hint for runtime.GOOS=" + goos + ")"
	}
}

// surfaceDaemonError checks whether err is the client-side
// "daemon not reachable" sentinel and rewrites it into the canonical
// CLI diagnostic. Other errors are returned unchanged. Callers wrap
// every client.* call with this so the operator never sees a raw
// "dial unix: no such file or directory" frame.
func surfaceDaemonError(cmdName string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, client.ErrServerUnavailable) {
		return daemonNotRunningError(cmdName)
	}
	return err
}

// stdoutW returns the global stdout writer the cutover gates print
// to. Centralised here so a future test fixture can substitute a
// bytes.Buffer without touching every print call.
func stdoutW() io.Writer { return os.Stdout }

// switchClientOrMuxSession routes a "switch the active session to
// target" intent through the right substrate. Under PRISM_USE_MUX=1
// it tells the mux daemon to change its active-session pointer; in
// the default path it falls back to tmux's switch-client primitive.
//
// Centralised here so every picker-driven entry point (the top-level
// picker, the review-session attach, the review-group agent pick)
// branches on the gate uniformly — a single function call instead of
// a copy-pasted if/else at every site, and one place to audit when
// the cutover invariant changes.
//
// cmdName is forwarded to surfaceDaemonError so the error message
// names the calling subcommand ("prism switch" / "prism nav").
func switchClientOrMuxSession(cmdName, target string) error {
	if muxCutoverEnabled() {
		mc, err := newMuxClient()
		if err != nil {
			return surfaceDaemonError(cmdName, err)
		}
		defer mc.Close()

		ctx, cancel := context.WithTimeout(context.Background(), muxClientTimeout)
		defer cancel()

		if _, err := mc.Sessions().Switch(ctx, target); err != nil {
			return surfaceDaemonError(cmdName, err)
		}
		return nil
	}
	client, _ := tmuxCurrentClient()
	if client == "" {
		client = tmuxCallerClient()
	}
	if client != "" {
		return tmuxSwitchClient(client, target)
	}
	_, err := tmuxSwitchClientCurrent(target)
	return err
}
