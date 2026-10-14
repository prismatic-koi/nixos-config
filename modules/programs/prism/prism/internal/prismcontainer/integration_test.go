package prismcontainer_test

// Integration tests against the real podman of the host. They run only when
// PRISM_CONTAINER_INTEGRATION=1 is set, because they pull an image from
// Docker Hub and need a reachable podman:
//
//	PRISM_CONTAINER_INTEGRATION=1 go test ./internal/prismcontainer -run Integration -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

func integrationCaller(t *testing.T) prismcontainer.Caller {
	t.Helper()
	if os.Getenv("PRISM_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set PRISM_CONTAINER_INTEGRATION=1 to run against the real podman")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := prismcontainer.Caller{
		SessionName: "prism-test@integration",
		InstanceID:  uuid.New().String(),
		Worktree:    integrationWorktree(t),
	}
	t.Cleanup(func() { assertNoLabelledContainer(t, c.InstanceID) })
	return c
}

func assertNoLabelledContainer(t *testing.T, instanceID string) {
	t.Helper()
	out, err := exec.Command("podman", "ps", "-a", "--filter", "label="+prismcontainer.LabelInstanceID+"="+instanceID, "--format", "{{.Names}}").Output()
	if err != nil {
		t.Errorf("podman ps: %v", err)
		return
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("containers with the label of the run remain: %s", s)
	}
}

func runReal(t *testing.T, c prismcontainer.Caller, req prismcontainer.RunRequest) prismcontainer.RunResult {
	t.Helper()
	if req.Image == "" {
		req.Image = "alpine"
	}
	return prismcontainer.Run(context.Background(), prismcontainer.Deps{}, c, req)
}

func TestIntegration_EchoHello(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Command: []string{"echo", "hello"}})
	if res.ExitCode != 0 || string(res.Stdout) != "hello\n" {
		t.Errorf("result = %+v (stderr %q)", res, res.Stderr)
	}
}

func TestIntegration_ExitCodeAndStreams(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Command: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}})
	if res.ExitCode != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Errorf("result = %+v, stdout %q, stderr %q", res, res.Stdout, res.Stderr)
	}
}

func TestIntegration_NoShell(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Command: []string{"echo", "$(id)"}})
	if string(res.Stdout) != "$(id)\n" {
		t.Errorf("stdout = %q, want $(id)", res.Stdout)
	}
}

func TestIntegration_MountReadOnlyIsDefault(t *testing.T) {
	c := integrationCaller(t)
	if err := os.WriteFile(filepath.Join(c.Worktree, "marker"), []byte("from-worktree"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runReal(t, c, prismcontainer.RunRequest{Command: []string{"sh", "-c", "pwd; cat marker; echo; touch /workspace/new"}})
	if !strings.HasPrefix(string(res.Stdout), "/workspace\nfrom-worktree\n") {
		t.Errorf("stdout = %q, want /workspace and the marker", res.Stdout)
	}
	if res.ExitCode == 0 {
		t.Errorf("a write under /workspace succeeded with the default mount")
	}
	if _, err := os.Stat(filepath.Join(c.Worktree, "new")); err == nil {
		t.Errorf("the read-only mount let the container create a file")
	}
}

func TestIntegration_MountReadWrite(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Mount: prismcontainer.MountRW, Command: []string{"sh", "-c", "echo data > /workspace/created"}})
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v (stderr %q)", res, res.Stderr)
	}
	path := filepath.Join(c.Worktree, "created")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file written in the container is not in the worktree: %v", err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		t.Errorf("file owner uid %d, want the host user %d so the agent can delete it", st.Uid, os.Getuid())
	}
	if err := os.Remove(path); err != nil {
		t.Errorf("remove the file: %v", err)
	}
}

func TestIntegration_MountNone(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Mount: prismcontainer.MountNone, Command: []string{"test", "-e", "/workspace"}})
	if res.ExitCode != 1 {
		t.Errorf("exit = %d, want 1 (/workspace absent)", res.ExitCode)
	}
}

func TestIntegration_Env(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Env: []string{"KEY=VALUE"}, Command: []string{"sh", "-c", "echo $KEY"}})
	if string(res.Stdout) != "VALUE\n" {
		t.Errorf("stdout = %q, want VALUE", res.Stdout)
	}
}

func TestIntegration_Network(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Command: []string{"wget", "-q", "-O", "/dev/null", "https://example.com"}})
	if res.ExitCode != 0 {
		t.Errorf("outbound HTTPS failed: %+v (stderr %q)", res, res.Stderr)
	}
}

func TestIntegration_Timeout(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{TimeoutSeconds: 3, Command: []string{"sleep", "60"}})
	if res.ExitCode != prismcontainer.ExitTimeout || !strings.Contains(res.Message, "--timeout of 3s") {
		t.Errorf("result = %+v", res)
	}
}

func TestIntegration_MissingImage(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Image: "prism-no-such-image-3061"})
	if res.ExitCode == 0 || len(res.Stderr) == 0 {
		t.Errorf("result = %+v, want a non-zero exit and the pull error", res)
	}
}

func TestIntegration_OutputTruncated(t *testing.T) {
	c := integrationCaller(t)
	res := runReal(t, c, prismcontainer.RunRequest{Command: []string{"head", "-c", "1100000", "/dev/zero"}})
	var stderr bytes.Buffer
	prismcontainer.WriteResult(io.Discard, &stderr, res)
	if len(res.Stdout) != prismcontainer.OutputLimit || !strings.Contains(stderr.String(), "truncated") {
		t.Errorf("stdout %d bytes, notes %q", len(res.Stdout), stderr.String())
	}
}

