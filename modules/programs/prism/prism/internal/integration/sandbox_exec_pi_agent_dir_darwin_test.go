//go:build darwin

package integration_test

// sandbox_exec_pi_agent_dir_darwin_test.go — the pi agent dir rules of the
// sandbox-exec profile: section 6a (a read of ~/.pi/agent and a write of an
// exact list), section 6c (a read and a write of the shared lock dir and of
// the Notion store dir), section 6d (a read and a write of the per-session
// agent dir), section 21f (no create or unlink of auth.json and of the
// credential files), and section 21h (the per-session agent dir node cannot
// be replaced). See internal/container/pi_agent_dir.go,
// internal/container/pi_shared_state.go,
// internal/container/pi_auth_session.go, #3284, #3289, #3290, and #3295.
//
// The positive tests run, under the production profile, the file
// operations that pi and its extensions do during a normal session, and the
// operations that a worker uses to plant code for a later session. The
// negative tests mutate the profile and show that each positive test can
// fail, per docs/sandbox-exec-testing.md.
//
// The tests set HOME to a test-owned dir under the real home (useTestHome),
// so they never touch the real ~/.pi/agent.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// piAgentDirEnv holds the paths of one test: the agent dir, the sessions
// dir of the worktree, the sessions dir of another worktree, the shared
// lock dir, the Notion store dir, and the per-session agent dir, which is
// PI_CODING_AGENT_DIR of the session.
type piAgentDirEnv struct {
	m          *container.Manager
	agentDir   string
	ownSess    string
	otherDir   string
	lockDir    string
	notionDir  string
	sessionDir string
}

