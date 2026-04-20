package cmd

// prism cleanup — session teardown (replaces cli.tmux.worktreeCleanup)
//
// There are three distinct cases:
//
//  1. Non-worktree sessions (no "@" in name, e.g. "obsidian"):
//     Soft cleanup — kill the tmux session, kill the sidecar, mark the DB as
//     ended. No git operations.
//
//  2. Default-branch sessions (e.g. repo@main):
//     Soft cleanup — same as above, but the worktree and branch are kept intact.
//
//  3. Feature-branch sessions (e.g. repo@feature):
//     Full cleanup — remove the worktree, optionally delete the branch, kill
//     the tmux session, kill the sidecar, mark the DB as ended.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/review"
	prismSession "github.com/prismatic-koi/prism/internal/session"
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
		prismSession.KillSidecar(m.session)
		if d, err := openDB(); err == nil {
			// Kill and clean up all review sessions spawned by this parent session,
			// including their port allocations and DB rows.
			review.CleanupReviewSessionsForParent(d, m.session)
			if !hostModeFromDB(d, m.session) {
				removeContainerIfExists(m.session)
			}
			if releaseErr := d.ReleasePort(m.session); releaseErr != nil {
				fmt.Fprintf(os.Stderr, "[prism] doCleanup: release port: %v\n", releaseErr)
			}
			_ = d.SetEnded(m.session)
			_ = d.PurgeBusMessages(m.session)
			d.Close()
		} else {
			// DB unavailable — still attempt container removal conservatively.
			review.KillReviewSessionsForParent(m.session)
			removeContainerIfExists(m.session)
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
	Short: "Close and clean up a worktree session",
	RunE: func(cmd *cobra.Command, args []string) error {
		yesFlag, _ := cmd.Flags().GetBool("yes")
		sessionFlag, _ := cmd.Flags().GetString("session")

		// Inside a container: proxy all cleanup invocations to the host sidecar.
		// The proxy handles both --yes (headless) and any future interactive paths.
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			target := sessionFlag
			if target == "" {
				return fmt.Errorf("--session is required when running inside a container")
			}
			return proxyToHostAPI(apiURL, "/cleanup", map[string]any{
				"session": target,
				"yes":     yesFlag,
			}, nil)
		}

		// Only require tmux when we need to auto-detect the current session.
		if sessionFlag == "" && os.Getenv("TMUX") == "" {
			return fmt.Errorf("interactive cleanup requires tmux — use --yes for non-interactive use outside tmux")
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
			// Non-worktree session (e.g. "obsidian"): perform a soft cleanup —
			// kill the tmux session, kill the sidecar, mark the DB as ended.
			// No git operations (no worktree removal, no branch deletion).
			if yesFlag {
				return headlessCloseSession(session)
			}
			// Interactive path: simplified confirmation (no worktree/branch prompts).
			if os.Getenv("TMUX") == "" {
				return fmt.Errorf("interactive cleanup requires tmux — use --yes for non-interactive use outside tmux")
			}
			if !confirm(fmt.Sprintf("Close session %s?", session)) {
				return nil
			}
			// Use headlessCloseSession so all attached clients are redirected to
			// scratchpad (not just the calling client).
			return headlessCloseSession(session)
		}

		parts := strings.SplitN(session, "@", 2)
		worktreeName := parts[1]

		// Find worktree path from agent window.
		worktreePath := worktreePathFromSession(session)

		// When the worktree path cannot be determined (tmux session gone AND DB
		// path missing on disk), we can still clean up the sidecar/container,
		// release the DB port, and mark the session as ended.  This is only
		// supported in the headless (--yes) path; interactive cleanup needs a
		// real worktree to display info.
		if worktreePath == "" {
			if yesFlag {
				fmt.Fprintf(os.Stderr, "[prism] warning: could not determine worktree path for session %q — skipping worktree removal\n", session)
				return headlessCleanup(session, worktreeName, "", "")
			}
			return fmt.Errorf("could not determine worktree path for session %q (not found in tmux windows or DB)", session)
		}

		bareRoot := git.BareRoot(worktreePath)
		if bareRoot == "" {
			return fmt.Errorf("could not find bare repo root above %s", worktreePath)
		}

		defaultBr := git.DefaultBranchFromBareRoot(bareRoot)
		isDefaultBranch := defaultBr != "" && worktreeName == defaultBr

		// DB-backed coordinator check: prefer root_agent_name == "coordinator"
		// over the default-branch heuristic. Fallback to the heuristic when the
		// DB row is absent or root_agent_name is NULL (pre-migration).
		isCoordinatorSession := false
		if d, dbErr := openDB(); dbErr == nil {
			rootName, rowExists, rootErr := d.RootAgentName(session)
			d.Close()
			if rootErr == nil && rowExists && rootName != "" {
				isCoordinatorSession = rootName == "coordinator"
				if isCoordinatorSession != isDefaultBranch {
					fmt.Fprintf(os.Stderr, "[debug] cleanup: isCoordinator(%q): DB says %v (root_agent_name=%q), branch heuristic says %v\n",
						session, isCoordinatorSession, rootName, isDefaultBranch)
				}
			} else {
				// Pre-migration fallback: use branch-name heuristic.
				if rootErr != nil {
					fmt.Fprintf(os.Stderr, "[prism] warning: cleanup: DB error reading root_agent_name for %q: %v — using branch heuristic\n", session, rootErr)
				} else if rowExists {
					// Row exists but root_agent_name is NULL — pre-migration.
					fmt.Fprintf(os.Stderr, "[deprecation] cleanup: root_agent_name NULL for %q — using branch heuristic\n", session)
				}
				// rowExists=false: no row yet — use heuristic silently.
				isCoordinatorSession = isDefaultBranch
			}
		} else {
			fmt.Fprintf(os.Stderr, "[prism] warning: cleanup: could not open DB for %q: %v — using branch heuristic\n", session, dbErr)
			isCoordinatorSession = isDefaultBranch
		}

		if isCoordinatorSession {
			// Default-branch sessions (e.g. repo@main): close the tmux
			// session and mark the DB as ended, but keep the worktree
			// and branch intact.
			if yesFlag {
				return headlessCloseSession(session)
			}
			// Interactive path: simplified confirmation (no worktree/branch prompts).
			if os.Getenv("TMUX") == "" {
				return fmt.Errorf("interactive cleanup requires tmux — use --yes for non-interactive use outside tmux")
			}
			if !confirm(fmt.Sprintf("Close session %s? (worktree will be kept)", session)) {
				return nil
			}
			// closeSession (not headlessCloseSession) is used here deliberately:
			// the interactive path redirects only the calling client to scratchpad,
			// not all attached clients. The user who invoked cleanup is the one
			// who gets moved; other clients remain on the session until it is killed.
			return closeSession(session)
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
//
// Both worktreePath and bareRoot may be empty when the caller could not
// determine the worktree location (e.g. tmux session already gone and DB path
// missing on disk).  In that case the worktree and branch removal steps are
// skipped, and cleanup continues with sidecar teardown and DB updates.
func headlessCleanup(session, worktreeName, worktreePath, bareRoot string) error {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyToHostAPI(apiURL, "/cleanup", map[string]any{
			"session": session,
			"yes":     true,
		}, nil)
	}

	if worktreePath == "" {
		fmt.Printf("worktree path unknown — skipping worktree removal for session %s\n", session)
	} else {
		fmt.Printf("removing worktree %s...\n", worktreePath)
		if err := git.RemoveWorktree(bareRoot, worktreePath); err != nil {
			// Non-fatal: the path may no longer be a registered git worktree
			// (e.g. already removed, or pointing at the prism source dir).
			// Log a warning and continue so sidecar/DB cleanup still happens.
			fmt.Fprintf(os.Stderr, "[prism] warning: worktree remove: %v — continuing cleanup\n", err)
		}
	}

	if bareRoot != "" && git.BranchExists(bareRoot, worktreeName) {
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
	prismSession.KillSidecar(session)
	if d, err := openDB(); err == nil {
		// Kill and clean up all review sessions spawned by this parent session,
		// including their port allocations and DB rows.
		review.CleanupReviewSessionsForParent(d, session)
		if !hostModeFromDB(d, session) {
			removeContainerIfExists(session)
		}
		if releaseErr := d.ReleasePort(session); releaseErr != nil {
			fmt.Fprintf(os.Stderr, "[prism] headlessCleanup: release port: %v\n", releaseErr)
		}
		_ = d.SetEnded(session)
		_ = d.PurgeBusMessages(session)
		d.Close()
	} else {
		// DB unavailable — still attempt container removal conservatively.
		// Also try to kill review sessions via tmux even without DB cleanup.
		review.KillReviewSessionsForParent(session)
		removeContainerIfExists(session)
	}
	fmt.Println("done")
	return nil
}

// closeSession redirects attached clients to scratchpad, kills the tmux
// session, and marks the DB as ended. It does NOT remove the worktree or
// delete the branch — used for default-branch sessions where the checkout
// must remain intact.
func closeSession(session string) error {
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
	_ = tmux.KillSession(session)
	prismSession.KillSidecar(session)
	if d, err := openDB(); err == nil {
		if !hostModeFromDB(d, session) {
			removeContainerIfExists(session)
		}
		if releaseErr := d.ReleasePort(session); releaseErr != nil {
			fmt.Fprintf(os.Stderr, "[prism] closeSession: release port: %v\n", releaseErr)
		}
		_ = d.SetEnded(session)
		_ = d.PurgeBusMessages(session)
		d.Close()
	} else {
		// DB unavailable — still attempt container removal conservatively.
		removeContainerIfExists(session)
	}
	return nil
}

// headlessCloseSession is the non-interactive soft-close path — used for both
// default-branch sessions (e.g. repo@main) and non-worktree sessions (no "@").
// It kills the tmux session and marks the DB as ended without touching the
// worktree or branch (for @main) or without any git operations (for non-@ sessions).
func headlessCloseSession(session string) error {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyToHostAPI(apiURL, "/cleanup", map[string]any{
			"session": session,
			"yes":     true,
		}, nil)
	}

	fmt.Printf("closing session %s...\n", session)

	// Ensure scratchpad exists.
	if !tmux.HasSession("scratchpad") {
		home, _ := os.UserHomeDir()
		_ = tmux.NewSessionDetached("scratchpad", home)
		_ = tmux.RenameWindow("scratchpad:0", "term")
	}

	// Redirect clients attached to the target session.
	if clients, err := tmux.ListClients(); err == nil {
		for _, c := range clients {
			if sess, err := tmux.ClientSession(c); err == nil && sess == session {
				_ = tmux.SwitchClient(c, "scratchpad")
			}
		}
	}

	fmt.Printf("killing session %s\n", session)
	_ = tmux.KillSession(session)
	prismSession.KillSidecar(session)
	if d, err := openDB(); err == nil {
		// Kill and clean up all review sessions spawned by this parent session,
		// including their port allocations and DB rows.
		review.CleanupReviewSessionsForParent(d, session)
		if !hostModeFromDB(d, session) {
			removeContainerIfExists(session)
		}
		if releaseErr := d.ReleasePort(session); releaseErr != nil {
			fmt.Fprintf(os.Stderr, "[prism] headlessCloseSession: release port: %v\n", releaseErr)
		}
		_ = d.SetEnded(session)
		_ = d.PurgeBusMessages(session)
		d.Close()
	} else {
		// DB unavailable — still attempt container removal conservatively.
		// Also try to kill review sessions via tmux even without DB cleanup.
		review.KillReviewSessionsForParent(session)
		removeContainerIfExists(session)
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

// hostModeFromDB queries the already-open database d and returns true when
// the agent_status row for sessionName has host_mode = 1. Returns false when
// the row is missing, host_mode is NULL (pre-migration), or on any error.
func hostModeFromDB(d *db.DB, sessionName string) bool {
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil {
		return false
	}
	return status.HostMode
}

// removeContainerIfExists stops and removes any podman container for the given
// prism session, and cleans up the associated temp files.
// It is idempotent: all steps are safe to call when the container does not exist.
// Called after KillSidecar to handle the case where the sidecar is already dead.
// For host-mode (non-container) sessions, the podman calls return "no such
// container" which is silently ignored.
// Each podman command gets its own independent context so that a slow or
// hung stop does not consume the budget for the rm --force fallback.
func removeContainerIfExists(sessionName string) {
	name := container.NameForSession(sessionName)

	// Stop the container (10s grace period). Own context so that a slow stop
	// cannot starve the rm --force that follows.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	stopCmd := exec.CommandContext(stopCtx, "podman", "stop", "--time", "10", name)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		if !container.IsNoSuchContainerError(string(out)) {
			fmt.Fprintf(os.Stderr, "[prism] stop container %q: %v\n", name, err)
		}
	}

	// Force-remove the container. Independent context ensures this always runs
	// even when podman stop timed out.
	rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rmCancel()
	rmCmd := exec.CommandContext(rmCtx, "podman", "rm", "--force", name)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		if !container.IsNoSuchContainerError(string(out)) {
			fmt.Fprintf(os.Stderr, "[prism] rm container %q: %v\n", name, err)
		}
	}

	// Clean up temp files created by container.Manager.Create. This list must
	// stay in sync with container.Manager.EnsureRemoved / Shutdown so that
	// a direct cleanup path (sidecar already dead) leaves no stale files
	// behind that a subsequent Create would otherwise overwrite.
	_ = os.Remove(filepath.Join(os.TempDir(), "prism-gitdir-"+name))
	_ = os.Remove(filepath.Join(os.TempDir(), "prism-wt-gitdir-"+name))
	_ = os.Remove(filepath.Join(os.TempDir(), "prism-ssh-config-"+name))
	_ = os.Remove(filepath.Join(os.TempDir(), "prism-gitconfig-"+name))
	_ = os.Remove(filepath.Join(os.TempDir(), "prism-allowed-signers-"+name))

	// Clean up the host-API Unix socket file created by the sidecar. The
	// sidecar's own shutdown path would normally remove it, but cleanup runs
	// after KillSidecar so we cannot rely on that — remove it directly.
	if sockPath, err := prismSession.SidecarHostAPIPath(sessionName); err == nil {
		_ = os.Remove(sockPath)
	}
}
