package main

// notify_parent.go — wires the daemon-side parent-notification callback that
// Supervisor.setState invokes on a terminal state transition (issue #1700).
//
// The iris analogue of prism's sidecar `notifyParentWorker`: when an iris
// session reaches StateFinished or StateError, the daemon delivers a
// body-bearing prompt to the parent session that called `iris spawn` (the
// `Parent` field on the session_spawn wire frame, captured from
// IRIS_SESSION_NAME at spawn time and stored on sessions.parent_session).
//
// Wording matches prism byte-for-byte so a coordinator that has been
// trained on the prism phrasing does not have to learn iris-specific text:
//
//	"Agent <name> has finished its current task"   — StateFinished
//	"Agent <name> has errored its current task"    — StateError
//
// Delivery is via the daemon's in-process deliverPrompt callback (which
// ultimately calls Supervisor.SendRPC against the parent's pi child). This
// is naturally exactly-once at the daemon boundary; the delivery_id is still
// minted per call so the audit row (session.parent_notified) can be
// correlated with the underlying prompt frame should an out-of-process
// dedup layer ever be added (#1695 contract).
//
// Edge cases:
//
//   - Parent already cleaned up: the supervisor map lookup misses; the call
//     is a no-op. Logged at info level for observability.
//   - Kill path: Supervisor.Kill transitions through setState (#1692), so
//     SIGTERM and SIGKILL-induced terminals both fire the notification.
//   - D-9 restore path: the restore code does NOT call setState for
//     sessions that were already terminal pre-restart, so a daemon restart
//     does not re-fire notifications. The mid-restore re-spawn does call
//     setState (spawning → active → terminal as usual), in which case a
//     genuine new terminal fires exactly one notification.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// deliverPromptFn matches the signature of the daemon's deliverFn closure
// (see runDaemon in main.go). It is the in-process prompt-delivery surface
// that wraps Supervisor.SendRPC.
type deliverPromptFn func(ctx context.Context, name, text, deliverAs string, images []string) error

// makeNotifyParent returns a closure suitable for
// iris.SupervisorConfig.NotifyParent. The closure resolves the parent
// supervisor against the daemon's in-memory map, formats the prism-verbatim
// notification body, delivers it via deliverFn, and writes an audit row.
//
// ctx is the daemon root context (used for the deliver call). state is the
// daemon's supervisor map. deliverFn is the same deliverFn the client socket
// uses for `iris prompt` and `iris escalate`. database is used for the
// audit row write.
func makeNotifyParent(
	ctx context.Context,
	state *daemonState,
	deliverFn deliverPromptFn,
	database *db.DB,
) func(child, parent string, terminal iris.SessionState, deliveryID string) {
	return func(child, parent string, terminal iris.SessionState, deliveryID string) {
		if parent == "" {
			return // defence-in-depth: setState already guards this
		}

		// Parent-resolution: look up the supervisor by name. We DO NOT
		// fall back to a DB scan or anything fancy — if the parent has
		// terminated and been removed from the map, there is no live pi
		// child to deliver into. Drop the notification silently (the
		// audit row still goes to agent_events so a human can see that
		// the parent had already gone when the child finished).
		state.mu.Lock()
		_, parentLive := state.supervisors[parent]
		state.mu.Unlock()

		if deliveryID == "" {
			deliveryID = uuid.New().String()
		}

		body := formatTerminalBody(child, terminal)

		writeParentNotifiedEvent(database, child, parent, body, deliveryID, string(terminal), parentLive)

		if !parentLive {
			log.Printf("[iris] notifyParent: parent %q is no longer active, dropping notification for child=%s state=%s delivery_id=%s",
				parent, child, terminal, deliveryID)
			return
		}

		// Deliver as "followUp" so the parent receives the notification
		// after its current turn completes. Matches prism's choice for
		// post-turn signals (sidecar/notify.go).
		deliverCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := deliverFn(deliverCtx, parent, body, "followUp", nil); err != nil {
			log.Printf("[iris] notifyParent: deliver to parent=%q failed: %v (child=%s state=%s delivery_id=%s)",
				parent, err, child, terminal, deliveryID)
			return
		}
		log.Printf("[iris] notifyParent: delivered to parent=%q (child=%s state=%s delivery_id=%s)",
			parent, child, terminal, deliveryID)
	}
}

// formatTerminalBody returns the notification body text for a terminal
// transition. The wording matches prism's sidecar/notify.go verbatim so
// pi extensions that pattern-match on the phrase (e.g. for coordinator UX
// hints) work identically for iris workers.
func formatTerminalBody(child string, terminal iris.SessionState) string {
	switch terminal {
	case iris.StateError:
		return fmt.Sprintf("Agent %s has errored its current task", child)
	default:
		// StateFinished (and any defensive future-terminal we treat as
		// "finished" for body purposes).
		return fmt.Sprintf("Agent %s has finished its current task", child)
	}
}

// writeParentNotifiedEvent records the parent-notification attempt in
// agent_events for audit. The event row is written under the CHILD session's
// name so that examining the child's event stream shows the lifecycle
// terminator including the downstream notification. delivered is true when
// the parent's supervisor was found in the live map at the moment of the
// attempt; false when the parent had already been cleaned up.
//
// Failures to write the audit row are logged but never propagated — the
// notification's correctness does not depend on the audit row landing.
func writeParentNotifiedEvent(database *db.DB, child, parent, body, deliveryID, terminal string, delivered bool) {
	if database == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"child":       child,
		"parent":      parent,
		"state":       terminal,
		"body":        body,
		"delivery_id": deliveryID,
		"delivered":   delivered,
	})
	if err != nil {
		log.Printf("[iris] notifyParent: marshal session.parent_notified payload: %v", err)
		return
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: child,
		Type:        "session.parent_notified",
		Payload:     string(payload),
		CreatedAt:   time.Now(),
	}
	if _, err := database.WriteEventReturningRowID(ev); err != nil {
		log.Printf("[iris] notifyParent: write session.parent_notified event: %v", err)
	}
}
