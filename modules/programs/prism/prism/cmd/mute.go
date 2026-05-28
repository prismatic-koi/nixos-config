package cmd

// prism mute — toggle or set the muted flag on a session.
//
// This command is intentionally hidden from --help, cobra completion, and any
// agent-facing documentation. It is an operator escape hatch for the human at
// the keyboard, not a tool for AI coordinators to discover. See issue #2013
// for the design rationale.
//
// Surface:
//
//	prism mute             # toggle current pane's session (reads PRISM_SESSION_NAME)
//	prism mute <session>   # toggle the named session
//	prism mute --on <s>    # idempotently set <s> muted=true
//	prism mute --off <s>   # idempotently set <s> muted=false
//
// Semantics:
//
//   - The muted flag is an orthogonal boolean on agent_status; agent lifecycle
//     (active / waiting / idle / escalated / etc.) keeps moving normally.
//   - Persistence survives sidecar restart.
//   - No auto-unmute. A muted session that hits finished stays muted; the
//     terminal `has finished` notification is suppressed too. Missed
//     notifications are dropped, not queued for replay on unmute.
//   - Suppression scope is outbound coordinator notifications only
//     (session.finished AND session.escalated). DB writes are unaffected;
//     inbound delivery to the muted session is unaffected.
//   - Muting a coordinator is permitted and persists, but has no observable
//     effect on notification flow (coordinators do not notify themselves).

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// muteCmd is the (hidden) `prism mute` command. Hidden=true keeps it out of
// `--help` and cobra's completion output; the help text below is still
// reachable via `prism help mute` for the operator who knows the command
// exists.
var muteCmd = &cobra.Command{
	Use:    "mute [session]",
	Short:  "Toggle or set the muted flag on a session (operator escape hatch)",
	Hidden: true,
	Long: `Toggle or set the muted flag on a session.

With no positional argument, toggles the session named in PRISM_SESSION_NAME
(set automatically inside a prism pane). With a positional argument, toggles
the named session regardless of the calling pane's PRISM_SESSION_NAME.

While muted, the session's outbound coordinator notifications are suppressed
(both 'has finished' and the escalation bus notification). DB writes,
lifecycle state transitions, and inbound delivery to the session are all
unaffected. The flag is persistent and survives sidecar restart; it is never
auto-cleared, including on session termination.

--on and --off set the flag idempotently. They are mutually exclusive.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMute,
}

func init() {
	muteCmd.Flags().Bool("on", false, "Idempotently set muted=true (cannot combine with --off)")
	muteCmd.Flags().Bool("off", false, "Idempotently set muted=false (cannot combine with --on)")
	muteCmd.MarkFlagsMutuallyExclusive("on", "off")
	rootCmd.AddCommand(muteCmd)
}

// runMute resolves the target session and applies the requested mute state.
//
// Target resolution:
//   - One positional argument: use it verbatim.
//   - No positional argument: read PRISM_SESSION_NAME; error if unset.
//
// Mode resolution:
//   - --on: set muted=true; no stdout output if already muted.
//   - --off: set muted=false; no stdout output if already unmuted.
//   - Neither: toggle; print "muted: <name>" or "unmuted: <name>".
//
// A non-existent target session is a hard error (exit non-zero) and never
// inserts a row \u2014 the muted flag belongs to a real lifecycle row, not a
// phantom one created on first reference.
func runMute(cmd *cobra.Command, args []string) error {
	on, _ := cmd.Flags().GetBool("on")
	off, _ := cmd.Flags().GetBool("off")

	var target string
	switch len(args) {
	case 0:
		target = os.Getenv("PRISM_SESSION_NAME")
		if target == "" {
			return errors.New(
				"prism mute: no current session — pass a session name or run from a prism pane",
			)
		}
	case 1:
		target = args[0]
	}

	database, err := openDB()
	if err != nil {
		return fmt.Errorf("prism mute: open db: %w", err)
	}
	defer database.Close()

	current, ok, err := database.IsMuted(target)
	if err != nil {
		return fmt.Errorf("prism mute: read mute state for %q: %w", target, err)
	}
	if !ok {
		return fmt.Errorf("prism mute: session %q not found", target)
	}

	switch {
	case on:
		if current {
			// Already in desired state — idempotent no-op, no output.
			return nil
		}
		if _, err := database.SetMuted(target, true); err != nil {
			return fmt.Errorf("prism mute: set muted: %w", err)
		}
		fmt.Printf("muted: %s\n", target)
		return nil
	case off:
		if !current {
			return nil
		}
		if _, err := database.SetMuted(target, false); err != nil {
			return fmt.Errorf("prism mute: clear muted: %w", err)
		}
		fmt.Printf("unmuted: %s\n", target)
		return nil
	default:
		// Toggle.
		next := !current
		if _, err := database.SetMuted(target, next); err != nil {
			return fmt.Errorf("prism mute: toggle muted: %w", err)
		}
		if next {
			fmt.Printf("muted: %s\n", target)
		} else {
			fmt.Printf("unmuted: %s\n", target)
		}
		return nil
	}
}
