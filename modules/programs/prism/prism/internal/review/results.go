package review

// results.go — result formatting and assessment.
//
// This file contains the pure post-processing functions that turn raw polling
// outcomes into human-readable reports. None of the code here has any
// dependency on DB, tmux, or spawn machinery.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/prismatic-koi/prism/internal/session"
)

// DefaultFindingsSizeBudget is the default maximum inline size (in bytes) for
// full per-agent findings. When the total findings exceed this budget, they are
// written to a file and a pointer is included instead.
const DefaultFindingsSizeBudget = 20 * 1024 // 20 KB

// FindingsSizeBudgetEnvVar is the environment variable that overrides the
// default findings size budget. Set to an integer byte count (e.g. "10240").
const FindingsSizeBudgetEnvVar = "PRISM_REVIEW_SIZE_BUDGET"

// VerdictKind describes what kind of verdict marker was found by AssessPassed.
type VerdictKind int

const (
	VerdictNone VerdictKind = iota // no recognised verdict marker
	VerdictPass                    // explicit PASS marker
	VerdictFail                    // explicit FAIL marker
)

// extractAssistantText parses the text field from a msg_assistant payload.
func extractAssistantText(payload string) string {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err == nil && p.Text != "" {
		return p.Text
	}
	return payload
}

// AssessPassed determines whether a review agent passed by requiring an
// explicit positive verdict marker. It returns (passed bool, kind VerdictKind).
//
// Layer 1 defence: default to fail, not pass. An agent's output must contain
// an explicit <verdict>PASS</verdict> marker to be classified as passed. Any
// other text — benign partial output, startup messages, empty strings — returns
// (false, VerdictNone) so the caller can surface it for human inspection.
//
// Recognised markers (case-insensitive):
//   - <verdict>PASS</verdict>  → (true,  VerdictPass)
//   - <verdict>FAIL</verdict>  → (false, VerdictFail)
//   - anything else            → (false, VerdictNone)
//
// Exported so it can be tested directly without needing a live DB.
func AssessPassed(text string) (bool, VerdictKind) {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<verdict>pass</verdict>") {
		return true, VerdictPass
	}
	if strings.Contains(lower, "<verdict>fail</verdict>") {
		return false, VerdictFail
	}
	return false, VerdictNone
}

// failureReason returns the user-facing reason string for a spawn / readiness
// failure. For *session.ReadinessTimeoutError it produces "not ready within
// <timeout>" (matching the AC-5 example text exactly); other errors are
// passed through verbatim.
func failureReason(err error) string {
	if err == nil {
		return ""
	}
	if session.IsReadinessTimeout(err) {
		// session.ReadinessTimeoutError already renders as "not ready
		// within <timeout>" via its Error() method. Surfacing the inner
		// message keeps the Ack readable without redundant prefixes from
		// the gate-side wrapping.
		var rte *session.ReadinessTimeoutError
		if errors.As(err, &rte) {
			return rte.Error()
		}
	}
	return err.Error()
}

// sanitisePRNumber returns a version of prNumber safe for use in a filename by
// retaining only alphanumeric characters and hyphens. This prevents path
// traversal when prNumber comes from user-supplied CLI input.
func sanitisePRNumber(prNumber string) string {
	var b strings.Builder
	for _, r := range prNumber {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	safe := b.String()
	if safe == "" {
		safe = "unknown"
	}
	return safe
}

// FindingsFilePath returns the path where full findings are written when the
// size budget is exceeded. prNumber is sanitised before use to prevent path
// traversal; round identifies the review round.
func FindingsFilePath(prNumber string, round int) string {
	return fmt.Sprintf("/tmp/prism-review-%s-round-%d.md", sanitisePRNumber(prNumber), round)
}

// resolveSizeBudget returns the effective size budget. sizeBudget ≤ 0 means use
// the default (or the env-var override, if set).
func resolveSizeBudget(sizeBudget int) int {
	if sizeBudget > 0 {
		return sizeBudget
	}
	if v := os.Getenv(FindingsSizeBudgetEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultFindingsSizeBudget
}

// FormatResults formats the aggregated results as a human-readable report.
// It always includes a one-line-per-agent summary header for terse scanning,
// followed by full per-agent findings (verdict, summary, blocking issues, and
// non-blocking observations).
//
// When total output exceeds sizeBudget bytes (default 20 KB when ≤ 0), the
// full findings are written to /tmp/prism-review-<pr>-round-<round>.md and the
// inline output contains only summaries + blocking issues with a file pointer.
// Pass round=0 to omit the file-overflow path (used in unit tests).
//
// Returns the formatted string and a boolean indicating whether all passed.
func FormatResults(results []AgentResult, prNumber string, round int, sizeBudget int) (string, bool) {
	budget := resolveSizeBudget(sizeBudget)

	var header strings.Builder
	var findings strings.Builder
	allPassed := true
	var failed []string

	for _, r := range results {
		// ── Summary header line ──────────────────────────────────────────
		if r.Passed {
			header.WriteString(fmt.Sprintf("✓ %-20s passed\n", r.Agent.Name))
		} else {
			allPassed = false
			failed = append(failed, r.Agent.Name)
			if r.IsError {
				header.WriteString(fmt.Sprintf("✗ %-20s error\n", r.Agent.Name))
			} else {
				header.WriteString(fmt.Sprintf("✗ %-20s failed\n", r.Agent.Name))
			}
		}

		// ── Full per-agent findings ──────────────────────────────────────
		findings.WriteString(fmt.Sprintf("\n### %s\n\n", r.Agent.Name))
		if r.Passed {
			findings.WriteString("**Verdict:** PASS\n\n")
		} else if r.IsError {
			findings.WriteString("**Verdict:** ERROR\n\n")
		} else {
			findings.WriteString("**Verdict:** FAIL\n\n")
		}
		if r.Output != "" {
			findings.WriteString(r.Output)
			if !strings.HasSuffix(r.Output, "\n") {
				findings.WriteString("\n")
			}
		}
	}

	if !allPassed {
		header.WriteString(fmt.Sprintf("\n%d agent(s) failed. Retry: prism review %s --only %s\n",
			len(failed), prNumber, strings.Join(failed, ",")))
	}

	findingsStr := findings.String()
	headerStr := header.String()

	// ── Size budget check ────────────────────────────────────────────────
	totalSize := len(headerStr) + len(findingsStr)
	if round > 0 && totalSize > budget {
		// Overflow: write full findings to file and inline a pointer.
		filePath := FindingsFilePath(prNumber, round)
		fullContent := headerStr + "\n## Per-agent findings\n" + findingsStr
		writeErr := os.WriteFile(filePath, []byte(fullContent), 0o644)

		var result strings.Builder
		result.WriteString(headerStr)
		if writeErr == nil {
			result.WriteString(fmt.Sprintf(
				"\nFull findings: `%s` (%d KB) — read with `cat` or `rg` as needed.\n",
				filePath, totalSize/1024,
			))
		} else {
			// File write failed — inline the findings anyway rather than losing them.
			result.WriteString("\n## Per-agent findings\n")
			result.WriteString(findingsStr)
		}
		return result.String(), allPassed
	}

	// Findings fit within budget — inline them.
	var result strings.Builder
	result.WriteString(headerStr)
	result.WriteString("\n## Per-agent findings\n")
	result.WriteString(findingsStr)
	return result.String(), allPassed
}
