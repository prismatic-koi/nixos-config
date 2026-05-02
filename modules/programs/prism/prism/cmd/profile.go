package cmd

// prism profile — manage the runtime active profile (#1207, #1215).
//
// Subcommands:
//
//   prism profile use <name>   Set the active profile via the runtime state
//                              file at $XDG_STATE_HOME/prism/active-profile.
//                              New sessions spawned afterwards (without an
//                              explicit --profile flag) pick up the change
//                              automatically.
//
//                              With --scope the command also (or exclusively)
//                              swaps live sessions via POST /apply-profile
//                              on the host-API sidecar:
//
//                                --scope session=<name>  target one session
//                                --scope coordinator     all PI sessions in
//                                                        this coordinator's
//                                                        repo
//                                --scope global          every live PI session
//                                                        (coordinator-only)
//                                --scope all             live swap (coordinator
//                                                        scope) + future-spawn
//                                                        default update
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

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/session"
)

// profileUseFlags holds the flags for `prism profile use`.
var profileUseFlags struct {
	scope   string
	yes     bool
	verbose bool
}

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage the active model profile (runtime state)",
	Long: `Manage the active model profile.

The active profile determines which model identifiers are rendered into
opencode.json for newly spawned sessions. It is resolved in this order:

  1. The --profile flag passed to "prism spawn" (highest)
  2. The runtime state file at $XDG_STATE_HOME/prism/active-profile
  3. The nix-configured default in profiles.json (lowest)

"prism profile use <name>" writes the runtime state file (future-spawn
default). Add --scope to also swap live sessions via the host-API.`,
}

var profileUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active profile for future spawns (and optionally live sessions)",
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

	profileUseCmd.Flags().StringVar(&profileUseFlags.scope, "scope", "",
		`Live-session swap scope: session=<name>, coordinator, global, or all.
Without --scope only the future-spawn default is updated (P1.RUNTIME behaviour).`)
	profileUseCmd.Flags().BoolVar(&profileUseFlags.yes, "yes", false,
		"Skip the confirmation prompt for --scope global.")
	profileUseCmd.Flags().BoolVarP(&profileUseFlags.verbose, "verbose", "v", false,
		"List each session's individual outcome when --scope is used.")
}

