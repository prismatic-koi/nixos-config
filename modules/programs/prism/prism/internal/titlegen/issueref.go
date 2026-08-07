package titlegen

import "regexp"

// Issue/ticket reference extraction (issue #2683).
//
// THE MODEL IS NEVER ASKED FOR THIS VALUE.
//
// That is the whole design of this file, and it is not a performance
// choice. `agent_status.issue_ref` is rendered as a link to real work. A
// model asked "which issue is this?" will answer with a well-formed number
// whether or not one is present, and a well-formed wrong number is
// indistinguishable from a right one at the point of display. The failure
// is silent misattribution: session A's work filed under issue B. An empty
// column is strictly better — it is visibly empty, and a reader knows to go
// look.
//
// So the reference is a pure function of the source text. If the caller can
// see the string, so can the reader; if it is not in the string, the answer
// is "none".
//
// If a future change ever does route a model-supplied reference through
// here (to catch an indirect mention such as "that gitlab ticket"), it MUST
// reject any candidate that does not appear verbatim in the source text.
// That check is the invariant; the extraction is just the current, strictest
// implementation of it.

var (
	// gitHubRef matches the GitHub form: `#` then digits — `#2683`.
	//
	// The trailing \b stops `#123abc` from yielding `#123`. There is no
	// leading \b: `#` is a non-word character, so `issue#123` and `(#123)`
	// both match, which is what a reader means in both cases.
	gitHubRef = regexp.MustCompile(`#\d+\b`)

	// jiraRef matches the Jira form: a project key then `-` then digits —
	// `PLAT-123`, `CH-9`.
	//
	// The leading \b stops a key being pulled out of the middle of a longer
	// token (`xPLAT-123` does not yield `PLAT-123`). The key must start with
	// a letter and be at least two characters, matching Jira's own rule.
	//
	// Known collision, accepted deliberately: this form also matches
	// `UTF-8`, `ISO-8601`, `SHA-256` and similar standards names. Those are
	// real substrings of the source text, not invented values, so the
	// misattribution risk the model ban exists to prevent does not apply —
	// and narrowing the pattern with a denylist of standards names would
	// invent policy this issue did not ask for. The GitHub form is tried
	// first (see ExtractIssueRef), which is what keeps the collision off
	// this GitHub-hosted repo's own sessions in practice.
	jiraRef = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)
)

// ExtractIssueRef returns the first issue or ticket reference in text, or ""
// when text carries none.
//
// "" means NULL at the call site. It never means "unknown, so guess".
//
// Precedence is GitHub first, then Jira, and it is deliberate rather than
// positional: prism's own work lives on GitHub, so when a prompt carries
// both an issue number and something Jira-shaped, the issue number is the
// one that identifies the work. Within one form the FIRST match in the text
// wins, which puts the reference from the opening line of a spawn prompt
// ahead of any number quoted in the body.
//
// The returned value is guaranteed to be a verbatim substring of text and to
// contain no control bytes, since both patterns admit only `#`, `-`, ASCII
// letters and digits. Callers may render it without further escaping.
func ExtractIssueRef(text string) string {
	if m := gitHubRef.FindString(text); m != "" {
		return m
	}
	return jiraRef.FindString(text)
}
