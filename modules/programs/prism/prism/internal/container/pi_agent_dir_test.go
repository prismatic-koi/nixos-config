package container

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// piAgentDirFixture makes a host agent dir with the entries that a real one
// has, and returns the host dir and a worktree path.
func piAgentDirFixture(t *testing.T) (hostDir, worktree string) {
	t.Helper()
	hostDir = filepath.Join(t.TempDir(), ".pi", "agent")
	for _, d := range []string{"themes", "bin", "skills", "extensions", "sessions/--other-worktree--", "auth.json.lock"} {
		if err := os.MkdirAll(filepath.Join(hostDir, d), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for _, f := range []string{"settings.json", "trust.json", "models-store.json", "notion-mcp-oauth.json", "notion-mcp-client.json", "auth.json"} {
		if err := os.WriteFile(filepath.Join(hostDir, f), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return hostDir, "/home/user/code/repo/feature"
}

// bindsAt returns the bwrap flag of each mount whose destination is under
// sandboxDir, keyed by the destination relative to sandboxDir.
func bindsAt(args []string, sandboxDir string) map[string]string {
	got := map[string]string{}
	for i := 0; i+2 < len(args); i++ {
		if !strings.Contains(args[i], "bind") {
			continue
		}
		if rel, ok := strings.CutPrefix(args[i+2], sandboxDir+"/"); ok {
			got[rel] = args[i]
		}
	}
	return got
}

// The agent dir in a bwrap sandbox is a private dir. Each host entry is
// read-only. Only the credential files and the sessions dir of the worktree
// are read-write. A host lock dir and the sessions dirs of other worktrees
// are not in the sandbox. A Notion store file that is still in the agent
// dir, before the move to its own dir, is read-only like any other entry.
func TestAppendPIAgentDirBwrapMounts_ReadOnlyExceptWriteList(t *testing.T) {
	hostDir, worktree := piAgentDirFixture(t)
	const sandboxDir = "/run/prism/pi-agent"

	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, sandboxDir, worktree)
	if err != nil {
		t.Fatalf("appendPIAgentDirBwrapMounts: %v", err)
	}

	if !hasPair(args, "--dir", sandboxDir) {
		t.Errorf("expected --dir %s; args = %v", sandboxDir, args)
	}
	for i := 0; i+2 < len(args); i++ {
		if strings.Contains(args[i], "bind") && args[i+2] == sandboxDir {
			t.Errorf("the sandbox agent dir must not be a bind (%s %s %s)", args[i], args[i+1], args[i+2])
		}
	}

	enc := encodePiCWD(worktree)
	want := map[string]string{
		"themes":                   "--ro-bind-try",
		"bin":                      "--ro-bind-try",
		"skills":                   "--ro-bind-try",
		"extensions":               "--ro-bind-try",
		"settings.json":            "--ro-bind-try",
		"trust.json":               "--ro-bind-try",
		"models-store.json":        "--ro-bind-try",
		"notion-mcp-oauth.json":    "--ro-bind-try",
		"notion-mcp-client.json":   "--ro-bind-try",
		"auth.json":                "--bind",
		"atlassian-mcp-oauth.json": "--bind",
		"sessions/" + enc:          "--bind",
	}
	if got := bindsAt(args, sandboxDir); !mapsEqual(got, want) {
		t.Errorf("mounts in the sandbox agent dir:\ngot:  %v\nwant: %v", got, want)
	}

	// preparePIAgentDir made the host paths that the binds need.
	for _, p := range []string{
		filepath.Join(hostDir, "atlassian-mcp-oauth.json"),
		filepath.Join(hostDir, "sessions", enc),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s on the host: %v", p, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(hostDir, "atlassian-mcp-oauth.json")); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("atlassian-mcp-oauth.json mode = %v, want 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(hostDir, "auth.json")); string(b) != "{}" {
		t.Errorf("preparePIAgentDir changed an existing auth.json: %q", b)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A credential file that is a symlink on the host is not bound read-write:
// the bind would follow the symlink to a file that prism did not choose.
func TestAppendPIAgentDirBwrapMounts_SymlinkCredentialNotBound(t *testing.T) {
	hostDir, worktree := piAgentDirFixture(t)
	if err := os.Remove(filepath.Join(hostDir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(hostDir, "auth.json")); err != nil {
		t.Fatal(err)
	}

	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, "/run/prism/pi-agent", worktree)
	if err != nil {
		t.Fatalf("appendPIAgentDirBwrapMounts: %v", err)
	}
	if hasTriple(args, "--bind", filepath.Join(hostDir, "auth.json"), "/run/prism/pi-agent/auth.json") {
		t.Errorf("a symlink auth.json is bound read-write: %v", args)
	}
}

func TestAppendPIAgentDirBwrapMounts_UnreadableHostDirIsAnError(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := appendPIAgentDirBwrapMounts(nil, notADir, "/run/prism/pi-agent", "/w"); err == nil {
		t.Fatal("appendPIAgentDirBwrapMounts = nil error for a host dir that is not a dir")
	}
}

// pi in the sandbox writes only through these destinations. A write of any
// other destination in the agent dir goes to a read-only bind or to the
// private dir.
func TestAppendPIAgentDirBwrapMounts_ReadWriteDestinations(t *testing.T) {
	hostDir, worktree := piAgentDirFixture(t)
	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, "/run/prism/pi-agent", worktree)
	if err != nil {
		t.Fatalf("appendPIAgentDirBwrapMounts: %v", err)
	}
	var rw []string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" {
			rw = append(rw, args[i+2])
		}
	}
	slices.Sort(rw)
	want := []string{
		"/run/prism/pi-agent/atlassian-mcp-oauth.json",
		"/run/prism/pi-agent/auth.json",
		"/run/prism/pi-agent/sessions/" + encodePiCWD(worktree),
	}
	if !slices.Equal(rw, want) {
		t.Errorf("read-write binds = %v, want %v", rw, want)
	}
}

// The sandbox-exec profile allows a read of the agent dir and a write of
// the exact list. A write of the whole dir must not be in the profile.
func TestGenerateProfile_PIAgentDirWriteList(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	worktree := filepath.Join(fakeHome, "code", "repo", "feature")
	m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: worktree})
	profile := generateProfile(m)
	agentDir := filepath.Join(fakeHome, ".pi", "agent")

	readClause := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + quoteSBPL(agentDir) + "))\n"
	if !strings.Contains(profile, readClause) {
		t.Errorf("profile has no read clause for the agent dir:\n%s", profile)
	}

	wantRules := []string{
		"(literal " + quoteSBPL(filepath.Join(agentDir, "auth.json")) + ")",
		"(literal " + quoteSBPL(filepath.Join(agentDir, "atlassian-mcp-oauth.json")) + ")",
		"(literal " + quoteSBPL(filepath.Join(agentDir, "auth.json.lock")) + ")",
		"(literal " + quoteSBPL(filepath.Join(agentDir, "settings.json.lock")) + ")",
		"(literal " + quoteSBPL(filepath.Join(agentDir, "trust.json.lock")) + ")",
		"(literal " + quoteSBPL(filepath.Join(agentDir, "models-store.json.lock")) + ")",
		"(subpath " + quoteSBPL(filepath.Join(agentDir, "sessions", encodePiCWD(worktree))) + ")",
	}
	writeClause := "(allow file-read* file-write* file-test-existence file-read-metadata\n  " + strings.Join(wantRules, "\n  ") + ")\n"
	if !strings.Contains(profile, writeClause) {
		t.Errorf("profile has no exact write clause for the agent dir.\nwant:\n%s\nprofile:\n%s", writeClause, profile)
	}

	// No clause grants a write of the agent dir, of sessions/, or of a
	// subpath other than the two above.
	writeSubpath := regexp.MustCompile(`\(subpath "` + regexp.QuoteMeta(agentDir) + `[^"]*"\)`)
	for _, match := range writeSubpath.FindAllStringIndex(profile, -1) {
		rule := profile[match[0]:match[1]]
		clause := profile[strings.LastIndex(profile[:match[0]], "(allow"):match[0]]
		if !strings.Contains(clause, "file-write") {
			continue
		}
		if !slices.Contains(wantRules, rule) {
			t.Errorf("unexpected write grant %s", rule)
		}
	}
}

