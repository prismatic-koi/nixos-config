package cmd

// prism list-sessions — print agent sessions (from DB) in a human-readable
// table. By default shows only sessions for the current repo. Use --all / -A
// to list sessions across all repos.
//
// Sessions are sorted using db.ParentSessionFor — the single source of truth
// for parent attribution shared with the dashboard. Depth-2 review sessions
// appear immediately after their parent branch in the output, matching the
// parent-child nesting shown by the dashboard TUI.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

var listSessionsCmd = &cobra.Command{
	Use:   "list-sessions",
	Short: "List agent sessions with their state and title",
	RunE: func(cmd *cobra.Command, args []string) error {
		showAll, _ := cmd.Flags().GetBool("all")

		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			return proxyListSessionsAndRender(apiURL, showAll)
		}

		// Derive currentRepo from the working directory using the same
		// normalisation as deriveRepo() so the filter matches the repo column
		// written at session-start time.
		currentRepo := ""
		cwd, err := os.Getwd()
		if err != nil {
			cwd = os.Getenv("PRISM_SPAWN_PATH")
		}
		if cwd != "" {
			currentRepo = deriveRepo(cwd)
		}

		// If repo detection failed and --all was not requested, refuse to
		// silently show all sessions — that would violate the scoped-by-default
		// contract. Direct the user to --all instead.
		if !showAll && currentRepo == "" {
			if cwd == "" {
				return fmt.Errorf("list-sessions: CWD unavailable and PRISM_SPAWN_PATH not set; use --all to list all sessions")
			}
			return fmt.Errorf("list-sessions: %q is not inside a prism worktree; use --all to list all sessions", cwd)
		}

		d, err := openDB()
		if err != nil {
			return fmt.Errorf("list-sessions: open db: %w", err)
		}
		defer d.Close()

		var ss []db.Status
		if showAll {
			ss, err = d.AllActiveStatus()
		} else {
			ss, err = d.AllActiveStatusForRepo(currentRepo)
		}
		if err != nil {
			return fmt.Errorf("list-sessions: query db: %w", err)
		}

		// Fetch group parents to apply the same parent-attribution source of
		// truth as the dashboard (db.ParentSessionFor via AllGroupParents).
		// Non-fatal: if unavailable, sort falls back to the name heuristic.
		groupParents, _ := d.AllGroupParents()

		return renderSessionTable(ss, groupParents)
	},
}

func init() {
	listSessionsCmd.Flags().BoolP("all", "A", false, "List all sessions across all repos")
	rootCmd.AddCommand(listSessionsCmd)
}

// displayHarness returns the harness display string for a session, falling
// back to "opencode" when the field is nil or empty (pre-migration rows).
func displayHarness(h *string) string {
	if h == nil || *h == "" {
		return "opencode"
	}
	return *h
}

