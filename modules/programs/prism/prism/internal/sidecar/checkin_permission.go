package sidecar

// checkin_permission.go — the sidecar's binding to the shared `prism checkin`
// permission predicate (issues #2587, #2619).
//
// The three-tier model itself lives in internal/authz. It moved there in
// #2619, unchanged, so the direct CLI route can call the same copy: the
// predicate used to be a method on *Sidecar that read the caller from
// s.cfg.SessionName, which `cmd/` has no way to construct. authz.CheckinRequest
// takes the caller as a parameter instead.
//
// What stays here is the sidecar-specific half: the adapter that fills a
// CheckinRequest from Config, and the audit writer, which needs s.writeEvent
// and s.mu.
//
// The host-API handler in host_api.go is deliberately untouched by the move.
// authz.CheckinDecision carries the same four fields under the same names, and
// checkinPrivilegeGrantName below keeps its sidecar-local spelling, so the
// handler and its regression tests compile and behave exactly as before. That
// is the point: #2619 must not change the host-API route for any tier.

import (
	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/payload"
)

// Sidecar-local spellings of the shared constants. They are aliases, not
// copies: the value comes from internal/authz, so the audit rows written by
// the host-API route and by the direct CLI route carry one grant label.
const (
	checkinTierPrivilegedCoordinator = authz.CheckinTierPrivilegedCoordinator
	checkinPrivilegeGrantName        = authz.CheckinPrivilegeGrantName
)

// authorizeCheckin decides whether this sidecar's session may read the
// conversation history of targetSession.
//
// It is a binding, not a rule: every decision is made by
// authz.AuthorizeCheckin. The equivalent binding on the direct CLI route is
// authorizeDirectCheckinFor in cmd/checkin_permission.go, and the two must
// keep passing the same facts.
func (s *Sidecar) authorizeCheckin(targetSession string) authz.CheckinDecision {
	return authz.AuthorizeCheckin(authz.CheckinRequest{
		Caller:          s.cfg.SessionName,
		Target:          targetSession,
		DB:              s.cfg.DB,
		PrivilegedRepos: s.cfg.CheckinPrivilegedRepos,
		Logger:          s.logger(),
	})
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
