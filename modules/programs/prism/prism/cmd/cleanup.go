package cmd

// prism cleanup — worktree + session teardown (replaces cli.tmux.worktreeCleanup)
//
// 1. Detects the current tmux session; aborts if not project@worktree.
// 2. Finds the worktree path via the agent window's pane_current_path.
// 3. Confirms with the user (y/n).
// 4. Optionally offers to delete the git branch.
// 5. Switches to scratchpad, kills the session, removes the worktree.

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── confirm prompt model ──────────────────────────────────────────────────────

type confirmModel struct {
	question string
	answer   bool
	done     bool
	width    int
}

func (m confirmModel) Init() tea.Cmd { return nil }

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch strings.ToLower(msg.String()) {
		case "y":
			m.answer = true
			m.done = true
			return m, tea.Quit
		case "n", "ctrl+c", "esc", "enter":
			m.answer = false
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m confirmModel) View() string {
	styleYellow := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorYellow))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	return "\n" + styleYellow.Render(m.question+" [y/N] ") + "\n" +
		styleDim.Render("  y confirm  n/esc cancel") + "\n"
}

func confirm(question string) bool {
	m := confirmModel{question: question}
	p := tea.NewProgram(m, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		return false
	}
	final, ok := result.(confirmModel)
	return ok && final.answer
}

// ── cleanup TUI ───────────────────────────────────────────────────────────────

type cleanupState int

const (
	stateInfo cleanupState = iota
	stateConfirmRemove
	stateConfirmBranch
	stateConfirmForceDel
	stateDone
	stateAbort
)

type cleanupModel struct {
	state        cleanupState
	session      string
	worktreeName string
	worktreePath string
	bareRoot     string
	defaultBr    string
	deleteBranch bool
	forceDelete  bool
	log          []string
	errMsg       string
	width        int
}

func newCleanupModel(session, worktreeName, worktreePath, bareRoot, defaultBr string) cleanupModel {
	return cleanupModel{
		state:        stateInfo,
		session:      session,
		worktreeName: worktreeName,
		worktreePath: worktreePath,
		bareRoot:     bareRoot,
		defaultBr:    defaultBr,
	}
}

func (m cleanupModel) Init() tea.Cmd { return nil }

func (m cleanupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width

	case tea.KeyMsg:
		key := strings.ToLower(msg.String())

		switch m.state {
		case stateInfo:
			// First keypress moves to confirm.
			m.state = stateConfirmRemove

		case stateConfirmRemove:
			if key == "y" {
				// Check if branch exists to offer deletion.
				if git.BranchExists(m.bareRoot, m.worktreeName) {
					m.state = stateConfirmBranch
				} else {
					m.state = stateDone
					return m, m.doCleanup()
				}
			} else {
				m.state = stateAbort
				return m, tea.Quit
			}

		case stateConfirmBranch:
			if key == "y" {
				m.deleteBranch = true
				// Check if merged.
				if !git.BranchMerged(m.bareRoot, m.worktreeName, m.defaultBr) {
					m.state = stateConfirmForceDel
				} else {
					m.state = stateDone
					return m, m.doCleanup()
				}
			} else {
				m.state = stateDone
				return m, m.doCleanup()
			}

		case stateConfirmForceDel:
			if key == "y" {
				m.forceDelete = true
			}
			m.state = stateDone
			return m, m.doCleanup()
		}
	}
	return m, nil
}

type cleanupDoneMsg struct{ err error }

func (m cleanupModel) doCleanup() tea.Cmd {
	return func() tea.Msg {
		// Remove worktree first (while still in the session).
		if err := git.RemoveWorktree(m.bareRoot, m.worktreePath); err != nil {
			return cleanupDoneMsg{fmt.Errorf("worktree remove: %w", err)}
		}

		// Delete branch if requested.
		if m.deleteBranch {
			var branchErr error
			if m.forceDelete {
				branchErr = git.ForceDeleteBranch(m.bareRoot, m.worktreeName)
			} else {
				branchErr = git.DeleteBranch(m.bareRoot, m.worktreeName)
			}
			if branchErr != nil {
				// Non-fatal — log but continue.
				_ = branchErr
			}
		}

		// Ensure scratchpad exists.
		if !tmux.HasSession("scratchpad") {
			home, _ := os.UserHomeDir()
			_ = tmux.NewSessionDetached("scratchpad", home)
			_ = tmux.RenameWindow("scratchpad:0", "term")
		}

		// Switch only this client to scratchpad, then kill the session.
		client, _ := tmux.CurrentClient()
		if client == "" {
			client = tmux.CallerClient()
		}
		if client != "" {
			_ = tmux.SwitchClient(client, "scratchpad")
		} else {
			_, _ = tmux.SwitchClientCurrent("scratchpad")
		}
		_ = tmux.KillSession(m.session)
		if d, err := openDB(); err == nil {
			_ = d.SetEnded(m.session)
			d.Close()
		}

		return cleanupDoneMsg{}
	}
}

