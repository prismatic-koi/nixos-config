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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
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
		// Orphan-sidecar safety net (issue #1751): SIGTERM any sidecar
		// processes still alive for review-group or investigator-group
		// children of this worker. Runs before the DB-backed cleanup so
		// processes whose DB rows were already purged are still caught.
		killOrphanReviewSidecars(m.session)
		if d, err := openDB(); err == nil {
			// Stop and remove child review-agent containers BEFORE removing their
			// DB rows — so a failed stop still has a record to retry against.
			stopAndRemoveChildContainers(d, m.session)
			// Kill and clean up all review sessions spawned by this parent session,
			// including their port allocations and DB rows.
			review.CleanupReviewSessionsForParent(d, m.session)
			if isolationModeFromDB(d, m.session) != "host" {
				removeContainerIfExists(m.session)
			}
			// Orphan-container sweep (#2324 Step 7). The interactive TUI
			// path does not surface a JSON envelope — the count goes into
			// the warning log only. Passing result=nil keeps the helper
			// safe even though there is nothing to record.
			applyOrphanContainerSweep(m.session, nil)
			if releaseErr := d.ReleasePort(m.session); releaseErr != nil {
				proglog.Errorf("[prism] doCleanup: release port: %v\n", releaseErr)
			}
			instanceIDForSessions := instanceIDFromStatus(d, m.session)
			isolationMode := isolationModeFromDB(d, m.session)
			_ = d.SetEnded(m.session)
			if instanceIDForSessions != "" {
				if updErr := d.UpdateSessionEnded(instanceIDForSessions, "finished"); updErr != nil {
					proglog.Errorf("[prism] doCleanup: update session ended: %v\n", updErr)
				}
				if outcomeErr := d.WriteSpawnOutcome(instanceIDForSessions); outcomeErr != nil {
					proglog.Errorf("[prism] doCleanup: write spawn outcome: %v\n", outcomeErr)
				}
			}
			// Archive the session storage, then sever the pi resume linkage
			// (issue #2219): the archive copies the same transcript JSONL the
			// sever deletes, so the sever runs second — and is skipped when
			// the archive fails so the transcript is not lost. The TUI path
			// does not surface the per-update outcome — failures are logged
			// via proglog inside the helper.
			archiveErr, _ := archiveThenSeverPiResume(d, m.session, instanceIDForSessions, isolationMode)
			if errors.Is(archiveErr, archive.ErrAlreadyExists) {
				_ = d.PurgeBusMessages(m.session)
				d.Close()
				return cleanupDoneMsg{archiveErr}
			}
			// Other archive errors are non-fatal — cleanup continues.
			if instanceIDForSessions != "" {
				// Remove the per-session work dir for this session instance
				// (issue #2213; also covers staging-HOME remnants from
				// pre-Step-5-of-#2132 sessions nested at <sessionDir>/home/).
				container.RemoveSessionWorkDir(instanceIDForSessions)
			}
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
		jsonFlag, _ := cmd.Flags().GetBool("json")
		keepWorktreeFlag, _ := cmd.Flags().GetBool("keep-worktree")

		// --json implies --yes: emitting a single structured object is
		// incompatible with an interactive TUI.
		if jsonFlag && !yesFlag {
			return fmt.Errorf("--json requires --yes (interactive cleanup is incompatible with structured output)")
		}

		// Inside a container: proxy all cleanup invocations to the host sidecar.
		// The proxy handles both --yes (headless) and any future interactive paths.
		// stdout and stderr from the host-side subprocess are forwarded verbatim,
		// so the container caller sees the same per-resource progress lines as a
		// host invocation (issue #1527).
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			target := sessionFlag
			if target == "" {
				return fmt.Errorf("--session is required when running inside a container")
			}
			return proxyCleanupToHostAPI(apiURL, target, yesFlag, jsonFlag, keepWorktreeFlag)
		}

		// Only require tmux when we need to auto-detect the current session.
		if sessionFlag == "" && os.Getenv("TMUX") == "" {
			return fmt.Errorf("interactive cleanup requires tmux — use --yes for non-interactive use outside tmux")
		}

		var session string
		if sessionFlag != "" {
			session = sessionFlag
			// Validate the session name early against the DB so a typo produces
			// a helpful enumerated error instead of a later "worktree not found".
			if d, dbErr := openDB(); dbErr == nil {
				defer d.Close()
				st, stErr := d.CurrentStatus(session)
				if stErr == nil && st == nil {
					// Session not in DB — list active sessions to aid recovery.
					names, _ := activeSessionNamesForError(d, 10)
					if len(names) == 0 {
						return fmt.Errorf("--session %q not found — no active sessions in DB", session)
					}
					return fmt.Errorf("--session must be one of: %s (got: %q)", strings.Join(names, ", "), session)
				}
			}
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
				return headlessCloseSessionWithJSON(session, jsonFlag)
			}
			_ = keepWorktreeFlag // soft close is already implied for non-worktree sessions
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
			// Primary lookup failed (tmux gone, DB path missing or not on
			// disk).  Attempt a filesystem probe at the conventional location
			// <bare-root>/<branch>/ before giving up.
			probed, probedBareRoot := probeConventionalWorktreePath(session, worktreeName)
			if probed != "" {
				worktreePath = probed
			} else if yesFlag {
				if probedBareRoot != "" {
					// We know where the worktree *should* be but it isn't
					// there — log an actionable message and continue.
					proglog.Warnf("[prism] warning: worktree not found at conventional path %s — skipping worktree removal\n",
						filepath.Join(probedBareRoot, worktreeName))
				} else {
					proglog.Warnf("[prism] warning: could not determine worktree path for session %q — skipping worktree removal\n", session)
				}
				return headlessCleanupWithJSON(session, worktreeName, "", "", jsonFlag)
			} else {
				return fmt.Errorf("could not determine worktree path for session %q (not found in tmux windows or DB)", session)
			}
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
		isCoordinatorSession := isCoordinatorFromDB(session, isDefaultBranch)

		if isCoordinatorSession {
			// Default-branch sessions (e.g. repo@main): close the tmux
			// session and mark the DB as ended, but keep the worktree
			// and branch intact.
			if yesFlag {
				return headlessCloseSessionWithJSON(session, jsonFlag)
			}
			_ = keepWorktreeFlag // soft close is already implied for coordinator sessions
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
			// --keep-worktree forces a soft close on this worker session
			// (issue #2179): preserve the worktree and branch instead of
			// removing them. Coordinator and non-worktree sessions already
			// soft-close above, so this branch only affects worker sessions.
			if keepWorktreeFlag {
				return headlessCloseSessionWithJSON(session, jsonFlag)
			}
			return headlessCleanupWithJSON(session, worktreeName, worktreePath, bareRoot, jsonFlag)
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

// cleanupResult tracks per-resource outcomes for one headless cleanup. It is
// the source of truth for both the textual progress lines (host-path output)
// and the structured --json object. Empty/false fields mean the corresponding
// resource was not touched (e.g. worktree path unknown — see
// worktreeRemoved=nil for the JSON encoding).
//
// The three lifecycle-bookkeeping fields (EndedAtStamped, HarnessPortReleased,
// HarnessSessionIDCleared) carry a heterogeneous value per the issue #2134
// contract: `true` on success (or idempotent no-op — the row is in the
// cleaned-up state), a string describing the failure on error, or null when
// the DB block was never reached (e.g. the function returned early via the
// PRISM_HOST_API proxy short-circuit at the top of the function). The `any`
// type marshals as `true | "err msg" | null` directly through encoding/json.
type cleanupResult struct {
	Session                 string  `json:"session"`
	WorktreeRemoved         *string `json:"worktree_removed"` // nil when no worktree was touched
	BranchDeleted           *string `json:"branch_deleted"`   // nil when no branch was deleted
	SessionKilled           bool    `json:"session_killed"`
	EndedAtStamped          any     `json:"ended_at_stamped"`
	HarnessPortReleased     any     `json:"harness_port_released"`
	HarnessSessionIDCleared any     `json:"harness_session_id_cleared"`
	// ContainersSwept reports the number of orphan containers
	// (matching prism-<session>-<8 hex> on the host) removed by the
	// containers_enabled=1 sweep. nil omits the field entirely from
	// the JSON envelope per the #2324 contract: when containers_enabled=0,
	// the cleanup path issues NO podman commands AND emits no
	// containers_swept key. *int (not int + omitempty) is used so a
	// run that produces zero matches still emits "containers_swept": 0.
	ContainersSwept *int `json:"containers_swept,omitempty"`
}

// applyDBLifecycleClears performs the agent_status lifecycle updates for
// sessionName — release harness_port and stamp ended_at — and records
// per-update outcomes into result. Each outcome is `true` on success
// (including idempotent no-ops where the column is already in the cleaned-up
// state) or a string describing the failure.
//
// Issue #2134: prior to this helper, errors from these calls were either
// silently discarded (`_ = d.SetEnded(...)`) or only logged via proglog. The
// JSON envelope had no fields for them, so operators had no way to verify
// whether the bookkeeping actually ran.
//
// The harness_session_id clear (severPiResumeLinkage) is intentionally NOT
// part of this helper: the sever deletes the pi transcript JSONL that the
// session archive copies, so it must run after runSessionArchive — see
// archiveThenSeverPiResume (issue #2219). Callers populate
// result.HarnessSessionIDCleared from that helper's outcome.
func applyDBLifecycleClears(d *db.DB, sessionName string, result *cleanupResult) {
	// 1. Release the harness port. ReleasePort is idempotent on existing
	//    rows (UPDATE on an already-NULL port is a no-op success); the
	//    "session not found" error only fires when the agent_status row
	//    does not exist at all, which the cleanupCmd.RunE validation
	//    rejects up-front for direct cleanup invocations.
	if err := d.ReleasePort(sessionName); err != nil {
		result.HarnessPortReleased = err.Error()
		proglog.Errorf("[prism] cleanup: release port for %s: %v\n", sessionName, err)
	} else {
		result.HarnessPortReleased = true
	}

	// 2. Stamp ended_at. SetEnded internally guards with `AND ended_at IS
	//    NULL` so re-invocations on an already-ended row succeed as a
	//    no-op (idempotent). The LIKE cascade also stamps ended_at on any
	//    review-child rows whose parent is sessionName.
	if err := d.SetEnded(sessionName); err != nil {
		result.EndedAtStamped = err.Error()
		proglog.Errorf("[prism] cleanup: set ended for %s: %v\n", sessionName, err)
	} else {
		result.EndedAtStamped = true
	}
}

// markDBLifecycleSkipped populates the three lifecycle fields with the same
// error description so the JSON envelope clearly tells the operator that the
// bookkeeping was NOT attempted (DB couldn't be opened). Without this, those
// fields would marshal as `null` and the operator could not distinguish
// "not attempted" from "silently skipped without explanation" — exactly the
// silent-failure mode issue #2134 calls out.
func markDBLifecycleSkipped(result *cleanupResult, reason string) {
	result.HarnessPortReleased = reason
	result.HarnessSessionIDCleared = reason
	result.EndedAtStamped = reason
}

// emitCleanupJSON writes the cleanup result as a single JSON object on a line
// to os.Stdout. Used by the --json path; mutually exclusive with the textual
// per-step lines.
func emitCleanupJSON(r cleanupResult) {
	emitCleanupJSONTo(os.Stdout, r)
}

// emitCleanupJSONTo writes the cleanup result as a single JSON object on a
// line to the supplied writer. Used by callers (e.g. `prism close --yes`)
// that need to redirect the JSON envelope to io.Discard for popup-safe
// silent-on-success behaviour while preserving the structured-output
// contract when --json is also set.
func emitCleanupJSONTo(w io.Writer, r cleanupResult) {
	data, err := json.Marshal(r)
	if err != nil {
		// Marshal of a small fixed-shape struct should never fail; surface
		// the error on stderr so the caller has something to work with
		// rather than empty stdout.
		proglog.Warnf("[prism] warning: cleanup --json: marshal: %v\n", err)
		return
	}
	fmt.Fprintln(w, string(data))
}

// headlessCleanup removes the worktree and session without any TUI interaction.
// It always force-deletes the branch, relying on the orchestrator-trust
// contract: the orchestrator should only call this after confirming the PR has
// been merged. Force-delete is required because squash-merges produce a fresh
// commit on main with a different SHA than the branch tip, so the conventional
// "git branch --merged" check always returns false after a squash-merge.
//
// Both worktreePath and bareRoot may be empty when the caller could not
// determine the worktree location (e.g. tmux session already gone and DB path
// missing on disk).  In that case the worktree and branch removal steps are
// skipped, and cleanup continues with sidecar teardown and DB updates.
func headlessCleanup(session, worktreeName, worktreePath, bareRoot string) error {
	return headlessCleanupWithJSON(session, worktreeName, worktreePath, bareRoot, false)
}

// headlessCleanupWithJSON is the JSON-aware form of headlessCleanup. When
// jsonMode is true, per-step textual progress lines are suppressed and a
// single JSON object is emitted on success. Stderr warnings (e.g. branch
// delete failure, archive collision) are preserved in both modes per AC.
func headlessCleanupWithJSON(session, worktreeName, worktreePath, bareRoot string, jsonMode bool) error {
	return headlessCleanupWithJSONTo(session, worktreeName, worktreePath, bareRoot, jsonMode, os.Stdout)
}

// headlessCleanupWithJSONTo is the writer-aware form of
// headlessCleanupWithJSON. The `stdout` writer receives the per-step textual
// progress lines, the "done" line, and the JSON envelope. Callers that want
// popup-safe silent-on-success behaviour (e.g. `prism close --yes` from a
// tmux keybind) pass io.Discard; the standard CLI path passes os.Stdout.
// proglog warnings continue to fire to os.Stderr regardless — they only
// trigger on partial failures, not on a clean happy-path success.
func headlessCleanupWithJSONTo(session, worktreeName, worktreePath, bareRoot string, jsonMode bool, stdout io.Writer) error {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyCleanupToHostAPI(apiURL, session, true, jsonMode, false)
	}

	result := cleanupResult{Session: session}

	printLine := func(format string, a ...any) {
		if !jsonMode {
			fmt.Fprintf(stdout, format, a...)
		}
	}

	if worktreePath == "" {
		printLine("worktree path unknown — skipping worktree removal for session %s\n", session)
	} else if isSafeToRemoveWorktree(session, worktreePath, bareRoot) {
		printLine("removing worktree %s...\n", worktreePath)
		if err := git.RemoveWorktree(bareRoot, worktreePath); err != nil {
			// Non-fatal: the path may no longer be a registered git worktree
			// (e.g. already removed, or pointing at the prism source dir).
			// Log a warning and continue so sidecar/DB cleanup still happens.
			proglog.Warnf("[prism] warning: worktree remove: %v — continuing cleanup\n", err)
		} else {
			wt := worktreePath
			result.WorktreeRemoved = &wt
		}
	} else {
		proglog.Warnf("[prism] warning: cleanup: refusing to remove worktree %s — path matches main worktree or an active session's worktree; skipping filesystem removal\n", worktreePath)
	}

	if bareRoot != "" && git.BranchExists(bareRoot, worktreeName) {
		printLine("deleting branch %s...\n", worktreeName)
		if err := git.ForceDeleteBranch(bareRoot, worktreeName); err != nil {
			proglog.Warnf("[prism] warning: branch delete: %v — continuing cleanup\n", err)
		} else {
			bn := worktreeName
			result.BranchDeleted = &bn
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

	printLine("killing session %s\n", session)
	_ = tmux.KillSession(session)
	prismSession.KillSidecar(session)
	// Orphan-sidecar safety net (issue #1751): SIGTERM any sidecar
	// processes still alive for review-group or investigator-group
	// children of this worker. Done before DB cleanup so processes
	// whose DB rows have already been purged are still caught.
	killOrphanReviewSidecars(session)
	// session_killed reflects the post-condition (tmux session is gone),
	// which is true whether the session was alive or already-dead before this
	// call. KillSession's error is intentionally ignored above (it returns
	// non-nil for already-dead sessions, which is not a failure for our
	// purposes), so we set the field unconditionally here.
	result.SessionKilled = true
	// archiveCollisionWarning records a non-fatal archive-already-exists
	// outcome so it can be surfaced as a stderr warning at the end of cleanup
	// rather than aborting (per the issue-comment AC: archive.ErrAlreadyExists
	// is a benign collision once worktree/branch/session are already torn
	// down). Other steps continue as normal.
	var archiveCollisionWarning string
	if d, err := openDB(); err == nil {
		// Stop and remove child review-agent containers BEFORE removing their
		// DB rows — so a failed stop still has a record to retry against.
		stopAndRemoveChildContainers(d, session)
		// Kill and clean up all review sessions spawned by this parent session,
		// including their port allocations and DB rows.
		review.CleanupReviewSessionsForParent(d, session)
		if isolationModeFromDB(d, session) != "host" {
			removeContainerIfExists(session)
		}
		// Orphan-container sweep (#2324 Step 7). Gated on the
		// session's agent_status.containers_enabled column inside
		// sweepOrphanContainersForSession; when the flag is 0 this
		// is a fast DB read and no podman commands are issued. Runs
		// after removeContainerIfExists so the session's own
		// runtime container has already been torn down — the sweep
		// only deals with DERIVATIVE containers the agent created
		// via the proxy.
		applyOrphanContainerSweep(session, &result)
		// Capture instance_id and isolation_mode before SetEnded clears the
		// row's lifecycle fields. Done before applyDBLifecycleClears so the
		// CurrentStatus reads inside instanceIDFromStatus still observe the
		// pre-cleanup row.
		instanceIDForSessions := instanceIDFromStatus(d, session)
		isolationMode := isolationModeFromDB(d, session)
		// Apply the lifecycle clears — release harness_port and stamp
		// ended_at — and capture per-update outcomes for the JSON envelope
		// (issue #2134). The harness_session_id clear runs later, after the
		// archive (issue #2219).
		applyDBLifecycleClears(d, session, &result)
		if instanceIDForSessions != "" {
			if updErr := d.UpdateSessionEnded(instanceIDForSessions, "finished"); updErr != nil {
				proglog.Errorf("[prism] headlessCleanup: update session ended: %v\n", updErr)
			}
			if outcomeErr := d.WriteSpawnOutcome(instanceIDForSessions); outcomeErr != nil {
				proglog.Errorf("[prism] headlessCleanup: write spawn outcome: %v\n", outcomeErr)
			}
		}
		// Archive the session storage, then sever the pi resume linkage
		// (issue #2219): the archive copies the same transcript JSONL the
		// sever deletes, so the sever runs second — and is skipped when the
		// archive fails so the transcript is not lost.
		archiveErr, severOutcome := archiveThenSeverPiResume(d, session, instanceIDForSessions, isolationMode)
		result.HarnessSessionIDCleared = severOutcome
		if errors.Is(archiveErr, archive.ErrAlreadyExists) {
			// Non-fatal: by this point the worktree, branch, tmux
			// session, and DB rows are already torn down. The collision
			// just means a previous cleanup attempt for the same
			// instance_id already wrote the archive directory. Surface
			// it as a stderr warning instead of aborting (issue #1527
			// follow-up).
			archiveCollisionWarning = archiveErr.Error()
		}
		// Other archive errors are non-fatal — cleanup continues.
		if instanceIDForSessions != "" {
			// Remove the per-session work dir for this session instance
			// (issue #2213; also covers staging-HOME remnants from
			// pre-Step-5-of-#2132 sessions). Non-fatal and idempotent —
			// silently skips when the directory does not exist (e.g.
			// non-sandbox-exec sessions).
			container.RemoveSessionWorkDir(instanceIDForSessions)
		}
		_ = d.PurgeBusMessages(session)
		d.Close()
	} else {
		// DB unavailable — still attempt container removal conservatively.
		// Also try to kill review sessions via tmux even without DB cleanup.
		// Issue #2134: report the skip in the JSON envelope so operators can
		// distinguish "DB bookkeeping ran successfully" from "DB was
		// unavailable and the bookkeeping never ran".
		markDBLifecycleSkipped(&result, fmt.Sprintf("db open failed: %v", err))
		proglog.Errorf("[prism] headlessCleanup: open db: %v\n", err)
		review.KillReviewSessionsForParent(session)
		removeContainerIfExists(session)
		// No sweep here: containers_enabled is unreadable without the
		// DB. Skipping the sweep matches the AC "containers_enabled=0
		// → no podman commands" in spirit: when we cannot prove the
		// flag was on, default to off.
	}
	if archiveCollisionWarning != "" {
		proglog.Warnf("[prism] warning: archive: %s — continuing cleanup\n", archiveCollisionWarning)
	}
	if jsonMode {
		emitCleanupJSONTo(stdout, result)
	} else {
		fmt.Fprintln(stdout, "done")
	}
	return nil
}

// applyOrphanContainerSweep runs the #2324 orphan-container sweep for
// session and, when the sweep actually ran (containers_enabled=1),
// records the count on result.ContainersSwept so it surfaces in the
// --json envelope. When containers_enabled=0 the field stays nil and
// the JSON encoder omits it entirely (json:"containers_swept,omitempty").
//
// Errors inside the sweep are non-fatal and logged at warning level by
// the sweep itself; the helper has no error path of its own.
func applyOrphanContainerSweep(session string, result *cleanupResult) {
	count, ran := sweepOrphanContainersForSession(session)
	if !ran {
		return
	}
	if result != nil {
		c := count
		result.ContainersSwept = &c
	}
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
		if isolationModeFromDB(d, session) != "host" {
			removeContainerIfExists(session)
		}
		// Orphan-container sweep (#2324 Step 7). closeSession is the
		// interactive @main / non-worktree path and does not surface a
		// JSON envelope; the helper records the count into the log only.
		applyOrphanContainerSweep(session, nil)
		if releaseErr := d.ReleasePort(session); releaseErr != nil {
			proglog.Errorf("[prism] closeSession: release port: %v\n", releaseErr)
		}
		instanceIDForSessions := instanceIDFromStatus(d, session)
		isolationMode := isolationModeFromDB(d, session)
		_ = d.SetEnded(session)
		if instanceIDForSessions != "" {
			if updErr := d.UpdateSessionEnded(instanceIDForSessions, "finished"); updErr != nil {
				proglog.Errorf("[prism] closeSession: update session ended: %v\n", updErr)
			}
			if outcomeErr := d.WriteSpawnOutcome(instanceIDForSessions); outcomeErr != nil {
				proglog.Errorf("[prism] closeSession: write spawn outcome: %v\n", outcomeErr)
			}
		}
		// Archive the session storage, then sever the pi resume linkage
		// (issue #2219): the archive copies the same transcript JSONL the
		// sever deletes, so the sever runs second — and is skipped when the
		// archive fails so the transcript is not lost. closeSession is the
		// interactive @main path and does not surface a JSON envelope — the
		// per-update outcome is captured by the proglog warnings inside the
		// helper.
		archiveErr, _ := archiveThenSeverPiResume(d, session, instanceIDForSessions, isolationMode)
		if errors.Is(archiveErr, archive.ErrAlreadyExists) {
			_ = d.PurgeBusMessages(session)
			d.Close()
			return archiveErr
		}
		// Other archive errors are non-fatal — cleanup continues.
		if instanceIDForSessions != "" {
			// Remove the per-session work dir for this session instance
			// (issue #2213; also covers staging-HOME remnants from
			// pre-Step-5-of-#2132 sessions).
			container.RemoveSessionWorkDir(instanceIDForSessions)
		}
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
	return headlessCloseSessionWithJSON(session, false)
}

// headlessCloseSessionWithJSON is the JSON-aware form of headlessCloseSession.
// When jsonMode is true, per-step textual progress lines are suppressed and a
// single JSON object is emitted on success (worktree_removed=null,
// branch_deleted=null since this path never touches them).
func headlessCloseSessionWithJSON(session string, jsonMode bool) error {
	return headlessCloseSessionWithJSONTo(session, jsonMode, os.Stdout)
}

// headlessCloseSessionWithJSONTo is the writer-aware form of
// headlessCloseSessionWithJSON. See headlessCleanupWithJSONTo for the
// silent-on-success rationale; identical contract on stdout.
func headlessCloseSessionWithJSONTo(session string, jsonMode bool, stdout io.Writer) error {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		// The host-side `prism cleanup` already implements the soft-close
		// path for coordinator and non-@ sessions. We don't need
		// keep-worktree here because the host's session-routing logic in
		// cleanupCmd.RunE already chooses the soft path on its own. For
		// container-mode `prism close --keep-worktree` on a worker session,
		// see proxyCloseToHostAPI in close.go, which goes through /close.
		return proxyCleanupToHostAPI(apiURL, session, true, jsonMode, false)
	}

	result := cleanupResult{Session: session}
	printLine := func(format string, a ...any) {
		if !jsonMode {
			fmt.Fprintf(stdout, format, a...)
		}
	}

	printLine("closing session %s...\n", session)

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

	printLine("killing session %s\n", session)
	result.SessionKilled = true
	_ = tmux.KillSession(session)
	prismSession.KillSidecar(session)
	// Orphan-sidecar safety net (issue #1751): SIGTERM any sidecar
	// processes still alive for review-group or investigator-group
	// children of this session. Done before DB cleanup so processes
	// whose DB rows have already been purged are still caught.
	killOrphanReviewSidecars(session)
	var archiveCollisionWarning string
	if d, err := openDB(); err == nil {
		// Stop and remove child review-agent containers BEFORE removing their
		// DB rows — so a failed stop still has a record to retry against.
		stopAndRemoveChildContainers(d, session)
		// Kill and clean up all review sessions spawned by this parent session,
		// including their port allocations and DB rows.
		review.CleanupReviewSessionsForParent(d, session)
		if isolationModeFromDB(d, session) != "host" {
			removeContainerIfExists(session)
		}
		// Orphan-container sweep (#2324 Step 7). See headlessCleanupWithJSONTo
		// for the placement rationale — the gate is in
		// sweepOrphanContainersForSession, so calling it on a non-container
		// session (containers_enabled=0) is a fast DB read with no podman
		// commands issued.
		applyOrphanContainerSweep(session, &result)
		instanceIDForSessions := instanceIDFromStatus(d, session)
		isolationMode := isolationModeFromDB(d, session)
		// Apply the lifecycle clears — release harness_port and stamp
		// ended_at — and capture per-update outcomes for the JSON envelope
		// (issue #2134). The harness_session_id clear runs later, after the
		// archive (issue #2219).
		applyDBLifecycleClears(d, session, &result)
		if instanceIDForSessions != "" {
			if updErr := d.UpdateSessionEnded(instanceIDForSessions, "finished"); updErr != nil {
				proglog.Errorf("[prism] headlessCloseSession: update session ended: %v\n", updErr)
			}
			if outcomeErr := d.WriteSpawnOutcome(instanceIDForSessions); outcomeErr != nil {
				proglog.Errorf("[prism] headlessCloseSession: write spawn outcome: %v\n", outcomeErr)
			}
		}
		// Archive the session storage, then sever the pi resume linkage
		// (issue #2219): the archive copies the same transcript JSONL the
		// sever deletes, so the sever runs second — and is skipped when the
		// archive fails so the transcript is not lost.
		archiveErr, severOutcome := archiveThenSeverPiResume(d, session, instanceIDForSessions, isolationMode)
		result.HarnessSessionIDCleared = severOutcome
		if errors.Is(archiveErr, archive.ErrAlreadyExists) {
			// Non-fatal: see headlessCleanup for rationale.
			archiveCollisionWarning = archiveErr.Error()
		}
		// Other archive errors are non-fatal — cleanup continues.
		if instanceIDForSessions != "" {
			// Remove the per-session work dir for this session instance
			// (issue #2213; also covers staging-HOME remnants from
			// pre-Step-5-of-#2132 sessions).
			container.RemoveSessionWorkDir(instanceIDForSessions)
		}
		_ = d.PurgeBusMessages(session)
		d.Close()
	} else {
		// DB unavailable — still attempt container removal conservatively.
		// Also try to kill review sessions via tmux even without DB cleanup.
		// Issue #2134: report the skip in the JSON envelope.
		markDBLifecycleSkipped(&result, fmt.Sprintf("db open failed: %v", err))
		proglog.Errorf("[prism] headlessCloseSession: open db: %v\n", err)
		review.KillReviewSessionsForParent(session)
		removeContainerIfExists(session)
	}
	if archiveCollisionWarning != "" {
		proglog.Warnf("[prism] warning: archive: %s — continuing cleanup\n", archiveCollisionWarning)
	}
	if jsonMode {
		emitCleanupJSONTo(stdout, result)
	} else {
		fmt.Fprintln(stdout, "done")
	}
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

// probeConventionalWorktreePath attempts to locate a worktree for the given
// session at the conventional bare-root layout location: <bare-root>/<branch>/.
//
// It derives the bare-root hint from the DB row's worktree_path column (using
// the parent directory of the stored path, even if that path no longer exists
// on disk).  This handles the case where the session died before its worktree
// was visible to the normal worktreePathFromSession lookup (e.g. the stored
// path points at a now-deleted directory, but the worktree was re-created at
// the conventional sibling location).
//
// Return values:
//   - (probed, bareRoot) where probed is non-empty when a worktree was found at
//     the conventional location, and bareRoot is non-empty whenever we could
//     derive a candidate bare-root from the DB (even when the probe failed).
//   - ("", "") when there is no DB row or the row contains no worktree path.
func probeConventionalWorktreePath(session, worktreeName string) (worktreePath, bareRoot string) {
	d, err := openDB()
	if err != nil {
		return "", ""
	}
	defer d.Close()
	status, err := d.CurrentStatus(session)
	if err != nil || status == nil || status.Worktree == "" {
		return "", ""
	}
	// Derive the bare-root as the parent of the stored worktree path.
	// E.g. if the DB contains "/code/nixos-config/sandbox-exec-smoke", the
	// bare root is "/code/nixos-config" and we probe
	// "/code/nixos-config/<worktreeName>/.git".
	candidateBareRoot := filepath.Dir(status.Worktree)
	candidate := filepath.Join(candidateBareRoot, worktreeName)
	if _, statErr := os.Stat(filepath.Join(candidate, ".git")); statErr == nil {
		return candidate, candidateBareRoot
	}
	return "", candidateBareRoot
}

// runSessionArchive performs the harness-storage copy for the session identified by
// instanceID using the already-open DB d and the agent_status isolation mode
// from statusIsolationMode. It is called after UpdateSessionEnded so the
// sessions row has ended_at and end_state populated.
//
// On success it calls UpdateSessionArchivePath to record the archive directory
// path in the DB.
//
// Return values (issue #2336): (copied, err). err is the archive.Run error
// (nil on success). copied reports whether the harness adapter actually
// wrote a transcript file into the archive directory — (false, nil) is the
// "nothing to copy" outcome (manifest-only archive), (true, nil) means the
// transcript survived the archive step. The caller (archiveThenSeverPiResume)
// uses copied to gate the sever step: severing when copied == false would
// delete the same source file the archive step failed to preserve.
//
// The caller is responsible for deciding whether an error should be fatal:
//   - archive.ErrAlreadyExists should be propagated — the AC requires cleanup
//     to exit non-zero when the archive directory already exists on a re-run.
//   - Other errors are treated as non-fatal (logged + cleanup continues).
//
// Skip paths that return (false, nil) — archive was not attempted and no
// transcript was preserved:
//  1. instanceID == ""
//  2. statusIsolationMode is not one of host/bwrap/sandbox-exec
//  3. SessionByInstanceID errors or returns nil
//  4. no ArchiveAdapter registered for sess.Harness
//  5. archiveAdapter.SourcePath returns an error
//
// The 6th path (SourcePath returns a sentinel non-existent path with nil
// error, or a directory) is signalled by the adapter's Archive method
// returning (false, nil): the manifest-only archive is still written and
// archive_path is still populated in the DB, but the sever must be skipped.
func runSessionArchive(d *db.DB, sessionName, instanceID, statusIsolationMode string) (copied bool, err error) {
	if instanceID == "" {
		// No instance_id — nothing to archive.
		return false, nil
	}

	// Validate isolation mode before doing any work.
	switch statusIsolationMode {
	case "host", "bwrap", "sandbox-exec":
		// OK — supported modes.
	default:
		proglog.Warnf("[prism] archive: skipping session %q — unsupported isolation mode %q\n",
			sessionName, statusIsolationMode)
		return false, nil
	}

	// Fetch the full sessions row (has ended_at/end_state set by caller).
	sess, sessErr := d.SessionByInstanceID(instanceID)
	if sessErr != nil {
		proglog.Warnf("[prism] archive: get session %q: %v — skipping archive\n", instanceID, sessErr)
		return false, nil
	}
	if sess == nil {
		// No sessions row — pre-migration or session that never inserted.
		proglog.Warnf("[prism] archive: no sessions row for instance_id %q — skipping archive\n", instanceID)
		return false, nil
	}

	// Resolve the archive adapter for the session's harness. A missing or
	// unregistered harness is a clear error (not a nil-pointer panic).
	archiveAdapter, adapterErr := harness.ArchiveAdapterFor(sess.Harness)
	if adapterErr != nil {
		proglog.Warnf("[prism] archive: skipping session %q — %v\n", sessionName, adapterErr)
		return false, nil
	}

	// Build srcParams. Fully resolve HarnessSessionID BEFORE calling
	// SourcePath (issue #2336): the old code did the sessions-NULL fallback
	// after SourcePath had already been invoked with the empty value, so the
	// pi adapter's file-scan fell through to the sessions-root branch and
	// Archive no-op'd on the directory. Doing the fallback here ensures
	// SourcePath sees the resolved value the first time.
	srcParams := harnessarchive.SourceParams{
		SessionName:   sessionName,
		InstanceID:    instanceID,
		IsolationMode: statusIsolationMode,
		Worktree:      sess.Worktree,
	}
	if sess.HarnessSessionID != nil {
		srcParams.HarnessSessionID = *sess.HarnessSessionID
	} else {
		// sessions.harness_session_id is NULL — this can happen for sessions
		// started before UpdateHarnessSessionID was fixed to write to both
		// tables. Fall back to agent_status, which is where the sidecar
		// historically wrote the value.
		if sid, fallbackErr := d.HarnessSessionIDForInstance(instanceID); fallbackErr != nil {
			proglog.Warnf("[prism] archive: harness_session_id fallback for %q: %v\n", instanceID, fallbackErr)
		} else if sid != "" {
			srcParams.HarnessSessionID = sid
		}
	}

	// Resolve source path (harness-specific storage root) via the adapter.
	ctx := context.Background()
	srcPath, srcErr := archiveAdapter.SourcePath(srcParams)
	if srcErr != nil {
		proglog.Warnf("[prism] archive: SourcePath for session %q: %v — skipping archive\n", sessionName, srcErr)
		return false, nil
	}

	// Resolve harness version via the adapter (replaces archive.HarnessVersion()).
	harnessVersion, _ := archiveAdapter.Version(ctx)

	// Build archive params from DB fields.
	params := archive.Params{
		InstanceID:       sess.InstanceID,
		SessionName:      sess.SessionName,
		Harness:          sess.Harness,
		HarnessSessionID: srcParams.HarnessSessionID,
		Repo:             sess.Repo,
		Worktree:         sess.Worktree,
		StartedAt:        sess.StartedAt,
		EndedAt:          time.Now(), // EndedAt was just set in DB; use now as fallback
		IsolationMode:    statusIsolationMode,
		PrismVersion:     archive.PrismGitSHA(),
		HarnessVersion:   harnessVersion,
		// Copier delegates the harness-specific file-copy to the adapter's Archive
		// method, allowing any registered harness to provide its own copy logic.
		// archiveDir is the per-session archive directory itself; the adapter
		// writes its final-layout artifacts (e.g. session.jsonl for pi) directly
		// there — there is no longer a `raw/` subdirectory indirection.
		//
		// The Copier captures the outer `copied` variable so runSessionArchive
		// can propagate the adapter's copied/not-copied signal back up to
		// archiveThenSeverPiResume without changing archive.Run's signature
		// (issue #2336).
		Copier: func(copyCtx context.Context, archiveDir string) error {
			c, cErr := archiveAdapter.Archive(copyCtx, srcPath, archiveDir)
			if c {
				copied = true
			}
			return cErr
		},
	}
	if sess.AgentRole != nil {
		params.AgentRole = *sess.AgentRole
	}
	if sess.RootAgentName != nil {
		params.RootAgentName = *sess.RootAgentName
	}
	if sess.GroupID != nil {
		params.GroupID = *sess.GroupID
	}
	if sess.EndedAt != nil {
		params.EndedAt = *sess.EndedAt
	}
	if sess.EndState != nil {
		params.EndState = *sess.EndState
	}
	if sess.PrismVersion != nil {
		params.PrismVersion = *sess.PrismVersion
	}

	// Pre-resolve extra files (e.g. agent-run.log for bwrap/sandbox-exec) via
	// the container isolator registry. This path pre-dates #1142 and is kept
	// as-is: the isolator's ExtraFiles are harness-agnostic filesystem paths
	// that complement the harness adapter's storage root resolution.
	if statusIsolationMode != "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			if iso, isoErr := container.For(config.IsolationMode(statusIsolationMode), container.ConstructorOpts{Name: sessionName}); isoErr == nil {
				ap := iso.ArchivePaths(home, sessionName)
				if params.AgentRunLogPath == "" && len(ap.ExtraFiles) > 0 {
					params.AgentRunLogPath = ap.ExtraFiles[0]
				}
			}
		}
	}
	// Fallback: include the agent-run log via the legacy lookup when the
	// registry path did not populate it (e.g. statusIsolationMode == "" on
	// pre-migration rows). Non-fatal if the path cannot be resolved — non-
	// bwrap sessions never create this file.
	if params.AgentRunLogPath == "" {
		if agentRunLogPath, arLogErr := prismSession.AgentRunLogPath(sessionName); arLogErr == nil {
			params.AgentRunLogPath = agentRunLogPath
		}
	}

	archivePath, archiveErr := archive.Run(params)
	if archiveErr != nil {
		proglog.Warnf("[prism] archive: copy failed for session %q: %v\n", sessionName, archiveErr)
		return false, archiveErr
	}

	if updErr := d.UpdateSessionArchivePath(instanceID, archivePath); updErr != nil {
		proglog.Warnf("[prism] archive: update archive_path for %q: %v\n", instanceID, updErr)
	}

	// The pre-fix two-stage flow ran a separate adapter.Export here that
	// byte-copied raw/session.jsonl to session.jsonl. With pi as the only
	// remaining harness, Archive writes the final layout in one step — no
	// post-process Export is needed.
	return copied, nil
}

