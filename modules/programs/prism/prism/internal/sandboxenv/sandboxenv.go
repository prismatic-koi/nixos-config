// Package sandboxenv provides helpers for detecting whether the current
// process is running inside a prism sandbox and for accessing sandbox
// configuration injected via environment variables.
//
// # Sandbox sentinel
//
// IsInsideSandbox uses PRISM_HOST_API != "" as its sentinel:
//
//   - PRISM_HOST_API is set exclusively by the prism sidecar when launching a
//     sandboxed session (bwrap or sandbox-exec).
//   - PRISM_SPAWN_PATH is also set in sandboxed sessions but serves as a
//     working-directory hint only — it is NOT a sandbox sentinel and NOT a
//     keybind discriminator. The dedicated tmux-keybind sentinel is
//     PRISM_KEYBIND_SPAWN, which the sidecar never injects into a sandboxed
//     session.
//
// Callers that need to know "am I sandboxed?" should use IsInsideSandbox()
// rather than repeating the inline os.Getenv check.
package sandboxenv

import "os"

// IsInsideSandbox reports whether the current process is running inside a
// prism sandbox (bwrap or sandbox-exec).
//
// It uses PRISM_HOST_API != "" as the sentinel: the sidecar sets this variable
// exclusively when launching a sandboxed session, so its presence is a reliable
// indicator that the process is inside a sandbox.
func IsInsideSandbox() bool {
	return os.Getenv("PRISM_HOST_API") != ""
}

// HostAPISocket returns the host-API URL injected into the sandbox via the
// PRISM_HOST_API environment variable. Returns "" if not set.
//
// NOTE: despite the name, this returns a URL string (e.g.
// "unix:///path/to/sock" or "http://host:port"), not a bare socket path.
// Callers pass it to proxyToHostAPI / hostapi helpers that parse the URL.
// The value is returned verbatim — no parsing or transformation is applied.
func HostAPISocket() string {
	return os.Getenv("PRISM_HOST_API")
}
