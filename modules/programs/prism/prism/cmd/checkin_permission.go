package cmd

// checkin_permission.go — the coordinator/worker permission gate on the DIRECT
// CLI route of `prism checkin` (issue #2619).
//
// `prism checkin` reaches the DB by one of two routes:
//
//   - Proxy route (PRISM_HOST_API set — a sandboxed caller): the host-API
//     `/checkin` handler in internal/sidecar/host_api.go applies the tiers and
//     answers HTTP 403 / 404 (issue #2587, PR #2617).
//   - Direct route (PRISM_HOST_API unset — a `host`-isolation caller):
//     authorizeDirectCheckin below applies the SAME predicate and returns a
//     non-zero exit.
//
// Both routes must stay gated. When you change either one, change the other in
// the same edit. The predicate itself is authz.AuthorizeCheckin — one copy,
// imported by both routes, with the caller session passed in as a parameter.
// Do not add a rule here that the host-API handler does not apply, or the role
// boundary of the verb starts to depend on the caller's isolation mode.
//
// # This is not a security boundary
//
// A `host`-mode session runs with no sandbox. It can read the same history
// without the verb at all:
//
//	sqlite3 ~/.local/state/prism/prism.db 'select * from agent_events where ...'
//
// The gate is justified on three narrower grounds, recorded on #2619: audit
// completeness (a tier-3 read now leaves a record on either route), correct
// behaviour for a cooperative caller, and route consistency with
// `prism investigate` (#2597) and `prism merges` (#2608).
//
// # Scope
//
// The gate sits on runCheckinSession. Every caller of that function inherits
// it, and there are two:
//
//  1. `prism checkin <session>` — the default view, `--json`, and `--types`.
//  2. `prism checkin <parent>~review --verbose` — runCheckinReviewRoundsByGroup
//     and runCheckinReviewRounds (cmd/checkin_review.go) render each group
//     member THROUGH runCheckinSession, so the gate applies once per member.
//     A refused member prints `[error reading session ...]` on stderr and the
//     loop continues to the next one; the command does not abort. On the
//     host-API route each of those per-member calls already proxied to
//     `/checkin` and met the tiers there, so this closes the direct-route half
//     of a path that was half-gated, rather than gating something new.
//
// Not gated, on either route:
//
//   - `prism checkin` with no argument — lists sessions, reads no history.
//   - `--compare`.
//
// `prism checkin <parent>~review` WITHOUT `--verbose` — the aggregate summary
// — is gated separately (issue #2628), by
// authorizeDirectCheckinReviewAggregate below and, on the host-API route, by
// GET /checkin/review-summary. It could not reuse authorizeDirectCheckin
// as-is: the target there is a set of sessions ("the review agents OF parent
// X"), not a single session, so passing the parent straight in as Target
// would hit AuthorizeCheckin's tier-1 self-checkin denial — the exact worker
// reading its own review group that tier 1 exists to admit. See
// authz.AuthorizeCheckinReviewAggregate's doc comment for the full
// reasoning. Its underlying group listing (GroupMembersForParent — session
// names and states, no conversation content) still runs unconditionally
// ahead of the gate on the direct route, matching the no-arg listing
// precedent above; only the content-bearing read is gated.
//
// The verbose form (`--verbose`) is gated per member through
// runCheckinSession, as documented above; its own group-listing call is
// likewise unconditional and non-content-bearing.
//
// Inside a sandbox, the direct route's group-listing call (openDB()) fails,
// so the non-verbose summary now proxies through GET /checkin/review-summary
// when PRISM_HOST_API is set — see cmd/checkin_review.go.
//
// # Audit consequence of the per-member path
//
// A tier-3 caller running `prism checkin <parent>~review --verbose` writes one
// audit event PER MEMBER, not one per command, because
// writeDirectCheckinPrivilegeAudit fires inside each runCheckinSession call.
// That is deliberate and route-consistent: the host-API route issues one
// `/checkin` per member and audits each one, and each member's history is a
// distinct privileged read.

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/review"
)

// directCheckinAuditTool is the payload.Audit.Tool value for a tier-3 access
// admitted on the direct CLI route. The host-API route writes
// "prism-host-api". The two differ so `prism audit` shows which route a
// privileged read came through; the grant label — the field that names the
// privilege — is the shared authz.CheckinPrivilegeGrantName on both.
const directCheckinAuditTool = "prism-cli"

// authorizeDirectCheckin resolves the caller session and applies the shared
// permission predicate. It returns nil when the read is permitted, and a
// non-nil error (hence a non-zero exit) otherwise.
func authorizeDirectCheckin(target string) error {
	return authorizeDirectCheckinFor(review.LookupParentSession(), target)
}

