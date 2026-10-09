package prismcontainer_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

const testInstanceID = "0f0e0d0c-0b0a-4908-8706-050403020100"

// newCaller returns a caller with a real worktree directory and points the
// prism state dir at a temp dir.
func newCaller(t *testing.T) prismcontainer.Caller {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return prismcontainer.Caller{
		SessionName: "prism-test@container-run",
		InstanceID:  testInstanceID,
		Worktree:    prismcontainertest.RealTempDir(t),
	}
}

func deps(f *prismcontainertest.Fake) prismcontainer.Deps {
	return prismcontainer.Deps{Runner: f, HostLimit: 4, GOOS: "linux", LockWait: time.Second}
}

func readAudit(t *testing.T, instanceID string) []map[string]any {
	t.Helper()
	path, err := container.PrismContainerAuditLogPath(instanceID)
	if err != nil {
		t.Fatalf("audit path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("audit line %q is not JSON: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

func optionValue(args []string, flag string) []string {
	var vals []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			vals = append(vals, args[i+1])
		}
	}
	return vals
}

func TestRun_EchoHello(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.OnRun = func(_ context.Context, stdout, _ io.Writer, args []string) (int, error) {
		cmd := args[slices.Index(args, "docker.io/library/alpine")+1:]
		_, _ = io.WriteString(stdout, strings.Join(cmd[1:], " ")+"\n")
		return 0, nil
	}

	res := prismcontainer.Run(context.Background(), deps(f), c,
		prismcontainer.RunRequest{Image: "alpine", Command: []string{"echo", "hello"}})

	if res.ExitCode != 0 || string(res.Stdout) != "hello\n" || res.Message != "" {
		t.Fatalf("result = %+v, want exit 0, stdout hello", res)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("containers left after run: %+v", left)
	}
}

func TestRun_ExitCodeAndStreams(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.OnRun = func(_ context.Context, stdout, stderr io.Writer, _ []string) (int, error) {
		_, _ = io.WriteString(stdout, "to-stdout")
		_, _ = io.WriteString(stderr, "to-stderr")
		return 3, nil
	}
	res := prismcontainer.Run(context.Background(), deps(f), c,
		prismcontainer.RunRequest{Image: "alpine", Command: []string{"sh", "-c", "exit 3"}})
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "to-stdout" || string(res.Stderr) != "to-stderr" {
		t.Errorf("stdout = %q, stderr = %q", res.Stdout, res.Stderr)
	}
}

// TestRun_ArgvFixedOptions checks every fixed option for every agent input
// shape, including inputs that look like podman options.
func TestRun_ArgvFixedOptions(t *testing.T) {
	requests := []prismcontainer.RunRequest{
		{Image: "alpine"},
		{Image: "alpine", Mount: prismcontainer.MountNone},
		{Image: "alpine", Mount: prismcontainer.MountRW, Env: []string{"A=--privileged"}},
		{Image: "alpine", Command: []string{"--privileged", "--cap-add=ALL", "--device=/dev/kvm", "--network=host", "-v", "/:/host", "--rm=false"}},
		{Image: "alpine", Env: []string{"X=--pid=host", "Y=-v /:/h"}, TimeoutSeconds: 3600},
	}
	for i, req := range requests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c := newCaller(t)
			f := &prismcontainertest.Fake{ImagePresent: true}
			if res := prismcontainer.Run(context.Background(), deps(f), c, req); res.ExitCode != 0 {
				t.Fatalf("run: %+v", res)
			}
			runs := f.RunCalls()
			if len(runs) != 1 {
				t.Fatalf("run calls = %d, want 1", len(runs))
			}
			args := runs[0]
			imageAt := slices.Index(args, "docker.io/library/alpine")
			if imageAt < 0 {
				t.Fatalf("image not in argv %q", args)
			}
			opts := args[:imageAt]
			if got := args[imageAt+1:]; !slices.Equal(got, req.Command) && !(len(got) == 0 && len(req.Command) == 0) {
				t.Errorf("command after image = %q, want %q unchanged", got, req.Command)
			}

			if opts[0] != "run" || !slices.Contains(opts, "--rm") {
				t.Errorf("argv does not start with run --rm: %q", opts)
			}
			want := map[string]string{
				"--memory":       prismcontainer.MemoryLimit,
				"--memory-swap":  prismcontainer.MemoryLimit,
				"--cpus":         prismcontainer.CPULimit,
				"--pids-limit":   prismcontainer.PidsLimit,
				"--security-opt": "no-new-privileges",
				"--label":        prismcontainer.LabelInstanceID + "=" + testInstanceID,
			}
			for flag, val := range want {
				if got := optionValue(opts, flag); !slices.Equal(got, []string{val}) {
					t.Errorf("%s = %q, want exactly [%q]", flag, got, val)
				}
			}
			prefix := container.ResourceNamePrefixForOwner(testInstanceID, c.SessionName)
			if names := optionValue(opts, "--name"); len(names) != 1 || !strings.HasPrefix(names[0], prefix) {
				t.Errorf("--name = %q, want one name with prefix %q", names, prefix)
			}

			for _, o := range opts {
				for _, bad := range []string{"--privileged", "--cap-add", "--device", "--network", "--net", "--pid", "--ipc", "--uts", "--userns", "--cgroupns", "--mount", "--volumes-from", "--env-host", "--env-file"} {
					if o == bad || strings.HasPrefix(o, bad+"=") {
						t.Errorf("option section holds %q: %q", o, opts)
					}
				}
			}
			for _, vol := range optionValue(opts, "--volume") {
				src, _, _ := strings.Cut(vol, ":")
				if src != c.Worktree {
					t.Errorf("--volume source %q is not the session worktree %q", src, c.Worktree)
				}
			}
			for _, e := range optionValue(opts, "--env") {
				if !slices.Contains(req.Env, e) {
					t.Errorf("--env %q is not an agent KEY=VALUE input", e)
				}
			}
		})
	}
}

