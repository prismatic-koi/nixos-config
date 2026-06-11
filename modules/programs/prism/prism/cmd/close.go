package cmd

// prism close — smart-decide session close based on PR state (issue #2179).
//
// The decision tree:
//
//	1. Force flags win:
//	   --keep-worktree   → soft close (preserve worktree + branch)
//	   --remove-worktree → hard cleanup (remove worktree, force-delete branch)
//	2. Coordinator session (root_agent_name == "coordinator" or @main heuristic)
//	   → soft close
//	3. Non-worktree session (no "@" in name, e.g. "obsidian") → soft close
//	4. Worker worktree session: probe `gh pr list --head <branch>`:
//	   - any PR is OPEN                              → soft close
//	   - all PRs are MERGED/CLOSED                   → hard cleanup
//	   - no PR found                                 → hard cleanup
//	   - probe error / timeout / unauthenticated     → soft close (fail-safe)
//
// The asymmetric-risk argument: a spurious soft close costs one
// `prism close --remove-worktree` later; a spurious hard cleanup destroys
// uncommitted work. Every error path on the gh probe therefore routes to
// soft close.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ghProbeTimeout caps the `gh pr list` probe so a hung GitHub API never
// wedges the tmux popup (issue #2179, AC: performance ≤ 5s). On timeout the
// command fails safe to a soft close.
//
// A var, not a const: tests shrink it so the timeout→fail-safe path is
// covered without a real 5-second wall-clock wait (issue #2217).
var ghProbeTimeout = 5 * time.Second

// prProbe is the indirection used by decideClose to query PR state for a
// branch. Replaced in tests with a fake that returns canned responses.
var prProbe = probePRStateExec

// ghExecOutput is the exec seam used by probePRStateExec to run the gh CLI
// (issue #2217). Production points at ghExecOutputReal, which shells out to
// `gh` on $PATH. Tests replace it with a stub returning canned stdout so the
// probe's argv construction, JSON parsing, and state reduction run
// in-process — no subprocess, no $PATH lookup, and no network. PATH-injected
// fake binaries proved environment-fragile (the probe reached the real gh in
// some worker sandboxes and hit its 5s network timeout), which is why the
// seam sits at the exec boundary rather than on $PATH.
var ghExecOutput = ghExecOutputReal

// ghExecOutputReal runs `gh` with the given argv in workdir (empty = caller
// cwd), returning its stdout. The context bounds the subprocess lifetime:
// exec.CommandContext kills it when the deadline fires.
func ghExecOutputReal(ctx context.Context, workdir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	return cmd.Output()
}

// closeCmd is the public `prism close` cobra command.
var closeCmd = &cobra.Command{
	Use:   "close",
	Short: "Smart-decide session close: soft-close on an open PR; hard-cleanup otherwise",
	Long: `Close a prism session, deciding between soft close (preserve the worktree
and branch) and hard cleanup (remove worktree, force-delete branch) based on
whether an open pull request exists for the branch.

Decision tree:

  - Coordinator session or non-worktree session → soft close
  - Worker session:
      * branch has an OPEN pull request          → soft close
      * branch has a MERGED or CLOSED PR         → hard cleanup
      * branch has no pull request               → hard cleanup
      * gh probe errors out / times out          → soft close (fail-safe)

The fail-safe to soft close is deliberate: a spurious soft close costs one
extra ` + "`prism close --remove-worktree`" + ` later, while a spurious hard cleanup
destroys uncommitted work.

Force flags override the decision:
  --keep-worktree    preserve the worktree regardless of PR state
  --remove-worktree  remove the worktree regardless of PR state`,
	RunE: runCloseCmd,
}

func init() {
	closeCmd.Flags().Bool("yes", false, "Non-interactive: skip all prompts and close immediately")
	closeCmd.Flags().String("session", "", "Target session name (default: current session)")
	closeCmd.Flags().Bool("json", false, "Emit a single JSON object describing per-resource outcomes (requires --yes)")
	closeCmd.Flags().Bool("keep-worktree", false, "Force soft close: preserve the worktree and branch regardless of PR state")
	closeCmd.Flags().Bool("remove-worktree", false, "Force hard cleanup: remove the worktree and branch regardless of PR state")
	rootCmd.AddCommand(closeCmd)
}