func init() {
	cleanupCmd.Flags().Bool("yes", false, "Non-interactive: skip all prompts and clean up immediately")
	cleanupCmd.Flags().String("session", "", "Target session name (default: current session)")
	cleanupCmd.Flags().Bool("json", false, "Emit a single JSON object describing per-resource outcomes (requires --yes)")
	cleanupCmd.Flags().Bool("keep-worktree", false, "Soft-close: preserve the worktree and branch (matches coordinator / non-@ session behaviour)")
	rootCmd.AddCommand(cleanupCmd)
}

// severPiResumeLinkage breaks the pi conversation-resume linkage for
// sessionName so that a future spawn on the same branch name (and therefore
// the same agent_status row) does NOT silently resume the now-defunct
// pi conversation (issue #2035).
//
// Two surfaces survive a plain `prism cleanup` otherwise:
//
//  1. agent_status.harness_session_id — db.SetEnded only stamps ended_at;
//     the harness_session_id column persists. cmd/agent_run.go reads it back
//     on the next spawn and threads it into container.Config, where
//     PIInvocation appends `--session <id>` to pi when a matching JSONL is
//     found on disk.
//
//  2. <piSessionsRoot>/<encodePiCWD(worktree)>/*_<harness_session_id>.jsonl
//     where <piSessionsRoot> is $PI_CODING_AGENT_DIR/sessions when the env
//     var is set, else ~/.pi/agent/sessions — see
//     internal/harness/pi/archive.go::piSessionsRoot for the authoritative
//     resolution. The on-disk JSONL transcript. The encoded-cwd directory is keyed off
//     cfg.Worktree, which is stable across a reused branch name (the worktree
//     path is derived deterministically from the branch). The same host root
//     applies to sandbox-exec sessions (#2210): pi writes there because the
//     dispatcher injects PI_CODING_AGENT_DIR into the sandbox env, so this
//     removal is the only step that deletes the transcript — the per-session
//     work-dir wipe in RemoveSessionWorkDir never touches it.
//
// Order:
//
//   - Capture worktree + harness_session_id from the live agent_status row
//     BEFORE the DB clear (the clear nulls the latter).
//   - Remove the on-disk JSONL (FS-side; non-fatal).
//   - Null the DB column (load-bearing; logged on error but cleanup
//     continues so the rest of the teardown still runs).
//
// Best-effort — no error path aborts cleanup. A failure here is logged and
// teardown proceeds: the worst case is that the next spawn on this branch
// name resumes a defunct conversation, which is recoverable by the operator,
// whereas a half-cleaned worktree/branch is not.
//
// Return value (issue #2134): the error from d.ClearHarnessSessionID (the
// load-bearing DB clear), or nil on success / when the row is absent. The
// on-disk JSONL removal is defence-in-depth and its failure is logged but
// not propagated. All four cleanup paths reach this function via
// archiveThenSeverPiResume (issue #2219), which converts the return value
// into the JSON-envelope outcome for the headless paths; the interactive
// paths (doCleanup, closeSession) rely on the proglog warning to surface
// failures.
func severPiResumeLinkage(d *db.DB, sessionName string) error {
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil {
		// No row to read — no JSONL to scope to, no DB clear needed.
		return nil
	}
	if status.HarnessSessionID != nil && *status.HarnessSessionID != "" && status.Worktree != "" {
		cfg := container.Config{
			SessionName:      sessionName,
			Worktree:         status.Worktree,
			HarnessSessionID: *status.HarnessSessionID,
		}
		if status.InstanceID != nil {
			cfg.InstanceID = *status.InstanceID
		}
		if rmErr := container.RemovePiResumeJSONL(cfg); rmErr != nil {
			proglog.Warnf("[prism] warning: remove pi resume jsonl for %s: %v — continuing cleanup\n", sessionName, rmErr)
		}
	}
	if clearErr := d.ClearHarnessSessionID(sessionName); clearErr != nil {
		proglog.Errorf("[prism] severPiResumeLinkage: clear harness_session_id for %s: %v\n", sessionName, clearErr)
		return clearErr
	}
	return nil
}

