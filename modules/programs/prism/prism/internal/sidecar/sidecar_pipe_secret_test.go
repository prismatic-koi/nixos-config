package sidecar

// Tests for #3356: the sidecar refuses a pipe peer that does not show the
// pipe secret of the session. On Darwin the pipe is a loopback TCP port that
// every session can reach. Without the check, one session can read the held
// prompts of another session, or send frames as it.
//
// If the handshake accepts a peer without the secret, the refusal tests fail.
// If a refused peer can change the session (state, events, archive, loop),
// or gets a frame of the session, they fail too.

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// expectRefused reads what the sidecar sends on conn until it closes the
// connection. It must be exactly one error frame with the code unauthorized:
// no hello_ack, and no frame of the session.
func expectRefused(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("%s: the sidecar did not close the connection: %v (read %q)", what, err, all)
	}
	lines := strings.Split(strings.TrimRight(string(all), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%s: the sidecar sent %d frames %q, want one error frame", what, len(lines), lines)
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &f); err != nil {
		t.Fatalf("%s: unmarshal %q: %v", what, lines[0], err)
	}
	if f["type"] != "error" || f["code"] != pipeRefusedCode {
		t.Errorf("%s: frame = %v, want an error frame with code %q", what, f, pipeRefusedCode)
	}
}

