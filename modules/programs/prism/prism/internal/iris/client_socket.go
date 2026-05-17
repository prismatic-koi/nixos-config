package iris

// client_socket.go — user-facing client IPC socket for the iris daemon.
//
// The ClientSocket binds to ~/.local/state/iris/iris.sock (mode 0600, parent
// dir 0700) and accepts multiple simultaneous client connections. Each
// connection is an independent client session on the same daemon.
//
// # Protocol
//
// Framing is JSON-line (§4.2 of daemon-mode-design.md). The client sends
// request frames (sessions_list, session_subscribe, session_unsubscribe,
// session_spawn, session_kill, prompt_deliver, ping) and the daemon pushes
// response and event frames back on the same connection.
//
// # Fan-out
//
// The daemon maintains a per-session subscriber set. When a new event is
// written to the DB via the harness socket (D-3), the caller also invokes
// ClientSocket.Publish to broadcast the event to all subscribers of that
// session. Each subscriber goroutine reads from a buffered channel and
// writes session_event frames to its client connection.
//
// # since_event_id replay
//
// A client that reconnects after a gap can send session_subscribe with a
// since_event_id field. The handler:
//
//  1. Snapshots the maximum rowid currently in the DB for that session.
//  2. Queries the DB for events with rowid > since_event_id (up to the
//     snapshot max) — this is the "catch-up" range.
//  3. Registers the live channel in the subscriber set.
//  4. Streams the catch-up range as session_event frames.
//  5. Switches to live mode, draining the channel.
//
// This avoids the replay-vs-live race: any event that arrives during the
// DB query and is written to the channel before the subscriber was registered
// would be missed by a naive implementation. By snapshotting first and then
// registering, we ensure the channel receives all events from that point
// forward, and the replay covers up to the snapshot. Events with rowid ≤
// snapshot are served from the DB; events with rowid > snapshot come from
// the live channel. Duplicates (an event arriving on the channel with rowid ≤
// snapshot) are discarded by the rowid guard in the drain step.
//
// See also: daemon-mode-design.md §4.3.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// subscriberChanSize is the buffer size for per-subscriber event channels.
// A full channel causes events to be dropped for that subscriber (the
// subscriber is too slow or the connection is backed up). The subscriber
// goroutine logs and disconnects on overflow rather than blocking the
// broadcaster.
const subscriberChanSize = 256

// EventPublication is the unit passed through the fan-out channels. It
// carries enough information to write a DaemonSessionEventFrame.
type EventPublication struct {
	SessionName string
	RowID       int64
	EventType   string
	Payload     string
}

// subscriberMessage is the wrapper type carried on per-subscriber channels.
// Exactly one of Event or State is populated. State carries a session-state
// transition ("spawning" -> "active" -> "finished"|"error"); Event carries a
// session_event from the harness. Splitting these in a union avoids the cost
// of a separate channel per subscriber.
type subscriberMessage struct {
	Event *EventPublication
	State string // "" when this message is an event
}

// ClientSocket is the user-facing IPC socket. It binds to the iris.sock path
// and manages per-client connection goroutines and the per-session subscriber
// map for fan-out.
type ClientSocket struct {
	sockPath string
	database *db.DB
	// getActiveSessions returns the current in-memory session list. This is
	// set by the daemon to a function that reads the Supervisor map.
	getActiveSessions func() []SessionSnapshot
	// spawnSession spawns a new session. Set by the daemon at construction.
	// parent is the spawning session's logical name (from the session_spawn
	// frame's Parent field; empty for top-level spawns). See #1700.
	// sessionName, when non-empty, fixes the new session's logical name
	// (overriding the daemon's GenerateSessionName default). Used by
	// `iris investigate` to enforce the `<parent>~investigate-<slug>` shape.
	spawnSession func(ctx context.Context, sessionName, worktree, role, parent string, configOverrides map[string]any) (*Supervisor, error)
	// deliverPrompt delivers a prompt to a named session. Set by the daemon.
	deliverPrompt func(ctx context.Context, name, text, deliverAs string, images []string) error
	// killSession terminates a named session. Returns the terminal state
	// reached ("finished", "error", "already_terminal") on success. Set by
	// the daemon at construction.
	killSession func(ctx context.Context, name string, timeout time.Duration) (string, error)
	// spawnReviewGroup orchestrates a `iris review` request: spawns the
	// review agents, registers the group, and starts the completion
	// watcher. Set by the daemon at construction (or via SetSpawnReviewGroup
	// when the daemon needs to capture a reference to the ClientSocket).
	// Returns the ack frame ready to be written to the calling client.
	spawnReviewGroup func(ctx context.Context, req ClientReviewSpawnFrame) (*DaemonReviewSpawnedFrame, error)
	// escalateSession transitions the named session from active to
	// escalated. Set by the daemon at construction — wraps
	// Supervisor.Escalate so the client socket does not need a direct
	// reference to the supervisor map. Used by handleEscalationDeliver
	// (issue #1693).
	escalateSession func(name string) error
	// resumeSession transitions the named session from escalated back to
	// active. Called by handlePromptDeliver immediately after a successful
	// deliver so that ANY incoming prompt (coordinator, human via
	// `iris prompt`, future TUI) clears the escalated state — mirrors
	// prism's escalated→active rule on any turn_start (#1693).
	resumeSession func(name string)
	// roleOf returns the agent role ("worker", "coordinator", ...) of the
	// named session, or "" when the session is unknown. Set by the daemon
	// at construction. Used by handleEscalationDeliver to validate that
	// --to targets a coordinator session.
	roleOf func(name string) string

	listener net.Listener

	// subscribers maps session name → set of subscriber channels.
	// The mutex guards both the map and the slices inside it.
	subMu       sync.Mutex
	subscribers map[string][]subscriberChan

	// wg tracks every goroutine the ClientSocket spawns: Serve, each
	// per-connection handleConn, each runSubscription, and the per-frame
	// handlers (handleSessionSpawn / handleSessionKill / handlePromptDeliver
	// / handleReviewSpawn / handleEscalationDeliver). Wait() blocks until
	// all of them return and is the canonical drain point for test cleanup
	// (issue #1705) — callers that own the DB or tempdir must Wait before
	// closing those resources, otherwise late writes from runSubscription
	// or a frame handler race against teardown.
	wg sync.WaitGroup
}

