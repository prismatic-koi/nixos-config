package git

// host_config_pins_test.go — a planted common-dir config against the
// hostGitConfig pins (#3337).
//
// A bwrap worker can write .bare/config after a host rename detaches its
// read-only pin. Each test plants keys that run a program in .bare/config,
// runs a plain git command to show that the plant runs (the control), and
// then runs the prism host git command and checks that the payload did not
// run. The payload only appends a line to a marker file in t.TempDir().

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plantConfig sets each key in the config of gitDir, as a worker does after
// the pin on .bare/config detaches.
func plantConfig(t *testing.T, gitDir string, kvs ...[2]string) {
	t.Helper()
	for _, kv := range kvs {
		if out, err := exec.Command("git", "config", "--file", filepath.Join(gitDir, "config"), "--replace-all", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("plant %s: %v\n%s", kv[0], err, out)
		}
	}
}

// payload returns a shell command that appends name to the marker.
func (f hostFixture) payload(name string) string {
	return fmt.Sprintf("echo %s >> '%s'", name, f.marker)
}

// payloadScript writes an executable script that appends name to the marker
// and exits with code, and returns its path.
func (f hostFixture) payloadScript(t *testing.T, name string, code int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := fmt.Sprintf("#!/bin/sh\necho %s >> '%s'\nexit %d\n", name, f.marker, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeProgramsOnPath puts a script for each name first on PATH. Each script
// appends its name to the returned log file and exits 1. The scripts stand
// in for the programs that the pins name, so a test can see that git runs
// the pinned program and not the planted one.
func fakeProgramsOnPath(t *testing.T, names ...string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "programs.log")
	for _, name := range names {
		script := fmt.Sprintf("#!/bin/sh\necho %s >> '%s'\nexit 1\n", name, log)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// unsetEnv removes key for the test, and restores it after.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// setCommitIdentity lets git commit in a test with no global config.
func setCommitIdentity(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "Test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@test.com")
	}
}

// plainGit runs git with no pinning in dir.
func plainGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f hostFixture) bare() string { return filepath.Join(f.root, ".bare") }

func (f hostFixture) workerRepo(t *testing.T) Repo {
	t.Helper()
	r, err := OpenWorktree(f.root, f.worker)
	if err != nil {
		t.Fatalf("OpenWorktree: %v", err)
	}
	return r
}

// TestHostGit_PlantedSSHCommand: a planted core.sshCommand does not run on a
// host fetch or push. git runs ssh from PATH, so SSH auth works as before.
func TestHostGit_PlantedSSHCommand(t *testing.T) {
	f := newHostFixture(t)
	unsetEnv(t, "GIT_SSH_COMMAND")
	unsetEnv(t, "GIT_SSH")
	sshLog := fakeProgramsOnPath(t, "ssh")
	plantConfig(t, f.bare(),
		[2]string{"core.sshCommand", f.payload("sshcommand") + "; false"},
		[2]string{"remote.origin.url", "ssh://prism-test.invalid/x.git"},
	)

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "fetch", "origin")

	if err := FetchRemote(f.root); err == nil {
		t.Error("FetchRemote: want an error from the fake ssh")
	}
	if _, err := f.workerRepo(t).CombinedOutput("push", "origin", "refs/heads/feat:refs/heads/feat"); err == nil {
		t.Error("push: want an error from the fake ssh")
	}
	f.requireNoPayload(t, "a host fetch and push")
	if got := readLog(t, sshLog); got != "ssh\nssh" {
		t.Errorf("fake ssh log = %q, want one ssh run for the fetch and one for the push", got)
	}
}

