package sidecar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
		Worktree:           t.TempDir(),
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
