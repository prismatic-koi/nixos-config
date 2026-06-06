package cmd

// prism launch — PRISM_USE_MUX=1 path (issue #2176).
//
// When PRISM_USE_MUX=1 is set, runLaunch dispatches here instead of
// creating tmux scratchpad+dashboard sessions. This file owns the
// renderer wire-up: it constructs the §3.1 bubbletea renderer
// (internal/mux/render), seeds its providers (state.Subscriber for
// sidebar colours, a client-side ReadOutput poller for active-pane
// content), and runs the program against the user's terminal.
//
// Architecture:
//
//   - One *pane.SessionTree shared between the renderer and a
//     reconcile-loop. The reconcile loop polls the daemon's
//     Sessions().List() every refreshInterval, diffs against the
//     tree, and applies AddSession / RemoveSession / AddPane to keep
//     them in lock-step.
//
//   - One state.Store + one state.Subscriber per active session.
//     Each subscriber tails its session's sidecar /events stream.
//     The reconcile loop spawns subscribers for new sessions and
//     cancels them when the session disappears.
//
//   - A clientHostProvider that maintains one *vt.Host per
//     (sessionID, paneName) the renderer asks about. A background
//     goroutine polls the daemon's /pane/read_output for every cached
//     entry every refreshInterval and re-feeds the vt.Host so
//     RenderRows() reflects the latest frame.
//
//   - All goroutines are bound by a single context tied to the
//     bubbletea program's lifetime. tea.Quit cancels the context
//     which unwinds every poller, subscriber, and reconciler.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/mux/client"
	"github.com/prismatic-koi/prism/internal/mux/pane"
	"github.com/prismatic-koi/prism/internal/mux/render"
	"github.com/prismatic-koi/prism/internal/mux/state"
	"github.com/prismatic-koi/prism/internal/mux/vt"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// muxLaunchRefreshInterval bounds how often the launch path polls the
// daemon for session-list and pane-output deltas. 100 ms is short enough
// to feel live for an interactive operator but long enough not to flood
// the daemon's socket with redundant GETs. Tuneable; the renderer is
// repainted on every successful poll so a slower interval just means
// stale frames.
const muxLaunchRefreshInterval = 100 * time.Millisecond

// muxLaunchHTTPTimeout bounds a single GET round-trip to the daemon's
// /pane/read_output or /session/list endpoint. Long enough that a
// momentarily-busy daemon does not flap into "(no PTY)"; short enough
// that a wedged daemon does not stall the reconcile loop.
const muxLaunchHTTPTimeout = 2 * time.Second

// runLaunchMux is the PRISM_USE_MUX=1 entry point for `prism launch`.
// It connects to the local mux daemon, builds a renderer Model wired
// to a live SessionTree + state.Store + clientHostProvider, and runs
// the bubbletea program in the calling terminal.
//
// Returns an error in three cases:
//
//   - daemon unreachable (surfaces the canonical
//     daemonNotRunningError diagnostic so the operator sees the
//     supervisor hint)
//   - tea.Program.Run failure
//   - reconcile-loop initial-list failure not covered by the
//     daemon-not-running check (best-effort: we still launch the
//     program; the sidebar simply shows "0 sessions" until the
//     reconcile recovers)
func runLaunchMux() error {
	mc, err := newMuxClient()
	if err != nil {
		return surfaceDaemonError("prism launch", err)
	}
	defer mc.Close()

	// Probe the daemon once up-front so a stopped daemon surfaces the
	// canonical hint immediately rather than after the program loop is
	// running (a tea.Program eating the error would be a worse UX than
	// a clean exit).
	probeCtx, probeCancel := context.WithTimeout(context.Background(), muxLaunchHTTPTimeout)
	if _, err := mc.Sessions().List(probeCtx); err != nil {
		probeCancel()
		return surfaceDaemonError("prism launch", err)
	}
	probeCancel()

	tree := pane.New()
	store := state.New(tree)

	hostProvider := newClientHostProvider(mc)

	model := render.New(
		tree,
		render.WithStates(newStateAdapter(store)),
		render.WithHosts(hostProvider),
	)

	prog := tea.NewProgram(model, tea.WithAltScreen())

	// All background goroutines share one context tied to the
	// program's lifetime. Cancelled in the defer below so a tea.Quit
	// cleanly unwinds every poller.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscriber manager: one state.Subscriber per active session.
	// Spawned/cancelled by the reconcile loop as sessions appear and
	// disappear from the daemon's session list.
	mgr := newSubscriberManager(ctx, store)

	// Reconcile loop: polls Sessions().List on a tick, diffs against
	// the tree, applies the diff, and starts/stops subscribers.
	go runReconcileLoop(ctx, mc, tree, mgr, prog)

	// Host poll loop: polls /pane/read_output for every cached host
	// on a tick, feeds the bytes into the corresponding *vt.Host, and
	// sends a redraw message to the program.
	go hostProvider.run(ctx, prog)

	// On every store transition send a redraw message so the sidebar
	// glyph for the affected session repaints "within one frame of an
	// agent_events row change" (AC #2176).
	store.AddListener(func() { prog.Send(muxRedrawMsg{}) })

	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("prism launch: tea.Program.Run: %w", err)
	}
	return nil
}

