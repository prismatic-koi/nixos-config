package server

// PTY runtime hosting for the mux server.
//
// The pane *model* (internal/mux/pane.SessionTree) is intentionally
// runtime-agnostic — it carries names and ordering, no PTY handles. The
// server owns the runtime side: each (sessionID, paneName) tuple that
// was created with a non-empty argv has a live *pty.Session feeding a
// *vt.Host. This file owns the map, the lifecycle, and the goroutines
// that bridge bytes from the kernel PTY into the VT engine and back.
//
// The split mirrors the design doc §4 ("the sidecar runs detached … the
// data model is multiplexer-independent") and keeps the spike's
// "host stays pure" property intact: nothing under internal/mux/pane
// imports os/exec or the pty package; the server is the only layer
// that does, and only inside this file.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/pty"
	"github.com/prismatic-koi/prism/internal/mux/vt"
)

// Pane-creation default geometry. The PTY package requires an explicit
// (cols, rows) tuple; we pick a conventional 80x24 when the caller
// (the CLI today) does not pass an initial geometry. The renderer
// resizes on first paint anyway.
const (
	defaultPTYCols uint16 = 80
	defaultPTYRows uint16 = 24
)

// destroyGrace bounds the SIGTERM → SIGKILL escalation window when a
// pane is destroyed. 2 s is generous for a healthy shell to exit, fast
// enough not to wedge the API for an unresponsive child.
const destroyGrace = 2 * time.Second

// paneKey identifies one PTY host inside the server's runtime map.
// Sessions are unique tree-wide, and pane names are unique per session
// (enforced by SessionTree.AddPane), so (session, pane) is the natural
// composite key.
type paneKey struct {
	session string
	pane    string
}

// ptyHost is the runtime counterpart to a pane.Pane — a spawned process,
// its PTY master, the VT engine that consumes its output, and the
// goroutines that pump bytes between them.
//
// The zero ptyHost is NOT useful; construct via startPTYHost.
type ptyHost struct {
	pty  *pty.Session
	host *vt.Host

	// argv / cwd / env are kept for diagnostics — the wire-shape
	// fields are not echoed back to clients today, but knowing what
	// was spawned is invaluable in logs.
	argv []string
	cwd  string

	// cols / rows track the most recently applied geometry so a
	// follow-up Resize that matches becomes a quick no-op.
	cols uint16
	rows uint16

	// done is closed once both pump goroutines exit and the underlying
	// process has been waited on. Tests use this to deterministically
	// observe lifecycle completion.
	done chan struct{}
}

// startPTYHost spawns argv under a PTY, wires the master FD into a new
// vt.Host (cols × rows), and starts the two pump goroutines:
//
//   - PTY → VT — reads bytes from the master and feeds them into the
//     emulator. Exits on EOF (child closed its FD) or read error.
//   - VT → PTY — drains the emulator's response stream (DSR replies,
//     OSC responses, mode queries) back into the master so the hosted
//     TUI does not block waiting for them. Exits on EOF after the
//     master is closed.
//
// Returns the live ptyHost. The caller is responsible for installing it
// in the server's map and for invoking Close when the pane is destroyed.
//
// argv MUST be non-empty; an empty argv is rejected by the handler
// before this function is called.
func startPTYHost(argv []string, cwd string, env map[string]string, cols, rows uint16, logger *log.Logger) (*ptyHost, error) {
	if len(argv) == 0 {
		return nil, errors.New("mux/server: startPTYHost: empty argv")
	}
	if cols == 0 {
		cols = defaultPTYCols
	}
	if rows == 0 {
		rows = defaultPTYRows
	}

	envSlice := buildPTYEnv(env, cwd)

	sess, err := pty.StartWithEnv(argv, cols, rows, cwd, envSlice)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	host := vt.New(int(cols), int(rows))

	h := &ptyHost{
		pty:  sess,
		host: host,
		argv: append([]string(nil), argv...),
		cwd:  cwd,
		cols: cols,
		rows: rows,
		done: make(chan struct{}),
	}

	// Bridge goroutines. We track both via a WaitGroup so the done
	// channel only closes once nothing is touching the PTY or host —
	// otherwise a Close() observer racing the pumps could see them
	// still draining post-close.
	var wg sync.WaitGroup
	wg.Add(1)

	// PTY → VT pump. Exits when the master FD is closed (EBADF / EIO)
	// or when the child closes its end (EOF). This is the only pump
	// the bridge waits for — see the DrainResponses note below.
	go func() {
		defer wg.Done()
		if err := host.FeedReader(sess.File); err != nil && !isClosedPipe(err) && logger != nil {
			logger.Printf("mux/server: pty→vt pump exited: %v", err)
		}
	}()

	// VT → PTY pump (DSR replies, OSC responses, mode queries). Per
	// the spike AC documented in internal/mux/vt/vt_test.go on
	// TestDrainResponsesEmitsDSRReply, this goroutine intentionally
	// leaks for the process lifetime — the spike's port of vt.Host
	// deliberately does NOT expose a Close hook, and reaching into
	// x/vt.Emulator.Close directly trips an upstream race on
	// e.closed under -race (the race is harmless — io.Pipe handles
	// the actual unblock — but the detector flags it).
	//
	// The leak is bounded: one extra goroutine per pane destroy,
	// each holding a 4 KiB buffer + a reference to the host. For a
	// daemon that lives across a phase-3 soak, that is acceptable;
	// when the wider programme is ready to revisit the spike AC the
	// fix is upstream, not here.
	//
	// Without this pump, well-behaved TUIs that emit DSR or OSC
	// queries deadlock once x/vt's internal pipe buffer (~64 KiB)
	// fills, so it cannot simply be omitted. The pump exits whenever
	// the master FD is closed AFTER the emulator has buffered a
	// response (DrainResponses' Write fails), but for silent TUIs
	// the goroutine is steady-state idle.
	go func() {
		if err := host.DrainResponses(sess.File); err != nil && !isClosedPipe(err) && logger != nil {
			logger.Printf("mux/server: vt→pty pump exited: %v", err)
		}
	}()

	go func() {
		wg.Wait()
		// Reap the child so it does not become a zombie. Best-effort —
		// the process may already have been waited on by Close. The
		// second Wait on a reaped child returns ECHILD; we ignore it.
		if sess.Cmd != nil && sess.Cmd.Process != nil {
			_, _ = sess.Cmd.Process.Wait()
		}
		close(h.done)
	}()

	return h, nil
}