// TestIntegration_FixedOptionsInPodman inspects a running container and
// checks the options that podman applied.
func TestIntegration_FixedOptionsInPodman(t *testing.T) {
	c := integrationCaller(t)
	done := make(chan prismcontainer.RunResult, 1)
	go func() { done <- runReal(t, c, prismcontainer.RunRequest{Command: []string{"sleep", "20"}}) }()

	var id string
	for deadline := time.Now().Add(60 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, _ := exec.Command("podman", "ps", "--filter", "label="+prismcontainer.LabelInstanceID+"="+c.InstanceID, "--format", "{{.ID}}").Output()
		id = strings.TrimSpace(string(out))
	}
	if id == "" {
		t.Fatalf("no running container with the label of the run")
	}
	out, err := exec.Command("podman", "inspect", id).Output()
	if err != nil {
		t.Fatalf("podman inspect: %v", err)
	}
	var inspect []struct {
		Name       string
		HostConfig struct {
			Memory      int64
			MemorySwap  int64
			NanoCpus    int64
			PidsLimit   int64
			Privileged  bool
			CapAdd      []string
			SecurityOpt []string
			AutoRemove  bool
		}
	}
	if err := json.Unmarshal(out, &inspect); err != nil || len(inspect) != 1 {
		t.Fatalf("parse inspect: %v", err)
	}
	h := inspect[0].HostConfig
	if h.Memory != 4<<30 || h.MemorySwap != 4<<30 || h.NanoCpus != 2_000_000_000 || h.PidsLimit != 1024 || h.Privileged || len(h.CapAdd) != 0 || !h.AutoRemove {
		t.Errorf("HostConfig = %+v", h)
	}
	if !strings.Contains(strings.Join(h.SecurityOpt, ","), "no-new-privileges") {
		t.Errorf("SecurityOpt = %v", h.SecurityOpt)
	}
	if !strings.HasPrefix(inspect[0].Name, "prism-") {
		t.Errorf("name = %q, want the session prefix", inspect[0].Name)
	}
	if res := <-done; res.ExitCode != 0 {
		t.Errorf("result = %+v", res)
	}
}

func buildReal(t *testing.T, c prismcontainer.Caller, containerfile string, req prismcontainer.BuildRequest) prismcontainer.BuildResult {
	t.Helper()
	// On macOS the build checks the podman machine mounts against the
	// allowlist of the host config.
	return buildRealDeps(t, c, containerfile, prismcontainer.Deps{MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist()}, req)
}

func buildRealDeps(t *testing.T, c prismcontainer.Caller, containerfile string, deps prismcontainer.Deps, req prismcontainer.BuildRequest) prismcontainer.BuildResult {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.Worktree, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := prismcontainer.SweepSession(ctx, prismcontainer.ExecRunner{}, prismcontainer.DefaultBuildExecutor(runtime.GOOS, nil, nil), []string{c.InstanceID}); err != nil {
			t.Errorf("sweep after the build: %v", err)
		}
	})
	return prismcontainer.Build(context.Background(), deps, c, req)
}

// wantBuildFailed fails the test unless podman build started and then
// failed, and its output matches cause. A build that did not start (a
// failure of the scope or the namespace setup) or that failed for another
// reason must not pass a test that expects a failure.
func wantBuildFailed(t *testing.T, res prismcontainer.BuildResult, cause *regexp.Regexp) {
	t.Helper()
	if strings.Contains(res.Message, "podman build did not start") {
		t.Fatalf("podman build did not start, so the test proves nothing: %s (output %s)", res.Message, res.Output)
	}
	if res.ExitCode == 0 || res.Image != "" || !strings.Contains(res.Message, "the build failed") {
		t.Fatalf("build = %+v (output %s), want podman build to start and fail", res, res.Output)
	}
	if !cause.Match(res.Output) {
		t.Errorf("the build failed, but its output does not match %s, so it failed for another reason:\n%s", cause, res.Output)
	}
}

func TestIntegration_BuildThenRun(t *testing.T) {
	c := integrationCaller(t)
	res := buildReal(t, c, "FROM alpine\nARG GREETING\nRUN echo \"$GREETING\" > /built\n",
		prismcontainer.BuildRequest{BuildArgs: []string{"GREETING=hello from the build"}, Tag: "itest"})
	if res.ExitCode != 0 || !strings.HasSuffix(res.Image, "-itest") {
		t.Fatalf("build = %+v (output %s)", res, res.Output)
	}
	run := runReal(t, c, prismcontainer.RunRequest{Image: res.Image, Command: []string{"cat", "/built"}})
	if run.ExitCode != 0 || string(run.Stdout) != "hello from the build\n" {
		t.Errorf("run of the built image = %+v", run)
	}
}

func TestIntegration_BuildFailure(t *testing.T) {
	c := integrationCaller(t)
	res := buildReal(t, c, "FROM alpine\nRUN echo failing-step >&2; exit 3\n", prismcontainer.BuildRequest{})
	wantBuildFailed(t, res, regexp.MustCompile(`failing-step`))
}

// TestIntegration_BuildMemoryLimit: tail of /dev/zero reads one endless
// line into memory, so the step must hit the 4 GiB limit and fail.
func TestIntegration_BuildMemoryLimit(t *testing.T) {
	c := integrationCaller(t)
	res := buildReal(t, c, "FROM alpine\nRUN tail /dev/zero\n", prismcontainer.BuildRequest{TimeoutSeconds: 300})
	// The kernel OOM kill of the step gives exit status 137 (SIGKILL).
	wantBuildFailed(t, res, regexp.MustCompile(`(?i)RUN tail /dev/zero[\s\S]*(137|killed|out of memory|oom)`))
}