// renderSessionTable renders a []db.Status as a sorted SESSION/STATE/PORT/HARNESS/TITLE
// table to stdout. Both the direct DB path and the host-API proxy path use
// this shared renderer.
//
// groupParents is a map of group_id → parent_session from db.AllGroupParents.
// It is used to sort depth-2 review sessions immediately after their parent
// branch — the same parent-attribution logic used by the dashboard. When nil
// (e.g. the host-API proxy path), the sort falls back to name heuristic.
func renderSessionTable(ss []db.Status, groupParents map[string]string) error {
	type row struct {
		name    string
		state   string
		port    string
		harness string
		title   string
	}

	var rows []row
	for _, s := range ss {
		title := "—"
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		port := ""
		if s.HarnessPort != nil {
			port = fmt.Sprintf("%d", *s.HarnessPort)
		}
		rows = append(rows, row{name: s.SessionName, state: s.State, port: port, harness: displayHarness(s.Harness), title: title})
	}

	// Sort rows using the same parent-attribution logic as the dashboard's
	// SortDisplayed: depth-2 review sessions sort immediately after their
	// parent branch. The sort key is derived from db.ParentSessionFor
	// (backing both views), with a name-heuristic fallback for pre-migration
	// rows where group_id is not set.
	//
	// Key structure (mirrors dashboard.SortDisplayed):
	//   - Plain sessions (no @): "repo\x00<name>"
	//   - @main sessions:        "repo\x00repo@main"
	//   - Branch sessions:       "repo\x01<branch>\x00"
	//   - Depth-2 child of @main:  "repo\x00repo@main\x01<label>"
	//   - Depth-2 child of @branch: "repo\x01<parent-branch>\x00<label>"
	listSessionKey := func(name string, groupID *string) string {
		atIdx := strings.Index(name, "@")
		if atIdx < 0 {
			// Plain session (no @).
			repo := name
			return repo + "\x00" + name
		}
		repo := name[:atIdx]
		branch := name[atIdx:] // includes "@"

		if branch == "@main" {
			return repo + "\x00" + name
		}

		// Resolve parent: prefer DB-backed group_id → parent_session; fall back to name heuristic.
		parentBranch := ""
		if groupID != nil && groupParents != nil {
			if parent, ok := groupParents[*groupID]; ok && parent != "" {
				// parent is e.g. "nixos-config@main" or "nixos-config@feature"
				if idx := strings.Index(parent, "@"); idx >= 0 {
					parentBranch = parent[idx:] // e.g. "@main"
				}
			}
		}
		// Name-heuristic fallback for pre-migration rows.
		if parentBranch == "" {
			inner := branch[1:] // strip leading "@"
			if tildeIdx := strings.Index(inner, "~"); tildeIdx >= 0 {
				// Depth-2 session.
				parentBranch = "@" + inner[:tildeIdx]
			}
		}

		if parentBranch != "" {
			// Depth-2 session: sort immediately after parent branch.
			label := ""
			inner := branch[1:] // strip "@"
			if tildeIdx := strings.Index(inner, "~"); tildeIdx >= 0 {
				label = inner[tildeIdx:] // e.g. "~review-1-review-goal"
			}
			if parentBranch == "@main" {
				parentName := repo + "@main"
				return repo + "\x00" + parentName + "\x01" + label
			}
			return repo + "\x01" + parentBranch + "\x00" + label
		}

		// Regular depth-1 branch session.
		return repo + "\x01" + branch + "\x00"
	}

	// Build an index of session_name → group_id for the sort key helper.
	// The Status slice already carries the group_id; we map by name for O(1) lookup.
	type nameAndGID struct {
		groupID *string
	}
	gidByName := make(map[string]*string, len(ss))
	for i := range ss {
		gidByName[ss[i].SessionName] = ss[i].GroupID
	}

	for i := 1; i < len(rows); i++ {
		key := rows[i]
		keyStr := listSessionKey(key.name, gidByName[key.name])
		j := i - 1
		for j >= 0 && listSessionKey(rows[j].name, gidByName[rows[j].name]) > keyStr {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = key
	}

	if len(rows) == 0 {
		fmt.Println("no agent sessions found")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleName := lipgloss.NewStyle().Bold(true)
	styleTitle := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render(fmt.Sprintf("%-40s  %-8s  %-6s  %-10s  %s", "SESSION", "STATE", "PORT", "HARNESS", "TITLE")))
	for _, r := range rows {
		state := r.state
		if state == "" {
			state = string(agent.StateIdle)
		}
		title := r.title
		if runes := []rune(title); len(runes) > 60 {
			title = string(runes[:57]) + "..."
		}
		// Colour the state field.
		stateStyled := stateStyle(state).Render(fmt.Sprintf("%-8s", state))

		// Only bold worktree sessions (project@branch).
		nameStyle := styleTitle
		if strings.Contains(r.name, "@") {
			nameStyle = styleName
		}

		fmt.Printf("%s  %s  %s  %s  %s\n",
			nameStyle.Render(fmt.Sprintf("%-40s", r.name)),
			stateStyled,
			styleTitle.Render(fmt.Sprintf("%-6s", r.port)),
			styleTitle.Render(fmt.Sprintf("%-10s", r.harness)),
			styleTitle.Render(title),
		)
	}
	return nil
}

// proxyListSessionsAndRender proxies a list-sessions request to the host-API
// sidecar and renders the result as a session table.
func proxyListSessionsAndRender(apiURL string, showAll bool) error {
	raw, err := proxyListSessions(apiURL, showAll)
	if err != nil {
		return err
	}

	var ss []db.Status
	if err := json.Unmarshal(raw, &ss); err != nil {
		return fmt.Errorf("list-sessions proxy: unmarshal response: %w", err)
	}

	// Proxy path has no DB access; pass nil groupParents so renderSessionTable
	// falls back to name heuristic for parent attribution.
	return renderSessionTable(ss, nil)
}