// TestHostGit_PlantedCredentialHelperAndAskPass: a host fetch from an HTTPS
// remote that asks for credentials runs no planted credential helper and no
// planted askpass program. Host git allows HTTPS, so the fetch reaches the
// credential step.
func TestHostGit_PlantedCredentialHelperAndAskPass(t *testing.T) {
	f := newHostFixture(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="prism-test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	askPass := f.payloadScript(t, "askpass", 0)
	unsetEnv(t, "GIT_ASKPASS")
	t.Setenv("SSH_ASKPASS", askPass)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	plantConfig(t, f.bare(),
		[2]string{"remote.origin.url", srv.URL + "/x.git"},
		[2]string{"http.sslVerify", "false"},
		[2]string{"credential.helper", "!" + f.payload("helper") + "; true"},
		[2]string{"credential." + srv.URL + ".helper", "!" + f.payload("urlhelper") + "; true"},
		[2]string{"core.askPass", askPass},
	)

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "fetch", "origin")

	err := FetchRemote(f.root)
	if err == nil || strings.Contains(err.Error(), "not allowed") {
		t.Errorf("FetchRemote = %v, want an error from the 401 server, not a transport refusal", err)
	}
	f.requireNoPayload(t, "a host fetch that needs credentials")
}

// driverAttributes selects the planted drivers for every path.
const driverAttributes = "* filter=evil diff=evil merge=evil\n"

