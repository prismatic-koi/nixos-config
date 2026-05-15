// file_sandbox_darwin.go — macOS implementation of the per-tool file sandbox.
//
// On macOS we use Apple's sandbox-exec with a generated SBPL profile to
// create an isolated subprocess with the per-tool access set described in
// the daemon-mode design doc §5.
//
// macOS does not have bwrap's bind-mount mechanism; the profile grants
// read/write access to the host paths directly instead of remapping them.
// The profile is built from scratch; StandardSandboxMounts is NOT used.

package iris

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runInFileSandbox runs name+argv inside a sandbox-exec file-tool sandbox.
// worktreeReadOnly controls whether the worktree is accessible read-only or
// read-write in the SBPL profile.
// Returns (combined stdout+stderr, exitOK, error).
func (d *toolDispatcher) runInFileSandbox(ctx context.Context, worktreeReadOnly bool, name string, argv ...string) (string, bool, error) {
	profile := GenerateFileToolSBPLProfile(d.worktree, d.tmpDir, worktreeReadOnly)

	resolvedName, err := resolveBinaryPath(name)
	if err != nil {
		return "", false, fmt.Errorf("resolve binary %q: %w", name, err)
	}

	return runInSandboxExecImpl(ctx, profile, d.worktree, nil, resolvedName, argv...)
}

// runInFileSandboxWithStdin is like runInFileSandbox but pipes stdin into the
// subprocess.  Used by the edit and write executors which pass file content
// via stdin rather than as a command-line argument.
func (d *toolDispatcher) runInFileSandboxWithStdin(ctx context.Context, worktreeReadOnly bool, stdin string, name string, argv ...string) (string, bool, error) {
	profile := GenerateFileToolSBPLProfile(d.worktree, d.tmpDir, worktreeReadOnly)

	resolvedName, err := resolveBinaryPath(name)
	if err != nil {
		return "", false, fmt.Errorf("resolve binary %q: %w", name, err)
	}

	return runInSandboxExecImpl(ctx, profile, d.worktree, strings.NewReader(stdin), resolvedName, argv...)
}