// muxRedrawMsg is a no-op message used to nudge bubbletea into
// repainting after a background goroutine updated the store or a
// cached host. The renderer's Update() does not need to handle it
// explicitly — any tea.Msg triggers a View() pass.
type muxRedrawMsg struct{}

// ── State adapter: agent.AgentState → render.State ─────────────────

// stateAdapter implements render.StateProvider by reading from a
// state.Store. The Store keeps per-session agent.AgentState; the
// renderer wants the render.State enum. The mapping is the canonical
// vocabulary from agent/agent.go translated into the renderer's int
// enum from render/state.go.
type stateAdapter struct {
	store *state.Store
}

func newStateAdapter(store *state.Store) *stateAdapter {
	return &stateAdapter{store: store}
}

// State implements render.StateProvider.
func (a *stateAdapter) State(sessionID string) render.State {
	if a == nil || a.store == nil {
		return render.StateIdle
	}
	st, ok := a.store.SessionState(sessionID)
	if !ok {
		return render.StateIdle
	}
	switch st {
	case agent.StateActive:
		return render.StateActive
	case agent.StateWaiting:
		return render.StateWaiting
	case agent.StateReviewing:
		return render.StateReviewing
	case agent.StateEscalated:
		return render.StateEscalated
	case agent.StateFinished:
		return render.StateFinished
	default:
		// idle, compacting, error, interrupted, deleted → idle glyph.
		// The renderer treats any unknown state as StateIdle (zero
		// value) which is the desired UX for "session exists but is
		// not actively doing anything".
		return render.StateIdle
	}
}

// ── Subscriber manager ────────────────────────────────────────────

// subscriberManager owns one state.Subscriber goroutine per active
// session. The reconcile loop calls ensure(id) on every session it
// sees in the daemon's list and stop(id) on every session that
// disappears.
type subscriberManager struct {
	ctx   context.Context
	store *state.Store

	mu   sync.Mutex
	subs map[string]context.CancelFunc
}

func newSubscriberManager(ctx context.Context, store *state.Store) *subscriberManager {
	return &subscriberManager{
		ctx:   ctx,
		store: store,
		subs:  make(map[string]context.CancelFunc),
	}
}

// ensure spawns a Subscriber for sessionID if one is not already
// running. The Subscriber's cancellation is bound to the manager's
// parent context so a manager-level shutdown unwinds every goroutine.
//
// Each Subscriber connects to its session's per-session host-API
// socket — there is no daemon-wide events socket in the current
// architecture (every prism session has its own sidecar; see
// internal/session/sidecar.go's SidecarHostAPIPath).
func (m *subscriberManager) ensure(sessionID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[sessionID]; ok {
		return
	}
	sockPath, err := prismSession.SidecarHostAPIPath(sessionID)
	if err != nil {
		// The session's sidecar may not have started yet, or the
		// path resolution failed. Try again on the next reconcile
		// tick by leaving the map entry unset.
		return
	}
	subCtx, cancel := context.WithCancel(m.ctx)
	sub := &state.Subscriber{
		SockPath: sockPath,
		Sessions: []string{sessionID},
		Store:    m.store,
	}
	m.subs[sessionID] = cancel
	go func() {
		// Run blocks until subCtx fires. Errors surface to the
		// default logger; the launch path itself does not log
		// directly so the renderer's altscreen is not polluted.
		_ = sub.Run(subCtx)
	}()
}

