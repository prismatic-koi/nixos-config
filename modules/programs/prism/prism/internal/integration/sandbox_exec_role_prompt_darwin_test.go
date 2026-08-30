//go:build darwin

package integration_test

// Integration tests for the prism agent role-prompt directory under
// sandbox-exec.
// The prism PI extension reads ~/.config/prism/agents/<role>.md at
// before_agent_start and injects it as the role system prompt. The
// capability is the explicit RO (subpath ~/.config/prism/agents) grant in
// the section-5f block emitted by generateProfile, evaluated at the REAL
// host path.
//
// Read-target strategy: on deployed hosts the
// agents dir is home-manager-managed — typically a read-only symlink into
// the nix store, overwritten on every switch — so planting a test sentinel
// there is impossible. The tests therefore prefer the DEPLOYED role file
// (worker.md) when one exists, and fall back to sentinel-planting only when
// the dir is genuinely writable (dev machines without hm management).
//
// Per the convention this file carries:
//
//   - a positive (the role file readable at the real path under the
//     production profile — the exact capability the PI extension needs),
//   - a whole-block strip negative (removing the ENTIRE 5f block makes the
//     same read fail — stripping only the agents (subpath ...) line leaves
//     the clipboard line carrying the block).
//     The strip negative additionally requires a read target whose
//     EvalSymlinks-resolved path is the lexical path itself: SBPL evaluates
//     open(2)-class operations against the RESOLVED target (same as the
//     documented §5g no-strip-negative deviation), so a store-symlinked
//     deployed role file remains readable
//     via the §2 /nix allow even with 5f stripped — the denial is
//     unobservable there. On hm-managed hosts where no in-place target can
//     be planted either, the strip negative SKIPs with that justification;
//     the 5f grant shape is pinned at unit level.
//   - a write-denied negative (RO must not silently become RW). No sentinel
//     needed: attempting to create a file in the agents dir from inside the
//     sandbox and asserting denial suffices. On hm hosts the write is doubly
//     denied (no SBPL write grant on the resolved store path + the store
//     itself is unwritable) — both denials are correct outcomes.
//
// Build tag: darwin (sandbox-exec is Darwin-only).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// rolePromptSentinelContent is the sentinel written into a planted role file
// on hosts where the agents dir is writable (dev machines without hm
// management).
const rolePromptSentinelContent = "PRISM-2032-ROLE-PROMPT-SENTINEL"

// rolePromptAgentsDir returns the real-host path of the role-prompt agents
// dir governed by the section-5f grant.
func rolePromptAgentsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(realUserHome(t), ".config", "prism", "agents")
}

// plantRolePromptSentinel attempts to plant a sentinel role file in the real
// agents dir. Returns (path, true) on success. Returns ("", false) when the
// dir hierarchy cannot be created or written — e.g. on home-manager-managed
// hosts where ~/.config/prism/agents is a read-only symlink into the nix
// store. Cleanup removes only what this call created.
func plantRolePromptSentinel(t *testing.T) (string, bool) {
	t.Helper()
	home := realUserHome(t)
	agentsDir := rolePromptAgentsDir(t)
	prismDir := filepath.Join(home, ".config", "prism")

	// Track pre-existence at each level so cleanup removes ONLY what this
	// call created — a pre-existing ~/.config/prism (e.g. holding the live
	// config.json on a non-hm dev machine) must never be deleted. Mirrors
	// prepareClipboardSentinel's scoping.
	prismDirExisted := false
	if _, statErr := os.Stat(prismDir); statErr == nil {
		prismDirExisted = true
	}
	agentsDirExisted := false
	if _, statErr := os.Stat(agentsDir); statErr == nil {
		agentsDirExisted = true
	}
	if mkErr := os.MkdirAll(agentsDir, 0o700); mkErr != nil {
		return "", false
	}

	sentinel := filepath.Join(agentsDir, ".prism-2245-test-role.md")
	if wErr := os.WriteFile(sentinel, []byte(rolePromptSentinelContent), 0o600); wErr != nil {
		// MkdirAll may have created the dir(s) before the write failed —
		// remove what was created even on the failure path.
		if !agentsDirExisted {
			if prismDirExisted {
				_ = os.Remove(agentsDir)
			} else {
				_ = os.RemoveAll(prismDir)
			}
		}
		return "", false
	}

	switch {
	case agentsDirExisted:
		// Dir pre-existed — remove only the planted sentinel.
		t.Cleanup(func() { _ = os.Remove(sentinel) })
	case prismDirExisted:
		// ~/.config/prism pre-existed but agents/ did not — remove only the
		// agents dir this call created (with the sentinel inside it).
		t.Cleanup(func() { _ = os.RemoveAll(agentsDir) })
	default:
		// Neither existed — this call created ~/.config/prism and below.
		t.Cleanup(func() { _ = os.RemoveAll(prismDir) })
	}
	return sentinel, true
}

