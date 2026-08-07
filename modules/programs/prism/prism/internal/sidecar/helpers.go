package sidecar

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/db"
)

// ── host-API request decoding ────────────────────────────────────────────────
//
// decodeRequestJSON wraps r.Body in an http.MaxBytesReader sized to maxBytes,
// constructs a json.Decoder with DisallowUnknownFields enabled, and decodes
// the body into req. It is the single entry point used by every POST handler
// in host_api.go so that body-size caps and strict-field decoding are applied
// consistently across the surface (issue #1848).
//
// On success it returns (0, nil) and the caller proceeds with req.
//
// On failure it returns one of:
//
//   - (http.StatusRequestEntityTooLarge, err) when the body exceeds maxBytes.
//     Detected via errors.As against *http.MaxBytesError, which the Go runtime
//     surfaces from the wrapped reader.
//   - (http.StatusBadRequest, err) for every other decode failure (malformed
//     JSON, type mismatch, unknown field with DisallowUnknownFields, etc.).
//
// 413 vs 400 trade-off (AC #3): when DisallowUnknownFields is enabled we can
// always tell a body-cap overflow from any other decode error because the
// runtime wraps the cap-exceeded condition in *http.MaxBytesError before the
// decoder ever sees it. We therefore return 413 on cap overflow and 400 on
// every other parse error — the two are distinguishable, so we use the more
// informative status.
//
// allowUnknownFields, when true, disables DisallowUnknownFields. This is for
// the rare handler that must accept forward-compatible trailing fields; every
// such call site must justify the deviation in a code comment (AC #4).
func decodeRequestJSON(w http.ResponseWriter, r *http.Request, req any, maxBytes int64, allowUnknownFields bool) (int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if !allowUnknownFields {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return http.StatusRequestEntityTooLarge, err
		}
		return http.StatusBadRequest, err
	}
	return 0, nil
}

// Body-size caps for host-API POST handlers (issue #1848). 1 MiB is the
// default for every endpoint; /prompt is bumped to 16 MiB because prompts may
// legitimately carry file attachments and worker-spawn context (see
// the call site at mux.HandleFunc("/prompt", ...)). Any other deviation must
// be justified in a code comment at the call site.
const (
	defaultMaxBodyBytes int64 = 1 << 20  // 1 MiB
	promptMaxBodyBytes  int64 = 16 << 20 // 16 MiB
)

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

func marshalTruncated(v any, maxLen int) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return truncate(string(data), maxLen)
}

// ── audit helpers ────────────────────────────────────────────────────────────

// highImpactPrefixes lists the command prefixes that trigger an audit event.
// Each entry is lowercased and compared against the trimmed, lowercased command.
//
// `prism investigate`, `prism pr`, and `prism review` were added in #2364 so
// their invocations surface in `prism audit` alongside the other spawn-shaped
// commands. Before the addition, an invoker's `prism investigate` invocation
// left no audit-log row and a failed spawn (which by design writes durable
// session.spawn_intent / session.spawn_failed rows now) had no companion
// audit-log row keyed on the invoker's session, so grep-by-slug forensic
// queries would return nothing.
var highImpactPrefixes = []string{
	"gh pr merge",
	"gh pr create",
	"gh issue close",
	"git push",
	"prism spawn",
	"prism cleanup",
	"prism prompt",
	"prism merge",
	"prism investigate",
	"prism pr",
	"prism review",
}

// HighImpactCommandPrefixes returns the command prefixes that promote a bash
// tool call to an audit event, as a copy so a caller cannot mutate the
// package's own list.
//
// It exists so the `prism audit` help text and table footer can enumerate the
// audit writers from this list instead of restating it. A hand-maintained
// second copy drifts: the footer still named the pre-#2364 set long after
// `prism merge`, `prism investigate`, `prism pr`, and `prism review` joined
// the list, and nothing failed. See cmd/audit.go and its tests.
func HighImpactCommandPrefixes() []string {
	out := make([]string, len(highImpactPrefixes))
	copy(out, highImpactPrefixes)
	return out
}

