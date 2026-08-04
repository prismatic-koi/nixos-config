// Package authz holds the permission predicates that more than one route of a
// prism verb must share.
//
// A prism verb reaches the DB by one of two routes. A sandboxed session
// (`bwrap`, `sandbox-exec`) has PRISM_HOST_API set and proxies to the host
// sidecar, where the host-API handler applies the rule. A `host`-mode session
// has no socket, so the CLI opens prism.db itself and the handler never runs.
// A predicate that lives inside the handler therefore gates one route only.
//
// This package is the shared home for such a predicate: `cmd/` and
// `internal/sidecar` both import it, and it takes the caller session as a
// parameter rather than reading it off a receiver, so neither route owns it.
package authz

// checkin.go — the three-tier permission model for `prism checkin`
// (issue #2587; extracted to this package by issue #2619).
//
// Before #2587 the endpoint had one rule: requireCoordinator. That rule
// refused every worker, including a worker reading the review agents it had
// just spawned for its own PR — the exact read that `agents/worker.md` and the
// `prism` skill tell a worker to perform. #2587 replaced the single rule with
// three tiers.
//
//	Tier 1 — worker.
//	  A non-coordinator caller may read ONLY the review agents of its own
//	  session: a target whose session_groups.parent_session equals the
//	  caller's session name. Not its own session, not another worker, not a
//	  coordinator, not any session in another repo.
//
//	Tier 2 — coordinator.
//	  Unchanged: own-repo sessions, plus a cross-repo target only when that
//	  target is itself a coordinator.
//
//	Tier 3 — coordinator of a privileged repo.
//	  A coordinator whose repo is named in CheckinRequest.PrivilegedRepos may
//	  read ANY session in ANY repo, including another coordinator's workers
//	  and review agents. The privilege widens tier 2 and nothing else: it is
//	  consulted only where tier 2 refuses, it covers `prism checkin` alone,
//	  and every access it admits writes an audit event.
//
// # What this predicate is, and is not
//
// It is not a security boundary, and #2619 must not be read as one. A
// `host`-mode session runs with no sandbox and can read the same history with
// `sqlite3` straight out of prism.db, without using the verb at all. The
// justification for gating both routes is narrower and holds anyway: audit
// completeness (a tier-3 read leaves a record on either route), correct
// behaviour for a cooperative caller (an agent that follows the documented
// surface meets the documented rule in every isolation mode), and route
// consistency with `prism investigate` and `prism merges`.
//
// # Trust basis on each route
//
// Host-API route: caller identity rests on socket isolation. The sidecar knows
// its own Config.SessionName, and each session has its own socket directory
// (#960), so a worker cannot reach another session's sidecar to impersonate
// it.
//
// Direct CLI route: caller identity comes from PRISM_SESSION_NAME, else the
// current tmux session — the same resolution `requireMergesCoordinator` uses
// (#2608). That is a cooperative signal, not an enforced one, which is why
// this route is justified on audit completeness rather than on containment.
//
// On both routes the target is never trusted — it is resolved against the DB.

import (
	"fmt"
	"log"
	"net/http"
	"slices"

	"github.com/prismatic-koi/prism/internal/db"
)

// Checkin permission tiers. The value is carried on CheckinDecision so the
// caller can tell an ordinary grant from a privileged one without repeating
// the predicate.
const (
	CheckinTierWorker                = 1
	CheckinTierCoordinator           = 2
	CheckinTierPrivilegedCoordinator = 3
)

// CheckinPrivilegeGrantName is the grant label written to the audit event for
// every tier-3 access, on either route. It names the mechanism, so a later
// grant on the same verb is distinguishable in `prism audit` without a schema
// change.
const CheckinPrivilegeGrantName = "checkin-privileged-repo"

// CheckinDecision is the outcome of the `prism checkin` permission gate.
//
// When Allow is true, Tier names the tier that admitted the caller. When Allow
// is false, Status and Message are the HTTP status and error text. The gate
// never returns Allow=false with Status=0.
//
// Status is an HTTP status because the host-API route writes it onto the
// response. The direct CLI route has no status line to write: it turns any
// refusal into a non-zero exit and prints Message. The two routes therefore
// agree on the verdict and on the text, and differ only in how they report it.
type CheckinDecision struct {
	Allow   bool
	Tier    int
	Status  int
	Message string
}

