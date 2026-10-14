package prismcontainer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

const otherInstanceID = "99999999-2222-4333-8444-555555555555"

// startService starts a service container in f and returns its result.
func startService(t *testing.T, f *prismcontainertest.Fake, c prismcontainer.Caller, ports ...string) prismcontainer.StartResult {
	t.Helper()
	res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{
		Image: "postgres:16", Env: []string{"POSTGRES_PASSWORD=x"}, Ports: ports,
	})
	if res.ExitCode != 0 {
		t.Fatalf("start = %+v", res)
	}
	return res
}

func onlyContainer(t *testing.T, f *prismcontainertest.Fake) prismcontainertest.Container {
	t.Helper()
	cs := f.Containers()
	if len(cs) != 1 {
		t.Fatalf("containers = %+v, want one", cs)
	}
	return cs[0]
}

func callsOf(f *prismcontainertest.Fake, sub string) [][]string {
	var out [][]string
	for _, c := range f.Calls() {
		if len(c) > 0 && c[0] == sub {
			out = append(out, c)
		}
	}
	return out
}

func TestStart_PrintsHandleAndPortAndKeepsRunning(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	res := startService(t, f, c, "5432")

	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(res.Handle) {
		t.Errorf("handle = %q, want 8 hex digits", res.Handle)
	}
	if len(res.Ports) != 1 || res.Ports[0].ContainerPort != 5432 || res.Ports[0].HostIP != "127.0.0.1" || res.Ports[0].HostPort < 1 {
		t.Fatalf("ports = %+v, want 5432 on 127.0.0.1", res.Ports)
	}
	ctr := onlyContainer(t, f)
	if ctr.Labels[prismcontainer.LabelInstanceID] != testInstanceID || ctr.Labels[prismcontainer.LabelService] != res.Handle {
		t.Errorf("labels = %v", ctr.Labels)
	}
	if !strings.HasPrefix(ctr.Name, "prism-") || !strings.HasSuffix(ctr.Name, "-"+res.Handle) {
		t.Errorf("name = %q, want the session prefix and the handle", ctr.Name)
	}

	var stdout, stderr bytes.Buffer
	prismcontainer.WriteStartResult(&stdout, &stderr, res)
	want := fmt.Sprintf("handle %s\nport 5432 127.0.0.1:%d\n", res.Handle, res.Ports[0].HostPort)
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	lines := readAudit(t, testInstanceID)
	last := lines[len(lines)-1]
	if last["command"] != "start" || last["decision"] != "allowed" || last["handle"] != res.Handle || last["image"] != "docker.io/library/postgres:16" {
		t.Errorf("audit line = %v", last)
	}
	if strings.Contains(fmt.Sprint(last), "POSTGRES_PASSWORD=x") {
		t.Errorf("audit line holds an --env value: %v", last)
	}
}

