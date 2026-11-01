package cmd

// Tests for #3391. A worker holds the pipe secret, so it can open a second
// harness pipe connection and queue session_shutdown on it. The sidecar
// reads that frame at the next pipe drop, for example an owner /new in pi,
// and exits on its clean-exit path, where Shutdown does not run. If the
// sidecar removes its PID file while a host-API handler runs, a podman
// client of that handler can create a container after the sweep of prism
// cleanup, and no stop can find the client.
//
// The fake podman client below runs in the test process, not in the
// process group of the sidecar. So RemoveOwnSidecarPIDFile cannot see it,
// as it cannot see a podman client that starts after its group check. If
// the exit path does not close the host API and wait for the handler
// before it removes the PID file, these tests fail.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/hostapiauth"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const cleanExitIID = "3391aaaa-bbbb-4ccc-8ddd-eeeeeeee3391"

// lateClient is a fake podman. Its `podman run` waits until the test
// releases it, and the container appears only then. It does not stop when
// its context ends, as a podman client that ignores the end of its
// request.
type lateClient struct {
	*prismcontainertest.Fake
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (c *lateClient) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	if len(args) > 0 && args[0] == "run" {
		c.startOnce.Do(func() { close(c.started) })
		<-c.release
	}
	return c.Fake.Run(ctx, stdout, stderr, args...)
}

func (c *lateClient) letGo() { c.releaseOnce.Do(func() { close(c.release) }) }

// sessionContainers returns the IDs of the containers of the session.
func (c *lateClient) sessionContainers() []string {
	var ids []string
	for _, ctr := range c.Containers() {
		if ctr.Labels[prismcontainer.LabelInstanceID] == cleanExitIID {
			ids = append(ids, ctr.ID)
		}
	}
	return ids
}

type cleanExitFixture struct {
	session string
	dbFile  string
	sc      *sidecar.Sidecar
	cfg     sidecar.Config
	client  *lateClient
	pidPath string
	// runDone receives the result of Run. runEnded is closed after Run.
	runDone  chan error
	runEnded chan struct{}
}

// newCleanExitFixture runs a real sidecar with a harness pipe of transport
// ("tcp" as a sandbox-exec session on Darwin, "unix" as a bwrap session)
// and a host-API socket. The PID file of the session holds the PID of the
// test process, in place of the PID of a sidecar process.
func newCleanExitFixture(t *testing.T, transport string, drainTimeout time.Duration) *cleanExitFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	// A descendant name makes the cleanup skip git, so that it needs no
	// repository.
	const session = "myrepo@clean-exit~investigate-x"
	dbFile, _ := setupCleanupCaller(t, session)
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.SetInstanceID(session, cleanExitIID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	client := &lateClient{
		Fake:    &prismcontainertest.Fake{ImagePresent: true},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	prismContainerRunnerForTest = client
	prismContainerBuildExecutorForTest = client.Fake
	t.Cleanup(func() {
		prismContainerRunnerForTest = nil
		prismContainerBuildExecutorForTest = nil
	})

	h, err := harness.New("pi", "", nil, "worker", "")
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	dir := stateguardtest.ShortTempDir(t, "ce-")
	cfg := sidecar.Config{
		SessionName:            session,
		Repo:                   "myrepo",
		Worktree:               prismcontainertest.RealTempDir(t),
		DB:                     d,
		Clock:                  sidecar.RealClock(),
		AgentRole:              "worker",
		HarnessName:            "pi",
		Harness:                h,
		InstanceID:             cleanExitIID,
		HostAPISockPath:        filepath.Join(dir, "hostapi.sock"),
		HarnessPipeSecretPath:  filepath.Join(dir, "pipe.secret"),
		StartupConnectTimeout:  30 * time.Second,
		PipeReconnectTimeout:   30 * time.Second,
		ReviewRecoveryInterval: -1,
		HostAPIDrainTimeout:    drainTimeout,
		ContainerRunner:        client,
		ContainerBuildExecutor: client.Fake,
		ContainerHostLimit:     4,
		Logger:                 log.New(io.Discard, "", 0),
	}
	if transport == "tcp" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("allocate a port: %v", err)
		}
		cfg.HarnessPipeTCPPort = ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
	} else {
		cfg.HarnessPipeSockPath = filepath.Join(dir, "pipe.sock")
	}

	pidPath, err := prismSession.SidecarPIDPath(session)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	f := &cleanExitFixture{session: session, dbFile: dbFile, sc: sidecar.New(cfg), cfg: cfg, client: client, pidPath: pidPath, runDone: make(chan error, 1), runEnded: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(f.runEnded)
		f.runDone <- f.sc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		client.letGo()
		_ = f.sc.CloseHostAPI()
		select {
		case <-f.runEnded:
		case <-time.After(10 * time.Second):
			t.Error("the sidecar did not return after the test")
		}
		f.sc.WaitNotifies()
		d.Close()
	})
	return f
}