// TestSandboxExecProfile_RolePromptReadable is the positive integration test:
// the role-prompt markdown is readable at its REAL host path under the
// production profile via the section-5f RO grant — the same path shape the
// PI extension resolves. The deployed worker.md is preferred as the read
// target (see the
// file-top strategy note); a planted sentinel is the dev-machine fallback.
func TestSandboxExecProfile_RolePromptReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	deployed := filepath.Join(rolePromptAgentsDir(t), "worker.md")
	var readTarget, wantContent string
	if content, readErr := os.ReadFile(deployed); readErr == nil && len(content) > 0 {
		// Deployed role file present (hm-managed or otherwise) — read the
		// real thing. This is the exact file the PI extension loads.
		readTarget, wantContent = deployed, string(content)
	} else if planted, ok := plantRolePromptSentinel(t); ok {
		readTarget, wantContent = planted, rolePromptSentinelContent
	} else {
		t.Skipf("no readable deployed role file at %s and the agents dir is not writable for sentinel planting", deployed)
	}

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The production profile must carry the whole 5f block.
	block := roGrantBlock5f(t)
	if !strings.Contains(prepared.content, block) {
		t.Fatalf("generated profile does not contain the section-5f RO block:\n%s\nProfile:\n%s", block, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read the role prompt at the REAL path from inside the sandbox.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"cat "+shQuote(readTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("reading the role prompt at its real path failed under the production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, readTarget)
	}
	if !strings.Contains(string(out), wantContent) {
		t.Errorf("role-prompt read did not return the expected content.\nTarget: %s\nGot: %s", readTarget, string(out))
	}
}

// TestSandboxExecProfile_RolePromptDeniedWithoutGrantBlock is the paired
// strip negative: removing the ENTIRE section-5f block makes the same
// real-path read fail — proving the block is load-bearing and the positive
// is not green by accident (whole-block strip).
//
// The read target must resolve to itself (no symlinks): SBPL evaluates
// open(2) against the RESOLVED path, so a deployed role file that is a
// symlink into /nix/store stays readable via the §2 /nix allow even with 5f
// stripped — the denial cannot manifest (the same mechanism behind the
// documented §5g no-strip-negative deviation). On hosts where no in-place
// target exists or can be planted (hm-managed agents dir), the test SKIPs;
// the 5f grant shape is pinned at unit level and the RO property is covered
// by the write-denied negative below.
func TestSandboxExecProfile_RolePromptDeniedWithoutGrantBlock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	deployed := filepath.Join(rolePromptAgentsDir(t), "worker.md")
	var readTarget string
	if resolved, evalErr := filepath.EvalSymlinks(deployed); evalErr == nil && resolved == deployed {
		// Deployed role file is a regular file at its lexical path — the
		// 5f grant is the only rule covering it, so the strip is observable.
		readTarget = deployed
	} else if planted, ok := plantRolePromptSentinel(t); ok {
		// Planted sentinel is a regular file in a real dir — resolves to
		// itself by construction.
		readTarget = planted
	} else {
		t.Skipf("no strip-negative-capable read target: the deployed role file resolves outside its lexical path "+
			"(reads through it are independently allowed at the resolved target, e.g. the §2 /nix allow — "+
			"same justification as the documented §5g no-strip-negative deviation) and the agents dir is not "+
			"writable for sentinel planting; the 5f grant shape is pinned at unit level (deployed: %s)", deployed)
	}

	m := newProfileManagerWithBareRoot(t)

	block := roGrantBlock5f(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, block, "", 1)
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"cat "+shQuote(readTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("role-prompt read succeeded WITHOUT the section-5f block.\n"+
			"The 5f grant is not load-bearing — investigate.\n"+
			"Output: %s\nMutated profile: %s\nTarget: %s", string(out), mutatedPath, readTarget)
	} else {
		t.Logf("ka pai — role-prompt read correctly denied without the 5f block (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_RolePromptWriteDenied is the write-denied negative:
// under the PRODUCTION profile, writing into the real agents dir fails — the
// 5f grant is read-only and must not silently become RW (agents never write
// their own role prompts). No sentinel is needed: the assertion is on the
// in-sandbox CREATE attempt itself. On hm-managed hosts the write is doubly
// denied (no SBPL write grant on the resolved store path + the nix store is
// unwritable) — either denial is the correct outcome; the SBPL grant shape
// is pinned at unit level.
func TestSandboxExecProfile_RolePromptWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	agentsDir := rolePromptAgentsDir(t)

	// The dir must exist on the host so the in-sandbox failure is a denial,
	// not a vacuous ENOENT. On deployed hosts it exists (hm-managed); on dev
	// machines create it (and clean up what we created).
	if _, statErr := os.Stat(agentsDir); statErr != nil {
		home := realUserHome(t)
		prismDirExisted := false
		if _, pErr := os.Stat(filepath.Join(home, ".config", "prism")); pErr == nil {
			prismDirExisted = true
		}
		if mkErr := os.MkdirAll(agentsDir, 0o700); mkErr != nil {
			t.Skipf("agents dir %s does not exist and cannot be created: %v", agentsDir, mkErr)
		}
		if prismDirExisted {
			t.Cleanup(func() { _ = os.Remove(agentsDir) })
		} else {
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(home, ".config", "prism")) })
		}
	}

	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	writeTarget := filepath.Join(agentsDir, ".prism-2245-write-denied.md")
	t.Cleanup(func() { _ = os.Remove(writeTarget) }) // in case the deny fails
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c",
		"echo prism-2245-write > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write into the RO agents dir succeeded under the production profile — RO silently became RW.\n"+
			"Output: %s\nProfile: %s\nTarget: %s", string(out), testProfilePath, writeTarget)
	} else {
		t.Logf("ka pai — agents-dir write correctly denied under the production profile (exit: %v)", runErr)
	}
}