// subscriberChan wraps a channel together with a unique ID so that
// unsubscribe can remove the correct entry from the slice.
type subscriberChan struct {
	id string
	ch chan subscriberMessage
}

// PublisherFunc is an adapter that allows a plain function to be used as an
// EventPublisher. Useful in tests where a full ClientSocket is not needed.
type PublisherFunc func(EventPublication)

// Publish implements EventPublisher.
func (f PublisherFunc) Publish(pub EventPublication) { f(pub) }

// ClientSocketConfig holds the parameters for constructing a ClientSocket.
type ClientSocketConfig struct {
	// SockPath is the absolute path for the Unix domain socket.
	// Typically p.Sock from ResolvePaths().
	SockPath string
	// Database is the open iris DB (used for sessions_list and replay queries).
	Database *db.DB
	// GetActiveSessions returns the current in-memory session list.
	GetActiveSessions func() []SessionSnapshot
	// SpawnSession spawns a new session and returns the supervisor.
	// parent is the spawning session's logical name (issue #1700, forwarded
	// from the session_spawn frame's Parent field). Empty for top-level
	// spawns invoked from outside an iris session. sessionName, when
	// non-empty, fixes the new session's logical name (overriding the
	// daemon's GenerateSessionName default); used by `iris investigate`
	// to enforce the `<parent>~investigate-<slug>` shape.
	SpawnSession func(ctx context.Context, sessionName, worktree, role, parent string, configOverrides map[string]any) (*Supervisor, error)
	// DeliverPrompt delivers a prompt to a named session.
	DeliverPrompt func(ctx context.Context, name, text, deliverAs string, images []string) error
	// KillSession terminates a named session. Returns the terminal state
	// ("finished" / "error" / "already_terminal") on success.
	KillSession func(ctx context.Context, name string, timeout time.Duration) (string, error)
	// SpawnReviewGroup orchestrates an `iris review` request. Set by the
	// daemon; the implementation lives in review_handler.go.
	SpawnReviewGroup func(ctx context.Context, req ClientReviewSpawnFrame) (*DaemonReviewSpawnedFrame, error)
	// EscalateSession transitions the named session from active to
	// escalated (issue #1693). Optional: when nil, escalation_deliver is
	// rejected with "not configured". The daemon wires this to a closure
	// over the supervisor map.
	EscalateSession func(name string) error
	// ResumeSession transitions the named session from escalated back to
	// active. Called by handlePromptDeliver immediately after a successful
	// deliver so that any incoming prompt resumes a paused worker. Safe
	// no-op when the session is not in escalated state.
	ResumeSession func(name string)
	// RoleOf returns the agent role of the named session, or "" when
	// unknown. Used by handleEscalationDeliver to validate --to targets.
	RoleOf func(name string) string
}

// NewClientSocket creates a ClientSocket. Call Listen() to bind the socket,
// then Serve() in a goroutine to accept connections.
func NewClientSocket(cfg ClientSocketConfig) *ClientSocket {
	return &ClientSocket{
		sockPath:          cfg.SockPath,
		database:          cfg.Database,
		getActiveSessions: cfg.GetActiveSessions,
		spawnSession:      cfg.SpawnSession,
		deliverPrompt:     cfg.DeliverPrompt,
		killSession:       cfg.KillSession,
		spawnReviewGroup:  cfg.SpawnReviewGroup,
		escalateSession:   cfg.EscalateSession,
		resumeSession:     cfg.ResumeSession,
		roleOf:            cfg.RoleOf,
		subscribers:       make(map[string][]subscriberChan),
	}
}

// Listen binds the Unix domain socket. The parent directory is created with
// mode 0700 if it does not exist; the socket inode is chmod'd 0600.
//
// Any stale socket file from a previous daemon incarnation is removed before
// binding (os.Remove is idempotent and safe).
func (cs *ClientSocket) Listen() error {
	// Ensure the parent directory exists with 0700.
	parent := dirOf(cs.sockPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("iris: client socket: mkdir %q: %w", parent, err)
	}

	// Remove stale socket from a previous daemon run.
	_ = os.Remove(cs.sockPath)

	ln, err := net.Listen("unix", cs.sockPath)
	if err != nil {
		return fmt.Errorf("iris: client socket listen %q: %w", cs.sockPath, err)
	}
	// Enforce 0600 on the socket inode (filesystem-level access control
	// per §4.1 and §6 of daemon-mode-design.md).
	if err := os.Chmod(cs.sockPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("iris: client socket chmod %q: %w", cs.sockPath, err)
	}
	cs.listener = ln
	log.Printf("[iris] client socket listening at %s", cs.sockPath)
	return nil
}