// dialPipe connects to the harness pipe, and retries until it listens.
func (f *cleanExitFixture) dialPipe(t *testing.T) net.Conn {
	t.Helper()
	network, addr := "unix", f.cfg.HarnessPipeSockPath
	if f.cfg.HarnessPipeTCPPort != 0 {
		network, addr = "tcp", fmt.Sprintf("127.0.0.1:%d", f.cfg.HarnessPipeTCPPort)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout(network, addr, time.Second)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close() })
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial the harness pipe at %s %s: %v", network, addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeFrames writes hello with the pipe secret, and then frames.
func (f *cleanExitFixture) writeFrames(t *testing.T, conn net.Conn, frames ...map[string]any) {
	t.Helper()
	secret, err := os.ReadFile(f.cfg.HarnessPipeSecretPath)
	if err != nil {
		t.Fatalf("read the pipe secret: %v", err)
	}
	hello := map[string]any{"type": "hello", "protocol_version": 2, "harness": "pi", "harness_version": "test", "secret": string(secret)}
	var b []byte
	for _, fr := range append([]map[string]any{hello}, frames...) {
		line, err := json.Marshal(fr)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		b = append(append(b, line...), '\n')
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("write frames: %v", err)
	}
}

// connectAgent connects the live agent of the session and waits for
// hello_ack.
func (f *cleanExitFixture) connectAgent(t *testing.T) net.Conn {
	t.Helper()
	conn := f.dialPipe(t)
	f.writeFrames(t, conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil || !strings.Contains(string(line), `"hello_ack"`) {
		t.Fatalf("hello_ack: %q, %v", line, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

type hostAPIResult struct {
	status int
	body   string
	err    error
}

// startContainer sends POST /container/start to the host API of the
// sidecar, with the host-API secret, and returns at once.
func (f *cleanExitFixture) startContainer() <-chan hostAPIResult {
	out := make(chan hostAPIResult, 1)
	go func() {
		client := &http.Client{Transport: hostapiauth.UnixTransport(f.cfg.HostAPISockPath, nil), Timeout: 60 * time.Second}
		resp, err := client.Post("http://prism-hostapi/container/start", "application/json",
			strings.NewReader(`{"image":"alpine","command":["sleep","600"]}`))
		if err != nil {
			out <- hostAPIResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out <- hostAPIResult{status: resp.StatusCode, body: string(body)}
	}()
	return out
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
	var zero T
	return zero
}

// AC: a session_shutdown frame that a worker queues on a second pipe
// connection does not make the sidecar exit while a host-API container
// request runs. The sidecar keeps its PID file until the handler ends.
// After the sidecar exits, prism cleanup removes the container of the
// request. The test runs the cleanup as soon as the sidecar exits, and a
// podman client that outlives the exit then creates its container late.
func TestSidecarCleanExit_QueuedSessionShutdownLeavesNoContainer_3391(t *testing.T) {
	for _, transport := range []string{"tcp", "unix"} {
		t.Run(transport, func(t *testing.T) {
			f := newCleanExitFixture(t, transport, 0)
			agent := f.connectAgent(t)
			req := f.startContainer()
			waitFor(t, f.client.started, "the podman client of the request did not start")

			// The worker queues a second connection: the sidecar still
			// reads the live pipe, so the kernel holds this one.
			queued := f.dialPipe(t)
			f.writeFrames(t, queued, map[string]any{"type": "session_shutdown"})
			// /new in pi drops the live pipe.
			_ = agent.Close()

			if err := waitFor(t, f.runDone, "the sidecar did not take the queued session_shutdown"); err != nil {
				t.Fatalf("Run = %v, want nil on the clean-exit path", err)
			}
			if st := readStatus(t, f.dbFile, f.session); st.State != "finished" {
				t.Fatalf("state = %q, want finished from the queued session_shutdown", st.State)
			}

			exited := make(chan struct{})
			go func() {
				removeOwnPIDFileAfterHostAPI(f.session, f.sc.CloseHostAPI)
				close(exited)
			}()
			cleanup := func() {
				if err := cleanupCallers[0].run(t, f.session, ""); err != nil {
					t.Errorf("prism cleanup: %v", err)
				}
			}
			select {
			case <-exited:
				t.Error("the sidecar exited while the podman client of a host-API request ran")
				cleanup()
				f.client.letGo()
				waitFor(t, req, "the host-API request did not end")
			case <-time.After(500 * time.Millisecond):
				if _, err := os.Stat(f.pidPath); err != nil {
					t.Errorf("PID file while the handler runs: %v, want kept", err)
				}
				f.client.letGo()
				waitFor(t, exited, "the sidecar did not exit after the handler ended")
				if _, err := os.Stat(f.pidPath); err == nil {
					t.Error("the PID file stays after the handler ended, want removed")
				}
				res := waitFor(t, req, "the host-API request did not end")
				var started prismcontainer.StartResult
				if res.err != nil || res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &started) != nil || started.ExitCode != 0 {
					t.Errorf("container start: status %d, body %s, err %v; want 200 and exit 0", res.status, res.body, res.err)
				}
				cleanup()
			}
			if ids := f.client.sessionContainers(); len(ids) != 0 {
				t.Errorf("containers of the session after prism cleanup: %v, want none", ids)
			}
		})
	}
}

// AC: /new, /resume, /fork, and quit in pi work as before, in a
// sandbox-exec session (TCP pipe) and a bwrap session (Unix pipe). For
// /new, /resume, and /fork the extension half-closes its connection and
// dials a new one. For quit it closes the connection. No pipe drop makes
// the sidecar exit or close its host API: a request that runs over the
// drop ends with its result, and a later request gets an answer.
func TestSidecarPipeDrop_NewAndQuitKeepTheSidecarAndItsHostAPI_3391(t *testing.T) {
	for _, transport := range []string{"tcp", "unix"} {
		t.Run(transport, func(t *testing.T) {
			f := newCleanExitFixture(t, transport, 0)
			first := f.connectAgent(t)
			req := f.startContainer()
			waitFor(t, f.client.started, "the podman client of the request did not start")

			for _, command := range []string{"/new", "/resume", "/fork"} {
				if err := first.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
					t.Fatalf("%s: half-close: %v", command, err)
				}
				first = f.connectAgent(t)
			}
			f.client.letGo()
			res := waitFor(t, req, "the host-API request did not end")
			if res.err != nil || res.status != http.StatusOK {
				t.Errorf("request over the pipe drops: status %d, body %s, err %v; want 200", res.status, res.body, res.err)
			}

			// quit in pi
			_ = first.Close()
			select {
			case <-f.runEnded:
				t.Fatal("the sidecar exited after quit in pi")
			case <-time.After(300 * time.Millisecond):
			}
			if res := waitFor(t, f.startContainer(), "the request after quit did not end"); res.err != nil || res.status != http.StatusOK {
				t.Errorf("request after quit: status %d, body %s, err %v; want 200", res.status, res.body, res.err)
			}
			if _, err := os.Stat(f.pidPath); err != nil {
				t.Errorf("PID file after quit: %v, want kept", err)
			}
		})
	}
}

// AC: if a host-API handler does not end in the bound, the sidecar logs
// the handler and keeps its PID file, so that a later stop can still find
// its process group. The PID file goes at a later exit check, after the
// handler ended.
func TestSidecarExit_HandlerPastTheBoundKeepsPIDFile_3391(t *testing.T) {
	f := newCleanExitFixture(t, "tcp", 300*time.Millisecond)
	f.connectAgent(t)
	req := f.startContainer()
	waitFor(t, f.client.started, "the podman client of the request did not start")

	stderr := captureStderrDuringFn(t, func() { removeOwnPIDFileAfterHostAPI(f.session, f.sc.CloseHostAPI) })

	if _, err := os.Stat(f.pidPath); err != nil {
		t.Errorf("PID file after the bound: %v, want kept", err)
	}
	for _, want := range []string{strconv.Quote(f.session), "POST /container/start", "the PID file stays"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr)
		}
	}

	f.client.letGo()
	waitFor(t, req, "the host-API request did not end")
	deadline := time.Now().Add(10 * time.Second)
	for f.sc.CloseHostAPI() != nil {
		if time.Now().After(deadline) {
			t.Fatal("CloseHostAPI still reports a handler after the handler ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	removeOwnPIDFileAfterHostAPI(f.session, f.sc.CloseHostAPI)
	if _, err := os.Stat(f.pidPath); err == nil {
		t.Error("the PID file stays after the handler ended, want removed")
	}
}