// authorizeDirectCheckinFor is authorizeDirectCheckin with the caller session
// injected, so the refusal paths are testable without a live tmux server.
//
// # Fail-closed behaviour
//
// A caller that cannot be resolved is refused, exactly as
// requireMergesCoordinator refuses one (#2608), with the same remedy named in
// the error text. The consequence is deliberate and was decided on #2619:
// bare-shell `prism checkin` from a plain terminal outside tmux is refused.
// There is no carve-out. A bare shell has no session identity, so there is no
// tier to place it in, and the fallback that would admit it is the widest
// grant rather than the narrowest.
//
// A DB that cannot be opened is refused for the same reason: every tier scopes
// on rows in that DB, so a gate that cannot read it cannot admit. This does
// change one pre-#2619 behaviour on this route — `prism checkin` used to fall
// back to the tmux screen-scrape when the DB was unreachable, and that
// fallback is now behind the gate. The screen-scrape still runs for a session
// that has no rows in a DB that opened.
func authorizeDirectCheckinFor(caller, target string) error {
	if caller == "" {
		return fmt.Errorf("prism checkin: cannot determine caller session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism checkin: cannot verify checkin permission (open db: %v) — checkin denied", err)
	}
	defer d.Close()

	// A missing file returns an empty list with no error, which grants the
	// tier-3 privilege to nobody. A malformed or unreadable file fails closed
	// the same way, matching how cmd/sidecar.go treats it on the host-API
	// route: warn and carry on with an empty list rather than widen the grant.
	privilegedRepos, privErr := config.LoadCheckinPrivilegedRepos()
	if privErr != nil {
		log.Printf("prism checkin: read %s: %v (no repo carries the checkin troubleshooting privilege)",
			config.CheckinPrivilegedReposFileName, privErr)
		privilegedRepos = nil
	}

	decision := authz.AuthorizeCheckin(authz.CheckinRequest{
		Caller:          caller,
		Target:          target,
		DB:              d,
		PrivilegedRepos: privilegedRepos,
		Logger:          log.Default(),
	})
	if !decision.Allow {
		// decision.Status is an HTTP status, which this route has nowhere to
		// put. The refusal text is the same on both routes; only the transport
		// differs.
		return fmt.Errorf("prism checkin: %s", decision.Message)
	}
	if decision.Tier == authz.CheckinTierPrivilegedCoordinator {
		writeDirectCheckinPrivilegeAudit(d, caller, target)
	}
	return nil
}

// authorizeDirectCheckinReviewAggregate resolves the caller session and
// applies the shared aggregate permission predicate for the non-verbose
// `prism checkin <parent>~review` form (issue #2628). It returns nil when the
// read is permitted, and a non-nil error (hence a non-zero exit) otherwise.
//
// This is the aggregate counterpart of authorizeDirectCheckin: it answers "may
// the caller read the review agents OF parentSession" via
// authz.AuthorizeCheckinReviewAggregate, rather than "may the caller read
// session parentSession" via authz.AuthorizeCheckin. See that function's doc
// comment for why the two questions need separate predicates.
func authorizeDirectCheckinReviewAggregate(parentSession string) error {
	return authorizeDirectCheckinReviewAggregateFor(review.LookupParentSession(), parentSession)
}

// authorizeDirectCheckinReviewAggregateFor is
// authorizeDirectCheckinReviewAggregate with the caller session injected, so
// the refusal paths are testable without a live tmux server. Fail-closed
// behaviour matches authorizeDirectCheckinFor: an unresolvable caller and an
// unreadable DB are both refused.
func authorizeDirectCheckinReviewAggregateFor(caller, parentSession string) error {
	if caller == "" {
		return fmt.Errorf("prism checkin: cannot determine caller session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism checkin: cannot verify checkin permission (open db: %v) — checkin denied", err)
	}
	defer d.Close()

	privilegedRepos, privErr := config.LoadCheckinPrivilegedRepos()
	if privErr != nil {
		log.Printf("prism checkin: read %s: %v (no repo carries the checkin troubleshooting privilege)",
			config.CheckinPrivilegedReposFileName, privErr)
		privilegedRepos = nil
	}

	decision := authz.AuthorizeCheckinReviewAggregate(authz.CheckinRequest{
		Caller:          caller,
		Target:          parentSession,
		DB:              d,
		PrivilegedRepos: privilegedRepos,
		Logger:          log.Default(),
	})
	if !decision.Allow {
		return fmt.Errorf("prism checkin: %s", decision.Message)
	}
	if decision.Tier == authz.CheckinTierPrivilegedCoordinator {
		writeDirectCheckinPrivilegeAudit(d, caller, parentSession)
	}
	return nil
}

// writeDirectCheckinPrivilegeAudit records one tier-3 access admitted on the
// direct CLI route, so `prism audit` answers "who read what, and when"
// regardless of which route the read came through. It mirrors
// Sidecar.writeCheckinPrivilegeAudit field for field.
//
// A failure to write the audit row does not refuse the read: the read was
// already authorised, and turning a logging failure into a refusal would make
// the verb less available without making it more accountable. The failure is
// reported on stderr so it is not silent.
func writeDirectCheckinPrivilegeAudit(d *db.DB, caller, target string) {
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: caller,
		Type:        "audit",
		CreatedAt:   time.Now(),
	}
	// Stamp the caller's repo, worktree, and instance_id when the row is
	// available, so the audit event joins to the session the way the
	// sidecar-written rows do. A missing row is not fatal: the caller name,
	// the target, and the grant are what the audit trail is for.
	if status, statusErr := d.CurrentStatus(caller); statusErr == nil && status != nil {
		event.Repo = status.Repo
		event.Worktree = status.Worktree
		event.InstanceID = status.InstanceID
	}

	data, marshalErr := json.Marshal(payload.Audit{
		Tool:        directCheckinAuditTool,
		Command:     "prism checkin " + target,
		SessionName: caller,
		Target:      target,
		Grant:       authz.CheckinPrivilegeGrantName,
	})
	if marshalErr != nil {
		log.Printf("prism checkin: audit: marshal privileged-checkin event: %v", marshalErr)
		return
	}
	event.Payload = string(data)

	if writeErr := d.WriteEvent(event); writeErr != nil {
		log.Printf("prism checkin: audit: record privileged checkin (%s read %s): %v", caller, target, writeErr)
	}
}
