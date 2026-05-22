package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/feedback"
	"github.com/prismatic-koi/prism/internal/harness"
	investigatepkg "github.com/prismatic-koi/prism/internal/investigate"
	prismsession "github.com/prismatic-koi/prism/internal/session"
)

// statusClientClosedRequest is the de-facto HTTP status code (popularised by
// nginx and HAProxy) returned when the client closes the connection before
// the server has finished processing the request. Issue #1847 documents this
// as the response status when a host-API handler's context is cancelled
// because the caller disconnected (as opposed to the per-endpoint timeout
// firing, which uses 504 Gateway Timeout).
const statusClientClosedRequest = 499

// contextErrStatus reports, for a context that has fired, the HTTP status code
// that the host-API handler should return. It distinguishes a per-endpoint
// timeout (deadline exceeded → 504 Gateway Timeout) from a client disconnect
// or other cancellation (→ 499 Client Closed Request). The boolean is false
// when the context has not fired — in that case the caller falls through to
// the normal subprocess-failure path so it can surface the underlying error.
//
// This helper is used by the host-API handlers that wrap r.Context() with
// context.WithTimeout and exec.CommandContext (issue #1847). When the child
// is killed by context cancellation, os/exec returns an error like
// "signal: killed" which says nothing about *why*; the caller's ctx.Err() is
// the authoritative signal.
func contextErrStatus(ctx context.Context) (int, bool) {
	err := ctx.Err()
	if err == nil {
		return 0, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, true
	}
	return statusClientClosedRequest, true
}

