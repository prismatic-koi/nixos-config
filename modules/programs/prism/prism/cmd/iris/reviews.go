package main

// reviews.go — `iris reviews` historical review-group listing (#1722).
//
// This is the iris analogue of `prism reviews`. It exposes the historical
// review-group ledger so coordinators and workers can revisit past review
// activity without dropping into raw SQL or grepping session names.
//
// Distinct from `iris review <pr>`, which spawns a NEW review round. This
// surface only reads the session_groups table and its members.
//
// Subcommands:
//
//   iris reviews                    list recent review groups, newest first
//   iris reviews --days N           filter to groups created in the last N days
//   iris reviews --json             emit a JSON array
//   iris reviews show <group_id>    list one group's members + outcomes
//   iris reviews show <group_id> --json   JSON object for the group
//
// Data source: reads iris.db directly via internal/db (the same read-only
// carve-out as `iris checkin` — #1676). The daemon does not need to be
// running for this surface to work; SQLite's WAL mode lets us share the
// database with the daemon when it is.
//
// Empty / missing-group behaviour:
//
//   - `iris reviews` with zero rows prints "no reviews recorded" and exits 0.
//   - `iris reviews show <id>` with an unknown id exits non-zero with a
//     clear "no such group" error.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// reviewsCmd is the parent cobra command. Plain `iris reviews` runs
// runReviewsList (the same RunE as `iris reviews list`-style would, but
// we keep the surface flat to match the issue spec: `iris reviews` is the
// list verb, and `iris reviews show <group>` is the only subcommand).
var reviewsCmd = &cobra.Command{
	Use:   "reviews",
	Short: "List historical review groups recorded in the iris database",
	Long: `List historical review groups recorded in the iris database.

Each row shows: PR number (inferred from the parent session's branch when
possible), parent (worker) session, round number, group state
(in-progress / completed / empty), member count, age, and the per-agent
session names.

This subcommand is read-only. It reads the iris database directly and
does not require the iris daemon to be running. Use 'iris reviews show
<group_id>' to inspect one group's members in detail.

Empty ledger → "no reviews recorded" (exit 0). Missing group_id in
'iris reviews show' → "no such review group" (exit non-zero).`,
	Args:          cobra.NoArgs,
	RunE:          runReviewsList,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// reviewsShowCmd implements `iris reviews show <group_id>`.
var reviewsShowCmd = &cobra.Command{
	Use:   "show <group_id>",
	Short: "Show one review group's members with state and timing",
	Long: `Show one review group's members with terminal state and timing.

Each row of the output corresponds to one agent_status row in the group,
with columns: session name, role (root agent name), state, started-at
(last_seen for active rows; ended_at for terminal rows), and duration.

Use --json to emit a JSON object suitable for scripting. An unknown
<group_id> exits non-zero with a clear "no such review group" error.`,
	Args:          cobra.ExactArgs(1),
	RunE:          runReviewsShow,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	reviewsCmd.Flags().Bool("json", false, "Emit a JSON array of review-group entries instead of the human-readable table")
	reviewsCmd.Flags().Int("days", 0, "Only show groups created within the last N days (0 = no filter)")
	reviewsCmd.Flags().Int("limit", 0, "Limit the number of rows returned (0 = all)")

	reviewsShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of the human-readable table")

	reviewsCmd.AddCommand(reviewsShowCmd)
	rootCmd.AddCommand(reviewsCmd)
}

// reviewSessionPattern matches per-agent review session names of the form
// "<parent>~review-<N>-<agent>". Used to recover the round number N from
// any one member.
var reviewSessionPattern = regexp.MustCompile(`^(.+)~review-(\d+)-(.+)$`)

// reviewParentBranchPattern extracts the branch component from a parent
// session name "<repo>@<branch>". Used as a heuristic for surfacing the
// PR number when the branch happens to be of the form "pr-<N>".
var reviewParentBranchPattern = regexp.MustCompile(`@(.+)$`)

// reviewsOpenDB opens the iris DB for read. Mirrors openIrisDBForRead in
// checkin.go — we wrap the missing-file failure mode in an actionable
// "iris database not found" message rather than the raw modernc.org
// error. Defined separately from openIrisDBForRead so the error prefix
// is correctly "iris reviews:" and the caller can be tested in isolation.
func reviewsOpenDB(dbPath string) (*db.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("iris database not found at %s; has the iris daemon ever run? (start it with `systemctl --user start iris` on Linux, or `launchctl kickstart -k gui/$UID/local.iris.daemon` on Darwin)", dbPath)
		}
		return nil, fmt.Errorf("cannot read iris database at %s: %w", dbPath, err)
	}
	d, err := iris.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open iris database at %s: %w", dbPath, err)
	}
	return d, nil
}