// Serve accepts client connections in a loop until ctx is cancelled. Each
// accepted connection is handled in its own goroutine. Serve is intended to
// be called in its own goroutine.
//
// Serve itself, every spawned handleConn, and every goroutine those handlers
// spawn (runSubscription, handle*Frame) are tracked in cs.wg so test
// scaffolding can Wait() for all of them to drain before tearing down the
// DB / tempdir (issue #1705).
func (cs *ClientSocket) Serve(ctx context.Context) {
	cs.wg.Add(1)
	defer cs.wg.Done()
	for {
		conn, err := cs.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // Normal shutdown.
			}
			log.Printf("[iris] client socket accept error: %v", err)
			// Brief pause to avoid a tight loop on persistent accept errors.
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		cs.wg.Add(1)
		go func(conn net.Conn) {
			defer cs.wg.Done()
			cs.handleConn(ctx, conn)
		}(conn)
	}
}

// Wait blocks until every goroutine spawned by this ClientSocket has
// returned: the Serve accept loop, every per-connection handleConn, every
// runSubscription, and every per-frame handler. It is the canonical drain
// point for test cleanup (issue #1705) — callers that own the DB or the
// tempdir backing the iris paths must Wait before closing those resources.
//
// Wait does NOT cancel anything itself. Callers must first cancel the
// context passed to Serve and Close() the listener so the goroutines
// actually exit; Wait only blocks until they do.
//
// Production callers in the iris daemon do not need to call Wait — the
// daemon's process lifetime owns these goroutines and they exit when the
// daemon does. Wait exists for test scaffolding.
func (cs *ClientSocket) Wait() {
	cs.wg.Wait()
}

// SockPath returns the filesystem path of the client IPC socket.
func (cs *ClientSocket) SockPath() string { return cs.sockPath }

// SetSpawnSession wires the spawn function after construction. This is used
// by the daemon when it needs to capture a reference to the ClientSocket
// inside the spawn function (circular dependency: spawnFn needs clientSock,
// clientSock needs spawnFn). Call before Serve().
func (cs *ClientSocket) SetSpawnSession(fn func(ctx context.Context, sessionName, worktree, role, parent string, configOverrides map[string]any) (*Supervisor, error)) {
	cs.spawnSession = fn
}

// SetKillSession wires the kill function after construction. Symmetric with
// SetSpawnSession; call before Serve() when the daemon's killFn captures a
// reference to the ClientSocket.
func (cs *ClientSocket) SetKillSession(fn func(ctx context.Context, name string, timeout time.Duration) (string, error)) {
	cs.killSession = fn
}

// SetSpawnReviewGroup wires the review-spawn orchestrator after construction.
// Symmetric with SetSpawnSession; call before Serve() when the daemon's
// review orchestrator captures references that depend on the ClientSocket.
func (cs *ClientSocket) SetSpawnReviewGroup(fn func(ctx context.Context, req ClientReviewSpawnFrame) (*DaemonReviewSpawnedFrame, error)) {
	cs.spawnReviewGroup = fn
}

// Database returns the underlying *db.DB. Exposed for the review-handler
// orchestrator, which is constructed by the daemon and needs to thread the
// same DB handle that the client socket uses for sessions_list, etc.
func (cs *ClientSocket) Database() *db.DB { return cs.database }

// Close closes the listener and removes the socket file.
func (cs *ClientSocket) Close() {
	if cs.listener != nil {
		cs.listener.Close()
	}
	_ = os.Remove(cs.sockPath)
}

// Publish broadcasts an event to all clients subscribed to the named session.
// It is called by the harness socket handler (and, in future, by the
// supervisor's RPC event reader) every time a new event is written to the DB.
// Publish never blocks: slow subscribers receive a dropped-event log and are
// removed from the subscriber set on their next send attempt.
func (cs *ClientSocket) Publish(pub EventPublication) {
	cs.subMu.Lock()
	chans := make([]subscriberChan, len(cs.subscribers[pub.SessionName]))
	copy(chans, cs.subscribers[pub.SessionName])
	cs.subMu.Unlock()

	event := pub // copy so the pointer below is stable for this iteration
	msg := subscriberMessage{Event: &event}
	for _, sc := range chans {
		select {
		case sc.ch <- msg:
		default:
			// The subscriber channel is full. We drop the event for this
			// subscriber; the subscriber goroutine will detect the next
			// failure and close the connection.
			log.Printf("[iris] client socket: subscriber %s for session %q channel full, dropping event", sc.id, pub.SessionName)
		}
	}
}

// PublishState broadcasts a session_state transition to all clients
// subscribed to the named session. Implements stateNotifier so the supervisor
// can deliver state changes without coupling to the ClientSocket concrete
// type. State is one of the SessionState string values ("spawning",
// "active", "finished", "error").
func (cs *ClientSocket) PublishState(sessionName, state string) {
	cs.subMu.Lock()
	chans := make([]subscriberChan, len(cs.subscribers[sessionName]))
	copy(chans, cs.subscribers[sessionName])
	cs.subMu.Unlock()

	msg := subscriberMessage{State: state}
	for _, sc := range chans {
		select {
		case sc.ch <- msg:
		default:
			// Channel full — the slow subscriber will be cleaned up on its
			// next event. Dropping a state transition is acceptable here:
			// the caller (e.g. `iris logs --follow`) can fall back to
			// sessions_list to recover. Log so this is observable.
			log.Printf("[iris] client socket: subscriber %s for session %q channel full, dropping state %q", sc.id, sessionName, state)
		}
	}
}

