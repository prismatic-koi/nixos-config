package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

const containerCmdTestInstanceID = "abcdef01-2345-4678-89ab-cdef01234567"

func TestParseContainerRunArgs(t *testing.T) {
	cases := []struct {
		args    []string
		image   string
		command []string
		wantErr bool
	}{
		{args: []string{"alpine"}, image: "alpine"},
		{args: []string{"alpine", "--"}, image: "alpine", command: []string{}},
		{args: []string{"alpine", "--", "echo", "$(id)"}, image: "alpine", command: []string{"echo", "$(id)"}},
		{args: []string{"alpine", "--", "sh", "--", "--mount", "rw"}, image: "alpine", command: []string{"sh", "--", "--mount", "rw"}},
		{args: []string{"alpine", "echo", "hi"}, wantErr: true},
	}
	for _, tc := range cases {
		image, command, err := parseContainerRunArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err = %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if image != tc.image || !slices.Equal(command, tc.command) {
			t.Errorf("%q: got %q %q, want %q %q", tc.args, image, command, tc.image, tc.command)
		}
	}
}

// TestContainerRunFlags_StopAtImage checks that a flag-shaped argument after
// the image is a command argument, not a prism flag.
func TestContainerRunFlags_StopAtImage(t *testing.T) {
	cmd := containerRunCmd
	t.Cleanup(func() { _ = cmd.Flags().Set("mount", "ro") })
	if err := cmd.Flags().Parse([]string{"--mount", "none", "alpine", "--", "echo", "--mount", "rw"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, _ := cmd.Flags().GetString("mount"); got != "none" {
		t.Errorf("--mount = %q, want none (the value before the image)", got)
	}
	if args := cmd.Flags().Args(); !slices.Equal(args, []string{"alpine", "--", "echo", "--mount", "rw"}) {
		t.Errorf("positional args = %q", args)
	}
}

func TestAgentContext_ListsContainerRun(t *testing.T) {
	doc := buildAgentContextDocument(false)
	run, ok := doc.Commands["container"].Subcommands["run"]
	if !ok {
		t.Fatalf("agent-context has no container run: %+v", doc.Commands["container"])
	}
	for _, flag := range []string{"--mount", "--env", "--timeout"} {
		if _, ok := run.Flags[flag]; !ok {
			t.Errorf("container run flag %s missing from agent-context", flag)
		}
	}
	mount := run.Flags["--mount"]
	if mount.Type != "enum" || !slices.Equal(mount.Values, prismcontainer.MountModes) || mount.Default != "ro" {
		t.Errorf("--mount meta = %+v, want enum %v default ro", mount, prismcontainer.MountModes)
	}
	if run.Flags["--timeout"].Default != "10m0s" {
		t.Errorf("--timeout default = %q, want 10m0s", run.Flags["--timeout"].Default)
	}
	data, _ := json.Marshal(run)
	if !strings.Contains(string(data), `"image"`) {
		t.Errorf("container run positional args do not name the image: %s", data)
	}
}

func TestFinishContainerRun_ExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := finishContainerRun(&stdout, &stderr, prismcontainer.RunResult{ExitCode: 3, Stdout: []byte("out"), Stderr: []byte("err")})
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.ExitCode() != 3 || ec.Error() != "" {
		t.Fatalf("err = %v, want a silent exit code 3", err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if err := finishContainerRun(io.Discard, io.Discard, prismcontainer.RunResult{}); err != nil {
		t.Errorf("exit 0: err = %v, want nil", err)
	}
}

// seedContainerRunSession writes an agent_status row with a worktree and
// an instance ID, and points openDB at it.
func seedContainerRunSession(t *testing.T, session, worktree string) {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d := sidecartest.OpenDB(t, dbFile)
	if err := d.UpsertStatus(session, "prism-test", worktree, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, containerCmdTestInstanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
}

func installPrismContainerFake(t *testing.T, f *prismcontainertest.Fake) {
	t.Helper()
	prismContainerRunnerForTest = f
	t.Cleanup(func() { prismContainerRunnerForTest = nil })
}

// TestContainerRunHostMode_SameArgvAsDirectRun pins the parity of the two
// routes: the host-mode route gives podman the same argument vector as a
// direct prismcontainer.Run call for the session that the database names.
// The sidecar route has the matching test in internal/sidecar.
func TestContainerRunHostMode_SameArgvAsDirectRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))
	session := "prism-test@container-host"
	worktree := prismcontainertest.RealTempDir(t)
	t.Setenv("PRISM_SESSION_NAME", session)
	seedContainerRunSession(t, session, worktree)

	viaHost := &prismcontainertest.Fake{ImagePresent: true}
	installPrismContainerFake(t, viaHost)
	req := prismcontainer.RunRequest{Image: "alpine", Command: []string{"echo", "$(id)"}, Env: []string{"K=v"}, Mount: prismcontainer.MountRO}
	if res := runContainerHostMode(context.Background(), req); res.ExitCode != 0 {
		t.Fatalf("host-mode run: %+v", res)
	}

	direct := &prismcontainertest.Fake{ImagePresent: true}
	prismcontainer.Run(context.Background(), prismcontainer.Deps{Runner: direct, HostLimit: 4}, prismcontainer.Caller{
		SessionName: session, InstanceID: containerCmdTestInstanceID, Worktree: worktree,
	}, req)

	got, want := viaHost.RunCalls(), direct.RunCalls()
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("run calls: host %d, direct %d; want 1 each", len(got), len(want))
	}
	if g, w := prismcontainertest.MaskRunArgs(got[0]), prismcontainertest.MaskRunArgs(want[0]); !slices.Equal(g, w) {
		t.Errorf("host-mode argv differs from direct Run argv:\n got %q\nwant %q", g, w)
	}
	if !slices.Contains(got[0], worktree+":/workspace:ro") {
		t.Errorf("host-mode argv does not mount the session worktree %s: %q", worktree, got[0])
	}
}

func TestContainerRunHostMode_UnknownSessionRefused(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_SESSION_NAME", "prism-test@not-seeded")
	seedContainerRunSession(t, "prism-test@other", t.TempDir())
	f := &prismcontainertest.Fake{ImagePresent: true}
	installPrismContainerFake(t, f)

	res := runContainerHostMode(context.Background(), prismcontainer.RunRequest{Image: "alpine"})
	if res.ExitCode == 0 || !strings.Contains(res.Message, "not in the prism database") {
		t.Errorf("result = %+v, want refusal", res)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("podman called: %q", f.Calls())
	}
}

func TestTimeoutSeconds(t *testing.T) {
	cases := map[time.Duration]int64{
		10 * time.Minute:        600,
		61 * time.Minute:        3660,
		1500 * time.Millisecond: 2,
		time.Millisecond:        1,
		0:                       -1,
		-time.Second:            -1,
	}
	for d, want := range cases {
		if got := timeoutSeconds(d); got != want {
			t.Errorf("timeoutSeconds(%s) = %d, want %d", d, got, want)
		}
	}
}