func TestRun_Mounts(t *testing.T) {
	cases := map[prismcontainer.Mount]string{
		"":                       ":/workspace:ro",
		prismcontainer.MountRO:   ":/workspace:ro",
		prismcontainer.MountRW:   ":/workspace:rw",
		prismcontainer.MountNone: "",
	}
	for mount, suffix := range cases {
		t.Run(string(mount), func(t *testing.T) {
			c := newCaller(t)
			f := &prismcontainertest.Fake{ImagePresent: true}
			prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine", Mount: mount})
			opts := f.RunCalls()[0]
			vols := optionValue(opts, "--volume")
			workdir := optionValue(opts, "--workdir")
			if suffix == "" {
				if len(vols) != 0 || len(workdir) != 0 {
					t.Errorf("--mount none: volumes %q, workdir %q, want none", vols, workdir)
				}
				return
			}
			if !slices.Equal(vols, []string{c.Worktree + suffix}) || !slices.Equal(workdir, []string{"/workspace"}) {
				t.Errorf("volumes %q workdir %q, want [%s] and [/workspace]", vols, workdir, c.Worktree+suffix)
			}
		})
	}
}

func TestRun_RefusalsBeforePodman(t *testing.T) {
	cases := map[string]prismcontainer.RunRequest{
		"dash image":  {Image: "-v"},
		"bad env key": {Image: "alpine", Env: []string{"A-B=1"}},
		"long":        {Image: "alpine", TimeoutSeconds: 3601},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCaller(t)
			f := &prismcontainertest.Fake{ImagePresent: true}
			res := prismcontainer.Run(context.Background(), deps(f), c, req)
			if res.ExitCode == 0 || !strings.HasPrefix(res.Message, "refused: ") {
				t.Errorf("result = %+v, want a refusal", res)
			}
			if calls := f.Calls(); len(calls) != 0 {
				t.Errorf("podman called before the refusal: %q", calls)
			}
			lines := readAudit(t, testInstanceID)
			if len(lines) != 1 || lines[0]["decision"] != prismcontainer.DecisionRefused {
				t.Errorf("audit = %v, want one refused line", lines)
			}
		})
	}
}

func TestRun_NoInstanceIDRefused(t *testing.T) {
	c := newCaller(t)
	c.InstanceID = "not-a-uuid"
	f := &prismcontainertest.Fake{ImagePresent: true}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode == 0 || !strings.Contains(res.Message, "instance ID") {
		t.Errorf("result = %+v, want refusal naming the instance ID", res)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("podman called: %q", f.Calls())
	}
}

func TestRun_PerSessionLimit(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.Add(prismcontainertest.Container{Name: "x", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})

	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode == 0 || !strings.Contains(res.Message, "per-session limit is 1") {
		t.Errorf("result = %+v, want refusal naming the per-session limit", res)
	}
	for _, call := range f.Calls() {
		if call[0] != "ps" {
			t.Errorf("podman %q called after the limit refusal", call)
		}
	}
}

