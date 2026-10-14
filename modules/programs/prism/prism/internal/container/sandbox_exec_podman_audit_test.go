package container

// sandbox_exec_podman_audit_test.go — the legacy podman-proxy audit log
// must sit outside every write-granted path of the generated SBPL profile.
//
// The audit log is the record used to reconstruct what a session asked the
// container runtime to do. The agent is the subject of that record, so the
// agent must have no write path to it. On Darwin the only write paths the
// agent has are the (allow ... file-write* ...) clauses of the profile
// generateProfile renders, so this file parses those clauses and asserts
// the audit path is covered by none of them.
//
// The paired control asserts the same parser flags the work-dir location
// (<sessionDir>/podman-proxy.log). Without it, a parser that found no
// write grants at all would pass the positive assertion on its own.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sbplPathRule is one path covered by one clause of the profile.
type sbplPathRule struct {
	form   string // "subpath" or "literal"
	path   string
	clause string
}

// covers reports whether the rule grants its clause's operations on path.
// A (subpath P) rule covers P and everything below it. A (literal P) rule
// covers P alone.
func (r sbplPathRule) covers(path string) bool {
	if r.path == path {
		return true
	}
	return r.form == "subpath" && strings.HasPrefix(path, r.path+string(filepath.Separator))
}

// sbplPathRuleRe matches one (subpath "…") or (literal "…") term. The path
// group tolerates the two escapes quoteSBPL emits — \" and \\.
var sbplPathRuleRe = regexp.MustCompile(`\((subpath|literal)\s+"((?:[^"\\]|\\.)*)"\)`)

// topLevelSBPLClauses splits a profile into its top-level parenthesised
// clauses by paren depth. It assumes no path in the profile contains a
// parenthesis, which holds for every path the tests in this file build.
func topLevelSBPLClauses(profile string) []string {
	var clauses []string
	depth, start := 0, -1
	for i := 0; i < len(profile); i++ {
		switch profile[i] {
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth == 0 && start >= 0 {
				clauses = append(clauses, profile[start:i+1])
				start = -1
			}
		}
	}
	return clauses
}

// writeGrantedSBPLPaths returns every path the profile grants a write
// operation on. A clause qualifies when it is an `allow` clause whose
// operation list names any file-write operation (file-write*,
// file-write-data, …). `deny` clauses are skipped: a deny narrows a grant,
// it never creates one.
func writeGrantedSBPLPaths(t *testing.T, profile string) []sbplPathRule {
	t.Helper()
	var rules []sbplPathRule
	for _, clause := range topLevelSBPLClauses(profile) {
		if !strings.HasPrefix(clause, "(allow ") {
			continue
		}
		// The operation list runs from the verb to the first path term.
		ops := clause
		if idx := strings.IndexByte(clause, '\n'); idx >= 0 {
			ops = clause[:idx]
		}
		if !strings.Contains(ops, "file-write") {
			continue
		}
		for _, m := range sbplPathRuleRe.FindAllStringSubmatch(clause, -1) {
			path := strings.ReplaceAll(m[2], `\"`, `"`)
			path = strings.ReplaceAll(path, `\\`, `\`)
			rules = append(rules, sbplPathRule{form: m[1], path: path, clause: clause})
		}
	}
	return rules
}

// auditLocationTestHome is the synthetic HOME these tests render the
// profile against. It is deliberately NOT a t.TempDir(): a test temp dir can
// be under the per-session TMPDIR, which section 3 of the profile grants
// read-write in full, so every path derived from such a HOME would report as
// write-granted and the assertion would be meaningless. It matches the
// production shape instead (a Darwin home under /Users). No test in this
// file reads or writes the tree, so the path does not have to exist.
const auditLocationTestHome = "/Users/prism-audit-location-test"

// newAuditLocationTestManager builds a sandbox-exec Manager whose every
// session-derived path resolves under the synthetic HOME, so the rendered
// profile matches the production shape.
func newAuditLocationTestManager(t *testing.T) *Manager {
	t.Helper()
	home := auditLocationTestHome
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	runDir := filepath.Join(home, ".local", "state", "prism", "run", "repo-main")
	return newSandboxExecManagerWithInstance(Config{
		SessionName:     "repo@main",
		Worktree:        filepath.Join(home, "code", "repo", "main"),
		BareRoot:        filepath.Join(home, "code", "repo"),
		HostAPISockPath: filepath.Join(runDir, "hostapi.sock"),
	})
}

// TestGenerateProfile_PodmanProxyAuditLog_OutsideWriteGrantedSubpaths is the
// security assertion: no write-granting allow
// clause of the profile covers the audit-log path or its directory.
//
// Move the audit log back under <sessionDir> and this test fails, because
// section 6 grants (subpath <sessionDir>) read-write.
func TestGenerateProfile_PodmanProxyAuditLog_OutsideWriteGrantedSubpaths(t *testing.T) {
	m := newAuditLocationTestManager(t)
	profile := generateProfile(m)

	granted := writeGrantedSBPLPaths(t, profile)
	if len(granted) == 0 {
		t.Fatalf("parsed no write-granted paths out of the profile — the parser is broken, not the profile:\n%s", profile)
	}

	auditPath, err := PodmanProxyAuditLogPath(m.cfg.InstanceID)
	if err != nil {
		t.Fatalf("PodmanProxyAuditLogPath: %v", err)
	}
	auditDir, err := PodmanProxyAuditDirPath(m.cfg.InstanceID)
	if err != nil {
		t.Fatalf("PodmanProxyAuditDirPath: %v", err)
	}

	// The file and its directory are both checked: write access to the
	// directory lets the agent unlink or replace the log wholesale.
	for _, target := range []string{auditPath, auditDir} {
		for _, rule := range granted {
			if rule.covers(target) {
				t.Errorf("audit path %s is inside write-granted (%s %q) — the agent can edit its own audit trail.\nclause:\n%s",
					target, rule.form, rule.path, rule.clause)
			}
		}
	}

	// Defence in depth: no clause of any kind names the audit tree, so no
	// future edit to an operation list can widen a grant onto it.
	if strings.Contains(profile, auditDir) {
		t.Errorf("profile names the audit dir %s; the audit tree must appear in no clause at all:\n%s", auditDir, profile)
	}
}

// TestWriteGrantedSBPLPaths_FlagsWorkDirAuditLocation is the control for the
// test above. It asserts the same parser and the same containment check flag
// the work-dir location, which section 6 grants read-write. If this test
// fails, the positive assertion above proves nothing.
func TestWriteGrantedSBPLPaths_FlagsWorkDirAuditLocation(t *testing.T) {
	m := newAuditLocationTestManager(t)
	profile := generateProfile(m)

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	workDirAuditPath := filepath.Join(sessionDir, podmanProxyAuditLogFileName)

	for _, rule := range writeGrantedSBPLPaths(t, profile) {
		if rule.covers(workDirAuditPath) {
			return // the check fires where it must
		}
	}
	t.Errorf("no write-granted path covers %s, so the audit-location assertion is vacuous.\nprofile:\n%s",
		workDirAuditPath, profile)
}
