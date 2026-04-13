package cmd

// prism list-sessions — print agent sessions (from DB) in a human-readable
// table. By default shows only sessions for the current repo. Use --all / -A
// to list sessions across all repos.

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

		type row struct {
			name  string
			state string
			port  string
			title string
		}

		var ss []db.Status
		if showAll {
			ss, err = d.AllActiveStatus()
		} else {
			ss, err = d.AllActiveStatusForRepo(currentRepo)
		}
		if err != nil {
			return fmt.Errorf("list-sessions: query db: %w", err)
		}

		var rows []row
		for _, s := range ss {
			title := "—"
			if s.Title != nil && *s.Title != "" {
				title = *s.Title
			}
			port := ""
			if s.OpencodePort != nil {
				port = fmt.Sprintf("%d", *s.OpencodePort)
			}
			rows = append(rows, row{name: s.SessionName, state: s.State, port: port, title: title})
		}

		// Sort rows alphabetically by session name for stable, predictable output.
		for i := 1; i < len(rows); i++ {
			key := rows[i]
			j := i - 1
			for j >= 0 && rows[j].name > key.name {
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

		fmt.Println(styleHeader.Render(fmt.Sprintf("%-40s  %-8s  %-6s  %s", "SESSION", "STATE", "PORT", "TITLE")))
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

			fmt.Printf("%s  %s  %s  %s\n",
				nameStyle.Render(fmt.Sprintf("%-40s", r.name)),
				stateStyled,
				styleTitle.Render(fmt.Sprintf("%-6s", r.port)),
				styleTitle.Render(title),
			)
		}
		return nil
	},
}

func init() {
	listSessionsCmd.Flags().BoolP("all", "A", false, "List all sessions across all repos")
	rootCmd.AddCommand(listSessionsCmd)
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

	type row struct {
		name  string
		state string
		port  string
		title string
	}

	var rows []row
	for _, s := range ss {
		title := "—"
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		port := ""
		if s.OpencodePort != nil {
			port = fmt.Sprintf("%d", *s.OpencodePort)
		}
		rows = append(rows, row{name: s.SessionName, state: s.State, port: port, title: title})
	}

	// Sort rows alphabetically by session name for stable, predictable output.
	for i := 1; i < len(rows); i++ {
		key := rows[i]
		j := i - 1
		for j >= 0 && rows[j].name > key.name {
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

	fmt.Println(styleHeader.Render(fmt.Sprintf("%-40s  %-8s  %-6s  %s", "SESSION", "STATE", "PORT", "TITLE")))
	for _, r := range rows {
		state := r.state
		if state == "" {
			state = string(agent.StateIdle)
		}
		title := r.title
		if runes := []rune(title); len(runes) > 60 {
			title = string(runes[:57]) + "..."
		}
		stateStyled := stateStyle(state).Render(fmt.Sprintf("%-8s", state))
		nameStyle := styleTitle
		if strings.Contains(r.name, "@") {
			nameStyle = styleName
		}
		fmt.Printf("%s  %s  %s  %s\n",
			nameStyle.Render(fmt.Sprintf("%-40s", r.name)),
			stateStyled,
			styleTitle.Render(fmt.Sprintf("%-6s", r.port)),
			styleTitle.Render(title),
		)
	}
	return nil
}
