package sidecar

// message_tracking_bound_test.go — acceptance-criteria tests for the
// bounded-LRU treatment of the four per-message tracking maps on Sidecar
// (writtenMessages / textByMessage / msgCreatedAtMs / ttftByMessage).

import (
	"strconv"
	"testing"
)

// TestMessageTrackingMaps_BoundedAfterFlood checks that after many more inserts
// than the cap, none of the four maps exceed the cap. This drives the real
// HandleEvent pipeline (not the boundedMap helper directly), so it also
// guards against any future call-site that bypasses the bounded type.
func TestMessageTrackingMaps_BoundedAfterFlood(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Generate (cap * 3) abandoned assistant messages. Each one feeds the
	// "first message.updated with time.created" path (populating
	// msgCreatedAtMs) followed by a text part (populating textByMessage and
	// ttftByMessage). We deliberately omit the final time.completed event
	// so writtenMessages, textByMessage, msgCreatedAtMs, and ttftByMessage
	// all retain entries — this is the exact leak the issue describes.
	//
	// To populate writtenMessages we then emit a completed user message
	// (which writes to writtenMessages and not the others) for each ID.
	const flood = messageTrackingCap * 3

	for i := range flood {
		id := "msg-" + strconv.Itoa(i)
		created := float64(1000 + i)
		start := created + 10

		// 1) message.updated assistant with time.created — populates msgCreatedAtMs.
		sc.HandleEvent(makeSSE("message.updated", map[string]any{
			"info": map[string]any{
				"id":   id,
				"role": "assistant",
				"time": map[string]*float64{
					"created": &created,
				},
			},
		}))

		// 2) message.part.updated text — populates textByMessage and ttftByMessage.
		sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
			"part": map[string]any{
				"type":      "text",
				"messageID": id,
				"text":      "partial-" + id,
				"time": map[string]*float64{
					"start": &start,
				},
			},
		}))

		// 3) Completed user message with a different ID — populates writtenMessages.
		userID := "user-" + strconv.Itoa(i)
		sendEvents(sc, makeUserMessage(userID, "worker", "hello"))
	}

	if got := sc.writtenMessages.len(); got > messageTrackingCap {
		t.Errorf("writtenMessages.len = %d, want <= %d", got, messageTrackingCap)
	}
	if got := sc.textByMessage.len(); got > messageTrackingCap {
		t.Errorf("textByMessage.len = %d, want <= %d", got, messageTrackingCap)
	}
	if got := sc.msgCreatedAtMs.len(); got > messageTrackingCap {
		t.Errorf("msgCreatedAtMs.len = %d, want <= %d", got, messageTrackingCap)
	}
	if got := sc.ttftByMessage.len(); got > messageTrackingCap {
		t.Errorf("ttftByMessage.len = %d, want <= %d", got, messageTrackingCap)
	}

	// All four maps should be at or very near the cap after flood >> cap.
	// (writtenMessages collects user-msg IDs; the other three collect the
	// abandoned-assistant IDs.) We expect each map to be at least 95% of
	// the cap — the exact value is sensitive to interleaved del() calls
	// from completed user messages, which can free a slot per iteration.
	const nearSaturated = messageTrackingCap * 95 / 100
	if got := sc.writtenMessages.len(); got < nearSaturated {
		t.Errorf("writtenMessages.len = %d, want >= %d (near-saturated)", got, nearSaturated)
	}
	if got := sc.textByMessage.len(); got < nearSaturated {
		t.Errorf("textByMessage.len = %d, want >= %d (near-saturated)", got, nearSaturated)
	}
	if got := sc.msgCreatedAtMs.len(); got < nearSaturated {
		t.Errorf("msgCreatedAtMs.len = %d, want >= %d (near-saturated)", got, nearSaturated)
	}
	if got := sc.ttftByMessage.len(); got < nearSaturated {
		t.Errorf("ttftByMessage.len = %d, want >= %d (near-saturated)", got, nearSaturated)
	}
}

// TestMessageTrackingMaps_ShortConversationNoEviction checks that a normal
// 10-turn conversation must not trigger eviction. writtenMessages
// accumulates every completed user + assistant message; the streaming maps
// (textByMessage / msgCreatedAtMs / ttftByMessage) are drained on
// completion so they stay empty between turns.
func TestMessageTrackingMaps_ShortConversationNoEviction(t *testing.T) {
	sc, _ := newTestSidecar(t)
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	const turns = 10

	for i := range turns {
		userID := "user-" + strconv.Itoa(i)
		asstID := "asst-" + strconv.Itoa(i)
		sendEvents(sc, makeUserMessage(userID, "worker", "hello "+userID))
		sendEvents(sc, makeAssistantMessage(asstID, "worker", "reply "+asstID))
	}

	// writtenMessages: one entry per user + assistant message = 2 * turns.
	if got := sc.writtenMessages.len(); got != 2*turns {
		t.Errorf("writtenMessages.len after %d turns = %d, want %d", turns, got, 2*turns)
	}
	// All other maps are cleared on completion, so they should be empty.
	if got := sc.textByMessage.len(); got != 0 {
		t.Errorf("textByMessage.len after %d completed turns = %d, want 0", turns, got)
	}
	if got := sc.msgCreatedAtMs.len(); got != 0 {
		t.Errorf("msgCreatedAtMs.len after %d completed turns = %d, want 0", turns, got)
	}
	if got := sc.ttftByMessage.len(); got != 0 {
		t.Errorf("ttftByMessage.len after %d completed turns = %d, want 0", turns, got)
	}

	// Sanity: every message ID emitted should still be present in
	// writtenMessages (no premature eviction).
	for i := range turns {
		userID := "user-" + strconv.Itoa(i)
		asstID := "asst-" + strconv.Itoa(i)
		if !sc.writtenMessages.has(userID) {
			t.Errorf("writtenMessages missing user message %q after %d turns (premature eviction)", userID, turns)
		}
		if !sc.writtenMessages.has(asstID) {
			t.Errorf("writtenMessages missing assistant message %q after %d turns (premature eviction)", asstID, turns)
		}
	}
}