func TestRun_HostLimit(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	for i := range 4 {
		f.Add(prismcontainertest.Container{Name: fmt.Sprint(i), State: "running",
			Labels: map[string]string{prismcontainer.LabelInstanceID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i)}})
	}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode == 0 || !strings.Contains(res.Message, "host-wide limit is 4") ||
		!strings.Contains(res.Message, prismcontainer.HostLimitNixOption) {
		t.Errorf("result = %+v, want refusal naming the limit and %s", res, prismcontainer.HostLimitNixOption)
	}
	if len(f.RunCalls()) != 0 {
		t.Errorf("podman run called after refusal")
	}
}

func TestRun_ExitedContainersDoNotCount(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.Add(prismcontainertest.Container{Name: "old", State: "exited", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode != 0 {
		t.Errorf("result = %+v, want the run to pass the limit check", res)
	}
}

func TestRun_Timeout(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.OnRun = func(ctx context.Context, _, _ io.Writer, _ []string) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine", TimeoutSeconds: 1})
	if res.ExitCode != prismcontainer.ExitTimeout || !strings.Contains(res.Message, "--timeout of 1s") {
		t.Errorf("result = %+v, want exit %d and a message naming the timeout", res, prismcontainer.ExitTimeout)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("container left after timeout: %+v", left)
	}
}

func TestRun_PullFailure(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{PullCode: 125, PullStderr: "Error: initializing source docker://docker.io/library/nope:latest: requested access to the resource is denied\n"}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "nope"})
	if res.ExitCode != 125 || !strings.Contains(string(res.Stderr), "requested access to the resource is denied") {
		t.Errorf("result = %+v, want exit 125 with the pull error on stderr", res)
	}
	if len(f.RunCalls()) != 0 || len(f.Containers()) != 0 {
		t.Errorf("a container was started after a failed pull")
	}
}

func TestRun_PodmanUnreachable(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin": "podman machine start",
		"linux":  "systemctl --user status podman.socket",
	} {
		t.Run(goos, func(t *testing.T) {
			c := newCaller(t)
			f := &prismcontainertest.Fake{PsErr: errors.New("Cannot connect to Podman")}
			d := deps(f)
			d.GOOS = goos
			res := prismcontainer.Run(context.Background(), d, c, prismcontainer.RunRequest{Image: "alpine"})
			if res.ExitCode == 0 || !strings.Contains(res.Message, want) {
				t.Errorf("message = %q, want it to contain %q", res.Message, want)
			}
		})
	}
}

func TestRun_OutputTruncated(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.OnRun = func(_ context.Context, stdout, _ io.Writer, _ []string) (int, error) {
		_, _ = stdout.Write(bytes.Repeat([]byte("a"), 10))
		_, _ = stdout.Write(bytes.Repeat([]byte("b"), prismcontainer.OutputLimit))
		return 0, nil
	}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if len(res.Stdout) != prismcontainer.OutputLimit || res.StdoutDropped != 10 || res.Stdout[0] != 'b' {
		t.Fatalf("stdout len %d dropped %d, want the last 1 MiB and 10 dropped", len(res.Stdout), res.StdoutDropped)
	}
	var stdout, stderr bytes.Buffer
	prismcontainer.WriteResult(&stdout, &stderr, res)
	if !strings.Contains(stderr.String(), "stdout was truncated") {
		t.Errorf("stderr = %q, want a truncation line", stderr.String())
	}
	if stdout.Len() != prismcontainer.OutputLimit {
		t.Errorf("stdout written = %d bytes, want %d", stdout.Len(), prismcontainer.OutputLimit)
	}
}

func TestRun_AuditLine(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	f.OnRun = func(context.Context, io.Writer, io.Writer, []string) (int, error) { return 7, nil }
	prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{
		Image: "alpine", Command: []string{"true"}, Env: []string{"TOKEN=s3cr3t-value"},
	})

	lines := readAudit(t, testInstanceID)
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	l := lines[0]
	if l["command"] != "run" || l["image"] != "docker.io/library/alpine" || l["decision"] != prismcontainer.DecisionAllowed || l["exit_code"] != float64(7) {
		t.Errorf("audit line = %v", l)
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(l["time"])); err != nil {
		t.Errorf("audit time %v: %v", l["time"], err)
	}
	path, _ := container.PrismContainerAuditLogPath(testInstanceID)
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("s3cr3t-value")) {
		t.Errorf("audit log holds an --env value: %s", raw)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("audit dir mode = %v (%v), want 0700", info.Mode().Perm(), err)
	}
}

