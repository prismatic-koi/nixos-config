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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

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
		Worktree:    t.TempDir(),
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
