package cmd

// prism reviews — inspect the review-group ledger (#1500).
//
// Mirrors the shape of `prism merges` but for review groups. The
// session_groups table records every review round prism has spawned; this
// command exposes that ledger so coordinators and workers don't have to
// `prism sessions list | grep '~review-N-'` and reconstruct the metadata.
//
// Subcommands:
//
//   prism reviews             alias for `prism reviews list`
//   prism reviews list        all review groups, newest first
//   prism reviews list --json JSON array suitable for scripting
//
// The list shows: PR number (parsed from agent session names), parent
// session, agent sessions, group state (in-progress / completed / empty),
// and the started-at timestamp.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
)

var reviewsCmd = &cobra.Command{
	Use:   "reviews",
	Short: "Inspect the review-group ledger",
	Long: `Inspect the review-group ledger.

Defaults to listing all review groups, newest first. Each row shows the
review's PR number, parent (worker) session, agent sessions, group state,
and the started-at timestamp.

The session_groups table is the authoritative source. Use this command
instead of 'prism sessions list | grep ~review-' — that workaround is
fragile and lacks group-level metadata.`,
	Args: cobra.NoArgs,
	RunE: runReviewsList,
}

var reviewsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List review groups",
	Long: `List review groups in the prism database, newest first.

Each row shows: PR number, parent (worker) session, agent sessions, group
state, and started-at timestamp. With --json, emits a JSON array of review
group objects suitable for scripting and polling.`,
	Args: cobra.NoArgs,
	RunE: runReviewsList,
}

func init() {
	reviewsCmd.Flags().Bool("json", false, "Emit a JSON array of review-group entries to stdout instead of the human-readable table")
	reviewsCmd.Flags().Int("limit", 0, "Limit the number of rows returned (default: all)")
	reviewsListCmd.Flags().Bool("json", false, "Emit a JSON array of review-group entries to stdout instead of the human-readable table")
	reviewsListCmd.Flags().Int("limit", 0, "Limit the number of rows returned (default: all)")

	reviewsCmd.AddCommand(reviewsListCmd)
	rootCmd.AddCommand(reviewsCmd)
}

// reviewSessionPattern matches per-agent review session names of the form
// "<parent>~review-<N>-<agent>". We use it to recover the PR number's parent
// branch from any one member; the actual PR number is not encoded in the
// session name (the round number N is) so PR is left unknown unless we can
// derive it from the parent's branch suffix.
var reviewSessionPattern = regexp.MustCompile(`^(.+)~review-(\d+)-(.+)$`)

// reviewParentBranchPattern extracts the branch component from a parent
// session name "<repo>@<branch>". Used as a heuristic for surfacing the PR
// number when the branch happens to be of the form "pr-<N>".
var reviewParentBranchPattern = regexp.MustCompile(`@(.+)$`)

func runReviewsList(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	limit, _ := cmd.Flags().GetInt("limit")

	// Inside a bwrap / podman / sandbox-exec sandbox: proxy the list to
	// the host sidecar (#1043 pattern). The host's prism.db is invisible
	// to direct reads from inside the sandbox — falling through to the DB
	// path would silently return an empty list (the shadow tmpfs DB has
	// no rows the host watcher writes). The sibling `prism merges list`
	// already does this; without the same branch, `prism reviews list`
	// inside a sandbox would silently return [] (#1500 round-2
	// review-context blocker).
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		params := map[string]string{}
		if limit > 0 {
			params["limit"] = strconv.Itoa(limit)
		}
		var groups []db.ReviewGroupSummary
		if proxyErr := proxyGetFromHostAPI(apiURL, "/groups/list", params, &groups); proxyErr != nil {
			return fmt.Errorf("prism reviews list: %w", proxyErr)
		}
		if jsonMode {
			return renderReviewsListJSON(groups)
		}
		return renderReviewsList(groups)
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism reviews list: open db: %w", err)
	}
	defer d.Close()

	groups, err := d.ReviewGroupsList(limit)
	if err != nil {
		return fmt.Errorf("prism reviews list: %w", err)
	}

	if jsonMode {
		return renderReviewsListJSON(groups)
	}
	return renderReviewsList(groups)
}