// resolveReviewsDBPath returns the path used by `iris reviews`. testDBPath
// (from db.go) wins so tests can redirect via SetTestDBPath; otherwise we
// defer to iris.ResolvePaths().DB. Sharing the test hook with the `iris
// db` family keeps the test surface consistent.
func resolveReviewsDBPath() string {
	if testDBPath != "" {
		return testDBPath
	}
	return iris.ResolvePaths().DB
}

func runReviewsList(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	days, _ := cmd.Flags().GetInt("days")
	limit, _ := cmd.Flags().GetInt("limit")

	if days < 0 {
		return fmt.Errorf("iris reviews: --days must be ≥ 0, got %d", days)
	}
	if limit < 0 {
		return fmt.Errorf("iris reviews: --limit must be ≥ 0, got %d", limit)
	}

	dbPath := resolveReviewsDBPath()
	d, err := reviewsOpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("iris reviews: %w", err)
	}
	defer d.Close()

	// Fetch all groups (limit is applied client-side after the --days filter
	// so the two flags compose predictably: "limit N after filtering to the
	// last D days" rather than "limit before filtering").
	groups, err := d.ReviewGroupsList(0)
	if err != nil {
		return fmt.Errorf("iris reviews: %w", err)
	}

	if days > 0 {
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		filtered := groups[:0]
		for _, g := range groups {
			if g.CreatedAt.After(cutoff) {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}
	if limit > 0 && len(groups) > limit {
		groups = groups[:limit]
	}

	out := cmd.OutOrStdout()
	if jsonMode {
		return renderReviewsListJSON(out, groups)
	}
	return renderReviewsList(out, groups)
}

func runReviewsShow(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	groupID := strings.TrimSpace(args[0])
	if groupID == "" {
		return fmt.Errorf("iris reviews show: empty <group_id> argument")
	}

	dbPath := resolveReviewsDBPath()
	d, err := reviewsOpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("iris reviews show: %w", err)
	}
	defer d.Close()

	// Resolve the group: pull all groups and find the one with this id.
	// This is the simplest way to surface a clear "no such group" error
	// without adding a single-row lookup helper to internal/db (which
	// would risk overlap with #1699's work on the same package). The list
	// query is cheap — session_groups is small.
	allGroups, err := d.ReviewGroupsList(0)
	if err != nil {
		return fmt.Errorf("iris reviews show: %w", err)
	}
	var summary *db.ReviewGroupSummary
	for i := range allGroups {
		if allGroups[i].GroupID == groupID {
			summary = &allGroups[i]
			break
		}
	}
	if summary == nil {
		return fmt.Errorf("iris reviews show: no such review group %q", groupID)
	}

	members, err := d.GroupMembersForGroup(groupID)
	if err != nil {
		return fmt.Errorf("iris reviews show: members: %w", err)
	}

	out := cmd.OutOrStdout()
	if jsonMode {
		return renderReviewsShowJSON(out, *summary, members)
	}
	return renderReviewsShow(out, *summary, members)
}

// reviewsJSONRow is the snake_case JSON shape for one review-group row.
// Defined explicitly so the JSON contract is decoupled from
// db.ReviewGroupSummary's field naming. Mirrors `prism reviews list`'s
// JSON shape so scripts can be shared between the two surfaces.
type reviewsJSONRow struct {
	GroupID       string   `json:"group_id"`
	PR            *int     `json:"pr"`
	ParentSession string   `json:"parent_session"`
	AgentSessions []string `json:"agent_sessions"`
	AgentStates   []string `json:"agent_states"`
	GroupState    string   `json:"group_state"`
	MemberCount   int      `json:"member_count"`
	StartedAt     string   `json:"started_at"` // RFC3339, UTC
	Round         *int     `json:"round"`
}