func runCloseCmd(cmd *cobra.Command, args []string) error {
	yesFlag, _ := cmd.Flags().GetBool("yes")
	sessionFlag, _ := cmd.Flags().GetString("session")
	jsonFlag, _ := cmd.Flags().GetBool("json")
	keepFlag, _ := cmd.Flags().GetBool("keep-worktree")
	removeFlag, _ := cmd.Flags().GetBool("remove-worktree")

	if keepFlag && removeFlag {
		return fmt.Errorf("--keep-worktree and --remove-worktree are mutually exclusive")
	}
	// --json implies --yes: emitting a single structured object is incompatible
	// with an interactive TUI. Match `prism cleanup`'s wording exactly so
	// scripts that pattern-match on the error see no difference.
	if jsonFlag && !yesFlag {
		return fmt.Errorf("--json requires --yes (interactive close is incompatible with structured output)")
	}

	// Inside a container: proxy to the host sidecar. The host runs the same
	// `prism close` binary so the decision tree is identical there; the
	// container side just forwards stdout/stderr verbatim.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		target := sessionFlag
		if target == "" {
			return fmt.Errorf("--session is required when running inside a container")
		}
		return proxyCloseToHostAPI(apiURL, target, yesFlag, jsonFlag, keepFlag, removeFlag)
	}

	// Only require tmux when we need to auto-detect the current session.
	if sessionFlag == "" && os.Getenv("TMUX") == "" {
		return fmt.Errorf("interactive close requires tmux — use --yes for non-interactive use outside tmux")
	}

	var session string
	if sessionFlag != "" {
		session = sessionFlag
		// Validate the session name early against the DB so a typo produces a
		// helpful enumerated error rather than a later "worktree not found".
		// Match `prism cleanup`'s error shape exactly (AC: edge-case parity).
		if d, dbErr := openDB(); dbErr == nil {
			st, stErr := d.CurrentStatus(session)
			if stErr == nil && st == nil {
				names, _ := activeSessionNamesForError(d, 10)
				d.Close()
				if len(names) == 0 {
					return fmt.Errorf("--session %q not found — no active sessions in DB", session)
				}
				return fmt.Errorf("--session must be one of: %s (got: %q)", strings.Join(names, ", "), session)
			}
			d.Close()
		}
	} else {
		s, err := tmux.CurrentSession()
		if err != nil || s == "" {
			return fmt.Errorf("could not determine current tmux session")
		}
		session = s
	}

	soft, hard := decideClose(session, keepFlag, removeFlag)

	// quiet = silent on stdout (popup-safe). --json keeps emitting the JSON
	// envelope; only the "$yes && !$json" combination is fully silent on
	// success.
	quiet := yesFlag && !jsonFlag
	stdout := io.Writer(os.Stdout)
	if quiet {
		stdout = io.Discard
	}

	switch {
	case soft:
		if yesFlag {
			return headlessCloseSessionWithJSONTo(session, jsonFlag, stdout)
		}
		// Interactive path: prompt the user, then run the same headless flow
		// with os.Stdout so progress is visible.
		if os.Getenv("TMUX") == "" {
			return fmt.Errorf("interactive close requires tmux — use --yes for non-interactive use outside tmux")
		}
		if !confirm(fmt.Sprintf("Close session %s? (worktree will be kept)", session)) {
			return nil
		}
		return closeSession(session)

	case hard:
		// Resolve worktree path & bareRoot for hard cleanup. Mirrors the
		// resolution in cleanupCmd.RunE.
		worktreeName := ""
		if i := strings.Index(session, "@"); i >= 0 {
			worktreeName = session[i+1:]
		}
		worktreePath := worktreePathFromSession(session)
		bareRoot := ""
		if worktreePath != "" {
			bareRoot = git.BareRoot(worktreePath)
		} else {
			probed, probedBareRoot := probeConventionalWorktreePath(session, worktreeName)
			if probed != "" {
				worktreePath = probed
				bareRoot = git.BareRoot(worktreePath)
			} else if probedBareRoot != "" {
				bareRoot = probedBareRoot
			}
		}

		if yesFlag {
			return headlessCleanupWithJSONTo(session, worktreeName, worktreePath, bareRoot, jsonFlag, stdout)
		}
		// Interactive path: drive the same TUI cleanup model as `prism cleanup`.
		// The popup keybind always passes --yes so this path is only reached
		// when a user types `prism close` at a shell prompt without --yes; we
		// keep behaviour parity with `prism cleanup` so the surface is
		// predictable.
		if os.Getenv("TMUX") == "" {
			return fmt.Errorf("interactive close requires tmux — use --yes for non-interactive use outside tmux")
		}
		defaultBr := ""
		if bareRoot != "" {
			defaultBr = git.DefaultBranchFromBareRoot(bareRoot)
		}
		m := newCleanupModel(session, worktreeName, worktreePath, bareRoot, defaultBr)
		prog := tea.NewProgram(cleanupWrapper{m}, tea.WithAltScreen())
		_, runErr := prog.Run()
		return runErr
	}
	return nil
}

