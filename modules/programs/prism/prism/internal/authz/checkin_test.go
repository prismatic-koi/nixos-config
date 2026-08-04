package authz

// checkin_test.go — issue #2619.
//
// The tier table itself is pinned from the outside, once per route:
// internal/sidecar/checkin_permission_test.go drives the host-API handler, and
// cmd/checkin_permission_test.go drives the direct CLI gate. This file pins the
// one property that is neither route's to prove — that the predicate reads the
// caller from its parameter and not from an ambient receiver, which is what
// makes a single shared copy possible at all.
//
// Keep it small on purpose. A third full copy of the tier table here would be
// a third place to update, and the two route-level suites would stop being the
// authority on their own routes.

import (
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// quietLogger keeps the fail-closed diagnostics out of the test output.
func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestAuthorizeCheckin_CallerIsAParameter is the structural AC of #2619: one
// copy of the predicate serves both routes, with caller identity passed in.
//
// The same target, the same configuration, and two different callers must
// produce two different verdicts. If the predicate ever reads identity from
// somewhere other than CheckinRequest.Caller, these two calls converge and the
// test fails.
func TestAuthorizeCheckin_CallerIsAParameter(t *testing.T) {
	const target = "demo@feature~review-1-review-goal"

	asWorker := AuthorizeCheckin(CheckinRequest{
		Caller: "demo@feature",
		Target: target,
		Logger: quietLogger(),
	})
	asCoordinator := AuthorizeCheckin(CheckinRequest{
		Caller: "demo@main",
		Target: target,
		Logger: quietLogger(),
	})

	// The worker call has no DB to resolve review-group membership with, so
	// tier 1 fails closed. The coordinator call resolves both repos from the
	// name and admits an own-repo target at tier 2.
	if asWorker.Allow {
		t.Errorf("caller %q was admitted with no DB to verify review-agent scope — tier 1 must fail closed", "demo@feature")
	}
	if !asCoordinator.Allow {
		t.Fatalf("caller %q was refused an own-repo target: %s", "demo@main", asCoordinator.Message)
	}
	if asCoordinator.Tier != CheckinTierCoordinator {
		t.Errorf("coordinator tier = %d, want %d", asCoordinator.Tier, CheckinTierCoordinator)
	}
}

// TestAuthorizeCheckin_EmptyCallerIsDenied pins the defence-in-depth branch.
// Both routes refuse an unresolvable caller before they get here — the
// host-API route always carries a session name, and the direct CLI route
// returns the PRISM_SESSION_NAME error first — but a future caller that skips
// that check must not be admitted on the strength of an empty name.
func TestAuthorizeCheckin_EmptyCallerIsDenied(t *testing.T) {
	got := AuthorizeCheckin(CheckinRequest{
		Caller: "",
		Target: "demo@main",
		Logger: quietLogger(),
	})
	if got.Allow {
		t.Fatal("an empty caller was admitted — the predicate must fail closed")
	}
	if got.Status == 0 {
		t.Error("a refusal carried Status 0; the gate never returns Allow=false with Status=0")
	}
}

// TestAuthorizeCheckin_NilLoggerDoesNotPanic covers the normalisation step. The
// fail-closed paths log, and a caller that passes no logger must still get a
// decision rather than a panic.
func TestAuthorizeCheckin_NilLoggerDoesNotPanic(t *testing.T) {
	got := AuthorizeCheckin(CheckinRequest{
		Caller: "demo@feature",
		Target: "other@feature",
		Logger: nil,
	})
	if got.Allow {
		t.Error("a worker with no DB was admitted — tier 1 must fail closed")
	}
}

// TestAuthorizeCheckin_WorkerScopeIsResolvedForTheGivenCaller is the DB-backed
// companion: two callers, one review group, one target. The caller that owns
// the group is admitted and the other is refused, so the tier-1 scope is keyed
// on the parameter rather than on the target's name shape.
func TestAuthorizeCheckin_WorkerScopeIsResolvedForTheGivenCaller(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	const (
		repo        = "prism-test-authz"
		owner       = "prism-test-authz@feature"
		stranger    = "prism-test-authz@other-feature"
		reviewAgent = "prism-test-authz@feature~review-1-review-goal"
	)
	for _, s := range []string{owner, stranger, reviewAgent} {
		if err := d.UpsertStatusSeedRootAgentName(s, repo, "/tmp/"+repo, "active", nil, nil, "worker", "", ""); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	groupID, err := d.RegisterGroupWithPR(owner, "42", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	if err := d.SetGroupID(reviewAgent, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	ownerDecision := AuthorizeCheckin(CheckinRequest{Caller: owner, Target: reviewAgent, DB: d, Logger: quietLogger()})
	if !ownerDecision.Allow {
		t.Errorf("the owning worker was refused its own review agent: %s", ownerDecision.Message)
	}
	if ownerDecision.Tier != CheckinTierWorker {
		t.Errorf("owner tier = %d, want %d", ownerDecision.Tier, CheckinTierWorker)
	}

	strangerDecision := AuthorizeCheckin(CheckinRequest{Caller: stranger, Target: reviewAgent, DB: d, Logger: quietLogger()})
	if strangerDecision.Allow {
		t.Error("a worker that does not own the review group was admitted")
	}
}