// commitOnMain commits files on main with plain git. A test calls it before
// it plants config, because a plain commit runs a planted driver.
func (f hostFixture) commitOnMain(t *testing.T, files map[string]string) {
	t.Helper()
	setCommitIdentity(t)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(f.main, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "setup"}} {
		if out, err := plainGit(t, f.main, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// plantDrivers commits a .gitattributes that selects the evil drivers on
// main, and then plants drivers in .bare/config.
func (f hostFixture) plantDrivers(t *testing.T, drivers ...[2]string) {
	t.Helper()
	f.commitOnMain(t, map[string]string{".gitattributes": driverAttributes, "data.txt": "base\n"})
	plantConfig(t, f.bare(), drivers...)
}

// conflictingBranches makes the worktrees control and spawned from main, and
// commits a change of data.txt on each of them and on main. It commits
// through host git, so a planted config runs nothing. A rebase of control or
// spawned onto main then has a conflict in data.txt. It returns the host
// Repo of spawned.
func (f hostFixture) conflictingBranches(t *testing.T) Repo {
	t.Helper()
	var spawned Repo
	for _, name := range []string{"control", "spawned", "main"} {
		dir := filepath.Join(f.root, name)
		if name != "main" {
			created, err := CreateWorktree(f.root, name)
			if err != nil {
				t.Fatalf("CreateWorktree %s: %v", name, err)
			}
			dir = created.Path
		}
		if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		repo, err := OpenWorktree(f.root, dir)
		if err != nil {
			t.Fatalf("OpenWorktree %s: %v", name, err)
		}
		if out, err := repo.CombinedOutput("commit", "-q", "-a", "-m", name); err != nil {
			t.Fatalf("commit %s: %v\n%s", name, err, out)
		}
		if name == "spawned" {
			spawned = repo
		}
	}
	f.requireNoPayload(t, "the host commits of the setup")
	return spawned
}

// TestHostGit_PlantedFilterDriver: a .gitattributes in the tree selects a
// filter and a diff driver that a planted .bare/config defines. A host
// worktree add, a checkout, a status, a diff, a commit, and a config command
// run no driver.
func TestHostGit_PlantedFilterDriver(t *testing.T) {
	f := newHostFixture(t)
	f.plantDrivers(t,
		[2]string{"filter.evil.smudge", f.payload("smudge") + "; cat"},
		[2]string{"filter.evil.clean", f.payload("clean") + "; cat"},
		[2]string{"diff.evil.textconv", f.payload("textconv") + "; cat"},
		[2]string{"diff.evil.command", f.payload("diffcommand")},
	)

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "worktree", "add", filepath.Join(f.root, "control"), "-b", "control", "main")

	created, err := CreateWorktree(f.root, "spawned")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	f.requireNoPayload(t, "CreateWorktree")
	if data, err := os.ReadFile(filepath.Join(created.Path, "data.txt")); err != nil || string(data) != "base\n" {
		t.Errorf("data.txt in the new worktree = %q, %v; want the committed content", data, err)
	}

	if err := os.WriteFile(filepath.Join(created.Path, "data.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "control", "data.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.requireControlFires(t, filepath.Join(f.root, "control"), "diff", "HEAD")

	if st, err := Stat(created.Path); err != nil || st.Insertions != 1 {
		t.Errorf("Stat = %+v, %v; want one insertion", st, err)
	}
	repo, err := OpenWorktree(f.root, created.Path)
	if err != nil {
		t.Fatalf("OpenWorktree: %v", err)
	}
	for _, args := range [][]string{
		{"status", "--short"},
		{"diff", "HEAD"},
		{"commit", "-q", "-a", "-m", "change"},
		{"switch", "-q", "-c", "other", "main"},
		{"switch", "-q", "spawned"},
		{"config", "--get", "filter.evil.smudge"},
	} {
		if out, err := repo.CombinedOutput(args...); err != nil {
			t.Errorf("git %v: %v\n%s", args, err, out)
		}
	}
	f.requireNoPayload(t, "host git in the new worktree")
}

// TestHostGit_PlantedMergeDriver: a host rebase with a conflict runs no
// merge driver that the tree selects and a planted config defines.
func TestHostGit_PlantedMergeDriver(t *testing.T) {
	f := newHostFixture(t)
	f.plantDrivers(t, [2]string{"merge.evil.driver", f.payload("mergedriver") + "; false"})
	spawned := f.conflictingBranches(t)

	f.requireControlFires(t, filepath.Join(f.root, "control"), "rebase", "main")

	_, _ = spawned.CombinedOutput("rebase", "main")
	f.requireNoPayload(t, "a host rebase with a conflict")
}

// TestHostGit_PlantedMergeDefault: with no merge attribute, git uses the
// driver that merge.default names. A planted merge.default needs no
// attribute, and GIT_ATTR_SOURCE makes every merge attribute unset. A host
// rebase with a conflict runs no planted driver. The plant also defines the
// driver names text and "", which beat a pin of merge.default to text or to
// an empty value.
func TestHostGit_PlantedMergeDefault(t *testing.T) {
	f := newHostFixture(t)
	f.commitOnMain(t, map[string]string{"data.txt": "base\n"})
	plantConfig(t, f.bare(),
		[2]string{"merge.default", "evil"},
		[2]string{"merge.evil.driver", f.payload("mergedefault") + "; false"},
		[2]string{"merge.text.driver", f.payload("mergetext") + "; false"},
	)
	cfg, err := os.OpenFile(filepath.Join(f.bare(), "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(cfg, "[merge \"\"]\n\tdriver = %s; false\n", f.payload("mergeempty"))
	if err := cfg.Close(); err != nil {
		t.Fatal(err)
	}
	spawned := f.conflictingBranches(t)

	f.requireControlFires(t, filepath.Join(f.root, "control"), "rebase", "main")

	out, err := spawned.CombinedOutput("rebase", "main")
	if err == nil || !strings.Contains(string(out), "CONFLICT") {
		t.Errorf("host rebase = %v, %q; want a conflict from the built-in text merge", err, out)
	}
	f.requireNoPayload(t, "a host rebase with a conflict and no merge attribute")
}

// TestHostGit_PlantedConfigHook: a hook that a planted config defines does
// not run on a host worktree add or fetch. A config that also sets
// hook.<event>.command defeats the pin (#3349), so the test does not plant
// that key.
func TestHostGit_PlantedConfigHook(t *testing.T) {
	f := newHostFixture(t)
	plantConfig(t, f.bare(),
		[2]string{"hook.evil.command", f.payload("confighook")},
		[2]string{"hook.evil.event", "post-checkout"},
	)
	if out, err := exec.Command("git", "config", "--file", filepath.Join(f.bare(), "config"), "--add", "hook.evil.event", "reference-transaction").CombinedOutput(); err != nil {
		t.Fatalf("plant event: %v\n%s", err, out)
	}

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "worktree", "add", "-b", "control", filepath.Join(f.root, "control"), "main")

	if _, err := CreateWorktree(f.root, "spawned"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if err := FetchRemote(f.root); err != nil {
		t.Errorf("FetchRemote: %v", err)
	}
	f.requireNoPayload(t, "a host worktree add and fetch")
}

// TestHostGit_FileTransportRefused: a planted remote URL or push URL that
// names a local repo makes git run upload-pack, receive-pack, and the hooks
// of that repo on the host. Host git refuses the file transport.
func TestHostGit_FileTransportRefused(t *testing.T) {
	f := newHostFixture(t)
	evil := filepath.Join(t.TempDir(), "evil.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", evil).CombinedOutput(); err != nil {
		t.Fatalf("init evil: %v\n%s", err, out)
	}
	hook := fmt.Sprintf("#!/bin/sh\n%s\nexit 0\n", f.payload("prereceive"))
	if err := os.WriteFile(filepath.Join(evil, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	plantConfig(t, f.bare(),
		[2]string{"remote.origin.pushurl", evil},
		[2]string{"remote.origin.uploadpack", f.payload("uploadpack") + "; git-upload-pack"},
	)

	f.requireControlFires(t, f.worker, "push", "origin", "refs/heads/feat:refs/heads/control")
	f.requireControlFires(t, f.root, "--git-dir", ".bare", "fetch", "origin")

	prev := allowFileTransport.Swap(false)
	t.Cleanup(func() { allowFileTransport.Store(prev) })

	if out, err := f.workerRepo(t).CombinedOutput("push", "origin", "refs/heads/feat:refs/heads/feat"); err == nil || !strings.Contains(string(out), "transport 'file' not allowed") {
		t.Errorf("push = %v, %q; want a file transport refusal", err, out)
	}
	if err := FetchRemote(f.root); err == nil || !strings.Contains(err.Error(), "transport 'file' not allowed") {
		t.Errorf("FetchRemote = %v, want a file transport refusal", err)
	}
	f.requireNoPayload(t, "a host push and fetch")
}

// TestHostGit_PlantedSigningPrograms: a host commit with signing on runs the
// default program of each signature format, not a planted one.
func TestHostGit_PlantedSigningPrograms(t *testing.T) {
	f := newHostFixture(t)
	setCommitIdentity(t)
	programs := fakeProgramsOnPath(t, "gpg", "gpgsm", "ssh-keygen")
	evil := f.payloadScript(t, "signer", 1)
	plantConfig(t, f.bare(),
		[2]string{"commit.gpgSign", "true"},
		[2]string{"user.signingKey", "key::ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		[2]string{"gpg.program", evil},
		[2]string{"gpg.openpgp.program", evil},
		[2]string{"gpg.x509.program", evil},
		[2]string{"gpg.ssh.program", evil},
	)
	for _, format := range []struct{ name, program string }{
		{"openpgp", "gpg"},
		{"x509", "gpgsm"},
		{"ssh", "ssh-keygen"},
	} {
		plantConfig(t, f.bare(), [2]string{"gpg.format", format.name})
		f.requireControlFires(t, f.worker, "commit", "--allow-empty", "-m", "control")

		if _, err := f.workerRepo(t).CombinedOutput("commit", "--allow-empty", "-m", "signed"); err == nil {
			t.Errorf("%s: commit: want an error from the fake %s", format.name, format.program)
		}
		f.requireNoPayload(t, "a signed host commit with gpg.format="+format.name)
		if got := readLog(t, programs); got != format.program {
			t.Errorf("%s: fake program log = %q, want %q", format.name, got, format.program)
		}
		_ = os.Remove(programs)
	}

	// With no signing key, git runs gpg.ssh.defaultKeyCommand.
	plantConfig(t, f.bare(), [2]string{"gpg.ssh.defaultKeyCommand", evil})
	if out, err := exec.Command("git", "config", "--file", filepath.Join(f.bare(), "config"), "--unset", "user.signingKey").CombinedOutput(); err != nil {
		t.Fatalf("unset signingKey: %v\n%s", err, out)
	}
	f.requireControlFires(t, f.worker, "commit", "--allow-empty", "-m", "control")
	_, _ = f.workerRepo(t).CombinedOutput("commit", "--allow-empty", "-m", "signed")
	f.requireNoPayload(t, "a signed host commit with no signing key")
}

// TestHostGit_PlantedAlternateRefsCommand: a planted alternates file and
// core.alternateRefsCommand do not run a command on a host fetch, and the
// fetch still works.
func TestHostGit_PlantedAlternateRefsCommand(t *testing.T) {
	f := newHostFixture(t)
	alternates := filepath.Join(f.bare(), "objects", "info", "alternates")
	if err := os.WriteFile(alternates, []byte(filepath.Join(f.origin, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plantConfig(t, f.bare(), [2]string{"core.alternateRefsCommand", f.payload("alternaterefs") + "; true"})

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "fetch", "origin")

	if err := FetchRemote(f.root); err != nil {
		t.Errorf("FetchRemote: %v", err)
	}
	f.requireNoPayload(t, "a host fetch with alternates")
}

// TestHostGit_PlantedGitProxy: a planted core.gitProxy does not run on a
// host fetch from a git:// remote. Host git refuses the git transport, and
// GIT_PROXY_COMMAND also turns off core.gitProxy.
func TestHostGit_PlantedGitProxy(t *testing.T) {
	f := newHostFixture(t)
	unsetEnv(t, "GIT_PROXY_COMMAND")
	plantConfig(t, f.bare(),
		[2]string{"core.gitProxy", f.payloadScript(t, "gitproxy", 1)},
		[2]string{"remote.origin.url", "git://127.0.0.1:1/x.git"},
	)

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "fetch", "origin")

	if err := FetchRemote(f.root); err == nil || !strings.Contains(err.Error(), "transport 'git' not allowed") {
		t.Errorf("FetchRemote = %v, want a refusal of the git transport", err)
	}
	f.requireNoPayload(t, "a host fetch from a git:// remote")
}

// TestHostGit_PlantedRemoteHelper: a planted remote.<name>.vcs or
// <name>:: URL makes git run the helper git-remote-<name>. A name with a
// slash is a path relative to the cwd of git, which is the worktree or
// .bare, and a worker writes both. A name with no slash is a program on
// PATH. The plant also allows the transport in config, which beats
// protocol.allow = never. Host git allows only the transports in
// GIT_ALLOW_PROTOCOL.
func TestHostGit_PlantedRemoteHelper(t *testing.T) {
	f := newHostFixture(t)
	helper := fmt.Sprintf("#!/bin/sh\n%s\nexit 1\n", f.payload("remotehelper"))
	for _, dir := range []string{f.worker, f.bare()} {
		if err := os.MkdirAll(filepath.Join(dir, "git-remote-x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "git-remote-x", "evil"), []byte(helper), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plantConfig(t, f.bare(),
		[2]string{"remote.origin.vcs", "x/evil"},
		[2]string{"protocol.x/evil.allow", "always"},
	)

	f.requireControlFires(t, f.worker, "fetch", "origin")
	f.requireControlFires(t, f.bare(), "--git-dir", ".", "fetch", "origin")

	if out, err := f.workerRepo(t).CombinedOutput("fetch", "origin"); err == nil || !strings.Contains(string(out), "transport 'x/evil' not allowed") {
		t.Errorf("fetch in the worktree = %v, %q; want a transport refusal", err, out)
	}
	if err := FetchRemote(f.root); err == nil || !strings.Contains(err.Error(), "transport 'x/evil' not allowed") {
		t.Errorf("FetchRemote = %v, want a transport refusal", err)
	}
	f.requireNoPayload(t, "a host fetch with a planted helper path")

	// A helper on PATH, named by a <name>:: URL.
	pathLog := fakeProgramsOnPath(t, "git-remote-pathhelper")
	if out, err := exec.Command("git", "config", "--file", filepath.Join(f.bare(), "config"), "--unset", "remote.origin.vcs").CombinedOutput(); err != nil {
		t.Fatalf("unset vcs: %v\n%s", err, out)
	}
	plantConfig(t, f.bare(),
		[2]string{"remote.origin.url", "pathhelper::x"},
		[2]string{"protocol.pathhelper.allow", "always"},
	)
	_, _ = plainGit(t, f.root, "--git-dir", ".bare", "fetch", "origin")
	if readLog(t, pathLog) == "" {
		t.Fatal("control: a plain fetch did not run the helper on PATH; the plant does not work")
	}
	_ = os.Remove(pathLog)
	if err := FetchRemote(f.root); err == nil || !strings.Contains(err.Error(), "transport 'pathhelper' not allowed") {
		t.Errorf("FetchRemote = %v, want a transport refusal", err)
	}
	if got := readLog(t, pathLog); got != "" {
		t.Errorf("a host fetch ran the helper on PATH: %q", got)
	}
}

// TestHostGit_PlantedEditor: a host commit that opens an editor runs no
// planted core.editor.
func TestHostGit_PlantedEditor(t *testing.T) {
	f := newHostFixture(t)
	setCommitIdentity(t)
	for _, k := range []string{"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "VISUAL", "EDITOR"} {
		unsetEnv(t, k)
	}
	plantConfig(t, f.bare(),
		[2]string{"core.editor", f.payloadScript(t, "editor", 0)},
		[2]string{"sequence.editor", f.payloadScript(t, "sequenceeditor", 0)},
	)

	f.requireControlFires(t, f.worker, "commit", "--allow-empty")

	_, _ = f.workerRepo(t).CombinedOutput("commit", "--allow-empty")
	f.requireNoPayload(t, "a host commit with no message")
}

// writeOldLooseObject writes a loose blob whose name starts with 17 and sets
// its mtime to the past. git gc --auto counts the loose objects in
// objects/17, and git gc keeps a recent unreachable object without a look
// at gc.recentObjectsHook.
func writeOldLooseObject(t *testing.T, gitDir string, seed int) {
	t.Helper()
	for i := 0; ; i++ {
		content := fmt.Sprintf("prism-test %d %d\n", seed, i)
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
		name := fmt.Sprintf("%x", sum)
		if !strings.HasPrefix(name, "17") {
			continue
		}
		cmd := exec.Command("git", "--git-dir", gitDir, "hash-object", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(content)
		if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != name {
			t.Fatalf("hash-object = %q, %v; want %s", out, err, name)
		}
		old := time.Now().Add(-365 * 24 * time.Hour)
		if err := os.Chtimes(filepath.Join(gitDir, "objects", name[:2], name[2:]), old, old); err != nil {
			t.Fatal(err)
		}
		return
	}
}

// TestHostGit_PlantedRecentObjectsHook: auto maintenance after a host commit
// would run a planted gc.recentObjectsHook. git runs every value of the key,
// so no value turns it off. Host git turns off auto maintenance.
func TestHostGit_PlantedRecentObjectsHook(t *testing.T) {
	f := newHostFixture(t)
	setCommitIdentity(t)
	plantConfig(t, f.bare(),
		[2]string{"gc.auto", "1"},
		[2]string{"maintenance.gc.enabled", "true"},
		[2]string{"maintenance.gc.auto", "1"},
		[2]string{"gc.autoDetach", "false"},
		[2]string{"maintenance.autoDetach", "false"},
		[2]string{"gc.recentObjectsHook", f.payload("recentobjects")},
	)
	writeOldLooseObject(t, f.bare(), 1)
	writeOldLooseObject(t, f.bare(), 2)

	f.requireControlFires(t, f.worker, "commit", "--allow-empty", "-m", "control")

	writeOldLooseObject(t, f.bare(), 3)
	writeOldLooseObject(t, f.bare(), 4)
	if out, err := f.workerRepo(t).Command(context.Background(), "commit", "--allow-empty", "-m", "host").CombinedOutput(); err != nil {
		t.Fatalf("host commit: %v\n%s", err, out)
	}
	f.requireNoPayload(t, "a host commit with auto maintenance due")
}

// TestHostGitConfig_DocListsEveryKey: docs/invariants/host-git-commands.md
// lists each key of hostGitConfig. It lists the hook keys as one row.
func TestHostGitConfig_DocListsEveryKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "invariants", "host-git-commands.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, kv := range hostGitConfig {
		key := kv[0]
		if strings.HasPrefix(key, "hook.") {
			key = "hook.<event>.enabled"
		}
		if !strings.Contains(doc, "`"+key+"`") {
			t.Errorf("host-git-commands.md does not list %s", key)
		}
	}
	for _, env := range []string{"GIT_ATTR_SOURCE", "GIT_PROXY_COMMAND", "GIT_NO_LAZY_FETCH", "GIT_PAGER", "GIT_ALLOW_PROTOCOL"} {
		if !strings.Contains(doc, "`"+env+"`") {
			t.Errorf("host-git-commands.md does not list %s", env)
		}
	}
}
