package review

// export_test.go — test-only export shims for unexported functions.
//
// These wrappers make unexported functions accessible to external test packages
// (package review_test) without expanding the production API surface. This is
// the standard Go pattern for white-box testing via black-box test files.
//
// All functions here are compiled only when running tests (the _test.go suffix
// ensures they are excluded from production builds).

import (
	"context"
	"sort"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/session"
)

// RunGHForTest is an exported wrapper around runGH for use in timeout tests.
func RunGHForTest(args ...string) (string, error) {
	return runGH(args...)
}

// SanitisedGHEnvForTest exposes sanitisedGHEnv for external test coverage of
// the $(-literal guard added in issue #2348.
func SanitisedGHEnvForTest(env []string) []string {
	return sanitisedGHEnv(env)
}

// RunGitInWorktreeForTest is an exported wrapper around runGitInWorktree for
// use in timeout tests.
func RunGitInWorktreeForTest(worktree string, args ...string) string {
	return runGitInWorktree(worktree, args...)
}

// GHTimeoutForTest exposes ghTimeout for assertions in tests.
const GHTimeoutForTest = ghTimeout

// GitWorktreeTimeoutForTest exposes gitWorktreeTimeout for assertions.
const GitWorktreeTimeoutForTest = gitWorktreeTimeout

// BuildReviewPromptForTest is an exported wrapper around buildReviewPrompt for
// use in external test packages. It allows tests to verify prompt content,
// section ordering, and fallback behaviour without needing a live tmux/DB.
// role is the agent role name (e.g. "review-goal"); pass "" to exercise
// the missing-file edge case without a pre-existing definition file.
func BuildReviewPromptForTest(prNumber string, prCtx *PRContext, role ...string) string {
	r := ""
	if len(role) > 0 {
		r = role[0]
	}
	return buildReviewPrompt(prNumber, prCtx, r)
}

// TruncateDiffForTest is an exported wrapper around truncateDiff for unit tests.
func TruncateDiffForTest(diff string, maxBytes, maxLines int) (string, bool) {
	return truncateDiff(diff, maxBytes, maxLines)
}

// ParseLinkedIssuesForTest is an exported wrapper around parseLinkedIssues for
// use in external test packages.
func ParseLinkedIssuesForTest(body string) []string {
	return parseLinkedIssues(body)
}

// DiffFilePathForTest is an exported wrapper around diffFilePath for tests.
// stateDir mirrors the StateDir field of FetchPRContextOpts: when non-empty the
// diff file lands in the provided directory; when empty it falls back to /tmp
// (matching the backward-compat path for host-mode agents).
func DiffFilePathForTest(stateDir, prNumber string, round int) string {
	return diffFilePath(stateDir, prNumber, round)
}

// BuildAsyncAckForTest is an exported wrapper around buildAsyncAck for use
// in external tests. Mirrors the production signature so AC-5 assertions
// can verify the partial-success summary text without spinning up a real
// review run.
func BuildAsyncAckForTest(prNumber string, round int, groupID string, sessionNames []string, failures [][2]string, workerSession string) string {
	return buildAsyncAck(prNumber, round, groupID, sessionNames, failures, workerSession)
}

// PollAgentsForTest is an exported wrapper around pollAgents for use in tests.
// It accepts pre-seeded DB rows and returns both results and the progress lines
// emitted via onProgress.
//
// groupID is the session_groups.group_id for the poll loop's GroupCompleted
// termination check. When empty, the caller must ensure the group has been
// registered via db.RegisterGroup before calling this function — passing ""
// will cause GroupCompleted to return true immediately (zero members).
func PollAgentsForTest(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration, spawnTimes []time.Time, onProgress func(string), groupID string) ([]AgentResult, error) {
	return pollAgents(ctx, d, agents, agentSessions, timeout, spawnTimes, onProgress, groupID)
}

// BuildDeliveryMessageForTest is an exported wrapper around buildDeliveryMessage
// for use in external test packages. Allows tests to verify the header text and
// no-start error signalling without spinning up a real monitor loop (#1222).
// The round is classified from groupData with no ended-row detail, which is
// the degraded shape a caller without a DB handle sees (#2573).
func BuildDeliveryMessageForTest(prNumber string, round int, formattedResults string, allPassed bool, groupData map[string]db.GroupMemberResult, agentSessions []string) string {
	return BuildDeliveryMessageWithEndedForTest(prNumber, round, formattedResults, allPassed, groupData, agentSessions, nil)
}

// BuildDeliveryMessageWithEndedForTest is the #2573 variant: it also supplies
// the group's closed (ended_at set) agent_status rows, so tests can assert the
// reaped-session reason text.
func BuildDeliveryMessageWithEndedForTest(prNumber string, round int, formattedResults string, allPassed bool, groupData map[string]db.GroupMemberResult, agentSessions []string, endedRows map[string]db.Status) string {
	return BuildDeliveryMessageWithCausesForTest(prNumber, round, formattedResults, allPassed, groupData, agentSessions, endedRows, nil)
}

