package iris

// supervisor_testhelpers.go — test-only constructors that expose enough of
// the Supervisor surface for cross-package integration tests (cmd/iris,
// internal/iris/parity) without making the production fields public.
//
// These helpers are not gated behind a `_test.go` filename suffix because
// they are consumed by tests in OTHER packages (cmd/iris), where Go's
// test-file visibility rules would hide them. They are still strictly
// for test use — production code does not construct Supervisors this way.

import (
	"time"

	"github.com/google/uuid"
)

// NewFakeSupervisorForTest returns a Supervisor populated with just enough
// fields to satisfy the review-spawn orchestrator and other callers that
// only invoke SessionRecord() / InstanceID(). The returned supervisor has
// NO running pi child and will NOT respond to SendRPC / Kill.
//
// Intended for the integration tests in cmd/iris/review_integration_test.go
// and internal/iris/parity/. Production callers MUST use SpawnSession or
// NewSupervisor instead — those construct a fully-wired supervisor with a
// real harness socket, log file, and child process.
func NewFakeSupervisorForTest(sessionName, worktree, role string) *Supervisor {
	return &Supervisor{
		sess: SessionRecord{
			InstanceID:  uuid.NewString(),
			SessionName: sessionName,
			Worktree:    worktree,
			Role:        role,
			State:       StateActive,
			StartedAt:   time.Now().UTC(),
		},
	}
}