// reviewsJSONRow is the snake_case JSON shape for one review-group row.
// Defined explicitly so the JSON contract is decoupled from
// db.ReviewGroupSummary's field naming.
type reviewsJSONRow struct {
	GroupID        string   `json:"group_id"`
	PR             *int     `json:"pr"`
	ParentSession  string   `json:"parent_session"`
	AgentSessions  []string `json:"agent_sessions"`
	AgentStates    []string `json:"agent_states"`
	GroupState     string   `json:"group_state"`
	StartedAt      string   `json:"started_at"` // RFC3339
	Round          *int     `json:"round"`      // round number derived from agent session names
}

func renderReviewsListJSON(groups []db.ReviewGroupSummary) error {
	rows := make([]reviewsJSONRow, 0, len(groups))
	for _, g := range groups {
		row := reviewsJSONRow{
			GroupID:       g.GroupID,
			ParentSession: g.ParentSession,
			AgentSessions: append([]string(nil), g.Members...),
			AgentStates:   append([]string(nil), g.AgentStates...),
			GroupState:    g.GroupState,
			StartedAt:     g.CreatedAt.UTC().Format(time.RFC3339),
		}
		if pr := inferPRFromGroup(g); pr > 0 {
			row.PR = &pr
		}
		if round := inferRoundFromGroup(g); round > 0 {
			row.Round = &round
		}
		rows = append(rows, row)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("prism reviews list --json: marshal: %w", err)
	}
	return printJSON(data)
}

// inferPRFromGroup returns the PR number for a review group when it is
// derivable from the parent session's branch component (e.g. "repo@pr-123").
// Returns 0 when no PR number can be inferred — the JSON contract represents
// this as a null `pr` field.
func inferPRFromGroup(g db.ReviewGroupSummary) int {
	m := reviewParentBranchPattern.FindStringSubmatch(g.ParentSession)
	if len(m) != 2 {
		return 0
	}
	branch := m[1]
	// Common conventions:
	//   pr-<N>   → PR #N
	//   pr<N>    → PR #N
	for _, prefix := range []string{"pr-", "pr"} {
		if strings.HasPrefix(branch, prefix) {
			rest := strings.TrimPrefix(branch, prefix)
			if n, err := strconv.Atoi(rest); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// inferRoundFromGroup returns the review round number derived from any one
// member's session name (`<parent>~review-<N>-<agent>`). All members of a
// group share the same round, so we can return the first one we parse.
func inferRoundFromGroup(g db.ReviewGroupSummary) int {
	for _, name := range g.Members {
		m := reviewSessionPattern.FindStringSubmatch(name)
		if len(m) == 4 {
			if n, err := strconv.Atoi(m[2]); err == nil {
				return n
			}
		}
	}
	return 0
}

func renderReviewsList(groups []db.ReviewGroupSummary) error {
	if len(groups) == 0 {
		fmt.Println("no review groups recorded")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	const (
		wPR     = 6
		wRound  = 5
		wState  = 12
		wParent = 30
		wAgents = 4
		wAge    = 8
	)
	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s",
		wPR, "PR",
		wRound, "ROUND",
		wState, "STATE",
		wParent, "PARENT",
		wAgents, "N",
		wAge, "AGE",
		"AGENTS",
	)
	fmt.Println(styleHeader.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header)+5)))

	now := time.Now()
	for _, g := range groups {
		prStr := "—"
		if pr := inferPRFromGroup(g); pr > 0 {
			prStr = fmt.Sprintf("#%d", pr)
		}
		roundStr := "—"
		if r := inferRoundFromGroup(g); r > 0 {
			roundStr = fmt.Sprintf("%d", r)
		}
		parent := truncateStr(g.ParentSession, wParent)
		ageStr := formatAge(now, g.CreatedAt)
		agents := truncateStr(strings.Join(g.Members, ","), 80)
		state := reviewGroupStateStyle(g.GroupState).Render(fmt.Sprintf("%-*s", wState, truncateStr(g.GroupState, wState)))
		fmt.Printf("%-*s  %-*s  %s  %-*s  %-*d  %-*s  %s\n",
			wPR, prStr,
			wRound, roundStr,
			state,
			wParent, parent,
			wAgents, len(g.Members),
			wAge, ageStr,
			agents,
		)
	}
	fmt.Fprintln(os.Stdout)
	return nil
}

// reviewGroupStateStyle returns the lipgloss style for a review group state.
func reviewGroupStateStyle(state string) lipgloss.Style {
	switch state {
	case "in-progress":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")) // yellow
	case "completed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("32")) // green
	case "empty":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	default:
		return lipgloss.NewStyle()
	}
}