// archiveThenSeverPiResume runs the session archive for sessionName and then
// — only when the archive step ACTUALLY PRESERVED THE TRANSCRIPT — severs
// the pi resume linkage.
//
// Ordering is load-bearing (issue #2219): runSessionArchive copies the pi
// transcript JSONL (<piSessionsRoot>/<encodePiCWD(worktree)>/*_<id>.jsonl)
// into the archive, and severPiResumeLinkage deletes that same file. Post
// #2210 both resolve the same host-root path in every isolation mode, so
// severing first produced manifest-only archives: lossy cleanup. All four
// cleanup paths (doCleanup, headlessCleanupWithJSON, closeSession,
// headlessCloseSessionWithJSON) route through this helper so the ordering
// cannot silently diverge per-path again.
//
// Gate refinement (issue #2336): the previous gate was "archiveErr == nil",
// which permitted a manifest-only archive (no session.jsonl) to trigger a
// sever that then deleted the transcript from the live pi sessions dir
// without preserving a copy. The new gate is (archiveErr == nil AND the
// adapter reported copied == true). runSessionArchive plumbs the copied
// bool up from the ArchiveAdapter.Archive return value, so both the six
// pre-copy skip paths and the manifest-only in-copier case route through
// the same not-copied guard.
//
// Archive-failure semantic: when runSessionArchive returns an error
// (including archive.ErrAlreadyExists), the sever is skipped ENTIRELY — the
// transcript stays on disk and agent_status.harness_session_id stays
// populated. Leaving both intact keeps cleanup re-runnable: a later attempt
// can still locate and archive the transcript, whereas clearing the DB
// pointer while keeping the file would orphan the transcript (the sever
// scopes its FS removal to the stored id). The trade-off is that a re-spawn
// on the same branch name could resume the defunct pi conversation (#2035)
// until a cleanup succeeds — recoverable by the operator, unlike a deleted
// transcript.
//
// Transcript-missing semantic: when runSessionArchive returns (false, nil)
// — archive attempted but no transcript was actually copied — the sever is
// skipped too, with an explicit "skipped: transcript missing" outcome on
// the JSON envelope. Same reasoning: the DB pointer stays populated so a
// re-run can archive-then-sever, and the pi sessions dir is untouched.
//
// Returns the archive error (nil on success or when there was nothing to
// archive — runSessionArchive treats an empty instanceID and other
// missing-precondition cases as skips) and the sever outcome in the
// cleanupResult convention: true when the sever completed, or a string
// describing why it did not (sever error, or skipped due to archive
// failure, or skipped due to transcript-missing). Headless callers assign
// the outcome to result.HarnessSessionIDCleared; the interactive paths
// discard it and rely on the proglog warnings emitted here and inside
// severPiResumeLinkage.
func archiveThenSeverPiResume(d *db.DB, sessionName, instanceID, isolationMode string) (archiveErr error, severOutcome any) {
	copied, archiveErr := runSessionArchive(d, sessionName, instanceID, isolationMode)
	if archiveErr != nil {
		proglog.Warnf("[prism] warning: pi resume sever skipped for %s — archive failed, leaving transcript in place: %v\n",
			sessionName, archiveErr)
		return archiveErr, fmt.Sprintf("skipped: archive failed: %v", archiveErr)
	}
	if !copied && !severGateForceAlwaysSever {
		proglog.Warnf("[prism] warning: pi resume sever skipped for %s — transcript not copied (archive step preserved nothing), leaving pi state intact\n",
			sessionName)
		return nil, "skipped: transcript missing"
	}
	if severErr := severPiResumeLinkage(d, sessionName); severErr != nil {
		return nil, severErr.Error()
	}
	return nil, true
}

