package prismcontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// writeScript writes an executable shell script.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeSystemd writes a fake systemd-run and a fake systemctl into a temp
// dir. The fake systemd-run records its arguments, creates the started
// file as the scope script does (not when dir/no-start exists), records
// the command that follows it, and then acts as podman: it prints
// dir/output, exits with the code in dir/code, or hangs when dir/hang
// exists. The fake systemctl logs each call, prints dir/units
// for list-units, and fails a kill when dir/kill-fails exists.
func fakeSystemd(t *testing.T) (ScopeExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FAKE_SCOPE_DIR", dir)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	run := writeScript(t, dir, "systemd-run", `d=$FAKE_SCOPE_DIR
printf '%s\n' "$@" > "$d/systemd-run.args"
while [ "$#" -gt 0 ] && [ "$1" != prism-container-build ]; do shift; done
shift
started=$1
shift
printf '%s\n' "$@" > "$d/podman.args"
[ -f "$d/output" ] && cat "$d/output"
[ -f "$d/no-start" ] && exit 125
: > "$started"
[ -f "$d/hang" ] && exec sleep 30
[ -f "$d/signal" ] && kill -9 $$
exit $(cat "$d/code" 2>/dev/null || echo 0)
`)
	ctl := writeScript(t, dir, "systemctl", `d=$FAKE_SCOPE_DIR
echo "$*" >> "$d/systemctl.log"
case "$*" in
*list-units*) [ -f "$d/units" ] && cat "$d/units"; exit 0 ;;
esac
if [ -f "$d/kill-fails" ]; then echo "Failed to connect to bus: No such file or directory" >&2; exit 1; fi
if [ -f "$d/not-loaded" ]; then echo "Failed to kill unit x.scope: Unit x.scope not loaded." >&2; exit 5; fi
exit 0
`)
	return ScopeExecutor{SystemdRun: run, Systemctl: ctl, Podman: "/fake/podman", Bwrap: "/fake/bwrap", Runner: &podmanStub{}}, dir
}

// podmanStub answers the podman calls of ScopeExecutor before the build:
// `unshare true` and `info --format json`. It records them.
type podmanStub struct {
	calls     [][]string
	unshareRC int
}

func (p *podmanStub) Run(_ context.Context, stdout, _ io.Writer, args ...string) (int, error) {
	p.calls = append(p.calls, args)
	switch args[0] {
	case "unshare":
		return p.unshareRC, nil
	case "info":
		_, _ = io.WriteString(stdout, `{"store":{"graphRoot":"/fake/graph","runRoot":"/fake/run"}}`)
		return 0, nil
	}
	return 125, nil
}

// stageContext creates a context dir in the build copy dir and returns it.
func stageContext(t *testing.T) string {
	t.Helper()
	root, err := container.PrismContainerBuildStageDirPath()
	if err != nil {
		t.Fatal(err)
	}
	ctx := filepath.Join(root, "0f0e0d0c-0b0a-4908-8706-050403020100.00000001", "context")
	if err := os.MkdirAll(ctx, 0o700); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestScopeExecutor_Args(t *testing.T) {
	e, dir := fakeSystemd(t)
	if err := os.WriteFile(filepath.Join(dir, "output"), []byte("STEP 1/1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctxDir := stageContext(t)
	code, err := e.Build(context.Background(), "prism-build-tok-0001", &out, []string{"build", "--file", "/f", ctxDir})
	if code != 0 || err != nil || out.String() != "STEP 1/1\n" {
		t.Fatalf("Build = %d, %v, output %q", code, err, out.String())
	}
	want := []string{
		"--user", "--scope", "--collect", "--quiet",
		"--expand-environment=no",
		"--unit", "prism-build-tok-0001",
		"--property", "Delegate=yes",
		"--property", "TasksMax=1024",
		"--", "/bin/sh", "-c",
	}
	got := readLines(t, filepath.Join(dir, "systemd-run.args"))
	if !slices.Equal(got[:len(want)], want) {
		t.Errorf("systemd-run args = %q, want them to start with %q", got, want)
	}
	policy, _ := container.PrismContainerBuildPolicyPath()
	podman := readLines(t, filepath.Join(dir, "podman.args"))
	if !slices.Equal(podman[:3], []string{"/fake/podman", "unshare", "/fake/bwrap"}) {
		t.Errorf("command = %q, want podman unshare bwrap", podman)
	}
	sep := slices.Index(podman, "--")
	wantInner := []string{"/fake/podman", "--cgroup-manager=cgroupfs", "build", "--cgroup-parent", cgroupToken, "--signature-policy", policy, "--file", "/f", ctxDir}
	if sep < 0 || !slices.Equal(podman[sep+1:], wantInner) {
		t.Errorf("inner command = %q, want %q", podman[sep+1:], wantInner)
	}
	stageDir := filepath.Dir(ctxDir)
	bw := podman[3:sep]
	for _, bind := range [][]string{
		{"--bind", "/fake/graph", "/fake/graph"},
		{"--bind", "/fake/run", "/fake/run"},
		{"--ro-bind", stageDir, stageDir},
		{"--bind", filepath.Join(stageDir, "vartmp"), "/var/tmp"},
		{"--ro-bind", policy, policy},
	} {
		if !containsSeq(bw, bind) {
			t.Errorf("bwrap options lack %q: %q", bind, bw)
		}
	}
	if info, err := os.Stat(filepath.Join(stageDir, "vartmp")); err != nil || !info.IsDir() {
		t.Errorf("the per-build /var/tmp dir was not created: %v", err)
	}
	// The pause process starts before the scope, so the scope kill does
	// not stop it.
	stub := e.Runner.(*podmanStub)
	if len(stub.calls) != 2 || !slices.Equal(stub.calls[0], []string{"unshare", "true"}) || stub.calls[1][0] != "info" {
		t.Errorf("podman calls before the build = %q, want unshare true, then info", stub.calls)
	}
	if data, err := os.ReadFile(policy); err != nil || string(data) != BuildPolicy {
		t.Errorf("signature policy = %q, %v; want BuildPolicy", data, err)
	}
	// The scope is killed after every build, so that a build step that
	// outlives podman stops too.
	if log := readLines(t, filepath.Join(dir, "systemctl.log")); !slices.Equal(log, []string{"--user kill --signal=SIGKILL prism-build-tok-0001.scope"}) {
		t.Errorf("systemctl calls = %q", log)
	}
}

func TestScopeExecutor_ExitCode(t *testing.T) {
	e, dir := fakeSystemd(t)
	if err := os.WriteFile(filepath.Join(dir, "code"), []byte("3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", stageContext(t)}); code != 3 || err != nil {
		t.Errorf("Build = %d, %v; want 3, nil", code, err)
	}
}

func TestScopeExecutor_KilledBySignalIsAnError(t *testing.T) {
	e, dir := fakeSystemd(t)
	if err := os.WriteFile(filepath.Join(dir, "signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", stageContext(t)})
	if err == nil || !strings.Contains(err.Error(), "killed by signal") {
		t.Errorf("err = %v, want a signal error", err)
	}
}

// TestScopeExecutor_CancelKillsScope: when ctx ends, the executor kills
// the scope, which holds every process of the build, and returns.
func TestScopeExecutor_CancelKillsScope(t *testing.T) {
	e, dir := fakeSystemd(t)
	if err := os.WriteFile(filepath.Join(dir, "hang"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.Build(ctx, "prism-build-tok-0002", &bytes.Buffer{}, []string{"build", stageContext(t)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context error", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("Build returned after %s, want soon after the cancel", time.Since(start))
	}
	log := readLines(t, filepath.Join(dir, "systemctl.log"))
	if len(log) == 0 || log[0] != "--user kill --signal=SIGKILL prism-build-tok-0002.scope" {
		t.Errorf("systemctl calls = %q, want a SIGKILL of the scope", log)
	}
}

func TestScopeExecutor_StopFailureIsReported(t *testing.T) {
	e, dir := fakeSystemd(t)
	if err := os.WriteFile(filepath.Join(dir, "kill-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctxDir := stageContext(t)
	code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", ctxDir})
	var sf *stopFailedError
	if code != 0 || !errors.As(err, &sf) || sf.cause != nil || !strings.Contains(sf.err.Error(), "did not confirm") {
		t.Errorf("Build = %d, %v; want exit 0 and a stop failure", code, err)
	}

	if err := os.Remove(filepath.Join(dir, "kill-fails")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-loaded"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", ctxDir}); code != 0 || err != nil {
		t.Errorf("a scope that is gone: Build = %d, %v; want 0, nil", code, err)
	}
}

func TestScopeExecutor_StopBuilds(t *testing.T) {
	e, dir := fakeSystemd(t)
	units := "prism-build-tok-0001.scope loaded active running prism-build-tok-0001.scope\n" +
		"prism-build-tok-0002.scope loaded active running prism-build-tok-0002.scope\n"
	if err := os.WriteFile(filepath.Join(dir, "units"), []byte(units), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := e.StopBuilds(context.Background(), "prism-build-tok-")
	if n != 2 || err != nil {
		t.Fatalf("StopBuilds = %d, %v; want 2, nil", n, err)
	}
	want := []string{
		"--user list-units --all --plain --no-legend --no-pager --type=scope prism-build-tok-*",
		"--user kill --signal=SIGKILL prism-build-tok-0001.scope",
		"--user kill --signal=SIGKILL prism-build-tok-0002.scope",
	}
	if log := readLines(t, filepath.Join(dir, "systemctl.log")); !slices.Equal(log, want) {
		t.Errorf("systemctl calls = %q, want %q", log, want)
	}
}

// TestScopeScript runs the scope script with the cgroup paths moved into a
// temp dir and a fake command. The script moves itself into a leaf cgroup,
// puts the cgroup path of the build in place of cgroupToken, and runs its
// arguments unchanged otherwise.
func TestScopeScript(t *testing.T) {
	dir := t.TempDir()
	cgRoot := filepath.Join(dir, "cgroup")
	scope := "/user.slice/user-1000.slice/user@1000.service/app.slice/prism-build-x.scope"
	if err := os.MkdirAll(cgRoot+scope, 0o755); err != nil {
		t.Fatal(err)
	}
	cgFile := filepath.Join(dir, "self-cgroup")
	if err := os.WriteFile(cgFile, []byte("0::"+scope+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	echo := writeScript(t, t.TempDir(), "echo-args", `printf '%s\n' "$@"`+"\n")
	script := strings.NewReplacer("/proc/self/cgroup", cgFile, "/sys/fs/cgroup", cgRoot).Replace(scopeScript)

	started := filepath.Join(dir, "started")
	in := []string{started, echo, "unshare", "--", "build", "--cgroup-parent", cgroupToken, "--build-arg", "A=$(id)", "--build-arg", "B=" + cgroupToken, "a b"}
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, "prism-container-build"}, in...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	if _, err := os.Stat(started); err != nil {
		t.Errorf("the script did not create the started file: %v", err)
	}
	want := []string{"unshare", "--", "build", "--cgroup-parent", scope + "/build", "--build-arg", "A=$(id)", "--build-arg", "B=" + cgroupToken, "a b"}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); !slices.Equal(got, want) {
		t.Errorf("command args = %q, want %q", got, want)
	}
	procs, err := os.ReadFile(filepath.Join(cgRoot+scope, "podman", "cgroup.procs"))
	if err != nil || string(procs) != "0\n" {
		t.Errorf("cgroup.procs of the leaf = %q, %v; want 0 (the writing process)", procs, err)
	}
	if strings.Contains(scopeScript, "$$") {
		t.Error("the scope script holds $$, which an expanding systemd-run changes to $")
	}

	if err := os.WriteFile(cgFile, []byte("1:name=systemd:/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notStarted := filepath.Join(dir, "not-started")
	cmd = exec.Command("/bin/sh", "-c", script, "prism-container-build", notStarted, echo, "x")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "no cgroup v2 path") {
		t.Errorf("no cgroup v2 line: err = %v, output %q; want a refusal before the command", err, out)
	}
	if _, err := os.Stat(notStarted); !os.IsNotExist(err) {
		t.Errorf("the started file exists after a refusal: %v", err)
	}
}

// TestScopeScript_MoveFailureDiagnostics: when the script cannot move into
// the leaf cgroup, it writes the cgroup and scheduler state, and it does
// not create the started file. A mkdir wrapper puts a directory at
// leaf/cgroup.procs, so the write fails as a refused move does.
func TestScopeScript_MoveFailureDiagnostics(t *testing.T) {
	dir := t.TempDir()
	cgRoot := filepath.Join(dir, "cgroup")
	scope := "/app.slice/prism-build-x.scope"
	if err := os.MkdirAll(cgRoot+scope, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"cgroup.type": "domain", "cgroup.controllers": "cpu memory pids", "cgroup.subtree_control": ""} {
		if err := os.WriteFile(filepath.Join(cgRoot+scope, f), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cgFile := filepath.Join(dir, "self-cgroup")
	if err := os.WriteFile(cgFile, []byte("0::"+scope+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realMkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skip("no mkdir in PATH")
	}
	bin := t.TempDir()
	writeScript(t, bin, "mkdir", realMkdir+` "$1" && `+realMkdir+` "$1/cgroup.procs"`+"\n")
	script := strings.NewReplacer("/proc/self/cgroup", cgFile, "/sys/fs/cgroup", cgRoot).Replace(scopeScript)
	started := filepath.Join(dir, "started")
	cmd := exec.Command("/bin/sh", "-c", script, "prism-container-build", started, "true")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script exit 0, want a failure; output %s", out)
	}
	for _, want := range []string{"prism: cannot move the build process into the cgroup", "prism: cgroup.type: domain", "prism: cgroup.controllers: cpu memory pids", "prism: sched:", "prism: kernel:"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Errorf("the started file exists after a failed move: %v", err)
	}
}

// TestScopeExecutor_NotStarted: when the scope ends with no started file,
// podman build did not run, and the error says so with the scope output.
func TestScopeExecutor_NotStarted(t *testing.T) {
	e, dir := fakeSystemd(t)
	for name, content := range map[string]string{
		"no-start": "",
		"output":   "prism: cannot move the build process into the cgroup /x/podman\n",
		"code":     "125",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", stageContext(t)})
	var ns *buildNotStartedError
	if code != -1 || !errors.As(err, &ns) || !strings.Contains(err.Error(), "did not start podman build") ||
		!strings.Contains(err.Error(), "cannot move the build process") {
		t.Errorf("Build = %d, %v; want a not-started error with the scope output", code, err)
	}
}

// TestScopeExecutor_ContextOutsideStageRefused: the executor binds the
// parent of the context dir, so it refuses a context dir that is not in
// the build copy dir of prism.
func TestScopeExecutor_ContextOutsideStageRefused(t *testing.T) {
	e, dir := fakeSystemd(t)
	for _, ctxDir := range []string{"/ctx", t.TempDir(), filepath.Join(filepath.Dir(stageContext(t)), "context", "deeper")} {
		_, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", ctxDir})
		if err == nil || !strings.Contains(err.Error(), "not in the build copy dir") {
			t.Errorf("context %s: err = %v, want a refusal", ctxDir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "systemd-run.args")); !os.IsNotExist(err) {
		t.Errorf("systemd-run ran: %v", err)
	}
}

func TestScopeExecutor_PauseProcessFailure(t *testing.T) {
	e, dir := fakeSystemd(t)
	e.Runner = &podmanStub{unshareRC: 125}
	_, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", stageContext(t)})
	var ns *buildNotStartedError
	if !errors.As(err, &ns) || !strings.Contains(err.Error(), "podman unshare true failed") {
		t.Errorf("err = %v, want a not-started error that names podman unshare", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "systemd-run.args")); !os.IsNotExist(err) {
		t.Errorf("systemd-run ran: %v", err)
	}
}

// TestBwrapArgs_Allowlist checks the file view of a Linux build: a new pid
// namespace with its own /proc, and binds of the allowlist only. The home
// dir, the root dir, /home, /tmp of the host, and /run/user as a whole are
// not visible.
func TestBwrapArgs_Allowlist(t *testing.T) {
	v := buildView{
		Graphroot: "/home/u/.local/share/containers/storage", Runroot: "/run/user/1000/containers",
		RuntimeDir: "/run/user/1000", ConfigDir: "/home/u/.config/containers",
		StageDir: "/s/build-stage/b", VarTmp: "/s/build-stage/b/vartmp", Policy: "/s/build-policy.json",
	}
	args := bwrapArgs(v)
	if !containsSeq(args, []string{"--unshare-pid", "--proc", "/proc"}) {
		t.Errorf("bwrap options lack a new pid namespace with its own /proc: %q", args)
	}
	for _, f := range []string{"--unshare-user", "--unshare-net", "--share-net"} {
		if slices.Contains(args, f) {
			t.Errorf("bwrap options hold %s: %q", f, args)
		}
	}
	allowed := map[string]bool{
		v.Graphroot: true, v.Runroot: true, v.ConfigDir: true, v.StageDir: true, v.VarTmp: true, v.Policy: true,
		"/sys": true, "/sys/fs/cgroup": true, "/dev/net/tun": true, "/dev/fuse": true,
	}
	for _, p := range append(append([]string{}, systemPaths...), etcPaths...) {
		allowed[p] = true
	}
	for _, d := range runtimeSubdirs {
		allowed[filepath.Join(v.RuntimeDir, d)] = true
	}
	binds := map[string]bool{"--bind": true, "--bind-try": true, "--ro-bind": true, "--ro-bind-try": true, "--dev-bind": true, "--dev-bind-try": true}
	for i := 0; i+2 < len(args); i++ {
		if !binds[args[i]] {
			continue
		}
		src := args[i+1]
		if !allowed[src] {
			t.Errorf("bwrap binds %s, which is not in the allowlist", src)
		}
		for _, never := range []string{"/", "/home", "/home/u", "/tmp", "/run", "/run/user/1000", "/proc", "/etc", "/var"} {
			if src == never {
				t.Errorf("bwrap binds %s", src)
			}
		}
		i += 2
	}
}

// containsSeq reports whether seq appears in s as consecutive elements.
func containsSeq(s, seq []string) bool {
	for i := 0; i+len(seq) <= len(s); i++ {
		if slices.Equal(s[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestPlainExecutor_AddsNprocLimit(t *testing.T) {
	r := &recordRunner{}
	if _, err := (PlainExecutor{Runner: r}).Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", "--file", "/f", "/ctx"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"build", "--ulimit", "nproc=1024:1024", "--file", "/f", "/ctx"}; !slices.Equal(r.args, want) {
		t.Errorf("podman args = %q, want %q", r.args, want)
	}
	if n, err := (PlainExecutor{Runner: r}).StopBuilds(context.Background(), "x"); n != 0 || err != nil {
		t.Errorf("StopBuilds = %d, %v", n, err)
	}
}

type recordRunner struct{ args []string }

func (r *recordRunner) Run(_ context.Context, _, _ io.Writer, args ...string) (int, error) {
	r.args = args
	return 0, nil
}

func TestDefaultBuildExecutor(t *testing.T) {
	if _, ok := DefaultBuildExecutor("linux", nil, nil).(ScopeExecutor); !ok {
		t.Errorf("linux executor is not ScopeExecutor")
	}
	if _, ok := DefaultBuildExecutor("darwin", nil, nil).(PlainExecutor); !ok {
		t.Errorf("darwin executor is not PlainExecutor")
	}
}

func TestParseIgnoreSubset(t *testing.T) {
	s := parseIgnoreSubset([]byte("# c\n\n/target/\n  node_modules  \r\n**/cache\n*.log\na/**\n./b/./c\n../up\n**/x*y\n#notacomment-but-comment\n"))
	for rel, want := range map[string]bool{
		"target":           true,
		"node_modules":     true,
		"cache":            true,
		"pkg/cache":        true,
		"pkg/mycache":      false,
		"b/c":              true,
		"b":                false,
		"app.log":          false,
		"a":                false,
		"target2":          false,
		".containerignore": false,
	} {
		if got := s.match(rel); got != want {
			t.Errorf("match(%q) = %v, want %v", rel, got, want)
		}
	}
	if ex := parseIgnoreSubset([]byte("target\n!target/keep\n")); ex.match("target") {
		t.Errorf("a file with a \"!\" exception applied a pattern")
	}
	if self := parseIgnoreSubset([]byte(".containerignore\n.dockerignore\n")); self.match(".containerignore") || self.match(".dockerignore") {
		t.Errorf("the ignore file excluded itself from the copy")
	}
}

func TestRunningBuilds_StaleMarkerRemoved(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	live, err := createBuildMarker("0f0e0d0c-0b0a-4908-8706-050403020100", "00000001")
	if err != nil {
		t.Fatal(err)
	}
	defer live.release()
	stale := filepath.Join(filepath.Dir(live.path), "11111111-2222-4333-8444-555555555555.00000002.lock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := runningBuilds()
	if err != nil || !slices.Equal(ids, []string{"0f0e0d0c-0b0a-4908-8706-050403020100"}) {
		t.Errorf("runningBuilds = %v, %v; want the live build only", ids, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale marker not removed: %v", err)
	}
	live.release()
	if ids, _ := runningBuilds(); len(ids) != 0 {
		t.Errorf("runningBuilds after release = %v, want none", ids)
	}
}

// TestRunningBuilds_StaleCopyRemoved: a context copy whose build marker is
// not held belongs to a dead build, and the next scan removes it. The copy
// of a live build stays.
func TestRunningBuilds_StaleCopyRemoved(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const id = "0f0e0d0c-0b0a-4908-8706-050403020100"
	live, err := createBuildMarker(id, "00000001")
	if err != nil {
		t.Fatal(err)
	}
	defer live.release()
	stage, err := container.PrismContainerBuildStageDirPath()
	if err != nil {
		t.Fatal(err)
	}
	liveCopy := filepath.Join(stage, buildName(id, "00000001"))
	deadCopy := filepath.Join(stage, buildName(id, "00000002"))
	unlockedCopy := filepath.Join(stage, buildName(id, "00000003"))
	for _, d := range []string{liveCopy, deadCopy, unlockedCopy} {
		if err := os.MkdirAll(filepath.Join(d, "context"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lockDir, _ := container.PrismContainerBuildLockDirPath()
	if err := os.WriteFile(filepath.Join(lockDir, buildName(id, "00000003")+buildMarkerSuffix), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runningBuilds(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(liveCopy); err != nil {
		t.Errorf("copy of the live build removed: %v", err)
	}
	for _, d := range []string{deadCopy, unlockedCopy} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("stale copy %s not removed: %v", d, err)
		}
	}
}

// TestBuildPolicy: the signature policy of a Linux build rejects every
// transport except a registry and the local store.
func TestBuildPolicy(t *testing.T) {
	var policy struct {
		Default []struct {
			Type string `json:"type"`
		} `json:"default"`
		Transports map[string]map[string][]struct {
			Type string `json:"type"`
		} `json:"transports"`
	}
	if err := json.Unmarshal([]byte(BuildPolicy), &policy); err != nil {
		t.Fatalf("BuildPolicy is not JSON: %v", err)
	}
	if len(policy.Default) != 1 || policy.Default[0].Type != "reject" {
		t.Errorf("default = %+v, want [reject]", policy.Default)
	}
	var names []string
	for name, scopes := range policy.Transports {
		names = append(names, name)
		if len(scopes) != 1 || len(scopes[""]) != 1 || scopes[""][0].Type != "insecureAcceptAnything" {
			t.Errorf("transport %s = %+v, want one default scope that accepts", name, scopes)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"containers-storage", "docker"}) {
		t.Errorf("accepted transports = %v, want [containers-storage docker]", names)
	}
	for _, refused := range buildRefusedTransports() {
		// The build commits to the local store and reads local images
		// from it, so the policy must accept containers-storage. The
		// check refuses only a Containerfile that names it.
		if refused == "containers-storage" {
			continue
		}
		if _, ok := policy.Transports[refused]; ok {
			t.Errorf("the policy accepts the refused transport %s", refused)
		}
	}
}

// TestScopeExecutor_RuntimeMax: the scope stops on its own at the request
// deadline plus the grace time, so a build stops when prism dies first.
func TestScopeExecutor_RuntimeMax(t *testing.T) {
	e, dir := fakeSystemd(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := e.Build(ctx, "u", &bytes.Buffer{}, []string{"build", stageContext(t)}); err != nil {
		t.Fatal(err)
	}
	args := readLines(t, filepath.Join(dir, "systemd-run.args"))
	var got string
	for i, a := range args {
		if a == "--" {
			break
		}
		if a == "--property" && strings.HasPrefix(args[i+1], "RuntimeMaxSec=") {
			got = args[i+1]
		}
	}
	if got != "RuntimeMaxSec=1860" && got != "RuntimeMaxSec=1859" {
		t.Errorf("RuntimeMaxSec property = %q, want 1860 (30m plus 60s)", got)
	}
}