func (m cleanupModel) View() string {
	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleYellow := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorYellow))
	styleGreen := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorGreen))
	styleRed := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorRed))

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(styleBold.Render("prism worktree cleanup"))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  session  : %s\n", styleBold.Render(m.session)))
	sb.WriteString(fmt.Sprintf("  worktree : %s\n", styleDim.Render(m.worktreePath)))
	sb.WriteString(fmt.Sprintf("  branch   : %s\n", styleBold.Render(m.worktreeName)))
	sb.WriteString("\n")

	if m.errMsg != "" {
		sb.WriteString(styleRed.Render("error: "+m.errMsg) + "\n")
		sb.WriteString(styleDim.Render("  press any key to close") + "\n")
		return sb.String()
	}

	switch m.state {
	case stateInfo:
		sb.WriteString(styleDim.Render("  press any key to continue") + "\n")

	case stateConfirmRemove:
		sb.WriteString(styleYellow.Render("remove this worktree and session? [y/N] ") + "\n")
		sb.WriteString(styleDim.Render("  y confirm  n/esc cancel") + "\n")

	case stateConfirmBranch:
		sb.WriteString(styleYellow.Render(
			fmt.Sprintf("also delete branch '%s'? [y/N] ", m.worktreeName)) + "\n")
		sb.WriteString(styleDim.Render("  y delete branch  n keep branch") + "\n")

	case stateConfirmForceDel:
		sb.WriteString(styleYellow.Render(
			fmt.Sprintf("branch '%s' is not fully merged — force delete? [y/N] ", m.worktreeName)) + "\n")
		sb.WriteString(styleDim.Render("  y force delete  n keep branch") + "\n")

	case stateDone:
		sb.WriteString(styleGreen.Render("done") + "\n")

	case stateAbort:
		sb.WriteString(styleDim.Render("cancelled") + "\n")
	}

	return sb.String()
}

// ── cobra command ─────────────────────────────────────────────────────────────

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove the current worktree session (project@branch sessions only)",
	RunE: func(cmd *cobra.Command, args []string) error {
		yesFlag, _ := cmd.Flags().GetBool("yes")
		sessionFlag, _ := cmd.Flags().GetString("session")

		// Only require tmux when we need to auto-detect the current session.
		if sessionFlag == "" && os.Getenv("TMUX") == "" {
			return fmt.Errorf("not running inside tmux — invoke via the tmux binding (prefix+W)")
		}

		var session string
		if sessionFlag != "" {
			session = sessionFlag
		} else {
			var err error
			session, err = tmux.CurrentSession()
			if err != nil || session == "" {
				return fmt.Errorf("could not determine current tmux session")
			}
		}

		if !strings.Contains(session, "@") {
			return fmt.Errorf("'%s' is not a worktree session\n  prefix+W only works in project@branch sessions", session)
		}

		parts := strings.SplitN(session, "@", 2)
		worktreeName := parts[1]

		// Find worktree path from agent window.
		worktreePath := worktreePathFromSession(session)
		if worktreePath == "" {
			return fmt.Errorf("could not determine worktree path for session %q (not found in tmux windows or DB)", session)
		}

		bareRoot := git.BareRoot(worktreePath)
		if bareRoot == "" {
			return fmt.Errorf("could not find bare repo root above %s", worktreePath)
		}

		defaultBr := git.DefaultBranchFromBareRoot(bareRoot)
		if defaultBr != "" && worktreeName == defaultBr {
			return fmt.Errorf("refusing to remove the default branch worktree '%s'\n  switch to a feature branch session first", worktreeName)
		}

		// Non-interactive path: --yes skips all prompts and runs headlessly.
		if yesFlag {
			return headlessCleanup(session, worktreeName, worktreePath, bareRoot)
		}

		// Interactive TUI requires tmux (bubbletea needs a real TTY).
		if os.Getenv("TMUX") == "" {
			return fmt.Errorf("interactive cleanup requires tmux — use --yes for non-interactive use outside tmux")
		}

		m := newCleanupModel(session, worktreeName, worktreePath, bareRoot, defaultBr)
		prog := tea.NewProgram(cleanupWrapper{m}, tea.WithAltScreen())
		_, runErr := prog.Run()
		return runErr
	},
}

