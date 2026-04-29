package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// reviewAgentParentSession derives the parent worker session name from a
// review-agent session name. Review-agent sessions follow the naming convention:
//
//	<parent>~review-<N>-<role>   e.g. nixos-config@feature~review-2-review-goal
//
// The parent session name is the prefix before "~review".
// Returns ("", false) when the session name does not contain "~review".
func reviewAgentParentSession(sessionName string) (string, bool) {
	idx := strings.Index(sessionName, "~review")
	if idx < 0 {
		return "", false
	}
	return sessionName[:idx], true
}

// notifyParentWorkerOnStartupFailure sends a notification to the parent worker
// when a review-agent container fails to start. It is called asynchronously via
// go from writeStartupError, so s.mu must NOT be held.
//
// If the parent worker session cannot be found or has ended, the failure is
// logged and the sidecar exits cleanly (the notification failure is not fatal).
//
// Normal finish notifications for review agents (on the success path) remain
// suppressed via the isReviewAgentSession guard in notifyCoordinator. This
// function is an exception only for the startup-failure path.
func (s *Sidecar) notifyParentWorkerOnStartupFailure(startupErr error) {
	// Only apply to review-agent sessions.
	parentSession, isReview := reviewAgentParentSession(s.cfg.SessionName)
	if !isReview {
		return
	}

	// Look up the parent worker in the DB.
	parentStatus, err := s.cfg.DB.CurrentStatus(parentSession)
	if err != nil {
		log.Printf("sidecar: notifyParentWorker: DB lookup for parent %q: %v — skipping notification", parentSession, err)
		return
	}
	if parentStatus == nil {
		log.Printf("sidecar: notifyParentWorker: parent session %q not found in DB — skipping notification", parentSession)
		return
	}
	if parentStatus.EndedAt != nil {
		log.Printf("sidecar: notifyParentWorker: parent session %q has ended — skipping notification", parentSession)
		return
	}
	if parentStatus.HarnessPort == nil {
		log.Printf("sidecar: notifyParentWorker: parent session %q has no harness port — cannot deliver notification", parentSession)
		return
	}
	port := *parentStatus.HarnessPort

	notifyText := fmt.Sprintf("review agent %s failed to start: %v", s.cfg.SessionName, startupErr)

	storedSID := ""
	if parentStatus.HarnessSessionID != nil {
		storedSID = *parentStatus.HarnessSessionID
	}

	const maxAttempts = 3
	backoff := []time.Duration{500 * time.Millisecond, 1 * time.Second}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(backoff[attempt-2])
		}

		targetSID, validationErr := validateOrRefreshCoordinatorSID(
			port, storedSID, parentSession, s.cfg.DB, s.cfg.HTTPClient,
		)
		if validationErr != nil {
			lastErr = fmt.Errorf("attempt %d: SID validation failed: %w", attempt, validationErr)
			log.Printf("sidecar: notifyParentWorker: %v (parent=%s)", lastErr, parentSession)
			if errors.Is(validationErr, errEmptySessionList) {
				break
			}
			continue
		}
		storedSID = targetSID

		log.Printf("sidecar: notifyParentWorker: attempt %d/%d POST to parent=%s sid=%s",
			attempt, maxAttempts, parentSession, targetSID)

		httpErr := deliverNotificationViaHTTP(port, targetSID, notifyText, parentStatus, s.cfg.HTTPClient)
		if httpErr != nil {
			lastErr = fmt.Errorf("attempt %d: HTTP delivery failed: %w", attempt, httpErr)
			log.Printf("sidecar: notifyParentWorker: %v (parent=%s sid=%s)", lastErr, parentSession, targetSID)
			continue
		}

		log.Printf("sidecar: notifyParentWorker: delivered to parent=%s sid=%s", parentSession, targetSID)
		return
	}

	// All retries exhausted — log and exit cleanly (notification failure is not fatal).
	log.Printf("sidecar: notifyParentWorker: FAILED after %d attempts — parent=%s reason=%v",
		maxAttempts, parentSession, lastErr)
}