// --- Connection handler ---

// handleConn manages a single client connection for its lifetime.
// It reads request frames from the client and dispatches them. Multiple
// session subscriptions may be active simultaneously on one connection.
func (cs *ClientSocket) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	addr := conn.RemoteAddr().String()
	log.Printf("[iris] client socket: connection opened (%s)", addr)
	defer log.Printf("[iris] client socket: connection closed (%s)", addr)

	// cancelConn cancels all goroutines spawned for this connection when the
	// connection closes (either by client disconnect or ctx cancellation).
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	reader := bufio.NewReaderSize(conn, 16*1024*1024)
	w := &jsonlWriter{conn: conn}

	// activeSubscriptions tracks the subscriber channel IDs registered for
	// this connection, keyed by session name. Used for unsubscribe and for
	// cleanup on disconnect.
	type subEntry struct {
		id     string
		cancel context.CancelFunc
	}
	var subMu sync.Mutex
	activeSubs := make(map[string]subEntry) // session name → {id, cancel}

	cleanupSubs := func() {
		subMu.Lock()
		defer subMu.Unlock()
		for name, entry := range activeSubs {
			cs.removeSubscriber(name, entry.id)
			entry.cancel()
		}
		activeSubs = make(map[string]subEntry)
	}
	defer cleanupSubs()

	for {
		line, err := readLine(reader)
		if err != nil {
			return // Connection closed or error.
		}

		var generic GenericFrame
		if err := json.Unmarshal(line, &generic); err != nil {
			log.Printf("[iris] client socket: parse error: %v", err)
			_ = w.write(DaemonErrorFrame{
				Type:        DaemonFrameError,
				RequestType: "unknown",
				Message:     fmt.Sprintf("invalid JSON: %v", err),
			})
			continue
		}

		switch generic.Type {

		case ClientFrameSessionsList:
			cs.handleSessionsList(w)

		case ClientFrameSessionSubscribe:
			var frame ClientSessionSubscribeFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameSessionSubscribe, "invalid frame: "+err.Error())
				continue
			}
			subMu.Lock()
			if _, exists := activeSubs[frame.Name]; exists {
				// Already subscribed — ignore (idempotent).
				subMu.Unlock()
				continue
			}
			subCtx, subCancel := context.WithCancel(connCtx)
			subID := uuid.New().String()
			activeSubs[frame.Name] = subEntry{id: subID, cancel: subCancel}
			subMu.Unlock()

			onDone := func() {
				// Called by the subscription goroutine when done (client
				// disconnect or ctx cancel). Remove from the local map and
				// the global subscriber set.
				subMu.Lock()
				if e, ok := activeSubs[frame.Name]; ok && e.id == subID {
					delete(activeSubs, frame.Name)
				}
				subMu.Unlock()
				cs.removeSubscriber(frame.Name, subID)
				subCancel()
			}

			// Register the live subscriber synchronously, before reading the
			// next frame on this connection. This is what makes the
			// session_subscribe → next-frame ordering on the wire a true
			// happens-before relationship for Publish: any frame the client
			// sends after observing a response to a subsequent request (e.g.
			// ping/pong) is guaranteed to see the subscriber registered. If
			// setupSubscription fails (session not found, etc.) it has
			// already sent the error frame; we run onDone to clear the
			// local activeSubs entry and continue.
			subState, ok := cs.setupSubscription(w, frame, subID)
			if !ok {
				onDone()
				continue
			}
			cs.wg.Add(1)
			go func() {
				defer cs.wg.Done()
				cs.runSubscription(subCtx, w, frame, subState, onDone)
			}()

		case ClientFrameSessionUnsubscribe:
			var frame ClientSessionUnsubscribeFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameSessionUnsubscribe, "invalid frame: "+err.Error())
				continue
			}
			subMu.Lock()
			if entry, ok := activeSubs[frame.Name]; ok {
				cs.removeSubscriber(frame.Name, entry.id)
				entry.cancel()
				delete(activeSubs, frame.Name)
			}
			subMu.Unlock()

		case ClientFrameSessionSpawn:
			var frame ClientSessionSpawnFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameSessionSpawn, "invalid frame: "+err.Error())
				continue
			}
			cs.wg.Add(1)
			go func(frame ClientSessionSpawnFrame) {
				defer cs.wg.Done()
				cs.handleSessionSpawn(connCtx, w, frame)
			}(frame)

		case ClientFrameSessionKill:
			var frame ClientSessionKillFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameSessionKill, "invalid frame: "+err.Error())
				continue
			}
			cs.wg.Add(1)
			go func(frame ClientSessionKillFrame) {
				defer cs.wg.Done()
				cs.handleSessionKill(connCtx, w, frame)
			}(frame)

		case ClientFramePromptDeliver:
			var frame ClientPromptDeliverFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFramePromptDeliver, "invalid frame: "+err.Error())
				continue
			}
			cs.wg.Add(1)
			go func(frame ClientPromptDeliverFrame) {
				defer cs.wg.Done()
				cs.handlePromptDeliver(connCtx, w, frame)
			}(frame)

		case ClientFrameReviewSpawn:
			var frame ClientReviewSpawnFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameReviewSpawn, "invalid frame: "+err.Error())
				continue
			}
			cs.wg.Add(1)
			go func(frame ClientReviewSpawnFrame) {
				defer cs.wg.Done()
				cs.handleReviewSpawn(connCtx, w, frame)
			}(frame)

		case ClientFrameEscalationDeliver:
			var frame ClientEscalationDeliverFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameEscalationDeliver, "invalid frame: "+err.Error())
				continue
			}
			cs.wg.Add(1)
			go func(frame ClientEscalationDeliverFrame) {
				defer cs.wg.Done()
				cs.handleEscalationDeliver(connCtx, w, frame)
			}(frame)

		case ClientFramePing:
			_ = w.write(DaemonPongFrame{Type: DaemonFramePong})

		default:
			// Unknown frame — log and skip (forward-compatibility).
			log.Printf("[iris] client socket: unknown frame type %q (skipping)", generic.Type)
		}
	}
}