func TestIntegration_BuildSymlinkNotInImage(t *testing.T) {
	c := integrationCaller(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s3cr3t-outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.Worktree, "leak")); err != nil {
		t.Fatal(err)
	}
	res := buildReal(t, c, "FROM alpine\nCOPY . /ctx\n", prismcontainer.BuildRequest{})
	if res.ExitCode != 0 {
		t.Fatalf("build = %+v (output %s)", res, res.Output)
	}
	run := runReal(t, c, prismcontainer.RunRequest{Image: res.Image, Mount: prismcontainer.MountNone,
		Command: []string{"sh", "-c", "test ! -e /ctx/leak && ! grep -rq s3cr3t-outside /ctx"}})
	if run.ExitCode != 0 {
		t.Errorf("the image holds the symlink target: %+v", run)
	}
}

// buildStepProcs returns the processes whose command line holds marker.
// On Linux it reads the host process list. On macOS a build step runs in
// the podman machine VM, so it reads the process list of the VM.
func buildStepProcs(t *testing.T, marker string) []string {
	t.Helper()
	var found []string
	if runtime.GOOS == "linux" {
		procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, p := range procs {
			if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, []byte(marker)) {
				found = append(found, fmt.Sprintf("%s %q", p, data))
			}
		}
		return found
	}
	out, err := exec.Command("podman", "machine", "ssh", "ps", "-eo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("read the process list of the podman machine: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, marker) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// TestIntegration_BuildTimeoutStopsStep: after the timeout, no process of
// the build step runs: on the Linux host, or in the podman machine VM on
// macOS.
func TestIntegration_BuildTimeoutStopsStep(t *testing.T) {
	c := integrationCaller(t)
	const marker = "613.317"
	res := buildReal(t, c, "FROM alpine\nRUN sleep "+marker+"\n", prismcontainer.BuildRequest{TimeoutSeconds: 20})
	if res.ExitCode != prismcontainer.ExitTimeout {
		t.Fatalf("build = %+v (output %s), want the timeout", res, res.Output)
	}
	// Without the step in the output, the test cannot show that the step
	// ran and then stopped.
	if !bytes.Contains(res.Output, []byte("RUN sleep "+marker)) {
		t.Fatalf("output %s, want the RUN step to have started before the timeout", res.Output)
	}
	time.Sleep(2 * time.Second)
	if found := buildStepProcs(t, marker); len(found) != 0 {
		t.Errorf("processes of the build step still run after the timeout: %q", found)
	}
}

// TestIntegration_BuildMultiStage: COPY --from with a stage name and with a
// stage index works in the build namespace.
func TestIntegration_BuildMultiStage(t *testing.T) {
	c := integrationCaller(t)
	res := buildReal(t, c, "FROM alpine AS builder\nRUN echo stage-output > /built\nFROM alpine\nCOPY --from=builder /built /from-name\nCOPY --from=0 /built /from-index\n",
		prismcontainer.BuildRequest{Tag: "multistage"})
	if res.ExitCode != 0 {
		t.Fatalf("build = %+v (output %s)", res, res.Output)
	}
	run := runReal(t, c, prismcontainer.RunRequest{Image: res.Image, Mount: prismcontainer.MountNone, Command: []string{"cat", "/from-name", "/from-index"}})
	if run.ExitCode != 0 || string(run.Stdout) != "stage-output\nstage-output\n" {
		t.Errorf("run of the multi-stage image = %+v", run)
	}
}