// severGateForceAlwaysSever is a test-only knob used by the revert-and-watch-
// fail tests in cleanup_sever_gate_test.go to prove the copied-gate in
// archiveThenSeverPiResume is not a no-op. Production code never mutates it.
// The variable is package-scoped rather than a `testing.T` argument because
// the gate is checked inside archiveThenSeverPiResume, which sits several
// call frames deep and is not reachable from tests without either a knob or
// substantial refactoring. See issue #2336 test discipline for the pattern.
var severGateForceAlwaysSever bool

// instanceIDFromStatus returns the instance_id from the agent_status row for
// sessionName, or an empty string when the row is missing, instance_id is
// NULL, or on any DB error. Used by cleanup paths to record the incarnation ID
// in the sessions table before SetEnded clears lifecycle data.
func instanceIDFromStatus(d *db.DB, sessionName string) string {
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil || status.InstanceID == nil {
		return ""
	}
	return *status.InstanceID
}

// isolationModeFromDB queries the already-open database d and returns the
// isolation mode for sessionName by reading Status.IsolationMode directly.
// Returns "" when the row is missing or on any error.
func isolationModeFromDB(d *db.DB, sessionName string) string {
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil {
		return ""
	}
	return status.IsolationMode
}

// stopAndRemoveChildContainers stops and removes podman containers for all
// review-agent child sessions of the given parent.
//
// Child sessions are enumerated via the DB's GroupMembersForParent (preferred)
// with a name-prefix fallback for pre-migration rows where group_id is not set.
// For each child the container is stopped (5-second grace) and force-removed.
//
// This function is intentionally non-fatal: a failure to stop or remove a
// single child container is logged as a warning and does not prevent cleanup of
// the remaining children. Container teardown runs here — before the caller
// removes the child's DB row — so that a failed stop still has a DB record to
// retry against.
//
// Parents with no review-agent children are handled gracefully: the function
// returns immediately without issuing any podman commands.
func stopAndRemoveChildContainers(d *db.DB, parentSession string) {
	prefix := parentSession + "~review-"

	// Collect child session names. Prefer DB-backed group membership, fall
	// back to name-prefix scan for pre-migration rows.
	var childNames []string
	if members, err := d.GroupMembersForParent(parentSession); err == nil && len(members) > 0 {
		for _, m := range members {
			childNames = append(childNames, m.SessionName)
		}
	} else {
		if err != nil {
			proglog.Warnf("[prism] warning: stopAndRemoveChildContainers: DB group lookup for %q: %v — using name-prefix fallback\n", parentSession, err)
		}
		// Fallback: scan all rows with the review prefix.
		rows, scanErr := d.AllStatusesWithPrefix(prefix)
		if scanErr != nil {
			proglog.Warnf("[prism] warning: stopAndRemoveChildContainers: AllStatusesWithPrefix for %q: %v — skipping child container teardown\n", parentSession, scanErr)
			return
		}
		for _, row := range rows {
			childNames = append(childNames, row.SessionName)
		}
	}

	// No children — fast path (most non-reviewed sessions).
	if len(childNames) == 0 {
		return
	}

	for _, childSession := range childNames {
		name := container.NameForSession(childSession)

		// Resolve the child session's persisted isolation mode. Dispatch
		// via the registry handles all modes uniformly. Fall back to bwrap
		// when the DB row is missing or the column is empty.
		isoMode := config.IsolationBwrap
		if d != nil {
			if modeStr := isolationModeFromDB(d, childSession); modeStr != "" {
				isoMode = config.IsolationMode(modeStr)
			}
		}

		iso, isoErr := container.For(isoMode, container.ConstructorOpts{Name: name})
		if isoErr != nil {
			// Unknown mode — fall back to bwrap (best-effort temp-file cleanup).
			if iso, isoErr = container.For(config.IsolationBwrap, container.ConstructorOpts{Name: name}); isoErr != nil {
				proglog.Warnf("[prism] warning: stopAndRemoveChildContainers: unknown mode %q for %q: %v\n", isoMode, childSession, isoErr)
				continue
			}
		}

		// 30-second budget collapses the previous 15s stop + 15s rm split.
		// The podman isolator internally issues stop (10s grace) followed
		// by rm --force.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		iso.EnsureRemoved(ctx, nil)
		cancel()
	}
}

