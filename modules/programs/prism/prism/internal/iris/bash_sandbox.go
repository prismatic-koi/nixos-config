package iris

// bash_sandbox.go — OS-independent helpers for the bash tool sandbox (D-5).
//
// Shared between bash_sandbox_linux.go (bwrap) and bash_sandbox_darwin.go
// (sandbox-exec).
//
// This file provides:
//   - generateBashSandboxConfigs: writes per-call gitconfig, SSH config, and
//     allowed_signers temp files; returns paths and a cleanup function.
//   - spillThreshold: the output size above which bash output is spilled to
//     a temp file (matching pi's /tmp/pi-bash-<id>.log convention).
//   - spillOutput: write oversized output to a spill file.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// spillThreshold is the maximum output size (in bytes) that the bash tool
// returns inline.  Output exceeding this threshold is written to a spill file
// in the per-session /tmp directory, matching pi's built-in bash executor
// convention (pi-rpc-interface.md Q6: /tmp/pi-bash-<id>.log).
const spillThreshold = 50 * 1024 // 50 KB

// maybeSpill checks whether output exceeds spillThreshold.  If so, it writes
// the full output to a spill file inside tmpDir and returns a truncated result
// that references the spill file path so the LLM can subsequently read it.
//
// The spill file is named pi-bash-<toolExecID>.log to match pi's convention
// (/tmp/pi-bash-<id>.log).
//
// If tmpDir is empty, output is returned as-is regardless of size.
func maybeSpill(output, toolExecID, tmpDir string) (finalOutput string, details map[string]any) {
	if tmpDir == "" || len(output) <= spillThreshold {
		return output, nil
	}

	// Create the spill file in the per-session /tmp directory.
	spillName := "pi-bash-" + toolExecID + ".log"
	spillPath := filepath.Join(tmpDir, spillName)

	if err := os.WriteFile(spillPath, []byte(output), 0o644); err != nil {
		// If we can't write the spill file, return the truncated output with a note.
		truncated := output[:spillThreshold]
		truncated += fmt.Sprintf("\n... [output truncated at %d bytes; spill file write failed: %v]", spillThreshold, err)
		return truncated, nil
	}

	// Return a brief summary pointing at the spill file.
	truncated := output[:spillThreshold]
	summary := fmt.Sprintf("%s\n\n[output truncated — full output (%d bytes) written to %s]",
		truncated, len(output), spillPath)
	return summary, map[string]any{
		"spill_path": spillPath,
	}
}