func TestSweepInstances(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	f := &prismcontainertest.Fake{}
	f.Add(prismcontainertest.Container{ID: "mine1", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	f.Add(prismcontainertest.Container{ID: "mine2", State: "exited", Labels: map[string]string{prismcontainer.LabelInstanceID: other}})
	f.Add(prismcontainertest.Container{ID: "theirs", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: "99999999-2222-4333-8444-555555555555"}})
	f.Add(prismcontainertest.Container{ID: "unlabelled", State: "running", Labels: map[string]string{}})

	n, err := prismcontainer.SweepInstances(context.Background(), f, []string{testInstanceID, other})
	if err != nil || n != 2 {
		t.Fatalf("SweepInstances = %d, %v; want 2, nil", n, err)
	}
	var left []string
	for _, c := range f.Containers() {
		left = append(left, c.ID)
	}
	if !slices.Equal(left, []string{"theirs", "unlabelled"}) {
		t.Errorf("left = %v, want [theirs unlabelled]", left)
	}
}

// TestRun_SymlinkWorktreeRefused: a worktree path that is a symlink is
// refused before podman runs, for both mount modes that mount it.
func TestRun_SymlinkWorktreeRefused(t *testing.T) {
	for _, mount := range []prismcontainer.Mount{prismcontainer.MountRO, prismcontainer.MountRW} {
		t.Run(string(mount), func(t *testing.T) {
			c := newCaller(t)
			link := filepath.Join(t.TempDir(), "worktree")
			if err := os.Symlink(c.Worktree, link); err != nil {
				t.Fatal(err)
			}
			c.Worktree = link
			f := &prismcontainertest.Fake{ImagePresent: true}
			res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine", Mount: mount})
			if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "symlink is refused") {
				t.Errorf("result = %+v, want a refusal naming the symlink", res)
			}
			if len(f.Calls()) != 0 {
				t.Errorf("podman called: %q", f.Calls())
			}
		})
	}
}

// TestRun_CIDFileInStateDir: podman writes the cidfile on the host and
// follows a symlink at its path, so the file must be in the prism-container
// state tree, which no sandbox can write.
func TestRun_CIDFileInStateDir(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	cidDir, err := container.PrismContainerCIDDirPath()
	if err != nil {
		t.Fatal(err)
	}
	cids := optionValue(f.RunCalls()[0], "--cidfile")
	if len(cids) != 1 || !strings.HasPrefix(cids[0], cidDir+string(filepath.Separator)) {
		t.Errorf("--cidfile = %q, want one path under %s", cids, cidDir)
	}
	if _, err := os.Stat(filepath.Dir(cids[0])); !os.IsNotExist(err) {
		t.Errorf("per-run cid dir still exists after the run: %v", err)
	}
}

// TestRun_SymlinkParentRefused: a symlink in a parent directory of the
// worktree redirects the mount source just as a symlink at the worktree
// does, so prism refuses it.
func TestRun_SymlinkParentRefused(t *testing.T) {
	c := newCaller(t)
	link := filepath.Join(prismcontainertest.RealTempDir(t), "bare")
	if err := os.Symlink(filepath.Dir(c.Worktree), link); err != nil {
		t.Fatal(err)
	}
	c.Worktree = filepath.Join(link, filepath.Base(c.Worktree))
	f := &prismcontainertest.Fake{ImagePresent: true}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "goes through a symlink") {
		t.Errorf("result = %+v, want a refusal naming the symlink", res)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("podman called: %q", f.Calls())
	}
}

// TestRun_WorktreeSwappedDuringPullRefused: prism checks the worktree path
// again under the host lock, so a swap during the pull is refused before
// podman run.
func TestRun_WorktreeSwappedDuringPullRefused(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{}
	f.OnPull = func() {
		if err := os.Rename(c.Worktree, c.Worktree+".moved"); err != nil {
			t.Errorf("rename: %v", err)
		}
		if err := os.Symlink(t.TempDir(), c.Worktree); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "symlink is refused") {
		t.Errorf("result = %+v, want a refusal after the swap", res)
	}
	if len(f.RunCalls()) != 0 {
		t.Errorf("podman run called after the worktree swap")
	}
}
