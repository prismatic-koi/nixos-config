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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
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
		Worktree:    prismcontainertest.RealTempDir(t),
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
	// On macOS the build checks the podman machine mounts against the
	// allowlist of the host config.
	return prismcontainer.Build(context.Background(), prismcontainer.Deps{MachineMountAllowlist: config.LoadFresh().MachineMountAllowlist()}, c, req)
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
	if res.ExitCode == 0 || res.Image != "" || !strings.Contains(string(res.Output), "failing-step") {
		t.Errorf("build = %+v (output %s), want a failure with the output", res, res.Output)
	}
}

// TestIntegration_BuildMemoryLimit: tail of /dev/zero reads one endless
// line into memory, so the step must hit the 4 GiB limit and fail.
func TestIntegration_BuildMemoryLimit(t *testing.T) {
	c := integrationCaller(t)
	res := buildReal(t, c, "FROM alpine\nRUN tail /dev/zero\n", prismcontainer.BuildRequest{TimeoutSeconds: 300})
	if res.ExitCode == 0 || res.ExitCode == prismcontainer.ExitTimeout {
		t.Errorf("build = %+v, want the step to fail on the memory limit", res)
	}
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

// TestIntegration_BuildTimeoutStopsStep: after the timeout, no process of
// the build step runs on the host. The check reads the host process list,
// so it runs on Linux only. On macOS the step runs in the VM.
func TestIntegration_BuildTimeoutStopsStep(t *testing.T) {
	c := integrationCaller(t)
	const marker = "613.317"
	res := buildReal(t, c, "FROM alpine\nRUN sleep "+marker+"\n", prismcontainer.BuildRequest{TimeoutSeconds: 20})
	if res.ExitCode != prismcontainer.ExitTimeout {
		t.Fatalf("build = %+v, want the timeout", res)
	}
	if runtime.GOOS != "linux" {
		t.Skip("the build step runs in the podman machine VM")
	}
	time.Sleep(2 * time.Second)
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, []byte(marker)) {
			t.Errorf("a process of the build step still runs: %s %q", p, data)
		}
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

// onbuildBase builds a base image with plain podman, outside prism, whose
// ONBUILD instruction is onbuild. The Containerfile check of prism cannot
// see it. The image uses the docker format, because the OCI format drops
// ONBUILD.
func onbuildBase(t *testing.T, c prismcontainer.Caller, onbuild string) string {
	t.Helper()
	dir := t.TempDir()
	base := "localhost/prism-itest-onbuild-" + strings.ReplaceAll(c.InstanceID, "-", "")
	file := filepath.Join(dir, "Containerfile.base")
	if err := os.WriteFile(file, []byte("FROM alpine\nONBUILD "+onbuild+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("podman", "build", "--format", "docker", "-t", base, "-f", file, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the base image: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "rmi", "--force", base).Run() })
	return base
}

// homeTar writes a tar archive in the home dir of the host user, outside
// every path that a build can see, and removes it after the test.
func homeTar(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".prism-itest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("s3cr3t-home"), 0o600); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "secret.tar")
	if out, err := exec.Command("tar", "-cf", tarPath, "-C", dir, "secret").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	return tarPath
}

// TestIntegration_BuildHomeTarballNotReachable: an ONBUILD instruction of a
// base image names a tar archive in the home dir. The check cannot see it.
// On Linux the path does not exist in the mount namespace of the build, so
// the source fails before the signature policy reads it.
func TestIntegration_BuildHomeTarballNotReachable(t *testing.T) {
	c := integrationCaller(t)
	if runtime.GOOS != "linux" {
		t.Skip("the build namespace applies to Linux builds only")
	}
	tarPath := homeTar(t)
	base := onbuildBase(t, c, "COPY --from=tarball:"+tarPath+" / /leak")
	res := buildReal(t, c, "FROM "+base+"\n", prismcontainer.BuildRequest{})
	if res.ExitCode == 0 {
		t.Fatalf("build = %+v (output %s), want a failure", res, res.Output)
	}
	if !bytes.Contains(res.Output, []byte("no such file or directory")) || bytes.Contains(res.Output, []byte("rejected by policy")) {
		t.Errorf("output = %s, want the source to be missing in the build namespace, not refused by the policy", res.Output)
	}
}

// TestIntegration_BuildOtherStoreNotReachable: an ONBUILD instruction of a
// base image names an image in another podman store of the host user. The
// signature policy accepts containers-storage, so only the build namespace
// stops it.
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
	if out, err := exec.Command("podman", append(other, "pull", "-q", "docker.io/library/alpine")...).CombinedOutput(); err != nil {
		t.Fatalf("pull into the other store: %v: %s", err, out)
	}
	// Control: the image is in the other store.
	if err := exec.Command("podman", append(other, "image", "exists", "docker.io/library/alpine")...).Run(); err != nil {
		t.Fatalf("control: the other store has no alpine image: %v", err)
	}
	src := "containers-storage:[vfs@" + root + "+" + runRoot + "]docker.io/library/alpine:latest"
	base := onbuildBase(t, c, "COPY --from="+src+" /etc/alpine-release /leak")
	res := buildReal(t, c, "FROM "+base+"\n", prismcontainer.BuildRequest{})
	if res.ExitCode == 0 {
		t.Errorf("build = %+v (output %s), want a failure: the other store must not be reachable", res, res.Output)
	}
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
