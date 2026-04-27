package sidecar

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
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
var highImpactPrefixes = []string{
	"gh pr merge",
	"gh pr create",
	"gh issue close",
	"git push",
	"prism spawn",
	"prism cleanup",
	"prism prompt",
	"prism merge",
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

// repoFromSession extracts the repo prefix from a session name (e.g.
// "nixos-config" from "nixos-config@main"). Returns an error when the session
// name contains no "@" — this indicates a misconfigured or non-worktree session.
func repoFromSession(sessionName string) (string, error) {
	idx := strings.Index(sessionName, "@")
	if idx < 0 {
		return "", fmt.Errorf("session name %q contains no '@' — cannot derive repo", sessionName)
	}
	return sessionName[:idx], nil
}

// isCoordinator returns true when the session name ends with "@main", which is
// the legacy convention for coordinator sessions in the prism model.
// Deprecated: prefer isCoordinatorSession which also checks the DB.
func isCoordinator(sessionName string) bool {
	return strings.HasSuffix(sessionName, "@main")
}

// isCoordinatorSession returns true when the session is a coordinator. When d
// is non-nil and the session has a row with a non-NULL root_agent_name, it
// computes dbBased = (root_agent_name == "coordinator") and nameBased =
// (sessionName ends with "@main") and returns dbBased || nameBased. This means
// the @main heuristic wins when it disagrees with a stale or incorrect DB value
// (e.g. "worker" written during an SSE inference race); a [debug] log is emitted
// on mismatch. Falls back to the name-suffix heuristic for pre-migration rows
// (NULL root_agent_name) and when d is nil.
func isCoordinatorSession(sessionName string, d *db.DB) bool {
	if d != nil {
		name, rowExists, err := d.RootAgentName(sessionName)
		if err == nil && rowExists {
			if name != "" {
				nameBased := strings.HasSuffix(sessionName, "@main")
				dbBased := name == "coordinator"
				if dbBased != nameBased {
					log.Printf("[debug] sidecar: isCoordinatorSession(%q): DB says %v (root_agent_name=%q), name heuristic says %v — heuristic wins",
						sessionName, dbBased, name, nameBased)
				}
				return dbBased || nameBased
			}
			// Row exists but root_agent_name is NULL — pre-migration row.
			log.Printf("[deprecation] sidecar: isCoordinatorSession(%q): root_agent_name is NULL — pre-migration row, using name heuristic", sessionName)
		} else if err != nil {
			log.Printf("sidecar: isCoordinatorSession: DB error for %q: %v — falling back to name heuristic", sessionName, err)
		}
		// rowExists=false means no row — no log needed, just use heuristic.
	}
	// Pre-migration fallback or DB unavailable: use name-suffix heuristic.
	return strings.HasSuffix(sessionName, "@main")
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
func isReviewAgentSession(sessionName string, d *db.DB) bool {
	nameBased := strings.Contains(sessionName, "~review")
	if d != nil {
		isMember, err := d.IsGroupMember(sessionName)
		if err != nil {
			log.Printf("sidecar: isReviewAgentSession: DB error for %q: %v — falling back to name heuristic", sessionName, err)
			return nameBased
		}
		if isMember {
			// DB-backed: confirmed group member.
			if !nameBased {
				log.Printf("[debug] sidecar: isReviewAgentSession(%q): DB says group member, name heuristic says false",
					sessionName)
			}
			return true
		}
		// DB says not a group member. If the name heuristic says it IS a review
		// agent, this is likely a pre-migration row (group_id not yet set).
		// Fall back to the name heuristic.
		if nameBased {
			log.Printf("[deprecation] sidecar: isReviewAgentSession(%q): group_id not set but name heuristic matches — pre-migration row, using name heuristic", sessionName)
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
