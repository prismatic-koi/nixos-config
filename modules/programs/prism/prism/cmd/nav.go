package cmd

// prism nav — directional session switching for the agent window.
//
// `prism nav up|down|left|right` is intended to be wired to root-level
// C-h/C-j/C-k/C-l in tmux.nix via an if-shell guard that fires only on the
// `agent` window. The bindings on `edit` and `term` windows pass the literal
// keystroke through.
//
// Behaviour summary:
//   - up/down walks every real switchable session in dashboard order
//     (top-level rows and their depth-1 children), excluding review-group
//     virtual rows, depth-2 review-agent children, sessions in terminal
//     states (finished/deleted/interrupted), and sessions without a live
//     tmux session. The cycle wraps at both ends.
//   - left/right walks the review cycle for the current session:
//       [parent, review-goal, review-code, review-security, review-qa, review-context]
//     when the current session is either a depth-2 review agent (the cycle is
//     anchored on that agent's parent and round) or the parent of an active
//     review round (the cycle is anchored on the lowest-numbered active round).
//     Outside both contexts, left/right are silent no-ops.
//   - Invocation outside a tmux client ($TMUX unset) returns a non-zero exit
//     with a clear error.
//   - Unknown direction arguments return a non-zero exit with a clear error.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/nav"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var navCmd = &cobra.Command{
	Use:   "nav <up|down|left|right>",
	Short: "Switch tmux client to an adjacent prism session",
	Args:  cobra.ExactArgs(1),
	RunE:  runNav,
}

func init() {
	rootCmd.AddCommand(navCmd)
}

func runNav(_ *cobra.Command, args []string) error {
	dir, err := nav.ParseDirection(args[0])
	if err != nil {
		return err
	}

	// Require a tmux client. We treat $TMUX as the marker — `prism nav` is
	// only ever invoked from a tmux key binding, and outside that context
	// there is nothing to switch.
	if os.Getenv("TMUX") == "" {
		return fmt.Errorf("prism nav: not running inside a tmux client ($TMUX is not set)")
	}

	current, err := tmux.CurrentSession()
	if err != nil {
		return fmt.Errorf("prism nav: failed to query current tmux session: %w", err)
	}

	sessions, err := fetchNavSessions()
	if err != nil {
		return fmt.Errorf("prism nav: %w", err)
	}

	target, ok := resolveTarget(current, dir, sessions)
	if !ok {
		// Silent no-op: per spec, exit 0 without switching and without
		// writing to stderr.
		return nil
	}

	// Use the explicit -c <client> form whenever CurrentClient returns a name.
	// This matches the pattern used by cmd/switch.go, cmd/switch_project.go,
	// and cmd/cleanup.go and ensures that `prism nav` switches the client
	// whose pane invoked the binding rather than the server-global
	// "most-recently-active" client (which differs after a fresh spawn).
	client, _ := tmux.CurrentClient()
	if client != "" {
		if err := tmux.SwitchClient(client, target); err != nil {
			return fmt.Errorf("prism nav: switch-client (client=%q, target=%q) failed: %w", client, target, err)
		}
		return nil
	}
	if _, err := tmux.SwitchClientCurrent(target); err != nil {
		return fmt.Errorf("prism nav: switch-client to %q failed: %w", target, err)
	}
	return nil
}

// resolveTarget computes the target session name for the requested direction,
// or ("", false) when the navigation is a no-op.
func resolveTarget(current string, dir nav.Direction, sessions []dashboard.AgentSession) (string, bool) {
	live := tmux.HasSession
	switch dir {
	case nav.DirUp, nav.DirDown:
		targets := nav.VerticalTargets(sessions, live)
		return nav.ResolveVertical(current, dir, targets)
	case nav.DirLeft, nav.DirRight:
		cycle, ok := nav.ResolveReviewContext(current, sessions, live)
		if !ok {
			return "", false
		}
		return nav.ResolveLateral(current, dir, cycle, live)
	default:
		return "", false
	}
}

// fetchNavSessions returns the slice of dashboard sessions sorted in
// SortDisplayed order. Internal/meta sessions (scratchpad, prism-dashboard)
// are filtered out so they never appear in the nav spine.
func fetchNavSessions() ([]dashboard.AgentSession, error) {
	d, err := openNavDB()
	if err != nil {
		return nil, err
	}
	defer d.Close()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return nil, fmt.Errorf("agent_status query: %w", err)
	}

	groupParents, _ := d.AllGroupParents()

	// Build AgentSession values without the tmux client-count fetch — nav
	// does not need attachment counts and avoiding the extra `list-clients`
	// subprocess keeps the binding cheap.
	out := make([]dashboard.AgentSession, 0, len(statuses))
	for _, s := range statuses {
		if session.IsMetaSession(s.SessionName) {
			continue
		}
		out = append(out, dashboard.StatusToAgentSession(s, nil, groupParents))
	}
	dashboard.SortDisplayed(out)
	return out, nil
}

// openNavDB opens prism.db at the standard path. Factored out as a var so
// tests in the same package can replace it (not currently used; the pure
// resolver is exercised under internal/nav/).
var openNavDB = func() (*db.DB, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = home + "/.local/state"
	}
	return db.Open(stateHome + "/prism/prism.db")
}