// TestStart_ArgvFixedOptions checks the fixed options of a service
// container and that every port binds to 127.0.0.1 only.
func TestStart_ArgvFixedOptions(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{
		Image: "postgres:16", Ports: []string{"5432", "8080"}, Env: []string{"A=--privileged"},
		Command: []string{"--privileged", "--cap-add=ALL", "-p", "0.0.0.0:1:1"},
	})
	runs := f.RunCalls()
	if len(runs) != 1 {
		t.Fatalf("run calls = %q", runs)
	}
	args := runs[0]
	img := slices.Index(args, "docker.io/library/postgres:16")
	if img < 0 {
		t.Fatalf("argv has no image: %q", args)
	}
	opts := args[:img]
	for flag, want := range map[string][]string{
		"--memory":       {"4g"},
		"--memory-swap":  {"4g"},
		"--cpus":         {"2"},
		"--pids-limit":   {"1024"},
		"--security-opt": {"no-new-privileges"},
		"--pull":         {"never"},
		"--publish":      {"127.0.0.1::5432/tcp", "127.0.0.1::8080/tcp"},
		"--volume":       {c.Worktree + ":/workspace:ro"},
	} {
		if got := optionValue(opts, flag); !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if !slices.Contains(opts, "--detach") {
		t.Errorf("argv has no --detach: %q", opts)
	}
	for _, banned := range []string{"--rm", "--timeout", "--privileged", "--cap-add", "--device", "--user", "--network", "--pid", "--ipc", "--uts", "-p"} {
		for _, a := range opts {
			if a == banned || strings.HasPrefix(a, banned+"=") {
				t.Errorf("options hold %s: %q", banned, opts)
			}
		}
	}
	labels := optionValue(opts, "--label")
	handle := onlyContainer(t, f).Labels[prismcontainer.LabelService]
	wantLabels := []string{
		prismcontainer.LabelInstanceID + "=" + testInstanceID,
		prismcontainer.LabelService + "=" + handle,
		prismcontainer.LabelBuild + "=",
	}
	if handle == "" || !slices.Equal(labels, wantLabels) {
		t.Errorf("labels = %q, want %q with a handle", labels, wantLabels)
	}
	if got := args[img+1:]; !slices.Equal(got, []string{"--privileged", "--cap-add=ALL", "-p", "0.0.0.0:1:1"}) {
		t.Errorf("command = %q", got)
	}
}

// TestStart_PortRefusedBeforePodman checks that a --port value that is not
// an integer from 1 to 65535 is refused before any podman command.
func TestStart_PortRefusedBeforePodman(t *testing.T) {
	for _, port := range []string{"0", "65536", "99999", "-1", "abc", "5432/udp", "1.5", " 80", "+80", "080", "", "0x50", "127.0.0.1:5432"} {
		t.Run(port, func(t *testing.T) {
			c := newCaller(t)
			f := &prismcontainertest.Fake{ImagePresent: true}
			res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine", Ports: []string{port}})
			if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "an integer from 1 to 65535") {
				t.Errorf("result = %+v, want the --port refusal", res)
			}
			if calls := f.Calls(); len(calls) != 0 {
				t.Errorf("podman ran: %q", calls)
			}
			if lines := readAudit(t, testInstanceID); lines[0]["decision"] != "refused" {
				t.Errorf("audit = %v", lines)
			}
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		c := newCaller(t)
		f := &prismcontainertest.Fake{ImagePresent: true}
		res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine", Ports: []string{"80", "80"}})
		if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "more than once") || len(f.Calls()) != 0 {
			t.Errorf("result = %+v, calls %q", res, f.Calls())
		}
	})
	t.Run("valid edges", func(t *testing.T) {
		c := newCaller(t)
		f := &prismcontainertest.Fake{ImagePresent: true}
		res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine", Ports: []string{"1", "65535"}})
		if res.ExitCode != 0 || len(res.Ports) != 2 {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestStart_RefusedWhileSessionContainerRuns(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	startService(t, f, c)
	res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine"})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "per-session limit is 1") {
		t.Errorf("second start = %+v, want the per-session refusal", res)
	}
	if n := len(f.RunCalls()); n != 1 {
		t.Errorf("podman run calls = %d, want 1", n)
	}
	// A run container counts too: the service holds the one place.
	run := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if run.ExitCode != prismcontainer.ExitRefused || !strings.Contains(run.Message, "per-session limit") ||
		!strings.Contains(run.Message, "prism container list") || !strings.Contains(run.Message, "prism container stop HANDLE") {
		t.Errorf("run while a service runs = %+v, want the per-session refusal with the stop step", run)
	}
}

func TestStart_HostLimitNamesNixOption(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	for i := 0; i < 4; i++ {
		f.Add(prismcontainertest.Container{Name: fmt.Sprint(i), State: "running",
			Labels: map[string]string{prismcontainer.LabelInstanceID: fmt.Sprintf("%08d-2222-4333-8444-555555555555", i)}})
	}
	res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine"})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "host-wide limit is 4") ||
		!strings.Contains(res.Message, prismcontainer.HostLimitNixOption) {
		t.Errorf("result = %+v, want the host-wide refusal with the Nix option", res)
	}
}

// TestStart_RemovesContainerOnWrongBinding checks the check after the
// start: a port that podman reports on an address other than 127.0.0.1
// removes the container.
func TestStart_RemovesContainerOnWrongBinding(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true, PublishIP: "0.0.0.0"}
	res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine", Ports: []string{"80"}})
	if res.ExitCode != prismcontainer.ExitRefused || res.Handle != "" || !strings.Contains(res.Message, `host address "0.0.0.0", not on 127.0.0.1`) {
		t.Errorf("result = %+v", res)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("containers left = %+v", left)
	}
}

func TestStart_PodmanRunFailureRemovesContainer(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.OnRun = func(_ context.Context, _, stderr io.Writer, _ []string) (int, error) {
		_, _ = io.WriteString(stderr, "Error: something\n")
		return 126, nil
	}
	res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine"})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "podman run exited 126") || string(res.Stderr) != "Error: something\n" {
		t.Errorf("result = %+v", res)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("containers left = %+v", left)
	}
}