// TestIntegration_SweepStopsRunningBuild: the cleanup sweep of a session
// stops its running build, and no process of the build step stays: on the
// Linux host, or in the podman machine VM on macOS.
func TestIntegration_SweepStopsRunningBuild(t *testing.T) {
	c := integrationCaller(t)
	const marker = "617.413"
	if err := os.WriteFile(filepath.Join(c.Worktree, "Containerfile"), []byte("FROM alpine\nRUN sleep "+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := prismcontainer.DefaultBuildExecutor(runtime.GOOS, nil, nil)
	done := make(chan prismcontainer.BuildResult, 1)
	go func() {
		done <- prismcontainer.Build(context.Background(), prismcontainer.Deps{MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist()}, c, prismcontainer.BuildRequest{TimeoutSeconds: 600})
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = prismcontainer.SweepSession(ctx, prismcontainer.ExecRunner{}, ex, []string{c.InstanceID})
	})
	stepProcs := func() []string { return buildStepProcs(t, marker) }
	deadline := time.Now().Add(3 * time.Minute)
	for len(stepProcs()) == 0 {
		select {
		case res := <-done:
			t.Fatalf("the build ended before its RUN step ran: %+v (output %s)", res, res.Output)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the RUN step did not start within 3 minutes")
		}
		time.Sleep(200 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := prismcontainer.SweepSession(ctx, prismcontainer.ExecRunner{}, ex, []string{c.InstanceID}); err != nil {
		t.Errorf("SweepSession: %v", err)
	}
	select {
	case res := <-done:
		if res.ExitCode == 0 || res.Image != "" {
			t.Errorf("build = %+v (output %s), want a build that the sweep stopped", res, res.Output)
		}
	case <-time.After(time.Minute):
		t.Fatal("the build did not end within a minute after the sweep")
	}
	time.Sleep(2 * time.Second)
	if found := stepProcs(); len(found) != 0 {
		t.Errorf("processes of the build step still run after the sweep: %q", found)
	}
}

func TestIntegration_BuildImagesRemovedBySweep(t *testing.T) {
	c := integrationCaller(t)
	if err := os.WriteFile(filepath.Join(c.Worktree, "Containerfile"), []byte("FROM alpine\nRUN echo layer > /l\nRUN echo second > /s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := prismcontainer.Build(context.Background(), prismcontainer.Deps{MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist()}, c, prismcontainer.BuildRequest{})
	if res.ExitCode != 0 {
		t.Fatalf("build = %+v", res)
	}
	if err := prismcontainer.SweepSession(context.Background(), prismcontainer.ExecRunner{}, prismcontainer.DefaultBuildExecutor(runtime.GOOS, nil, nil), []string{c.InstanceID}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}
	out, err := exec.Command("podman", "images", "--all", "--quiet", "--filter", "label="+prismcontainer.LabelInstanceID+"="+c.InstanceID).Output()
	if err != nil {
		t.Fatalf("podman images: %v", err)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("images with the label of the session remain: %s", s)
	}
}

// pastCheck runs the real build executor after it replaces the Containerfile
// of the build copy with containerfile. The Containerfile check of prism
// reads the copy before the executor runs, so the build namespace gets a
// source that the check refuses.
type pastCheck struct {
	prismcontainer.BuildExecutor
	containerfile string
}

func (p pastCheck) Build(ctx context.Context, unit string, out io.Writer, args []string) (int, error) {
	i := slices.Index(args, "--file")
	if i < 0 || i+1 == len(args) {
		return 125, errors.New("pastCheck: the build args hold no --file")
	}
	if err := os.WriteFile(args[i+1], []byte(p.containerfile), 0o600); err != nil {
		return 125, err
	}
	return p.BuildExecutor.Build(ctx, unit, out, args)
}

// namespacedBuild runs containerfile through prism Build on Linux, past the
// Containerfile check.
func namespacedBuild(t *testing.T, c prismcontainer.Caller, containerfile string) prismcontainer.BuildResult {
	t.Helper()
	ex := pastCheck{prismcontainer.DefaultBuildExecutor("linux", nil, nil), containerfile}
	return buildRealDeps(t, c, "FROM alpine\n", prismcontainer.Deps{BuildExecutor: ex}, prismcontainer.BuildRequest{})
}

// leakFile copies the file /secret of the image source into a stage and
// prints it, so a build that reads the source shows the secret.
func leakFile(source string) string {
	return "FROM " + source + "\nFROM alpine\nCOPY --from=0 /secret /leak\nRUN cat /leak\n"
}

// plainBuild is the control of a negative test. It runs containerfile with
// plain podman build, outside the build namespace, with a policy that
// accepts every source, in a throwaway store. The control must read the
// source and print want. If it does not, the source fails for a cause other
// than the namespace, and the negative test measures nothing.
func plainBuild(t *testing.T, containerfile, want string) {
	t.Helper()
	// podman limits the runroot path to 50 characters, and t.TempDir
	// embeds the test name, so use a short directory under /tmp.
	dir, err := os.MkdirTemp("/tmp", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	// The store holds files of sub-uids, so remove it in the user
	// namespace of podman first.
	t.Cleanup(func() {
		_ = exec.Command("podman", "unshare", "rm", "-rf", store).Run()
		_ = os.RemoveAll(dir)
	})
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(containerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	ctxDir := filepath.Join(dir, "context")
	if err := os.Mkdir(ctxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("podman", "--root", filepath.Join(store, "root"), "--runroot", filepath.Join(store, "run"), "--storage-driver", "vfs",
		"build", "--signature-policy", policy, "--file", file, ctxDir).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte(want)) {
		t.Fatalf("control: plain podman build outside the build namespace did not print %q, so the test does not measure the namespace: %v\n%s", want, err, out)
	}
}

// secretTar writes a tar archive in dir that holds the file secret with
// the content s.
func secretTar(t *testing.T, dir, s string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "secret.tar")
	if out, err := exec.Command("tar", "-cf", tarPath, "-C", dir, "secret").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	return tarPath
}

// TestIntegration_BuildHomeTarballNotReachable: a FROM source names a tar
// archive in the home dir. On Linux the path does not exist in the mount
// namespace of the build. The tarball transport opens the file when it
// parses the reference, before the signature policy applies.
//
// The FROM form is used because buildah replaces each error of a COPY
// --from source with "no stage or image found with that name"
// (imagebuildah/stage_executor.go), so a COPY --from or ONBUILD form cannot
// tell a missing path from a policy refusal.
func TestIntegration_BuildHomeTarballNotReachable(t *testing.T) {
	c := integrationCaller(t)
	if runtime.GOOS != "linux" {
		t.Skip("the build namespace applies to Linux builds only")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".prism-itest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	tarPath := secretTar(t, dir, "s3cr3t-home")
	cf := leakFile("tarball:" + tarPath)
	plainBuild(t, cf, "s3cr3t-home")
	res := namespacedBuild(t, c, cf)
	p := regexp.QuoteMeta(tarPath)
	wantBuildFailed(t, res, regexp.MustCompile(`error opening "`+p+`": open `+p+`: no such file or directory`))
}

// TestIntegration_BuildOtherStoreNotReachable: a FROM source names an image
// in another podman store of the host user. The signature policy accepts
// containers-storage, so only the build namespace stops it. The store root
// does not exist in the namespace, so the storage library creates an empty
// store in the tmpfs root of bwrap, or fails to open the store.
func TestIntegration_BuildOtherStoreNotReachable(t *testing.T) {
	c := integrationCaller(t)
	if runtime.GOOS != "linux" {
		t.Skip("the build namespace applies to Linux builds only")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".prism-itest-store-")
	if err != nil {
		t.Fatal(err)
	}
	root, runRoot := filepath.Join(dir, "root"), filepath.Join(dir, "run")
	other := []string{"--root", root, "--runroot", runRoot, "--storage-driver", "vfs"}
	t.Cleanup(func() {
		// The store holds files of sub-uids, so remove it in the user
		// namespace of podman.
		_ = exec.Command("podman", "unshare", "rm", "-rf", dir).Run()
	})
	image := "localhost/prism-itest-other-" + strings.ReplaceAll(c.InstanceID, "-", "") + ":latest"
	tarPath := secretTar(t, t.TempDir(), "s3cr3t-store")
	if out, err := exec.Command("podman", append(other, "import", tarPath, image)...).CombinedOutput(); err != nil {
		t.Fatalf("import into the other store: %v: %s", err, out)
	}
	cf := leakFile("containers-storage:[vfs@" + root + "+" + runRoot + "]" + image)
	plainBuild(t, cf, "s3cr3t-store")
	res := namespacedBuild(t, c, cf)
	wantBuildFailed(t, res, regexp.MustCompile(regexp.QuoteMeta(root)+`[^\n]*(does not resolve to an image ID|no such file or directory)`))
}

// TestIntegration_BuildRunStepNetwork: a RUN step runs in the build
// namespace and reaches the network.
func TestIntegration_BuildRunStepNetwork(t *testing.T) {
	c := integrationCaller(t)
	res := buildReal(t, c, "FROM alpine\nRUN wget -q -O /dev/null https://example.com && echo ok > /net\n", prismcontainer.BuildRequest{})
	if res.ExitCode != 0 {
		t.Errorf("build = %+v (output %s), want a RUN step with network to work", res, res.Output)
	}
}

// secondCaller returns another session incarnation in the same prism
// state dir as c.
func secondCaller(t *testing.T, c prismcontainer.Caller) prismcontainer.Caller {
	t.Helper()
	o := prismcontainer.Caller{
		SessionName: c.SessionName + "-other",
		InstanceID:  uuid.New().String(),
		Worktree:    integrationWorktree(t),
	}
	t.Cleanup(func() { assertNoLabelledContainer(t, o.InstanceID) })
	return o
}

// startReal starts a service container and removes every container of the
// caller when the test ends, before the check that none remains.
func startReal(t *testing.T, c prismcontainer.Caller, req prismcontainer.StartRequest) prismcontainer.StartResult {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := prismcontainer.SweepInstances(ctx, prismcontainer.ExecRunner{}, []string{c.InstanceID}); err != nil {
			t.Errorf("SweepInstances: %v", err)
		}
	})
	return prismcontainer.Start(context.Background(), prismcontainer.Deps{}, c, req)
}

func execReal(c prismcontainer.Caller, handle string, command ...string) prismcontainer.RunResult {
	return prismcontainer.Exec(context.Background(), prismcontainer.Deps{}, c, prismcontainer.ExecRequest{Handle: handle, Command: command})
}

// waitReady runs pg_isready in the service container until it exits 0.
func waitReady(t *testing.T, c prismcontainer.Caller, handle string) {
	t.Helper()
	var last prismcontainer.RunResult
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if last = execReal(c, handle, "pg_isready", "-h", "127.0.0.1", "-U", "postgres"); last.ExitCode == 0 {
			return
		}
	}
	t.Fatalf("postgres did not accept connections: %+v (stdout %q, stderr %q)", last, last.Stdout, last.Stderr)
}

// TestIntegration_ServicePostgres is the database flow of the
// prism-container skill: start postgres:16 with a published port, wait
// until it accepts connections, connect from the host to 127.0.0.1, read
// the logs, check the fixed options, then stop it.
func TestIntegration_ServicePostgres(t *testing.T) {
	c := integrationCaller(t)
	// A real worktree is readable by every user. The postgres user of the
	// image then can enter /workspace, its working directory.
	if err := os.Chmod(c.Worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	res := startReal(t, c, prismcontainer.StartRequest{Image: "postgres:16", Env: []string{"POSTGRES_PASSWORD=x"}, Ports: []string{"5432"}})
	if res.ExitCode != 0 || res.Handle == "" || len(res.Ports) != 1 || res.Ports[0].ContainerPort != 5432 || res.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("start = %+v (stderr %s)", res, res.Stderr)
	}
	waitReady(t, c, res.Handle)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(res.Ports[0].HostPort))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the published port %s: %v", addr, err)
	}
	_ = conn.Close()

	if r := execReal(c, res.Handle, "sh", "-c", "echo out; echo err >&2; exit 7"); r.ExitCode != 7 || string(r.Stdout) != "out\n" || string(r.Stderr) != "err\n" {
		t.Errorf("exec = %+v, stdout %q, stderr %q", r, r.Stdout, r.Stderr)
	}
	logs := prismcontainer.Logs(context.Background(), prismcontainer.Deps{}, c, prismcontainer.HandleRequest{Handle: res.Handle})
	if logs.ExitCode != 0 || !strings.Contains(string(logs.Stdout)+string(logs.Stderr), "database system is ready to accept connections") {
		t.Errorf("logs = %+v, stdout %q, stderr %q", logs, logs.Stdout, logs.Stderr)
	}

	inspectService(t, c, res.Handle)

	list := prismcontainer.List(context.Background(), prismcontainer.Deps{}, c)
	if list.ExitCode != 0 || len(list.Containers) != 1 || list.Containers[0].Handle != res.Handle || list.Containers[0].Kind != "service" {
		t.Errorf("list = %+v", list)
	}

	if r := prismcontainer.Stop(context.Background(), prismcontainer.Deps{}, c, prismcontainer.HandleRequest{Handle: res.Handle}); r.ExitCode != 0 {
		t.Fatalf("stop = %+v", r)
	}
	if r := execReal(c, res.Handle, "true"); r.ExitCode != prismcontainer.ExitRefused || !strings.Contains(r.Message, "does not exist") {
		t.Errorf("exec after stop = %+v, want the does-not-exist refusal", r)
	}
	assertNoLabelledContainer(t, c.InstanceID)
}

// inspectService checks the options that podman applied to the service
// container, and that every port binding is on 127.0.0.1.
func inspectService(t *testing.T, c prismcontainer.Caller, handle string) {
	t.Helper()
	out, err := exec.Command("podman", "ps", "--filter", "label="+prismcontainer.LabelService+"="+handle, "--format", "{{.ID}}").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("podman ps for handle %s: %v %q", handle, err, out)
	}
	out, err = exec.Command("podman", "inspect", strings.TrimSpace(string(out))).Output()
	if err != nil {
		t.Fatalf("podman inspect: %v", err)
	}
	var inspect []struct {
		Name       string
		Config     struct{ Labels map[string]string }
		HostConfig struct {
			Memory       int64
			MemorySwap   int64
			NanoCpus     int64
			PidsLimit    int64
			Privileged   bool
			CapAdd       []string
			SecurityOpt  []string
			AutoRemove   bool
			PortBindings map[string][]struct{ HostIp, HostPort string }
		}
	}
	if err := json.Unmarshal(out, &inspect); err != nil || len(inspect) != 1 {
		t.Fatalf("parse inspect: %v", err)
	}
	h := inspect[0].HostConfig
	if h.Memory != 4<<30 || h.MemorySwap != 4<<30 || h.NanoCpus != 2_000_000_000 || h.PidsLimit != 1024 || h.Privileged || len(h.CapAdd) != 0 || h.AutoRemove {
		t.Errorf("HostConfig = %+v", h)
	}
	if !strings.Contains(strings.Join(h.SecurityOpt, ","), "no-new-privileges") {
		t.Errorf("SecurityOpt = %v", h.SecurityOpt)
	}
	if len(h.PortBindings) == 0 {
		t.Errorf("no port bindings")
	}
	for port, binds := range h.PortBindings {
		for _, b := range binds {
			if b.HostIp != "127.0.0.1" {
				t.Errorf("port %s binds to %q, want 127.0.0.1", port, b.HostIp)
			}
		}
	}
	if l := inspect[0].Config.Labels; l[prismcontainer.LabelInstanceID] != c.InstanceID || l[prismcontainer.LabelService] != handle {
		t.Errorf("labels = %v", l)
	}
	if !strings.HasPrefix(inspect[0].Name, "prism-") || !strings.HasSuffix(inspect[0].Name, "-"+handle) {
		t.Errorf("name = %q, want the session prefix and the handle", inspect[0].Name)
	}
}

// TestIntegration_ServiceLimitsAndExec checks that a running service
// container holds the per-session place, and that exec is not refused.
func TestIntegration_ServiceLimitsAndExec(t *testing.T) {
	c := integrationCaller(t)
	res := startReal(t, c, prismcontainer.StartRequest{Image: "alpine", Mount: prismcontainer.MountNone, Command: []string{"sleep", "600"}})
	if res.ExitCode != 0 {
		t.Fatalf("start = %+v (stderr %s)", res, res.Stderr)
	}
	second := startReal(t, c, prismcontainer.StartRequest{Image: "alpine", Command: []string{"sleep", "600"}})
	if second.ExitCode != prismcontainer.ExitRefused || !strings.Contains(second.Message, "per-session limit") {
		t.Errorf("second start = %+v, want the per-session refusal", second)
	}
	if r := execReal(c, res.Handle, "echo", "hello"); r.ExitCode != 0 || string(r.Stdout) != "hello\n" {
		t.Errorf("exec = %+v (stdout %q)", r, r.Stdout)
	}
	// podman exec stops option parsing at the container, so a command
	// that looks like an option is the command.
	if r := execReal(c, res.Handle, "--user", "root", "id"); r.ExitCode == 0 || !strings.Contains(string(r.Stderr), "--user") {
		t.Errorf("exec of --user = %+v (stderr %q), want podman to run --user as the command and fail", r, r.Stderr)
	}
}

// TestIntegration_ServiceRefusesOtherContainers checks that another
// session, and a run container of the same session, cannot be the target
// of exec, logs, or stop, and that the target is not changed.
func TestIntegration_ServiceRefusesOtherContainers(t *testing.T) {
	owner := integrationCaller(t)
	res := startReal(t, owner, prismcontainer.StartRequest{Image: "alpine", Mount: prismcontainer.MountNone, Command: []string{"sleep", "600"}})
	if res.ExitCode != 0 {
		t.Fatalf("start = %+v (stderr %s)", res, res.Stderr)
	}
	out, err := exec.Command("podman", "ps", "--filter", "label="+prismcontainer.LabelService+"="+res.Handle, "--format", "{{.ID}} {{.Names}}").Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) != 2 {
		t.Fatalf("podman ps: %v %q", err, out)
	}
	id, name := fields[0], fields[1]

	other := secondCaller(t, owner)
	for _, handle := range []string{res.Handle, id, name} {
		for op, r := range map[string]prismcontainer.RunResult{
			"exec": execReal(other, handle, "touch", "/changed"),
			"logs": prismcontainer.Logs(context.Background(), prismcontainer.Deps{}, other, prismcontainer.HandleRequest{Handle: handle}),
			"stop": prismcontainer.Stop(context.Background(), prismcontainer.Deps{}, other, prismcontainer.HandleRequest{Handle: handle}),
		} {
			if r.ExitCode != prismcontainer.ExitRefused || !strings.Contains(r.Message, "does not carry the label of this session") {
				t.Errorf("%s %s from another session = %+v", op, handle, r)
			}
		}
	}
	if r := execReal(owner, res.Handle, "test", "!", "-e", "/changed"); r.ExitCode != 0 {
		t.Errorf("the other session changed the container: %+v", r)
	}

	runner := secondCaller(t, owner)
	done := make(chan prismcontainer.RunResult, 1)
	go func() {
		done <- runReal(t, runner, prismcontainer.RunRequest{Mount: prismcontainer.MountNone, Command: []string{"sleep", "20"}})
	}()
	var runName string
	for deadline := time.Now().Add(time.Minute); runName == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, _ := exec.Command("podman", "ps", "--filter", "label="+prismcontainer.LabelInstanceID+"="+runner.InstanceID, "--format", "{{.Names}}").Output()
		runName = strings.TrimSpace(string(out))
	}
	if runName == "" {
		t.Fatalf("no running container of the run")
	}
	if r := execReal(runner, runName, "true"); r.ExitCode != prismcontainer.ExitRefused || !strings.Contains(r.Message, "was not created by prism container start") {
		t.Errorf("exec into a run container = %+v", r)
	}
	if r := <-done; r.ExitCode != 0 {
		t.Errorf("run = %+v", r)
	}
}

