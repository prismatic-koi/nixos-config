package sidecar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

const containerTestInstanceID = "12345678-9abc-4def-8123-456789abcdef"

func newContainerTestSidecar(t *testing.T, role string, fake *prismcontainertest.Fake) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return New(Config{
		SessionName:        "prism-test@container",
		Repo:               "prism-test",
		Worktree:           prismcontainertest.RealTempDir(t),
		HarnessURL:         "http://localhost:14000",
		Clock:              newTestClock(),
		AgentRole:          role,
		InstanceID:         containerTestInstanceID,
		Harness:            newSSEHarness(),
		ContainerRunner:    fake,
		ContainerHostLimit: 4,
	})
}

func TestHostAPI_ContainerRun_AllRoles(t *testing.T) {
	for _, role := range []string{"worker", "coordinator", "review-code"} {
		t.Run(role, func(t *testing.T) {
			fake := &prismcontainertest.Fake{ImagePresent: true}
			fake.OnRun = func(_ context.Context, stdout, stderr io.Writer, _ []string) (int, error) {
				_, _ = io.WriteString(stdout, "hello\n")
				_, _ = io.WriteString(stderr, "warn\n")
				return 3, nil
			}
			sc := newContainerTestSidecar(t, role, fake)

			rr := doHostAPI(t, sc, http.MethodPost, "/container/run", `{"image":"alpine","command":["echo","hello"]}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			var res prismcontainer.RunResult
			decodeJSONBody(t, rr, &res)
			if res.ExitCode != 3 || string(res.Stdout) != "hello\n" || string(res.Stderr) != "warn\n" {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

// TestHostAPI_ContainerRun_SameArgvAsDirectRun pins the parity of the two
// routes: the sidecar route gives podman the same argument vector as a
// direct prismcontainer.Run call for the sidecar's own session. The
// host-mode route has the matching test in cmd.
func TestHostAPI_ContainerRun_SameArgvAsDirectRun(t *testing.T) {
	viaSidecar := &prismcontainertest.Fake{ImagePresent: true}
	sc := newContainerTestSidecar(t, "worker", viaSidecar)
	req := prismcontainer.RunRequest{
		Image:   "alpine",
		Command: []string{"sh", "-c", "echo $(id)"},
		Env:     []string{"A=1"},
		Mount:   prismcontainer.MountRW,
	}
	body, _ := json.Marshal(req)
	if rr := doHostAPI(t, sc, http.MethodPost, "/container/run", string(body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	direct := &prismcontainertest.Fake{ImagePresent: true}
	prismcontainer.Run(context.Background(), prismcontainer.Deps{Runner: direct, HostLimit: 4}, prismcontainer.Caller{
		SessionName: sc.cfg.SessionName,
		InstanceID:  sc.cfg.InstanceID,
		Worktree:    sc.cfg.Worktree,
	}, req)

	got, want := viaSidecar.RunCalls(), direct.RunCalls()
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("run calls: sidecar %d, direct %d; want 1 each", len(got), len(want))
	}
	if g, w := prismcontainertest.MaskRunArgs(got[0]), prismcontainertest.MaskRunArgs(want[0]); !slices.Equal(g, w) {
		t.Errorf("sidecar argv differs from direct Run argv:\n got %q\nwant %q", g, w)
	}
}

// TestHostAPI_ContainerRun_IdentityNotFromRequest checks that the request
// cannot name a session, an instance, or a host path.
func TestHostAPI_ContainerRun_IdentityNotFromRequest(t *testing.T) {
	for _, field := range []string{"session", "instance_id", "worktree", "source", "volume", "label", "name"} {
		t.Run(field, func(t *testing.T) {
			fake := &prismcontainertest.Fake{ImagePresent: true}
			sc := newContainerTestSidecar(t, "worker", fake)
			rr := doHostAPI(t, sc, http.MethodPost, "/container/run", `{"image":"alpine","`+field+`":"/etc"}`)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown field") {
				t.Errorf("status = %d, body = %s; want 400 unknown field", rr.Code, rr.Body.String())
			}
			if len(fake.Calls()) != 0 {
				t.Errorf("podman called: %q", fake.Calls())
			}
		})
	}
}

func TestHostAPI_ContainerRun_GetNotAllowed(t *testing.T) {
	sc := newContainerTestSidecar(t, "worker", &prismcontainertest.Fake{})
	if rr := doHostAPI(t, sc, http.MethodGet, "/container/run", ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
}

func newContainerBuildTestSidecar(t *testing.T, role string, fake *prismcontainertest.Fake) *Sidecar {
	t.Helper()
	sc := newContainerTestSidecar(t, role, fake)
	sc.cfg.ContainerBuildExecutor = fake
	if err := os.WriteFile(filepath.Join(sc.cfg.Worktree, "Containerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestHostAPI_ContainerBuild_AllRoles(t *testing.T) {
	for _, role := range []string{"worker", "coordinator", "review-code"} {
		t.Run(role, func(t *testing.T) {
			fake := &prismcontainertest.Fake{}
			fake.OnBuild = func(_ context.Context, out io.Writer, _ []string) (int, error) {
				_, _ = io.WriteString(out, "STEP 1/1: FROM alpine\n")
				return 0, nil
			}
			sc := newContainerBuildTestSidecar(t, role, fake)

			rr := doHostAPI(t, sc, http.MethodPost, "/container/build", `{"tag":"app","build_args":["A=1"]}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			var res prismcontainer.BuildResult
			decodeJSONBody(t, rr, &res)
			if res.ExitCode != 0 || !strings.HasSuffix(res.Image, "-app") || string(res.Output) != "STEP 1/1: FROM alpine\n" {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

// TestHostAPI_ContainerBuild_SameArgvAsDirectBuild pins the parity of the
// two routes for a build. The host-mode route has the matching test in
// cmd.
func TestHostAPI_ContainerBuild_SameArgvAsDirectBuild(t *testing.T) {
	viaSidecar := &prismcontainertest.Fake{}
	sc := newContainerBuildTestSidecar(t, "worker", viaSidecar)
	req := prismcontainer.BuildRequest{BuildArgs: []string{"A=$(id)"}, Tag: "app:v1", TimeoutSeconds: 600}
	body, _ := json.Marshal(req)
	rr := doHostAPI(t, sc, http.MethodPost, "/container/build", string(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var viaRes prismcontainer.BuildResult
	decodeJSONBody(t, rr, &viaRes)

	direct := &prismcontainertest.Fake{}
	directRes := prismcontainer.Build(context.Background(), prismcontainer.Deps{Runner: direct, BuildExecutor: direct, HostLimit: 4}, prismcontainer.Caller{
		SessionName: sc.cfg.SessionName,
		InstanceID:  sc.cfg.InstanceID,
		Worktree:    sc.cfg.Worktree,
	}, req)

	if viaRes.Image != directRes.Image {
		t.Errorf("sidecar image %q differs from direct %q", viaRes.Image, directRes.Image)
	}
	got, want := viaSidecar.BuildCalls(), direct.BuildCalls()
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("build calls: sidecar %d, direct %d; want 1 each", len(got), len(want))
	}
	if g, w := prismcontainertest.MaskBuildArgs(got[0]), prismcontainertest.MaskBuildArgs(want[0]); !slices.Equal(g, w) {
		t.Errorf("sidecar argv differs from direct Build argv:\n got %q\nwant %q", g, w)
	}
}

// TestHostAPI_ContainerBuild_IdentityNotFromRequest checks that the request
// cannot name a session, an instance, a host path, or a podman option.
func TestHostAPI_ContainerBuild_IdentityNotFromRequest(t *testing.T) {
	for _, field := range []string{"session", "instance_id", "worktree", "label", "name", "volume", "secret", "network", "device", "cap_add", "image"} {
		t.Run(field, func(t *testing.T) {
			fake := &prismcontainertest.Fake{}
			sc := newContainerBuildTestSidecar(t, "worker", fake)
			rr := doHostAPI(t, sc, http.MethodPost, "/container/build", `{"`+field+`":"/etc"}`)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown field") {
				t.Errorf("status = %d, body = %s; want 400 unknown field", rr.Code, rr.Body.String())
			}
			if len(fake.Calls()) != 0 {
				t.Errorf("podman called: %q", fake.Calls())
			}
		})
	}
}

func TestHostAPI_ContainerBuild_GetNotAllowed(t *testing.T) {
	sc := newContainerTestSidecar(t, "worker", &prismcontainertest.Fake{})
	if rr := doHostAPI(t, sc, http.MethodGet, "/container/build", ""); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
}

// TestHostAPI_ContainerBuild_TransportRefused: the Containerfile check
// refuses a transport reference on the sidecar route, before podman runs.
func TestHostAPI_ContainerBuild_TransportRefused(t *testing.T) {
	fake := &prismcontainertest.Fake{}
	sc := newContainerBuildTestSidecar(t, "worker", fake)
	if err := os.WriteFile(filepath.Join(sc.cfg.Worktree, "Containerfile"), []byte("ARG SRC=tarball:/home/u/backup.tar.gz\nFROM ${SRC}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rr := doHostAPI(t, sc, http.MethodPost, "/container/build", `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var res prismcontainer.BuildResult
	decodeJSONBody(t, rr, &res)
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, `the "tarball" transport`) {
		t.Errorf("result = %+v, want the transport refusal", res)
	}
	if calls := fake.BuildCalls(); len(calls) != 0 {
		t.Errorf("podman build ran: %q", calls)
	}
}
