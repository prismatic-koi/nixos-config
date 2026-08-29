package dashboard

// export_test.go exposes package-private review-summary helpers to the
// dashboard_test package, so review_summary_test.go can assert the
// exact codepoint and column width each verdict renders without duplicating
// the mapping in the test file.

// LetterForVerdictForTest exposes letterForVerdict for tests.
func LetterForVerdictForTest(v string) string {
	return letterForVerdict(v)
}

// ColorForVerdictForTest exposes colorForVerdict for tests.
func ColorForVerdictForTest(v string) string {
	return colorForVerdict(v)
}

// RenderIconCellForTest exposes renderIconCell for tests.
func RenderIconCellForTest(v string) string {
	return renderIconCell(v)
}

// ReviewSummaryCompactWidthForTest exposes reviewSummaryCompactWidth for tests.
func ReviewSummaryCompactWidthForTest(s []ReviewChildSummary) int {
	return reviewSummaryCompactWidth(s)
}

// PlainSummaryForBudgetForTest exposes plainSummaryForBudget for tests.
func PlainSummaryForBudgetForTest(s []ReviewChildSummary, mode int) string {
	return plainSummaryForBudget(s, summaryMode(mode))
}

// ReviewSummaryLabelsWidthForTest exposes reviewSummaryLabelsWidth for tests.
func ReviewSummaryLabelsWidthForTest(s []ReviewChildSummary) int {
	return reviewSummaryLabelsWidth(s)
}

// RenderReviewSummaryForTest wraps RenderReviewSummary, returning the mode
// as a plain int so callers in dashboard_test don't need to name the
// unexported summaryMode type.
func RenderReviewSummaryForTest(s []ReviewChildSummary, budget int) (string, int, int) {
	rendered, width, mode := RenderReviewSummary(s, budget)
	return rendered, width, int(mode)
}

// Exported summaryMode values for tests, mirroring the unexported constants.
const (
	SummaryNoneForTest    = int(summaryNone)
	SummaryCompactForTest = int(summaryCompact)
	SummaryFullForTest    = int(summaryFull)
)