// stop cancels the Subscriber for sessionID, if one is running. Safe
// to call on an unknown ID (no-op).
func (m *subscriberManager) stop(sessionID string) {
	m.mu.Lock()
	cancel, ok := m.subs[sessionID]
	if ok {
		delete(m.subs, sessionID)
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ── Reconcile loop ────────────────────────────────────────────────

// runReconcileLoop polls the daemon's Sessions().List on a tick,
// diffs against the SessionTree, and applies the difference.
//
// New sessions in the daemon are AddSession'd into the tree (and a
// Subscriber is spawned for state). Sessions that disappear from the
// daemon are RemoveSession'd and their Subscriber stopped. Existing
// sessions whose pane list grew are AddPane'd.
//
// The loop sends a muxRedrawMsg after every successful pass so the
// sidebar repaints with the latest session set even when no state
// event flowed (e.g. the first paint after a fresh launch).
func runReconcileLoop(ctx context.Context, mc client.MuxClient, tree *pane.SessionTree, mgr *subscriberManager, prog *tea.Program) {
	ticker := time.NewTicker(muxLaunchRefreshInterval)
	defer ticker.Stop()

	// Run one pass synchronously so the program's first paint is
	// against the daemon's current state, not an empty tree.
	reconcileOnce(ctx, mc, tree, mgr)
	if prog != nil {
		prog.Send(muxRedrawMsg{})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOnce(ctx, mc, tree, mgr)
			if prog != nil {
				prog.Send(muxRedrawMsg{})
			}
		}
	}
}

// reconcileOnce is one pass of the reconcile loop, broken out for
// clarity. Fetches Sessions().List, diffs against the tree, applies
// AddSession / RemoveSession / AddPane, and reconciles the subscriber
// set.
func reconcileOnce(ctx context.Context, mc client.MuxClient, tree *pane.SessionTree, mgr *subscriberManager) {
	listCtx, cancel := context.WithTimeout(ctx, muxLaunchHTTPTimeout)
	defer cancel()
	list, err := mc.Sessions().List(listCtx)
	if err != nil {
		// Transient daemon error — leave the tree alone and retry
		// on the next tick. Subscribers continue running against
		// their per-session sockets independently.
		return
	}

	wanted := make(map[string]struct{}, len(list.Sessions))
	for _, s := range list.Sessions {
		wanted[s.ID] = struct{}{}
		// AddSession is idempotent at the "already exists" level
		// — we suppress that error and apply the pane diff below.
		if !tree.HasSession(s.ID) {
			toAdd := s
			// Strip ActivePane on add — the server's view may set
			// it but the tree's AddSession rejects a non-empty
			// ActivePane that does not resolve to a pane it has
			// been given. Adding panes one-by-one below re-derives
			// the active pane.
			toAdd.ActivePane = ""
			toAdd.Panes = nil
			_ = tree.AddSession(toAdd)
		}
		// Diff the pane list and add any new panes. Removing
		// panes mid-session is out of scope for this poll loop —
		// the daemon does not advertise pane removals through
		// Sessions().List in a way the tree can apply without a
		// full rebuild.
		existing := map[string]struct{}{}
		if existingSess, ok := tree.Session(s.ID); ok {
			for _, p := range existingSess.Panes {
				existing[p.Name] = struct{}{}
			}
		}
		for _, p := range s.Panes {
			if _, ok := existing[p.Name]; ok {
				continue
			}
			_ = tree.AddPane(s.ID, p)
		}
		// Activate the daemon's preferred pane explicitly. This
		// is a best-effort op: if the pane name is unknown to the
		// tree it returns ErrPaneNotFound, which we swallow.
		if s.ActivePane != "" {
			_ = tree.ActivatePane(s.ID, s.ActivePane)
		}
		mgr.ensure(s.ID)
	}

	// Remove sessions that vanished. tree.Sessions() returns a
	// snapshot, so this is safe to range over while calling
	// RemoveSession.
	for _, existing := range tree.Sessions() {
		if _, keep := wanted[existing.ID]; keep {
			continue
		}
		_ = tree.RemoveSession(existing.ID)
		mgr.stop(existing.ID)
	}

	// Activate the daemon's tree-level focus pointer if it has one.
	// Same best-effort semantics as the pane activation above.
	if list.ActiveSession != "" {
		_ = tree.ActivateSession(list.ActiveSession)
	}
}

// ── Client-side HostProvider ──────────────────────────────────────

// clientHostProvider implements render.HostProvider by maintaining a
// *vt.Host per (sessionID, paneName) and re-feeding it from the
// daemon's /pane/read_output endpoint on a tick.
//
// The renderer's Host() contract is "return the same *vt.Host across
// successive calls for the same key" — the Model dereferences the
// returned host every View(), so the value must be stable. We use a
// sync.Map-shaped cache keyed by sessionID+"\x00"+paneName.
//
// The provider is intentionally one-frame-behind: the renderer reads
// whatever the most recent poll cached. This is acceptable for the
// soak — the daemon's /pane/read_output endpoint is documented as
// "lossier than the in-process path but sufficient for the phase-3
// soak" (internal/mux/server/output.go).
type clientHostProvider struct {
	mc client.MuxClient

	mu    sync.RWMutex
	hosts map[string]*vt.Host
}

func newClientHostProvider(mc client.MuxClient) *clientHostProvider {
	return &clientHostProvider{
		mc:    mc,
		hosts: make(map[string]*vt.Host),
	}
}

// hostKey is the cache key — sessionID + NUL + paneName. The NUL
// byte separates the components so a pane named "a/b" in a session
// "x" cannot collide with a pane named "b" in a session "x/a". NUL
// is never a valid byte in either field so the encoding is
// unambiguous.
func hostKey(sessionID, paneName string) string {
	return sessionID + "\x00" + paneName
}

// Host implements render.HostProvider. Returns the cached *vt.Host
// for (sessionID, paneName), constructing an empty one on first call.
// The first call also triggers an immediate async poll so the next
// View() pass has content.
func (p *clientHostProvider) Host(sessionID, paneName string) *vt.Host {
	if sessionID == "" || paneName == "" {
		return nil
	}
	key := hostKey(sessionID, paneName)
	p.mu.RLock()
	h, ok := p.hosts[key]
	p.mu.RUnlock()
	if ok {
		return h
	}
	// First-call path: install an empty host so subsequent View()
	// passes get the cached value, then kick off an immediate refresh.
	p.mu.Lock()
	h, ok = p.hosts[key]
	if !ok {
		h = vt.New(80, 24)
		p.hosts[key] = h
	}
	p.mu.Unlock()
	// Async refresh so the next View() pass has live bytes. We do
	// not block the renderer on the round-trip.
	go p.refreshOne(context.Background(), sessionID, paneName, h)
	return h
}

// refreshOne fetches the latest PaneFrame for (sessionID, paneName)
// and re-feeds it into host. The feed sequence is:
//
//  1. Resize host to PaneFrame.Cols × PaneFrame.Rows so RenderRows()
//     returns the right row count for the renderer's truncation.
//  2. Clear and home: `\x1b[H\x1b[2J`.
//  3. Feed the rendered lines joined by `\r\n`.
//
// This rebuilds the cell grid from the daemon's already-rendered
// snapshot. Cursor position and attributes are NOT preserved — the
// /pane/read_output endpoint's response shape carries them but the
// renderer's renderHostFrame only inspects Size() and RenderRows(),
// so a faithful textual replay is sufficient for the soak.
func (p *clientHostProvider) refreshOne(ctx context.Context, sessionID, paneName string, host *vt.Host) {
	if host == nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, muxLaunchHTTPTimeout)
	defer cancel()
	frame, err := p.mc.Panes().ReadOutput(reqCtx, sessionID, paneName)
	if err != nil {
		return
	}
	cols := frame.Cols
	rows := frame.Rows
	if cols <= 0 || rows <= 0 {
		// Pane has no PTY yet (model-only row). Leave the host as
		// last-seen so the renderer shows the previous frame
		// rather than flashing the placeholder.
		return
	}
	host.Resize(cols, rows)
	// Reset + home + clear so each refresh starts from a known
	// state. Without the reset, repeated feeds would scroll
	// content off the top of the emulator's grid.
	var buf bytes.Buffer
	buf.WriteString("\x1bc")    // RIS — full reset
	buf.WriteString("\x1b[H")   // home
	buf.WriteString("\x1b[2J")  // clear screen
	for i, line := range frame.Lines {
		if i > 0 {
			buf.WriteString("\r\n")
		}
		buf.WriteString(line)
	}
	_, _ = host.Feed(buf.Bytes())
}