// BuildDeliveryMessageWithCausesForTest is the #2613 variant: it also supplies
// the close cause recorded for each closed row, so tests can assert that the
// rendered report names one cause rather than a disjunction.
func BuildDeliveryMessageWithCausesForTest(prNumber string, round int, formattedResults string, allPassed bool, groupData map[string]db.GroupMemberResult, agentSessions []string, endedRows map[string]db.Status, causes map[string]db.SessionEndCause) string {
	status := ClassifyRoundWithCauses(AgentsFromSessionsForTest(agentSessions), agentSessions, groupData, endedRows, causes)
	return buildDeliveryMessage(prNumber, round, formattedResults, allPassed, status)
}

// CleanupAgentSessionForTest is an exported wrapper around cleanupAgentSession
// so tests can pin that the reap record is guarded on ended_at (#2613).
func CleanupAgentSessionForTest(d *db.DB, agentSession string, cause db.SessionReapCause, detail ...string) {
	cleanupAgentSession(d, agentSession, cause, detail...)
}

// ExpectedRoundSetForTest is an exported wrapper around expectedRoundSet so
// tests can pin that a round's expected set is never filtered by spawn
// failures (#2613).
func ExpectedRoundSetForTest(agents []Agent, agentSessions []string, spawnErr []error) ([]Agent, []string) {
	return expectedRoundSet(agents, agentSessions, spawnErr)
}

// AgentsFromSessionsForTest builds the parallel Agent slice ClassifyRound
// expects from a list of review-agent session names.
func AgentsFromSessionsForTest(agentSessions []string) []Agent {
	agents := make([]Agent, 0, len(agentSessions))
	for _, sess := range agentSessions {
		agents = append(agents, Agent{Name: agentNameFromSession(sess)})
	}
	return agents
}

// EndedRowsFromForTest is an exported wrapper around endedRowsFrom (#2573).
func EndedRowsFromForTest(members []db.Status) map[string]db.Status {
	return endedRowsFrom(members)
}

// SanitizeSpawnErrorForTest is an exported wrapper around sanitizeSpawnError
// for use in external test packages. Allows tests to verify that the
// per-agent error message never includes PRISM_INITIAL_PROMPT or oversized
// argv content (issue #1194).
func SanitizeSpawnErrorForTest(prNumber, agentName string, err error) string {
	return sanitizeSpawnError(prNumber, agentName, err)
}

// TruncateProgressMsgForTest is an exported wrapper around truncateProgressMsg
// for use in external test packages.
func TruncateProgressMsgForTest(prNumber, agentName, msg string) string {
	return truncateProgressMsg(prNumber, agentName, msg)
}

// MaxProgressMsgBytesForTest exposes maxProgressMsgBytes for assertions.
const MaxProgressMsgBytesForTest = maxProgressMsgBytes

// BuildLoopLimitFooterForTest is an exported wrapper around buildLoopLimitFooter
// for use in external test packages (#1512).
func BuildLoopLimitFooterForTest(cycles int, prNumber string) string {
	return buildLoopLimitFooter(cycles, prNumber)
}

// ReviewerSpawnInputForTest mirrors the unexported reviewerSpawnInput
// struct (spawn_opts.go) so external tests in package review_test can
// drive newReviewerSpawnOpts via NewReviewerSpawnOptsForTest. Fields
// match 1:1 with the production struct; see spawn_opts.go for the
// field-level rationale.
type ReviewerSpawnInputForTest struct {
	AgentName          string
	AgentSession       string
	Prompt             string
	Repo               string
	Worktree           string
	PromptTemplateHash string
	IsolationMode      string
	PluginHostPath     string
	GroupID            string
	HarnessName        string
	HarnessHandle      harness.Harness
	ModelsByRole       map[string]string
	PIExtensionDir     string
	ProfileName        string
}

// NewReviewerSpawnOptsForTest is an exported wrapper around
// newReviewerSpawnOpts (spawn_opts.go) for use in external test
// packages verifying the #2097 ProfileName-inheritance wiring.
//
// Tests construct a ReviewerSpawnInputForTest, pass it through this
// shim, and assert on the returned session.SpawnOpts.ProfileName.
// The shim mirrors the production call signature so a regression in
// the field mapping (e.g. accidentally dropping ProfileName from the
// returned literal) is caught immediately.
func NewReviewerSpawnOptsForTest(in ReviewerSpawnInputForTest) session.SpawnOpts {
	return newReviewerSpawnOpts(reviewerSpawnInput{
		AgentName:          in.AgentName,
		AgentSession:       in.AgentSession,
		Prompt:             in.Prompt,
		Repo:               in.Repo,
		Worktree:           in.Worktree,
		PromptTemplateHash: in.PromptTemplateHash,
		IsolationMode:      in.IsolationMode,
		PluginHostPath:     in.PluginHostPath,
		GroupID:            in.GroupID,
		HarnessName:        in.HarnessName,
		HarnessHandle:      in.HarnessHandle,
		ModelsByRole:       in.ModelsByRole,
		PIExtensionDir:     in.PIExtensionDir,
		ProfileName:        in.ProfileName,
	})
}

