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
// dir. The fake systemd-run records its arguments, records the podman
// argument vector that follows the scope script, and then acts as podman:
// it prints dir/output, exits with the code in dir/code, or hangs
// when dir/hang exists. The fake systemctl logs each call, prints dir/units
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
printf '%s\n' "$@" > "$d/podman.args"
[ -f "$d/output" ] && cat "$d/output"
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
	return ScopeExecutor{SystemdRun: run, Systemctl: ctl}, dir
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
	code, err := e.Build(context.Background(), "prism-build-tok-0001", &out, []string{"build", "--file", "/f", "/ctx"})
	if code != 0 || err != nil || out.String() != "STEP 1/1\n" {
		t.Fatalf("Build = %d, %v, output %q", code, err, out.String())
	}
	want := []string{
		"--user", "--scope", "--collect", "--quiet",
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
	if podman := readLines(t, filepath.Join(dir, "podman.args")); !slices.Equal(podman, []string{"build", "--signature-policy", policy, "--file", "/f", "/ctx"}) {
		t.Errorf("podman args = %q", podman)
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
	if code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", "/ctx"}); code != 3 || err != nil {
		t.Errorf("Build = %d, %v; want 3, nil", code, err)
	}
}

func TestScopeExecutor_KilledBySignalIsAnError(t *testing.T) {
	e, dir := fakeSystemd(t)
	if err := os.WriteFile(filepath.Join(dir, "signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", "/ctx"})
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
	_, err := e.Build(ctx, "prism-build-tok-0002", &bytes.Buffer{}, []string{"build", "/ctx"})
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
	code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", "/ctx"})
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
	if code, err := e.Build(context.Background(), "u", &bytes.Buffer{}, []string{"build", "/ctx"}); code != 0 || err != nil {
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
// temp dir and a fake podman. The script moves itself into a leaf cgroup
// and gives podman a --cgroup-parent inside the scope.
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
	bin := t.TempDir()
	writeScript(t, bin, "podman", `printf '%s\n' "$@"`+"\n")
	script := strings.NewReplacer("/proc/self/cgroup", cgFile, "/sys/fs/cgroup", cgRoot).Replace(scopeScript)

	cmd := exec.Command("/bin/sh", "-c", script, "prism-container-build", "build", "--file", "/f", "--build-arg", "A=$(id)", "/ctx")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	want := []string{"build", "--cgroup-parent", scope + "/build", "--file", "/f", "--build-arg", "A=$(id)", "/ctx"}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); !slices.Equal(got, want) {
		t.Errorf("podman args = %q, want %q", got, want)
	}
	procs, err := os.ReadFile(filepath.Join(cgRoot+scope, "podman", "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(procs)) == "" {
		t.Errorf("the script did not move itself into the leaf cgroup: %q, %v", procs, err)
	}

	if err := os.WriteFile(cgFile, []byte("1:name=systemd:/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("/bin/sh", "-c", script, "prism-container-build", "build", "/ctx")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "no cgroup v2 path") {
		t.Errorf("no cgroup v2 line: err = %v, output %q; want a refusal before podman", err, out)
	}
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
	if _, ok := DefaultBuildExecutor("linux", nil).(ScopeExecutor); !ok {
		t.Errorf("linux executor is not ScopeExecutor")
	}
	if _, ok := DefaultBuildExecutor("darwin", nil).(PlainExecutor); !ok {
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
