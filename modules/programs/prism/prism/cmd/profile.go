package cmd

// prism profile — manage the runtime active profile (#1207).
//
// Subcommands:
//
//   prism profile use <name>   Set the active profile via the runtime state
//                              file at $XDG_STATE_HOME/prism/active-profile.
//                              New sessions spawned afterwards (without an
//                              explicit --profile flag) pick up the change
//                              automatically. Live sessions are NOT touched
//                              — that is P3.LIVE / P3.CLI, filed separately.
//
//   prism profile list         Print every profile defined in profiles.json,
//                              one per line, with the active profile marked
//                              by a leading "*".
//
//   prism profile show [name]  Print the per-role slot table for the named
//                              profile, defaulting to the active profile.
//                              Output is plain TSV-friendly text: one row per
//                              role, columns role | provider | model |
//                              thinking. The intent is human-readable
//                              diagnosis, not machine consumption — there
//                              is no JSON output flag in this issue.
//
// Resolution order for "the active profile" (mirrors spawn.go):
//   1. Runtime state file at $XDG_STATE_HOME/prism/active-profile
//   2. pf.Default from profiles.json
// `prism profile use` does NOT take an isolation/scope flag — that lands in
// P3.CLI alongside live-session manipulation.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage the active model profile (runtime state)",
	Long: `Manage the active model profile.

The active profile determines which model identifiers are rendered into
opencode.json for newly spawned sessions. It is resolved in this order:

  1. The --profile flag passed to "prism spawn" (highest)
  2. The runtime state file at $XDG_STATE_HOME/prism/active-profile
  3. The nix-configured default in profiles.json (lowest)

"prism profile use <name>" writes the runtime state file. It does not touch
live sessions — that is the future-spawn default, full stop.`,
}

var profileUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active profile for future spawns",
	Args:  cobra.ExactArgs(1),
	RunE:  runProfileUse,
}

var profileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all profiles defined in profiles.json",
	Args:  cobra.NoArgs,
	RunE:  runProfileList,
}

var profileShowCmd = &cobra.Command{
	Use:   "show [name]",
	Short: "Show the slot table for a profile (default: active)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProfileShow,
}

func init() {
	profileCmd.AddCommand(profileUseCmd)
	profileCmd.AddCommand(profileListCmd)
	profileCmd.AddCommand(profileShowCmd)
	rootCmd.AddCommand(profileCmd)
}

func runProfileUse(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	name := args[0]

	pf, err := config.LoadProfiles()
	if err != nil {
		return err
	}
	if err := config.SetActiveProfile(pf, name); err != nil {
		return err
	}
	path, _ := config.ActiveProfilePath()
	fmt.Fprintf(cmd.OutOrStdout(), "active profile set to %q (state file: %s)\n", name, path)
	return nil
}

func runProfileList(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	pf, err := config.LoadProfiles()
	if err != nil {
		return err
	}

	// Determine the effective active profile name for marking. We do not
	// pass through the --profile flag here (this is "prism profile list",
	// not "prism spawn") so flagValue is intentionally empty.
	active, _, resolveErr := config.ResolveActiveProfile(pf, "")
	if resolveErr != nil {
		// A corrupt state file should not block listing — surface as a
		// warning to stderr and keep going so the user can see what's
		// available and run `prism profile use` to recover.
		fmt.Fprintf(os.Stderr, "warning: %v\n", resolveErr)
	}

	w := cmd.OutOrStdout()
	for _, name := range config.AvailableProfileNames(pf) {
		marker := "  "
		if name == active {
			marker = "* "
		}
		fmt.Fprintf(w, "%s%s\n", marker, name)
	}
	return nil
}

func runProfileShow(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	pf, err := config.LoadProfiles()
	if err != nil {
		return err
	}

	var name string
	if len(args) == 1 {
		name = args[0]
	} else {
		resolved, _, resolveErr := config.ResolveActiveProfile(pf, "")
		if resolveErr != nil {
			return resolveErr
		}
		if resolved == "" {
			return fmt.Errorf("no active profile resolved (no state file and profiles.json has no default) — run `prism profile use <name>` or pass a profile name")
		}
		name = resolved
	}

	entry, ok := pf.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q — available: %s",
			name, strings.Join(config.AvailableProfileNames(pf), ", "))
	}

	// Sort roles for stable output.
	roles := make([]string, 0, len(entry))
	for role := range entry {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	fmt.Fprintf(cmd.OutOrStdout(), "profile: %s\n", name)
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tPROVIDER\tMODEL\tTHINKING")
	for _, role := range roles {
		slot := entry[role]
		thinking := slot.Thinking
		if thinking == "" {
			thinking = "-"
		}
		provider := slot.Provider
		if provider == "" {
			provider = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", role, provider, slot.Model, thinking)
	}
	return tw.Flush()
}
