package tui

// coordinator.go — coordinator-only affordances (issue #1772, child 7 of
// the bubbletea-native iris TUI design tracker #1765).
//
// When the focused session is a coordinator, the TUI surfaces two
// coordinator-specific signal classes more prominently than they would
// appear in a generic worker session:
//
//   - Escalations (agent_events.type == "session.escalated") emitted when
//     a worker calls `iris escalate` / `prism escalate`. The daemon-side
//     event is written by ClientSocket.writeSessionEscalatedEvent
//     (internal/iris/client_socket.go).
//
//   - Merge-queue notifications delivered to the coordinator as prompt
//     text by internal/mergequeue/watcher.go (succeedAndNotify /
//     failAndNotify). These arrive at the coordinator harness as a
//     regular prompt and are persisted as a `msg_user` agent_events
//     row by the harness adapter. There is no dedicated event type for
//     them in the iris DB today, so the TUI detects them by text-prefix
//     match — every notification produced by the watcher starts with
//     "PR #N " followed by an outcome verb ("merged", "merge failed",
//     "has merge conflicts", "CI failed", "was closed", "is blocked").
//
// "Coordinator session" detection follows prism's session-naming
// convention: a session is named `<repo>@<branch>` where `<branch>` is
// either the repo's main branch (typically `main` or `master`) or a
// feature branch. Workers carry the feature-branch name, review-children
// and investigators carry a `~review-N-...` / `~investigate-...` infix
// after the branch. The cleanest heuristic is therefore: a session is a
// coordinator iff its name contains no `~` AND its `@`-suffix matches a
// known main-branch name. The list is conservative — only `main` and
// `master` qualify; an extension PR can add more if conventions diverge.
//
// This file is intentionally light: detection, the merge-queue text
// matcher, and the typed accumulator struct. The renderer and overlay
// wiring live in renderer.go and overlay.go respectively, gated on the
// helpers defined here.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/narrative"
	"github.com/prismatic-koi/prism/internal/payload"
)

// coordinatorMainBranches lists the branch names that qualify a session
// for "coordinator" status under the `<repo>@<branch>` heuristic. Kept
// as a package-level var (not const) so future extension via overlay or
// test injection is straightforward; today the set is fixed to the two
// canonical Git defaults.
//
// We deliberately do NOT include `trunk`, `develop`, or `dev` — the
// prism convention encoded in internal/iris/spawn_role.go::ResolveAgent
// is exactly `basename == "main"`, and adding more branch names here
// without also updating ResolveAgent would split the truth between two
// places. If/when the convention expands, both sides update together.
var coordinatorMainBranches = []string{"main", "master"}

// IsCoordinatorSessionName reports whether a session name belongs to a
// coordinator (as opposed to a worker, review-child, or investigator).
// The heuristic is:
//
//   - Name must contain exactly one `@` separator (`<repo>@<branch>`).
//     Names without `@` are not iris session names produced by the
//     spawn helpers; we conservatively return false.
//
//   - Name must NOT contain `~` anywhere. The `~` infix is used by the
//     spawn helpers to mark review children (`~review-N-<agent>`) and
//     investigators (`~investigate-...`); those are never coordinators
//     even if the post-tilde portion happens to look like a main branch.
//
//   - The branch portion (after `@`) must match one of
//     coordinatorMainBranches.
//
// The function operates on the name string alone — it does NOT consult
// the session's Role field, deliberately. Role is set at spawn-time
// from the worktree heuristic (ResolveAgent), but a session that was
// spawned with an overridden role would still be visually a coordinator
// by name. The TUI's coordinator affordances key off the visible
// identifier, not the role label, so an operator who sees
// `nixos-config@main` in the sidebar always gets the coordinator
// affordances regardless of how the daemon was launched.
func IsCoordinatorSessionName(name string) bool {
	if name == "" {
		return false
	}
	if strings.Contains(name, "~") {
		return false
	}
	at := strings.Index(name, "@")
	if at <= 0 {
		// at < 0: no separator; at == 0: empty repo segment. Both
		// reject — a real session always has a non-empty repo prefix.
		return false
	}
	// A second `@` would be ambiguous (repo names cannot contain `@`
	// under the spawn helpers' conventions); reject defensively.
	if strings.Count(name, "@") != 1 {
		return false
	}
	branch := name[at+1:]
	if branch == "" {
		return false
	}
	for _, m := range coordinatorMainBranches {
		if branch == m {
			return true
		}
	}
	return false
}

// coordinatorEventKind discriminates the two signal classes accumulated
// in Model.coordinatorEvents. The overlay renders both with distinct
// styling so the operator can tell at a glance whether a row is a
// worker-escalation or a merge-queue outcome.
type coordinatorEventKind int

