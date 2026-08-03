package sidecar

// checkin_permission.go — the three-tier permission model for GET /checkin
// (issue #2587).
//
// Before #2587 the endpoint had one rule: requireCoordinator. That rule
// refused every worker, including a worker reading the review agents it had
// just spawned for its own PR — the exact read that `agents/worker.md` and the
// `prism` skill tell a worker to perform. This file replaces the single rule
// with three tiers.
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
//	  A coordinator whose repo is named in Config.CheckinPrivilegedRepos may
//	  read ANY session in ANY repo, including another coordinator's workers
//	  and review agents. The privilege widens tier 2 and nothing else: it is
//	  consulted only where tier 2 refuses, it covers /checkin alone, and every
//	  access it admits writes an audit event.
//
// Trust basis. Caller identity rests on socket isolation: the sidecar knows
// its own s.cfg.SessionName, and each session has its own socket directory
// (#960), so a worker cannot reach another session's sidecar to impersonate
// it. The target is never trusted — it is resolved against the DB.

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/prismatic-koi/prism/internal/payload"
)

// Checkin permission tiers. The value is carried on checkinDecision so the
// handler can tell an ordinary grant from a privileged one without repeating
// the predicate.
const (
	checkinTierWorker                = 1
	checkinTierCoordinator           = 2
	checkinTierPrivilegedCoordinator = 3
)

// checkinPrivilegeGrantName is the grant label written to the audit event for
// every tier-3 access. It names the mechanism, so a later grant on the same
// endpoint is distinguishable in `prism audit` without a schema change.
const checkinPrivilegeGrantName = "checkin-privileged-repo"

// checkinDecision is the outcome of the /checkin permission gate.
//
// When Allow is true, Tier names the tier that admitted the caller. When Allow
// is false, Status and Message are the HTTP status and error text the handler
// must write. The gate never returns Allow=false with Status=0.
type checkinDecision struct {
	Allow   bool
	Tier    int
	Status  int
	Message string
}

// denyCheckin builds a refusal decision.
func denyCheckin(status int, format string, args ...any) checkinDecision {
	return checkinDecision{Status: status, Message: fmt.Sprintf(format, args...)}
}

// allowCheckin builds a grant decision for the given tier.
func allowCheckin(tier int) checkinDecision {
	return checkinDecision{Allow: true, Tier: tier}
}

// authorizeCheckin decides whether this sidecar's session may read the
// conversation history of targetSession.
//
// The caller's role is resolved once, through isCoordinatorSession. Anything
// that is not a coordinator — a worker, a review agent, an investigator, a
// session whose role cannot be determined — takes the tier-1 path, which is
// the narrowest of the three. That is the fail-closed direction: an
// undeterminable role gets the smallest grant, not the largest.
func (s *Sidecar) authorizeCheckin(targetSession string) checkinDecision {
	caller := s.cfg.SessionName
	if !isCoordinatorSession(caller, s.cfg.DB, s.logger()) {
		return s.authorizeWorkerCheckin(caller, targetSession)
	}
	return s.authorizeCoordinatorCheckin(caller, targetSession)
}

// authorizeWorkerCheckin implements tier 1: a worker may read only the review
// agents of its own session.
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
func (s *Sidecar) authorizeWorkerCheckin(caller, target string) checkinDecision {
	// Self-checkin is not granted. The scope is "the review agents of your own
	// session", and the caller is not one of them.
	if target == caller {
		return denyCheckin(http.StatusForbidden,
			"a worker cannot check in on its own session; the grant covers the review agents of your session only (%s~review-<N>-<agent>)",
			caller)
	}

	if s.cfg.DB == nil {
		s.logger().Printf("sidecar: /checkin: no DB handle — cannot verify review-agent scope for %q, denying", target)
		return denyCheckin(http.StatusForbidden,
			"cannot verify that %q is a review agent of your session (no database handle) — checkin denied", target)
	}

	parent, found, err := s.cfg.DB.GroupParentForMember(target)
	if err != nil {
		// Fail closed. A scope check that cannot complete must not admit.
		s.logger().Printf("sidecar: /checkin: review-agent scope lookup for %q failed: %v — denying", target, err)
		return denyCheckin(http.StatusForbidden,
			"cannot verify that %q is a review agent of your session (scope lookup failed) — checkin denied", target)
	}
	if !found {
		return denyCheckin(http.StatusForbidden,
			"%q has no review-group row, so it is not a review agent of your session; workers can check in on %s~review-<N>-<agent> only",
			target, caller)
	}
	if parent != caller {
		// The parent of the target is deliberately not named: it belongs to
		// another session, and the refusal must not disclose it.
		return denyCheckin(http.StatusForbidden,
			"%q is a review agent of another session; workers can check in on %s~review-<N>-<agent> only",
			target, caller)
	}
	return allowCheckin(checkinTierWorker)
}