// Section 21f must follow every allow, or SBPL lets a later allow outrank it.
func TestGenerateProfile_PIAgentCredentialDenyFollowsEveryAllow(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w")})
	profile := generateProfile(m)

	deny := piAgentDirSBPLDeny(filepath.Join(fakeHome, ".pi", "agent"))
	denyIdx := strings.Index(profile, deny)
	if denyIdx < 0 {
		t.Fatalf("profile has no section-21f deny:\n%s", profile)
	}
	if last := strings.LastIndex(profile, "(allow "); denyIdx < last {
		t.Errorf("section-21f deny (idx %d) precedes an allow (idx %d)", denyIdx, last)
	}
	for _, name := range piAgentCredentialFiles {
		if !strings.Contains(deny, quoteSBPL(filepath.Join(fakeHome, ".pi", "agent", name))) {
			t.Errorf("section-21f deny does not name %s", name)
		}
	}
}

func TestGenerateProfile_NoPIAgentDirRulesForNonPiSession(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	m := newSandboxExecManager(Config{SessionName: "repo@feature", Worktree: filepath.Join(fakeHome, "w")})
	if profile := generateProfile(m); strings.Contains(profile, filepath.Join(fakeHome, ".pi")) {
		t.Errorf("profile of a non-pi session names ~/.pi:\n%s", profile)
	}
}
