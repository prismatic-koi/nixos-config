package container

// checkin_privileged_repos_test.go — issue #2587.
//
// The tier-3 `/checkin` troubleshooting privilege is configured by
// ~/.config/prism/checkin-privileged-repos.json, rendered by the prism NixOS
// module and read host-side by the sidecar. Its security value rests on the
// file being unreachable from inside every sandbox: an agent that could read
// it would learn which coordinator to impersonate, and an agent that could
// write it would grant itself the privilege.
//
// Only two entries out of ~/.config/prism/ are exposed to a sandbox — the
// agents/ directory and profiles.json — and both read-only. These tests pin
// that boundary against both isolators. They fail if a future change widens
// the grant to the whole ~/.config/prism/ directory, which is the realistic
// way this file would leak in.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// checkinPrivilegeFileName is asserted against the config package's own
// constant so a rename there cannot silently orphan these tests.
const checkinPrivilegeFileName = config.CheckinPrivilegedReposFileName

// TestBwrapBuildArgs_CheckinPrivilegedReposNotBound pins the AC "the rendered
// list file is not bound into any sandbox, and is not writable from inside
// one" for the bwrap isolator.
//
// The file is planted on the fake host home, so a widened mount would find a
// real file to bind and the assertion would fire. Both the RO and RW bind
// shapes are rejected: not bound at all is the requirement, and "read-only is
// good enough" is not, because the file's contents are the privilege list.
func TestBwrapBuildArgs_CheckinPrivilegedReposNotBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	privilegeFile := filepath.Join(fakeHome, ".config", "prism", checkinPrivilegeFileName)
	if err := os.MkdirAll(filepath.Dir(privilegeFile), 0o755); err != nil {
		t.Fatalf("MkdirAll ~/.config/prism: %v", err)
	}
	if err := os.WriteFile(privilegeFile, []byte(`{"privileged_repos":["nixos-config"]}`), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", checkinPrivilegeFileName, err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, privilegeFile) {
		t.Errorf("%s must not be bound into the sandbox, found as --ro-bind: %v", checkinPrivilegeFileName, redactedArgs(args))
	}
	if hasBind(args, privilegeFile) {
		t.Errorf("%s must not be bound into the sandbox, found as --bind: %v", checkinPrivilegeFileName, redactedArgs(args))
	}
	// The path must not appear anywhere in the argv, in any shape — a
	// src!=dst remap or a future flag would still expose it.
	for _, a := range redactedArgs(args) {
		if strings.Contains(a, checkinPrivilegeFileName) {
			t.Errorf("%s appears in the bwrap argv (%q) — it must stay host-side", checkinPrivilegeFileName, a)
		}
	}
}

// TestBwrapBuildArgs_PrismConfigDirNotBoundWholesale is the structural half of
// the same guarantee: ~/.config/prism/ is never bound as a directory. The
// checkin privilege file lives beside profiles.json, so a whole-directory
// mount would expose it without ever naming it. Only the agents/ subdirectory
// and profiles.json are permitted, both read-only.
func TestBwrapBuildArgs_PrismConfigDirNotBoundWholesale(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	prismConfigDir := filepath.Join(fakeHome, ".config", "prism")
	if err := os.MkdirAll(filepath.Join(prismConfigDir, "agents"), 0o755); err != nil {
		t.Fatalf("MkdirAll ~/.config/prism/agents: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, prismConfigDir) || hasBind(args, prismConfigDir) {
		t.Errorf("~/.config/prism must never be bound as a directory — that exposes %s and every future sibling: %v",
			checkinPrivilegeFileName, redactedArgs(args))
	}

	// Enumerate what IS exposed out of ~/.config/prism/, so a widening shows
	// up here as a diff rather than as a silent leak.
	allowed := map[string]bool{
		filepath.Join(prismConfigDir, "agents"):        true,
		filepath.Join(prismConfigDir, "profiles.json"): true,
	}
	seen := 0
	for _, triple := range append(findTriples(args, "--ro-bind"), findTriples(args, "--bind")...) {
		src := triple[0]
		if !strings.HasPrefix(src, prismConfigDir+string(filepath.Separator)) {
			continue
		}
		seen++
		if !allowed[src] {
			t.Errorf("unexpected mount out of ~/.config/prism: %q — every entry there must be reviewed against the checkin privilege file (#2587)", src)
		}
	}
	// Non-vacuity guard: the fixture creates ~/.config/prism/agents, so the
	// loop above must have inspected at least one mount. Without this an
	// unrelated change to the mount list would turn the enumeration into a
	// silent no-op that passes forever.
	if seen == 0 {
		t.Fatalf("no mount was found under %q — the enumeration is vacuous; args: %v", prismConfigDir, redactedArgs(args))
	}
}

// TestGenerateProfile_CheckinPrivilegedReposNoAllow is the sandbox-exec half.
// The profile is deny-default, so the assertion is that the path is never
// named in an allow rule: an unnamed path under a deny-default profile is
// unreadable and unwritable.
func TestGenerateProfile_CheckinPrivilegedReposNoAllow(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	privilegeFile := filepath.Join(fakeHome, ".config", "prism", checkinPrivilegeFileName)
	if strings.Contains(profile, privilegeFile) {
		t.Errorf("%s must not be named anywhere in the SBPL profile; under deny-default an unnamed path is already unreachable.\nfull profile:\n%s",
			checkinPrivilegeFileName, profile)
	}

	// A subpath grant on ~/.config/prism would reach the file without naming
	// it. Only the agents/ subdirectory and profiles.json are permitted.
	prismConfigDir := filepath.Join(fakeHome, ".config", "prism")
	if strings.Contains(profile, "(subpath \""+prismConfigDir+"\")") {
		t.Errorf("~/.config/prism must never be granted as a (subpath ...) — that reaches %s: \nfull profile:\n%s",
			checkinPrivilegeFileName, profile)
	}
}
