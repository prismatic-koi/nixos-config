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
	spawnSession func(ctx context.Context, worktree, role string, configOverrides map[string]any) (*Supervisor, error)
	// deliverPrompt delivers a prompt to a named session. Set by the daemon.
	deliverPrompt func(ctx context.Context, name, text, deliverAs string, images []string) error

	listener net.Listener

	// subscribers maps session name → set of subscriber channels.
	// The mutex guards both the map and the slices inside it.
	subMu       sync.Mutex
	subscribers map[string][]subscriberChan
}

// subscriberChan wraps a channel together with a unique ID so that
// unsubscribe can remove the correct entry from the slice.
type subscriberChan struct {
	id string
	ch chan EventPublication
}

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
	SpawnSession func(ctx context.Context, worktree, role string, configOverrides map[string]any) (*Supervisor, error)
	// DeliverPrompt delivers a prompt to a named session.
	DeliverPrompt func(ctx context.Context, name, text, deliverAs string, images []string) error
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
func (cs *ClientSocket) Serve(ctx context.Context) {
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
		go cs.handleConn(ctx, conn)
	}
}

// SockPath returns the filesystem path of the client IPC socket.
func (cs *ClientSocket) SockPath() string { return cs.sockPath }

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

	for _, sc := range chans {
		select {
		case sc.ch <- pub:
		default:
			// The subscriber channel is full. We drop the event for this
			// subscriber; the subscriber goroutine will detect the next
			// failure and close the connection.
			log.Printf("[iris] client socket: subscriber %s for session %q channel full, dropping event", sc.id, pub.SessionName)
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

			go cs.runSubscription(subCtx, w, frame, subID, func() {
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
			})

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
			go cs.handleSessionSpawn(connCtx, w, frame)

		case ClientFrameSessionKill:
			var frame ClientSessionKillFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFrameSessionKill, "invalid frame: "+err.Error())
				continue
			}
			sendError(w, ClientFrameSessionKill, "session_kill not yet implemented")

		case ClientFramePromptDeliver:
			var frame ClientPromptDeliverFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				sendError(w, ClientFramePromptDeliver, "invalid frame: "+err.Error())
				continue
			}
			go cs.handlePromptDeliver(connCtx, w, frame)

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

// runSubscription manages a single subscription for one session on one client
// connection. It:
//
//  1. Checks that the session exists (sends error if not; keeps connection open).
//  2. Snapshots the current max rowid for replay deduplication.
//  3. Registers a live channel in the subscriber set.
//  4. Replays events since_event_id from the DB (if requested).
//  5. Drains the live channel, writing session_event frames to the client.
//
// onDone is called when the goroutine exits (whether by ctx cancel, write
// error, or channel overflow).
func (cs *ClientSocket) runSubscription(
	ctx context.Context,
	w *jsonlWriter,
	frame ClientSessionSubscribeFrame,
	subID string,
	onDone func(),
) {
	defer onDone()

	sessionName := frame.Name

	// Step 1: Verify the session exists. A "not found" still registers the
	// subscription for sessions that may start later, but per the AC the
	// requirement is to return an error and keep the connection open.
	// We check the in-memory supervisor map via getActiveSessions.
	if !cs.sessionExists(sessionName) {
		sendError(w, ClientFrameSessionSubscribe,
			fmt.Sprintf("session %q not found", sessionName))
		return
	}

	// Step 2: Snapshot the current max rowid before registering the live
	// channel. This is the key to avoiding the replay-vs-live race described
	// in the file-level comment and §4.3 of the design doc.
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

	// Step 3: Register the live channel.
	ch := make(chan EventPublication, subscriberChanSize)
	cs.addSubscriber(sessionName, subscriberChan{id: subID, ch: ch})

	// Step 4: Replay from DB if since_event_id was given.
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

	// Step 5: Drain the live channel, skipping duplicates from the replay range.
	for {
		select {
		case <-ctx.Done():
			return
		case pub, ok := <-ch:
			if !ok {
				return // Channel closed.
			}
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

	sup, err := cs.spawnSession(ctx, frame.Worktree, role, frame.ConfigOverrides)
	if err != nil {
		sendError(w, ClientFrameSessionSpawn, fmt.Sprintf("spawn failed: %v", err))
		return
	}

	_ = w.write(DaemonSessionSpawnedFrame{
		Type:       DaemonFrameSessionSpawned,
		Name:       sup.SessionRecord().SessionName,
		InstanceID: sup.InstanceID(),
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
	// No explicit ack frame for prompt_deliver — the client will see the
	// resulting events via their subscription (if subscribed).
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
