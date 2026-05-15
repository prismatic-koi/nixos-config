package iris

// review.go — review-group orchestration for iris (D-10 parity).
//
// Prism's `prism review <pr>` spawns 5 review agents (review-code,
// review-security, review-qa, review-context, review-goal) that share a
// review-group identifier. The parent session subscribes to per-member
// completion and receives a single `prism prompt` (review-complete) frame
// once every member has reached a terminal state.
//
// The iris equivalent uses the same DB-level mechanism (session_groups +
// agent_status.group_id, already shared per the §10.4 schema-is-shared
// design point) plus an in-daemon watcher that:
//
//   1. Registers a group via db.RegisterGroup(parent).
//   2. Spawns the 5 review-agent sessions through SpawnSession and tags
//      each one's agent_status row with the group_id.
//   3. Polls db.GroupCompleted(groupID) until true.
//   4. Calls onComplete(groupID, parent, results) so the daemon can deliver
//      a single review-complete `prism prompt`-equivalent to the parent.
//
// The contract D-10 tests assert is structural: 5 sessions enter active
// state, share a group ID, and onComplete fires exactly once after the
// last member finishes. The review agents' actual verdicts are out of
// scope — the test supplies fake review-agent bodies (a shell script that
// exits 0) so the orchestration contract is exercised without paying for
// real LLM calls.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// ReviewAgentNames is the canonical set of review-agent role names spawned
// by `iris review`. Order is stable so callers (and tests) can iterate
// deterministically.
var ReviewAgentNames = []string{
	"review-goal",
	"review-code",
	"review-security",
	"review-qa",
	"review-context",
}

// ReviewSpawnConfig holds the parameters for spawning one review group.
type ReviewSpawnConfig struct {
	// Database is the open iris DB.
	Database *db.DB
	// ParentSession is the session that invoked `iris review` (the worker
	// or coordinator whose work is being reviewed). The review-complete
	// notification is delivered to this session.
	ParentSession string
	// Worktree is the absolute path of the worktree the review agents
	// operate on (typically the parent session's worktree at the PR's HEAD).
	Worktree string
	// PRNumber is the PR being reviewed. Recorded on agent_status for
	// observability; not used by the orchestration logic itself.
	PRNumber int
	// SpawnAgent is called once per review-agent role. Implementations
	// should spawn a worker session for the named role and return its
	// session name and instance ID. The spawn implementation is left to
	// the caller because iris (cmd/iris) wires this to its full
	// SupervisorConfig while tests wire it to fake spawn helpers.
	//
	// SpawnAgent runs serially for the 5 review agents — the orchestration
	// surface only needs serial dispatch. If a future change wants
	// parallel spawn, this can be promoted to a goroutine pool.
	SpawnAgent func(ctx context.Context, role string) (sessionName, instanceID string, err error)
	// PollInterval is how often the watcher checks db.GroupCompleted.
	// 0 means use a sensible default (250ms — tight enough for tests, slow
	// enough not to thrash the DB).
	PollInterval time.Duration
}

// ReviewSpawnResult holds the outcome of SpawnReviewGroup.
type ReviewSpawnResult struct {
	// GroupID is the UUID minted for this review group.
	GroupID string
	// Members maps role → session_name for the spawned review agents.
	Members map[string]string
}

// SpawnReviewGroup spawns the 5 review agents under a shared group_id,
// tags each agent_status row with the group_id, and returns the
// ReviewSpawnResult so callers can subscribe to completion.
//
// SpawnReviewGroup is synchronous — it returns after all 5 spawns have
// been requested. Completion is observed separately via WatchReviewGroup
// (or db.GroupCompleted).
func SpawnReviewGroup(ctx context.Context, cfg ReviewSpawnConfig) (*ReviewSpawnResult, error) {
	if cfg.Database == nil {
		return nil, fmt.Errorf("iris review: Database is required")
	}
	if cfg.ParentSession == "" {
		return nil, fmt.Errorf("iris review: ParentSession is required")
	}
	if cfg.SpawnAgent == nil {
		return nil, fmt.Errorf("iris review: SpawnAgent is required")
	}

	groupID, err := cfg.Database.RegisterGroup(cfg.ParentSession)
	if err != nil {
		return nil, fmt.Errorf("iris review: register group: %w", err)
	}

	res := &ReviewSpawnResult{
		GroupID: groupID,
		Members: make(map[string]string, len(ReviewAgentNames)),
	}

	for _, role := range ReviewAgentNames {
		sessionName, _, spawnErr := cfg.SpawnAgent(ctx, role)
		if spawnErr != nil {
			return res, fmt.Errorf("iris review: spawn %s: %w", role, spawnErr)
		}
		// Tag the spawned session's agent_status row with the group_id so
		// db.GroupCompleted can find it. agent_status is upserted by the
		// harness on hello, so we update it here directly via SQL.
		if err := updateGroupID(cfg.Database, sessionName, groupID); err != nil {
			return res, fmt.Errorf("iris review: set group_id for %s: %w", role, err)
		}
		res.Members[role] = sessionName
	}

	return res, nil
}

// WatchReviewGroup polls db.GroupCompleted until the group is complete
// (every member is terminal) or ctx is cancelled. When complete, onComplete
// is invoked exactly once with the per-member results.
//
// Callers typically launch WatchReviewGroup in a goroutine after
// SpawnReviewGroup returns; the goroutine is the analogue of prism's
// in-sidecar review monitor.
func WatchReviewGroup(
	ctx context.Context,
	database *db.DB,
	groupID string,
	pollInterval time.Duration,
	onComplete func(groupID string, results map[string]db.GroupMemberResult),
) error {
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			done, err := database.GroupCompleted(groupID)
			if err != nil {
				return fmt.Errorf("iris review: group completed check: %w", err)
			}
			if !done {
				continue
			}
			results, err := database.GroupResults(groupID)
			if err != nil {
				return fmt.Errorf("iris review: group results: %w", err)
			}
			if onComplete != nil {
				onComplete(groupID, results)
			}
			return nil
		}
	}
}

// updateGroupID sets the group_id column on agent_status for a session.
// Delegates to db.SetGroupID. Wrapped in a thin helper so the iris review
// orchestration can be retargeted at a future iris-native group store
// without touching call sites.
func updateGroupID(database *db.DB, sessionName, groupID string) error {
	return database.SetGroupID(sessionName, groupID)
}

// reviewCompleteOnceGuard wraps onComplete with a sync.Once so the
// review-complete callback fires exactly once even if WatchReviewGroup is
// re-entered (defensive — the watcher returns after one fire, but this
// protects callers that intentionally double-watch).
type reviewCompleteOnceGuard struct {
	once sync.Once
	fn   func(string, map[string]db.GroupMemberResult)
}

func (g *reviewCompleteOnceGuard) call(groupID string, results map[string]db.GroupMemberResult) {
	g.once.Do(func() { g.fn(groupID, results) })
}