// removeContainerIfExists stops and removes any podman container for the given
// prism session, and cleans up the associated temp files.
// It is idempotent: all steps are safe to call when the container does not exist.
// Called after KillSidecar to handle the case where the sidecar is already dead.
// For host-mode (non-container) sessions, the podman calls return "no such
// container" which is silently ignored.
//
// Post A1.L1 (issue #1140): the per-mode stop/rm + temp-file cleanup logic
// moved into the registered Isolator's EnsureRemoved method. This function
// resolves the session's persisted isolation mode (with podman as the
// fallback for pre-migration rows) and dispatches via
// registry.For(mode).EnsureRemoved. Both contexts (20s for stop, 15s for rm)
// are kept by collapsing into a single 35s budget consumed by the isolator.
func removeContainerIfExists(sessionName string) {
	name := container.NameForSession(sessionName)

	// Resolve the session's persisted isolation mode. When the DB row is
	// missing or has an empty isolation_mode, fall back to bwrap for
	// best-effort temp-file cleanup.
	isoMode := config.IsolationBwrap
	if d, dbErr := openDB(); dbErr == nil {
		modeStr := isolationModeFromDB(d, sessionName)
		d.Close()
		if modeStr != "" {
			isoMode = config.IsolationMode(modeStr)
		}
	}

	// Per-mode dispatch: bwrap and sandbox-exec do temp-file cleanup only;
	// host is a no-op.
	iso, isoErr := container.For(isoMode, container.ConstructorOpts{Name: name})
	if isoErr != nil {
		// Unknown mode — fall back to bwrap (best-effort temp-file cleanup).
		if iso, isoErr = container.For(config.IsolationBwrap, container.ConstructorOpts{Name: name}); isoErr != nil {
			proglog.Errorf("[prism] removeContainerIfExists: unknown isolation mode %q for %q: %v\n", isoMode, sessionName, isoErr)
			return
		}
	}

	// Single 35s budget for the per-mode EnsureRemoved (was: 20s stop + 15s
	// rm in two independent contexts). The isolator's EnsureRemoved owns the
	// internal split; for podman it issues the same stop+rm sequence with
	// the same 10s grace.
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	iso.EnsureRemoved(ctx, nil)

	// Clean up the host-API Unix socket, the agent-run log, and their
	// per-session directory. The sidecar's own shutdown path would normally
	// remove the socket, but cleanup runs after KillSidecar so we cannot rely
	// on that — remove them directly.
	// The per-session directory (run/<session>/) was introduced by security fix
	// #960 to isolate sockets; removing it here prevents accumulated empty dirs.
	if sockPath, err := prismSession.SidecarHostAPIPath(sessionName); err == nil {
		_ = os.Remove(sockPath)
		// Also remove the agent-run log from the same per-session directory so
		// the directory can be cleaned up without leaving orphaned files.
		if agentRunLogPath, arErr := prismSession.AgentRunLogPath(sessionName); arErr == nil {
			_ = os.Remove(agentRunLogPath)
		}
		_ = os.Remove(filepath.Dir(sockPath)) // remove now-empty per-session dir
	}
}