// notifyCoordinator sends a "finished" notification to the coordinator session
// for this repo. It is called asynchronously (via go) after writing
// StateFinished, so s.mu must NOT be held when this method runs.
//
// The coordinator is discovered by looking up "<repo>@main" in the DB. If the
// coordinator has an opencode_port, the notification is delivered via HTTP POST
// to /session/<sid>/prompt_async after pre-validating that the stored SID is
// present in the live session list. On confirmed delivery, an audit row is
// written via WriteBusMessageDelivered. On exhausted retries, a row is written
// via WriteBusMessageFailed and a structured error is logged.
//
// If no coordinator exists, it has ended, or this session IS the coordinator,
// the call is a silent no-op.
//
// Review-agent sessions (session names containing "~review") never notify:
// their finish events are internal progress signals consumed by the parent
// worker's pollAgents DB loop. Propagating them to the coordinator would be
// noise — 5 notifications per review round, none of which the coordinator
// needs to act on.
//
// Retry policy: up to 3 POST attempts with exponential backoff (500ms, 1s).
// SID validation (GET /session) is performed before each attempt. If GET /session
// returns an empty list, delivery fails immediately (no retry). If GET /session
// fails with a network or non-200 error, the retry policy applies.
func (s *Sidecar) notifyCoordinator() {
	// Self-notification guard: if this session IS the coordinator, skip.
	// DB-backed: check root_agent_name == "coordinator" for self.
	// Fallback to name heuristic for pre-migration rows.
	if isCoordinatorSession(s.cfg.SessionName, s.cfg.DB) {
		return
	}

	// Review-agent guard: review-agent sessions are internal to the worker's
	// prism review invocation. Their finish events are discovered by the
	// worker's pollAgents DB poll and must not be forwarded to the coordinator
	// as noise notifications.
	if isReviewAgentSession(s.cfg.SessionName, s.cfg.DB) {
		log.Printf("sidecar: notifyCoordinator: suppressed for review-agent session %s", s.cfg.SessionName)
		return
	}

	// DB-backed coordinator lookup: find the active coordinator for this repo.
	coordStatus, err := s.cfg.DB.CoordinatorForRepo(s.cfg.Repo)
	if err != nil {
		log.Printf("sidecar: notifyCoordinator: DB lookup coordinator for repo %q: %v — falling back to name-based lookup", s.cfg.Repo, err)
	}
	if coordStatus == nil {
		// No coordinator with root_agent_name='coordinator' found — fall back to
		// the name-based convention for pre-migration rows.
		fallbackName := s.cfg.Repo + "@main"
		var fallbackStatus *db.Status
		fallbackStatus, err = s.cfg.DB.CurrentStatus(fallbackName)
		if err != nil {
			log.Printf("sidecar: notifyCoordinator: fallback look up coordinator: %v", err)
			return
		}
		if fallbackStatus != nil {
			// Name-based fallback succeeded: a pre-migration coordinator row was
			// found via the @main name convention. Log deprecation only here,
			// not when no coordinator is running at all (which is a normal state).
			log.Printf("[deprecation] sidecar: notifyCoordinator: no DB-backed coordinator found for %q — falling back to name convention %q (pre-migration row)", s.cfg.Repo, fallbackName)
		}
		coordStatus = fallbackStatus
	}
	if coordStatus == nil {
		// No coordinator session at all — silent skip.
		return
	}
	if coordStatus.EndedAt != nil {
		// Coordinator has ended — silent skip.
		return
	}

	coordinatorName := coordStatus.SessionName

	notifyText := fmt.Sprintf("Agent %s has finished its current task", s.cfg.SessionName)

	// Capture the coordinator's current instance_id so the message is scoped
	// to the correct incarnation of the coordinator. If the coordinator has no
	// instance_id (e.g. legacy row), ToInstanceID remains nil and the message
	// is delivered to any coordinator instance (backward-compatible).
	var coordInstanceID *string
	if coordStatus.InstanceID != nil {
		coordInstanceID = coordStatus.InstanceID
	}

	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  s.cfg.SessionName,
		ToSession:    coordinatorName,
		ToInstanceID: coordInstanceID,
		Repo:         s.cfg.Repo,
		Text:         notifyText,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}

	// Require port for HTTP delivery.
	if coordStatus.HarnessPort == nil {
		log.Printf("sidecar: notifyCoordinator: coordinator has no harness port — cannot deliver notification")
		return
	}
	port := *coordStatus.HarnessPort

	// storedSID is the SID currently recorded in the DB. May be stale if the
	// coordinator created a new harness session after the last DB write.
	storedSID := ""
	if coordStatus.HarnessSessionID != nil {
		storedSID = *coordStatus.HarnessSessionID
	}

	const maxAttempts = 3
	// backoff[i] is the sleep duration before attempt i+2 (i.e. before the
	// 2nd and 3rd attempts). With 3 attempts, only 2 sleeps are needed.
	backoff := []time.Duration{500 * time.Millisecond, 1 * time.Second}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(backoff[attempt-2])
		}

		// Pre-delivery SID validation: call GET /session to confirm the stored
		// SID is present in the coordinator's live session list. If not, pick
		// the most recently active session and update the DB so future
		// deliveries use the fresh SID.
		targetSID, validationErr := validateOrRefreshCoordinatorSID(
			port, storedSID, coordinatorName, s.cfg.DB, s.cfg.HTTPClient,
		)
		if validationErr != nil {
			lastErr = fmt.Errorf("attempt %d: SID validation failed: %w", attempt, validationErr)
			log.Printf("sidecar: notifyCoordinator: %v (coordinator=%s)", lastErr, coordinatorName)
			// An empty session list is not a transient condition — retrying
			// will not help. Break immediately to avoid unnecessary backoff.
			if errors.Is(validationErr, errEmptySessionList) {
				break
			}
			continue
		}
		// Keep storedSID current for next iteration if it was refreshed.
		storedSID = targetSID

		log.Printf("sidecar: notifyCoordinator: attempt %d/%d POST to coordinator=%s sid=%s",
			attempt, maxAttempts, coordinatorName, targetSID)

		httpErr := deliverNotificationViaHTTP(port, targetSID, notifyText, coordStatus, s.cfg.HTTPClient)
		if httpErr != nil {
			lastErr = fmt.Errorf("attempt %d: HTTP delivery failed: %w", attempt, httpErr)
			log.Printf("sidecar: notifyCoordinator: %v (coordinator=%s sid=%s)", lastErr, coordinatorName, targetSID)
			continue
		}

		// Confirmed delivery: SID validated + POST returned 200.
		if err := s.cfg.DB.WriteBusMessageDelivered(msg); err != nil {
			log.Printf("sidecar: notifyCoordinator: write delivered audit: %v", err)
		}
		log.Printf("sidecar: notifyCoordinator: delivered to coordinator=%s sid=%s", coordinatorName, targetSID)
		return
	}

	// All retries exhausted — write failed_at and log structured error.
	if err := s.cfg.DB.WriteBusMessageFailed(msg); err != nil {
		log.Printf("sidecar: notifyCoordinator: write failed audit: %v", err)
	}
	log.Printf("sidecar: notifyCoordinator: FAILED after %d attempts — coordinator=%s sid=%s reason=%v",
		maxAttempts, coordinatorName, storedSID, lastErr)
}