// --- Request handlers ---

// handleSessionsList writes a sessions_snapshot frame to the client.
func (cs *ClientSocket) handleSessionsList(w *jsonlWriter) {
	var sessions []SessionSnapshot
	if cs.getActiveSessions != nil {
		sessions = cs.getActiveSessions()
	}
	if sessions == nil {
		sessions = []SessionSnapshot{}
	}
	_ = w.write(DaemonSessionsSnapshotFrame{
		Type:     DaemonFrameSessionsSnapshot,
		Sessions: sessions,
	})
}

// subscriptionState carries the result of setupSubscription into the
// runSubscription goroutine. It captures the snapshot rowid (for replay
// deduplication) and the live channel (registered in the subscriber set).
type subscriptionState struct {
	ch            chan subscriberMessage
	snapshotRowID int64
}

// setupSubscription performs the synchronous portion of subscribing a client
// to a session: verifying the session exists, snapshotting the max rowid for
// replay deduplication, and registering the live channel in the subscriber
// set. It runs on the connection's reader goroutine so that by the time it
// returns, any subsequent frame the client sends (and any response the
// client observes from this socket) is guaranteed to see the subscriber
// registered. This is what eliminates the subscribe-then-publish race that
// plagued tests using a fixed time.Sleep as their readiness barrier.
//
// Returns ok=false if the session does not exist; an error frame has
// already been written to the client.
func (cs *ClientSocket) setupSubscription(
	w *jsonlWriter,
	frame ClientSessionSubscribeFrame,
	subID string,
) (subscriptionState, bool) {
	sessionName := frame.Name

	// Verify the session exists. A "not found" still keeps the connection
	// open, per the documented contract for session_subscribe.
	if !cs.sessionExists(sessionName) {
		sendError(w, ClientFrameSessionSubscribe,
			fmt.Sprintf("session %q not found", sessionName))
		return subscriptionState{}, false
	}

	// Snapshot the current max rowid before registering the live channel.
	// This is the key to avoiding the replay-vs-live race described in the
	// file-level comment and §4.3 of the design doc.
	//
	// Timeline:
	//   T0: snapshotRowID = MAX(rowid) = N
	//   T1: register live channel (events from T1 onward arrive on ch)
	//   T2: replay DB events with rowid > sinceRowID AND rowid <= N
	//   T3: drain ch, skipping events with rowid <= N (already replayed)
	//
	// This guarantees no event in the window [sinceRowID, ∞) is missed,
	// at the cost of potentially duplicating events in [N+1, first-ch-item]
	// which the rowid guard in the drain step eliminates.
	snapshotRowID, err := cs.database.MaxSessionEventRowID(sessionName)
	if err != nil {
		log.Printf("[iris] client socket: snapshot rowid for %q: %v", sessionName, err)
		snapshotRowID = 0
	}

	// Register the live channel. After this returns, Publish() for this
	// session will see the subscriber.
	ch := make(chan subscriberMessage, subscriberChanSize)
	cs.addSubscriber(sessionName, subscriberChan{id: subID, ch: ch})

	return subscriptionState{ch: ch, snapshotRowID: snapshotRowID}, true
}

