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
	"strings"

	"github.com/prismatic-koi/prism/internal/session"
)

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

// extractTag extracts the inner text of the first occurrence of <tag>…</tag>
// (case-insensitive) from s. Returns ("", false) when the tag is absent.
func extractTag(s, tag string) (string, bool) {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	lower := strings.ToLower(s)
	lopen := strings.ToLower(open)
	lclose := strings.ToLower(close)

	start := strings.Index(lower, lopen)
	if start < 0 {
		return "", false
	}
	inner := start + len(open)
	end := strings.Index(lower[inner:], lclose)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(s[inner : inner+end]), true
}

// FormatResults formats the aggregated results as a human-readable report.
// It always includes a one-line-per-agent summary header, followed by a
// structured per-agent section containing only: verdict, <summary> content,
// and <blocking_issues> content extracted from the agent's raw output.
//
// The full per-agent monologue is not included. No file is written to /tmp.
//
// The sizeBudget and round parameters are retained in the signature for
// call-site compatibility but are no longer used.
//
// Returns the formatted string and a boolean indicating whether all passed.
func FormatResults(results []AgentResult, prNumber string, round int, sizeBudget int) (string, bool) {
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

		// ── Structured per-agent findings ────────────────────────────────
		findings.WriteString(fmt.Sprintf("\n### %s\n\n", r.Agent.Name))

		// Verdict line.
		if r.Passed {
			findings.WriteString("**Verdict:** PASS\n\n")
		} else if r.IsError {
			findings.WriteString("**Verdict:** ERROR\n\n")
			// For error results surface the full output (it is already a
			// short structured error message, not a monologue).
			if r.Output != "" {
				findings.WriteString(r.Output)
				if !strings.HasSuffix(r.Output, "\n") {
					findings.WriteString("\n")
				}
			}
			continue
		} else {
			findings.WriteString("**Verdict:** FAIL\n\n")
		}

		// Summary.
		if summary, ok := extractTag(r.Output, "summary"); ok {
			findings.WriteString("**Summary:** ")
			findings.WriteString(summary)
			if !strings.HasSuffix(summary, "\n") {
				findings.WriteString("\n")
			}
			findings.WriteString("\n")
		}

		// Blocking issues — only include on FAIL.
		if !r.Passed {
			if blocking, ok := extractTag(r.Output, "blocking_issues"); ok {
				findings.WriteString("**Blocking issues:**\n")
				findings.WriteString(blocking)
				if !strings.HasSuffix(blocking, "\n") {
					findings.WriteString("\n")
				}
				findings.WriteString("\n")
			} else {
				findings.WriteString("**Blocking issues:** (none found in agent output)\n\n")
			}
		}
	}

	if !allPassed {
		header.WriteString(fmt.Sprintf("\n%d agent(s) failed. Retry: prism review %s --only %s\n",
			len(failed), prNumber, strings.Join(failed, ",")))
	}

	var result strings.Builder
	result.WriteString(header.String())
	result.WriteString("\n## Per-agent findings\n")
	result.WriteString(findings.String())
	return result.String(), allPassed
}
