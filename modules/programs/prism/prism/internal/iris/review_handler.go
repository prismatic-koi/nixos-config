package iris

// review_handler.go — daemon-side orchestration for `iris review <pr>`.
//
// This is the daemon component sitting between the client IPC wire frame
// (ClientReviewSpawnFrame, handled in client_socket.go::handleReviewSpawn)
// and the lower-level D-10 internals in review.go (SpawnReviewGroup,
// WatchReviewGroup).
//
// Responsibilities:
//
//  1. Validate the request: known agent names, no in-progress group for the
//     same parent, parent session exists.
//  2. Compute the next round number for the parent (using the shared
//     review.NextRoundNumber helper, derived from the parent's prior
//     ~review-N-<agent> rows).
//  3. Construct a SpawnAgent closure that asks the daemon to spawn each
//     review-agent session via the existing session-spawn machinery.
//  4. Drive SpawnReviewGroup → WatchReviewGroup, delivering a single
//     review-complete prompt to the parent via the daemon's deliverPrompt
//     hook when the group reaches a terminal state.
//
// # Exactly-once delivery
//
// The watcher path is guarded by reviewCompleteOnceGuard (sync.Once-wrapped
// callback) so a defensive double-fire of WatchReviewGroup never produces
// two deliveries. The DeliveryID from the wire frame is forwarded to the
// receiving session as part of the prompt body — when the parent is a pi
// session, the sidecar at the far end dedupes by delivery_id (issue #1695).
// Iris-native parents (this daemon) receive the prompt directly via
// Supervisor.SendRPC; the sync.Once is the canonical exactly-once boundary
// for that path.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// defaultPerAgentTimeout is the watch budget used when the client did not
// supply a timeout. Matches prism's default and the issue's documented
// per-agent default.
const defaultPerAgentTimeout = 10 * time.Minute

// ReviewSpawnDeps is the dependency bundle the daemon hands to
// NewReviewSpawnHandler. Each function corresponds to one capability the
// orchestrator needs from the surrounding daemon. Separating these from
// the global daemon state keeps the orchestrator testable in isolation —
// the parity integration test wires fakes for each.
type ReviewSpawnDeps struct {
	// Database is the open iris DB. Used for group registration, status
	// lookups, round-number derivation, and the watcher's poll loop.
	Database *db.DB
	// SpawnSession asks the daemon to spawn a single review-agent session
	// with the given session name, worktree, and role. The orchestrator
	// uses this in a loop (one call per agent) instead of the bare
	// session_spawn path because review-agent names follow the
	// `<parent>~review-N-<agent>` convention, not the daemon's default
	// GenerateSessionName scheme.
	SpawnSession func(ctx context.Context, sessionName, worktree, role string) (*Supervisor, error)
	// DeliverPrompt delivers the final review-complete prompt to the
	// parent session. Same signature as the daemon's prompt_deliver hook.
	DeliverPrompt func(ctx context.Context, name, text, deliverAs string, images []string) error
	// ParentWorktree returns the worktree of the named (parent) session
	// so the orchestrator can stamp it onto each spawned review-agent
	// session. Implementations typically read the in-memory supervisor
	// map; a DB fallback is acceptable.
	ParentWorktree func(parent string) (string, error)
	// PollInterval is the watcher's poll cadence. 0 = default (250ms).
	PollInterval time.Duration
}

// NewReviewSpawnHandler returns a function suitable for
// ClientSocketConfig.SpawnReviewGroup. It captures deps and produces a
// closure that the client-socket layer invokes when a review_spawn frame
// arrives.
func NewReviewSpawnHandler(deps ReviewSpawnDeps) func(ctx context.Context, req ClientReviewSpawnFrame) (*DaemonReviewSpawnedFrame, error) {
	return func(ctx context.Context, req ClientReviewSpawnFrame) (*DaemonReviewSpawnedFrame, error) {
		return handleReviewSpawnRequest(ctx, deps, req)
	}
}