// dialPipeAddr dials the pipe of sc, unix or TCP, and retries until the
// listener is up.
func dialPipeAddr(t *testing.T, sc *Sidecar) net.Conn {
	t.Helper()
	network, addr := "unix", sc.cfg.HarnessPipeSockPath
	if sc.cfg.HarnessPipeTCPPort != 0 {
		network, addr = "tcp", fmt.Sprintf("127.0.0.1:%d", sc.cfg.HarnessPipeTCPPort)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout(network, addr, 500*time.Millisecond)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close() })
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial the pipe at %s %s: %v", network, addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSocketPipe_PipeSecret_RefusesPeerWithoutSecret is AC1, for both
// transports. A held prompt waits for the agent. Peers without the secret of
// the session connect: no secret, an empty one, a wrong one, the secret of
// another session, a frame that is not JSON, a frame that is not hello, and
// a bad hello with event frames after it. Each one gets only the refusal.
// None of them changes the state, the events, or the frame archive of the
// session, or ends the pipe loop. Then the agent with the secret connects
// and gets the held prompt (AC3).
func TestSocketPipe_PipeSecret_RefusesPeerWithoutSecret(t *testing.T) {
	for _, transport := range []string{"unix", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			var sc *Sidecar
			if transport == "tcp" {
				sc = newTCPPortSidecar(t, allocateFreePort(t))
			} else {
				sc = newSocketPipeSidecar(t, shortSockPath(t))
			}
			startSocketPipe(t, sc)
			session := sc.cfg.SessionName

			// The first dial waits for the listener. The sidecar writes the
			// secret before it listens, so the file is there now.
			first := dialPipeAddr(t, sc)
			secret := readPipeSecret(t, sc.cfg.HarnessPipeSecretPath)
			stateBefore := getState(t, sc.cfg.DB, session)

			rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
				`{"session":"`+session+`","prompt":"held for the agent","delivery_id":"held-3356"}`)
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"buffered":true`) {
				t.Fatalf("POST /prompt with no agent: status %d, body %s; want 200 and buffered", rr.Code, rr.Body.String())
			}

			otherSecret, err := writeHarnessPipeSecret(filepath.Join(t.TempDir(), "pipe.secret"))
			if err != nil {
				t.Fatalf("make the secret of another session: %v", err)
			}
			noSecret := helloWithSecret("")
			delete(noSecret, "secret")
			badHello, _ := json.Marshal(helloWithSecret("wrong"))
			peers := []struct {
				name  string
				write string
			}{
				{"hello with no secret", mustJSONLine(t, noSecret)},
				{"hello with an empty secret", mustJSONLine(t, helloWithSecret(""))},
				{"hello with a wrong secret", mustJSONLine(t, helloWithSecret(strings.Repeat("0", len(secret))))},
				{"hello with the secret of another session", mustJSONLine(t, helloWithSecret(otherSecret))},
				{"a frame that is not JSON", "this is not json\n"},
				{"a first frame that is not hello", `{"type":"state_change","state":"active"}` + "\n"},
				{"a bad hello and then event frames", string(badHello) + "\n" +
					`{"type":"state_change","state":"active"}` + "\n" +
					`{"type":"tool_call","id":"c1","name":"bash","args":{"command":"id"}}` + "\n" +
					`{"type":"session_shutdown"}` + "\n"},
			}
			for i, p := range peers {
				conn := first
				if i > 0 {
					conn = dialPipeAddr(t, sc)
				}
				if _, err := io.WriteString(conn, p.write); err != nil {
					t.Fatalf("%s: write: %v", p.name, err)
				}
				expectRefused(t, conn, p.name)
			}

			if got := getState(t, sc.cfg.DB, session); got != stateBefore {
				t.Errorf("state after the refused peers = %q, want %q (unchanged)", got, stateBefore)
			}
			for _, typ := range []string{"tool_call", "state_change", "startup_error"} {
				if n := countEvents(t, sc.cfg.DB, session, typ); n != 0 {
					t.Errorf("%d %s events after the refused peers, want 0", n, typ)
				}
			}
			if n, err := sc.cfg.DB.CountHarnessFrames(session); err != nil || n != 0 {
				t.Errorf("harness_frames rows after the refused peers = %d (err %v), want 0", n, err)
			}

			// The agent of the session connects, with the secret.
			agentConn := dialPipeAddr(t, sc)
			sendJSON(t, agentConn, helloWithSecret(secret))
			c := &pipeClient{conn: agentConn, rd: bufio.NewReader(agentConn)}
			c.nextOfType(t, "hello_ack")
			if held := c.nextOfType(t, "prompt"); held["text"] != "held for the agent" || held["replay"] != true {
				t.Errorf("held prompt frame = %v, want the held text with replay=true", held)
			}
			deadline := time.Now().Add(3 * time.Second)
			for countEvents(t, sc.cfg.DB, session, "state_change") == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no state_change event after the agent with the secret connected")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestSocketPipe_PipeSecret_ReconnectTimeout is AC4, the path of #3345:
// after the reconnect timeout the listener stays open with no deadline. A
// peer without the secret is refused and does not reattach the session. The
// agent with the secret then reattaches and gets the held prompt.
func TestSocketPipe_PipeSecret_ReconnectTimeout(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)
	startSocketPipe(t, sc)

	conn, _ := dialAndHandshake(t, sockPath)
	_ = conn.Close()
	waitForStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "reconnect timeout")
	waitDetached(t, sc)

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"testrepo@main","prompt":"held while detached","delivery_id":"held-3356-detached"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("POST /prompt while detached: status %d, body %s; want 200 and buffered", rr.Code, rr.Body.String())
	}

	intruder := dialHelloWithSecret(t, sockPath, "not-the-secret")
	expectRefused(t, intruder.conn, "a hello with a wrong secret after the timeout")
	sc.mu.Lock()
	detached := sc.harnessPipeDetached
	sc.mu.Unlock()
	if !detached {
		t.Error("a refused peer reattached the session")
	}
	if got := getState(t, sc.cfg.DB, sc.cfg.SessionName); got != string(agent.StateError) {
		t.Errorf("state after a refused peer = %q, want %q", got, agent.StateError)
	}

	c := dialHello(t, sockPath)
	c.nextOfType(t, "hello_ack")
	if held := c.nextOfType(t, "prompt"); held["text"] != "held while detached" || held["replay"] != true {
		t.Errorf("held prompt frame = %v, want the held text with replay=true", held)
	}
	waitForStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "agent reconnected")
	if got := getState(t, sc.cfg.DB, sc.cfg.SessionName); got != string(agent.StateActive) {
		t.Errorf("state after the agent reconnected = %q, want %q", got, agent.StateActive)
	}
}

// TestSocketPipe_PipeSecret_NewSecretForEachStart: each start of the pipe
// writes a new secret. A hello with the secret of the previous start is
// refused, and one with the new secret is not.
func TestSocketPipe_PipeSecret_NewSecretForEachStart(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)
	oldSecret := readPipeSecret(t, pipeSecretPathFor(sockPath))
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	_ = conn.Close()
	if err := wait(); err != nil {
		t.Fatalf("first start: %v", err)
	}

	sc2 := New(sc.cfg)
	startSocketPipe(t, sc2)
	stale := dialPipeAddr(t, sc2)
	sendJSON(t, stale, helloWithSecret(oldSecret))
	expectRefused(t, stale, "a hello with the secret of the previous start")
	if newSecret := readPipeSecret(t, pipeSecretPathFor(sockPath)); newSecret == oldSecret {
		t.Fatalf("the second start kept the secret %q", oldSecret)
	}
	c := dialHello(t, sockPath)
	c.nextOfType(t, "hello_ack")
}

// TestSocketPipe_PipeSecret_NoSecretPathFailsClosed: with no secret path the
// pipe does not listen, so no peer can connect without a secret.
func TestSocketPipe_PipeSecret_NoSecretPathFailsClosed(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessPipeSecretPath = ""

	err := sc.runStartupSocketPipe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HarnessPipeSecretPath") {
		t.Fatalf("runStartupSocketPipe with no secret path: err = %v, want a HarnessPipeSecretPath error", err)
	}
	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Errorf("the pipe socket exists with no secret path (stat err %v)", statErr)
	}
	if got := getState(t, sc.cfg.DB, sc.cfg.SessionName); got != string(agent.StateError) {
		t.Errorf("state = %q, want %q", got, agent.StateError)
	}
}

// TestSocketPipe_PipeSecret_ArchiveHasNoSecret: `prism logs` reads the frame
// archive, so the archived hello must not hold the live secret.
func TestSocketPipe_PipeSecret_ArchiveHasNoSecret(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)
	secret := readPipeSecret(t, pipeSecretPathFor(sockPath))
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	_ = conn.Close()
	if err := wait(); err != nil {
		t.Fatalf("runStartupSocketPipe: %v", err)
	}

	frames, err := sc.cfg.DB.QueryHarnessFrames(sc.cfg.SessionName, "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	sawHello := false
	for _, f := range frames {
		if strings.Contains(f.Payload, secret) {
			t.Errorf("archived %s frame %q holds the pipe secret", f.Type, f.Payload)
		}
		if f.Type == "hello" {
			sawHello = true
			if !strings.Contains(f.Payload, pipeSecretRedacted) {
				t.Errorf("archived hello %q has no %q marker", f.Payload, pipeSecretRedacted)
			}
		}
	}
	if !sawHello {
		t.Error("no hello frame in the archive")
	}
}

// TestWriteHarnessPipeSecret_ReplacesPlantedSymlink: the sandbox can write
// the run dir. A symlink that it puts at the secret path is replaced, not
// followed, so the host write cannot reach the target of the link.
func TestWriteHarnessPipeSecret_ReplacesPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dir, "pipe.secret")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	secret, err := writeHarnessPipeSecret(path)
	if err != nil {
		t.Fatalf("writeHarnessPipeSecret: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("the symlink target now holds %q, want it unchanged", b)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %v, want a regular file with mode 0600", info.Mode())
	}
	if b, _ := os.ReadFile(path); string(b) != secret {
		t.Errorf("secret file holds %q, want the returned secret", b)
	}
	if raw, err := hex.DecodeString(secret); err != nil || len(raw) != pipeSecretBytes {
		t.Errorf("secret %q is not %d hex-encoded bytes", secret, pipeSecretBytes)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir holds %d entries after the write, want only pipe.secret", len(entries))
	}
}

// TestWriteHarnessPipeSecret_RefusesSymlinkedDir: if the sandbox puts a
// symlink at its run dir, for example to the run dir of another session, the
// write fails and leaves the target dir as it was.
func TestWriteHarnessPipeSecret_RefusesSymlinkedDir(t *testing.T) {
	otherRunDir := t.TempDir()
	otherSecret := filepath.Join(otherRunDir, "pipe.secret")
	if err := os.WriteFile(otherSecret, []byte("secret-of-the-other-session"), 0o600); err != nil {
		t.Fatalf("write the other secret: %v", err)
	}
	runDir := filepath.Join(t.TempDir(), "abc123def456")
	if err := os.Symlink(otherRunDir, runDir); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if _, err := writeHarnessPipeSecret(filepath.Join(runDir, "pipe.secret")); err == nil {
		t.Fatal("writeHarnessPipeSecret wrote through a symlinked run dir")
	}
	if b, _ := os.ReadFile(otherSecret); string(b) != "secret-of-the-other-session" {
		t.Errorf("the secret file of the other session now holds %q", b)
	}
	if entries, _ := os.ReadDir(otherRunDir); len(entries) != 1 {
		t.Errorf("the other run dir holds %d entries, want only its pipe.secret", len(entries))
	}
}

// TestPipeSecretMatches: an empty expected secret matches nothing, so a
// sidecar with no secret refuses all peers.
func TestPipeSecretMatches(t *testing.T) {
	for _, tc := range []struct {
		got, want string
		match     bool
	}{
		{"abc", "abc", true},
		{"abd", "abc", false},
		{"ab", "abc", false},
		{"", "abc", false},
		{"", "", false},
		{"abc", "", false},
	} {
		if got := pipeSecretMatches(tc.got, tc.want); got != tc.match {
			t.Errorf("pipeSecretMatches(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.match)
		}
	}
}

// mustJSONLine returns v as one JSONL line.
func mustJSONLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b) + "\n"
}