// hostAPIServeLogsTail writes the last n lines of the log file to w.
// When n == 0, the response body is empty.
func hostAPIServeLogsTail(w http.ResponseWriter, logPath string, n int) {
	if n == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, `{"error":"cannot open log"}`, http.StatusInternalServerError)
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"cannot read log"}`, http.StatusInternalServerError)
		return
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	// Remove trailing empty entry produced when the file ends with a newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}

	w.WriteHeader(http.StatusOK)
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}

// hostAPIServeLogsFollow streams the log file to w, keeping the connection
// open until the session reaches a terminal state and 5 seconds of silence
// elapse, or the client disconnects.
func hostAPIServeLogsFollow(w http.ResponseWriter, r *http.Request, targetSession, logPath string, s *Sidecar) {
	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, `{"error":"cannot open log"}`, http.StatusInternalServerError)
		return
	}
	defer f.Close()

	flusher, canFlush := w.(http.Flusher)

	// isTerminal checks the DB for a terminal agent state.
	isTerminal := func() bool {
		st, dbErr := s.cfg.DB.CurrentStatus(targetSession)
		if dbErr != nil || st == nil {
			return false
		}
		return isHostAPITerminalState(agent.AgentState(st.State))
	}

	// If the session is already in a terminal state, send the full existing
	// log and return immediately.
	if isTerminal() {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	// Stream the existing content first, then poll for new lines.
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
	if canFlush {
		flusher.Flush()
	}

	ctx := r.Context()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var (
		terminalDetected bool
		silenceDeadline  time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, readErr := io.Copy(w, f)
			if readErr != nil {
				return
			}
			if n > 0 && canFlush {
				flusher.Flush()
			}

			if terminalDetected {
				// Reset the silence deadline each time new content arrives.
				if n > 0 {
					silenceDeadline = time.Now().Add(5 * time.Second)
				} else if time.Now().After(silenceDeadline) {
					// 5 s of silence after terminal state: close the connection.
					return
				}
			} else {
				if isTerminal() {
					terminalDetected = true
					silenceDeadline = time.Now().Add(5 * time.Second)
				}
			}
		}
	}
}

// hostAPIHandler returns an http.Handler that exposes host-side tmux operations
// to agents running inside the container via a Unix socket. Routes:
//
//	POST /spawn         — spawn a new worktree session (coordinator only)
//	POST /review        — run review agents against a PR (workers and coordinators)
//	POST /cleanup       — clean up an existing session (coordinator only)
//	POST /switch        — switch the tmux client to a session
//	GET  /list-sessions — list active sessions (role-scoped)
//	GET  /checkin       — return conversation history for a session (coordinator only)
//	GET  /stats         — return stats/events for rendering (all roles)
//	GET  /db/query      — run a single read-only SQL statement (coordinator only)
//	GET  /db/schema     — return CREATE TABLE / CREATE INDEX DDL (coordinator only)
//	GET  /db/tables     — return user table names (coordinator only)
//	POST /prompt        — deliver a prompt to a target session (role-scoped)
//	POST /merge         — enqueue a PR for the merge queue (coordinator only)
//	GET  /merges        — list merge queue entries (coordinator only)
//	POST /merges/cancel — cancel a watching merge queue entry (coordinator only)
//	POST /event         — write a lifecycle event to the host DB (all roles)
//	POST /escalate      — escalate to coordinator (worker sessions)
//	POST /investigate   — spawn an investigate-agent session (worker sessions)
//	POST /feedback      — append a feedback entry to the host feedback.jsonl (all roles)
//
// /db/query, /db/schema, /db/tables are coordinator-only because /db/query
// exposes a strict superset of /checkin: raw cross-session payloads (e.g.
// SELECT * FROM harness_frames) versus /checkin's single-session rendered
// view. Gating these at the same level as /checkin preserves the
// cross-session privilege isolation /checkin already enforces. The /stats
// analogue does not apply — /stats is aggregate counts, /db/query is
// row-level conversation content (#1467 round-3 review).
//
// Role-based permissions are enforced based on s.cfg.AgentRole and
// s.cfg.SessionName. Workers have restricted access; coordinators have broader
// access. All denied requests return HTTP 403 with a structured JSON error.
func (s *Sidecar) hostAPIHandler() http.Handler {
	mux := http.NewServeMux()

	// writeJSON writes a JSON response with the given status code.
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			http.Error(w, `{"error":"internal: marshal response"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}

	// writeError writes a JSON error response.
	writeError := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]string{"error": msg})
	}

	// requirePost writes a 405 and returns false if the method is not POST.
	// Returns true when the method is POST (caller should proceed).
	requirePost := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return false
		}
		return true
	}

	// requireGet writes a 405 and returns false if the method is not GET.
	// Returns true when the method is GET (caller should proceed).
	requireGet := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return false
		}
		return true
	}

	// requireCoordinator checks that the calling sidecar is a coordinator
	// session. Uses the DB-backed isCoordinatorSession check (same helper as
	// isCoordinator) which reads root_agent_name and falls back to AgentRole
	// for pre-migration rows. Returns false and writes HTTP 403 if not.
	requireCoordinator := func(w http.ResponseWriter, operation string) bool {
		if !isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()) {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("workers cannot perform %s", operation))
			return false
		}
		return true
	}

	// prismBinary returns the path to the prism binary (this process).
	// Uses os.Executable() — consistent with StartSidecarWithOpts — to get
	// the absolute path at binary launch time, avoiding CWD-relative resolution.
	// When Config.PrismBinaryPath is set (e.g. in tests), it is used instead.
	prismBinary := func() string {
		if s.cfg.PrismBinaryPath != "" {
			return s.cfg.PrismBinaryPath
		}
		self, err := os.Executable()
		if err != nil {
			return os.Args[0]
		}
		return self
	}

	// GET /stats
	// Query params:
	//   view     — one of: summary, doomloops, denials, asks, detail (required)
	//   session  — session name filter (optional for doomloops/denials/asks; required for detail)
	//   days     — look-back window in days (default 7 for event views; unused for summary/detail)
	//   repo     — repo filter (summary only, optional)
	//   since    — sinceMs timestamp as string (summary only, optional)
	//
	// Response shapes (all roles permitted — read-only):
	//   view=summary → {"sessions":[...db.Session...]} with token/cost totals per-session
	//     when session is set: {"type":"detail","session":{...db.Session...}} (same as view=detail)
	//   view=doomloops → {"events":[...db.Event...]}
	//   view=denials   → {"events":[...db.Event...]}
	//   view=asks      → {"events":[...db.Event...]}
	//   view=detail    → {"session":{...db.Session...}} (single-session incarnation detail)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}

		q := r.URL.Query()
		view := q.Get("view")
		sessionFilter := q.Get("session")
		daysStr := q.Get("days")
		sinceStr := q.Get("since")
		repoFilter := q.Get("repo")

		// Parse days (default 7 for event views).
		days := 7
		if daysStr != "" {
			if n, parseErr := strconv.Atoi(daysStr); parseErr == nil && n > 0 {
				days = n
			}
		}

		// Parse sinceMs for summary view.
		var sinceMs int64
		if sinceStr != "" {
			if ms, parseErr := strconv.ParseInt(sinceStr, 10, 64); parseErr == nil {
				sinceMs = ms
			}
		}

		switch view {
		case "doomloops":
			since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
			events, err := s.cfg.DB.QueryDoomLoopEvents(sessionFilter, since)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
			if events == nil {
				events = []db.Event{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": events})

		case "denials":
			if sessionFilter != "" {
				status, dbErr := s.cfg.DB.CurrentStatus(sessionFilter)
				evts, _ := s.cfg.DB.AllSessionEvents(sessionFilter)
				if dbErr == nil && status == nil && len(evts) == 0 {
					writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionFilter))
					return
				}
			}
			since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
			events, err := s.cfg.DB.QueryPermissionEvents("permission_denied", sessionFilter, since)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
			if events == nil {
				events = []db.Event{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": events})

		case "asks":
			if sessionFilter != "" {
				status, dbErr := s.cfg.DB.CurrentStatus(sessionFilter)
				evts, _ := s.cfg.DB.AllSessionEvents(sessionFilter)
				if dbErr == nil && status == nil && len(evts) == 0 {
					writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionFilter))
					return
				}
			}
			since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
			events, err := s.cfg.DB.QueryPermissionEvents("permission_ask", sessionFilter, since)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
			if events == nil {
				events = []db.Event{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": events})

		case "summary":
			var sessions []db.Session
			var err error
			switch {
			case repoFilter != "" && sinceMs > 0:
				sessions, err = s.cfg.DB.SessionsForRepoSince(repoFilter, sinceMs)
			case repoFilter != "":
				sessions, err = s.cfg.DB.SessionsForRepo(repoFilter)
			case sinceMs > 0:
				sessions, err = s.cfg.DB.SessionsSince(sinceMs)
			default:
				sessions, err = s.cfg.DB.AllSessions()
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
			if sessions == nil {
				sessions = []db.Session{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})

		case "detail":
			if sessionFilter == "" {
				writeError(w, http.StatusBadRequest, "session is required for view=detail")
				return
			}
			// Try exact session_name match first.
			sess, err := s.cfg.DB.MostRecentSessionForName(sessionFilter)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
			if sess == nil {
				// Try UUID prefix/exact match.
				if len(sessionFilter) == 36 {
					sess, err = s.cfg.DB.SessionByInstanceID(sessionFilter)
					if err != nil {
						writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
						return
					}
				}
				if sess == nil {
					writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionFilter))
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"session": sess})

		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown view %q — must be one of: summary, doomloops, denials, asks, detail", view))
		}
	})

	// GET /list-sessions
	// Query param: all=true (optional, coordinator only)
	// Response: JSON array of session status objects
	mux.HandleFunc("/list-sessions", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}

		showAll := r.URL.Query().Get("all") == "true"

		// Workers cannot use all=true.
		if showAll && s.cfg.AgentRole != "coordinator" {
			writeError(w, http.StatusForbidden, "workers cannot list sessions across all repos (all=true requires coordinator role)")
			return
		}

		var (
			sessions []db.Status
			err      error
		)
		if showAll {
			sessions, err = s.cfg.DB.AllActiveStatus()
		} else {
			// Same-repo: everything. Other repos: only coordinators
			// (root_agent_name = 'coordinator', with @main name-heuristic
			// fallback for pre-migration rows where root_agent_name IS NULL).
			ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
			if repoErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
				return
			}
			sessions, err = s.cfg.DB.AllActiveStatusForRepoAndOtherCoordinators(ownRepo)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
			return
		}

		// Return empty array rather than null when no sessions found.
		if sessions == nil {
			sessions = []db.Status{}
		}
		writeJSON(w, http.StatusOK, sessions)
	})

	// GET /checkin
	// Query params: session (required), last (default 10), types (optional),
	//               from (optional cursor), before (optional cursor)
	// Permission: coordinator only; own repo sessions or cross-repo @main sessions.
	mux.HandleFunc("/checkin", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "checkin") {
			return
		}

		q := r.URL.Query()
		targetSession := q.Get("session")
		if targetSession == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Permission check: coordinator can access own-repo sessions and
		// any cross-repo coordinator (@main) session.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}
		targetRepo, targetRepoErr := repoFromSession(targetSession)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		crossRepo := targetRepo != ownRepo
		if crossRepo && !isCoordinatorSession(targetSession, s.cfg.DB, s.logger()) {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("cross-repo checkin can only target coordinators (<repo>@main), got %q", targetSession))
			return
		}

		// Parse limit (default 10).
		limit := 10
		if lastStr := q.Get("last"); lastStr != "" {
			if n, parseErr := strconv.Atoi(lastStr); parseErr == nil && n > 0 {
				limit = n
			}
		}

		// Parse optional cursor params.
		var afterPtr, beforePtr *string
		if fromStr := q.Get("from"); fromStr != "" {
			afterPtr = &fromStr
		}
		if beforeStr := q.Get("before"); beforeStr != "" {
			beforePtr = &beforeStr
		}

		// Parse optional types filter.
		var types []string
		if typesStr := q.Get("types"); typesStr != "" {
			for _, t := range strings.Split(typesStr, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					types = append(types, t)
				}
			}
		}

		// Fetch session state.
		status, statusErr := s.cfg.DB.CurrentStatus(targetSession)
		var state string
		if statusErr == nil && status != nil {
			state = status.State
		}

		var events []db.Event
		if len(types) > 0 {
			// Explicit --types: return raw events with the type filter, same as
			// the runCheckinSessionRaw path in the CLI.
			var err error
			events, err = s.cfg.DB.QueryEvents(targetSession, limit, beforePtr, afterPtr, types)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
		} else {
			// Default (no --types): replicate the assistant-turn-centric logic from
			// runCheckinSession / renderCheckinTurns so that --last N means N
			// assistant turns, not N raw events.

			// Primary query: fetch last N msg_assistant events.
			assistantEvents, err := s.cfg.DB.QueryEvents(targetSession, limit, beforePtr, afterPtr, []string{"msg_assistant"})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}

			if len(assistantEvents) > 0 {
				// Collect all messageIds from the assistant events.
				messageIDs := make([]string, 0, len(assistantEvents))
				for _, e := range assistantEvents {
					msgID := extractMessageIDFromPayload(e.Payload)
					if msgID != "" {
						messageIDs = append(messageIDs, msgID)
					}
				}

				// Secondary query: fetch tool calls, results, permission events, and
				// thinking events that share a messageId with one of the assistant events.
				childTypes := []string{"tool_call", "tool_result", "permission_ask", "permission_denied", "thinking"}
				childEvents, _ := s.cfg.DB.QueryEventsByMessageIDs(targetSession, messageIDs, childTypes)

				// Determine the time window for msg_user events.
				earliest := assistantEvents[0].CreatedAt
				latest := assistantEvents[len(assistantEvents)-1].CreatedAt
				for _, ae := range assistantEvents {
					if ae.CreatedAt.Before(earliest) {
						earliest = ae.CreatedAt
					}
					if ae.CreatedAt.After(latest) {
						latest = ae.CreatedAt
					}
				}

				// Fetch msg_user events and filter to the time window.
				allUserEvents, _ := s.cfg.DB.QueryEvents(targetSession, 0, nil, nil, []string{"msg_user"})
				var userEvents []db.Event
				for _, ue := range allUserEvents {
					if !ue.CreatedAt.Before(earliest) && !ue.CreatedAt.After(latest) {
						userEvents = append(userEvents, ue)
					}
				}

				// Merge all into a single sorted timeline (insertion sort, ASC).
				merged := make([]db.Event, 0, len(assistantEvents)+len(childEvents)+len(userEvents))
				merged = append(merged, assistantEvents...)
				merged = append(merged, childEvents...)
				merged = append(merged, userEvents...)
				for i := 1; i < len(merged); i++ {
					for j := i; j > 0 && merged[j].CreatedAt.Before(merged[j-1].CreatedAt); j-- {
						merged[j], merged[j-1] = merged[j-1], merged[j]
					}
				}
				events = merged
			}
			// If no assistant events exist, events stays nil → returned as [].
		}

		// Ensure empty arrays rather than null.
		if events == nil {
			events = []db.Event{}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"session": targetSession,
			"state":   state,
			"events":  events,
		})
	})

	// GET /logs
	// Query params: session (required), tail (optional int ≥ 0), follow (optional bool),
	//               source (optional: "sidecar" [default] or "agent-run")
	// Permission: coordinator only; own-repo sessions or cross-repo @main sessions.
	// Returns 404 with JSON error when the log file does not exist.
	// When follow=true, streams new lines and closes after the session reaches a
	// terminal state and 5 s of silence elapse.
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "logs") {
			return
		}

		q := r.URL.Query()
		targetSession := q.Get("session")
		if targetSession == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Validate session name format before semantic permission checks.
		// Valid session names are "repo@branch" — slashes and dot-segments are
		// not valid and would escape the logs directory via filepath.Join.
		if strings.Contains(targetSession, "/") || strings.Contains(targetSession, "..") {
			writeError(w, http.StatusBadRequest, "invalid session name: must not contain '/' or '..'")
			return
		}

		// Permission check: own-repo sessions or cross-repo @main sessions.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}
		targetRepo, targetRepoErr := repoFromSession(targetSession)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		if targetRepo != ownRepo && !isCoordinatorSession(targetSession, s.cfg.DB, s.logger()) {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("cross-repo logs can only target coordinators (<repo>@main), got %q", targetSession))
			return
		}

		// Resolve log file path. source=agent-run selects the bwrap harness log;
		// the default (source absent or "sidecar") selects the sidecar log.
		logSource := q.Get("source")
		var logPath string
		var pathErr error
		switch logSource {
		case "agent-run":
			logPath, pathErr = prismsession.AgentRunLogPath(targetSession)
			if pathErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot resolve agent-run log path: "+pathErr.Error())
				return
			}
			if _, statErr := os.Stat(logPath); os.IsNotExist(statErr) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("no agent-run log file for session %s", targetSession))
				return
			}
		default:
			logPath, pathErr = prismsession.SidecarLogPath(targetSession)
			if pathErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot resolve log path: "+pathErr.Error())
				return
			}
			if _, statErr := os.Stat(logPath); os.IsNotExist(statErr) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("no log file for session %s", targetSession))
				return
			}
		}

		// Parse optional tail param (non-negative integer).
		tailN := 0
		tailSet := false
		if tailStr := q.Get("tail"); tailStr != "" {
			if n, parseErr := strconv.Atoi(tailStr); parseErr == nil && n >= 0 {
				tailN = n
				tailSet = true
			}
		}

		follow := q.Get("follow") == "true"

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		if follow {
			hostAPIServeLogsFollow(w, r, targetSession, logPath, s)
			return
		}

		if tailSet {
			hostAPIServeLogsTail(w, logPath, tailN)
			return
		}

		// Full log: stream the whole file.
		f, openErr := os.Open(logPath)
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot open log: "+openErr.Error())
			return
		}
		defer f.Close()
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	})

	// POST /prompt
	// Request:  {"session":"<target>", "prompt":"<text>", "deliver_as":"<mode>", "delivery_id":"<uuid>"}
	// Permission: worker → own coordinator (@main) only;
	//             coordinator → own repo any session, cross-repo coordinator only.
	//
	// Idempotency: each request carries an optional delivery_id (UUID minted
	// by the sender). The receiving sidecar tracks recent IDs in a bounded
	// LRU set; repeats are dropped before any frame is enqueued and the
	// response carries {"replayed":true} so the sender can log/observe. When
	// delivery_id is empty the request is treated as legacy and dedup is
	// skipped (the frame is delivered unconditionally). Issue #1685.
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var req struct {
			Session string `json:"session"`
			Prompt  string `json:"prompt"`
			// Source identifies the logical origin of the delivery. When
			// "review-complete" the sidecar clears reviewingInFlight AFTER the
			// prompt frame is enqueued on the outbound writer (synchronous-
			// success path) or after flushPendingReplay re-enqueues the frame
			// post-reconnect (buffered path) — see #1843. Clearing only after
			// the frame is on the wire keeps the events.go suppression guards
			// armed through the delivery window, preventing the spurious
			// "finished" notification class (#1372 / #1652). All other
			// deliveries (coordinator follow-ups, merge-queue notifications,
			// etc.) leave the flag unchanged — clearing on non-review prompts
			// would prematurely end the reviewing window (#1372, AC #7).
			Source string `json:"source,omitempty"`
			// DeliverAs controls the delivery mode for same-session PI targets.
			// Accepted values: "steer", "followUp", "nextTurn". When omitted the
			// sidecar defaults to "nextTurn" (existing behaviour, backward
			// compatible with callers that do not set this field). Unknown values
			// are rejected with HTTP 400 before any frame is enqueued.
			DeliverAs string `json:"deliver_as,omitempty"`
			// DeliveryID is the sender-minted UUID used for idempotency. When
			// non-empty, the receiving sidecar dedups against its in-memory set
			// and drops repeats. When empty, dedup is skipped (legacy callers).
			// Issue #1685.
			DeliveryID string `json:"delivery_id,omitempty"`
		}
		// /prompt uses the bumped 16 MiB body cap (issue #1848): worker spawn
		// prompts may legitimately embed file attachments and large context
		// blobs, so the default 1 MiB ceiling is too tight for this surface.
		if status, err := decodeRequestJSON(w, r, &req, promptMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		if req.Prompt == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}

		// Validate deliver_as before any permission checks or delivery. Unknown
		// values are rejected immediately so the caller gets a clear error
		// instead of silently using the default.
		deliverAs := req.DeliverAs
		if deliverAs == "" {
			deliverAs = "nextTurn"
		} else if deliverAs != "steer" && deliverAs != "followUp" && deliverAs != "nextTurn" {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("invalid deliver_as %q: accepted values are \"steer\", \"followUp\", \"nextTurn\"", deliverAs))
			return
		}

		// Same-session targeting bypasses cross-session auth: the request
		// is being routed to the sidecar's own pipe-connected harness, so
		// there is no cross-session boundary to enforce. This is the path
		// taken by the host-side `prism prompt <pi-session>` CLI when it
		// dials the per-session host-API socket directly, and by the
		// promptdelivery.DeliverToSession helper used by the review-complete
		// monitor, coordinator notify, and merge-queue watcher (#1364).
		// We still gate on the harness having a TransportSocketPipe shape
		// so an HTTP-harness session — which uses HTTP-port delivery — does not
		// silently route through this branch.
		if req.Session == s.cfg.SessionName {
			if shape, ok := harness.ShapeOf(s.cfg.HarnessName); ok && shape == harness.TransportSocketPipe {
				// Reject delivery when the pi session is in "waiting" state,
				// consistent with the `prism prompt` CLI behaviour (#1364).
				if s.cfg.DB != nil {
					selfStatus, dbErr := s.cfg.DB.CurrentStatus(s.cfg.SessionName)
					if dbErr == nil && selfStatus != nil && selfStatus.State == "waiting" {
						writeError(w, http.StatusConflict,
							fmt.Sprintf("session %q is in waiting state — prompt not delivered", s.cfg.SessionName))
						return
					}
				}

				// Idempotency check: if the sender supplied a delivery_id and we
				// have seen it recently, this is a repeat — drop it and respond
				// 200 with {"replayed":true} so the sender can observe. The
				// dedup set is bounded (LRU, capacity 256) and per-sidecar.
				// See delivery_dedup.go and issue #1685.
				if req.DeliveryID != "" && s.promptDedup != nil {
					if s.promptDedup.markSeen(req.DeliveryID) {
						s.logger().Printf("sidecar: host-API /prompt: dedup hit, dropping repeat delivery_id=%s (session=%s)", req.DeliveryID, req.Session)
						writeJSON(w, http.StatusOK, map[string]bool{"replayed": true})
						return
					}
				}

				// reviewingInFlight handling for source=="review-complete":
				// see issue #1843. The flag MUST remain true until the prompt
				// is actually on the wire (or, on the buffered path, has been
				// re-enqueued from flushPendingReplay) — otherwise an
				// incidental state_change{finished} / session.idle arriving
				// between this point and the actual delivery evades the
				// suppression guards in events.go and fires a spurious
				// "finished" notification (the #1372 / #1652 race class).
				//
				// Strategy (#1843):
				//   - Synchronous-success path: call DeliverPrompt first;
				//     clear reviewingInFlight only after it returns true.
				//   - Buffered (PI-disconnected) path: tag the pending entry
				//     with Source=="review-complete" and leave the flag set.
				//     flushPendingReplay clears it after the replayed frame is
				//     successfully enqueued post-reconnect.
				//
				// Non-review-complete deliveries (coordinator follow-ups,
				// merge-queue notifications) must NEVER clear the flag — doing
				// so would prematurely end the reviewing window (#1372, AC #7).
				s.logger().Printf("sidecar: host-API /prompt: delivering via socket-pipe to self (%s) deliver_as=%s", req.Session, deliverAs)
				if !s.DeliverPrompt(req.Prompt, deliverAs) {
					// PI extension is disconnected. Buffer the delivery so it
					// will be flushed on next handshake with replay=true, then
					// respond 200 — the delivery is accepted but deferred.
					// Pre-#1685 behaviour returned 503; the new contract is that
					// a single /prompt call delivers exactly once (after
					// reconnect, marked replay) rather than failing the call.
					// Issue #1685 AC #7. Source is propagated so flushPendingReplay
					// can clear reviewingInFlight after the replayed frame is
					// enqueued on the new connection (#1843).
					s.bufferPendingReplay(pendingReplayDelivery{
						DeliveryID: req.DeliveryID,
						Text:       req.Prompt,
						DeliverAs:  deliverAs,
						Source:     req.Source,
					})
					s.logger().Printf("sidecar: host-API /prompt: PI disconnected, buffered for replay (session=%s deliver_as=%s delivery_id=%s source=%s)", req.Session, deliverAs, req.DeliveryID, req.Source)
					writeJSON(w, http.StatusOK, map[string]bool{"buffered": true})
					return
				}
				// Synchronous success: the prompt frame is enqueued on the
				// outbound writer. Now it is safe to clear reviewingInFlight
				// for the review-complete case — the suppression guards have
				// served their purpose, and the immediately-following turn_start
				// must observe the cleared flag to transition normally to active.
				if req.Source == "review-complete" {
					s.mu.Lock()
					s.reviewingInFlight = false
					s.mu.Unlock()
				}
				writeJSON(w, http.StatusOK, map[string]string{})
				return
			}
		}

		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}

		targetRepo, targetRepoErr := repoFromSession(req.Session)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		crossRepo := targetRepo != ownRepo

		if s.cfg.AgentRole == "coordinator" {
			// Coordinator: own repo any session allowed; cross-repo only @main.
			if crossRepo && !isCoordinatorSession(req.Session, s.cfg.DB, s.logger()) {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("cross-repo prompts can only target coordinators (<repo>@main), got %q", req.Session))
				return
			}
		} else {
			// Worker: only own coordinator allowed.
			// DB-backed: look up the coordinator for this repo.
			coordStatus, coordErr := s.cfg.DB.CoordinatorForRepo(ownRepo)
			if coordErr != nil {
				s.logger().Printf("sidecar: /prompt worker check: DB error looking up coordinator for %q: %v — falling back to name heuristic", ownRepo, coordErr)
				coordStatus = nil
			}
			var ownCoordinator string
			if coordStatus != nil {
				ownCoordinator = coordStatus.SessionName
			} else {
				// No coordinator with root_agent_name='coordinator' in DB.
				// Fall back to name convention for pre-migration rows.
				fallbackCoord := ownRepo + "@main"
				fallbackStatus, _ := s.cfg.DB.CurrentStatus(fallbackCoord)
				if fallbackStatus != nil {
					// Pre-migration row found via name convention — log deprecation
					// only when the fallback actually finds a row.
					s.logger().Printf("[deprecation] sidecar: /prompt worker check: no DB-backed coordinator for %q — using name convention %q (pre-migration row)", ownRepo, fallbackCoord)
					ownCoordinator = fallbackCoord
				} else {
					// Normal "no coordinator running" state — silent fallback.
					ownCoordinator = fallbackCoord
				}
			}
			if req.Session != ownCoordinator {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("workers can only prompt their own coordinator (%s), got %q", ownCoordinator, req.Session))
				return
			}
		}

		// Deliver via prism prompt on the host.
		args := []string{"prompt", req.Session, "--prompt", req.Prompt}
		s.logger().Printf("sidecar: host-API /prompt: prism prompt %s <omitted>", req.Session)

		// Per-endpoint timeout: 5 min. `prism prompt` writes one bus event and
		// (in pi-harness mode) waits for the harness side to ACK delivery; the
		// host side can stall on a wedged tmux/pi pipe, but 5 min is a generous
		// outer bound that still surfaces a true hang. On timeout we return
		// 504 Gateway Timeout; on client disconnect we return 499 (the de-facto
		// "client closed request" code used by nginx/HAProxy).
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /prompt: context fired (%v): killed child after %v", ctx.Err(), 5*time.Minute)
				writeError(w, status, fmt.Sprintf("prompt delivery aborted: %v", ctx.Err()))
				return
			}
			s.logger().Printf("sidecar: host-API /prompt: %v: %s", err, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("prompt delivery failed: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{})
	})

	// POST /spawn
	// Request:  {"branch":"my-feature","prompt":"...","agent":"worker","profile":"gemini-hybrid","harness":"pi"}
	// The "repo" field is accepted but ignored — the sidecar always substitutes
	// its own repo (derived from its session name) so that a client sending a
	// mount-path name (e.g. "prism-git") still spawns into the correct repo
	// (e.g. "nixos-config"). See issue #616.
	//
	// Optional field "model_variant_overrides" accepts a JSON-encoded
	// map[string]string produced by proxySpawn (cmd/spawn.go). Each entry is
	// forwarded as a --model-override role=model flag to the host-side prism
	// spawn invocation. A malformed JSON value is rejected with HTTP 400.
	// Absence of the field (the pre-#1263 behaviour) is treated as an empty map.
	// See issue #1263 (C2.PROXY proxy-spawn model-override parity).
	//
	// Optional field "abtest" accepts a two-element array of profile names that
	// are forwarded as --abtest flags to the host-side prism spawn. Exactly 0
	// or 2 values are accepted; 1 or 3+ return HTTP 400. When abtest is set,
	// "profile" must be absent. The response carries "session_names" (a two-
	// element array) rather than the singular "session_name". See issue #1330.
	//
	// Response: {"session_name":"nixos-config@my-feature"} | {"error":"..."}
	//           {"session_names":["nixos-config@branch-a","nixos-config@branch-b"]} (abtest)
	mux.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if !requireCoordinator(w, "spawn") {
			return
		}
		var req struct {
			Repo                  string   `json:"repo"` // accepted but ignored — ownRepo is always used
			Branch                string   `json:"branch"`
			Prompt                string   `json:"prompt"`
			Agent                 string   `json:"agent"`
			Profile               string   `json:"profile"`
			Model                 string   `json:"model"`
			Variant               string   `json:"variant"`
			Isolation             string   `json:"isolation"`
			Harness               string   `json:"harness"`
			IgnoreConcurrencyCap  bool     `json:"ignore_concurrency_cap"`
			Reuse                 bool     `json:"reuse"`
			ModelVariantOverrides string   `json:"model_variant_overrides"` // JSON-encoded map[string]string; see #1263
			Abtest                []string `json:"abtest"`                  // two-element array of profile names; see #1330
		}
		// /spawn body cap: default 1 MiB (issue #1848). DisallowUnknownFields
		// is applied via decodeRequestJSON — already strict on this endpoint.
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}

		// Parse model_variant_overrides when present. An empty string is
		// treated as no overrides (backwards-compatible). A non-empty string
		// must decode to map[string]string — a malformed value returns 400.
		var modelsByRole map[string]string
		if req.ModelVariantOverrides != "" {
			if err := json.Unmarshal([]byte(req.ModelVariantOverrides), &modelsByRole); err != nil {
				writeError(w, http.StatusBadRequest, "invalid model_variant_overrides: must be a JSON-encoded map[string]string: "+err.Error())
				return
			}
		}
		if req.Branch == "" {
			writeError(w, http.StatusBadRequest, "branch is required")
			return
		}
		// Reject an empty prompt at the API boundary (layer 3 of issue #1891).
		// The CLI proxy (proxySpawn) already rejects empty prompts at layers 1+2,
		// so a well-behaved client never reaches this branch. Defence-in-depth:
		// a malformed or alternate client that POSTs {"prompt":""} would otherwise
		// produce a session that comes up successfully but sits idle forever
		// because no --prompt argument is forwarded to the host-side prism spawn.
		if req.Prompt == "" {
			writeError(w, http.StatusBadRequest, "prompt is required — the request body must include a non-empty \"prompt\" field")
			return
		}
		// Validate --abtest: must be 0 or 2 values; mutually exclusive with profile.
		if len(req.Abtest) == 1 || len(req.Abtest) > 2 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("--abtest requires exactly two profile names (got %d)", len(req.Abtest)))
			return
		}
		if len(req.Abtest) == 2 && req.Profile != "" {
			writeError(w, http.StatusBadRequest, "--abtest and --profile are mutually exclusive")
			return
		}
		// Validate abtest profile names against the known profile set to prevent
		// flag injection (e.g. a name like "--isolation host" would inject CLI
		// flags into the host-side prism spawn invocation).
		if len(req.Abtest) == 2 {
			// Structural check: reject names that could be interpreted as flags
			// or contain path separators.
			validProfileName := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
			for _, name := range req.Abtest {
				if !validProfileName.MatchString(name) {
					writeError(w, http.StatusBadRequest, fmt.Sprintf(
						"invalid abtest profile name %q: must contain only letters, digits, hyphens, and underscores", name))
					return
				}
			}
			// Semantic check: validate against the known profile set when
			// profiles.json is accessible. On load error, the structural check
			// above is still in effect.
			if pf, pfErr := config.LoadProfiles(); pfErr == nil {
				known := config.AvailableProfileNames(pf)
				knownSet := make(map[string]bool, len(known))
				for _, n := range known {
					knownSet[n] = true
				}
				for _, name := range req.Abtest {
					if !knownSet[name] {
						writeError(w, http.StatusBadRequest, fmt.Sprintf(
							"unknown abtest profile name %q: must be one of: %s",
							name, strings.Join(known, ", ")))
						return
					}
				}
			}
		}
		// Validate harness before spawning. An empty string means the client
		// did not pass --harness explicitly; the host-side spawn will derive
		// the harness from the profile slot as designed (#1421). Only validate
		// when the field is non-empty.
		if req.Harness != "" {
			if _, ok := harness.Lookup(req.Harness); !ok {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"unknown harness %q: valid harnesses: %s",
					req.Harness, strings.Join(harness.Names(), ", ")))
				return
			}
		}
		// Validate isolation server-side as defence-in-depth (the client
		// already validated, but a non-prism client could send anything).
		if req.Isolation != "" && !config.IsValidIsolationMode(req.Isolation) {
			valid := make([]string, len(config.ValidIsolationModes))
			for i, m := range config.ValidIsolationModes {
				valid[i] = string(m)
			}
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown isolation mode %q; valid values: %s", req.Isolation, strings.Join(valid, ", ")))
			return
		}

		// Always derive the repo from the sidecar's own session name.
		// This means a client that sends the wrong repo (e.g. a container
		// mount-path name instead of the actual repo name) is silently
		// corrected. The own-repo restriction is enforced implicitly: the
		// sidecar can only spawn into its own repo.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}

		args := []string{"spawn", "--branch", req.Branch}
		// Pass --prompt-source proxy-spawn unconditionally so the host-side spawn
		// records the correct C.4.SRC discriminator regardless of whether a prompt
		// is included (C.4.SRC, issue #1148).
		args = append(args, "--prompt-source", "proxy-spawn")
		if req.Prompt != "" {
			args = append(args, "--prompt", req.Prompt)
		}
		if req.Agent != "" {
			args = append(args, "--agent", req.Agent)
		}
		// --abtest and --profile are mutually exclusive; validated above.
		if len(req.Abtest) == 2 {
			args = append(args, "--abtest", req.Abtest[0], "--abtest", req.Abtest[1])
		} else if req.Profile != "" {
			args = append(args, "--profile", req.Profile)
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.Variant != "" {
			args = append(args, "--variant", req.Variant)
		}
		if req.Isolation != "" {
			args = append(args, "--isolation", req.Isolation)
		}
		if req.IgnoreConcurrencyCap {
			args = append(args, "--ignore-concurrency-cap")
		}
		if req.Reuse {
			args = append(args, "--reuse")
		}
		// Forward per-role model overrides as repeating --model-override flags.
		for role, model := range modelsByRole {
			args = append(args, "--model-override", role+"="+model)
		}
		// Only pass --harness when the client explicitly set it. When absent,
		// the host-side spawn derives the harness from the profile slot (#1421).
		if req.Harness != "" {
			args = append(args, "--harness", req.Harness)
		}
		args = append(args, "--repo", ownRepo)

		// Log without the prompt value — it may contain sensitive context.
		logArgs := []string{"spawn", "--branch", req.Branch}
		if req.Prompt != "" {
			logArgs = append(logArgs, "--prompt", "<omitted>")
		}
		if req.Agent != "" {
			logArgs = append(logArgs, "--agent", req.Agent)
		}
		if len(req.Abtest) == 2 {
			logArgs = append(logArgs, "--abtest", req.Abtest[0], "--abtest", req.Abtest[1])
		} else if req.Profile != "" {
			logArgs = append(logArgs, "--profile", req.Profile)
		}
		if req.Model != "" {
			logArgs = append(logArgs, "--model", req.Model)
		}
		if req.Variant != "" {
			logArgs = append(logArgs, "--variant", req.Variant)
		}
		if req.Isolation != "" {
			logArgs = append(logArgs, "--isolation", req.Isolation)
		}
		if req.IgnoreConcurrencyCap {
			logArgs = append(logArgs, "--ignore-concurrency-cap")
		}
		if req.Reuse {
			logArgs = append(logArgs, "--reuse")
		}
		for role, model := range modelsByRole {
			logArgs = append(logArgs, "--model-override", role+"="+model)
		}
		if req.Harness != "" {
			logArgs = append(logArgs, "--harness", req.Harness)
		}
		logArgs = append(logArgs, "--repo", ownRepo)
		s.logger().Printf("sidecar: host-API /spawn: prism %s", strings.Join(logArgs, " "))

		// Per-endpoint timeout: 10 min. `prism spawn` can legitimately take a
		// while — it creates a git worktree, sets up a tmux session, and may
		// pull a container image on first use. 10 min is the documented outer
		// bound (issue #1847). On timeout returns 504 Gateway Timeout; on
		// client disconnect returns 499 ("client closed request").
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /spawn: context fired (%v): killed child after %v", ctx.Err(), 10*time.Minute)
				writeError(w, status, fmt.Sprintf("spawn aborted: %v", ctx.Err()))
				return
			}
			s.logger().Printf("sidecar: host-API /spawn: %v: %s", err, out)
			msg := fmt.Sprintf("spawn failed: %v", err)
			if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
				msg = fmt.Sprintf("spawn failed: %v\n%s", err, trimmed)
			}
			writeError(w, http.StatusInternalServerError, msg)
			return
		}

		outStr := string(out)

		// Abtest path: prism spawn --abtest prints two session lines.
		// Parse both and return them in "session_names".
		if len(req.Abtest) == 2 {
			sessionNames := parseAllSpawnSessionNames(outStr)
			if len(sessionNames) == 0 {
				// Fallback: derive from ownRepo@branch-profile (best effort).
				for _, p := range req.Abtest {
					sessionNames = append(sessionNames, ownRepo+"@"+req.Branch+"-"+p)
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"session_names": sessionNames})
			return
		}

		// prism spawn headless prints: session "name" created
		// Parse the session name from the output.
		sessionName := parseSpawnSessionName(outStr)
		if sessionName == "" {
			// Fallback: derive from ownRepo@branch (branch already sanitised by spawn).
			sessionName = ownRepo + "@" + req.Branch
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_name": sessionName})
	})

	// POST /review
	// Request:  {"pr_number":"123","agents":["review-code","review-goal"],"timeout":"10m"}
	// pr_number must be a numeric string (e.g. "123"). Non-numeric values are rejected.
	// agents is optional (empty = full set resolved by prism review on host).
	// timeout is optional (default: 10m).
	//
	// Response: plain-text chunked stream (Transfer-Encoding: chunked).
	//   - Each line of subprocess stdout is written to the response body and
	//     flushed immediately via http.Flusher as it is emitted.
	//   - After the subprocess exits, a sentinel line is appended:
	//       ReviewSentinelPassed ("__PRISM_REVIEW_PASSED__") on exit 0
	//       ReviewSentinelFailed ("__PRISM_REVIEW_FAILED__") on non-zero exit
	//   - Before streaming begins, validation failures are returned as JSON
	//     {"error":"..."} with the appropriate 4xx or 5xx status code.
	//   - If the subprocess cannot be started, HTTP 500 is returned before
	//     any streaming begins.
	//
	// The review is async: `prism review` spawns agents, registers a group,
	// starts a background monitor process, and exits quickly with an ack
	// message. The ack text (streamed line-by-line) is NOT the review result —
	// results are delivered later to the worker session via `prism prompt`.
	//
	// This endpoint is called by workers running inside containers that cannot
	// reach tmux directly. The sidecar runs on the host where tmux is available,
	// so it delegates to `prism review` on the host.
	//
	// PRISM_SESSION_NAME is injected into the subprocess environment so that
	// `review.LookupParentSession()` can determine the parent session name
	// (the sidecar daemon process does not run inside tmux, so the fallback
	// tmux.CurrentSession() call would fail).
	mux.HandleFunc("/review", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		// Pre-emptive: mark the calling session as `reviewing` immediately,
		// before spawning the `prism review` subprocess. The subprocess's
		// RunAsync also writes `reviewing` to the DB, but it does so several
		// seconds later (after argument parsing, PR validation, agent-spec
		// computation, etc.). In practice the worker idles in that window and
		// the idle-debounce path runs before the subprocess gets to its DB
		// write — leaving `currentDBState()` returning `active` and the
		// reviewing-state suppression in handleSessionStatus / the idle path
		// failing to fire. The result is a spurious "has finished"
		// notification before the review has even started.
		//
		// Writing here, in-process with the DB handle already open, closes
		// the race deterministically: by the time we return the first byte
		// to the worker, the row is already `reviewing`. See #1068. The
		// subprocess-side write in RunAsync remains as defence in depth.
		//
		// Retry on SQLITE_BUSY: the sidecar's socket-pipe reader goroutine
		// may hold the write lock for a brief turn_start upsert at the same
		// instant this handler fires (the agent called `prism review` as a
		// bash tool call, so turn_start fired moments before). SQLite WAL
		// mode serialises writers, but if the busy_timeout is not honoured
		// by the in-process pool the write can return SQLITE_BUSY immediately.
		// Three attempts with 10 ms backoff (≤30 ms total) outlast a typical
		// sidecar write without blocking the socket-pipe reader goroutine for
		// more than one write round-trip. See #1355.
		//
		// If all retries fail, return HTTP 500 so the agent receives a clear
		// failure it can retry, rather than silently spawning review agents
		// with the session stuck in a non-reviewing state (which causes the
		// review-complete prompt to be silently dropped).
		const (
			reviewingWriteAttempts = 3
			reviewingWriteBackoff  = 10 * time.Millisecond
		)
		if status, dbErr := s.cfg.DB.CurrentStatus(s.cfg.SessionName); dbErr != nil {
			s.logger().Printf("sidecar: host-API /review: pre-emptive reviewing write skipped — CurrentStatus error: %v", dbErr)
		} else if status == nil {
			s.logger().Printf("sidecar: host-API /review: pre-emptive reviewing write skipped — no agent_status row for session %q", s.cfg.SessionName)
		} else {
			// Retry on SQLITE_BUSY using the shared helper. See the block comment
			// above for why 3 attempts × 10 ms is the right budget here.
			var attempt int
			upsertErr := db.WithBusyRetry(reviewingWriteAttempts, reviewingWriteBackoff, func() error {
				err := s.cfg.DB.UpsertStatus(s.cfg.SessionName, status.Repo, status.Worktree, string(agent.StateReviewing), nil, nil)
				if err != nil && db.IsSQLiteBusy(err) {
					s.logger().Printf("sidecar: host-API /review: pre-emptive reviewing write SQLITE_BUSY (attempt %d/%d): %v", attempt+1, reviewingWriteAttempts, err)
				}
				attempt++
				return err
			})
			if upsertErr != nil {
				s.logger().Printf("sidecar: host-API /review: pre-emptive reviewing write failed after %d attempt(s): %v", reviewingWriteAttempts, upsertErr)
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("reviewing state write failed: %v", upsertErr))
				return
			}
			// Set the in-memory flag atomically now that the DB write succeeded.
			// handlePipeFrame's turn_start guard reads this flag instead of calling
			// currentDBState(), eliminating the SQLite read-after-write race (#1372).
			s.mu.Lock()
			s.reviewingInFlight = true
			s.mu.Unlock()
		}

		var req struct {
			PRNumber string   `json:"pr_number"`
			Agents   []string `json:"agents"`
			Timeout  string   `json:"timeout"`
			Rebase   bool     `json:"rebase"`
		}
		// /review body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.PRNumber == "" {
			writeError(w, http.StatusBadRequest, "pr_number is required")
			return
		}
		// Validate pr_number is numeric to prevent flag injection into the
		// subprocess (e.g. "--keep" being interpreted as a cobra flag).
		for _, c := range req.PRNumber {
			if c < '0' || c > '9' {
				writeError(w, http.StatusBadRequest, "pr_number must be a numeric string (e.g. \"123\")")
				return
			}
		}

		// Validate each agent name against the known set to prevent flag
		// injection via the --only argument.
		for _, name := range req.Agents {
			if !isKnownReviewAgent(name) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown agent name %q — must be one of: %s",
					name, strings.Join(knownReviewAgentNames(), ", ")))
				return
			}
		}

		args := []string{"review", req.PRNumber}
		if len(req.Agents) > 0 {
			args = append(args, "--only", strings.Join(req.Agents, ","))
		}
		if req.Timeout != "" {
			args = append(args, "--timeout", req.Timeout)
		}
		if req.Rebase {
			// Forward the inline-rebase request from the container worker so
			// the host-side subprocess runs the gate with --rebase. The gate
			// itself runs in the host subprocess (issue #1518).
			args = append(args, "--rebase")
		}

		s.logger().Printf("sidecar: host-API /review: prism %s", strings.Join(args, " "))

		// Async review: `prism review` spawns agents and a monitor, then exits
		// quickly (well under 30 s in practice). We stream its stdout as it
		// runs and write the sentinel after cmd.Wait(). The 30 s exec deadline
		// is the binding constraint — the client-side context (60 s) is set
		// wider to include connection overhead on top of this.
		const execTimeout = 30 * time.Second

		ctx, cancel := context.WithTimeout(r.Context(), execTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)

		// Anchor the subprocess CWD to the calling session's worktree so that
		// `prism review` can run `gh` and `git` commands against the correct
		// repository. Without this, the subprocess inherits the sidecar's CWD
		// (typically the tmux session-start directory), which may not be a git
		// repository — producing the "not a git repository" warning and causing
		// prism review to fall back to degraded per-agent git discovery.
		//
		// Lookup order:
		//   1. DB: agent_status.worktree for this session.
		//   2. Existence check: verify the directory is reachable on disk
		//      (defence in depth — the path is from the trusted DB but the
		//      worktree may have been cleaned up since the row was written).
		//   3. Fallback: log and proceed with default CWD when the lookup
		//      fails or the directory no longer exists, so that the existing
		//      degraded-context path keeps working rather than hard-failing.
		if status, dbErr := s.cfg.DB.CurrentStatus(s.cfg.SessionName); dbErr != nil {
			s.logger().Printf("sidecar: host-API /review: worktree lookup failed (DB error): %v — using default CWD", dbErr)
		} else if status == nil || status.Worktree == "" {
			s.logger().Printf("sidecar: host-API /review: worktree not set for session %q — using default CWD", s.cfg.SessionName)
		} else if _, statErr := os.Stat(status.Worktree); statErr != nil {
			s.logger().Printf("sidecar: host-API /review: worktree %q is not accessible: %v — using default CWD", status.Worktree, statErr)
		} else {
			cmd.Dir = status.Worktree
			s.logger().Printf("sidecar: host-API /review: subprocess CWD set to worktree %q", status.Worktree)
		}

		// Build the subprocess environment:
		// PRISM_SESSION_NAME: so review.LookupParentSession() resolves the
		// parent session correctly. The sidecar daemon is not inside tmux,
		// so the fallback tmux.CurrentSession() would fail without this.
		env := append(os.Environ(), "PRISM_SESSION_NAME="+s.cfg.SessionName)
		cmd.Env = env

		// Capture stderr separately so we can log it on failure.
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf

		// Obtain the subprocess stdout as a pipe so we can stream it to the
		// HTTP client line-by-line as it arrives, rather than buffering the
		// entire output with cmd.Output(). This ensures that progress lines
		// ("Review-Goal started", etc.) reach the container-mode worker within
		// 2 s of emission rather than all arriving at once at the end.
		stdoutPipe, pipeErr := cmd.StdoutPipe()
		if pipeErr != nil {
			writeError(w, http.StatusInternalServerError, "review: stdout pipe: "+pipeErr.Error())
			return
		}

		if startErr := cmd.Start(); startErr != nil {
			writeError(w, http.StatusInternalServerError, "review: start: "+startErr.Error())
			return
		}

		// Stream subprocess stdout to the HTTP response line-by-line.
		// We use Transfer-Encoding: chunked (the default for HTTP/1.1 when no
		// Content-Length is set) and flush after every line so the client
		// receives each progress line immediately.
		//
		// The response body is a plain text stream terminated by one of two
		// sentinel lines that the client consumes but does not print:
		//
		//   __PRISM_REVIEW_PASSED__   — subprocess exited 0
		//   __PRISM_REVIEW_FAILED__   — subprocess exited non-zero
		//
		// The sentinel conveys pass/fail status through the streaming body so
		// that proxyReviewAsync() can return an appropriate error without
		// needing a JSON wrapper around the stream.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)

		flusher, canFlush := w.(http.Flusher)

		scanner := bufio.NewScanner(stdoutPipe)
		// Allow lines up to 1 MiB to handle very long output without truncation.
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = fmt.Fprintln(w, line)
			if canFlush {
				flusher.Flush()
			}
		}
		// scanner.Err() is checked implicitly — if the pipe closed cleanly it
		// returns nil; errors are logged below after cmd.Wait().

		waitErr := cmd.Wait()
		if stderrBuf.Len() > 0 {
			s.logger().Printf("sidecar: host-API /review: stderr: %s", strings.TrimSpace(stderrBuf.String()))
			// Forward stderr content to the client so that the worker agent
			// can see the full error message (e.g. the preflight rebase gate
			// failure with git commands). Each stderr line is written as an
			// ordinary output line before the sentinel so that the sentinel
			// remains the final line of the response as proxyReviewAsync expects.
			for _, line := range strings.Split(strings.TrimRight(stderrBuf.String(), "\n"), "\n") {
				_, _ = fmt.Fprintln(w, line)
			}
			if canFlush {
				flusher.Flush()
			}
		}

		// Write pass/fail sentinel. The client (proxyReviewAsync) consumes
		// this line and does not print it to its own stdout.
		if waitErr != nil {
			s.logger().Printf("sidecar: host-API /review: review process failed: %v", waitErr)
			_, _ = fmt.Fprintln(w, ReviewSentinelFailed)
		} else {
			_, _ = fmt.Fprintln(w, ReviewSentinelPassed)
		}
		if canFlush {
			flusher.Flush()
		}
	})

	// POST /cleanup
	// Request:  {"session":"nixos-config@my-feature","yes":true,"json":false}
	// Response: {"stdout":"...","stderr":"..."} | {"error":"...","stdout":"...","stderr":"..."}
	//
	// stdout/stderr from the spawned `prism cleanup` subprocess are captured
	// separately and forwarded to the caller. The container-side proxy
	// (proxyCleanupToHostAPI) writes them verbatim to its own stdout/stderr so
	// that an agent running inside a coordinator container sees the same
	// per-resource progress lines a host invocation would print. Without this
	// forwarding, the container path was silent on success — issue #1527.
	mux.HandleFunc("/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if !requireCoordinator(w, "cleanup") {
			return
		}
		var req struct {
			Session string `json:"session"`
			Yes     bool   `json:"yes"`
			JSON    bool   `json:"json"`
		}
		// /cleanup body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Own-repo restriction: coordinators may only clean up sessions in their own repo.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}
		targetRepo, targetRepoErr := repoFromSession(req.Session)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		if targetRepo != ownRepo {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("coordinators can only clean up sessions in their own repo (%s), got %q", ownRepo, req.Session))
			return
		}

		args := []string{"cleanup", "--session", req.Session}
		if req.Yes {
			args = append(args, "--yes")
		}
		if req.JSON {
			args = append(args, "--json")
		}

		s.logger().Printf("sidecar: host-API /cleanup: prism %s", strings.Join(args, " "))

		// Per-endpoint timeout: 5 min. `prism cleanup` removes git worktrees,
		// archives session state, and may run `git` operations that touch the
		// network on detached worktrees. 5 min is a comfortable upper bound;
		// anything longer is a hang. On timeout returns 504 Gateway Timeout;
		// on client disconnect returns 499.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		err := cmd.Run()
		stdoutStr := stdoutBuf.String()
		stderrStr := stderrBuf.String()
		if err != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /cleanup: context fired (%v): killed child after %v", ctx.Err(), 5*time.Minute)
				writeJSON(w, status, map[string]any{
					"error":  fmt.Sprintf("cleanup aborted: %v", ctx.Err()),
					"stdout": stdoutStr,
					"stderr": stderrStr,
				})
				return
			}
			s.logger().Printf("sidecar: host-API /cleanup: %v: stdout=%q stderr=%q", err, stdoutStr, stderrStr)
			// Forward stdout and stderr alongside the error so the caller can
			// surface the underlying cause (e.g. archive collision) instead of
			// just the outer transport's exit shape.
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":  fmt.Sprintf("cleanup failed: %v", err),
				"stdout": stdoutStr,
				"stderr": stderrStr,
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"stdout": stdoutStr,
			"stderr": stderrStr,
		})
	})

	// POST /switch
	// Request:  {"session":"nixos-config@my-feature"}
	// Response: {} | {"error":"..."}
	mux.HandleFunc("/switch", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Session string `json:"session"`
		}
		// /switch body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Resolve worktree path for the session from the DB, then use
		// prism switch --path <worktree> to switch the tmux client.
		worktreePath, err := s.worktreePathForSession(req.Session)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("resolve worktree: %v", err))
			return
		}

		args := []string{"switch", "--path", worktreePath}
		s.logger().Printf("sidecar: host-API /switch: prism %s", strings.Join(args, " "))

		// Per-endpoint timeout: 30 s. `prism switch` issues a single tmux
		// switch-client command and exits — it should complete in well under
		// a second. 30 s is the documented outer bound (issue #1847). On
		// timeout returns 504 Gateway Timeout; on client disconnect returns 499.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		out, switchErr := cmd.CombinedOutput()
		if switchErr != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /switch: context fired (%v): killed child after %v", ctx.Err(), 30*time.Second)
				writeError(w, status, fmt.Sprintf("switch aborted: %v", ctx.Err()))
				return
			}
			s.logger().Printf("sidecar: host-API /switch: %v: %s", switchErr, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("switch failed: %v", switchErr))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{})
	})

	// POST /merge
	// Request:  {"pr": <int>, "title": <string|null>}
	// Response: PendingMerge JSON | {"error":"..."}
	//
	// Enqueues a PR into the merge queue using the sidecar's own session_name
	// and instance_id — the values the merge-queue watcher queries against.
	// This is the proxy path for `prism merge <pr>` invoked from inside a
	// bwrap sandbox where dbPath() resolves to a shadow tmpfs (#1043).
	//
	// Coordinator-only: the merge queue is owned by coordinator sessions.
	mux.HandleFunc("/merge", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if !requireCoordinator(w, "merge") {
			return
		}
		var req struct {
			PR    int     `json:"pr"`
			Title *string `json:"title"`
		}
		// /merge body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.PR <= 0 {
			writeError(w, http.StatusBadRequest, "pr is required and must be a positive integer")
			return
		}
		if s.cfg.InstanceID == "" {
			s.logger().Printf("sidecar: host-API /merge: no instance_id — sidecar startup did not run correctly")
			writeError(w, http.StatusInternalServerError,
				"sidecar has no instance_id — sidecar startup did not run")
			return
		}

		// Use the sidecar's own session_name and instance_id so that the row
		// is keyed on exactly the values the merge-queue watcher queries
		// against. This is the architectural reason for routing through the
		// sidecar at all (#1043).
		row, err := s.cfg.DB.EnqueueMerge(req.PR, s.cfg.SessionName, s.cfg.InstanceID, req.Title)
		if err != nil {
			s.logger().Printf("sidecar: host-API /merge: EnqueueMerge: %v", err)
			writeError(w, http.StatusInternalServerError, "enqueue merge: "+err.Error())
			return
		}
		s.logger().Printf("sidecar: host-API /merge: PR #%d enqueued (queue_position=%d, status=%s)",
			row.PR, row.QueuePosition, row.Status)
		writeJSON(w, http.StatusOK, row)
	})

	// GET /merges
	// Query params: filter=watching|failed|abandoned|all (default: watching)
	// Response: JSON array of PendingMerge | {"error":"..."}
	//
	// Lists merge queue entries scoped to the sidecar's own instance_id and
	// session_name — the same filters used by the host-side `prism merges`
	// command. This is the proxy path for `prism merges` invoked from inside
	// a bwrap sandbox (#1043).
	mux.HandleFunc("/merges", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "merges") {
			return
		}
		if s.cfg.InstanceID == "" {
			s.logger().Printf("sidecar: host-API /merges: no instance_id — sidecar startup did not run correctly")
			writeError(w, http.StatusInternalServerError,
				"sidecar has no instance_id — sidecar startup did not run")
			return
		}

		filter := r.URL.Query().Get("filter")
		merges, err := s.cfg.DB.MergeQueueForInstance(s.cfg.InstanceID, s.cfg.SessionName, filter)
		if err != nil {
			s.logger().Printf("sidecar: host-API /merges: MergeQueueForInstance: %v", err)
			writeError(w, http.StatusInternalServerError, "list merges: "+err.Error())
			return
		}
		// Return empty array rather than null when no rows.
		if merges == nil {
			merges = []db.PendingMerge{}
		}
		writeJSON(w, http.StatusOK, merges)
	})

	// POST /merges/cancel
	// Request:  {"pr": <int>}
	// Response: {"cancelled": <bool>, "row": <PendingMerge|null>} | {"error":"..."}
	//
	// Cancels a watching row owned by the sidecar's instance_id. The response
	// includes the current row (when present) so the client can render a
	// helpful message when cancellation is a no-op (already terminal, owned
	// by a different incarnation, etc.).
	mux.HandleFunc("/merges/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if !requireCoordinator(w, "merges cancel") {
			return
		}
		var req struct {
			PR int `json:"pr"`
		}
		// /merges/cancel body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.PR <= 0 {
			writeError(w, http.StatusBadRequest, "pr is required and must be a positive integer")
			return
		}
		if s.cfg.InstanceID == "" {
			s.logger().Printf("sidecar: host-API /merges/cancel: no instance_id — sidecar startup did not run correctly")
			writeError(w, http.StatusInternalServerError,
				"sidecar has no instance_id — sidecar startup did not run")
			return
		}

		cancelled, err := s.cfg.DB.CancelMerge(req.PR, s.cfg.InstanceID)
		if err != nil {
			s.logger().Printf("sidecar: host-API /merges/cancel: CancelMerge: %v", err)
			writeError(w, http.StatusInternalServerError, "cancel merge: "+err.Error())
			return
		}
		// Always look up the current row so the client can render a helpful
		// message when cancellation is a no-op. PendingMergeByPR returning nil
		// means the row does not exist at all.
		row, lookupErr := s.cfg.DB.PendingMergeByPR(req.PR)
		if lookupErr != nil {
			s.logger().Printf("sidecar: host-API /merges/cancel: PendingMergeByPR: %v", lookupErr)
			// Lookup error is non-fatal — return cancelled status without the row.
			row = nil
		}
		s.logger().Printf("sidecar: host-API /merges/cancel: PR #%d cancelled=%v", req.PR, cancelled)
		writeJSON(w, http.StatusOK, map[string]any{
			"cancelled": cancelled,
			"row":       row,
		})
	})

	// POST /event
	// Request:  {"kind":"<kind>","session":"<session>","args":{...kind-specific...}}
	// Response: {} | {"error":"..."}
	//
	// Writes a lifecycle event to the host DB by running `prism event <kind>`
	// with the supplied args on the host. This is the proxy path for every
	// `prism event <kind>` subcommand invoked from inside a container where
	// dbPath() resolves to a per-container shadow DB invisible to the host (#1254).
	//
	// Allowed kinds: state-change, pane-died, tmux-session-start, tmux-session-end,
	// compaction, error, doom-loop-detected.
	//
	// Validation:
	//   - kind must be a known subcommand name (400 for unknown kinds).
	//   - session must be non-empty (400 for empty or whitespace).
	//     NOTE: we intentionally do NOT require the session to already exist in
	//     the host DB. tmux-session-start is called for brand-new sessions that
	//     have no agent_status row yet; a DB-existence check would reject the
	//     very first event that creates the session record. #1254 originally
	//     required "matches a known session", but that AC item was relaxed to
	//     non-empty-only to allow tmux-session-start to work correctly.
	//   - All role levels (worker and coordinator) are permitted — lifecycle
	//     events are emitted by both.
	//
	// Security: accessible only over the Unix socket (consistent with all other
	// host-API endpoints — no network exposure).
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var req struct {
			Kind    string            `json:"kind"`
			Session string            `json:"session"`
			Args    map[string]string `json:"args"`
		}
		// /event body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}

		// Validate kind against the known allowlist.
		if !isKnownEventKind(req.Kind) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"unknown event kind %q — must be one of: %s",
				req.Kind, knownEventKindsStr()))
			return
		}

		// Validate session: must be non-empty.
		if strings.TrimSpace(req.Session) == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Build the prism event <kind> [flags] invocation.
		// Always include --session from the top-level field; remaining args
		// come from the Args map as --key value pairs. Empty-value entries
		// are skipped so that optional flags remain at their defaults on the
		// host side (e.g. --agent-role "" behaves the same as omitting it).
		args := []string{"event", req.Kind, "--session", req.Session}
		for k, v := range req.Args {
			if v != "" {
				args = append(args, "--"+k, v)
			}
		}

		s.logger().Printf("sidecar: host-API /event: kind=%s session=%s", req.Kind, req.Session)

		// Per-endpoint timeout: 30 s. `prism event` writes one row to the host
		// DB and exits — it should complete in well under a second. 30 s is
		// the documented outer bound (issue #1847). On timeout returns 504
		// Gateway Timeout; on client disconnect returns 499.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		// Propagate PRISM_SESSION_NAME so that any internal lookup resolves correctly.
		cmd.Env = append(os.Environ(), "PRISM_SESSION_NAME="+req.Session)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /event: context fired (%v): killed child after %v", ctx.Err(), 30*time.Second)
				writeError(w, status, fmt.Sprintf("event write aborted: %v", ctx.Err()))
				return
			}
			s.logger().Printf("sidecar: host-API /event: %v: %s", err, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("event write failed: %v\n%s", err, strings.TrimSpace(string(out))))
			return
		}

		s.logger().Printf("sidecar: host-API /event: kind=%s session=%s written to host DB", req.Kind, req.Session)
		writeJSON(w, http.StatusOK, map[string]string{})
	})

	// POST /feedback
	// Request:  feedback.Entry JSON (same struct used by cmd/feedback.go and
	//           internal/feedback/feedback.go).
	// Response: {"path":"<absolute path to feedback.jsonl>"} | {"error":"..."}
	//
	// Appends one feedback entry to the host's feedback.jsonl file. This is the
	// proxy path for `prism feedback` invoked from inside a bwrap worker sandbox
	// where the sandbox namespace is ephemeral — writes inside the sandbox never
	// reach the host filesystem (issue #1644). The sidecar runs on the host, so
	// its Append call lands in the real ~/.local/state/prism/feedback.jsonl.
	//
	// All roles (worker and coordinator) are permitted — feedback is intentionally
	// low-privilege: any sandboxed child that can reach the host-API socket may
	// record a note.
	//
	// The response includes the resolved path so the CLI can print it in the
	// success message — the worker sees the host path, which is the same path
	// `prism feedback list` will read from on the host (AC: "message prints the
	// path the entry actually landed at").
	mux.HandleFunc("/feedback", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var entry feedback.Entry
		// /feedback body cap: default 1 MiB (issue #1848). DisallowUnknownFields
		// is enabled because feedback.Entry is a closed schema co-owned by this
		// repo (cmd/feedback.go and internal/feedback) — no external producers.
		if status, err := decodeRequestJSON(w, r, &entry, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(entry.Text) == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}

		// Resolve the host-side feedback store path. DefaultPath honours
		// $XDG_STATE_HOME, which the test suite sets to a t.TempDir() so that
		// the homeless-shelter gate (HOME=/homeless-shelter in nix sandbox) is
		// avoided. The production path is the user's real state dir.
		path, err := feedback.DefaultPath()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "resolve feedback path: "+err.Error())
			return
		}
		store := feedback.NewStore(path)
		if err := store.Append(entry); err != nil {
			s.logger().Printf("sidecar: host-API /feedback: append: %v", err)
			writeError(w, http.StatusInternalServerError, "append feedback: "+err.Error())
			return
		}

		s.logger().Printf("sidecar: host-API /feedback: entry appended to %s", path)
		writeJSON(w, http.StatusOK, map[string]string{"path": path})
	})

	// POST /escalate
	// Request:  {"prompt":"...","to":"<session>" (optional),"from":"<session>" (optional)}
	// Response: {} on success, {"error":"..."} otherwise.
	// Wait-probe endpoints (issue #1500) — read-only DB lookups used by the
	// `--wait` flag on prism merge / review / spawn when the CLI runs inside a
	// sandbox. The CLI cannot poll the host's prism.db directly (the sandbox
	// has its own shadow DB), so it polls these endpoints instead. Each is a
	// single-row / single-shape lookup; rendering and aggregation stay on the
	// CLI side.

	// GET /merges/by-pr?pr=N
	// Response: 200 with PendingMerge JSON when the row exists,
	//           404 {"error":"not found"} when no row exists for that PR.
	mux.HandleFunc("/merges/by-pr", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		prStr := r.URL.Query().Get("pr")
		if prStr == "" {
			writeError(w, http.StatusBadRequest, "pr is required")
			return
		}
		prNum, parseErr := strconv.Atoi(prStr)
		if parseErr != nil || prNum <= 0 {
			writeError(w, http.StatusBadRequest, "pr must be a positive integer")
			return
		}
		row, lookupErr := s.cfg.DB.PendingMergeByPR(prNum)
		if lookupErr != nil {
			writeError(w, http.StatusInternalServerError, "lookup merge: "+lookupErr.Error())
			return
		}
		if row == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, row)
	})

	// GET /sessions/status?session=NAME
	// Response: 200 with db.Status JSON when the row exists,
	//           404 {"error":"not found"} when no row exists.
	mux.HandleFunc("/sessions/status", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		sessionName := r.URL.Query().Get("session")
		if sessionName == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		st, lookupErr := s.cfg.DB.CurrentStatus(sessionName)
		if lookupErr != nil {
			writeError(w, http.StatusInternalServerError, "lookup status: "+lookupErr.Error())
			return
		}
		if st == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	// GET /groups/list?limit=N
	// Response: 200 with []db.ReviewGroupSummary (newest first).
	//
	// Backs `prism reviews list` (issue #1500) when invoked from inside a
	// sandbox — the in-sandbox prism.db is a tmpfs shadow with no review
	// groups, so a direct read returns an empty list. Routing through this
	// endpoint lets the in-sandbox CLI see the host's session_groups
	// table. Same #1043 pattern as /merges.
	mux.HandleFunc("/groups/list", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		limit := 0
		if l := r.URL.Query().Get("limit"); l != "" {
			n, parseErr := strconv.Atoi(l)
			if parseErr != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			limit = n
		}
		groups, gErr := s.cfg.DB.ReviewGroupsList(limit)
		if gErr != nil {
			writeError(w, http.StatusInternalServerError, "list groups: "+gErr.Error())
			return
		}
		if groups == nil {
			groups = []db.ReviewGroupSummary{}
		}
		writeJSON(w, http.StatusOK, groups)
	})

	// GET /groups/poll?group_id=UUID
	// Response: 200 with {"completed": bool, "members": [Status...],
	//          "results": map[session]GroupMemberResult}.
	// The CLI uses `completed` to decide when to stop polling, and the
	// other fields to aggregate the per-agent verdicts (so a single
	// terminal poll fetches everything needed).
	mux.HandleFunc("/groups/poll", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		groupID := r.URL.Query().Get("group_id")
		if groupID == "" {
			writeError(w, http.StatusBadRequest, "group_id is required")
			return
		}
		completed, gErr := s.cfg.DB.GroupCompleted(groupID)
		if gErr != nil {
			writeError(w, http.StatusInternalServerError, "group completed: "+gErr.Error())
			return
		}
		members, mErr := s.cfg.DB.GroupMembersForGroup(groupID)
		if mErr != nil {
			writeError(w, http.StatusInternalServerError, "group members: "+mErr.Error())
			return
		}
		results, rErr := s.cfg.DB.GroupResults(groupID)
		if rErr != nil {
			writeError(w, http.StatusInternalServerError, "group results: "+rErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"completed": completed,
			"members":   members,
			"results":   results,
		})
	})

	//
	// Forwards an escalation request from a worker container to the host's
	// `prism escalate` CLI. The from field defaults to the calling sidecar's
	// session name when omitted (the common case from a containerised worker).
	//
	// Cross-session integrity: when `from` is set explicitly it must equal the
	// calling sidecar's own session name (the per-session host-API socket is
	// the auth boundary), unless the caller is a coordinator. This mirrors
	// the rule applied by /prompt and /set-model in this same file. Without
	// the check, a non-coordinator could mutate `agent_status.state`,
	// emit a `session.escalated` bus event attributed to a victim, and pin
	// that victim in `escalated` so its legitimate finish notifications are
	// suppressed.
	mux.HandleFunc("/escalate", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
			To     string `json:"to,omitempty"`
			From   string `json:"from,omitempty"`
		}
		// /escalate body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		fromSession := req.From
		if fromSession == "" {
			fromSession = s.cfg.SessionName
		} else if fromSession != s.cfg.SessionName {
			// Cross-session: only coordinators may escalate on behalf of
			// another session. The DB-backed coordinator check matches the
			// pattern used by /prompt's cross-repo branch and /set-model.
			if !isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()) {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("workers can only escalate from their own session (%s), got from=%q",
						s.cfg.SessionName, fromSession))
				return
			}
		}

		args := []string{"escalate", "--prompt", req.Prompt}
		if req.To != "" {
			args = append(args, "--to", req.To)
		}
		s.logger().Printf("sidecar: host-API /escalate: from=%s to=%s", fromSession, req.To)

		// Per-endpoint timeout: 30 s. `prism escalate` writes one bus event
		// and may best-effort prompt the coordinator; it should return well
		// inside a second. 30 s is the documented outer bound (issue #1847).
		// On timeout returns 504 Gateway Timeout; on client disconnect returns 499.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		// PRISM_SESSION_NAME tells the host-side `prism escalate` which session
		// is calling, bypassing the CWD walk that would otherwise resolve to
		// the sidecar's own working directory.
		cmd.Env = append(os.Environ(), "PRISM_SESSION_NAME="+fromSession, "PRISM_HOST_API=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /escalate: context fired (%v): killed child after %v", ctx.Err(), 30*time.Second)
				writeError(w, status, fmt.Sprintf("escalate aborted: %v", ctx.Err()))
				return
			}
			s.logger().Printf("sidecar: host-API /escalate: %v: %s", err, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("escalate failed: %v\n%s", err, strings.TrimSpace(string(out))))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{})
	})

	// POST /investigate
	// Request:  {"prompt":"...","from":"<session>" (optional)}
	// Response: {"session_name":"<name>"} on success, {"error":"..."} otherwise.
	//
	// Spawns a new investigate-agent session named <invoker>~investigate-<slug>
	// and returns the session name immediately. Shells out to `prism investigate`
	// on the host with PRISM_SESSION_NAME set to the invoker session so the
	// host-side CWD walk is bypassed.
	mux.HandleFunc("/investigate", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Prompt string `json:"prompt"`
			From   string `json:"from,omitempty"`
			Name   string `json:"name,omitempty"`
		}
		// /investigate body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name != "" {
			if err := investigatepkg.ValidateName(req.Name); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		fromSession := req.From
		if fromSession == "" {
			fromSession = s.cfg.SessionName
		}
		args := []string{"investigate", "--prompt", req.Prompt}
		if req.Name != "" {
			args = append(args, "--name", req.Name)
		}
		s.logger().Printf("sidecar: host-API /investigate: from=%s", fromSession)

		// Per-endpoint timeout: 10 min. `prism investigate` spawns a new
		// session — same shape as /spawn, so the bound matches. On timeout
		// returns 504 Gateway Timeout; on client disconnect returns 499.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)
		// PRISM_SESSION_NAME tells the host-side `prism investigate` which session
		// is invoking, bypassing the CWD walk.
		cmd.Env = append(os.Environ(), "PRISM_SESSION_NAME="+fromSession, "PRISM_HOST_API=")
		out, err := cmd.Output()
		if err != nil {
			if status, ok := contextErrStatus(ctx); ok {
				s.logger().Printf("sidecar: host-API /investigate: context fired (%v): killed child after %v", ctx.Err(), 10*time.Minute)
				writeError(w, status, fmt.Sprintf("investigate aborted: %v", ctx.Err()))
				return
			}
			s.logger().Printf("sidecar: host-API /investigate: %v", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("investigate failed: %v", err))
			return
		}
		sessionName := strings.TrimSpace(string(out))
		writeJSON(w, http.StatusOK, map[string]string{"session_name": sessionName})
	})

	// POST /set-model
	// Request:  {"session":"<name>","provider":"...","model":"...","thinking":"..."}
	// Response: {"session":"<name>","status":"applied"|"error:disconnected"} | {"error":"..."}
	//
	// Swaps the model on a single live PI session.  The calling sidecar must
	// own that session (same session name) OR be a coordinator in the same
	// repo.  Workers may only target their own session.
	//
	// Frame is enqueued best-effort.  The response is returned immediately; no
	// ACK is awaited from the extension.
	mux.HandleFunc("/set-model", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var req struct {
			Session  string `json:"session"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Thinking string `json:"thinking"`
		}
		// /set-model body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		if req.Model == "" {
			writeError(w, http.StatusBadRequest, "model is required")
			return
		}

		// Role-scope check: workers may only target their own session.
		// We check both the configured AgentRole and the DB-backed
		// isCoordinatorSession so that the guard fires even when the session
		// name ends in @main but the sidecar is running as a worker.
		//
		// When a coordinator fans out to another session via forwardSetModel,
		// the target sidecar's /set-model receives req.Session == s.cfg.SessionName
		// (the target's own session name), so the guard passes correctly.
		// There is no sidecar-to-sidecar authentication beyond the per-session
		// Unix socket filesystem permissions (same as /register-provider-direct).
		callerIsCoordinator := s.cfg.AgentRole == "coordinator" || (s.cfg.AgentRole == "" && isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()))
		if !callerIsCoordinator && req.Session != s.cfg.SessionName {
			writeError(w, http.StatusForbidden, "workers can only call /set-model for their own session")
			return
		}

		status := liveModelSwapForSession(s, req.Session, req.Provider, req.Model, req.Thinking)
		// When the outbound channel is full, return 503 so the caller knows the
		// frame was dropped rather than silently accepting 200 OK. Issue #1844.
		httpStatus := http.StatusOK
		if status == "error:queue-full" {
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(w, httpStatus, map[string]string{
			"session": req.Session,
			"status":  status,
		})
	})

	// POST /apply-profile
	// Request:  {"profile":"<name>","scope":"session|coordinator|global","session":"<name-if-session-scope>"}
	// Response: {"results":[{"session":"...","status":"..."},...]} | {"error":"..."}
	//
	// Resolves which sessions match scope, computes the per-role slot from the
	// named profile, and delivers a set_model frame to each matching live PI
	// session.
	//
	// Scope rules:
	//   session    — target the named session only; available to workers (own
	//                session) or coordinators.
	//   coordinator — all live PI sessions in the calling coordinator's repo;
	//                coordinator only.
	//   global     — every live PI session across all repos; coordinator only.
	mux.HandleFunc("/apply-profile", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var req struct {
			Profile string `json:"profile"`
			Scope   string `json:"scope"`
			Session string `json:"session"` // only for scope=session
		}
		// /apply-profile body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Profile == "" {
			writeError(w, http.StatusBadRequest, "profile is required")
			return
		}
		switch req.Scope {
		case "session", "coordinator", "global":
		default:
			writeError(w, http.StatusBadRequest, `scope must be "session", "coordinator", or "global"`)
			return
		}

		// Permission: global and coordinator scopes require coordinator role.
		applyCallerIsCoordinator := s.cfg.AgentRole == "coordinator" || (s.cfg.AgentRole == "" && isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()))
		if req.Scope == "global" || req.Scope == "coordinator" {
			if !applyCallerIsCoordinator {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("workers cannot perform apply-profile with scope=%s", req.Scope))
				return
			}
		}
		// Session scope: workers may only target their own session.
		if req.Scope == "session" {
			if req.Session == "" {
				writeError(w, http.StatusBadRequest, "session is required when scope=session")
				return
			}
			if !applyCallerIsCoordinator && req.Session != s.cfg.SessionName {
				writeError(w, http.StatusForbidden, "workers can only apply-profile to their own session")
				return
			}
		}

		// Load profiles file to resolve slots.
		pf, pfErr := hostAPILoadProfiles()
		if pfErr != nil {
			writeError(w, http.StatusInternalServerError, "load profiles: "+pfErr.Error())
			return
		}
		if _, ok := pf.Profiles[req.Profile]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown profile %q", req.Profile))
			return
		}

		// Resolve target sessions.
		var targets []string
		switch req.Scope {
		case "session":
			targets = []string{req.Session}
		case "coordinator":
			ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
			if repoErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot derive repo: "+repoErr.Error())
				return
			}
			statuses, dbErr := s.cfg.DB.ActivePISessionsForRepo(ownRepo)
			if dbErr != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+dbErr.Error())
				return
			}
			for _, st := range statuses {
				targets = append(targets, st.SessionName)
			}
		case "global":
			statuses, dbErr := s.cfg.DB.AllActivePISessions()
			if dbErr != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+dbErr.Error())
				return
			}
			for _, st := range statuses {
				targets = append(targets, st.SessionName)
			}
		}

		// Fan out set_model frames.
		type sessionResult struct {
			Session string `json:"session"`
			Status  string `json:"status"`
		}
		results := make([]sessionResult, 0, len(targets))
		applyAnyQueueFull := false
		for _, targetSess := range targets {
			// Look up role to find the correct slot.
			role, status := resolveRoleForSession(s, targetSess)
			if status != "" {
				// Error or skip condition.
				results = append(results, sessionResult{Session: targetSess, Status: status})
				continue
			}

			// Look up slot for role.
			slot, ok := pf.Profiles[req.Profile][role]
			if !ok {
				s.logger().Printf("sidecar: host-API /apply-profile: profile %q has no slot for role %q (session %s) — skipping",
					req.Profile, role, targetSess)
				results = append(results, sessionResult{Session: targetSess, Status: "skipped:no-matching-slot"})
				continue
			}

			deliveryStatus := liveModelSwapForSession(s, targetSess, slot.Provider, slot.Model, slot.Thinking)
			results = append(results, sessionResult{Session: targetSess, Status: deliveryStatus})
			if deliveryStatus == "error:queue-full" {
				applyAnyQueueFull = true
			}
		}

		// When any target's outbound channel was full, return 503 so the caller
		// knows at least one set_model frame was dropped. Issue #1844.
		applyHTTPStatus := http.StatusOK
		if applyAnyQueueFull {
			applyHTTPStatus = http.StatusServiceUnavailable
		}
		writeJSON(w, applyHTTPStatus, map[string]any{"results": results})
	})

	// POST /register-provider
	// Request:  {"name":"...","config":{...},"scope":"session|coordinator|global","session":"<name-if-session-scope>"}
	// Response: {"results":[{"session":"...","status":"..."},...]} | {"error":"..."}
	//
	// Pushes a register_provider frame to live PI sessions in scope.
	// Scope rules mirror /apply-profile.
	mux.HandleFunc("/register-provider", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var req struct {
			Name    string         `json:"name"`
			Config  map[string]any `json:"config"`
			Scope   string         `json:"scope"`
			Session string         `json:"session"` // only for scope=session
		}
		// /register-provider body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		switch req.Scope {
		case "session", "coordinator", "global":
		default:
			writeError(w, http.StatusBadRequest, `scope must be "session", "coordinator", or "global"`)
			return
		}

		// Permission: global and coordinator scopes require coordinator role.
		regCallerIsCoordinator := s.cfg.AgentRole == "coordinator" || (s.cfg.AgentRole == "" && isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()))
		if req.Scope == "global" || req.Scope == "coordinator" {
			if !regCallerIsCoordinator {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("workers cannot perform register-provider with scope=%s", req.Scope))
				return
			}
		}
		if req.Scope == "session" {
			if req.Session == "" {
				writeError(w, http.StatusBadRequest, "session is required when scope=session")
				return
			}
			if !regCallerIsCoordinator && req.Session != s.cfg.SessionName {
				writeError(w, http.StatusForbidden, "workers can only register-provider for their own session")
				return
			}
		}

		// Resolve target sessions.
		var targets []string
		switch req.Scope {
		case "session":
			targets = []string{req.Session}
		case "coordinator":
			ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
			if repoErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot derive repo: "+repoErr.Error())
				return
			}
			statuses, dbErr := s.cfg.DB.ActivePISessionsForRepo(ownRepo)
			if dbErr != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+dbErr.Error())
				return
			}
			for _, st := range statuses {
				targets = append(targets, st.SessionName)
			}
		case "global":
			statuses, dbErr := s.cfg.DB.AllActivePISessions()
			if dbErr != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+dbErr.Error())
				return
			}
			for _, st := range statuses {
				targets = append(targets, st.SessionName)
			}
		}

		type sessionResult struct {
			Session string `json:"session"`
			Status  string `json:"status"`
		}
		results := make([]sessionResult, 0, len(targets))
		anyQueueFull := false
		for _, targetSess := range targets {
			deliveryStatus := liveRegisterProviderForSession(s, targetSess, req.Name, req.Config)
			results = append(results, sessionResult{Session: targetSess, Status: deliveryStatus})
			if deliveryStatus == "error:queue-full" {
				anyQueueFull = true
			}
		}

		// When any target's outbound channel was full, return 503 so the caller
		// knows at least one frame was dropped. Issue #1844.
		regHTTPStatus := http.StatusOK
		if anyQueueFull {
			regHTTPStatus = http.StatusServiceUnavailable
		}
		writeJSON(w, regHTTPStatus, map[string]any{"results": results})
	})

	// POST /register-provider-direct
	// Request:  {"session":"<name>","name":"...","config":{...}}
	// Response: {"session":"<name>","status":"..."} | {"error":"..."}
	//
	// Internal endpoint called by forwardRegisterProvider when fanning out to
	// a different sidecar process.  No role check: reachable only via the
	// per-session Unix socket, which is protected by filesystem permissions.
	mux.HandleFunc("/register-provider-direct", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Session string         `json:"session"`
			Name    string         `json:"name"`
			Config  map[string]any `json:"config"`
		}
		// /register-provider-direct body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if !isPipeConnected(s) {
			writeJSON(w, http.StatusOK, map[string]string{"session": req.Session, "status": "error:disconnected"})
			return
		}
		// Propagate channel-full failure as 503 so the forwarding sidecar
		// knows the frame was dropped. Issue #1844.
		if !s.RegisterProvider(req.Name, req.Config) {
			s.logger().Printf("sidecar: /register-provider-direct: enqueue failed (queue full) for session %s", req.Session)
			writeError(w, http.StatusServiceUnavailable, "outbound queue full")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session": req.Session, "status": "applied"})
	})

	// POST /set-active-tools
	// Request:  {"session":"<name>","tools":["tool1","tool2",...]}
	// Response: {"session":"<name>","status":"applied"|"error:disconnected"} | 503
	//
	// Enqueues a set_active_tools frame to the PI extension for the session's
	// own harness pipe. Only targets the calling session (workers) or a named
	// session (coordinator). Returns 503 Service Unavailable when the outbound
	// channel is full so the caller knows the frame was dropped. Issue #1844.
	mux.HandleFunc("/set-active-tools", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Session string   `json:"session"`
			Tools   []string `json:"tools"`
		}
		// /set-active-tools body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		// Workers may only target their own session.
		satCallerIsCoordinator := s.cfg.AgentRole == "coordinator" || (s.cfg.AgentRole == "" && isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()))
		if !satCallerIsCoordinator && req.Session != s.cfg.SessionName {
			writeError(w, http.StatusForbidden, "workers can only call /set-active-tools for their own session")
			return
		}
		if !isPipeConnected(s) {
			writeJSON(w, http.StatusOK, map[string]string{"session": req.Session, "status": "error:disconnected"})
			return
		}
		// Return 503 when the outbound channel is full so the caller knows the
		// frame was dropped rather than silently accepting 200 OK. Issue #1844.
		if !s.SetActiveTools(req.Tools) {
			s.logger().Printf("sidecar: /set-active-tools: enqueue failed (queue full) for session %s", req.Session)
			writeError(w, http.StatusServiceUnavailable, "outbound queue full")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session": req.Session, "status": "applied"})
	})

	// POST /abort
	// Request:  {"session":"<name>"}
	// Response: {"session":"<name>","status":"applied"|"error:disconnected"} | 503
	//
	// Enqueues an abort frame to the PI extension for the session's own harness
	// pipe. Only targets the calling session (workers) or a named session
	// (coordinator). Returns 503 Service Unavailable when the outbound channel
	// is full so the caller knows the frame was dropped. Issue #1844.
	mux.HandleFunc("/abort", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Session string `json:"session"`
		}
		// /abort body cap: default 1 MiB (issue #1848).
		if status, err := decodeRequestJSON(w, r, &req, defaultMaxBodyBytes, false); err != nil {
			writeError(w, status, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		// Workers may only target their own session.
		abortCallerIsCoordinator := s.cfg.AgentRole == "coordinator" || (s.cfg.AgentRole == "" && isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()))
		if !abortCallerIsCoordinator && req.Session != s.cfg.SessionName {
			writeError(w, http.StatusForbidden, "workers can only call /abort for their own session")
			return
		}
		if !isPipeConnected(s) {
			writeJSON(w, http.StatusOK, map[string]string{"session": req.Session, "status": "error:disconnected"})
			return
		}
		// Return 503 when the outbound channel is full so the caller knows the
		// frame was dropped rather than silently accepting 200 OK. Issue #1844.
		if !s.Abort() {
			s.logger().Printf("sidecar: /abort: enqueue failed (queue full) for session %s", req.Session)
			writeError(w, http.StatusServiceUnavailable, "outbound queue full")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session": req.Session, "status": "applied"})
	})

	// GET /db/query, /db/schema, /db/tables — read-only query surface (#1467).
	// All three open a fresh read-only handle (?mode=ro) per request rather
	// than sharing the sidecar's writable handle. Handlers live in
	// host_api_db.go so the wiring stays minimal here.
	//
	// Coordinator-only: /db/query exposes a strict superset of /checkin
	// (raw cross-session payloads vs. rendered single-session view), so it
	// is gated at the same level as /checkin. /db/schema and /db/tables are
	// also coordinator-only for consistency — a worker that cannot read
	// payloads has no need to enumerate the schema either.
	mux.HandleFunc("/db/query", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "db query") {
			return
		}
		s.hostAPIDBQuery(w, r)
	})
	mux.HandleFunc("/db/schema", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "db schema") {
			return
		}
		s.hostAPIDBSchema(w, r)
	})
	mux.HandleFunc("/db/tables", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "db tables") {
			return
		}
		s.hostAPIDBTables(w, r)
	})

	return mux
}

// eventKindAllowlist is the set of valid kind values accepted by the /event
// host-API endpoint. These match the subcommand names in cmd/event.go.
var eventKindAllowlist = map[string]bool{
	"state-change":       true,
	"pane-died":          true,
	"tmux-session-start": true,
	"tmux-session-end":   true,
	"compaction":         true,
	"error":              true,
	"doom-loop-detected": true,
}

// isKnownEventKind returns true if kind is a recognised event subcommand name.
func isKnownEventKind(kind string) bool {
	return eventKindAllowlist[kind]
}

// knownEventKindsStr returns a sorted comma-separated list of known event kinds
// for use in error messages.
func knownEventKindsStr() string {
	names := make([]string, 0, len(eventKindAllowlist))
	for name := range eventKindAllowlist {
		names = append(names, name)
	}
	// Simple insertion sort for stable error messages.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	result := ""
	for i, n := range names {
		if i > 0 {
			result += ", "
		}
		result += n
	}
	return result
}

// worktreePathForSession looks up the worktree path for a session from the DB.
// Used by the /switch host-API handler to resolve the path for prism switch --path.
func (s *Sidecar) worktreePathForSession(sessionName string) (string, error) {
	status, err := s.cfg.DB.CurrentStatus(sessionName)
	if err != nil {
		return "", fmt.Errorf("db lookup: %w", err)
	}
	if status == nil {
		return "", fmt.Errorf("session %q not found in DB", sessionName)
	}
	if status.Worktree == "" {
		return "", fmt.Errorf("session %q has no worktree path in DB", sessionName)
	}
	return status.Worktree, nil
}

// ReviewSentinelPassed and ReviewSentinelFailed are the terminal lines written
// by the /review handler after the subprocess exits. They convey pass/fail
// status through the streaming response body so that proxyReviewAsync() can
// determine the exit status without needing a JSON wrapper.
//
// The client (cmd/hostapi.go proxyReviewAsync) consumes whichever sentinel
// arrives and does not echo it to the worker's stdout.
const (
	ReviewSentinelPassed = "__PRISM_REVIEW_PASSED__"
	ReviewSentinelFailed = "__PRISM_REVIEW_FAILED__"
)

// reviewAgentAllowlist is the set of valid review agent names accepted by the
// /review host-API endpoint. These match the names in review.Agents().
// New agents must be added here when introduced.
// Keeping this inline avoids importing the review package from the sidecar.
var reviewAgentAllowlist = map[string]bool{
	"review-goal":     true,
	"review-code":     true,
	"review-security": true,
	"review-qa":       true,
	"review-context":  true,
}

// isKnownReviewAgent returns true if name is a recognised review agent.
func isKnownReviewAgent(name string) bool {
	return reviewAgentAllowlist[name]
}

// knownReviewAgentNames returns the sorted list of known review agent names
// for use in error messages.
func knownReviewAgentNames() []string {
	names := make([]string, 0, len(reviewAgentAllowlist))
	for name := range reviewAgentAllowlist {
		names = append(names, name)
	}
	// Sort for stable error messages.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// ── P3.LIVE live-model-swap helpers (#1214) ───────────────────────────────────

// liveModelSwapForSession delivers a set_model frame to the named session.
//
// When targetSess == s.cfg.SessionName (own session) and the sidecar is
// running a PI harness, the frame is enqueued directly via s.SetModel.
// When targetSess is a different session, the call is forwarded to that
// session's own sidecar host-API socket via an HTTP POST to /set-model.
//
// Returns a status string: "applied" or "error:disconnected".
func liveModelSwapForSession(s *Sidecar, targetSess, provider, model, thinking string) string {
	if targetSess == s.cfg.SessionName {
		if !isPipeConnected(s) {
			return "error:disconnected"
		}
		// Propagate channel-full failure as error:queue-full so the HTTP caller
		// receives a non-200 response rather than a silent 200 OK. Issue #1844.
		if !s.SetModel(provider, model, thinking) {
			s.logger().Printf("sidecar: liveModelSwapForSession: enqueue failed (queue full) for %s", targetSess)
			return "error:queue-full"
		}
		return "applied"
	}

	if err := forwardSetModel(targetSess, provider, model, thinking); err != nil {
		s.logger().Printf("sidecar: liveModelSwapForSession: forward to %s: %v", targetSess, err)
		return "error:disconnected"
	}
	return "applied"
}

// liveRegisterProviderForSession delivers a register_provider frame to the
// named session.  Same routing logic as liveModelSwapForSession.
func liveRegisterProviderForSession(s *Sidecar, targetSess, name string, cfg map[string]any) string {
	if targetSess == s.cfg.SessionName {
		if !isPipeConnected(s) {
			return "error:disconnected"
		}
		// Propagate channel-full failure as error:queue-full so the HTTP caller
		// receives a non-200 response rather than a silent 200 OK. Issue #1844.
		if !s.RegisterProvider(name, cfg) {
			s.logger().Printf("sidecar: liveRegisterProviderForSession: enqueue failed (queue full) for %s", targetSess)
			return "error:queue-full"
		}
		return "applied"
	}

	if err := forwardRegisterProvider(targetSess, name, cfg); err != nil {
		s.logger().Printf("sidecar: liveRegisterProviderForSession: forward to %s: %v", targetSess, err)
		return "error:disconnected"
	}
	return "applied"
}

// isPipeConnected returns true when the sidecar has an active harness pipe
// connection (harnessPipeOutCh is non-nil).
func isPipeConnected(s *Sidecar) bool {
	s.mu.Lock()
	ch := s.harnessPipeOutCh
	s.mu.Unlock()
	return ch != nil
}



// resolveRoleForSession returns the root_agent_name (role) for targetSess, or
// a non-empty skipStatus string when the session should be skipped.
//
// Returns (role, "") on success.
// Returns ("", "error:disconnected") when the session cannot be looked up.
func resolveRoleForSession(s *Sidecar, targetSess string) (role, skipStatus string) {
	if s.cfg.DB == nil {
		return "", "error:disconnected"
	}
	name, _, err := s.cfg.DB.RootAgentName(targetSess)
	if err != nil {
		return "", "error:disconnected"
	}
	if name == "" {
		name = "worker" // fallback when role is not recorded
	}
	return name, ""
}

// forwardSetModel dials the target session's host-API socket and POSTs a
// /set-model request.
func forwardSetModel(targetSess, provider, model, thinking string) error {
	sockPath, err := hostAPISocketPath(targetSess)
	if err != nil {
		return fmt.Errorf("resolve socket: %w", err)
	}
	body := struct {
		Session  string `json:"session"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Thinking string `json:"thinking"`
	}{Session: targetSess, Provider: provider, Model: model, Thinking: thinking}
	return dialUnixAndPostFn(sockPath, "/set-model", body)
}

