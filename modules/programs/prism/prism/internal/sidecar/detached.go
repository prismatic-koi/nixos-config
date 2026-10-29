package sidecar

// detached.go — the state of a socket-pipe session while no agent is
// connected after the reconnect timeout, and the coordinator notice for a
// review-complete prompt that the sidecar holds in that time (#3345).

import (
	"fmt"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/review"
)

// reassertDetachedError keeps a detached session in state error. Another
// process can write a state that says that the agent works: the review
// monitor writes active before it delivers the review-complete prompt. The
// /prompt handler buffers that prompt, because no agent is connected, and
// then calls this function.
//
// It changes only a state that is not terminal. It also tells the
// coordinator about a held review-complete prompt.
//
// Must be called WITHOUT s.mu held.
func (s *Sidecar) reassertDetachedError() {
	s.mu.Lock()
	if !s.harnessPipeDetached || s.shuttingDown || s.cfg.DB == nil {
		s.mu.Unlock()
		return
	}
	if st := s.currentDBState(); st != "" && !agent.IsTerminal(st) {
		s.logger().Printf("sidecar: no agent connected; state %s -> error", st)
		s.upsertState(agent.StateError, nil, nil)
		s.writeEvent("state_change", map[string]string{"state": string(agent.StateError), "note": "no agent connected"}, nil)
		s.lastState = agent.StateError
	}
	s.mu.Unlock()
	s.notifyHeldReviewComplete()
}

// notifyHeldReviewComplete tells the coordinator about each review-complete
// prompt in the replay buffer that it was not told about yet, while the
// session is detached. The prompt stays in the buffer, and the next agent
// that connects receives it.
//
// Must be called WITHOUT s.mu held.
func (s *Sidecar) notifyHeldReviewComplete() {
	s.mu.Lock()
	if !s.harnessPipeDetached {
		s.mu.Unlock()
		return
	}
	var held []string
	for _, d := range s.pendingReplayDeliveries {
		key := d.DeliveryID
		if key == "" {
			key = d.PersistKey
		}
		if d.Source != "review-complete" || key == "" || s.heldReviewNotified[key] {
			continue
		}
		if s.heldReviewNotified == nil {
			s.heldReviewNotified = make(map[string]bool)
		}
		s.heldReviewNotified[key] = true
		held = append(held, d.DeliveryID)
	}
	s.mu.Unlock()

	for _, deliveryID := range held {
		text := s.heldReviewNotifyText(deliveryID)
		s.logger().Printf("sidecar: no agent connected; telling the coordinator about held review-complete prompt (delivery_id=%s)", deliveryID)
		s.goNotify(func() { s.sendCoordinatorNotification(text, false) })
	}
}

// heldReviewNotifyText is the coordinator notice for a held review-complete
// prompt. It names the PR and the round when the delivery ID identifies the
// review group.
func (s *Sidecar) heldReviewNotifyText(deliveryID string) string {
	prompt := "A review-complete prompt"
	if groupID, ok := review.GroupIDFromRecoveryDeliveryID(deliveryID); ok && s.cfg.DB != nil {
		if g, err := s.cfg.DB.GetGroup(groupID); err == nil && g != nil && g.PRNumber != "" {
			prompt = fmt.Sprintf("The review-complete prompt of review round %d for PR #%s", g.Round, g.PRNumber)
		}
	}
	return fmt.Sprintf("Agent %[1]s is not connected to its sidecar. %[2]s did not reach the agent. "+
		"The sidecar keeps the prompt and sends it when an agent process of the session connects. "+
		"The session is in state error until then. "+
		"To restart the agent and its sidecar, run `prism restore --session %[1]s` in a host shell.",
		s.cfg.SessionName, prompt)
}
