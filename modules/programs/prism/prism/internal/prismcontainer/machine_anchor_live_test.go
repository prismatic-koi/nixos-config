//go:build darwin

package prismcontainer_test

// Live tests of the macOS anchor executor against the podman machine of
// the Mac. They run only when PRISM_CONTAINER_INTEGRATION=1 is set:
//
//	PRISM_CONTAINER_INTEGRATION=1 go test ./internal/prismcontainer -run MachineAnchor -v
//
// The go-tests-container-live CI job runs on Linux, so these tests are not
// in its -run selector.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

func machineDeps() prismcontainer.Deps {
	return prismcontainer.Deps{MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist()}
}

// vmShell runs script in the podman machine and returns its output.
func vmShell(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("podman", "machine", "ssh", script).CombinedOutput()
	if err != nil {
		t.Fatalf("podman machine ssh %q: %v: %s", script, err, out)
	}
	return string(out)
}

// waitStep waits until a process whose command line holds marker runs in
// the VM, and returns its PID.
func waitStep(t *testing.T, marker string, done <-chan prismcontainer.BuildResult) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if found := buildStepProcs(t, marker); len(found) > 0 {
			return strings.Fields(found[0])[0]
		}
		select {
		case res := <-done:
			t.Fatalf("the build ended before its RUN step ran: %+v (output %s)", res, res.Output)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the RUN step did not start within 3 minutes")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestMachineAnchor_StepInScopeWithLimits: a RUN step runs in
// <anchor scope>/build, and the scope has the fixed limits.
func TestMachineAnchor_StepInScopeWithLimits(t *testing.T) {
	c := integrationCaller(t)
	const marker = "619.211"
	if err := os.WriteFile(filepath.Join(c.Worktree, "Containerfile"), []byte("FROM alpine\nRUN sleep "+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := prismcontainer.DefaultBuildExecutor("darwin", nil, nil)
	t.Cleanup(func() {
		_ = prismcontainer.SweepSession(context.Background(), prismcontainer.ExecRunner{}, ex, []string{c.InstanceID})
	})
	done := make(chan prismcontainer.BuildResult, 1)
	go func() {
		done <- prismcontainer.Build(context.Background(), machineDeps(), c, prismcontainer.BuildRequest{TimeoutSeconds: 300})
	}()
	pid := waitStep(t, marker, done)

	cg := strings.TrimSpace(vmShell(t, "sed -n 's/^0:://p' /proc/"+pid+"/cgroup"))
	if !regexp.MustCompile(`/libpod-[0-9a-f]{64}\.scope/build$`).MatchString(cg) {
		t.Fatalf("the RUN step is in cgroup %q, want <anchor scope>/build", cg)
	}
	scope := strings.TrimSuffix(cg, "/build")
	if got := vmShell(t, "cd /sys/fs/cgroup"+scope+" && cat pids.max memory.max cpu.max"); got != "1024\n4294967296\n200000 100000\n" {
		t.Errorf("limits of the anchor scope = %q, want pids 1024, memory 4 GiB, and 2 CPUs", got)
	}
	out, err := exec.Command("podman", "ps", "--filter", "label="+prismcontainer.LabelInstanceID+"="+c.InstanceID,
		"--filter", "label="+prismcontainer.LabelBuild, "--format", "{{.ID}}").Output()
	if err != nil || len(strings.Fields(string(out))) != 1 || !strings.Contains(scope, strings.TrimSpace(string(out))) {
		t.Errorf("anchors of the session = %q (%v), want the one anchor whose scope holds the step", out, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := prismcontainer.SweepSession(ctx, prismcontainer.ExecRunner{}, ex, []string{c.InstanceID}); err != nil {
		t.Errorf("SweepSession: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the build did not end within a minute after the sweep")
	}
	if found := buildStepProcs(t, marker); len(found) != 0 {
		t.Errorf("processes of the build step still run after the sweep: %q", found)
	}
}

func forkContainerfile(n int) string {
	return fmt.Sprintf("FROM alpine\nRUN i=0; while [ $i -lt %d ]; do sleep 120 & i=$((i+1)); done; echo spawned=$i\n", n)
}

// TestMachineAnchor_ProcessLimitCountsThisBuildOnly: the process limit of a
// build counts the processes of that build only. Processes of another
// session do not make a fork of the build fail, and the build still has
// its own limit of 1024.
func TestMachineAnchor_ProcessLimitCountsThisBuildOnly(t *testing.T) {
	c := integrationCaller(t)
	other := secondCaller(t, c)
	const holders = 500
	start := startReal(t, other, prismcontainer.StartRequest{Image: "alpine", Mount: prismcontainer.MountNone,
		Command: []string{"sh", "-c", fmt.Sprintf("i=0; while [ $i -lt %d ]; do sleep 600 & i=$((i+1)); done; wait", holders)}})
	if start.ExitCode != 0 {
		t.Fatalf("start = %+v", start)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		n, _ := strconv.Atoi(strings.TrimSpace(vmShell(t, "pgrep -c -f '^sleep 600$' || true")))
		if n >= holders {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the other session holds %d processes, want %d", n, holders)
		}
	}

	// Control: with RLIMIT_NPROC of 1024 for the user, as a build had
	// before the anchor, the same fork count fails now. Otherwise the test
	// does not measure the limit.
	ctrlDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctrlDir, "Containerfile"), []byte(forkContainerfile(900)), 0o644); err != nil {
		t.Fatal(err)
	}
	ctrl, err := exec.Command("podman", "build", "--no-cache", "--ulimit", "nproc=1024:1024", ctrlDir).CombinedOutput()
	if err == nil || !strings.Contains(string(ctrl), "can't fork") {
		t.Fatalf("control: a build with --ulimit nproc=1024:1024 did not fail to fork (%v):\n%s", err, ctrl)
	}

	res := buildReal(t, c, forkContainerfile(900), prismcontainer.BuildRequest{TimeoutSeconds: 300})
	if res.ExitCode != 0 || !strings.Contains(string(res.Output), "spawned=900") {
		t.Errorf("build that forks 900 processes = %+v (output %s), want it to pass", res, res.Output)
	}
	res = buildReal(t, c, forkContainerfile(1100), prismcontainer.BuildRequest{TimeoutSeconds: 300})
	wantBuildFailed(t, res, regexp.MustCompile(`can't fork|Resource temporarily unavailable`))
}

// TestMachineAnchor_PrismDeathStopsBuild: when the prism process of a
// build dies by SIGKILL, the guard stops the podman client, the build
// counts toward the limits while its anchor runs, and no process of the
// build runs after the build timeout plus 60 seconds.
func TestMachineAnchor_PrismDeathStopsBuild(t *testing.T) {
	c := integrationCaller(t)
	const marker, nextStep, timeout = "623.101", "627.101", 15
	if err := os.WriteFile(filepath.Join(c.Worktree, "Containerfile"), []byte("FROM alpine\nRUN sleep "+marker+"\nRUN sleep "+nextStep+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestMachineAnchorHelperProcess$")
	helper.Env = append(os.Environ(),
		"PRISM_ANCHOR_HELPER=1", "PRISM_ANCHOR_HELPER_SESSION="+c.SessionName,
		"PRISM_ANCHOR_HELPER_INSTANCE="+c.InstanceID, "PRISM_ANCHOR_HELPER_WORKTREE="+c.Worktree,
		"PRISM_ANCHOR_HELPER_TIMEOUT="+strconv.Itoa(timeout))
	started := time.Now()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = prismcontainer.SweepSession(context.Background(), prismcontainer.ExecRunner{}, prismcontainer.DefaultBuildExecutor("darwin", nil, nil), []string{c.InstanceID})
	})
	waitStep(t, marker, nil)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	killed := time.Now()

	tag := "localhost/" + container.ResourceNamePrefixRoot + container.InstanceTokenForID(c.InstanceID) + "-"
	time.Sleep(3 * time.Second)
	if out, _ := exec.Command("ps", "-axo", "pid=,command=").Output(); strings.Contains(string(out), tag) {
		t.Errorf("a podman client of the build still runs on the Mac 3 seconds after the SIGKILL")
	}
	run := prismcontainer.Run(context.Background(), prismcontainer.Deps{}, c, prismcontainer.RunRequest{Image: "alpine", Mount: prismcontainer.MountNone, Command: []string{"true"}})
	if run.ExitCode != prismcontainer.ExitRefused || !strings.Contains(run.Message, "per-session limit is 1") {
		t.Errorf("run while the anchor of the dead build runs = %+v, want the per-session refusal", run)
	}

	// The build timeout starts after the helper process starts and copies
	// the context, so the limit has 10 seconds more.
	limit := started.Add((timeout + 60 + 10) * time.Second)
	var goneAt time.Time
	for time.Now().Before(limit) {
		procs := vmShell(t, "ps -eo pid=,args=")
		if strings.Contains(procs, "sleep "+nextStep) {
			t.Fatalf("the next RUN step started after the prism process died:\n%s", procs)
		}
		if !strings.Contains(procs, "sleep "+marker) {
			goneAt = time.Now()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if goneAt.IsZero() {
		t.Fatalf("the RUN step still runs %s after the build started, more than the timeout plus 60 seconds", time.Since(started).Round(time.Second))
	}
	t.Logf("SIGKILL at %s after the start; the RUN step was gone at %s after the start (timeout %ds plus 60s)",
		killed.Sub(started).Round(time.Second), goneAt.Sub(started).Round(time.Second), timeout)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		out, _ := exec.Command("podman", "ps", "-a", "--filter", "label="+prismcontainer.LabelInstanceID+"="+c.InstanceID, "--format", "{{.Names}}").Output()
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the anchor %s remains after its timeout", out)
		}
	}
	if run := prismcontainer.Run(context.Background(), prismcontainer.Deps{}, c, prismcontainer.RunRequest{Image: "alpine", Mount: prismcontainer.MountNone, Command: []string{"true"}}); run.ExitCode != 0 {
		t.Errorf("run after the anchor stopped = %+v, want it to pass the limit check", run)
	}
}

// TestMachineAnchorHelperProcess is the prism process of
// TestMachineAnchor_PrismDeathStopsBuild. It does nothing in a normal test
// run.
func TestMachineAnchorHelperProcess(t *testing.T) {
	if os.Getenv("PRISM_ANCHOR_HELPER") != "1" {
		return
	}
	timeout, _ := strconv.ParseInt(os.Getenv("PRISM_ANCHOR_HELPER_TIMEOUT"), 10, 64)
	c := prismcontainer.Caller{
		SessionName: os.Getenv("PRISM_ANCHOR_HELPER_SESSION"),
		InstanceID:  os.Getenv("PRISM_ANCHOR_HELPER_INSTANCE"),
		Worktree:    os.Getenv("PRISM_ANCHOR_HELPER_WORKTREE"),
	}
	res := prismcontainer.Build(context.Background(), machineDeps(), c, prismcontainer.BuildRequest{TimeoutSeconds: timeout})
	fmt.Printf("%+v\n", res)
	os.Exit(0)
}