// GenerateFileToolSBPLProfile generates a minimal SBPL (version 3) profile
// for a file tool subprocess on macOS.
//
// Exported so that the integration test package can generate profiles for
// assertion without constructing a toolDispatcher (D-4 testing convention).
//
// worktree is accessible RO or RW depending on worktreeReadOnly.
// tmpDir is the per-session tmpfs backing directory on the host.
// The profile follows the same deny-default → specific-allows pattern as
// the existing container/sandbox_exec.go but is built from scratch without
// pi-process credentials.
func GenerateFileToolSBPLProfile(worktree, tmpDir string, worktreeReadOnly bool) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	var sb strings.Builder

	// ── Version 3 header and deny-default ────────────────────────────────────
	sb.WriteString("(version 3)\n")
	sb.WriteString("(deny default)\n")
	sb.WriteString("\n")

	// ── Cryptex graft points (macOS 15+) ─────────────────────────────────────
	sb.WriteString("(allow file-read* file-test-existence file-map-executable\n")
	sb.WriteString("  (subpath \"/System/Volumes/Preboot/Cryptexes\")\n")
	sb.WriteString("  (subpath \"/System/Cryptexes\"))\n")
	sb.WriteString("\n")

	// ── Standard system read-only roots ──────────────────────────────────────
	// These allow Nix-built binaries (the actual tools — cat, grep, find, ls)
	// to start under the sandbox.
	sb.WriteString("(allow file-read* file-test-existence file-map-executable file-read-metadata\n")
	sb.WriteString("  (subpath \"/nix\")\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/bin\")\n")
	sb.WriteString("  (subpath \"/sbin\")\n")
	sb.WriteString("  (subpath \"/System\")\n")
	sb.WriteString("  (subpath \"/Library\")\n")
	sb.WriteString("  (subpath \"/Applications/Xcode.app\")\n")
	sb.WriteString("  (subpath \"/etc\")\n")
	sb.WriteString("  (subpath \"/private/etc\")\n")
	sb.WriteString("  (subpath \"/var\")\n")
	sb.WriteString("  (subpath \"/private/var\")\n")
	sb.WriteString("  (subpath \"/run/current-system\")\n")
	sb.WriteString("  (subpath \"/opt\")\n")
	sb.WriteString("  (literal \"/\")\n")
	sb.WriteString("  (literal \"/dev\")\n")
	sb.WriteString("  (subpath \"/dev\"))\n")
	sb.WriteString("\n")

	// ── Deny sensitive /etc subtrees ─────────────────────────────────────────
	// These override the broad /etc allow above, preventing exfiltration of
	// SSH host keys, VPN configs, and Wi-Fi credentials.
	sb.WriteString("(deny file-read* file-write*\n")
	sb.WriteString("  (subpath \"/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/etc/wpa_supplicant\")\n")
	sb.WriteString("  (subpath \"/etc/ssh\")\n")
	sb.WriteString("  (subpath \"/private/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/private/etc/wpa_supplicant\")\n")
	sb.WriteString("  (subpath \"/private/etc/ssh\"))\n")
	sb.WriteString("\n")

	// ── ~/.nix-profile (RO) ──────────────────────────────────────────────────
	nixProfile := filepath.Join(home, ".nix-profile")
	sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
	sb.WriteString("  (subpath " + sbplQuote(nixProfile) + "))\n")
	sb.WriteString("\n")

	// ── Worktree (RO or RW) ──────────────────────────────────────────────────
	if worktreeReadOnly {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
	} else {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
	}
	sb.WriteString("  (subpath " + sbplQuote(worktree) + "))\n")
	sb.WriteString("\n")

	// ── Per-session /tmp (RW) ────────────────────────────────────────────────
	// On macOS there is no bind-mount mechanism; we allow both the host
	// backing dir (tmpDir) and the canonical /tmp and /private/tmp paths.
	sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
	if tmpDir != "" {
		sb.WriteString("  (subpath " + sbplQuote(tmpDir) + ")\n")
	}
	sb.WriteString("  (subpath \"/tmp\")\n")
	sb.WriteString("  (subpath \"/private/tmp\"))\n")
	sb.WriteString("\n")

	// ── Process and IPC primitives (required by dyld, AMFI) ─────────────────
	sb.WriteString("(allow process-exec process-fork signal)\n")
	sb.WriteString("(allow mach-lookup)\n")
	sb.WriteString("(allow ipc-posix-shm-read-data ipc-posix-shm-write-data ipc-posix-shm-write-create)\n")
	sb.WriteString("(allow syscall-unix syscall-mach system-mac-syscall system-fcntl)\n")
	sb.WriteString("(allow sysctl-read sysctl-write)\n")
	sb.WriteString("\n")

	// ── Network (permitted per design doc §5 and §6.3) ───────────────────────
	sb.WriteString("(allow network*)\n")
	sb.WriteString("\n")

	// ── File metadata / iokit / misc required by dynamic linker ─────────────
	sb.WriteString("(allow file-read-metadata (subpath \"/\"))\n")
	sb.WriteString("(allow iokit-open iokit-get-properties iokit-issue-command)\n")
	sb.WriteString("(allow lsopen)\n")

	return sb.String()
}

// runInSandboxExecImpl runs a command under sandbox-exec with the given SBPL profile.
// stdinReader, if non-nil, is piped to the subprocess's stdin.
func runInSandboxExecImpl(ctx context.Context, profile string, cwd string, stdinReader interface{ Read([]byte) (int, error) }, name string, argv ...string) (string, bool, error) {
	f, err := os.CreateTemp("", "iris-file-tool-*.sb")
	if err != nil {
		return "", false, fmt.Errorf("sandbox-exec: create profile temp file: %w", err)
	}
	profilePath := f.Name()
	if _, err := f.WriteString(profile); err != nil {
		f.Close()
		os.Remove(profilePath)
		return "", false, fmt.Errorf("sandbox-exec: write profile: %w", err)
	}
	f.Close()
	defer os.Remove(profilePath)

	allArgs := []string{"-f", profilePath, name}
	allArgs = append(allArgs, argv...)

	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", allArgs...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if stdinReader != nil {
		cmd.Stdin = stdinReader
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return string(out), false, nil
		}
		return string(out), false, fmt.Errorf("sandbox-exec: %w", err)
	}
	return string(out), true, nil
}