// decideClose returns (soft, hard) — exactly one is true. Implements the
// decision tree documented at the top of this file.
//
// keepFlag and removeFlag are caller-provided force-overrides; the function
// rejects mutual specification upstream (in runCloseCmd) so this helper can
// assume at most one is set.
func decideClose(session string, keepFlag, removeFlag bool) (soft, hard bool) {
	// Force flags win.
	switch {
	case keepFlag:
		return true, false
	case removeFlag:
		return false, true
	}

	// Non-worktree session (no "@"): soft close (matches `prism cleanup`).
	if !strings.Contains(session, "@") {
		return true, false
	}

	branch := session[strings.Index(session, "@")+1:]

	// Coordinator detection: resolve default branch via the worktree path,
	// then fall back to the DB-backed coordinator check.
	isDefaultBranch := false
	worktreePath := worktreePathFromSession(session)
	if worktreePath != "" {
		if bareRoot := git.BareRoot(worktreePath); bareRoot != "" {
			if defaultBr := git.DefaultBranchFromBareRoot(bareRoot); defaultBr != "" && branch == defaultBr {
				isDefaultBranch = true
			}
		}
	}
	if isCoordinatorFromDB(session, isDefaultBranch) {
		return true, false
	}

	// Worker session: probe PR state.
	state, err := prProbe(worktreePath, branch)
	if err != nil {
		// Fail-safe: any probe failure routes to soft close so an unreachable
		// gh, missing PATH entry, or auth failure cannot destroy a worktree.
		return true, false
	}
	switch strings.ToUpper(state) {
	case "OPEN":
		return true, false
	case "MERGED", "CLOSED":
		return false, true
	case "":
		// No PR found for this branch → hard cleanup.
		return false, true
	default:
		// Unknown state (e.g. future gh output) → fail-safe to soft close.
		return true, false
	}
}

// prProbeResult is the gh JSON shape we care about.
type prProbeResult struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// probePRStateExec is the production gh probe. It runs
// `gh pr list --head <branch> --state all --json state,number --limit 10`
// in the supplied working directory with a bounded context timeout.
//
// Why `gh pr list` and not `gh pr view`: the AC requires that when multiple
// PRs exist for the same head branch and any of them is OPEN, the probe
// returns OPEN. `gh pr view` returns a single PR; `gh pr list --head` returns
// the full set, which is necessary to honour the multi-PR AC.
//
// Returns:
//   - ("OPEN", nil) when any PR for the branch is OPEN.
//   - ("MERGED"|"CLOSED", nil) when no PR is OPEN; the state of the first
//     non-open PR is returned (caller treats both identically: hard cleanup).
//   - ("", nil) when no PR exists for the branch.
//   - ("", err) on any failure — caller fails safe to soft close.
func probePRStateExec(workdir, branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("probePRState: empty branch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghProbeTimeout)
	defer cancel()
	out, err := ghExecOutput(ctx, workdir, "pr", "list",
		"--head", branch,
		"--state", "all",
		"--json", "state,number",
		"--limit", "10")
	if err != nil {
		return "", err
	}
	var prs []prProbeResult
	if uerr := json.Unmarshal(out, &prs); uerr != nil {
		return "", uerr
	}
	if len(prs) == 0 {
		return "", nil
	}
	// Multi-PR case: if any PR is OPEN, treat as OPEN (AC: edge-case).
	for _, pr := range prs {
		if strings.EqualFold(pr.State, "OPEN") {
			return "OPEN", nil
		}
	}
	// No open PR — return the state of the first non-open PR; the caller
	// treats MERGED and CLOSED identically (both → hard cleanup).
	return strings.ToUpper(prs[0].State), nil
}