// TestIntegration_ImageLabelsDoNotSetIdentity: podman copies the labels of
// an image to each container of the image. The labels that prism sets on
// the command line replace the image labels with the same key. A run
// container of an image with forged prism labels is thus a run container
// of the caller, and exec, logs, and stop refuse it.
func TestIntegration_ImageLabelsDoNotSetIdentity(t *testing.T) {
	c := integrationCaller(t)
	const forgedHandle = "cafef00d"
	forgedOwner := uuid.New().String()
	image := "localhost/prism-itest-forged-labels-" + forgedOwner[:8]
	dir := t.TempDir()
	containerfile := fmt.Sprintf("FROM alpine\nLABEL %s=%s %s=%s %s=prism-build-forged\n",
		prismcontainer.LabelInstanceID, forgedOwner, prismcontainer.LabelService, forgedHandle, prismcontainer.LabelBuild)
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("podman", "build", "--tag", image, dir).CombinedOutput(); err != nil {
		t.Fatalf("podman build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "rmi", "--force", image).Run() })

	done := make(chan prismcontainer.RunResult, 1)
	go func() {
		done <- runReal(t, c, prismcontainer.RunRequest{Image: image, Mount: prismcontainer.MountNone, Command: []string{"sleep", "20"}})
	}()
	var fields []string
	for deadline := time.Now().Add(time.Minute); len(fields) == 0 && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, _ := exec.Command("podman", "ps", "--filter", "label="+prismcontainer.LabelInstanceID+"="+c.InstanceID, "--format", "{{.ID}} {{.Names}}").Output()
		fields = strings.Fields(string(out))
	}
	if len(fields) != 2 {
		t.Fatalf("podman ps of the run container = %q, want one ID and one name", fields)
	}
	id, name := fields[0], fields[1]

	out, err := exec.Command("podman", "inspect", id).Output()
	if err != nil {
		t.Fatalf("podman inspect: %v", err)
	}
	var inspect []struct {
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(out, &inspect); err != nil || len(inspect) != 1 {
		t.Fatalf("parse inspect: %v", err)
	}
	for key, want := range map[string]string{
		prismcontainer.LabelInstanceID: c.InstanceID,
		prismcontainer.LabelService:    "",
		prismcontainer.LabelBuild:      "",
	} {
		if got, ok := inspect[0].Config.Labels[key]; !ok || got != want {
			t.Errorf("label %s = %q (set %v), want %q from the podman command line", key, got, ok, want)
		}
	}

	for _, handle := range []string{id, name, forgedHandle} {
		want := "was not created by prism container start"
		if handle == forgedHandle {
			want = "does not exist in this session"
		}
		for op, r := range map[string]prismcontainer.RunResult{
			"exec": execReal(c, handle, "true"),
			"logs": prismcontainer.Logs(context.Background(), prismcontainer.Deps{}, c, prismcontainer.HandleRequest{Handle: handle}),
			"stop": prismcontainer.Stop(context.Background(), prismcontainer.Deps{}, c, prismcontainer.HandleRequest{Handle: handle}),
		} {
			if r.ExitCode != prismcontainer.ExitRefused || !strings.Contains(r.Message, want) {
				t.Errorf("%s %s = %+v, want a refusal with %q", op, handle, r, want)
			}
		}
	}
	list := prismcontainer.List(context.Background(), prismcontainer.Deps{}, c)
	if list.ExitCode != 0 || len(list.Containers) != 1 || list.Containers[0].Kind != prismcontainer.KindRun {
		t.Errorf("list = %+v, want the container as kind run", list)
	}
	// A refused stop leaves the container, so the run exits 0 after sleep.
	if r := <-done; r.ExitCode != 0 {
		t.Errorf("run = %+v (stderr %q)", r, r.Stderr)
	}
}

// failingRunner makes one podman subcommand exit 125, as a podman that
// fails during the sweep does.
type failingRunner struct {
	prismcontainer.Runner
	sub string
}

func (f failingRunner) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	if len(args) > 0 && args[0] == f.sub {
		return 125, nil
	}
	return f.Runner.Run(ctx, stdout, stderr, args...)
}

// TestIntegration_FailedSweepKeepsAuditDir checks the cleanup gate against
// the real podman, for a service container and for a built image: after a
// failed sweep the audit dir and the resource stay, and a later sweep
// removes both.
func TestIntegration_FailedSweepKeepsAuditDir(t *testing.T) {
	c := integrationCaller(t)
	if res := startReal(t, c, prismcontainer.StartRequest{Image: "alpine", Mount: prismcontainer.MountNone, Command: []string{"sleep", "600"}}); res.ExitCode != 0 {
		t.Fatalf("start = %+v", res)
	}
	ex := prismcontainer.DefaultBuildExecutor(runtime.GOOS, nil, config.LoadFresh().MachineMountAllowlist())
	// The service holds the per-session place, so the image belongs to a
	// second incarnation.
	imgCaller := secondCaller(t, c)
	if err := os.WriteFile(filepath.Join(imgCaller.Worktree, "Containerfile"), []byte("FROM alpine\nRUN echo layer > /l\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := prismcontainer.Build(context.Background(), prismcontainer.Deps{MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist()}, imgCaller, prismcontainer.BuildRequest{}); b.ExitCode != 0 {
		t.Fatalf("build = %+v", b)
	}
	t.Cleanup(func() {
		_, _ = prismcontainer.SweepImages(context.Background(), prismcontainer.ExecRunner{}, []string{imgCaller.InstanceID})
	})

	for _, tc := range []struct {
		caller prismcontainer.Caller
		sub    string
		left   func() string
	}{
		{c, "rm", func() string {
			return podmanQuiet(t, "ps", "-a", "--quiet", "--filter", "label="+prismcontainer.LabelInstanceID+"="+c.InstanceID)
		}},
		{imgCaller, "rmi", func() string {
			return podmanQuiet(t, "images", "--all", "--quiet", "--filter", "label="+prismcontainer.LabelInstanceID+"="+imgCaller.InstanceID)
		}},
	} {
		err := prismcontainer.RemoveAuditDir(context.Background(), failingRunner{prismcontainer.ExecRunner{}, tc.sub}, ex, tc.caller.InstanceID)
		if err == nil || !strings.Contains(err.Error(), "keep the prism-container audit dir") {
			t.Errorf("%s: RemoveAuditDir with a failing podman %s = %v", tc.sub, tc.sub, err)
		}
		if tc.left() == "" {
			t.Fatalf("%s: the resource is gone although podman %s failed", tc.sub, tc.sub)
		}
		if !container.PrismContainerAuditDirExists(tc.caller.InstanceID) {
			t.Errorf("%s: the audit dir was removed after a failed sweep", tc.sub)
		}
		if err := prismcontainer.RemoveAuditDir(context.Background(), prismcontainer.ExecRunner{}, ex, tc.caller.InstanceID); err != nil {
			t.Errorf("%s: RemoveAuditDir = %v", tc.sub, err)
		}
		if left := tc.left(); left != "" {
			t.Errorf("%s: resources remain after the later sweep: %s", tc.sub, left)
		}
		if container.PrismContainerAuditDirExists(tc.caller.InstanceID) {
			t.Errorf("%s: the audit dir stays after a successful sweep", tc.sub)
		}
	}
}

func podmanQuiet(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("podman", args...).Output()
	if err != nil {
		t.Fatalf("podman %q: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// serviceVolumes returns the names of the volumes of the service container
// with handle.
func serviceVolumes(t *testing.T, handle string) []string {
	t.Helper()
	id := podmanQuiet(t, "ps", "--quiet", "--filter", "label="+prismcontainer.LabelService+"="+handle)
	out, err := exec.Command("podman", "inspect", id).Output()
	if err != nil {
		t.Fatalf("podman inspect %s: %v", id, err)
	}
	var inspect []struct {
		Mounts []struct{ Type, Name string }
	}
	if err := json.Unmarshal(out, &inspect); err != nil || len(inspect) != 1 {
		t.Fatalf("parse inspect: %v", err)
	}
	var names []string
	for _, m := range inspect[0].Mounts {
		if m.Type == "volume" && m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names
}

// TestIntegration_ServiceVolumesRemoved checks that stop and the cleanup
// sweep remove the anonymous volume that the VOLUME of postgres:16 gives a
// service container.
func TestIntegration_ServiceVolumesRemoved(t *testing.T) {
	c := integrationCaller(t)
	for _, path := range []string{"stop", "sweep"} {
		res := startReal(t, c, prismcontainer.StartRequest{Image: "postgres:16", Mount: prismcontainer.MountNone, Command: []string{"sleep", "600"}})
		if res.ExitCode != 0 {
			t.Fatalf("%s: start = %+v (stderr %s)", path, res, res.Stderr)
		}
		volumes := serviceVolumes(t, res.Handle)
		if len(volumes) == 0 {
			t.Fatalf("%s: the service container has no anonymous volume, so the test proves nothing", path)
		}
		switch path {
		case "stop":
			if r := prismcontainer.Stop(context.Background(), prismcontainer.Deps{}, c, prismcontainer.HandleRequest{Handle: res.Handle}); r.ExitCode != 0 {
				t.Fatalf("stop = %+v", r)
			}
		case "sweep":
			if err := prismcontainer.SweepSession(context.Background(), prismcontainer.ExecRunner{}, prismcontainer.DefaultBuildExecutor(runtime.GOOS, nil, nil), []string{c.InstanceID}); err != nil {
				t.Fatalf("SweepSession: %v", err)
			}
		}
		for _, v := range volumes {
			if err := exec.Command("podman", "volume", "exists", v).Run(); err == nil {
				t.Errorf("%s: volume %s of the service container stays on the host", path, v)
				_ = exec.Command("podman", "volume", "rm", "--force", v).Run()
			}
		}
	}
}
