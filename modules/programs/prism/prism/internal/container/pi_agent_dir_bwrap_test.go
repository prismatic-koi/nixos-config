//go:build linux

package container

// pi_agent_dir_bwrap_test.go — live bwrap coverage of the pi agent dir
// layout (#3284). The unit tests in pi_agent_dir_test.go pin the argv. These
// tests start a real bwrap with the args of appendPIAgentDirBwrapMounts and
// run, in the sandbox, the file operations that pi does during a normal
// session and the operations that a worker uses to plant code for a later
// session.
//
// Skips (never fails) when bwrap cannot start a sandbox here. See
// requireUsableBwrapForUsageTest for the environments.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const piAgentLiveSandboxDir = "/run/prism/pi-agent"

// piAgentLiveFixture makes a host agent dir with the entries of a real one.
func piAgentLiveFixture(t *testing.T) (hostDir, worktree string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	hostDir = filepath.Join(root, ".pi", "agent")
	for _, d := range []string{"bin", "themes", "sessions/--other-worktree--"} {
		if err := os.MkdirAll(filepath.Join(hostDir, d), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	files := map[string]string{
		"settings.json":                       `{"extensions":[]}`,
		"trust.json":                          `{}`,
		"auth.json":                           `{"anthropic":{}}`,
		"notion-mcp-oauth.json":               `{"refreshToken":"r"}`,
		"sessions/--other-worktree--/a.jsonl": `{"type":"session","id":"a","cwd":"/other"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(hostDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return hostDir, filepath.Join(root, "worktree")
}

// hostTree lists every path under dir, relative to dir.
func hostTree(t *testing.T, dir string) []string {
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

func piAgentLiveArgs(t *testing.T, hostDir, worktree string) []string {
	t.Helper()
	args, err := appendPIAgentDirBwrapMounts(nil, hostDir, piAgentLiveSandboxDir, worktree)
	if err != nil {
		t.Fatalf("appendPIAgentDirBwrapMounts: %v", err)
	}
	return args
}

// The write list works and reaches the host: the four pi lock dirs, an
// in-place write of auth.json and atlassian-mcp-oauth.json, and a new
// session file in the sessions dir of the worktree. A Notion token file
// that is still in the agent dir, before the move to its own dir, is
// read-only.
func TestBwrapPIAgentDir_PiWritesWork(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostDir, worktree := piAgentLiveFixture(t)
	args := piAgentLiveArgs(t, hostDir, worktree)
	enc := encodePiCWD(worktree)

	script := `set -e
A=` + piAgentLiveSandboxDir + `
cat "$A/settings.json" >/dev/null
cat "$A/notion-mcp-oauth.json" >/dev/null
if printf x > "$A/notion-mcp-oauth.json" 2>/dev/null; then exit 9; fi
for l in auth.json.lock settings.json.lock trust.json.lock models-store.json.lock; do
  mkdir "$A/$l"; touch "$A/$l"; rmdir "$A/$l"
done
printf refreshed > "$A/auth.json"
printf atlassian > "$A/atlassian-mcp-oauth.json"
printf '{"type":"session"}\n' > "$A/sessions/` + enc + `/s.jsonl"
printf '{"type":"message"}\n' >> "$A/sessions/` + enc + `/s.jsonl"
`
	if out, err := runInBwrap(t, bwrapBin, args, script); err != nil {
		t.Fatalf("a pi write failed in the sandbox: %v\n%s", err, out)
	}
	for name, want := range map[string]string{
		"auth.json":                    "refreshed",
		"atlassian-mcp-oauth.json":     "atlassian",
		"sessions/" + enc + "/s.jsonl": "{\"type\":\"session\"}\n{\"type\":\"message\"}\n",
	} {
		got, err := os.ReadFile(filepath.Join(hostDir, name))
		if err != nil || string(got) != want {
			t.Errorf("host %s = (%q, %v), want %q", name, got, err, want)
		}
	}
}

// A worker cannot change, replace, or add a path in the host agent dir,
// cannot plant a binary in bin/, cannot replace a credential file, and
// cannot see the sessions dir of another worktree.
func TestBwrapPIAgentDir_PlantsDoNotReachHost(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostDir, worktree := piAgentLiveFixture(t)
	args := piAgentLiveArgs(t, hostDir, worktree)
	before := hostTree(t, hostDir)

	A := piAgentLiveSandboxDir
	mustFail := []string{
		`printf '{"extensions":["/tmp/x.ts"]}' > ` + A + `/settings.json`,
		`rm ` + A + `/settings.json`,
		`mv ` + A + `/settings.json ` + A + `/s2`,
		`touch ` + A + `/bin/git`,
		`touch ` + A + `/themes/x.json`,
		`printf '{}' > ` + A + `/trust.json`,
		`rm ` + A + `/auth.json`,
		`mv ` + A + `/auth.json ` + A + `/a2`,
		`ls ` + A + `/sessions/--other-worktree--`,
	}
	for _, script := range mustFail {
		if out, err := runInBwrap(t, bwrapBin, args, script); err == nil {
			t.Errorf("%q succeeded in the sandbox\n%s", script, out)
		}
	}
	// A create in the private dir works, but stays in the sandbox.
	plant := `touch ` + A + `/SYSTEM.md ` + A + `/mcp.json && mkdir ` + A + `/prompts && touch ` + A + `/prompts/p.md`
	if out, err := runInBwrap(t, bwrapBin, args, plant); err != nil {
		t.Fatalf("a create in the private agent dir failed: %v\n%s", err, out)
	}

	if after := hostTree(t, hostDir); !slices.Equal(after, before) {
		t.Errorf("host agent dir changed:\nbefore: %v\nafter:  %v", before, after)
	}
	if got, _ := os.ReadFile(filepath.Join(hostDir, "settings.json")); string(got) != `{"extensions":[]}` {
		t.Errorf("host settings.json changed: %q", got)
	}
}

// The no-op proof: with the old layout (a read-write bind of the host
// agent dir), the same plant reaches the host. So the host-tree assertion
// above can see a plant.
func TestBwrapPIAgentDir_PlantReachesHostWithOldLayout(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostDir, _ := piAgentLiveFixture(t)
	oldArgs := []string{"--dir", "/run/prism", "--dir", piAgentLiveSandboxDir, "--bind", hostDir, piAgentLiveSandboxDir}

	if out, err := runInBwrap(t, bwrapBin, oldArgs, `touch `+piAgentLiveSandboxDir+`/SYSTEM.md`); err != nil {
		t.Fatalf("plant failed with the old layout: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(hostDir, "SYSTEM.md")); err != nil {
		t.Fatalf("with the old layout the plant did not reach the host (%v), so the plant test proves nothing", err)
	}
	if !strings.Contains(strings.Join(hostTree(t, hostDir), " "), "SYSTEM.md") {
		t.Fatal("hostTree does not list the planted file")
	}
}

// piSharedStateLiveFixture makes the two shared dirs under a fake home,
// with a Notion token file, and returns the bind args that the production
// emitter (appendPIBwrapMounts) makes for them, and only those.
func piSharedStateLiveFixture(t *testing.T) (lockDir, notionDir string, bindArgs []string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Setenv("HOME", home)
	lockDir, notionDir = PISharedStateDirs(home)
	for _, d := range []string{lockDir, notionDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(notionDir, "notion-mcp-oauth.json"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := piBwrapMountsCfg(t)
	cfg.PILockHostDir, cfg.PINotionHostDir = lockDir, notionDir
	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && (args[i+1] == lockDir || args[i+1] == notionDir) {
			bindArgs = append(bindArgs, args[i:i+3]...)
		}
	}
	if len(bindArgs) != 6 {
		t.Fatalf("appendPIBwrapMounts made %v for the shared dirs, want two binds", bindArgs)
	}
	return lockDir, notionDir, bindArgs
}

// The shared dirs in the sandbox are the host dirs (#3289, #3290). A lock
// dir that the sandbox makes is on the host, a lock dir that the host holds
// is held in the sandbox, and the rename that the Notion extension does
// onto its store file works and reaches the host.
func TestBwrapPIAgentDir_SharedStateDirsAreHostDirs(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	lockDir, notionDir, bindArgs := piSharedStateLiveFixture(t)
	if err := os.Mkdir(filepath.Join(lockDir, "held-by-host.lock"), 0o700); err != nil {
		t.Fatal(err)
	}

	script := `set -e
L='` + lockDir + `'
N='` + notionDir + `'
mkdir "$L/anthropic-oauth-refresh.lock"
if mkdir "$L/held-by-host.lock" 2>/dev/null; then exit 3; fi
printf new > "$N/.notion-mcp.tmp.1"
mv "$N/.notion-mcp.tmp.1" "$N/notion-mcp-oauth.json"
`
	if out, err := runInBwrap(t, bwrapBin, bindArgs, script); err != nil {
		t.Fatalf("the shared dirs did not work in the sandbox: %v\n%s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(lockDir, "anthropic-oauth-refresh.lock")); err != nil || !fi.IsDir() {
		t.Errorf("the lock dir that the sandbox made is not on the host: (%v, %v)", fi, err)
	}
	if got, err := os.ReadFile(filepath.Join(notionDir, "notion-mcp-oauth.json")); err != nil || string(got) != "new" {
		t.Errorf("host Notion token file = (%q, %v), want %q", got, err, "new")
	}
}

// The no-op proof of the lock half: with a private dir in place of the
// bind, the lock dir that the sandbox makes does not reach the host. So the
// host assertion above can fail.
func TestBwrapPIAgentDir_SharedLockNotOnHostWithPrivateDir(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	lockDir, _, _ := piSharedStateLiveFixture(t)

	if out, err := runInBwrap(t, bwrapBin, []string{"--dir", lockDir}, `mkdir '`+lockDir+`/anthropic-oauth-refresh.lock'`); err != nil {
		t.Fatalf("mkdir in a private dir failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(lockDir, "anthropic-oauth-refresh.lock")); !os.IsNotExist(err) {
		t.Fatalf("with a private dir the lock dir reached the host (%v), so the shared-dir test proves nothing", err)
	}
}

// The no-op proof of the Notion half: with the old layout, a file bind of
// the token file, the rename onto it fails. So the rename assertion above
// can fail.
func TestBwrapPIAgentDir_NotionRenameFailsOnFileBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	_, notionDir, _ := piSharedStateLiveFixture(t)
	token := filepath.Join(notionDir, "notion-mcp-oauth.json")
	A := piAgentLiveSandboxDir
	oldArgs := []string{"--dir", "/run/prism", "--dir", A, "--bind", token, A + "/notion-mcp-oauth.json"}

	script := `printf new > ` + A + `/.notion-mcp.tmp.1 && mv ` + A + `/.notion-mcp.tmp.1 ` + A + `/notion-mcp-oauth.json`
	if out, err := runInBwrap(t, bwrapBin, oldArgs, script); err == nil {
		t.Fatalf("the rename onto a file bind worked, so the shared-dir test proves nothing\n%s", out)
	}
	if got, _ := os.ReadFile(token); string(got) != "old" {
		t.Errorf("host token file = %q, want %q", got, "old")
	}
}