// runSubscription handles the asynchronous portion of a subscription: the
// optional since_event_id replay from the DB, followed by the long-running
// drain of the live channel. setupSubscription must have been called
// successfully before this goroutine starts; the live channel and snapshot
// rowid are passed in via subscriptionState.
//
// onDone is called when the goroutine exits (whether by ctx cancel, write
// error, or channel overflow).
func (cs *ClientSocket) runSubscription(
	ctx context.Context,
	w *jsonlWriter,
	frame ClientSessionSubscribeFrame,
	state subscriptionState,
	onDone func(),
) {
	defer onDone()

	sessionName := frame.Name
	ch := state.ch
	snapshotRowID := state.snapshotRowID

	// Replay from DB if since_event_id was given.
	if frame.SinceEventID > 0 {
		events, err := cs.database.QuerySessionEventsSinceRowID(sessionName, frame.SinceEventID)
		if err != nil {
			log.Printf("[iris] client socket: replay query for %q since %d: %v",
				sessionName, frame.SinceEventID, err)
		}
		for _, er := range events {
			// Only replay up to the snapshot to avoid duplicating live events.
			if er.RowID > snapshotRowID {
				break
			}
			frame := DaemonSessionEventFrame{
				Type:        DaemonFrameSessionEvent,
				SessionName: sessionName,
				RowID:       er.RowID,
				EventType:   er.Event.Type,
				Payload:     er.Event.Payload,
			}
			if err := w.write(frame); err != nil {
				return // Client disconnected during replay.
			}
		}
	}

	// Drain the live channel, skipping duplicates from the replay range.
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return // Channel closed.
			}
			if msg.State != "" {
				stateFrame := DaemonSessionStateFrame{
					Type:        DaemonFrameSessionState,
					SessionName: sessionName,
					State:       msg.State,
				}
				if err := w.write(stateFrame); err != nil {
					return // Client disconnected.
				}
				continue
			}
			if msg.Event == nil {
				continue
			}
			pub := *msg.Event
			// Skip events that were already sent during the DB replay.
			if frame.SinceEventID > 0 && pub.RowID <= snapshotRowID {
				continue
			}
			evtFrame := DaemonSessionEventFrame{
				Type:        DaemonFrameSessionEvent,
				SessionName: sessionName,
				RowID:       pub.RowID,
				EventType:   pub.EventType,
				Payload:     pub.Payload,
			}
			if err := w.write(evtFrame); err != nil {
				return // Client disconnected.
			}
		}
	}
}