// generateBashSandboxConfigs writes per-call gitconfig, SSH config, and
// (optionally) allowed_signers temp files to be mounted inside the bash
// sandbox.  Returns the absolute paths, whether allowed_signers was written
// successfully, and a cleanup function that removes the temp files.
//
// The generated configs mirror what container.Manager.writeGitconfig /
// writeSshConfig produce for the bwrap sandbox.  They are regenerated per
// tool call rather than per session to keep credential freshness and avoid
// having to plumb a persistent container.Manager through the iris tool
// dispatcher.
func generateBashSandboxConfigs(home string) (gitconfigPath, sshConfigPath, allowedSignersPath string, allowedSignersReady bool, cleanup func(), err error) {
	sshDir := filepath.Join(home, ".ssh")

	var tempFiles []string
	cleanup = func() {
		for _, f := range tempFiles {
			_ = os.Remove(f)
		}
	}

	// ── SSH config ──────────────────────────────────────────────────────────
	// Minimal SSH config for GitHub: uses the in-sandbox access-key path.
	sshConfigContent := "Host github.com\n" +
		"  StrictHostKeyChecking accept-new\n" +
		"  IdentityFile " + filepath.Join(sshDir, "access-key") + "\n" +
		"  IdentitiesOnly yes\n"
	sshConfigFile, sshErr := os.CreateTemp("", "iris-bash-ssh-config-*")
	if sshErr != nil {
		cleanup()
		return "", "", "", false, cleanup, fmt.Errorf("create ssh config temp file: %w", sshErr)
	}
	sshConfigPath = sshConfigFile.Name()
	tempFiles = append(tempFiles, sshConfigPath)
	if _, writeErr := sshConfigFile.WriteString(sshConfigContent); writeErr != nil {
		sshConfigFile.Close()
		cleanup()
		return "", "", "", false, cleanup, fmt.Errorf("write ssh config: %w", writeErr)
	}
	sshConfigFile.Close()
	if chmodErr := os.Chmod(sshConfigPath, 0o600); chmodErr != nil {
		cleanup()
		return "", "", "", false, cleanup, fmt.Errorf("chmod ssh config: %w", chmodErr)
	}

	// ── Gitconfig ───────────────────────────────────────────────────────────
	// Mirrors container.Manager.writeGitconfig for isolationBwrap mode.
	const signingKeyName = "prismatic-koi-ed25519-signingkey"
	signingKeyPubPath := filepath.Join(sshDir, signingKeyName+".pub")
	_, errPriv := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName))
	signingKeyPubResolved, errPub := filepath.EvalSymlinks(signingKeyPubPath)

	hasSigning := errPriv == nil && errPub == nil

	// Build the gitconfig content.
	var sb strings.Builder

	// Read git identity from the environment (pi sets these; match bwrap pattern).
	gitName := os.Getenv("GIT_AUTHOR_NAME")
	gitEmail := os.Getenv("GIT_AUTHOR_EMAIL")

	if gitName != "" && gitEmail != "" {
		sb.WriteString("[user]\n")
		sb.WriteString("    name = " + gitName + "\n")
		sb.WriteString("    email = " + gitEmail + "\n")
		if hasSigning {
			// In-sandbox path for the signing public key.
			sandboxSigningKeyPub := filepath.Join(sshDir, "signing-key.pub")
			sb.WriteString("    signingKey = " + sandboxSigningKeyPub + "\n")
		}
	}

	if hasSigning && gitName != "" && gitEmail != "" {
		// Try to write allowed_signers.
		pubKeyContent, readErr := os.ReadFile(signingKeyPubResolved)
		if readErr == nil {
			allowedSignersContent := gitEmail + " " + strings.TrimSpace(string(pubKeyContent)) + "\n"
			allowedSignersFile, aErr := os.CreateTemp("", "iris-bash-allowed-signers-*")
			if aErr == nil {
				allowedSignersPath = allowedSignersFile.Name()
				tempFiles = append(tempFiles, allowedSignersPath)
				if _, wErr := allowedSignersFile.WriteString(allowedSignersContent); wErr == nil {
					allowedSignersReady = true
				}
				allowedSignersFile.Close()
			}
		}

		if allowedSignersReady {
			sandboxAllowedSigners := filepath.Join(sshDir, "allowed_signers")
			sb.WriteString("\n[commit]\n")
			sb.WriteString("    gpgsign = true\n")
			sb.WriteString("\n[gpg]\n")
			sb.WriteString("    format = ssh\n")
			sb.WriteString("\n[gpg \"ssh\"]\n")
			sb.WriteString("    allowedSignersFile = " + sandboxAllowedSigners + "\n")
		}
	}

	// Always include these sections.
	sb.WriteString("\n[push]\n")
	sb.WriteString("    autoSetupRemote = true\n")
	sb.WriteString("\n[init]\n")
	sb.WriteString("    defaultBranch = main\n")

	gitconfigFile, gcErr := os.CreateTemp("", "iris-bash-gitconfig-*")
	if gcErr != nil {
		cleanup()
		return "", "", "", false, cleanup, fmt.Errorf("create gitconfig temp file: %w", gcErr)
	}
	gitconfigPath = gitconfigFile.Name()
	tempFiles = append(tempFiles, gitconfigPath)
	if _, wErr := gitconfigFile.WriteString(sb.String()); wErr != nil {
		gitconfigFile.Close()
		cleanup()
		return "", "", "", false, cleanup, fmt.Errorf("write gitconfig: %w", wErr)
	}
	gitconfigFile.Close()
	if chmodErr := os.Chmod(gitconfigPath, 0o644); chmodErr != nil {
		cleanup()
		return "", "", "", false, cleanup, fmt.Errorf("chmod gitconfig: %w", chmodErr)
	}

	return gitconfigPath, sshConfigPath, allowedSignersPath, allowedSignersReady, cleanup, nil
}
