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
func BuildReviewPromptForTest(prNumber string, prCtx *PRContext) string {
	return buildReviewPrompt(prNumber, prCtx)
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
func DiffFilePathForTest(prNumber string, round int) string {
	return diffFilePath(prNumber, round)
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