// handleSessionSpawn spawns a new pi session and writes a session_spawned ack.
func (cs *ClientSocket) handleSessionSpawn(ctx context.Context, w *jsonlWriter, frame ClientSessionSpawnFrame) {
	if cs.spawnSession == nil {
		sendError(w, ClientFrameSessionSpawn, "spawn not configured on this daemon instance")
		return
	}
	if frame.Worktree == "" {
		sendError(w, ClientFrameSessionSpawn, "worktree is required")
		return
	}
	role := frame.Role
	if role == "" {
		role = "worker"
	}

	// If the caller supplied an explicit session name, reject if a session
	// with that name is already active. This is the equivalent of prism's
	// `session already exists` guard at SpawnSession's tmux check; we run
	// it at the daemon boundary so the CLI returns a clear error instead
	// of a low-level conflict from the supervisor map.
	if frame.SessionName != "" && cs.sessionExists(frame.SessionName) {
		sendError(w, ClientFrameSessionSpawn,
			fmt.Sprintf("session %q is already active", frame.SessionName))
		return
	}

	sup, err := cs.spawnSession(ctx, frame.SessionName, frame.Worktree, role, frame.Parent, frame.ConfigOverrides)
	if err != nil {
		sendError(w, ClientFrameSessionSpawn, fmt.Sprintf("spawn failed: %v", err))
		return
	}

	rec := sup.SessionRecord()
	snap := SessionSnapshot{
		Name:       rec.SessionName,
		InstanceID: sup.InstanceID(),
		State:      string(rec.State),
		Role:       rec.Role,
		Worktree:   rec.Worktree,
		StartedAt:  rec.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	_ = w.write(DaemonSessionSpawnedFrame{
		Type:       DaemonFrameSessionSpawned,
		Name:       snap.Name,
		InstanceID: snap.InstanceID,
		Session:    &snap,
	})
}

// killSessionTimeout is the SIGTERM grace period the daemon applies on every
// session_kill frame. It matches the AC for issue #1674: SIGTERM, 5s wait,
// then SIGKILL escalation.
const killSessionTimeout = 5 * time.Second

// handleSessionKill terminates a named session and writes a session_killed
// ack back to the client. Errors (no-such-session, kill timed out, no kill
// function wired) come back as DaemonErrorFrame so the client can distinguish
// success from failure on the same conn.
//
// Idempotent: a session already in a terminal state returns success with
// state="already_terminal" without re-killing.
func (cs *ClientSocket) handleSessionKill(ctx context.Context, w *jsonlWriter, frame ClientSessionKillFrame) {
	if cs.killSession == nil {
		sendError(w, ClientFrameSessionKill, "session kill not configured on this daemon instance")
		return
	}
	if frame.Name == "" {
		sendError(w, ClientFrameSessionKill, "name is required")
		return
	}
	if !cs.sessionExists(frame.Name) {
		sendError(w, ClientFrameSessionKill, fmt.Sprintf("session %q not found", frame.Name))
		return
	}
	state, err := cs.killSession(ctx, frame.Name, killSessionTimeout)
	if err != nil {
		sendError(w, ClientFrameSessionKill, fmt.Sprintf("kill failed: %v", err))
		return
	}
	_ = w.write(DaemonSessionKilledFrame{
		Type:  DaemonFrameSessionKilled,
		Name:  frame.Name,
		State: state,
	})
}

// handlePromptDeliver delivers a prompt to a named session.
func (cs *ClientSocket) handlePromptDeliver(ctx context.Context, w *jsonlWriter, frame ClientPromptDeliverFrame) {
	if cs.deliverPrompt == nil {
		sendError(w, ClientFramePromptDeliver, "prompt delivery not configured on this daemon instance")
		return
	}
	if frame.Name == "" {
		sendError(w, ClientFramePromptDeliver, "name is required")
		return
	}
	if frame.Text == "" {
		sendError(w, ClientFramePromptDeliver, "text is required")
		return
	}

	if err := cs.deliverPrompt(ctx, frame.Name, frame.Text, frame.DeliverAs, frame.Images); err != nil {
		sendError(w, ClientFramePromptDeliver, fmt.Sprintf("deliver failed: %v", err))
		return
	}
	// Issue #1693: any prompt_deliver to a session resumes it from
	// escalated→active. This fires regardless of source (coordinator's
	// reply, a human via `iris prompt`, or any other path) — mirroring
	// prism's escalated→active rule on any turn_start. Resume is a safe
	// no-op when the session is not currently escalated.
	if cs.resumeSession != nil {
		cs.resumeSession(frame.Name)
	}
	// No explicit ack frame for prompt_deliver — the client will see the
	// resulting events via their subscription (if subscribed).
}

// handleReviewSpawn handles a review_spawn frame: validates the request,
// invokes the daemon-provided review orchestrator, and writes a
// review_spawned ack (or an error frame) back to the calling client.
//
// The orchestrator (cs.spawnReviewGroup) is responsible for the heavy work:
// registering the group, spawning the per-agent sessions via the daemon's
// session-spawn machinery, and launching the completion watcher. This
// handler only validates the wire frame and translates orchestrator
// errors into DaemonErrorFrame messages.
func (cs *ClientSocket) handleReviewSpawn(ctx context.Context, w *jsonlWriter, frame ClientReviewSpawnFrame) {
	if cs.spawnReviewGroup == nil {
		sendError(w, ClientFrameReviewSpawn, "review spawn not configured on this daemon instance")
		return
	}
	if frame.Parent == "" {
		sendError(w, ClientFrameReviewSpawn, "parent is required")
		return
	}
	if frame.PRNumber == "" {
		sendError(w, ClientFrameReviewSpawn, "pr_number is required")
		return
	}
	if !cs.sessionExists(frame.Parent) {
		sendError(w, ClientFrameReviewSpawn,
			fmt.Sprintf("not a registered iris session: %q (run `iris sessions list` to verify)", frame.Parent))
		return
	}
	ack, err := cs.spawnReviewGroup(ctx, frame)
	if err != nil {
		sendError(w, ClientFrameReviewSpawn, err.Error())
		return
	}
	_ = w.write(ack)
}

// handleEscalationDeliver implements the worker-side of `iris escalate`
// (issue #1693). The frame's From field names the calling worker session;
// To, when set, names an explicit coordinator target. When To is empty the
// daemon auto-discovers the coordinator by scanning in-memory active
// sessions for Role == "coordinator".
//
// Discovery rules (match prism's `prism escalate` byte-for-byte):
//
//   - exactly one coordinator candidate → deliver to it.
//   - multiple candidates without To  → reject with --to-required error
//     listing candidates. The worker stays in active state.
//   - zero candidates without To       → transition worker to escalated
//     and write a self-marker event; no prompt is delivered. Returns the
//     escalation_delivered ack with delivered=false. A human is expected
//     to pick up the worker via tmux.
//
// Delivery uses the same path as prompt_deliver (deliverPrompt) so the
// coordinator's existing event stream receives the body. A delivery_id is
// minted per call (issue #1695 exactly-once-with-replay-marker contract) so
// if the path ever grows a retry mechanism the underlying harness can
// dedup. The wire ack carries the delivery_id so callers can correlate.
func (cs *ClientSocket) handleEscalationDeliver(ctx context.Context, w *jsonlWriter, frame ClientEscalationDeliverFrame) {
	if frame.From == "" {
		sendError(w, ClientFrameEscalationDeliver, "from is required")
		return
	}
	if frame.Prompt == "" {
		sendError(w, ClientFrameEscalationDeliver, "prompt is required")
		return
	}
	if !cs.sessionExists(frame.From) {
		sendError(w, ClientFrameEscalationDeliver,
			fmt.Sprintf("not a registered iris session: %q has no active supervisor", frame.From))
		return
	}
	if cs.escalateSession == nil {
		sendError(w, ClientFrameEscalationDeliver, "escalation not configured on this daemon instance")
		return
	}

	deliveryID := frame.DeliveryID
	if deliveryID == "" {
		deliveryID = uuid.New().String()
	}

	// Resolve the target.
	target, derr := cs.resolveEscalationTarget(frame.From, frame.To)
	if derr != nil {
		// Discovery error: do NOT transition state — the worker stays in
		// active state per the AC for multiple-candidates and bad --to.
		sendError(w, ClientFrameEscalationDeliver, derr.Error())
		return
	}

	// Transition the worker to escalated BEFORE attempting delivery so the
	// state change is observable even if the coordinator-side delivery
	// fails. Mirrors prism's ordering in cmd/escalate.go.
	if err := cs.escalateSession(frame.From); err != nil {
		sendError(w, ClientFrameEscalationDeliver, fmt.Sprintf("transition to escalated: %v", err))
		return
	}

	// Write the session.escalated bus event into agent_events for the
	// calling worker. Subscribed clients see this on the worker's event
	// stream; the bus event is the notification (mirrors the prism behaviour
	// of session.escalated suppressing the regular session.finished ping).
	cs.writeSessionEscalatedEvent(frame.From, target, frame.Prompt, deliveryID)

	if target == "" {
		// Zero-coordinator branch: the worker is paused; no delivery happens.
		// A human is expected to attend via tmux + `iris prompt`.
		_ = w.write(DaemonEscalationDeliveredFrame{
			Type:       DaemonFrameEscalationDelivered,
			From:       frame.From,
			To:         "",
			DeliveryID: "",
			Delivered:  false,
		})
		return
	}

	// Deliver to the coordinator using the same dispatch path as
	// prompt_deliver. The daemon's deliverPrompt is naturally exactly-once
	// (one call → one SendRPC at the supervisor); we still mint a
	// delivery_id for the audit trail and surface it in the ack frame so
	// any future cross-daemon retry layer can dedup.
	if cs.deliverPrompt == nil {
		sendError(w, ClientFrameEscalationDeliver, "prompt delivery not configured on this daemon instance")
		return
	}
	if err := cs.deliverPrompt(ctx, target, frame.Prompt, "followUp", nil); err != nil {
		sendError(w, ClientFrameEscalationDeliver,
			fmt.Sprintf("deliver to coordinator %q failed: %v", target, err))
		return
	}

	_ = w.write(DaemonEscalationDeliveredFrame{
		Type:       DaemonFrameEscalationDelivered,
		From:       frame.From,
		To:         target,
		DeliveryID: deliveryID,
		Delivered:  true,
	})
}

// resolveEscalationTarget applies the discovery rules. Returns:
//
//   - ("<name>", nil)        — explicit --to or single auto-discovered coord.
//   - ("", nil)              — zero coordinators (the caller should still
//     transition to escalated and ack with delivered=false).
//   - ("", err)              — multiple candidates without --to, or --to
//     names a session that is not a coordinator,
//     or --to names a session that does not exist.
func (cs *ClientSocket) resolveEscalationTarget(from, explicitTo string) (string, error) {
	if explicitTo != "" {
		if explicitTo == from {
			return "", fmt.Errorf("--to %q refers to the calling session; escalate cannot target self", explicitTo)
		}
		if !cs.sessionExists(explicitTo) {
			return "", fmt.Errorf("--to session %q is not a registered iris session (run `iris sessions list` to verify)", explicitTo)
		}
		role := ""
		if cs.roleOf != nil {
			role = cs.roleOf(explicitTo)
		}
		if role != "coordinator" {
			return "", fmt.Errorf("--to session %q is not a coordinator (role=%q); escalate must target a coordinator session", explicitTo, role)
		}
		return explicitTo, nil
	}

	// Auto-discovery: scan in-memory active sessions for coordinators.
	var candidates []string
	for _, s := range cs.getActiveSessions() {
		if s.Name == from {
			continue
		}
		if s.Role == "coordinator" {
			candidates = append(candidates, s.Name)
		}
	}
	switch len(candidates) {
	case 0:
		return "", nil
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple coordinator candidates found; pass --to to choose one: %v", candidates)
	}
}

// writeSessionEscalatedEvent writes a `session.escalated` row into
// agent_events for the calling worker. This is the iris analogue of prism's
// session.escalated bus event — the notification that an escalation occurred
// (distinct from `session_end` / `session.finished`).
//
// Failures are logged but never propagated: the wire-level success path
// depends on the prompt delivery, not on whether the audit row landed. The
// worker is already in escalated state by the time this runs.
func (cs *ClientSocket) writeSessionEscalatedEvent(from, to, prompt, deliveryID string) {
	if cs.database == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"source":      from,
		"target":      to,
		"prompt":      prompt,
		"delivery_id": deliveryID,
	})
	if err != nil {
		log.Printf("[iris] client socket: marshal session.escalated payload: %v", err)
		return
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: from,
		Type:        "session.escalated",
		Payload:     string(payload),
		CreatedAt:   time.Now(),
	}
	rowID, err := cs.database.WriteEventReturningRowID(ev)
	if err != nil {
		log.Printf("[iris] client socket: write session.escalated event: %v", err)
		return
	}
	// Fan out to subscribers so live consumers see the bus event in real
	// time without needing a separate notification path.
	cs.Publish(EventPublication{
		SessionName: from,
		RowID:       rowID,
		EventType:   "session.escalated",
		Payload:     string(payload),
	})
}