// handleReviewSpawnRequest is the body of the orchestrator. Split from the
// closure so it can be unit-tested without a real daemon.
func handleReviewSpawnRequest(ctx context.Context, deps ReviewSpawnDeps, req ClientReviewSpawnFrame) (*DaemonReviewSpawnedFrame, error) {
	if deps.Database == nil {
		return nil, fmt.Errorf("iris review: database not configured")
	}
	if deps.SpawnSession == nil {
		return nil, fmt.Errorf("iris review: spawn-session not configured")
	}
	if deps.DeliverPrompt == nil {
		return nil, fmt.Errorf("iris review: deliver-prompt not configured")
	}
	if deps.ParentWorktree == nil {
		return nil, fmt.Errorf("iris review: parent-worktree resolver not configured")
	}

	// Resolve the agent set.
	agents, err := resolveAgentSet(req.AgentNames)
	if err != nil {
		return nil, err
	}

	// In-progress group rejection (AC: "round N is already in progress").
	if roundNum, present, err := inProgressReviewRound(deps.Database, req.Parent); err != nil {
		return nil, fmt.Errorf("iris review: check in-progress: %w", err)
	} else if present {
		return nil, fmt.Errorf(
			"iris review: round %d is already in progress for this PR (parent=%s)\n"+
				"hint: wait for the round to complete, or cancel it via `iris sessions list` + `iris kill <agent-session>`",
			roundNum, req.Parent,
		)
	}

	// Resolve the parent's worktree so the review agents reuse it (they
	// inspect the same HEAD the worker is on).
	worktree, err := deps.ParentWorktree(req.Parent)
	if err != nil {
		return nil, fmt.Errorf("iris review: resolve parent worktree: %w", err)
	}

	// Derive the next round number from the parent's prior review rows.
	// review.NextRoundNumber returns 1 when no prior rounds exist.
	round := review.NextRoundNumber(deps.Database, req.Parent)

	// Parse the per-agent timeout (used for log context; watcher itself
	// relies on the parent ctx + DB poll loop).
	timeout, err := parseReviewTimeout(req.Timeout)
	if err != nil {
		return nil, err
	}

	// Compose the SpawnAgent closure threaded to SpawnReviewGroup.
	spawnAgent := func(spawnCtx context.Context, role string) (string, string, error) {
		sessionName := fmt.Sprintf("%s~review-%d-%s", req.Parent, round, role)
		sup, err := deps.SpawnSession(spawnCtx, sessionName, worktree, role)
		if err != nil {
			return "", "", err
		}
		return sup.SessionRecord().SessionName, sup.InstanceID(), nil
	}

	// Drive the spawn. SpawnReviewGroup iterates the canonical
	// ReviewAgentNames, so if the caller restricted via --only we
	// substitute a filtered list by overriding via a one-shot variable
	// swap is intrusive; instead we use a localised slice and call
	// spawnAgent directly for the requested subset.
	groupID, members, err := spawnFilteredReviewGroup(ctx, deps.Database, req.Parent, worktree, agents, spawnAgent)
	if err != nil {
		return nil, fmt.Errorf("iris review: %w", err)
	}

	deliveryID := req.DeliveryID
	if deliveryID == "" {
		deliveryID = uuid.NewString()
	}

	// Launch the watcher. The watcher runs for the lifetime of the
	// daemon process (or until the group completes); we deliberately use
	// context.Background here, not the per-connection ctx, because the
	// CLI invocation that triggered the spawn will return immediately
	// after the ack and the connection will close.
	go runReviewWatcher(reviewWatcherConfig{
		Database:      deps.Database,
		GroupID:       groupID,
		Parent:        req.Parent,
		PRNumber:      req.PRNumber,
		Round:         round,
		Members:       members,
		AgentNames:    agentRoleList(agents),
		DeliverPrompt: deps.DeliverPrompt,
		DeliveryID:    deliveryID,
		PerAgentBudget: timeout,
		PollInterval:  deps.PollInterval,
	})

	// Build ack.
	ack := &DaemonReviewSpawnedFrame{
		Type:    DaemonFrameReviewSpawned,
		GroupID: groupID,
		Parent:  req.Parent,
		Round:   round,
		Members: make([]DaemonReviewGroupMember, 0, len(members)),
	}
	for _, role := range agentRoleList(agents) {
		ack.Members = append(ack.Members, DaemonReviewGroupMember{
			Agent:       role,
			SessionName: members[role],
		})
	}
	return ack, nil
}

// resolveAgentSet returns the canonical ordered list of agents to spawn.
// An empty input means "all 5 standard agents". A non-empty input is
// validated against ReviewAgentNames; unknown names produce a clear error
// listing the valid set.
func resolveAgentSet(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return append([]string(nil), ReviewAgentNames...), nil
	}
	known := make(map[string]bool, len(ReviewAgentNames))
	for _, r := range ReviewAgentNames {
		known[r] = true
	}
	var unknown []string
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !known[name] {
			unknown = append(unknown, name)
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf(
			"iris review: unknown agent name(s): %s — valid names: %s",
			strings.Join(unknown, ", "), strings.Join(ReviewAgentNames, ", "),
		)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"iris review: --only must name at least one of: %s",
			strings.Join(ReviewAgentNames, ", "),
		)
	}
	return out, nil
}