func runProfileUse(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	name := args[0]

	// Validate the profile name exists regardless of scope — we do not want to
	// write the state file or make API calls for an unknown profile name.
	pf, err := config.LoadProfiles()
	if err != nil {
		return err
	}

	// Determine scope from flags.
	scope := profileUseFlags.scope
	verbose := profileUseFlags.verbose

	// Validate scope value if provided.
	var apiScope string      // scope value to send to /apply-profile
	var sessionTarget string // set when scope=session=<name>
	if scope != "" {
		switch {
		case scope == "coordinator", scope == "global", scope == "all":
			if scope == "all" {
				apiScope = "coordinator"
			} else {
				apiScope = scope
			}
		case strings.HasPrefix(scope, "session="):
			sessionTarget = strings.TrimPrefix(scope, "session=")
			if sessionTarget == "" {
				return fmt.Errorf("--scope session= requires a session name (e.g. --scope session=myrepo@main)")
			}
			apiScope = "session"
		default:
			return fmt.Errorf("--scope must be session=<name>, coordinator, global, or all — got %q", scope)
		}
	}

	// When scope is set to a live-swap scope, we need a host-API socket.
	// Detect whether we are running inside a container or from the host.
	hostAPIURL := sandboxenv.HostAPISocket()

	// For scopes that target live sessions (anything except ""), check
	// coordinator-only enforcement and session existence before the API call.
	if scope != "" {
		// Enforce coordinator-only for coordinator/global/all scopes.
		// When PRISM_HOST_API is set the server enforces this, but we also do
		// a preflight check so the error message is clear and no API call is made.
		if apiScope == "coordinator" || apiScope == "global" {
			callerSession := review.LookupParentSession()
			if callerSession != "" {
				d, dbErr := openDB()
				if dbErr == nil {
					defer d.Close()
					if !session.IsCoordinatorSession(callerSession, d) {
						return fmt.Errorf("prism profile use --scope %s: this scope is for coordinator sessions only.\n\nWorkers must not perform %s-scoped profile swaps directly.", scope, scope)
					}
				}
				// If DB open failed we let the server-side enforcement catch it.
			}
		}

		// For scope=session=<name>, verify the session exists and is active
		// before sending any API call.
		if apiScope == "session" && hostAPIURL == "" {
			// On the host we can check the DB directly.
			d, dbErr := openDB()
			if dbErr != nil {
				return fmt.Errorf("prism profile use: open db: %w", dbErr)
			}
			defer d.Close()
			st, stErr := d.CurrentStatus(sessionTarget)
			if stErr != nil {
				return fmt.Errorf("prism profile use: look up session %q: %w", sessionTarget, stErr)
			}
			if st == nil {
				return fmt.Errorf("prism profile use: session %q not found — run `prism list-sessions` to see active sessions", sessionTarget)
			}
			if st.EndedAt != nil {
				return fmt.Errorf("prism profile use: session %q is no longer active (ended)", sessionTarget)
			}
		}

		// Confirmation prompt for --scope global.
		if apiScope == "global" && !profileUseFlags.yes {
			fmt.Fprintf(cmd.OutOrStdout(), "This will swap the model profile on EVERY live PI session across all repos.\nType \"yes\" to continue: ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			if strings.TrimSpace(scanner.Text()) != "yes" {
				return fmt.Errorf("aborted")
			}
		}
	}

	// For scopes that include the future-spawn default (no scope or "all"),
	// update the state file.
	if scope == "" || scope == "all" {
		if err := config.SetActiveProfile(pf, name); err != nil {
			return err
		}
		path, _ := config.ActiveProfilePath()
		fmt.Fprintf(cmd.OutOrStdout(), "active profile set to %q (state file: %s)\n", name, path)
	}

	// No live-swap needed when scope is empty.
	if scope == "" {
		return nil
	}

	// Call /apply-profile on the host-API sidecar.
	reqBody := map[string]any{
		"profile": name,
		"scope":   apiScope,
	}
	if apiScope == "session" {
		reqBody["session"] = sessionTarget
	}

	type sessionResult struct {
		Session string `json:"session"`
		Status  string `json:"status"`
	}
	var resp struct {
		Results []sessionResult `json:"results"`
	}

	if hostAPIURL != "" {
		if err := proxyToHostAPI(hostAPIURL, "/apply-profile", reqBody, &resp); err != nil {
			return fmt.Errorf("prism profile use --scope %s: %w", scope, err)
		}
	} else {
		// On the host, dial the coordinator's own sidecar socket.
		callerSession := review.LookupParentSession()
		if callerSession == "" {
			return fmt.Errorf("prism profile use --scope %s: cannot determine calling session — run from inside a prism tmux session or set PRISM_SESSION_NAME", scope)
		}
		sockPath, sockErr := session.SidecarHostAPIPath(callerSession)
		if sockErr != nil {
			return fmt.Errorf("prism profile use --scope %s: host-API socket: %w", scope, sockErr)
		}
		apiURL := "unix://" + sockPath
		if err := proxyToHostAPI(apiURL, "/apply-profile", reqBody, &resp); err != nil {
			return fmt.Errorf("prism profile use --scope %s: %w", scope, err)
		}
	}

	// Render summary.
	var applied, skipped, failed int
	for _, r := range resp.Results {
		switch {
		case r.Status == "applied":
			applied++
		case strings.HasPrefix(r.Status, "skipped"):
			skipped++
		default:
			failed++
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "applied: %d, skipped: %d, failed: %d\n", applied, skipped, failed)

	if verbose {
		for _, r := range resp.Results {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", r.Session, r.Status)
		}
	}

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
