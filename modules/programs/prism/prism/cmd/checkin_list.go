package cmd

// checkin_list.go — no-argument mode: list active sessions from the DB (or
// tmux as fallback) and print a SESSION/STATE/TITLE table.

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// runCheckinNoArg lists sessions from the DB (scoped to the current repo by
// default), falling back to tmux.Sessions() if the DB is unavailable.
func runCheckinNoArg(showAll bool) error {
	// Derive currentRepo from CWD using same logic as list-sessions.
	currentRepo := ""
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.Getenv("PRISM_SPAWN_PATH")
	}
	if cwd != "" {
		currentRepo = repoFromWorktreePath(cwd)
	}

	d, dbErr := openDB()
	if dbErr == nil {
		defer d.Close()

		var (
			ss       []db.Status
			queryErr error
		)
		if showAll {
			ss, queryErr = d.AllActiveStatus()
		} else if currentRepo != "" {
			ss, queryErr = d.AllActiveStatusForRepo(currentRepo)
		} else {
			ss, queryErr = d.AllActiveStatus()
		}

		if queryErr == nil {
			return printSessionTable(ss)
		}
	}

	// DB unavailable — fall back to tmux.
	sessions, terr := tmux.Sessions()
	if terr != nil {
		return terr
	}

	if len(sessions) == 0 {
		return fmt.Errorf("no agent sessions found")
	}

	// Convert tmux sessions to a minimal Status slice for the shared renderer.
	var ss []db.Status
	for _, s := range sessions {
		if prismSession.IsMetaSession(s.Name) {
			continue
		}
		state := s.AgentState
		if state == "" {
			state = string(agent.StateIdle)
		}
		title := s.AgentTitle
		st := db.Status{
			SessionName: s.Name,
			State:       state,
		}
		if title != "" {
			st.Title = &title
		}
		ss = append(ss, st)
	}

	return printSessionTable(ss)
}

// printSessionTable renders a SESSION/STATE/TITLE table and a hint line.
// Returns an error only when the list is empty (to guide the user).
func printSessionTable(ss []db.Status) error {
	if len(ss) == 0 {
		fmt.Println("no agent sessions found")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleName := lipgloss.NewStyle().Bold(true)
	styleTitle := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render(fmt.Sprintf("%-40s  %-8s  %s", "SESSION", "STATE", "TITLE")))
	for _, s := range ss {
		state := s.State
		if state == "" {
			state = string(agent.StateIdle)
		}
		// DisplayTitle folds agent_status.issue_ref in front of the title, so
		// this table agrees with `prism sessions list` and the tmux dashboard
		// for the same row. The rows reaching here from the DB path
		// come from AllActiveStatus / AllActiveStatusForRepo, both of which
		// select issue_ref; the tmux fallback path above builds a Status with
		// no reference, so DisplayTitle returns the bare title there.
		title := s.DisplayTitle()
		if title == "" {
			title = "—"
		}
		if runes := []rune(title); len(runes) > 60 {
			title = string(runes[:57]) + "..."
		}
		stateStyled := stateStyle(state).Render(fmt.Sprintf("%-8s", state))
		nameStyle := styleTitle
		if strings.Contains(s.SessionName, "@") {
			nameStyle = styleName
		}
		fmt.Printf("%s  %s  %s\n",
			nameStyle.Render(fmt.Sprintf("%-40s", s.SessionName)),
			stateStyled,
			styleTitle.Render(title),
		)
	}

	fmt.Println()
	hint := "run `prism checkin <session>` to inspect a session"
	fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary)).Render(hint))

	return nil
}