func TestExec_RunsInServiceContainer(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	h := startService(t, f, c, "5432").Handle
	f.OnExec = func(_ context.Context, stdout, stderr io.Writer, args []string) (int, error) {
		_, _ = io.WriteString(stdout, strings.Join(args[2:], " ")+"\n")
		_, _ = io.WriteString(stderr, "err\n")
		return 3, nil
	}
	// The service holds the one per-session place. exec is not refused.
	res := prismcontainer.Exec(context.Background(), deps(f), c, prismcontainer.ExecRequest{Handle: h, Command: []string{"pg_isready", "-U", "postgres"}})
	if res.ExitCode != 3 || string(res.Stdout) != "pg_isready -U postgres\n" || string(res.Stderr) != "err\n" {
		t.Errorf("exec = %+v (stdout %q)", res, res.Stdout)
	}
	ctr := onlyContainer(t, f)
	execs := callsOf(f, "exec")
	if len(execs) != 1 || !slices.Equal(execs[0], []string{"exec", ctr.ID, "pg_isready", "-U", "postgres"}) {
		t.Errorf("exec argv = %q", execs)
	}
	lines := readAudit(t, testInstanceID)
	last := lines[len(lines)-1]
	if last["command"] != "exec" || last["container"] != ctr.Name || last["exit_code"] != float64(3) {
		t.Errorf("audit line = %v", last)
	}
}

// TestExec_ArgvHoldsNoAgentOption checks that a command that looks like
// podman exec options stays after the container ID, and that prism adds
// no option of its own.
func TestExec_ArgvHoldsNoAgentOption(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	h := startService(t, f, c).Handle
	command := []string{"--privileged", "--user", "root", "--cap-add", "ALL", "--user=0"}
	prismcontainer.Exec(context.Background(), deps(f), c, prismcontainer.ExecRequest{Handle: h, Command: command})
	execs := callsOf(f, "exec")
	if len(execs) != 1 {
		t.Fatalf("exec calls = %q", execs)
	}
	if want := append([]string{"exec", onlyContainer(t, f).ID}, command...); !slices.Equal(execs[0], want) {
		t.Errorf("exec argv = %q, want %q", execs[0], want)
	}

	for _, handle := range []string{"--privileged", "-u", "--user=0"} {
		res := prismcontainer.Exec(context.Background(), deps(f), c, prismcontainer.ExecRequest{Handle: handle, Command: []string{"id"}})
		if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "is not valid") {
			t.Errorf("handle %q: %+v", handle, res)
		}
	}
	if n := len(callsOf(f, "exec")); n != 1 {
		t.Errorf("exec calls = %d after refused handles, want 1", n)
	}
}