// authorizeCoordinatorCheckin implements tiers 2 and 3.
//
// Tier 2 is byte-for-byte the pre-#2587 rule: own-repo targets pass, and a
// cross-repo target passes only when it is itself a coordinator. Tier 3 is
// consulted at exactly one point — where tier 2 refuses a cross-repo
// non-coordinator target. Placing it there has two effects worth keeping:
// an unresolvable target still yields 404 rather than an empty 200 for a
// privileged caller, and the audit log records only the accesses the
// privilege actually admitted, not every own-repo read.
func (s *Sidecar) authorizeCoordinatorCheckin(caller, target string) checkinDecision {
	ownRepo, repoErr := repoFromSession(caller, s.cfg.DB)
	if repoErr != nil {
		return denyCheckin(http.StatusInternalServerError,
			"cannot derive repo from session name: %s", repoErr.Error())
	}
	targetRepo, targetRepoErr := repoFromSession(target, s.cfg.DB)
	if targetRepoErr != nil {
		// Both the DB lookup and the name-parse fallback failed — the target
		// session is unknown. Return 404 so the CLI can surface a clear
		// "session not found" rather than a parse error (issue #2112).
		return denyCheckin(http.StatusNotFound, "target %s", targetRepoErr.Error())
	}

	if targetRepo == ownRepo || isCoordinatorSession(target, s.cfg.DB, s.logger()) {
		return allowCheckin(checkinTierCoordinator)
	}

	if s.checkinPrivilegeGranted() {
		return allowCheckin(checkinTierPrivilegedCoordinator)
	}

	return denyCheckin(http.StatusForbidden,
		"cross-repo checkin can only target coordinators (<repo>@main), got %q", target)
}

// checkinPrivilegeGranted reports whether this session carries the tier-3
// troubleshooting privilege.
//
// Two conditions must both hold:
//
//  1. The caller's repo appears in Config.CheckinPrivilegedRepos. An empty or
//     absent list therefore grants the privilege to nobody, which is the
//     pre-#2587 behaviour.
//  2. The caller's agent_status row exists and carries
//     root_agent_name = 'coordinator'.
//
// Condition 2 is the answer to the caution recorded on #2587.
// isCoordinatorSession returns dbBased || nameBased, where nameBased is the
// "@main" suffix and wins on disagreement — a session named <repo>@main is
// admitted there on the name alone, with no DB evidence. A privilege that
// reaches into every repo must not rest on a string suffix, so this check
// consults root_agent_name directly and refuses when the row is absent, the
// column is NULL, or the read fails.
func (s *Sidecar) checkinPrivilegeGranted() bool {
	if len(s.cfg.CheckinPrivilegedRepos) == 0 {
		return false
	}
	if s.cfg.DB == nil {
		return false
	}

	repo, err := repoFromSession(s.cfg.SessionName, s.cfg.DB)
	if err != nil || repo == "" {
		return false
	}
	if !slices.Contains(s.cfg.CheckinPrivilegedRepos, repo) {
		return false
	}

	rootAgentName, rowExists, err := s.cfg.DB.RootAgentName(s.cfg.SessionName)
	if err != nil {
		s.logger().Printf("sidecar: /checkin: privilege check: root_agent_name read for %q failed: %v — privilege refused", s.cfg.SessionName, err)
		return false
	}
	if !rowExists || rootAgentName == "" {
		s.logger().Printf("sidecar: /checkin: privilege check: no DB-backed root_agent_name for %q — privilege refused (the @main name heuristic alone is not sufficient)", s.cfg.SessionName)
		return false
	}
	if rootAgentName != "coordinator" {
		s.logger().Printf("sidecar: /checkin: privilege check: root_agent_name=%q for %q is not a coordinator — privilege refused", rootAgentName, s.cfg.SessionName)
		return false
	}
	return true
}

// writeCheckinPrivilegeAudit records one tier-3 access in the persistent audit
// trail, so `prism audit` can answer "who read what, and when".
//
// writeEvent stamps the event with the caller's session name and the clock's
// current time; Target names the session that was read. The Command field
// carries the equivalent CLI invocation so the row renders in the `prism
// audit` table and matches `prism audit --pattern "prism checkin"` alongside
// the bash-promoted rows.
//
// # Locking
//
// s.mu MUST be held across the writeEvent call, and this function takes it.
// writeEvent reads s.harnessSessionID and takes its ADDRESS, which is
// dereferenced later inside DB.WriteEvent. That field lives inside the s.mu
// block and is written by handleSessionCreated / handleSessionUpdated under
// the lock HandleEvent holds for the whole SSE dispatch. This function runs on
// a host-API handler goroutine, which is concurrent with the SSE loop, so
// without the lock an unsynchronised read races a write on a string header —
// two words, so a torn read can produce a mismatched pointer and length, and
// the later dereference can crash the sidecar. The shape mirrors
// writeStartupErrorImpl, which locks solely to make its writeEvent call.
//
// Taking the lock here is safe: no caller on the /checkin path holds s.mu.
// The handler reaches this function straight after authorizeCheckin, and
// neither that predicate nor any helper it calls touches s.mu.
func (s *Sidecar) writeCheckinPrivilegeAudit(target string) {
	if s.cfg.DB == nil {
		return
	}
	s.mu.Lock()
	s.writeEvent("audit", payload.Audit{
		Tool:        "prism-host-api",
		Command:     "prism checkin " + target,
		SessionName: s.cfg.SessionName,
		Target:      target,
		Grant:       checkinPrivilegeGrantName,
	}, nil)
	s.mu.Unlock()
	s.logger().Printf("sidecar: audit: privileged checkin recorded: %s read %s", s.cfg.SessionName, target)
}