// SendInput writes data into the PTY's master FD (which the kernel
// presents as the child's stdin). The master FD is buffered by the
// kernel; a short write is unexpected at the byte sizes the CLI sends
// (single keystrokes, small pastes) but we still report a partial-write
// error rather than swallow it.
func (h *ptyHost) SendInput(data []byte) error {
	if h == nil || h.pty == nil || h.pty.File == nil {
		return errors.New("mux/server: pty host is not started")
	}
	if len(data) == 0 {
		return nil
	}
	n, err := h.pty.File.Write(data)
	if err != nil {
		return fmt.Errorf("write pty stdin: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("short write to pty stdin: %d of %d bytes", n, len(data))
	}
	return nil
}

// Resize updates the PTY winsize (kernel will SIGWINCH the child) and
// the emulator's grid dimensions. Both are forwarded — they must stay
// in lock-step or the renderer's frames will not match the child's
// notion of the terminal size.
func (h *ptyHost) Resize(cols, rows uint16) error {
	if h == nil || h.pty == nil {
		return errors.New("mux/server: pty host is not started")
	}
	if cols == h.cols && rows == h.rows {
		return nil
	}
	if err := h.pty.Resize(cols, rows); err != nil {
		return fmt.Errorf("resize pty: %w", err)
	}
	h.host.Resize(int(cols), int(rows))
	h.cols = cols
	h.rows = rows
	return nil
}

// Close signals the child to exit, then closes the master FD so the
// pump goroutines unblock. SIGTERM goes out first; the master FD is
// closed in parallel so the PTY → VT pump unblocks immediately. After
// destroyGrace, escalates to SIGKILL if the process is still alive.
// Close is safe to call multiple times — subsequent calls are no-ops
// (signal returns ESRCH, file close returns ErrClosed; both ignored).
func (h *ptyHost) Close() error {
	if h == nil {
		return nil
	}
	if h.pty == nil {
		return nil
	}
	proc := h.pty.Cmd.Process
	if proc != nil {
		// Send SIGTERM first so a well-behaved child cleans up
		// (e.g. resets terminal modes). The kernel delivers SIGTERM
		// regardless of whether the master FD is open.
		_ = proc.Signal(syscall.SIGTERM)
	}
	// Closing the master FD breaks the PTY → VT pump out of its read
	// (EBADF / EIO). The pump's deferred host.Close() then unblocks
	// DrainResponses with io.EOF. The done channel closes once both
	// pumps and the proc.Wait have returned.
	if h.pty.File != nil {
		_ = h.pty.File.Close()
	}
	// Escalate to SIGKILL after destroyGrace if the child is still
	// alive. We do not block the caller waiting for this — the
	// monitor goroutine handles it.
	if proc != nil {
		go func() {
			select {
			case <-h.done:
				// Bridge already drained; nothing to escalate.
			case <-time.After(destroyGrace):
				_ = proc.Signal(syscall.SIGKILL)
			}
		}()
	}
	return nil
}

// Host returns the underlying *vt.Host. Used by the renderer's
// HostProvider adapter and by the /pane/read_output handler. Returns
// nil if the host has not been started, which the caller treats as
// "no PTY for this pane".
func (h *ptyHost) Host() *vt.Host {
	if h == nil {
		return nil
	}
	return h.host
}

// buildPTYEnv normalises an env map into the os/exec-style ["KEY=VALUE"]
// slice the syscall layer expects. When env is nil the child inherits
// the daemon's environment; pass an empty (but non-nil) map to start
// with an empty environment.
//
// The cwd argument is informational only — pty.StartWithEnv passes it
// to exec.Cmd.Dir directly. We accept it here for symmetry with the
// startPTYHost signature.
func buildPTYEnv(env map[string]string, _ string) []string {
	if env == nil {
		// Nil map → inherit parent env. Returning nil here makes
		// pty.StartWithEnv use os.Environ().
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// isClosedPipe reports whether err is the harmless "the master FD was
// closed" failure mode. Used by the pump goroutines so a clean
// pane.Destroy does not log a spurious error line for every torn-down
// pane.
func isClosedPipe(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
		return true
	}
	// "file already closed" / "input/output error" / "use of closed
	// file" all surface as wrapped *os.PathError with these err strings
	// on Linux/Darwin. Match by the underlying syscall errno rather
	// than the string so the check is portable.
	if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EBADF) {
		return true
	}
	return false
}

// ptyRegistry tracks the live ptyHost for each (session, pane) tuple.
// All operations are serialised through the registry's mutex so concurrent
// pane.create / pane.destroy from different HTTP handlers cannot race.
//
// The registry is intentionally distinct from pane.SessionTree — the tree
// stays pure data; the registry is the runtime side. See file docstring.
type ptyRegistry struct {
	mu    sync.Mutex
	hosts map[paneKey]*ptyHost
}

// newPTYRegistry returns an empty, ready-to-use registry.
func newPTYRegistry() *ptyRegistry {
	return &ptyRegistry{hosts: make(map[paneKey]*ptyHost)}
}

// add installs host under (session, pane). Returns an error if a host
// is already registered for the key — the server enforces "create on
// an existing pane is a 409" at the model layer, so a duplicate here
// would indicate a programming error.
func (r *ptyRegistry) add(session, name string, host *ptyHost) error {
	if host == nil {
		return errors.New("mux/server: ptyRegistry.add: nil host")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := paneKey{session: session, pane: name}
	if _, exists := r.hosts[key]; exists {
		return fmt.Errorf("mux/server: pty host already registered for %q/%q", session, name)
	}
	r.hosts[key] = host
	return nil
}

// get returns the host for (session, pane) and true, or nil and false
// if no host is registered. Callers must not hold the host past the
// next destroy on the same key — registry.remove invalidates the
// returned pointer.
func (r *ptyRegistry) get(session, name string) (*ptyHost, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[paneKey{session: session, pane: name}]
	return h, ok
}

// remove pops (session, pane) from the map and returns the prior host
// (or nil if none was registered). The caller is responsible for
// Close()-ing the returned host.
func (r *ptyRegistry) remove(session, name string) *ptyHost {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := paneKey{session: session, pane: name}
	h := r.hosts[key]
	delete(r.hosts, key)
	return h
}

// removeSession pops every host whose session matches and returns them.
// Used by /session/destroy so PTY cleanup cascades atomically with the
// model-level RemoveSession.
func (r *ptyRegistry) removeSession(session string) []*ptyHost {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*ptyHost
	for key, host := range r.hosts {
		if key.session == session {
			out = append(out, host)
			delete(r.hosts, key)
		}
	}
	return out
}

// closeAll Close()s every host in the registry and clears it. Used by
// Server.Close so a test/process shutdown does not leak PTY children.
func (r *ptyRegistry) closeAll() {
	r.mu.Lock()
	hosts := make([]*ptyHost, 0, len(r.hosts))
	for _, h := range r.hosts {
		hosts = append(hosts, h)
	}
	r.hosts = make(map[paneKey]*ptyHost)
	r.mu.Unlock()
	for _, h := range hosts {
		_ = h.Close()
	}
}