const (
	// coordEventEscalation marks an `iris escalate` / `prism escalate`
	// emission, sourced from agent_events.type == "session.escalated".
	coordEventEscalation coordinatorEventKind = iota
	// coordEventMergeQueue marks a merge-queue notification (PR merged,
	// PR has merge conflicts, etc.), sourced from a msg_user event whose
	// text matches isMergeQueueNotificationText.
	coordEventMergeQueue
)

// coordinatorEvent is one entry in Model.coordinatorEvents. The buffer
// is bounded (see coordinatorEventBufferCap) and ordered by arrival
// time — the overlay renders newest-first by walking the slice
// backwards.
//
// Fields are deliberately minimal: the overlay row contains kind,
// session name, a one-line summary, and the wall-clock arrival time.
// Richer data (full payload, structured PR number) is not stored
// because the overlay does not action on it — child 7 is a display-only
// scope, escalation acks / re-enqueues are out of scope.
type coordinatorEvent struct {
	kind        coordinatorEventKind
	sessionName string
	// summary is the one-line text shown in the overlay row. For
	// escalations, this is the escalated worker name plus the
	// escalation prompt's first line (or "<no prompt>" when empty).
	// For merge-queue notifications, this is the verbatim notification
	// text written by the watcher ("PR #N merged ...").
	summary string
	// at is the wall-clock arrival time. We use time.Now() at frame
	// arrival rather than the agent_events.created_at column because
	// the daemon frame does not currently surface that value on the
	// wire — same convention as sidebar.go's lastEventAt.
	at time.Time
}

// coordinatorEventBufferCap is the maximum number of coordinator events
// the model retains. Bounded so a long-lived TUI session (days) with a
// busy merge queue does not grow the buffer unboundedly. 200 is
// generous — the design-doc guidance was "last N (e.g. 50)" but a
// higher cap is cheap and lets the overlay show enough history to
// triage a multi-day weekend backlog without truncation pain.
const coordinatorEventBufferCap = 200

// mergeQueueNotificationPrefix is the common prefix every notification
// emitted by internal/mergequeue/watcher.go starts with. We use it as
// a cheap pre-filter before the more specific keyword match below; if
// a future change to the watcher format drops this prefix, the more
// specific keyword arms still detect known phrasings.
const mergeQueueNotificationPrefix = "PR #"

// mergeQueueNotificationKeywords is the closed set of outcome phrases
// the watcher emits today (see mergequeue/watcher.go succeedAndNotify
// and failAndNotify). We match against this set rather than a regex
// over arbitrary text so a user typing "PR #42 looks good" into a
// prompt does NOT get visually flagged as a merge-queue notification.
//
// The match is substring (not prefix) because the watcher's output
// embeds the PR number between "PR #" and the outcome phrase:
//
//	"PR #42 merged. Archive: …"
//	"PR #42 has merge conflicts — worker rebase needed"
//	"PR #42 CI failed — needs worker fix"
//	"PR #42 was closed without merging — removed from queue"
//	"PR #42 is blocked — human reviewer approval required before merge"
//	"PR #42 merge failed: <errMsg>"
//
// New keywords are added here when watcher.go grows a new outcome.
var mergeQueueNotificationKeywords = []string{
	" merged",                     // "PR #N merged ..." (succeedAndNotify)
	" merge failed",               // "PR #N merge failed: ..." (failAndNotify default)
	" has merge conflicts",        // failAndNotify "merge conflicts" branch
	" CI failed",                  // failAndNotify "CI failed" branch
	" was closed without merging", // failAndNotify "PR was closed" branch
	" is blocked",                 // failAndNotify "human reviewer approval required" branch
}

// focusedIsCoordinator reports whether the model's currently subscribed
// (focused) session is a coordinator. This is the gate used by:
//
//   - styleEventLine, to decide whether session.escalated /
//     merge_queue_notification rows render with their prominent
//     coordinator-only styling or fall back to the neutral treatment.
//
//   - the C-o key handler, to decide whether to open the
//     coordinator-events overlay or surface the "not applicable"
//     errorMsg.
//
// Returns false when no session is subscribed (pre-snapshot, empty
// list) so the prominent styling is never accidentally applied to
// non-session views.
func (m Model) focusedIsCoordinator() bool {
	if m.subscribedTo == "" {
		return false
	}
	return IsCoordinatorSessionName(m.subscribedTo)
}