// --- Subscriber set management ---

// addSubscriber registers a channel in the per-session subscriber set.
func (cs *ClientSocket) addSubscriber(sessionName string, sc subscriberChan) {
	cs.subMu.Lock()
	defer cs.subMu.Unlock()
	cs.subscribers[sessionName] = append(cs.subscribers[sessionName], sc)
}

// removeSubscriber removes a subscriber by ID from the per-session set.
// It is safe to call even if the subscriber is not present (idempotent).
func (cs *ClientSocket) removeSubscriber(sessionName, subID string) {
	cs.subMu.Lock()
	defer cs.subMu.Unlock()
	chans := cs.subscribers[sessionName]
	out := chans[:0]
	for _, sc := range chans {
		if sc.id != subID {
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		delete(cs.subscribers, sessionName)
	} else {
		cs.subscribers[sessionName] = out
	}
}

// sessionExists returns true if the session is currently active in the
// in-memory supervisor map.
func (cs *ClientSocket) sessionExists(name string) bool {
	if cs.getActiveSessions == nil {
		return false
	}
	for _, s := range cs.getActiveSessions() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// --- Helpers ---

// sendError writes a DaemonErrorFrame to the client. Write errors are logged
// but do not propagate — the caller's read loop will detect the disconnect
// on the next read.
func sendError(w *jsonlWriter, requestType, message string) {
	_ = w.write(DaemonErrorFrame{
		Type:        DaemonFrameError,
		RequestType: requestType,
		Message:     message,
	})
}

// dirOf returns the directory component of a file path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}
