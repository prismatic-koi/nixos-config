//go:build darwin

package integration_test

// sandbox_exec_pi_agent_dir_darwin_test.go — the pi agent dir rules of the
// sandbox-exec profile: section 6a (a read of ~/.pi/agent and a write of an
// exact list) and section 21f (no create or unlink of the credential
// files). See internal/container/pi_agent_dir.go and #3284.
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
// dir of the worktree, and the sessions dir of another worktree.
type piAgentDirEnv struct {
	m        *container.Manager
	agentDir string
	ownSess  string
	otherDir string
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

	bareRoot := filepath.Join(home, "code", "repo")
	worktree := filepath.Join(bareRoot, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	m := container.New(container.Config{
		SessionName:  "integ-pi-agent-dir",
		InstanceID:   "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:     worktree,
		BareRoot:     bareRoot,
		Harness:      "pi",
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	})
	return piAgentDirEnv{
		m:        m,
		agentDir: agentDir,
		ownSess:  filepath.Join(agentDir, "sessions", encodeCWDForTest(worktree)),
		otherDir: otherDir,
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
		"PATH="+requireNixCoreutilsDir(t))
	return cmd.CombinedOutput()
}

// piWritesScript does each write that pi 1.4 and the extensions do during a
// normal session. Each step has its own exit code.
const piWritesScript = `cat "$A/settings.json" >/dev/null || exit 10
for l in auth.json.lock settings.json.lock trust.json.lock models-store.json.lock; do
  mkdir "$A/$l" || exit 11
  touch "$A/$l" || exit 12
  rmdir "$A/$l" || exit 13
done
printf refreshed > "$A/auth.json" || exit 20
chmod 600 "$A/auth.json" || exit 21
printf atlassian > "$A/atlassian-mcp-oauth.json" || exit 22
printf notion > "$A/.notion-mcp.tmp.1.a.b" || exit 30
mv "$A/.notion-mcp.tmp.1.a.b" "$A/notion-mcp-oauth.json" || exit 31
mkdir "$A/notion-mcp-oauth.json.lock" || exit 32
printf owner > "$A/notion-mcp-oauth.json.lock/owner" || exit 33
rm -rf "$A/notion-mcp-oauth.json.lock" || exit 34
printf 'h\n' > "$S/s.jsonl" || exit 40
printf 'm\n' >> "$S/s.jsonl" || exit 41
exit 0`

// TestSandboxExecPIAgentDir_PiWritesWork: under the production profile each
// write of the write list works and reaches the host. Prepare made the
// sessions dir of the worktree and the credential files.
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
	for name, want := range map[string]string{
		"auth.json":                "refreshed",
		"atlassian-mcp-oauth.json": "atlassian",
		"notion-mcp-oauth.json":    "notion",
	} {
		if got, err := os.ReadFile(filepath.Join(e.agentDir, name)); err != nil || string(got) != want {
			t.Errorf("host %s = (%q, %v), want %q", name, got, err, want)
		}
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
	`rm "$A/auth.json"`,
	`mv "$A/auth.json" "$A/a2"`,
	`printf x >> "$O/a.jsonl"`,
	`touch "$O/new.jsonl"`,
	`mkdir "$A/sessions/--new-worktree--"`,
	`mkdir "$A/auth.json.lock.x"`,
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

// TestSandboxExecPIAgentDir_PiWritesDeniedWithoutWriteClause is the paired
// negative test of PiWritesWork: without the section-6a write clause, the
// lock dir mkdir of pi fails.
func TestSandboxExecPIAgentDir_PiWritesDeniedWithoutWriteClause(t *testing.T) {
	e := newPIAgentDirEnv(t)
	writeClause := piAgentWriteClause(e)
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return writeClause.ReplaceAllString(p, "") })

	out, err := e.run(t, profilePath, piWritesScript)
	if err == nil {
		t.Fatalf("pi writes worked without the section-6a write clause, so PiWritesWork proves nothing.\nOutput: %s\nProfile: %s", out, profilePath)
	}
	if code := exitCode(err); code != 11 {
		t.Errorf("exit code = %d, want 11 (the first lock-dir mkdir)\nOutput: %s", code, out)
	}
}

// TestSandboxExecPIAgentDir_CredentialReplaceWorksWithoutDeny is the paired
// negative test of the section-21f part of PlantsDenied: without the deny,
// the session can remove auth.json and put a symlink at its path.
func TestSandboxExecPIAgentDir_CredentialReplaceWorksWithoutDeny(t *testing.T) {
	e := newPIAgentDirEnv(t)
	profilePath := withMutatedProfile(t, e.m, func(p string) string { return piAgentCredentialDeny(e).ReplaceAllString(p, "") })

	if out, err := e.run(t, profilePath, `rm "$A/auth.json" && ln -s /tmp/x "$A/auth.json"`); err != nil {
		t.Fatalf("without section 21f the replace of auth.json still failed, so PlantsDenied proves nothing for it.\nExit: %v\nOutput: %s", err, out)
	}
	if fi, err := os.Lstat(filepath.Join(e.agentDir, "auth.json")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("auth.json on the host = (%v, %v), want a symlink", fi, err)
	}
}

// piAgentWriteClause matches the section-6a write clause of e.
func piAgentWriteClause(e piAgentDirEnv) *regexp.Regexp {
	return regexp.MustCompile(`(?s)\(allow file-read\* file-write\* file-test-existence file-read-metadata\n  \(literal ` +
		regexp.QuoteMeta(sbplQuoteForTest(filepath.Join(e.agentDir, "auth.json"))) + `\).*?\(subpath ` +
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