// headlessCleanup removes the worktree and session without any TUI interaction.
// It always deletes the branch if it is already merged; if unmerged it skips
// branch deletion (safe default — the orchestrator should only call this after
// confirming the PR has been merged).
func headlessCleanup(session, worktreeName, worktreePath, bareRoot string) error {
	fmt.Printf("removing worktree %s...\n", worktreePath)
	if err := git.RemoveWorktree(bareRoot, worktreePath); err != nil {
		return fmt.Errorf("worktree remove: %w", err)
	}

	if git.BranchExists(bareRoot, worktreeName) {
		if git.BranchMerged(bareRoot, worktreeName, git.DefaultBranchFromBareRoot(bareRoot)) {
			fmt.Printf("deleting merged branch %s...\n", worktreeName)
			_ = git.DeleteBranch(bareRoot, worktreeName)
		} else {
			fmt.Printf("branch %s is not fully merged — skipping branch deletion\n", worktreeName)
		}
	}

	// Ensure the scratchpad exists so we have somewhere to send the client.
	if !tmux.HasSession("scratchpad") {
		home, _ := os.UserHomeDir()
		_ = tmux.NewSessionDetached("scratchpad", home)
		_ = tmux.RenameWindow("scratchpad:0", "term")
	}

	// Redirect only clients that are currently attached to the target session.
	// We must not touch the orchestrator's own client.
	if clients, err := tmux.ListClients(); err == nil {
		for _, c := range clients {
			if sess, err := tmux.ClientSession(c); err == nil && sess == session {
				_ = tmux.SwitchClient(c, "scratchpad")
			}
		}
	}

	fmt.Printf("killing session %s\n", session)
	_ = tmux.KillSession(session)
	if d, err := openDB(); err == nil {
		_ = d.SetEnded(session)
		d.Close()
	}
	fmt.Println("done")
	return nil
}

// cleanupWrapper wraps cleanupModel to intercept cleanupDoneMsg in Update.
type cleanupWrapper struct{ cleanupModel }

func (w cleanupWrapper) Init() tea.Cmd { return w.cleanupModel.Init() }
func (w cleanupWrapper) View() string  { return w.cleanupModel.View() }

func (w cleanupWrapper) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, ok := msg.(cleanupDoneMsg); ok {
		if done.err != nil {
			w.cleanupModel.errMsg = done.err.Error()
		}
		return w, tea.Quit
	}
	m, cmd := w.cleanupModel.Update(msg)
	w.cleanupModel = m.(cleanupModel)
	return w, cmd
}

func worktreePathFromSession(session string) string {
	// Try tmux first (works when the session is still alive).
	out, err := tmux.ListWindows(session)
	if err == nil && out != "" {
		// Prefer agent window, fall back to term.
		term := ""
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(line, "|", 2)
			if len(parts) != 2 {
				continue
			}
			name, path := parts[0], parts[1]
			if name == "agent" {
				return path
			}
			if name == "term" {
				term = path
			}
		}
		if term != "" {
			return term
		}
	}
	// Fall back to DB (works when the session no longer exists in tmux).
	d, err := openDB()
	if err != nil {
		return ""
	}
	defer d.Close()
	status, err := d.CurrentStatus(session)
	if err != nil || status == nil {
		return ""
	}
	// Only return the DB path if it still exists on disk; a stale path would
	// produce a confusing git error later rather than a clear "not found" here.
	if status.Worktree != "" {
		if _, statErr := os.Stat(status.Worktree); statErr == nil {
			return status.Worktree
		}
	}
	return ""
}

func init() {
	cleanupCmd.Flags().Bool("yes", false, "Non-interactive: skip all prompts and clean up immediately")
	cleanupCmd.Flags().String("session", "", "Target session name (default: current session)")
	rootCmd.AddCommand(cleanupCmd)
}