func newPIAgentDirEnv(t *testing.T) piAgentDirEnv {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	home := useTestHome(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))

	agentDir := filepath.Join(home, ".pi", "agent")
	otherDir := filepath.Join(agentDir, "sessions", "--other-worktree--")
	for _, d := range []string{filepath.Join(agentDir, "bin"), filepath.Join(agentDir, "themes"), otherDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for name, content := range map[string]string{
		"settings.json": `{"extensions":[]}`,
		"trust.json":    `{}`,
		"auth.json":     `{"anthropic":{}}`,
	} {
		if err := os.WriteFile(filepath.Join(agentDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(otherDir, "a.jsonl"), []byte(`{"type":"session","id":"a","cwd":"/other"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write other session: %v", err)
	}

	// prism agent-run makes the two shared dirs before Prepare
	// (EnsurePISharedStateDirs).
	lockDir, notionDir := container.PISharedStateDirs(home)
	for _, d := range []string{lockDir, notionDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// prism agent-run writes the per-session auth.json before Prepare
	// (PreparePIAuthSession). Prepare links the host entries into the dir.
	instanceID := "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-")
	sessionDir, err := container.PreparePIAuthSession(instanceID, filepath.Join(agentDir, "auth.json"))
	if err != nil {
		t.Fatalf("PreparePIAuthSession: %v", err)
	}

	bareRoot := filepath.Join(home, "code", "repo")
	worktree := filepath.Join(bareRoot, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	m := container.New(container.Config{
		SessionName:      "integ-pi-agent-dir",
		InstanceID:       instanceID,
		Worktree:         worktree,
		BareRoot:         bareRoot,
		Harness:          "pi",
		GitUserName:      "test-user",
		GitUserEmail:     "test@example.com",
		PILockHostDir:    lockDir,
		PINotionHostDir:  notionDir,
		PIAuthSessionDir: sessionDir,
	})
	return piAgentDirEnv{
		m:          m,
		agentDir:   agentDir,
		ownSess:    filepath.Join(agentDir, "sessions", encodeCWDForTest(worktree)),
		otherDir:   otherDir,
		lockDir:    lockDir,
		notionDir:  notionDir,
		sessionDir: sessionDir,
	}
}

// encodeCWDForTest mirrors the pi session-dir name formula.
func encodeCWDForTest(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	return "--" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped) + "--"
}

func (e piAgentDirEnv) run(t *testing.T, profilePath, script string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profilePath, requireNixBash(t), "-c", script)
	cmd.Env = append(os.Environ(),
		"A="+e.agentDir,
		"S="+e.ownSess,
		"O="+e.otherDir,
		"L="+e.lockDir,
		"N="+e.notionDir,
		"P="+e.sessionDir,
		"E="+filepath.Base(e.ownSess),
		"PATH="+requireNixCoreutilsDir(t))
	return cmd.CombinedOutput()
}

// piWritesScript does each write that pi 1.4 and the extensions do during a
// normal session, with PI_CODING_AGENT_DIR set to the per-session agent dir
// $P: in $P, through the symlinks in $P to the host agent dir, in the
// shared lock dir, and in the Notion store dir. Each step has its own exit
// code.
const piWritesScript = `cat "$P/settings.json" >/dev/null || exit 10
for l in auth.json.lock settings.json.lock trust.json.lock models-store.json.lock; do
  mkdir "$P/$l" || exit 11
  touch "$P/$l" || exit 12
  rmdir "$P/$l" || exit 13
done
printf refreshed > "$P/auth.json" || exit 20
chmod 600 "$P/auth.json" || exit 21
printf atlassian > "$P/atlassian-mcp-oauth.json" || exit 22
mkdir "$L/anthropic-oauth-refresh.lock" || exit 30
printf owner > "$L/anthropic-oauth-refresh.lock/owner" || exit 31
touch "$L/anthropic-oauth-refresh.lock" || exit 32
rm -rf "$L/anthropic-oauth-refresh.lock" || exit 33
printf notion > "$N/.notion-mcp.tmp.1.a.b" || exit 34
mv "$N/.notion-mcp.tmp.1.a.b" "$N/notion-mcp-oauth.json" || exit 35
mkdir "$N/notion-mcp-oauth.json.lock" || exit 36
printf owner > "$N/notion-mcp-oauth.json.lock/owner" || exit 37
rm -rf "$N/notion-mcp-oauth.json.lock" || exit 38
printf 'h\n' > "$P/sessions/$E/s.jsonl" || exit 40
printf 'm\n' >> "$P/sessions/$E/s.jsonl" || exit 41
exit 0`

// TestSandboxExecPIAgentDir_PiWritesWork: under the production profile each
// write of the write list works. auth.json reaches the per-session file,
// not the host file (#3295). The other writes reach the host. Prepare made
// the sessions dir of the worktree, the credential files, and the links.
func TestSandboxExecPIAgentDir_PiWritesWork(t *testing.T) {
	e := newPIAgentDirEnv(t)
	prepared, _ := preparePositiveProfile(t, e.m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	if fi, err := os.Stat(e.ownSess); err != nil || !fi.IsDir() {
		t.Fatalf("Prepare did not make the sessions dir %s: %v", e.ownSess, err)
	}
	if out, err := e.run(t, profilePath, piWritesScript); err != nil {
		t.Fatalf("a pi write failed under the production profile (see the exit codes in piWritesScript).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	for path, want := range map[string]string{
		filepath.Join(e.sessionDir, "auth.json"):              "refreshed",
		filepath.Join(e.agentDir, "auth.json"):                `{"anthropic":{}}`,
		filepath.Join(e.agentDir, "atlassian-mcp-oauth.json"): "atlassian",
		filepath.Join(e.notionDir, "notion-mcp-oauth.json"):   "notion",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("host %s = (%q, %v), want %q", path, got, err, want)
		}
	}
	if got := treeOf(t, e.lockDir); len(got) != 0 {
		t.Errorf("host lock dir = %v, want empty after the remove", got)
	}
	if got, err := os.ReadFile(filepath.Join(e.ownSess, "s.jsonl")); err != nil || string(got) != "h\nm\n" {
		t.Errorf("host session file = (%q, %v), want %q", got, err, "h\nm\n")
	}
}

// piPlantScripts are the operations that a worker uses to change what a
// later session loads or runs. Each must fail.
var piPlantScripts = []string{
	`printf '{"extensions":["/tmp/x.ts"]}' > "$A/settings.json"`,
	`rm "$A/settings.json"`,
	`mv "$A/settings.json" "$A/s2"`,
	`touch "$A/SYSTEM.md"`,
	`touch "$A/mcp.json"`,
	`mkdir "$A/prompts"`,
	`touch "$A/bin/git"`,
	`touch "$A/themes/x.json"`,
	`printf '{}' > "$A/trust.json"`,
	`ln -s /tmp/x "$A/extensions2"`,
	// The host auth.json (#3295): pi runs a "!" value in it.
	`printf '{"openai":{"type":"api_key","key":"!touch /tmp/x"}}' > "$A/auth.json"`,
	`printf x >> "$A/auth.json"`,
	`rm "$A/auth.json"`,
	`mv "$A/auth.json" "$A/a2"`,
	`mkdir "$A/auth.json.lock"`,
	// Through the symlinks in the per-session agent dir.
	`printf x > "$P/settings.json"`,
	`touch "$P/bin/git"`,
	`printf x >> "$P/sessions/--other-worktree--/a.jsonl"`,
	`printf x >> "$O/a.jsonl"`,
	`touch "$O/new.jsonl"`,
	`mkdir "$A/sessions/--new-worktree--"`,
	`mkdir "$A/auth.json.lock.x"`,
	`printf x > "$A/notion-mcp-oauth.json"`,
	`mkdir "$A/notion-mcp-oauth.json.lock"`,
	`touch "$A/.notion-mcp.tmp.1"`,
	// The shared dir nodes themselves (section 21g). The target of each
	// rename is in the other shared dir, where a create is allowed, so
	// only the deny on the node stops it.
	`rmdir "$L"`,
	`mv "$N" "$L/moved"`,
	`chmod 000 "$L"`,
	// The per-session agent dir node (section 21h).
	`chmod 000 "$P"`,
	`mv "$P" "$L/moved"`,
}

// TestSandboxExecPIAgentDir_PlantsDenied: under the production profile each
// plant fails with "Operation not permitted", and the host agent dir does
// not change.
func TestSandboxExecPIAgentDir_PlantsDenied(t *testing.T) {
	e := newPIAgentDirEnv(t)
	prepared, _ := preparePositiveProfile(t, e.m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)
	before := treeOf(t, e.agentDir)

	for _, script := range piPlantScripts {
		out, err := e.run(t, profilePath, script)
		if err == nil {
			t.Errorf("%q succeeded under the production profile.\nOutput: %s\nProfile: %s", script, out, profilePath)
			continue
		}
		if !strings.Contains(string(out), "Operation not permitted") {
			t.Errorf("%q failed for a reason other than the sandbox: %v\nOutput: %s", script, err, out)
		}
	}
	if after := treeOf(t, e.agentDir); !slices.Equal(after, before) {
		t.Errorf("host agent dir changed:\nbefore: %v\nafter:  %v", before, after)
	}
	for _, d := range []string{e.lockDir, e.notionDir, e.sessionDir} {
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("dir %s = (%v, %v), want the dir unchanged with mode 0700", d, fi, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(e.agentDir, "auth.json")); err != nil || string(got) != `{"anthropic":{}}` {
		t.Errorf("host auth.json = (%q, %v), want it unchanged", got, err)
	}
}

// TestSandboxExecPIAgentDir_PlantsWorkWithOldGrant is the paired negative
// test of PlantsDenied: with the old read-write grant of the whole dir, and
// without section 21f, the plants work. So PlantsDenied can fail.
func TestSandboxExecPIAgentDir_PlantsWorkWithOldGrant(t *testing.T) {
	e := newPIAgentDirEnv(t)
	writeClause := piAgentWriteClause(e)
	oldGrant := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(e.agentDir) + "))\n"
	profilePath := withMutatedProfile(t, e.m, func(p string) string {
		p = writeClause.ReplaceAllString(p, oldGrant)
		return piAgentCredentialDeny(e).ReplaceAllString(p, "")
	})

	for _, script := range []string{`touch "$A/SYSTEM.md"`, `touch "$A/bin/git"`, `printf x >> "$O/a.jsonl"`, `rm "$A/auth.json"`} {
		if out, err := e.run(t, profilePath, script); err != nil {
			t.Errorf("with the old grant %q still failed, so PlantsDenied proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
				script, err, out, profilePath)
		}
	}
	if _, err := os.Stat(filepath.Join(e.agentDir, "SYSTEM.md")); err != nil {
		t.Errorf("with the old grant SYSTEM.md did not reach the host: %v", err)
	}
}

// TestSandboxExecPIAgentDir_PiWritesDeniedWithoutSessionClause is the
// paired negative test of PiWritesWork: without the section-6d clause, the
// lock dir mkdir of pi in the per-session agent dir fails.
func TestSandboxExecPIAgentDir_PiWritesDeniedWithoutSessionClause(t *testing.T) {
	e := newPIAgentDirEnv(t)
	clause := regexp.MustCompile(regexp.QuoteMeta("(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " +
		sbplQuoteForTest(e.sessionDir) + "))\n"))
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return clause.ReplaceAllString(p, "") })

	out, err := e.run(t, profilePath, piWritesScript)
	if err == nil {
		t.Fatalf("pi writes worked without the section-6d clause, so PiWritesWork proves nothing.\nOutput: %s\nProfile: %s", out, profilePath)
	}
	if code := exitCode(err); code != 10 && code != 11 {
		t.Errorf("exit code = %d, want 10 or 11 (the first read or lock-dir mkdir in the per-session dir)\nOutput: %s", code, out)
	}
}

// TestSandboxExecPIAgentDir_PiWritesDeniedWithoutWriteClause is the paired
// negative test of the host half of PiWritesWork: without the section-6a
// write clause, the in-place write of the Atlassian file fails.
func TestSandboxExecPIAgentDir_PiWritesDeniedWithoutWriteClause(t *testing.T) {
	e := newPIAgentDirEnv(t)
	writeClause := piAgentWriteClause(e)
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return writeClause.ReplaceAllString(p, "") })

	out, err := e.run(t, profilePath, piWritesScript)
	if err == nil {
		t.Fatalf("pi writes worked without the section-6a write clause, so PiWritesWork proves nothing.\nOutput: %s\nProfile: %s", out, profilePath)
	}
	if code := exitCode(err); code != 22 {
		t.Errorf("exit code = %d, want 22 (the Atlassian file write)\nOutput: %s", code, out)
	}
}

// TestSandboxExecPIAgentDir_HostAuthWritableWithOldGrant is the paired
// negative test of the host auth.json plants in PlantsDenied: with the
// write grant of #3284, a session writes a "!" value into the host
// auth.json. So PlantsDenied can fail for it.
func TestSandboxExecPIAgentDir_HostAuthWritableWithOldGrant(t *testing.T) {
	e := newPIAgentDirEnv(t)
	writeClause := piAgentWriteClause(e)
	oldGrant := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (literal " +
		sbplQuoteForTest(filepath.Join(e.agentDir, "auth.json")) + "))\n"
	profilePath := withMutatedProfile(t, e.m, func(p string) string {
		return writeClause.ReplaceAllStringFunc(p, func(c string) string { return c + oldGrant })
	})

	if out, err := e.run(t, profilePath, `printf '{"openai":{"type":"api_key","key":"!touch /tmp/x"}}' > "$A/auth.json"`); err != nil {
		t.Fatalf("with the old grant the write of the host auth.json still failed, so PlantsDenied proves nothing for it.\nExit: %v\nOutput: %s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(e.agentDir, "auth.json")); !strings.Contains(string(got), "!touch") {
		t.Errorf("host auth.json = %q, want the planted value", got)
	}
}

// TestSandboxExecPIAgentDir_SessionDirReplaceWorksWithoutDeny is the paired
// negative test of the section-21h part of PlantsDenied: without the deny,
// the session can change the mode of the per-session agent dir node.
func TestSandboxExecPIAgentDir_SessionDirReplaceWorksWithoutDeny(t *testing.T) {
	e := newPIAgentDirEnv(t)
	deny := regexp.MustCompile(regexp.QuoteMeta("(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n  (literal " +
		sbplQuoteForTest(e.sessionDir) + "))\n"))
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return deny.ReplaceAllString(p, "") })
	t.Cleanup(func() { _ = os.Chmod(e.sessionDir, 0o700) })

	if out, err := e.run(t, profilePath, `chmod 000 "$P"`); err != nil {
		t.Fatalf("without section 21h the chmod of the per-session agent dir still failed, so PlantsDenied proves nothing for it.\nExit: %v\nOutput: %s", err, out)
	}
	if fi, err := os.Lstat(e.sessionDir); err != nil || fi.Mode().Perm() != 0 {
		t.Errorf("per-session agent dir = (%v, %v), want mode 000", fi, err)
	}
}

// TestSandboxExecPIAgentDir_SharedStateWritesDeniedWithoutClause is the
// paired negative test of the shared-dir half of PiWritesWork: without the
// section-6c clause, the mkdir of the refresh lock dir fails.
func TestSandboxExecPIAgentDir_SharedStateWritesDeniedWithoutClause(t *testing.T) {
	e := newPIAgentDirEnv(t)
	clause := regexp.MustCompile(regexp.QuoteMeta("(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " +
		sbplQuoteForTest(e.lockDir) + ")\n  (subpath " + sbplQuoteForTest(e.notionDir) + "))\n"))
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return clause.ReplaceAllString(p, "") })

	out, err := e.run(t, profilePath, piWritesScript)
	if err == nil {
		t.Fatalf("pi writes worked without the section-6c clause, so PiWritesWork proves nothing.\nOutput: %s\nProfile: %s", out, profilePath)
	}
	if code := exitCode(err); code != 30 {
		t.Errorf("exit code = %d, want 30 (the refresh lock dir mkdir)\nOutput: %s", code, out)
	}
}

// TestSandboxExecPIAgentDir_SharedDirReplaceWorksWithoutDeny is the paired
// negative test of the section-21g part of PlantsDenied: without the deny,
// the session can remove a shared dir node.
func TestSandboxExecPIAgentDir_SharedDirReplaceWorksWithoutDeny(t *testing.T) {
	e := newPIAgentDirEnv(t)
	deny := regexp.MustCompile(regexp.QuoteMeta("(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n  (literal " +
		sbplQuoteForTest(e.lockDir) + ")\n  (literal " + sbplQuoteForTest(e.notionDir) + "))\n"))
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return deny.ReplaceAllString(p, "") })

	if out, err := e.run(t, profilePath, `rmdir "$L"`); err != nil {
		t.Fatalf("without section 21g the rmdir of the lock dir still failed, so PlantsDenied proves nothing for it.\nExit: %v\nOutput: %s", err, out)
	}
	if _, err := os.Lstat(e.lockDir); !os.IsNotExist(err) {
		t.Errorf("lock dir on the host after the rmdir: %v", err)
	}
}

// TestSandboxExecPIAgentDir_CredentialReplaceWorksWithoutDeny is the paired
// negative test of the section-21f part of PlantsDenied: without the deny,
// the session can remove the Atlassian file and put a symlink at its path.
func TestSandboxExecPIAgentDir_CredentialReplaceWorksWithoutDeny(t *testing.T) {
	e := newPIAgentDirEnv(t)
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return piAgentCredentialDeny(e).ReplaceAllString(p, "") })

	name := "atlassian-mcp-oauth.json"
	if out, err := e.run(t, profilePath, `rm "$A/`+name+`" && ln -s /tmp/x "$A/`+name+`"`); err != nil {
		t.Fatalf("without section 21f the replace of %s still failed, so PlantsDenied proves nothing for it.\nExit: %v\nOutput: %s", name, err, out)
	}
	if fi, err := os.Lstat(filepath.Join(e.agentDir, name)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s on the host = (%v, %v), want a symlink", name, fi, err)
	}
}

// piAgentWriteClause matches the section-6a write clause of e.
func piAgentWriteClause(e piAgentDirEnv) *regexp.Regexp {
	return regexp.MustCompile(`(?s)\(allow file-read\* file-write\* file-test-existence file-read-metadata\n  \(literal ` +
		regexp.QuoteMeta(sbplQuoteForTest(filepath.Join(e.agentDir, "atlassian-mcp-oauth.json"))) + `\).*?\(subpath ` +
		regexp.QuoteMeta(sbplQuoteForTest(e.ownSess)) + `\)\)\n`)
}

// piAgentCredentialDeny matches the section-21f deny of e.
func piAgentCredentialDeny(e piAgentDirEnv) *regexp.Regexp {
	return regexp.MustCompile(`\(deny file-write-unlink file-write-create\n  \(literal ` +
		regexp.QuoteMeta(sbplQuoteForTest(filepath.Join(e.agentDir, "auth.json"))) + `\)\n  \(literal ` +
		regexp.QuoteMeta(sbplQuoteForTest(filepath.Join(e.agentDir, "atlassian-mcp-oauth.json"))) + `\)\)\n`)
}

// treeOf lists every path under dir, relative to dir.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}
