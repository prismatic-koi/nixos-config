package review

// undelivered.go — the report of a review-complete prompt that the monitor
// could not deliver to the worker (#3345).

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// undeliveredReview is a review-complete prompt that the monitor could not
// deliver after all of its retries.
type undeliveredReview struct {
	workerSession string
	deliveryID    string
	text          string
	prNumber      string
	round         int
	fallbackPath  string
	cause         error
}

// reportUndeliveredReview makes an undelivered review-complete prompt
// visible, so that it is not lost silently:
//
//   - It keeps the prompt in the durable replay buffer of the worker. The
//     next sidecar of the worker loads the buffer when it starts, and sends
//     the prompt to the first agent that connects.
//   - It sets the worker to state error. The pre-delivery write set active,
//     but no agent can receive a prompt.
//   - It tells the coordinator of the repo of the worker.
//
// It does nothing for a worker that has ended. The fallback file is then the
// only record, as before.
func reportUndeliveredReview(d *db.DB, u undeliveredReview) {
	status, err := d.CurrentStatus(u.workerSession)
	if err != nil || status == nil || status.EndedAt != nil {
		return
	}

	if _, err := d.InsertPendingReplayDelivery(db.PendingReplayRow{
		SessionName: u.workerSession,
		DeliveryID:  u.deliveryID,
		Text:        u.text,
		DeliverAs:   "nextTurn",
		Source:      "review-complete",
		QueuedAt:    time.Now(),
	}); err != nil {
		proglog.Warnf("[prism monitor-review] warning: could not keep the undelivered prompt for %s: %v\n", u.workerSession, err)
	}

	if !agent.IsTerminal(agent.AgentState(status.State)) {
		if err := d.UpsertStatus(u.workerSession, status.Repo, status.Worktree, string(agent.StateError), nil, nil); err != nil {
			proglog.Warnf("[prism monitor-review] warning: could not set %s to error: %v\n", u.workerSession, err)
		} else {
			payload, _ := json.Marshal(map[string]string{"state": string(agent.StateError), "note": "review-complete not delivered"})
			if err := d.WriteEvent(db.Event{
				ID:          uuid.New().String(),
				SessionName: u.workerSession,
				Repo:        status.Repo,
				Worktree:    status.Worktree,
				InstanceID:  status.InstanceID,
				Type:        "state_change",
				Payload:     string(payload),
				CreatedAt:   time.Now(),
			}); err != nil {
				proglog.Warnf("[prism monitor-review] warning: could not record the error state of %s: %v\n", u.workerSession, err)
			}
			proglog.Infof("[prism monitor-review] worker state %s→error (review-complete not delivered)\n", status.State)
		}
	}

	notifyCoordinatorUndelivered(d, status, u)
}

// notifyCoordinatorUndelivered sends the coordinator of the repo of the
// worker a notice about the undelivered prompt. A missing coordinator is not
// an error: the monitor log keeps the record.
func notifyCoordinatorUndelivered(d *db.DB, worker *db.Status, u undeliveredReview) {
	coord, err := d.CoordinatorForRepo(worker.Repo)
	if err == nil && coord == nil {
		coord, err = d.CurrentStatus(worker.Repo + "@main")
	}
	if err != nil || coord == nil || coord.EndedAt != nil || coord.SessionName == u.workerSession {
		proglog.Warnf("[prism monitor-review] no coordinator to tell about the undelivered prompt of %s (err=%v)\n", u.workerSession, err)
		return
	}
	result := "prism could not write the result to a file."
	if u.fallbackPath != "" {
		result = "The result is in " + u.fallbackPath + "."
	}
	text := fmt.Sprintf("Agent %[1]s did not receive the review-complete prompt of review round %[2]d for PR #%[3]s. "+
		"The delivery failed: %[4]v. "+
		"The session is in state error. %[5]s "+
		"prism keeps the prompt, and a new sidecar of the session sends it when an agent connects. "+
		"To restart the agent and its sidecar, run `prism restore --session %[1]s` in a host shell.",
		u.workerSession, u.round, u.prNumber, u.cause, result)
	if err := promptdelivery.DeliverToSession(coord.SessionName, coord, text, buildPromptBodyForMonitor, "", "followUp"); err != nil {
		proglog.Warnf("[prism monitor-review] warning: could not tell coordinator %s about the undelivered prompt: %v\n", coord.SessionName, err)
		return
	}
	proglog.Infof("[prism monitor-review] told coordinator %s about the undelivered prompt of %s\n", coord.SessionName, u.workerSession)
}