// reviewsShowJSON is the snake_case shape returned by `iris reviews show
// --json`. Contains the same group-level fields as reviewsJSONRow plus a
// members[] array with per-agent timing.
type reviewsShowJSON struct {
	GroupID       string                 `json:"group_id"`
	PR            *int                   `json:"pr"`
	ParentSession string                 `json:"parent_session"`
	GroupState    string                 `json:"group_state"`
	StartedAt     string                 `json:"started_at"`
	Round         *int                   `json:"round"`
	Members       []reviewsShowJSONMember `json:"members"`
}

type reviewsShowJSONMember struct {
	SessionName string  `json:"session_name"`
	Role        string  `json:"role"` // root_agent_name; empty when unrecorded
	State       string  `json:"state"`
	LastSeen    string  `json:"last_seen"` // RFC3339, UTC
	EndedAt     *string `json:"ended_at"`  // RFC3339 / UTC, or null when still running
	DurationMs  *int64  `json:"duration_ms"` // ended_at − last_seen-on-creation; null when active
}

func renderReviewsListJSON(out io.Writer, groups []db.ReviewGroupSummary) error {
	rows := make([]reviewsJSONRow, 0, len(groups))
	for _, g := range groups {
		row := reviewsJSONRow{
			GroupID:       g.GroupID,
			ParentSession: g.ParentSession,
			AgentSessions: append([]string(nil), g.Members...),
			AgentStates:   append([]string(nil), g.AgentStates...),
			GroupState:    g.GroupState,
			MemberCount:   len(g.Members),
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
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(rows)
}

func renderReviewsShowJSON(out io.Writer, g db.ReviewGroupSummary, members []db.Status) error {
	body := reviewsShowJSON{
		GroupID:       g.GroupID,
		ParentSession: g.ParentSession,
		GroupState:    g.GroupState,
		StartedAt:     g.CreatedAt.UTC().Format(time.RFC3339),
		Members:       make([]reviewsShowJSONMember, 0, len(members)),
	}
	if pr := inferPRFromGroup(g); pr > 0 {
		body.PR = &pr
	}
	if round := inferRoundFromGroup(g); round > 0 {
		body.Round = &round
	}
	for _, m := range members {
		role := ""
		if m.RootAgentName != nil {
			role = *m.RootAgentName
		}
		entry := reviewsShowJSONMember{
			SessionName: m.SessionName,
			Role:        role,
			State:       m.State,
			LastSeen:    m.LastSeen.UTC().Format(time.RFC3339),
		}
		if m.EndedAt != nil {
			endedStr := m.EndedAt.UTC().Format(time.RFC3339)
			entry.EndedAt = &endedStr
			// Duration: ended − started_at. The session group's created_at
			// (g.CreatedAt) is the closest proxy for "this agent started"
			// since the agent_status row doesn't carry a started_at. We
			// fall back to the member's last_seen when ended_at is older
			// than the group (which would yield a negative duration).
			if !m.EndedAt.Before(g.CreatedAt) {
				dur := m.EndedAt.Sub(g.CreatedAt).Milliseconds()
				entry.DurationMs = &dur
			}
		}
		body.Members = append(body.Members, entry)
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(body)
}

// renderReviewsList renders the human-readable table for `iris reviews`.
// Column layout matches `prism reviews list` (PR / ROUND / STATE / PARENT
// / N / AGE / AGENTS) so the two surfaces are visually consistent.
func renderReviewsList(out io.Writer, groups []db.ReviewGroupSummary) error {
	if len(groups) == 0 {
		fmt.Fprintln(out, "no reviews recorded")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cliColSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(cliColSecondary))

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
	fmt.Fprintln(out, styleHeader.Render(header))
	fmt.Fprintln(out, styleDim.Render(strings.Repeat("─", len(header)+5)))

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
		parent := truncateForColumn(g.ParentSession, wParent)
		ageStr := formatAgeFromTime(g.CreatedAt, now)
		agents := truncateForColumn(strings.Join(g.Members, ","), 80)
		state := reviewGroupStateStyle(g.GroupState).Render(fmt.Sprintf("%-*s", wState, truncateForColumn(g.GroupState, wState)))
		fmt.Fprintf(out, "%-*s  %-*s  %s  %-*s  %-*d  %-*s  %s\n",
			wPR, prStr,
			wRound, roundStr,
			state,
			wParent, parent,
			wAgents, len(g.Members),
			wAge, ageStr,
			agents,
		)
	}
	return nil
}

// renderReviewsShow renders the human-readable table for `iris reviews
// show <group_id>`. Header line states the group's metadata; the table
// below lists each member with role, state, started/ended timestamps,
// and duration.
func renderReviewsShow(out io.Writer, g db.ReviewGroupSummary, members []db.Status) error {
	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cliColSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(cliColSecondary))

	prStr := "—"
	if pr := inferPRFromGroup(g); pr > 0 {
		prStr = fmt.Sprintf("#%d", pr)
	}
	roundStr := "—"
	if r := inferRoundFromGroup(g); r > 0 {
		roundStr = fmt.Sprintf("%d", r)
	}

	fmt.Fprintf(out, "Group:   %s\n", g.GroupID)
	fmt.Fprintf(out, "PR:      %s\n", prStr)
	fmt.Fprintf(out, "Round:   %s\n", roundStr)
	fmt.Fprintf(out, "Parent:  %s\n", g.ParentSession)
	fmt.Fprintf(out, "State:   %s\n", g.GroupState)
	fmt.Fprintf(out, "Started: %s\n", g.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintln(out)

	if len(members) == 0 {
		fmt.Fprintln(out, "(no members)")
		return nil
	}

	// Session names of the form "<repo>@<branch>~review-<N>-<agent>" are
	// commonly 50–70 chars long. We size the column to fit the worst case
	// without truncation — a wide column is fine for `show`, which is a
	// detail view, even if it pushes other columns off-screen on narrow
	// terminals.
	const (
		wSession = 72
		wRole    = 18
		wState   = 12
		wDur     = 10
	)
	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %s",
		wSession, "SESSION",
		wRole, "ROLE",
		wState, "STATE",
		wDur, "DURATION",
		"ENDED",
	)
	fmt.Fprintln(out, styleHeader.Render(header))
	fmt.Fprintln(out, styleDim.Render(strings.Repeat("─", len(header)+5)))

	for _, m := range members {
		role := "—"
		if m.RootAgentName != nil && *m.RootAgentName != "" {
			role = *m.RootAgentName
		}
		dur := "—"
		endedStr := "—"
		if m.EndedAt != nil {
			endedStr = m.EndedAt.UTC().Format(time.RFC3339)
			if !m.EndedAt.Before(g.CreatedAt) {
				dur = formatDuration(m.EndedAt.Sub(g.CreatedAt))
			}
		}
		state := reviewGroupStateStyle(m.State).Render(fmt.Sprintf("%-*s", wState, truncateForColumn(m.State, wState)))
		fmt.Fprintf(out, "%-*s  %-*s  %s  %-*s  %s\n",
			wSession, truncateForColumn(m.SessionName, wSession),
			wRole, truncateForColumn(role, wRole),
			state,
			wDur, dur,
			endedStr,
		)
	}
	return nil
}

// reviewGroupStateStyle returns the lipgloss style for a review-group or
// per-agent state. Mirrors `prism reviews list`'s palette: yellow for in
// progress, green for completed/finished, grey for empty / deleted /
// interrupted, default for everything else.
func reviewGroupStateStyle(state string) lipgloss.Style {
	switch state {
	case "in-progress", "active":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33"))
	case "completed", "finished":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("32"))
	case "empty", "deleted", "interrupted":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(cliColSecondary))
	case "error":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	default:
		return lipgloss.NewStyle()
	}
}

// inferPRFromGroup returns the PR number for a review group when it is
// derivable from the parent session's branch component (e.g.
// "repo@pr-123"). Returns 0 when no PR number can be inferred.
func inferPRFromGroup(g db.ReviewGroupSummary) int {
	m := reviewParentBranchPattern.FindStringSubmatch(g.ParentSession)
	if len(m) != 2 {
		return 0
	}
	branch := m[1]
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

// inferRoundFromGroup returns the review round number derived from any
// one member's session name (`<parent>~review-<N>-<agent>`). All members
// share the same round, so the first parsed value wins.
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

// formatAgeFromTime renders the wall-clock duration since t as a compact
// string (`3m`, `1h`, `2d`). Matches the AGE column of `prism reviews
// list` for visual consistency. Returns "—" for unset / future times.
func formatAgeFromTime(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < 0 {
		return "—"
	}
	return formatDuration(d)
}

// formatDuration is the shared compact-duration formatter used by the
// AGE and DURATION columns. Three buckets:
//
//	< 1 minute   →  "Xs"
//	< 1 hour     →  "Xm"
//	< 1 day      →  "XhYm"
//	≥ 1 day      →  "XdYh"
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}