// isHighImpactCommand reports whether cmd matches any high-impact prefix.
// Matching is case-insensitive and ignores leading whitespace.
//
// Limitation: only the first (trimmed) line of the command is considered.
// Multi-line shell scripts where a high-impact command appears after an earlier
// line (e.g. "set -e\ngh pr merge 42") will not be matched. This is an
// accepted trade-off: simple prefix matching is sufficient for the forensic
// use-case and avoids false positives from subcommand arguments that happen to
// start with a matched prefix.
func isHighImpactCommand(cmd string) bool {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	for _, prefix := range highImpactPrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+"\t") {
			return true
		}
	}
	return false
}

// extractBashCommand extracts the "command" field from the bash tool's input.
// The input is the raw value of part.State.Input, which is a map[string]any
// after JSON unmarshalling by the SSE parser. Returns an empty string when the
// input is not a map or does not contain a "command" key with a string value.
func extractBashCommand(input any) string {
	if input == nil {
		return ""
	}
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	cmd, ok := m["command"].(string)
	if !ok {
		return ""
	}
	return cmd
}

// isGhPRCreateCommand reports whether cmd is a `gh pr create` invocation
// (case-insensitive, leading-whitespace tolerant). The check is intentionally
// strict — `gh pr create-merge-commit` or any future subcommand whose first
// token is "create-XXX" must not match. Matching is performed on the trimmed,
// lowercased command, treating a tab or space after "create" as the only
// valid termination of the prefix.
//
// Used by the bash tool-completion handler to decide whether to scan the
// command's output for a PR URL and persist the captured pr_number
// (issue #2110).
func isGhPRCreateCommand(cmd string) bool {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	const prefix = "gh pr create"
	if lower == prefix {
		return true
	}
	return strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+"\t")
}

// prURLRegex matches the PR-URL fragment in `gh pr create` output. The CLI
// prints a single line like `https://github.com/owner/repo/pull/123` on
// success, sometimes followed by additional output (e.g. `--web` prints a
// confirmation line). The capture group is the PR number.
//
// The pattern is anchored to /pull/ rather than the full host so that
// enterprise GitHub deployments (github.example.com) and gh-emit changes that
// re-route through a proxy still match. The trailing boundary `(?:[^0-9]|$)`
// prevents a partial match into a longer numeric suffix (e.g. /pull/1234
// would otherwise match as 123 with the `4` dangling).
var prURLRegex = regexp.MustCompile(`/pull/(\d+)(?:[^0-9]|$)`)

// extractPRNumberFromGhOutput scans the raw stdout/stderr of a successful
// `gh pr create` invocation for the PR URL and returns the parsed PR number.
// Returns (0, false) when no URL is found.
//
// The matcher trusts the FIRST URL it sees in output. `gh pr create` prints
// the URL on its own line at the top of its success output; later lines
// (the `--web` confirmation, browser-open warnings) do not include another
// /pull/ URL. If a future gh release changes the output shape the worker
// will silently skip the capture; the sister-write-paths (option (b)
// backfill via spawn_outcome.pr_number from merge-queue ledger) remain as
// fallbacks.
func extractPRNumberFromGhOutput(output string) (int, bool) {
	if output == "" {
		return 0, false
	}
	m := prURLRegex.FindStringSubmatch(output)
	if len(m) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// extractMessageIDFromPayload returns the "messageId" field from a raw event
// payload JSON string. Returns an empty string when the field is absent or the
// JSON cannot be parsed. Used by the /checkin handler's turn-centric logic.
func extractMessageIDFromPayload(raw string) string {
	var p struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ""
	}
	return p.MessageID
}

// ── session-identity helpers ─────────────────────────────────────────────────
//
// The bodies of the two helpers below moved to internal/authz in issue #2619,
// so the `prism checkin` permission predicate can reach them from `cmd/` as
// well as from here. These wrappers stay because roughly fifteen host-API
// handlers call them under these names; forwarding keeps those call sites and
// their tests untouched, and keeps exactly one implementation of each rule.
// Do not reintroduce a body here.

// repoFromSession returns the repo prefix for a session. See
// authz.RepoFromSession for the DB-first resolution and the name-parse
// fallback.
func repoFromSession(sessionName string, d *db.DB) (string, error) {
	return authz.RepoFromSession(sessionName, d)
}

// isCoordinator returns true when the session name ends with "@main", which is
// the legacy convention for coordinator sessions in the prism model.
// Deprecated: prefer isCoordinatorSession which also checks the DB.
func isCoordinator(sessionName string) bool {
	return strings.HasSuffix(sessionName, "@main")
}

