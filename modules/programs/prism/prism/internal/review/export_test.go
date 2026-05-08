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
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

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
func BuildDeliveryMessageForTest(prNumber string, round int, formattedResults string, allPassed bool, groupData map[string]db.GroupMemberResult, agentSessions []string) string {
	return buildDeliveryMessage(prNumber, round, formattedResults, allPassed, groupData, agentSessions)
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

// CurrentCycleProducedVerdictsForTest is an exported wrapper around
// currentCycleProducedVerdicts for use in external test packages (#1512).
func CurrentCycleProducedVerdictsForTest(groupData map[string]db.GroupMemberResult) bool {
	return currentCycleProducedVerdicts(groupData)
}