// TestServiceOps_RefuseOtherContainers checks exec, logs, and stop against
// every container that is not a service container of the caller: by
// handle, ID, ID prefix, and name. The other container is not changed.
func TestServiceOps_RefuseOtherContainers(t *testing.T) {
	type target struct {
		name, handle, want string
	}
	other := prismcontainertest.Container{ID: "0123456789abcdef0123", Name: "prism-other-svc", State: "running",
		Labels: map[string]string{prismcontainer.LabelInstanceID: otherInstanceID, prismcontainer.LabelService: "cafef00d"}}
	runCtr := prismcontainertest.Container{ID: "fedcba9876543210fedc", Name: "prism-mine-run", State: "running",
		Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}}
	host := prismcontainertest.Container{ID: "aaaaaaaaaaaabbbbbbbb", Name: "postgres", State: "running"}
	emptySvc := prismcontainertest.Container{ID: "abcdef0123456789abcd", Name: "prism-mine-empty-svc", State: "running",
		Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID, prismcontainer.LabelService: ""}}
	targets := []target{
		{"other session handle", "cafef00d", "does not carry the label of this session"},
		{"other session ID", other.ID, "does not carry the label of this session"},
		{"other session ID prefix", other.ID[:12], "does not carry the label of this session"},
		{"other session name", other.Name, "does not carry the label of this session"},
		{"unlabelled container name", "postgres", "does not exist in this session"},
		{"unlabelled container ID", host.ID, "does not exist in this session"},
		{"run container name", runCtr.Name, "was not created by prism container start"},
		{"run container ID", runCtr.ID, "was not created by prism container start"},
		{"empty service label name", emptySvc.Name, "was not created by prism container start"},
		{"empty service label ID", emptySvc.ID, "was not created by prism container start"},
		{"short ID prefix", other.ID[:6], "does not exist in this session"},
	}
	ops := map[string]func(prismcontainer.Deps, prismcontainer.Caller, string) prismcontainer.RunResult{
		"exec": func(d prismcontainer.Deps, c prismcontainer.Caller, h string) prismcontainer.RunResult {
			return prismcontainer.Exec(context.Background(), d, c, prismcontainer.ExecRequest{Handle: h, Command: []string{"id"}})
		},
		"logs": func(d prismcontainer.Deps, c prismcontainer.Caller, h string) prismcontainer.RunResult {
			return prismcontainer.Logs(context.Background(), d, c, prismcontainer.HandleRequest{Handle: h})
		},
		"stop": func(d prismcontainer.Deps, c prismcontainer.Caller, h string) prismcontainer.RunResult {
			return prismcontainer.Stop(context.Background(), d, c, prismcontainer.HandleRequest{Handle: h})
		},
	}
	for opName, op := range ops {
		for _, tg := range targets {
			t.Run(opName+"/"+tg.name, func(t *testing.T) {
				c := newCaller(t)
				f := &prismcontainertest.Fake{}
				f.Add(other)
				f.Add(runCtr)
				f.Add(host)
				f.Add(emptySvc)
				res := op(deps(f), c, tg.handle)
				if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, tg.want) {
					t.Errorf("result = %+v, want a refusal with %q", res, tg.want)
				}
				for _, sub := range []string{"exec", "logs", "rm"} {
					if calls := callsOf(f, sub); len(calls) != 0 {
						t.Errorf("podman %s ran: %q", sub, calls)
					}
				}
				if n := len(f.Containers()); n != 4 {
					t.Errorf("containers = %d, want 4 unchanged", n)
				}
				lines := readAudit(t, testInstanceID)
				if last := lines[len(lines)-1]; last["decision"] != "refused" || last["command"] != opName {
					t.Errorf("audit line = %v", last)
				}
			})
		}
	}
}

