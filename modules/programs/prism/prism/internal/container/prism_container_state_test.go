package container

// The prism-container state tree holds the audit log and the host-wide
// limit lock. The agent is the subject of the audit log and must not be
// able to edit it, so no sandbox may write the tree.

import (
	"path/filepath"
	"strings"
	"testing"
)

func prismContainerStatePaths(t *testing.T, instanceID string) []string {
	t.Helper()
	root, err := PrismContainerStateDir()
	if err != nil {
		t.Fatalf("PrismContainerStateDir: %v", err)
	}
	auditDir, err := PrismContainerAuditDirPath(instanceID)
	if err != nil {
		t.Fatalf("PrismContainerAuditDirPath: %v", err)
	}
	auditLog, err := PrismContainerAuditLogPath(instanceID)
	if err != nil {
		t.Fatalf("PrismContainerAuditLogPath: %v", err)
	}
	lock, err := PrismContainerLockPath()
	if err != nil {
		t.Fatalf("PrismContainerLockPath: %v", err)
	}
	cidDir, err := PrismContainerCIDDirPath()
	if err != nil {
		t.Fatalf("PrismContainerCIDDirPath: %v", err)
	}
	stageDir, err := PrismContainerBuildStageDirPath()
	if err != nil {
		t.Fatalf("PrismContainerBuildStageDirPath: %v", err)
	}
	buildLockDir, err := PrismContainerBuildLockDirPath()
	if err != nil {
		t.Fatalf("PrismContainerBuildLockDirPath: %v", err)
	}
	policy, err := PrismContainerBuildPolicyPath()
	if err != nil {
		t.Fatalf("PrismContainerBuildPolicyPath: %v", err)
	}
	return []string{root, auditDir, auditLog, lock, cidDir, stageDir, buildLockDir, policy}
}

// TestGenerateProfile_PrismContainerState_OutsideWriteGrantedSubpaths: no
// write-granting clause of the sandbox-exec profile covers the tree, for a
// session with or without --containers.
func TestGenerateProfile_PrismContainerState_OutsideWriteGrantedSubpaths(t *testing.T) {
	for _, containers := range []bool{false, true} {
		m := newAuditLocationTestManager(t)
		m.cfg.ContainersEnabled = containers
		profile := generateProfile(m)
		granted := writeGrantedSBPLPaths(t, profile)
		if len(granted) == 0 {
			t.Fatalf("parsed no write-granted paths out of the profile:\n%s", profile)
		}
		paths := prismContainerStatePaths(t, m.cfg.InstanceID)
		for _, target := range paths {
			for _, rule := range granted {
				if rule.covers(target) {
					t.Errorf("containers=%v: %s is inside write-granted (%s %q)\nclause:\n%s",
						containers, target, rule.form, rule.path, rule.clause)
				}
			}
		}
		if strings.Contains(profile, paths[0]) {
			t.Errorf("containers=%v: profile names the prism-container state dir %s", containers, paths[0])
		}
	}
}

// TestBwrapBuildArgs_PrismContainerStateNotBound: no bwrap bind reaches the
// tree, so the tree does not exist inside the sandbox.
func TestBwrapBuildArgs_PrismContainerStateNotBound(t *testing.T) {
	m, home, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	args := (&bwrapIsolator{name: m.name}).BuildArgs(m)

	paths := prismContainerStatePaths(t, "0f0e0d0c-0b0a-4908-8706-050403020100")
	for _, hit := range bwrapBindsReaching(args, paths) {
		t.Errorf("bwrap bind reaches the prism-container state tree: %s", hit)
	}

	// Control: the same check flags a bind of the parent state dir.
	stateDir := filepath.Dir(paths[0])
	if len(bwrapBindsReaching(append(args, "--bind", stateDir, stateDir), paths)) == 0 {
		t.Fatalf("control: a bind of %s was not flagged, so the check is vacuous", stateDir)
	}
}

// bwrapBindsReaching returns every bind triple in args whose source or
// destination is one of paths or a parent of one.
func bwrapBindsReaching(args, paths []string) []string {
	bindFlags := map[string]bool{"--bind": true, "--bind-try": true, "--ro-bind": true, "--ro-bind-try": true, "--dev-bind": true, "--dev-bind-try": true}
	var hits []string
	for i := 0; i+2 < len(args); i++ {
		if !bindFlags[args[i]] {
			continue
		}
		for _, bound := range args[i+1 : i+3] {
			for _, target := range paths {
				if bound == target || strings.HasPrefix(target, bound+string(filepath.Separator)) {
					hits = append(hits, args[i]+" "+args[i+1]+" "+args[i+2])
				}
			}
		}
	}
	return hits
}

// TestGenerateProfile_WorktreeEntryDenyFollowsEveryAllow checks that the
// sandbox-exec profile denies unlink and create on the worktree and on every
// ancestor of it, BareRoot included, and that no allow follows the deny.
// SBPL takes the later rule, so an allow after the deny re-opens the swap.
// The Darwin integration test TestSandboxExecWorktreeEntry_CannotBeReplaced
// proves the effect.
func TestGenerateProfile_WorktreeEntryDenyFollowsEveryAllow(t *testing.T) {
	m := newAuditLocationTestManager(t)
	profile := generateProfile(m)

	denyAt := strings.Index(profile, "(deny file-write-unlink file-write-create\n")
	if denyAt < 0 {
		t.Fatalf("profile has no worktree path deny.\nprofile:\n%s", profile)
	}
	clause := profile[denyAt:]
	clause = clause[:strings.Index(clause, "))\n")+3]
	for dir := m.cfg.Worktree; ; dir = filepath.Dir(dir) {
		if !strings.Contains(clause, "(literal "+quoteSBPL(dir)+")") {
			t.Errorf("worktree path deny does not list %s:\n%s", dir, clause)
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	if !strings.Contains(clause, "(literal "+quoteSBPL(m.cfg.BareRoot)+")") {
		t.Errorf("worktree path deny does not list BareRoot %s", m.cfg.BareRoot)
	}
	if allowAt := strings.LastIndex(profile, "(allow "); allowAt > denyAt {
		t.Errorf("an allow clause (offset %d) follows the worktree path deny (offset %d); SBPL takes the later rule.\nprofile:\n%s",
			allowAt, denyAt, profile)
	}
	if !strings.Contains(profile, "(subpath "+quoteSBPL(m.cfg.BareRoot)+")") {
		t.Fatalf("fixture premise changed: the profile no longer grants (subpath BareRoot)")
	}
}
