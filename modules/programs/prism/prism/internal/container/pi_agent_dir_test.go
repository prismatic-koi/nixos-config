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

// piAuthDirFixture makes a per-session auth dir with an auth.json.
func piAuthDirFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pi-auth", "id", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// symlinkArgs returns the --symlink pairs of args, keyed by destination.
func symlinkArgs(args []string) map[string]string {
	got := map[string]string{}
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--symlink" {
			got[args[i+2]] = args[i+1]
		}
	}
	return got
}

// The agent dir in a bwrap sandbox is a private dir. Each host entry is
// read-only. Only the credential files and the sessions dir of the worktree
// are read-write. auth.json is a symlink into the per-session auth dir,
// and the host auth.json is not in the sandbox (#3295). A host lock dir and
// the sessions dirs of other worktrees are not in the sandbox. A Notion
// store file that is still in the agent dir, before the move to its own
// dir, is read-only like any other entry.
func TestAppendPIAgentDirBwrapMounts_ReadOnlyExceptWriteList(t *testing.T) {
	hostDir, worktree := piAgentDirFixture(t)
	authDir := piAuthDirFixture(t)
	const sandboxDir = "/run/prism/pi-agent"

	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, sandboxDir, worktree, authDir)
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
		"atlassian-mcp-oauth.json": "--bind",
		"sessions/" + enc:          "--bind",
	}
	if got := bindsAt(args, sandboxDir); !mapsEqual(got, want) {
		t.Errorf("mounts in the sandbox agent dir:\ngot:  %v\nwant: %v", got, want)
	}
	if !hasTriple(args, "--bind", authDir, piAuthSessionSandboxDir) {
		t.Errorf("expected --bind %s %s; args = %v", authDir, piAuthSessionSandboxDir, args)
	}
	if got := symlinkArgs(args)[sandboxDir+"/auth.json"]; got != piAuthSessionSandboxDir+"/auth.json" {
		t.Errorf("symlink %s/auth.json -> %q, want %q", sandboxDir, got, piAuthSessionSandboxDir+"/auth.json")
	}
	if slices.Contains(args, filepath.Join(hostDir, "auth.json")) {
		t.Errorf("the host auth.json is in the sandbox: %v", args)
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
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(hostDir, "atlassian-mcp-oauth.json")); err != nil {
		t.Fatal(err)
	}

	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, "/run/prism/pi-agent", worktree, "")
	if err != nil {
		t.Fatalf("appendPIAgentDirBwrapMounts: %v", err)
	}
	if hasTriple(args, "--bind", filepath.Join(hostDir, "atlassian-mcp-oauth.json"), "/run/prism/pi-agent/atlassian-mcp-oauth.json") {
		t.Errorf("a symlink credential file is bound read-write: %v", args)
	}
}

// With no per-session auth dir, the sandbox gets no auth.json from the
// host: not a bind of the host file, and not a symlink.
func TestAppendPIAgentDirBwrapMounts_NoAuthDirNoAuthFile(t *testing.T) {
	hostDir, worktree := piAgentDirFixture(t)
	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, "/run/prism/pi-agent", worktree, "")
	if err != nil {
		t.Fatalf("appendPIAgentDirBwrapMounts: %v", err)
	}
	for _, a := range args {
		if strings.HasSuffix(a, "/auth.json") || a == piAuthSessionSandboxDir {
			t.Errorf("args name %s with no per-session auth dir: %v", a, args)
		}
	}
}

func TestAppendPIAgentDirBwrapMounts_UnreadableHostDirIsAnError(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := appendPIAgentDirBwrapMounts(nil, notADir, "/run/prism/pi-agent", "/w", ""); err == nil {
		t.Fatal("appendPIAgentDirBwrapMounts = nil error for a host dir that is not a dir")
	}
}

// pi in the sandbox writes only through these destinations. A write of any
// other destination in the agent dir goes to a read-only bind or to the
// private dir.
func TestAppendPIAgentDirBwrapMounts_ReadWriteDestinations(t *testing.T) {
	hostDir, worktree := piAgentDirFixture(t)
	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, "/run/prism/pi-agent", worktree, piAuthDirFixture(t))
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
		"/run/prism/pi-agent/sessions/" + encodePiCWD(worktree),
		piAuthSessionSandboxDir,
	}
	if !slices.Equal(rw, want) {
		t.Errorf("read-write binds = %v, want %v", rw, want)
	}
}

// The sandbox-exec profile allows a read of the agent dir and a write of
// the exact list. A write of the whole dir, and of the host auth.json, must
// not be in the profile. The per-session agent dir is read-write (#3295).
func TestGenerateProfile_PIAgentDirWriteList(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	worktree := filepath.Join(fakeHome, "code", "repo", "feature")
	authDir := filepath.Join(fakeHome, ".local", "state", "prism", "pi-auth", "id", "agent")
	m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: worktree, PIAuthSessionDir: authDir})
	profile := generateProfile(m)
	agentDir := filepath.Join(fakeHome, ".pi", "agent")

	readClause := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + quoteSBPL(agentDir) + "))\n"
	if !strings.Contains(profile, readClause) {
		t.Errorf("profile has no read clause for the agent dir:\n%s", profile)
	}

	wantRules := []string{
		"(literal " + quoteSBPL(filepath.Join(agentDir, "atlassian-mcp-oauth.json")) + ")",
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
	// The host auth.json is named only in a deny clause.
	hostAuth := quoteSBPL(filepath.Join(agentDir, "auth.json"))
	for i := strings.Index(profile, hostAuth); i >= 0; {
		allow, deny := strings.LastIndex(profile[:i], "(allow"), strings.LastIndex(profile[:i], "(deny")
		if allow > deny {
			t.Errorf("an allow clause names the host auth.json: %s", profile[allow:i+len(hostAuth)])
		}
		next := strings.Index(profile[i+1:], hostAuth)
		if next < 0 {
			break
		}
		i += 1 + next
	}

	sessionClause := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + quoteSBPL(authDir) + "))\n"
	if !strings.Contains(profile, sessionClause) {
		t.Errorf("profile has no section-6d clause for the per-session agent dir.\nwant:\n%s\nprofile:\n%s", sessionClause, profile)
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
	for _, name := range append([]string{"auth.json"}, piAgentCredentialFiles...) {
		if !strings.Contains(deny, quoteSBPL(filepath.Join(fakeHome, ".pi", "agent", name))) {
			t.Errorf("section-21f deny does not name %s", name)
		}
	}
}

// Section 21h must follow every allow: section 6d grants a subpath of the
// per-session agent dir, which covers the dir node.
func TestGenerateProfile_PIAuthSessionDenyFollowsEveryAllow(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	authDir := filepath.Join(fakeHome, "state", "pi-auth", "id", "agent")
	m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w"), PIAuthSessionDir: authDir})
	profile := generateProfile(m)

	deny := piAuthSessionSBPLDeny(authDir)
	denyIdx := strings.Index(profile, deny)
	if deny == "" || denyIdx < 0 {
		t.Fatalf("profile has no section-21h deny:\n%s", profile)
	}
	if last := strings.LastIndex(profile, "(allow "); denyIdx < last {
		t.Errorf("section-21h deny (idx %d) precedes an allow (idx %d)", denyIdx, last)
	}
	if !strings.Contains(deny, "(literal "+quoteSBPL(authDir)+")") {
		t.Errorf("section-21h deny does not name the dir node: %s", deny)
	}

	m = newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w")})
	if p := generateProfile(m); strings.Contains(p, "pi-auth") {
		t.Errorf("profile with no per-session agent dir names one:\n%s", p)
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