// isCoordinatorFromDB checks whether session is a coordinator using the
// DB-backed root_agent_name read. When the DB is unavailable, the row is
// missing, or root_agent_name is NULL (pre-migration), falls back to the
// branch-name heuristic (isDefaultBranch).
//
// This function is extracted from cleanupCmd.RunE to allow unit testing of
// the DB-backed coordinator detection independently of the full cleanup flow.
// isSafeToRemoveWorktree returns true when worktreePath is safe to remove
// during headless cleanup. It returns false (with a caller-logged warning) in
// two cases:
//
//  1. The path matches the default branch worktree (e.g. <bareRoot>/main).
//     Removing the default branch worktree would destroy the coordinator's
//     working directory and break all subsequent git/gh commands.
//
//  2. The path matches the worktree of any currently active session in the DB
//     other than the session being cleaned up. This guards against investigator
//     or other child sessions that inherit the parent's worktree path and would
//     otherwise clobber a live session.
//
// Both checks normalise paths with filepath.Clean before comparing.
// When bareRoot is empty the path-level checks are skipped (caller already
// handles the worktreePath=="" fast-path).
func isSafeToRemoveWorktree(session, worktreePath, bareRoot string) bool {
	if worktreePath == "" {
		return false
	}
	cleanPath := filepath.Clean(worktreePath)

	// Guard 1: never remove the default branch worktree.
	if bareRoot != "" {
		defaultBr := git.DefaultBranchFromBareRoot(bareRoot)
		if defaultBr != "" {
			defaultWorktreePath := filepath.Clean(filepath.Join(bareRoot, defaultBr))
			if cleanPath == defaultWorktreePath {
				return false
			}
		}
	}

	// Guard 2: never remove the worktree of any active session other than the
	// session being cleaned up. The session's own DB row is still active at
	// this point (SetEnded is called later), so we must exclude it from the
	// comparison to avoid blocking legitimate cleanup of dedicated worktrees.
	//
	// Descendant sessions — review and investigator sub-sessions named
	// "<session>~<anything>" (e.g. "<session>~review-1-review-code",
	// "<session>~investigate-<slug>") — inherit the parent's worktree path by
	// design and are torn down by the same cleanup invocation. They must not
	// block worktree removal of their parent.
	if d, err := openDB(); err == nil {
		defer d.Close()
		if statuses, err := d.AllActiveStatus(); err == nil {
			for _, st := range statuses {
				if st.SessionName == session {
					continue
				}
				// Skip descendant sessions (review / investigator sub-sessions)
				// that reuse the parent's worktree path.
				if strings.HasPrefix(st.SessionName, session+"~") {
					continue
				}
				if st.Worktree != "" && filepath.Clean(st.Worktree) == cleanPath {
					return false
				}
			}
		}
	}

	return true
}