// accumulateCoordinatorEvent inspects one decoded session_event frame and
// the rendered NarrativeLines it produced; when the event is an
// escalation or a merge-queue notification it (a) appends a typed
// coordinatorEvent entry to Model.coordinatorEvents and (b) for
// merge-queue notifications, re-labels the rendered line(s) so
// styleEventLine can apply the merge-queue-specific styling.
//
// Mutation order matters: the lines slice is the same slice the caller
// will subsequently append to m.eventLines, so the in-place EventType
// re-write here propagates to the conversation pane. We re-label both
// the header and the "_body" line that renderMsgUser produces — the
// two-line block reads as one merge-queue unit, matching the design
// for extension_error.
//
// The buffer is bounded: when len(coordinatorEvents) ==
// coordinatorEventBufferCap we drop the oldest entry. This keeps the
// model's memory footprint constant for long-lived TUI sessions even
// under sustained escalation / merge-queue traffic.
func (m *Model) accumulateCoordinatorEvent(e *iris.DaemonSessionEventFrame, lines []narrative.NarrativeLine) {
	if e == nil {
		return
	}
	switch e.EventType {
	case evTypeSessionEscalated:
		var p struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal([]byte(e.Payload), &p)
		preview := firstNonEmptyLine(p.Prompt)
		if preview == "" {
			preview = "(no prompt body)"
		}
		// The escalating worker is named by the payload's `source` field,
		// NOT by e.SessionName — the frame can arrive via either the
		// worker's subscription (e.SessionName == worker) or, post-fix
		// in client_socket.go's writeSessionEscalatedEvent, via the
		// target coordinator's subscription (e.SessionName ==
		// coordinator). Sourcing the summary from the payload makes the
		// rendered row consistent regardless of which stream carried
		// the frame.
		worker := p.Source
		if worker == "" {
			// Defensive fallback: older daemon writes (pre-fix replay)
			// without a source field. e.SessionName is the next best
			// candidate — on the worker's stream it names the
			// escalating worker correctly.
			worker = e.SessionName
		}
		var summary string
		switch {
		case p.Target != "":
			summary = worker + " \u2192 " + p.Target + ": " + preview
		default:
			summary = worker + " (no coordinator): " + preview
		}
		m.appendCoordinatorEvent(coordinatorEvent{
			kind:        coordEventEscalation,
			sessionName: worker,
			summary:     summary,
			at:          time.Now(),
		})
	case evTypeMsgUser:
		var p payload.MsgUser
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			return
		}
		text := strings.TrimSpace(p.Text)
		if !isMergeQueueNotificationText(text) {
			return
		}
		// Re-label every rendered line that descends from this
		// msg_user row so the conversation pane styles them with the
		// merge-queue treatment rather than the generic msg_user blue.
		// The re-label fires regardless of whether the focused session
		// is the coordinator — styleEventLine inspects the
		// `coordinator` flag and falls back to plain msg_user blue when
		// false, so a non-coordinator session viewing a stray
		// merge-queue-formatted line still gets the unchanged child-2
		// rendering.
		for i := range lines {
			switch lines[i].EventType {
			case evTypeMsgUser, evTypeMsgUser + "_body":
				lines[i].EventType = evTypeMergeQueueNotification
			}
		}
		m.appendCoordinatorEvent(coordinatorEvent{
			kind:        coordEventMergeQueue,
			sessionName: e.SessionName,
			summary:     firstNonEmptyLine(text),
			at:          time.Now(),
		})
	}
}

// appendCoordinatorEvent inserts a coordinator event at the tail of the
// model's buffer and trims the oldest entry off the front when the cap
// is exceeded. Newest-last ordering matches the overlay's most-recent-
// first traversal (range backwards over the slice).
func (m *Model) appendCoordinatorEvent(ev coordinatorEvent) {
	m.coordinatorEvents = append(m.coordinatorEvents, ev)
	if over := len(m.coordinatorEvents) - coordinatorEventBufferCap; over > 0 {
		// Drop the oldest `over` entries. Copy rather than re-slice so
		// the underlying array can be GC'd once the slice header no
		// longer references its head — important for long-lived TUI
		// sessions with bursty escalation traffic.
		m.coordinatorEvents = append([]coordinatorEvent(nil),
			m.coordinatorEvents[over:]...)
	}
}

// firstNonEmptyLine returns the first trimmed non-empty line of s, or
// "" when s contains only whitespace. Used to compress multi-line
// escalation prompts and merge-queue notifications to a single
// summary row.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// isMergeQueueNotificationText reports whether the given prompt/msg_user
// text was produced by internal/mergequeue/watcher.go's notify path.
//
// The check is intentionally narrow (prefix + keyword set) rather than
// "any text mentioning a PR number" so the visual prominence is
// reserved for actual watcher emissions. False positives would cause
// the TUI to mis-style an operator's free-form prompt as a merge-queue
// notification — visually loud and confusing.
//
// We expose the function (capitalised) primarily for unit tests; the
// renderer code calls it via the same name.
func isMergeQueueNotificationText(text string) bool {
	if !strings.HasPrefix(text, mergeQueueNotificationPrefix) {
		return false
	}
	for _, kw := range mergeQueueNotificationKeywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}