// CurrentCycleProducedVerdictsForTest is an exported wrapper around
// cycleProducedVerdicts for use in external test packages (#1512). The
// expected member list degrades to the groupData keys, which is the
// pre-#2573 shape: it can only see the members that came back.
func CurrentCycleProducedVerdictsForTest(groupData map[string]db.GroupMemberResult) bool {
	names := make([]string, 0, len(groupData))
	for name := range groupData {
		names = append(names, name)
	}
	sort.Strings(names)
	return cycleProducedVerdicts(names, groupData, nil)
}

// CycleProducedVerdictsForTest is the #2573 variant: the caller supplies the
// authoritative expected-member list, so a member absent from groupData is
// visible to the predicate.
func CycleProducedVerdictsForTest(expectedSessions []string, groupData map[string]db.GroupMemberResult, endedRows map[string]db.Status) bool {
	return cycleProducedVerdicts(expectedSessions, groupData, endedRows)
}

// ForceTerminateStuckMembersForTest is an exported wrapper around
// forceTerminateStuckMembers for use in external test packages (#1709, #2729).
// It returns the number of live members the sweep spared.
func ForceTerminateStuckMembersForTest(d *db.DB, agentSessions []string, groupDeadline time.Duration) int {
	return forceTerminateStuckMembers(d, agentSessions, groupDeadline)
}

// ReviewAgentActivityWindowForTest exposes the activity window the sweep uses
// so the guard test can assert it equals the sidecar watchdog timeout (#2729).
func ReviewAgentActivityWindowForTest() time.Duration {
	return reviewAgentActivityWindow
}

// ReapSideEffectsForTest replaces the reaper's three process-side effects
// (issue #2649) and returns a restore function the caller must defer.
//
// Tests must stub all three. The suite is isolated from host state
// (`internal/sidecar` test-isolation convention): a real tmux.KillSession, a
// real session.KillSidecar, or a real container EnsureRemoved would reach the
// developer's tmux server, signal a live sidecar process, and delete files
// under the real state root. Pass nil for a hook the test does not assert on
// and it becomes a no-op.
func ReapSideEffectsForTest(killTmux func(string), killSidecar func(string), removeContainer func(sessionName, isolationMode string)) func() {
	prevTmux, prevSidecar, prevContainer := reapKillTmuxSession, reapKillSidecar, reapRemoveContainer
	noopOne := func(string) {}
	noopTwo := func(string, string) {}
	if killTmux == nil {
		killTmux = noopOne
	}
	if killSidecar == nil {
		killSidecar = noopOne
	}
	if removeContainer == nil {
		removeContainer = noopTwo
	}
	reapKillTmuxSession, reapKillSidecar, reapRemoveContainer = killTmux, killSidecar, removeContainer
	return func() {
		reapKillTmuxSession, reapKillSidecar, reapRemoveContainer = prevTmux, prevSidecar, prevContainer
	}
}

// ReapClockForTest replaces the ReapGroupAfterGrace clock pair (issue #2649)
// and returns a restore function. Without it, a test that exercises the
// post-delivery reap would block for ReapGracePeriod.
//
// Both halves must be supplied together. A stubbed sleep with a real
// time.Now() would return instantly and then read a clock that has NOT
// advanced, so the sweep would find nothing and the test would fail for a
// reason that says nothing about production. Pass a fake clock that the sleep
// stub advances.
func ReapClockForTest(sleep func(time.Duration), now func() time.Time) func() {
	prevSleep, prevNow := reapSleep, reapNow
	reapSleep, reapNow = sleep, now
	return func() { reapSleep, reapNow = prevSleep, prevNow }
}

// PersistReviewOutcomeForTest is an exported wrapper around persistReviewOutcome
// for use in external test packages (#2110). It allows tests to verify the
// review-complete write trigger — verdict + pass/fail counts persisted on the
// worker's spawn_outcome row — without standing up an entire MonitorFunc poll
// loop.
func PersistReviewOutcomeForTest(d *db.DB, workerSession string, results []AgentResult, allPassed bool) {
	persistReviewOutcome(d, workerSession, results, allPassed)
}