// TestServiceOps_RefuseImageLabelledRunContainer: podman copies each image
// label to the container, and the agent controls the labels of an image. A
// run container of an image that carries the labels of prism is still a
// run container of the caller. exec, logs, and stop refuse it.
func TestServiceOps_RefuseImageLabelledRunContainer(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	const forgedHandle = "cafef00d"
	f.AddImage(prismcontainertest.Image{Names: []string{"docker.io/library/forged"}, Labels: map[string]string{
		prismcontainer.LabelInstanceID: otherInstanceID,
		prismcontainer.LabelService:    forgedHandle,
		prismcontainer.LabelBuild:      "prism-build-" + container.InstanceTokenForID(testInstanceID) + "-0000beef",
	}})

	// The checks run while the run container exists. OnRun runs on another
	// goroutine, so it only records, and the test goroutine asserts.
	type outcome struct {
		op, target string
		res        prismcontainer.RunResult
	}
	var (
		live     []prismcontainertest.Container
		list     prismcontainer.ListResult
		outcomes []outcome
		calls    [][]string
	)
	f.OnRun = func(context.Context, io.Writer, io.Writer, []string) (int, error) {
		live = f.Containers()
		if len(live) != 1 {
			return 0, nil
		}
		list = prismcontainer.List(context.Background(), deps(f), c)
		for _, target := range []string{live[0].Name, live[0].ID, forgedHandle} {
			outcomes = append(outcomes,
				outcome{"exec", target, prismcontainer.Exec(context.Background(), deps(f), c, prismcontainer.ExecRequest{Handle: target, Command: []string{"id"}})},
				outcome{"logs", target, prismcontainer.Logs(context.Background(), deps(f), c, prismcontainer.HandleRequest{Handle: target})},
				outcome{"stop", target, prismcontainer.Stop(context.Background(), deps(f), c, prismcontainer.HandleRequest{Handle: target})},
			)
		}
		calls = f.Calls()
		return 0, nil
	}
	if res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "forged", Command: []string{"sleep", "1"}}); res.ExitCode != 0 {
		t.Fatalf("run = %+v", res)
	}
	if len(live) != 1 {
		t.Fatalf("containers during the run = %+v, want one", live)
	}
	ctr := live[0]
	for key, want := range map[string]string{
		prismcontainer.LabelInstanceID: testInstanceID,
		prismcontainer.LabelService:    "",
		prismcontainer.LabelBuild:      "",
	} {
		if got, ok := ctr.Labels[key]; !ok || got != want {
			t.Errorf("label %s = %q (set %v), want %q from the podman command line", key, got, ok, want)
		}
	}
	if len(list.Containers) != 1 || list.Containers[0].Kind != prismcontainer.KindRun || list.Containers[0].Handle != ctr.Name {
		t.Errorf("list = %+v, want the container as kind run", list)
	}
	for _, o := range outcomes {
		want := "was not created by prism container start"
		if o.target == forgedHandle {
			want = "does not exist in this session"
		}
		if o.res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(o.res.Message, want) {
			t.Errorf("%s %s = %+v, want a refusal with %q", o.op, o.target, o.res, want)
		}
	}
	if len(outcomes) != 9 {
		t.Errorf("outcomes = %d, want 9", len(outcomes))
	}
	for _, call := range calls {
		if call[0] == "exec" || call[0] == "logs" || call[0] == "rm" {
			t.Errorf("podman %q ran against the run container", call)
		}
	}
}

func TestStop_RemovesThenExecSaysDoesNotExist(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	h := startService(t, f, c).Handle
	id := onlyContainer(t, f).ID

	res := prismcontainer.Stop(context.Background(), deps(f), c, prismcontainer.HandleRequest{Handle: h})
	if res.ExitCode != 0 || res.Message != "" {
		t.Fatalf("stop = %+v", res)
	}
	if rms := callsOf(f, "rm"); len(rms) != 1 || rms[0][len(rms[0])-1] != id || !slices.Contains(rms[0], "--force") {
		t.Errorf("rm argv = %q", rms)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("containers left = %+v", left)
	}
	exec := prismcontainer.Exec(context.Background(), deps(f), c, prismcontainer.ExecRequest{Handle: h, Command: []string{"id"}})
	if exec.ExitCode != prismcontainer.ExitRefused || !strings.Contains(exec.Message, fmt.Sprintf("service container %q does not exist", h)) {
		t.Errorf("exec after stop = %+v", exec)
	}
}