// forwardRegisterProvider dials the target session's host-API socket and POSTs
// a /register-provider-direct request.
func forwardRegisterProvider(targetSess, name string, cfg map[string]any) error {
	sockPath, err := hostAPISocketPath(targetSess)
	if err != nil {
		return fmt.Errorf("resolve socket: %w", err)
	}
	body := struct {
		Session string         `json:"session"`
		Name    string         `json:"name"`
		Config  map[string]any `json:"config"`
	}{Session: targetSess, Name: name, Config: cfg}
	return dialUnixAndPostFn(sockPath, "/register-provider-direct", body)
}

// dialUnixAndPostFn is the function used to forward HTTP POST requests to
// another sidecar's Unix socket.  Overridable in tests to intercept forwarding.
var dialUnixAndPostFn = dialUnixAndPost

// dialUnixAndPost opens a Unix socket connection to sockPath, POSTs JSON body
// to path, and returns an error when the response status is not 2xx.
func dialUnixAndPost(sockPath, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Post("http://prism-sidecar"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// hostAPISocketPath is the function used to resolve a session's host-API Unix
// socket path.  Overridable in tests.
var hostAPISocketPath = defaultHostAPISocketPath

func defaultHostAPISocketPath(sessionName string) (string, error) {
	return prismsession.SidecarHostAPIPath(sessionName)
}

// hostAPILoadProfiles loads the profiles file for the /apply-profile endpoint.
// Package-level variable so tests can inject a fake.
var hostAPILoadProfiles = config.LoadProfiles