func isCoordinatorFromDB(session string, isDefaultBranch bool) bool {
	if d, dbErr := openDB(); dbErr == nil {
		rootName, rowExists, rootErr := d.RootAgentName(session)
		d.Close()
		if rootErr == nil && rowExists && rootName != "" {
			isCoord := rootName == "coordinator"
			if isCoord != isDefaultBranch {
				fmt.Fprintf(os.Stderr, "[debug] cleanup: isCoordinator(%q): DB says %v (root_agent_name=%q), branch heuristic says %v\n",
					session, isCoord, rootName, isDefaultBranch)
			}
			return isCoord
		}
		// Pre-migration fallback: use branch-name heuristic.
		if rootErr != nil {
			proglog.Warnf("[prism] warning: cleanup: DB error reading root_agent_name for %q: %v — using branch heuristic\n", session, rootErr)
		} else if rowExists {
			// Row exists but root_agent_name is NULL — pre-migration.
			fmt.Fprintf(os.Stderr, "[deprecation] cleanup: root_agent_name NULL for %q — using branch heuristic\n", session)
		}
		// rowExists=false: no row yet — use heuristic silently.
		return isDefaultBranch
	} else {
		proglog.Warnf("[prism] warning: cleanup: could not open DB for %q: %v — using branch heuristic\n", session, dbErr)
		return isDefaultBranch
	}
}