// errEmptySessionList is a sentinel error returned by validateOrRefreshCoordinatorSID
// when GET /session returns an empty array. The caller treats this as a
// non-retriable condition and breaks out of the retry loop immediately.
var errEmptySessionList = errors.New("GET /session: empty session list — coordinator has no active opencode sessions")

// opencodeSessionEntry is a single entry from the opencode GET /session response.
type opencodeSessionEntry struct {
	ID   string `json:"id"`
	Time *struct {
		Updated *float64 `json:"updated"`
	} `json:"time"`
}

// validateOrRefreshCoordinatorSID calls GET /session on the coordinator's
// opencode port to retrieve the live session list, checks whether storedSID is
// present, and returns the SID to use for delivery.
//
//   - If storedSID is present in the list, it is returned as-is.
//   - If storedSID is absent, the most recently updated session ID is returned
//     and agent_status is updated with the fresh SID.
//   - If GET /session fails, an error is returned.
//   - If GET /session returns an empty list, errEmptySessionList is returned
//     (sentinel — caller should not retry).
func validateOrRefreshCoordinatorSID(port int, storedSID string, coordinatorName string, database *db.DB, httpClient *http.Client) (string, error) {
	url := fmt.Sprintf("http://localhost:%d/session", port)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create GET /session request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET /session: http status %d", resp.StatusCode)
	}

	var sessions []opencodeSessionEntry
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return "", fmt.Errorf("decode GET /session response: %w", err)
	}

	if len(sessions) == 0 {
		// Non-retriable: an empty session list is a definitive condition, not a
		// transient failure. The caller will break immediately on this error.
		return "", errEmptySessionList
	}

	// Check if stored SID is present.
	for _, s := range sessions {
		if s.ID == storedSID {
			// Stored SID confirmed present — use it.
			return storedSID, nil
		}
	}

	// Stored SID is absent (stale). Pick the most recently updated session.
	bestSID := sessions[0].ID
	var bestUpdated float64
	for _, s := range sessions {
		if s.Time != nil && s.Time.Updated != nil && *s.Time.Updated > bestUpdated {
			bestUpdated = *s.Time.Updated
			bestSID = s.ID
		}
	}

	log.Printf("sidecar: notifyCoordinator: stored SID %q not found in coordinator session list — using most recent SID %q", storedSID, bestSID)

	// Persist the refreshed SID so future deliveries use it.
	if err := database.UpdateHarnessSessionID(coordinatorName, bestSID); err != nil {
		log.Printf("sidecar: notifyCoordinator: UpdateHarnessSessionID failed: %v", err)
	}

	return bestSID, nil
}

