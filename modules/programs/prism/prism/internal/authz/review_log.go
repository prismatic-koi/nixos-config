package authz

// review_log.go — the access rule for `prism reviews log`, the read of the
// review monitor log of one review round (issue #3277).
//
// The rule:
//
//   - A caller can read the logs of the review rounds of its own session.
//   - A caller with the coordinator role can also read the logs of the
//     review rounds of a session in its own repo.
//   - Every other read is refused. There is no cross-repo grant, and the
//     tier-3 `prism checkin` privilege does not apply.
//
// Both routes of the verb call AuthorizeReviewLog: the host-API handler
// GET /groups/monitor-log, and the direct CLI route of a `host`-mode session
// (cmd/reviews_log.go). Each route supplies the caller and the caller role
// from its own trust source, as for `prism checkin` and `prism audit`:
//
//   - Host-API route: Config.SessionName of the sidecar, and the role from
//     callerHasCoordinatorRole of the sidecar. Each session has its own
//     socket, so a sandboxed caller cannot speak as another session.
//   - Direct CLI route: PRISM_SESSION_NAME, else the current tmux session,
//     and the role from callerHasCoordinatorRole in cmd/. This is a
//     cooperative signal: a `host`-mode session has no sandbox and can read
//     the file itself.
//
// The target is the only value from the request that reaches this rule. It
// can only narrow the access: a worker gets its own session and nothing
// else, whatever target it sends.

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/prismatic-koi/prism/internal/db"
)

// ReviewLogRequest carries the facts that AuthorizeReviewLog needs.
type ReviewLogRequest struct {
	// Caller is the session that asks. The route resolves it from its own
	// trust source, never from the request.
	Caller string

	// CallerIsCoordinator is the coordinator role of Caller, resolved by the
	// route.
	CallerIsCoordinator bool

	// Target is the parent session whose review rounds the caller asks for.
	// The route sets it to Caller when the request names no session.
	Target string

	// DB resolves the repo of Caller and Target. A nil handle refuses every
	// read of another session.
	DB *db.DB

	// Logger receives the diagnostics. Nil is replaced with log.Default().
	Logger *log.Logger
}

// ReviewLogDecision is the outcome of AuthorizeReviewLog. When Allow is
// false, Status is the HTTP status and Message is the error text. The direct
// CLI route turns a refusal into a non-zero exit with the same text.
type ReviewLogDecision struct {
	Allow   bool
	Status  int
	Message string
}

func denyReviewLog(status int, format string, args ...any) ReviewLogDecision {
	return ReviewLogDecision{Status: status, Message: fmt.Sprintf(format, args...)}
}

// ValidateReviewLogTarget refuses a session name that holds "/" or "..". The
// name never becomes part of a path, so this is a second barrier: a request
// of that shape is malformed, and it gets a clear refusal.
func ValidateReviewLogTarget(target string) error {
	if strings.Contains(target, "/") || strings.Contains(target, "..") {
		return fmt.Errorf("invalid session name %q: it must not contain '/' or '..'", target)
	}
	return nil
}

// AuthorizeReviewLog decides whether req.Caller can read the monitor logs of
// the review rounds of req.Target.
func AuthorizeReviewLog(req ReviewLogRequest) ReviewLogDecision {
	if req.Logger == nil {
		req.Logger = log.Default()
	}
	if req.Caller == "" {
		return denyReviewLog(http.StatusForbidden,
			"cannot determine the calling session — review log read denied")
	}
	if req.Target == "" {
		return denyReviewLog(http.StatusBadRequest, "session is required")
	}
	if err := ValidateReviewLogTarget(req.Target); err != nil {
		return denyReviewLog(http.StatusBadRequest, "%s", err.Error())
	}

	if req.Target == req.Caller {
		return ReviewLogDecision{Allow: true}
	}

	if !req.CallerIsCoordinator {
		return denyReviewLog(http.StatusForbidden,
			"a worker can read the monitor logs of the review rounds of its own session (%s) only. It cannot read the logs of %q",
			req.Caller, req.Target)
	}

	if req.DB == nil {
		req.Logger.Printf("review log: no DB handle — cannot verify the repo of %q, denying", req.Target)
		return denyReviewLog(http.StatusForbidden,
			"cannot verify the repo of %q (no database handle) — review log read denied", req.Target)
	}
	ownRepo, err := RepoFromSession(req.Caller, req.DB)
	if err != nil {
		return denyReviewLog(http.StatusForbidden,
			"cannot determine the repo of the calling session: %s", err.Error())
	}
	targetRepo, err := RepoFromSession(req.Target, req.DB)
	if err != nil {
		return denyReviewLog(http.StatusNotFound, "target %s", err.Error())
	}
	if ownRepo == "" || targetRepo != ownRepo {
		return denyReviewLog(http.StatusForbidden,
			"a coordinator can read the monitor logs of the sessions of its own repo (%s) only. %q is in another repo",
			ownRepo, req.Target)
	}
	return ReviewLogDecision{Allow: true}
}
