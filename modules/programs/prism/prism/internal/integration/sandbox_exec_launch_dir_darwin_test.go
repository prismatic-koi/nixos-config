//go:build darwin

package integration_test

// sandbox_exec_launch_dir_darwin_test.go — fixture helpers for launching
// the sandboxed test process from a CWD the profile under test actually
// grants (issue #2247 host-run follow-up; hole pre-existing since #2022 /
// #2221).
//
// Why this exists: exec.Command without cmd.Dir inherits the go-test
// binary's CWD — the integration package dir inside the repo checkout —
// which NO fixture profile grants. Processes that hard-require a resolvable
// CWD then die at bootstrap before exercising the rule under test:
//
//   - node:  "EPERM: process.cwd failed ... uv_cwd"
//   - git:   "fatal: Unable to read current working directory"
//   - bash:  merely WARNS ("shell-init: error retrieving current directory:
//     getcwd: cannot access parent directories: Operation not permitted")
//     and continues — which is why bash-based tests tolerated the hole and
//     the node/git-based tests (the playwright trio, GitConfigGlobalUsable)
//     could never have passed a host run in the old fixture shape.
//
// Production sessions do not hit this: the agent's CWD is the worktree,
// which the production profile grants RW (§6), and the §6b BareRoot
// ancestor block grants metadata up the chain. The fixture fix mirrors
// that production shape rather than widening the production profile:
//
//   1. Launch with cmd.Dir = <sessionDir> — a directory the profile under
//      test grants via the §6 (subpath "<sessionDir>") RW rule, exactly
//      like the production worktree.
//   2. Append test-harness-only allows for the getcwd ancestor walk:
//      getcwd's fallback path lstat()s and readdir()s each ancestor NODE of
//      the CWD to reconstruct the path. The extras grant file-read* on the
//      ancestor (literal ...) nodes only — node contents (readdir) and
//      metadata of those exact directories, nothing beneath them — the
//      fixture analogue of the production §6b ancestor block (which uses
//      metadata-class subpaths for the worktree's chain).
//
// Masking analysis: (literal ...) rules match the directory node only —
// never files or subdirectories beneath it — so these extras cannot mask
// any subtree-scoped negative in the suite (host-Library write-deny,
// clipboard/role-prompt write-deny, ~/.aws outside-carve-out deny,
// sessionDir strip negatives: all target leaf operations under subtrees,
// not the ancestor nodes themselves). The shared augmentProfileForTest
// extras already set the precedent with (literal "/").
//
// The deliberate omission: the launch dir ITSELF gets no extra rule. Its
// readability/writability must come from the production rule under test —
// that is the production-mirroring claim. In strip negatives that disable
// the sessionDir rule, bash (tolerant of getcwd failure) is the required
// in-sandbox binary.
//
// Equally deliberate: CHILDREN of the ancestor nodes are not granted
// (that is what keeps the masking guarantee). Tools that probe them fail
// with EPERM — notably git's repository discovery, which stats
// <parent-of-launch-dir>/.git on its way up and treats the EPERM as fatal.
// Cap the walk with GIT_CEILING_DIRECTORIES=:<parent-of-launch-dir> in the
// in-sandbox env instead of widening these extras — see the "Launch CWD"
// section of docs/sandbox-exec-testing.md and
// TestSandboxExecSessionWorkDir_GitConfigGlobalUsable.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// getcwdAncestorExtras returns a test-harness-only SBPL block granting
// file-read* / file-test-existence / file-read-metadata on every ancestor
// NODE of launchDir (its parent up to "/"), as (literal ...) rules. See the
// file header for the masking analysis. NOT added to the production
// profile.
func getcwdAncestorExtras(launchDir string) string {
	var b strings.Builder
	b.WriteString("\n;; --- test-harness getcwd ancestor allows (not in production profile) ---\n")
	b.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
	cur := filepath.Dir(filepath.Clean(launchDir))
	for {
		b.WriteString("  (literal " + sbplQuoteForTest(cur) + ")\n")
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	b.WriteString(")\n")
	return b.String()
}

// writeAugmentedPositiveProfileWithLaunchDir is
// writeAugmentedPositiveProfile plus the getcwd ancestor extras for
// launchDir. Use it for positive tests whose in-sandbox binary hard-fails
// on an unresolvable CWD (node, git); pair with cmd.Dir = launchDir on the
// sandbox-exec invocation.
func writeAugmentedPositiveProfileWithLaunchDir(t *testing.T, p preparedProfile, launchDir string) string {
	t.Helper()
	augmented := augmentProfileForTest(p.content) + getcwdAncestorExtras(launchDir)
	testProfilePath := p.path + ".integ-test"
	if err := os.WriteFile(testProfilePath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write test profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(testProfilePath) })
	return testProfilePath
}

// withMutatedProfileAndLaunchDir is withMutatedProfile plus the getcwd
// ancestor extras for launchDir. Use it for negative tests whose in-sandbox
// binary hard-fails on an unresolvable CWD (node, git) AND whose mutation
// leaves the launch dir's own grant intact (e.g. the playwright iokit /
// signal mutations): the extras keep CWD resolution working so the failure
// the test asserts is the mutated rule's, not a getcwd bootstrap death.
//
// Do NOT use this for mutations that disable the launch dir's own grant
// (e.g. sessionDir strip negatives) — there the CWD becomes unresolvable by
// design and the in-sandbox binary must be bash (tolerant).
func withMutatedProfileAndLaunchDir(t *testing.T, m *container.Manager, launchDir string, mutate func(string) string) string {
	t.Helper()

	prepared, _ := preparePositiveProfile(t, m)

	mutated := mutate(prepared.content)
	if mutated == prepared.content {
		t.Fatalf("withMutatedProfileAndLaunchDir: mutate returned identical content — the substitution did not match anything in the profile.\nProfile:\n%s",
			prepared.content)
	}

	augmented := augmentProfileForTest(mutated) + getcwdAncestorExtras(launchDir)
	mutatedPath := prepared.path + ".integ-mutated"
	if err := os.WriteFile(mutatedPath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write mutated profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(mutatedPath) })
	return mutatedPath
}