// deliverNotificationViaHTTP sends a notification prompt to the opencode HTTP API.
func deliverNotificationViaHTTP(port int, opencodeSID string, text string, status *db.Status, httpClient *http.Client) error {
	body := buildNotifyPromptBody(text, status)
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal prompt body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", port, opencodeSID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 200 bytes of the response body to include in the error so
		// that the root cause of non-2xx responses is self-diagnosing in logs.
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		bodySnippet := strings.TrimSpace(string(bodyBytes))
		if bodySnippet != "" {
			return fmt.Errorf("http status %d: %s", resp.StatusCode, bodySnippet)
		}
		return fmt.Errorf("http status %d", resp.StatusCode)
	}

	return nil
}

// buildNotifyPromptBody constructs the request body for the coordinator
// notification prompt_async call. When root_model_id is known, it is included
// so the session continues using its root model. Falls back to model_id for
// sessions created before the root fields migration.
//
// The "agent" field is included when root_agent_name is non-nil and non-empty.
// This re-asserts the correct agent on notification delivery, preventing
// opencode from defaulting to its last-active (wrong) agent in host mode.
//
// Background: issue #848 showed that setting "agent" let an incoming
// notification switch a subagent's context to the notifier's agent. That
// concern does not apply here: the status passed in is the *receiving*
// session's own status, not the sender's. Re-asserting root_agent_name on
// delivery is safe and correct — it keeps the coordinator pinned to the right
// agent persona regardless of what opencode last processed.
func buildNotifyPromptBody(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
	}

	// Re-assert the receiving session's root agent so opencode does not default
	// to its internally-tracked last-active agent (which may differ in host
	// mode). Only set when root_agent_name is known and non-empty.
	if status.RootAgentName != nil && *status.RootAgentName != "" {
		body["agent"] = *status.RootAgentName
	}

	modelID := status.RootModelID
	if modelID == nil {
		modelID = status.ModelID
	}

	if modelID != nil {
		// Split model_id on the first "/" to get providerID and modelID.
		slashIdx := strings.Index(*modelID, "/")
		providerID := *modelID
		modelIDStr := ""
		if slashIdx >= 0 {
			providerID = (*modelID)[:slashIdx]
			modelIDStr = (*modelID)[slashIdx+1:]
		}
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelIDStr,
		}
	}

	return body
}
