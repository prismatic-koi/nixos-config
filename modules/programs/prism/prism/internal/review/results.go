package review

// results.go — result formatting and assessment.
//
// This file contains the pure post-processing functions that turn raw polling
// outcomes into human-readable reports. It imports session to detect
// HostLaunchCmdTooLargeError (for spawn-failure sanitization); otherwise
// it has no dependency on DB, tmux, or runtime spawn machinery.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/prismatic-koi/prism/internal/session"
)

// maxProgressMsgBytes is the hard cap on any per-agent error message printed
// to stdout by prism review. Messages longer than this are truncated and a
// forensic path is appended. 4 KiB is large enough for any structured error
// but small enough to be safe for LLM context windows.
const maxProgressMsgBytes = 4 * 1024

// sanitizeSpawnError returns a short, safe error string for a per-agent spawn
// failure. It never includes the failed argv or env-injected payload
// (PRISM_INITIAL_PROMPT) in the returned string.
//
// Two tiers:
//  1. *session.HostLaunchCmdTooLargeError — always produces a ≤1 KB structured
//     message naming the agent, the failure category, the bound exceeded, and a
//     hint. This is the "command too long" case described in issue #1194.
//  2. All other errors — the raw error string is stripped of any
//     PRISM_INITIAL_PROMPT= content, then hard-capped at maxProgressMsgBytes
//     via truncateProgressMsg.
//
// prNumber is used to scope the forensic log path so concurrent review runs do
// not overwrite each other's error logs.
func sanitizeSpawnError(prNumber, agentName string, err error) string {
	if err == nil {
		return ""
	}

	// Tier 1: launch-command-too-large — structured short message.
	var hltl *session.HostLaunchCmdTooLargeError
	if errors.As(err, &hltl) {
		return fmt.Sprintf(
			"agent %s: launch command exceeded HostLaunchCmdSafeBound (%d bytes, limit %d bytes) — diff too large to inline\n"+
				"hint: try `prism review <pr> --diff-inline-max 0` to skip diff inlining, or reduce the PR diff size.",
			agentName, hltl.CmdSize, hltl.SafeBound,
		)
	}

	// Tier 2: any other error — strip env payload, then truncate.
	raw := err.Error()
	raw = stripEnvPayload(raw)
	return truncateProgressMsg(prNumber, agentName, raw)
}

// stripEnvPayload removes the value of PRISM_INITIAL_PROMPT from an error
// string so that launch-argv content does not reach stdout. The stripping is
// conservative: it looks for "PRISM_INITIAL_PROMPT=" and truncates at that
// point, appending a redaction note to indicate content was removed.
func stripEnvPayload(s string) string {
	const marker = "PRISM_INITIAL_PROMPT="
	idx := strings.Index(s, marker)
	if idx < 0 {
		return s
	}
	// Replace from the marker to the end-of-token (next whitespace after the
	// marker that is NOT inside the value) with a redacted placeholder.
	// Because the value may itself contain whitespace (it is a multi-line
	// prompt), we simply truncate at the marker and append a note.
	return s[:idx] + "[PRISM_INITIAL_PROMPT redacted]"
}

// truncateProgressMsg truncates msg to maxProgressMsgBytes, respecting UTF-8
// boundaries. When truncated, a suffix is appended that names a forensic log
// path where the full message can be read. The log path includes both prNumber
// and agentName so concurrent review runs do not overwrite each other's
// forensic output. The returned string is always ≤ maxProgressMsgBytes+len(suffix).
func truncateProgressMsg(prNumber, agentName, msg string) string {
	if len(msg) <= maxProgressMsgBytes {
		return msg
	}

	// Truncate at a UTF-8 boundary to avoid splitting multi-byte characters.
	// Walk the string rune-by-rune, stopping when the next rune would exceed
	// maxProgressMsgBytes.
	truncated := msg
	byteCount := 0
	for i, r := range msg {
		runeBytes := utf8.RuneLen(r)
		if byteCount+runeBytes > maxProgressMsgBytes {
			truncated = msg[:i]
			break
		}
		byteCount += runeBytes
	}

	safePR := sanitisePRNumber(prNumber)
	safeAgent := sanitisePRNumber(agentName)
	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("prism-review-error-%s-%s.log", safePR, safeAgent))
	suffix := fmt.Sprintf("\n[...truncated; full error in %s]", logPath)
	// Write full message to the forensic path (best-effort, non-fatal).
	_ = os.WriteFile(logPath, []byte(msg), 0o600)
	return truncated + suffix
}

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
// <timeout>" (matching the AC-5 example text exactly). All other errors are
// sanitized to prevent exposing PRISM_INITIAL_PROMPT payloads.
func failureReason(prNumber, agentName string, err error) string {
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
	// Sanitize spawn-machinery errors to prevent PRISM_INITIAL_PROMPT leakage.
	return sanitizeSpawnError(prNumber, agentName, err)
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