// isCoordinatorSession returns true when the session is a coordinator. See
// authz.IsCoordinatorSession for the DB / name-heuristic rule and which one
// wins on disagreement.
func isCoordinatorSession(sessionName string, d *db.DB, logger *log.Logger) bool {
	return authz.IsCoordinatorSession(sessionName, d, logger)
}

// isRootSession returns true when the session is the root session of its own
// project: a "<repo>@main" coordinator, or a non-worktree session with a bare
// name. See authz.IsRootSession for the full rule and for why it is a narrower
// grant than isCoordinatorSession (issue #2658).
//
// Only the cross-repo arm of /prompt reads this. Every other handler keeps
// reading isCoordinatorSession.
func isRootSession(sessionName string, d *db.DB, logger *log.Logger) bool {
	return authz.IsRootSession(sessionName, d, logger)
}

// isHostAPITerminalState returns true when the agent state is a terminal state
// for the purpose of the host-API /logs follow handler.
func isHostAPITerminalState(state agent.AgentState) bool {
	return state == agent.StateFinished ||
		state == agent.StateInterrupted ||
		state == agent.StateDeleted ||
		state == agent.StateError
}

// isReviewAgentSession returns true when the session name belongs to a
// review-agent spawned by `prism review`. Review-agent sessions are named
// <parent>~review-<N>-<role> (e.g. "nixos-config@feature~review-2-review-goal")
// and are identifiable by the presence of "~review" in the session name.
//
// These sessions are short-lived internal helpers; their finish events are
// consumed by the parent worker's pollAgents DB loop and must not propagate
// further up the chain as coordinator notifications.
//
// DB-backed check: a session is a review agent if it belongs to a session group
// (non-NULL group_id). The name-match heuristic is used as a fallback when the
// DB is unavailable or the row has no group_id (pre-migration rows).
func isReviewAgentSession(sessionName string, d *db.DB, logger *log.Logger) bool {
	nameBased := strings.Contains(sessionName, "~review")
	if d != nil {
		isMember, err := d.IsGroupMember(sessionName)
		if err != nil {
			logger.Printf("sidecar: isReviewAgentSession: DB error for %q: %v — falling back to name heuristic", sessionName, err)
			return nameBased
		}
		if isMember {
			// DB-backed: confirmed group member.
			if !nameBased {
				logger.Printf("[debug] sidecar: isReviewAgentSession(%q): DB says group member, name heuristic says false",
					sessionName)
			}
			return true
		}
		// DB says not a group member. If the name heuristic says it IS a review
		// agent, this is likely a pre-migration row (group_id not yet set).
		// Fall back to the name heuristic.
		if nameBased {
			logger.Printf("[deprecation] sidecar: isReviewAgentSession(%q): group_id not set but name heuristic matches — pre-migration row, using name heuristic", sessionName)
			return true
		}
		return false
	}
	// No DB available — use name heuristic.
	return nameBased
}

// parseSpawnSessionName parses the session name from the output of `prism spawn`
// in headless mode, which prints: session "name" created
// Returns empty string if the output does not match the expected format.
func parseSpawnSessionName(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		// Match: session "name" created
		if !strings.HasPrefix(line, "session ") || !strings.HasSuffix(line, " created") {
			continue
		}
		// Strip prefix "session " and suffix " created".
		inner := strings.TrimPrefix(line, "session ")
		inner = strings.TrimSuffix(inner, " created")
		// inner should now be a quoted string like `"name"`.
		if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
			return inner[1 : len(inner)-1]
		}
	}
	return ""
}

// isSQLiteBusy reports whether err is a SQLite SQLITE_BUSY or SQLITE_LOCKED
// error. Delegates to db.IsSQLiteBusy; kept as a package-local alias so
// existing call sites in this package need not change.
func isSQLiteBusy(err error) bool {
	return db.IsSQLiteBusy(err)
}

// parseAllSpawnSessionNames collects all session names from the output of
// `prism spawn --abtest`, which prints two lines of the form:
//
//	session "name" created
//
// Returns all matched names in order.
func parseAllSpawnSessionNames(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "session ") || !strings.HasSuffix(line, " created") {
			continue
		}
		inner := strings.TrimPrefix(line, "session ")
		inner = strings.TrimSuffix(inner, " created")
		if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
			names = append(names, inner[1:len(inner)-1])
		}
	}
	return names
}