// parseReviewTimeout parses the wire-frame timeout (a Go duration string)
// and returns the resolved per-agent budget. Empty string yields the
// default. Invalid input yields an error.
func parseReviewTimeout(s string) (time.Duration, error) {
	if s == "" {
		return defaultPerAgentTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("iris review: invalid timeout %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("iris review: timeout must be positive (got %s)", s)
	}
	return d, nil
}

// inProgressReviewRound returns the (1-indexed) round number of any
// in-progress review group for the given parent. present=false when no
// group is in progress.
//
// We piggy-back on review.NextRoundNumber for the round-number derivation:
// the in-progress round is one less than NextRoundNumber when the prior
// round still has non-terminal members. Concretely, we walk the parent's
// session_groups rows and check each via db.GroupCompleted.
func inProgressReviewRound(d *db.DB, parent string) (round int, present bool, err error) {
	// HasReviewGroup is the cheap first check; if no group exists for the
	// parent at all, we can return immediately.
	has, hErr := d.HasReviewGroup(parent)
	if hErr != nil {
		return 0, false, hErr
	}
	if !has {
		return 0, false, nil
	}
	// Walk all groups and look for one that has not yet completed.
	parents, err := d.AllGroupParents()
	if err != nil {
		return 0, false, err
	}
	for groupID, p := range parents {
		if p != parent {
			continue
		}
		done, dErr := d.GroupCompleted(groupID)
		if dErr != nil {
			return 0, false, dErr
		}
		if !done {
			// Derive the round number from the members' session names.
			members, mErr := d.GroupMembersForGroup(groupID)
			if mErr != nil {
				return 0, false, mErr
			}
			r := roundFromMembers(parent, members)
			return r, true, nil
		}
	}
	return 0, false, nil
}

// roundFromMembers extracts the round number embedded in any one of the
// member session names, which follow the `<parent>~review-N-<agent>`
// convention. Returns 0 when no member matches (defensive — should not
// happen for a freshly-registered group).
func roundFromMembers(parent string, members []db.Status) int {
	prefix := parent + "~review-"
	for _, m := range members {
		if !strings.HasPrefix(m.SessionName, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(m.SessionName, prefix)
		// suffix is "N-<agent>"; we want N.
		dashIdx := strings.Index(suffix, "-")
		if dashIdx <= 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(suffix[:dashIdx], "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// spawnFilteredReviewGroup is a thin variant of SpawnReviewGroup that
// honours an arbitrary ordered agent list (instead of the package's
// canonical full set). It performs the same DB-stamping work
// (RegisterGroup → SpawnAgent → SetGroupID) for each agent in `agents`.
func spawnFilteredReviewGroup(
	ctx context.Context,
	d *db.DB,
	parent, worktree string,
	agents []string,
	spawnAgent func(ctx context.Context, role string) (sessionName, instanceID string, err error),
) (string, map[string]string, error) {
	if d == nil {
		return "", nil, fmt.Errorf("database is required")
	}
	if parent == "" {
		return "", nil, fmt.Errorf("parent session is required")
	}
	if spawnAgent == nil {
		return "", nil, fmt.Errorf("spawn-agent callback is required")
	}
	if len(agents) == 0 {
		return "", nil, fmt.Errorf("at least one agent is required")
	}
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		return "", nil, fmt.Errorf("register group: %w", err)
	}
	members := make(map[string]string, len(agents))
	for _, role := range agents {
		sessionName, _, spawnErr := spawnAgent(ctx, role)
		if spawnErr != nil {
			return groupID, members, fmt.Errorf("spawn %s: %w", role, spawnErr)
		}
		if err := d.SetGroupID(sessionName, groupID); err != nil {
			return groupID, members, fmt.Errorf("set group_id for %s: %w", role, err)
		}
		members[role] = sessionName
	}
	_ = worktree // currently unused at this layer; retained for symmetry with SpawnReviewGroup signature
	return groupID, members, nil
}

// agentRoleList returns agents in the canonical ReviewAgentNames order
// (subset-preserving), so the ack frame's Members ordering is stable.
func agentRoleList(agents []string) []string {
	want := make(map[string]bool, len(agents))
	for _, a := range agents {
		want[a] = true
	}
	out := make([]string, 0, len(agents))
	for _, canonical := range ReviewAgentNames {
		if want[canonical] {
			out = append(out, canonical)
		}
	}
	return out
}

// reviewWatcherConfig bundles the parameters runReviewWatcher needs. It is
// constructed by handleReviewSpawnRequest and consumed by a single
// goroutine; the struct is not safe for concurrent re-use.
type reviewWatcherConfig struct {
	Database       *db.DB
	GroupID        string
	Parent         string
	PRNumber       string
	Round          int
	Members        map[string]string // role → session_name
	AgentNames     []string          // ordered subset of ReviewAgentNames
	DeliverPrompt  func(ctx context.Context, name, text, deliverAs string, images []string) error
	DeliveryID     string
	PerAgentBudget time.Duration
	PollInterval   time.Duration
}

// runReviewWatcher launches a WatchReviewGroup poll loop and, on
// completion, delivers a single review-complete prompt to the parent
// session. The sync.Once-wrapped callback (reviewCompleteOnceGuard) ensures
// exactly-once delivery even if the watcher fires defensively twice.
func runReviewWatcher(cfg reviewWatcherConfig) {
	// Watcher ctx: bounded by 2x the per-agent budget × number of agents,
	// floored at the per-agent budget itself. The intent is that the
	// watcher does not hold a goroutine forever if all agents wedge.
	wallclock := cfg.PerAgentBudget
	if wallclock <= 0 {
		wallclock = defaultPerAgentTimeout
	}
	// Generous outer bound: 2x per-agent budget × member count. With the
	// 10-minute default and 5 agents this yields 100 minutes — well
	// beyond a healthy review cycle, but a hard cap if everything hangs.
	hardCap := wallclock * 2 * time.Duration(len(cfg.Members)+1)
	ctx, cancel := context.WithTimeout(context.Background(), hardCap)
	defer cancel()

	guard := &reviewCompleteOnceGuard{
		fn: func(groupID string, results map[string]db.GroupMemberResult) {
			text := buildReviewCompleteBody(cfg, results)
			err := cfg.DeliverPrompt(context.Background(), cfg.Parent, text, "followUp", nil)
			if err != nil {
				log.Printf("[iris review] deliver review-complete to %s: %v", cfg.Parent, err)
				return
			}
			log.Printf("[iris review] delivered review-complete to %s (group=%s, delivery_id=%s)", cfg.Parent, groupID, cfg.DeliveryID)
		},
	}

	if err := WatchReviewGroup(ctx, cfg.Database, cfg.GroupID, cfg.PollInterval, guard.call); err != nil {
		log.Printf("[iris review] watcher exited with error (group=%s): %v", cfg.GroupID, err)
	}
}

// buildReviewCompleteBody constructs the text body delivered to the parent
// when a review round completes. It mirrors prism's compact summary:
// header, per-agent verdict + last message, and a closing instruction.
//
// The delivery_id is embedded verbatim at the head of the body so that any
// receiver that surfaces the prompt to a human sees the same correlation
// ID the daemon used. This is informational; deduplication itself is
// enforced by the reviewCompleteOnceGuard at the call site.
func buildReviewCompleteBody(cfg reviewWatcherConfig, results map[string]db.GroupMemberResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Review complete: PR #%s (round %d)\n\n", cfg.PRNumber, cfg.Round)

	allPassed := true
	anyMissing := false
	for _, role := range cfg.AgentNames {
		sessionName := cfg.Members[role]
		r, ok := results[sessionName]
		if !ok {
			anyMissing = true
			fmt.Fprintf(&sb, "### %s — MISSING\nsession=%s (not present in group results)\n\n", role, sessionName)
			allPassed = false
			continue
		}
		verdict := "PASS"
		if r.State != "finished" || r.StartupError != "" {
			verdict = "FAIL"
			allPassed = false
		}
		fmt.Fprintf(&sb, "### %s — %s\nsession=%s state=%s\n", role, verdict, sessionName, r.State)
		if r.StartupError != "" {
			fmt.Fprintf(&sb, "startup_error: %s\n", r.StartupError)
		}
		if r.LastMessage != "" {
			fmt.Fprintf(&sb, "%s\n", r.LastMessage)
		}
		sb.WriteString("\n")
	}

	switch {
	case allPassed:
		sb.WriteString("**All review agents passed.** You may proceed with announcing completion.\n")
	case anyMissing:
		sb.WriteString("**One or more review agents are missing from the group results.** Re-run `iris review` or investigate the missing sessions.\n")
	default:
		sb.WriteString("**One or more review agents failed.** Fix the blocking issues and re-run `iris review`.\n")
	}

	fmt.Fprintf(&sb, "\n(group_id=%s, delivery_id=%s)\n", cfg.GroupID, cfg.DeliveryID)
	return sb.String()
}

// reviewWatcherOnceMu is a package-level mutex retained for future use if
// the watcher needs to coordinate across multiple groups (e.g. shared
// rate limiter on prompt delivery). Currently unused; kept so adding such
// coordination does not require a new export.
var reviewWatcherOnceMu sync.Mutex //nolint:unused

// ensureReviewWatcherMuUsed silences the unused-variable lint if the
// package is built with strict settings. The mutex is reserved for future
// coordination work and not strictly required today.
func ensureReviewWatcherMuUsed() { //nolint:unused
	reviewWatcherOnceMu.Lock()
	reviewWatcherOnceMu.Unlock()
}
