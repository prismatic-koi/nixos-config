// bash_sandbox_darwin.go — macOS implementation of the per-tool bash sandbox.
//
// On macOS we use Apple's sandbox-exec with a generated SBPL (version 3)
// profile to create a restricted subprocess with the bash-specific access
// set described in the daemon-mode design doc §5.
//
// Unlike bwrap, sandbox-exec has no bind-mount mechanism; the SBPL profile
// grants read/write access to the host paths directly instead of remapping
// them.  The profile is built from scratch; StandardSandboxMounts is NOT used.
//
// The bash SBPL profile extends the file-tool profile (GenerateFileToolSBPLProfile)
// with the additional rules required by bash:
//   - ~/.cache/nix (RW)
//   - ~/.ssh/access-key, signing-key, signing-key.pub, allowed_signers,
//     known_hosts (RO)
//   - Synthesised ~/.ssh/config (RO via temp file allowed directly)
//   - Synthesised ~/.gitconfig (RO via temp file)
//   - ~/.aws/config, ~/.aws/credentials (RO, EvalSymlinks)
//   - ~/.aws/sso, ~/.aws/cli (RW, conditional)
//   - ~/.kube/config (RO, conditional)
//
// Pi-process credentials (~/.claude, ~/.mcp-auth, ~/.pi/agent/*, ~/.cache/bun,
// ~/.config/pi/*) are NOT included.  Network is permitted.

package iris

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runBashInSandbox runs a bash command in a sandbox-exec sandbox with the D-5
// SBPL profile and credential injection.
func (d *toolDispatcher) runBashInSandbox(ctx context.Context, command string) toolResult {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// Generate per-call gitconfig and SSH config temp files.
	gitconfigPath, sshConfigPath, allowedSignersPath, allowedSignersReady, cleanup, err := generateBashSandboxConfigs(home)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("bash sandbox: prepare configs: %v", err)}
	}
	defer cleanup()

	// Generate the SBPL profile.
	profile := GenerateBashSBPLProfile(home, d.worktree, d.tmpDir, gitconfigPath, sshConfigPath, allowedSignersPath, allowedSignersReady)

	// Write profile to a temp file.
	f, pErr := os.CreateTemp("", "iris-bash-*.sb")
	if pErr != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("bash sandbox: create profile: %v", pErr)}
	}
	profilePath := f.Name()
	if _, wErr := f.WriteString(profile); wErr != nil {
		f.Close()
		os.Remove(profilePath)
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("bash sandbox: write profile: %v", wErr)}
	}
	f.Close()
	defer os.Remove(profilePath)

	// Build the sandbox-exec env as a bash -c invocation.
	// sandbox-exec does not have a --clearenv / --setenv mechanism — the
	// subprocess inherits the parent env.  We exec a shell wrapper that
	// clears the env and re-exports only the bash-specific variables.
	//
	// We construct: env -i VAR=val ... bash -c '<command>'
	// This produces a clean environment without the host LLM API keys.
	// Use the dispatcher's broker so credential resolution is consistent
	// with the audit-log decision recorded against the matching tool_call
	// event (D-7).
	broker := d.broker
	if broker == nil {
		broker = NewCredentialBroker()
	}
	envPairs := broker.ResolveBash(d.role, d.bareRoot).Env

	// Build the argv for sandbox-exec.
	allArgs := []string{"-f", profilePath}
	// Use /usr/bin/env -i to clear the environment, then inject our pairs.
	allArgs = append(allArgs, "/usr/bin/env", "-i")
	allArgs = append(allArgs, envPairs...)
	allArgs = append(allArgs, "bash", "-c", command)

	return d.runSubprocess(ctx, d.worktree, nil, "/usr/bin/sandbox-exec", allArgs...)
}