// run is the host-poll background goroutine. Every tick, walks the
// cached host set and refreshes each one in parallel-bounded chunks.
// Sends a muxRedrawMsg after the pass so the renderer picks up the
// new frames.
func (p *clientHostProvider) run(ctx context.Context, prog *tea.Program) {
	ticker := time.NewTicker(muxLaunchRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshAll(ctx)
			if prog != nil {
				prog.Send(muxRedrawMsg{})
			}
		}
	}
}

// refreshAll polls every cached host once. Synchronous — the next
// tick fires only after this pass completes. For the soak's typical
// cardinality (≤ a dozen panes per developer) the cumulative
// round-trip time stays well under the tick interval.
func (p *clientHostProvider) refreshAll(ctx context.Context) {
	p.mu.RLock()
	keys := make([]string, 0, len(p.hosts))
	hosts := make([]*vt.Host, 0, len(p.hosts))
	for k, h := range p.hosts {
		keys = append(keys, k)
		hosts = append(hosts, h)
	}
	p.mu.RUnlock()
	for i, key := range keys {
		sessionID, paneName, ok := splitHostKey(key)
		if !ok {
			continue
		}
		p.refreshOne(ctx, sessionID, paneName, hosts[i])
	}
}

// splitHostKey is the inverse of hostKey — splits sessionID and
// paneName on the NUL separator.
func splitHostKey(key string) (sessionID, paneName string, ok bool) {
	idx := strings.IndexByte(key, '\x00')
	if idx < 0 {
		return "", "", false
	}
	return key[:idx], key[idx+1:], true
}