// CheckinRequest carries every fact the predicate needs. Caller is a
// parameter, not a receiver field, which is what lets one copy serve both the
// host-API handler and the direct CLI path.
type CheckinRequest struct {
	// Caller is the session asking to read. The host-API route passes its own
	// Config.SessionName; the direct CLI route passes the resolved caller
	// session and refuses before it gets here when that cannot be resolved.
	Caller string

	// Target is the session whose history was asked for. Never trusted — it
	// is resolved against the DB.
	Target string

	// DB is the prism database. A nil handle denies at tier 1 and denies the
	// tier-3 privilege; it never widens a grant.
	DB *db.DB

	// PrivilegedRepos is the tier-3 repo list. An empty or nil list grants the
	// privilege to nobody, which is the pre-#2587 behaviour.
	PrivilegedRepos []string

	// Logger receives the fail-closed diagnostics. Nil is replaced with
	// log.Default().
	Logger *log.Logger
}

// denyCheckin builds a refusal decision.
func denyCheckin(status int, format string, args ...any) CheckinDecision {
	return CheckinDecision{Status: status, Message: fmt.Sprintf(format, args...)}
}

// allowCheckin builds a grant decision for the given tier.
func allowCheckin(tier int) CheckinDecision {
	return CheckinDecision{Allow: true, Tier: tier}
}

// AuthorizeCheckin decides whether req.Caller may read the conversation
// history of req.Target.
//
// The caller's role is resolved once, through IsCoordinatorSession. Anything
// that is not a coordinator — a worker, a review agent, an investigator, a
// session whose role cannot be determined — takes the tier-1 path, which is
// the narrowest of the three. That is the fail-closed direction: an
// undeterminable role gets the smallest grant, not the largest.
func AuthorizeCheckin(req CheckinRequest) CheckinDecision {
	if req.Logger == nil {
		req.Logger = log.Default()
	}

	// Defence in depth. Neither route reaches here with an empty caller: the
	// host-API route always carries Config.SessionName, and the direct CLI
	// route refuses an unresolvable caller with the PRISM_SESSION_NAME error
	// before it calls this function. A future caller that skips that check
	// must still not be admitted on the strength of an empty name.
	if req.Caller == "" {
		return denyCheckin(http.StatusForbidden,
			"cannot determine the calling session — checkin denied")
	}

	if !IsCoordinatorSession(req.Caller, req.DB, req.Logger) {
		return req.authorizeWorker()
	}
	return req.authorizeCoordinator()
}

// authorizeWorker implements tier 1: a worker may read only the review agents
// of its own session.
//
// Membership comes from db.GroupParentForMember, which resolves
// agent_status.group_id → session_groups.parent_session and applies no name
// heuristic. Three consequences follow, and each is deliberate:
//
//   - The check needs no schema change. session_groups already carries
//     parent_session.
//   - A review agent from an earlier round of the same session is still
//     admitted: each round registers its own session_groups row, and every one
//     of those rows carries the same parent_session.
//   - A review agent whose session_groups row was deleted is refused with 403,
//     not admitted on the strength of its "<parent>~review-<N>-<agent>" name
//     and not surfaced as a 500.
func (req CheckinRequest) authorizeWorker() CheckinDecision {
	// Self-checkin is not granted. The scope is "the review agents of your own
	// session", and the caller is not one of them.
	if req.Target == req.Caller {
		return denyCheckin(http.StatusForbidden,
			"a worker cannot check in on its own session; the grant covers the review agents of your session only (%s~review-<N>-<agent>)",
			req.Caller)
	}

	if req.DB == nil {
		req.Logger.Printf("checkin: no DB handle — cannot verify review-agent scope for %q, denying", req.Target)
		return denyCheckin(http.StatusForbidden,
			"cannot verify that %q is a review agent of your session (no database handle) — checkin denied", req.Target)
	}

	parent, found, err := req.DB.GroupParentForMember(req.Target)
	if err != nil {
		// Fail closed. A scope check that cannot complete must not admit.
		req.Logger.Printf("checkin: review-agent scope lookup for %q failed: %v — denying", req.Target, err)
		return denyCheckin(http.StatusForbidden,
			"cannot verify that %q is a review agent of your session (scope lookup failed) — checkin denied", req.Target)
	}
	if !found {
		return denyCheckin(http.StatusForbidden,
			"%q has no review-group row, so it is not a review agent of your session; workers can check in on %s~review-<N>-<agent> only",
			req.Target, req.Caller)
	}
	if parent != req.Caller {
		// The parent of the target is deliberately not named: it belongs to
		// another session, and the refusal must not disclose it.
		return denyCheckin(http.StatusForbidden,
			"%q is a review agent of another session; workers can check in on %s~review-<N>-<agent> only",
			req.Target, req.Caller)
	}
	return allowCheckin(CheckinTierWorker)
}