// GenerateBashSBPLProfile generates an SBPL (version 3) profile for a bash
// tool subprocess on macOS.
//
// Exported so that the Darwin integration test package can generate profiles
// for assertion without constructing a toolDispatcher.
//
// The profile grants the bash subprocess access to:
//   - Standard system read-only roots (same as file-tool profile)
//   - Worktree (RW)
//   - Per-session /tmp backing dir (RW)
//   - ~/.cache/nix (RW)
//   - ~/.ssh/access-key, signing-key, signing-key.pub, allowed_signers,
//     known_hosts (RO, conditional)
//   - Synthesised ~/.ssh/config temp file (RO)
//   - Synthesised ~/.gitconfig temp file (RO)
//   - ~/.aws/config, ~/.aws/credentials (RO, EvalSymlinks paths)
//   - ~/.aws/sso, ~/.aws/cli (RW, conditional)
//   - ~/.kube/config (RO, conditional, EvalSymlinks)
//   - Network (permitted)
//
// Pi-process credentials are NOT in the profile.
func GenerateBashSBPLProfile(home, worktree, tmpDir, gitconfigPath, sshConfigPath, allowedSignersPath string, allowedSignersReady bool) string {
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

	// ── Worktree (RW) ────────────────────────────────────────────────────────
	sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
	sb.WriteString("  (subpath " + sbplQuote(worktree) + "))\n")
	sb.WriteString("\n")

	// ── Per-session /tmp (RW) ────────────────────────────────────────────────
	sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
	if tmpDir != "" {
		sb.WriteString("  (subpath " + sbplQuote(tmpDir) + ")\n")
	}
	sb.WriteString("  (subpath \"/tmp\")\n")
	sb.WriteString("  (subpath \"/private/tmp\"))\n")
	sb.WriteString("\n")

	// ── ~/.cache/nix (RW) ───────────────────────────────────────────────────
	cacheNix := filepath.Join(home, ".cache", "nix")
	if _, err := os.Stat(cacheNix); err == nil {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + sbplQuote(cacheNix) + "))\n")
		sb.WriteString("\n")
	}

	// ── SSH keys (RO, remapped to generic names) ──────────────────────────
	// Allow the in-sandbox generic paths under ~/.ssh/.  On macOS there is no
	// bind-mount mechanism so we grant access to the host key paths directly.
	sshDir := filepath.Join(home, ".ssh")
	const accessKeyName = "prismatic-koi-ed25519"
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, accessKeyName)); err == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(resolved) + "))\n")
		sb.WriteString("\n")
	}
	const signingKeyName = "prismatic-koi-ed25519-signingkey"
	signingKeyPrivResolved, errPriv := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName))
	signingKeyPubResolved, errPub := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName+".pub"))
	if errPriv == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(signingKeyPrivResolved) + "))\n")
		sb.WriteString("\n")
	}
	if errPub == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(signingKeyPubResolved) + "))\n")
		sb.WriteString("\n")
	}
	if allowedSignersReady && allowedSignersPath != "" {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(allowedSignersPath) + "))\n")
		sb.WriteString("\n")
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, "known_hosts")); err == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(resolved) + "))\n")
		sb.WriteString("\n")
	}

	// ── Synthesised SSH config temp file (RO) ────────────────────────────────
	if sshConfigPath != "" {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(sshConfigPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── Synthesised gitconfig temp file (RO) ─────────────────────────────────
	if gitconfigPath != "" {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(gitconfigPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── AWS credentials (RO, EvalSymlinks) ───────────────────────────────────
	awsConfigSrc := filepath.Join(home, ".config", "aws", "readonly-config")
	if resolved, err := filepath.EvalSymlinks(awsConfigSrc); err == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(resolved) + "))\n")
		sb.WriteString("\n")
	}
	awsCredsSrc := filepath.Join(home, ".config", "aws", "credentials")
	if resolved, err := filepath.EvalSymlinks(awsCredsSrc); err == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(resolved) + "))\n")
		sb.WriteString("\n")
	}

	// ── ~/.aws/sso (RW, conditional) ─────────────────────────────────────────
	awsSSOPath := filepath.Join(home, ".aws", "sso")
	if _, err := os.Stat(awsSSOPath); err == nil {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + sbplQuote(awsSSOPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── ~/.aws/cli (RW, conditional) ─────────────────────────────────────────
	awsCLIPath := filepath.Join(home, ".aws", "cli")
	if _, err := os.Stat(awsCLIPath); err == nil {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + sbplQuote(awsCLIPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── ~/.kube/config (RO, conditional, EvalSymlinks) ───────────────────────
	kubeConfigSrc := filepath.Join(home, ".config", "kube", "agents-config")
	if resolved, err := filepath.EvalSymlinks(kubeConfigSrc); err == nil {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + sbplQuote(resolved) + "))\n")
		sb.WriteString("\n")
	}

	// ── Process and IPC primitives (required by dyld, AMFI) ─────────────────
	sb.WriteString("(allow process-exec process-fork signal)\n")
	sb.WriteString("(allow mach-lookup)\n")
	sb.WriteString("(allow ipc-posix-shm-read-data ipc-posix-shm-write-data ipc-posix-shm-write-create)\n")
	sb.WriteString("(allow syscall-unix syscall-mach system-mac-syscall system-fcntl)\n")
	sb.WriteString("(allow sysctl-read sysctl-write)\n")
	sb.WriteString("\n")

	// ── Network (permitted per design doc §5) ────────────────────────────────
	sb.WriteString("(allow network*)\n")
	sb.WriteString("\n")

	// ── File metadata / iokit / misc ─────────────────────────────────────────
	sb.WriteString("(allow file-read-metadata (subpath \"/\"))\n")
	sb.WriteString("(allow iokit-open iokit-get-properties iokit-issue-command)\n")
	sb.WriteString("(allow lsopen)\n")

	return sb.String()
}
