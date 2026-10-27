package sidecar

// Host-API endpoint for `prism reviews log` (issue #3277).
//
// The review monitor writes its log to $XDG_STATE_HOME/prism/review-monitor/.
// No sandbox mounts that directory, so a sandboxed session reads the log
// through this endpoint.
//
// Endpoint:
//
//	GET /groups/monitor-log?round=<N>[&session=<parent>]
//
// Response body on success:
//
//	{"session": "<parent>", "round": N, "logs": [...review.MonitorLog...]}
//
// The access rule is authz.AuthorizeReviewLog. The caller is
// Config.SessionName of this sidecar, never a value from the request.
// `session` names the parent session to read, and it defaults to the caller.
// The log path comes from the session_groups rows of that parent
// (review.ReadMonitorLogs), so neither `session` nor `round` becomes part of
// a path.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/review"
)

// hostAPIReviewLog handles GET /groups/monitor-log.
func (s *Sidecar) hostAPIReviewLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	round, err := strconv.Atoi(q.Get("round"))
	if err != nil || round <= 0 {
		writeDBErr(w, http.StatusBadRequest, "round must be a positive integer")
		return
	}

	target := q.Get("session")
	if target == "" {
		target = s.cfg.SessionName
	}

	decision := authz.AuthorizeReviewLog(authz.ReviewLogRequest{
		Caller:              s.cfg.SessionName,
		CallerIsCoordinator: s.callerHasCoordinatorRole(),
		Target:              target,
		DB:                  s.cfg.DB,
		Logger:              s.logger(),
	})
	if !decision.Allow {
		writeDBErr(w, decision.Status, decision.Message)
		return
	}

	if s.cfg.DB == nil {
		writeDBErr(w, http.StatusInternalServerError, "review log: no database handle")
		return
	}
	logs, err := review.ReadMonitorLogs(s.cfg.DB, target, round)
	if errors.Is(err, review.ErrNoMonitorLog) {
		writeDBErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "review log: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"session": target, "round": round, "logs": logs}); err != nil {
		s.logger().Printf("sidecar: host-API /groups/monitor-log: encode response failed: %v", err)
	}
}