func TestLogs_PrintsLogs(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{}
	f.Add(prismcontainertest.Container{ID: "svc0123456789", Name: "prism-x-svc", State: "running", Logs: "database system is ready\n",
		Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID, prismcontainer.LabelService: "1234abcd"}})
	res := prismcontainer.Logs(context.Background(), deps(f), c, prismcontainer.HandleRequest{Handle: "1234abcd"})
	if res.ExitCode != 0 || string(res.Stdout) != "database system is ready\n" {
		t.Errorf("logs = %+v", res)
	}
	var stdout, stderr bytes.Buffer
	prismcontainer.WriteCommandResult("logs", &stdout, &stderr, res)
	if stdout.String() != "database system is ready\n" || stderr.Len() != 0 {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestList_ShowsSessionContainersAndImages(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	svc := startService(t, f, c, "5432")
	f.Add(prismcontainertest.Container{ID: "run1", Name: "prism-mine-run", Image: "docker.io/library/alpine:latest", State: "running",
		Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	f.Add(prismcontainertest.Container{ID: "theirs", Name: "prism-theirs", State: "running",
		Labels: map[string]string{prismcontainer.LabelInstanceID: otherInstanceID, prismcontainer.LabelService: "cafef00d"}})
	f.AddImage(prismcontainertest.Image{Names: []string{"localhost/prism-mine-app:latest"}, Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	f.AddImage(prismcontainertest.Image{Names: []string{"localhost/prism-theirs-app:latest"}, Labels: map[string]string{prismcontainer.LabelInstanceID: otherInstanceID}})

	res := prismcontainer.List(context.Background(), deps(f), c)
	if res.ExitCode != 0 || len(res.Containers) != 2 || !slices.Equal(res.Images, []string{"localhost/prism-mine-app:latest"}) {
		t.Fatalf("list = %+v", res)
	}
	var stdout, stderr bytes.Buffer
	prismcontainer.WriteListResult(&stdout, &stderr, res)
	out := stdout.String()
	for _, want := range []string{svc.Handle, "service", "docker.io/library/postgres:16", fmt.Sprintf("5432->127.0.0.1:%d", svc.Ports[0].HostPort),
		"prism-mine-run", "run", "localhost/prism-mine-app:latest"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"cafef00d", "theirs"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("list output holds %q of another session:\n%s", unwanted, out)
		}
	}
}

// TestSweepSession_RemovesServiceContainer checks the cleanup sweep for a
// running service container.
func TestSweepSession_RemovesServiceContainer(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	startService(t, f, c, "5432")
	if err := prismcontainer.SweepSession(context.Background(), f, f, []string{testInstanceID}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("containers left = %+v", left)
	}
}

// TestPodmanRm_AlwaysRemovesVolumes checks every podman rm of prism: stop,
// the remove after a failed start, the remove after a run, and the cleanup
// sweep. Without --volumes, the anonymous volume of an image VOLUME stays
// on the host with no prism label.
func TestPodmanRm_AlwaysRemovesVolumes(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	h := startService(t, f, c).Handle
	prismcontainer.Stop(context.Background(), deps(f), c, prismcontainer.HandleRequest{Handle: h})

	failing := &prismcontainertest.Fake{ImagePresent: true}
	failing.OnRun = func(context.Context, io.Writer, io.Writer, []string) (int, error) { return 126, nil }
	prismcontainer.Start(context.Background(), deps(failing), c, prismcontainer.StartRequest{Image: "alpine"})

	prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	startService(t, f, c)
	if err := prismcontainer.SweepSession(context.Background(), f, f, []string{testInstanceID}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}

	rms := append(callsOf(f, "rm"), callsOf(failing, "rm")...)
	if len(rms) < 4 {
		t.Fatalf("podman rm calls = %q, want stop, start failure, run, and sweep", rms)
	}
	for _, rm := range rms {
		if !slices.Contains(rm, "--volumes") {
			t.Errorf("podman rm without --volumes: %q", rm)
		}
	}
}