// authorizeCoordinator implements tiers 2 and 3.
//
// Tier 2 is byte-for-byte the pre-#2587 rule: own-repo targets pass, and a
// cross-repo target passes only when it is itself a coordinator. Tier 3 is
// consulted at exactly one point — where tier 2 refuses a cross-repo
// non-coordinator target. Placing it there has two effects worth keeping:
// an unresolvable target still yields 404 rather than an empty 200 for a
// privileged caller, and the audit log records only the accesses the
// privilege actually admitted, not every own-repo read.
func (req CheckinRequest) authorizeCoordinator() CheckinDecision {
	ownRepo, repoErr := RepoFromSession(req.Caller, req.DB)
	if repoErr != nil {
		return denyCheckin(http.StatusInternalServerError,
			"cannot derive repo from session name: %s", repoErr.Error())
	}
	targetRepo, targetRepoErr := RepoFromSession(req.Target, req.DB)
	if targetRepoErr != nil {
		// Both the DB lookup and the name-parse fallback failed — the target
		// session is unknown. Return 404 so the CLI can surface a clear
		// "session not found" rather than a parse error (issue #2112).
		return denyCheckin(http.StatusNotFound, "target %s", targetRepoErr.Error())
	}

	if targetRepo == ownRepo || IsCoordinatorSession(req.Target, req.DB, req.Logger) {
		return allowCheckin(CheckinTierCoordinator)
	}

	if req.privilegeGranted() {
		return allowCheckin(CheckinTierPrivilegedCoordinator)
	}

	return denyCheckin(http.StatusForbidden,
		"cross-repo checkin can only target coordinators (<repo>@main), got %q", req.Target)
}

// privilegeGranted reports whether the caller carries the tier-3
// troubleshooting privilege.
//
// Two conditions must both hold:
//
//  1. The caller's repo appears in PrivilegedRepos. An empty or absent list
//     therefore grants the privilege to nobody, which is the pre-#2587
//     behaviour.
//  2. The caller's agent_status row exists and carries
//     root_agent_name = 'coordinator'.
//
// Condition 2 is the answer to the caution recorded on #2587.
// IsCoordinatorSession returns dbBased || nameBased, where nameBased is the
// "@main" suffix and wins on disagreement — a session named <repo>@main is
// admitted there on the name alone, with no DB evidence. A privilege that
// reaches into every repo must not rest on a string suffix, so this check
// consults root_agent_name directly and refuses when the row is absent, the
// column is NULL, or the read fails.
func (req CheckinRequest) privilegeGranted() bool {
	if len(req.PrivilegedRepos) == 0 {
		return false
	}
	if req.DB == nil {
		return false
	}

	repo, err := RepoFromSession(req.Caller, req.DB)
	if err != nil || repo == "" {
		return false
	}
	if !slices.Contains(req.PrivilegedRepos, repo) {
		return false
	}

	rootAgentName, rowExists, err := req.DB.RootAgentName(req.Caller)
	if err != nil {
		req.Logger.Printf("checkin: privilege check: root_agent_name read for %q failed: %v — privilege refused", req.Caller, err)
		return false
	}
	if !rowExists || rootAgentName == "" {
		req.Logger.Printf("checkin: privilege check: no DB-backed root_agent_name for %q — privilege refused (the @main name heuristic alone is not sufficient)", req.Caller)
		return false
	}
	if rootAgentName != "coordinator" {
		req.Logger.Printf("checkin: privilege check: root_agent_name=%q for %q is not a coordinator — privilege refused", rootAgentName, req.Caller)
		return false
	}
	return true
}
