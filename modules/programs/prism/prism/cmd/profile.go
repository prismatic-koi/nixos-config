package cmd

// prism profile — manage the runtime active profile (#1207, #1215, #1591).
//
// Subcommands:
//
//   prism profile use <name>   By default: update the state file at
//                              $XDG_STATE_HOME/prism/active-profile AND
//                              live-swap every PI session in the current
//                              repo by routing through the auto-discovered
//                              coordinator (scope=coordinator).
//
//                              Optional flags:
//
//                                --no-live               Skip the live-swap;
//                                                        update the state file
//                                                        only (old bare behaviour).
//
//                                --coordinator <session> Explicit coordinator
//                                                        session to route through.
//                                                        Default: auto-discover
//                                                        from cwd or active
//                                                        sessions.
//
//                              --scope overrides the default live-swap target:
//
//                                --scope session=<name>  target one session
//                                --scope coordinator     all PI sessions in
//                                                        this coordinator's
//                                                        repo
//                                --scope global          every live PI session
//                                                        (coordinator-only)
//                                --scope all             live swap (coordinator
//                                                        scope) + state-file
//                                                        update (synonym for
//                                                        the new default)
//
//                              --no-live cannot be combined with --scope.
//
//   prism profile list         Print every profile defined in profiles.json,
//                              one per line, with the active profile marked
//                              by a leading "*".
//                              Use --json to emit a JSON array of profile
//                              objects (snake_case keys).
//
//   prism profile show [name]  Print the per-role slot table for the named
//                              profile, defaulting to the active profile.
//                              Output is plain TSV-friendly text: one row per
//                              role, columns role | provider | model |
//                              thinking. Use --json to emit a single JSON
//                              object describing the profile's slot table.
//
// Resolution order for "the active profile" (mirrors spawn.go):
//   1. Runtime state file at $XDG_STATE_HOME/prism/active-profile
//   2. pf.Default from profiles.json

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/session"
)

// profileUseFlags holds the flags for `prism profile use`.
var profileUseFlags struct {
	scope       string
	yes         bool
	verbose     bool
	noLive      bool
	coordinator string
}

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage the active model profile (runtime state)",
	Long: `Manage the active model profile.

The active profile determines which model identifiers are rendered into
harness config for newly spawned sessions. It is resolved in this order:

  1. The --profile flag passed to "prism spawn" (highest)
  2. The runtime state file at $XDG_STATE_HOME/prism/active-profile
  3. The nix-configured default in profiles.json (lowest)

"prism profile use <name>" writes the runtime state file (future-spawn
default). Add --scope to also swap live sessions via the host-API.`,
}

var profileUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active profile (live-swaps running sessions by default)",
	Long: `Set the active model profile.

Updates the state-file default ($XDG_STATE_HOME/prism/active-profile) AND
live-swaps running sessions via the auto-discovered coordinator (scope=coordinator).
Equivalent to the former "prism profile use NAME --scope all".

Use --no-live to update the state file only (no live-swap).
Use --coordinator <session> to bypass auto-discovery.
Use --scope to override the live-swap target (session=<name>, coordinator,
global, or all). --no-live cannot be combined with --scope.`,
	Args: cobra.ExactArgs(1),
	RunE: runProfileUse,
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

	profileListCmd.Flags().Bool("json", false, "Emit a JSON array of profile objects to stdout instead of the human-readable list")
	profileShowCmd.Flags().Bool("json", false, "Emit a JSON object describing the profile's slot table to stdout instead of the human-readable view")

	profileUseCmd.Flags().StringVar(&profileUseFlags.scope, "scope", "",
		`Live-session swap scope: session=<name>, coordinator, global, or all.
Without --scope the command live-swaps coordinator scope AND updates the state file.`)
	profileUseCmd.Flags().BoolVar(&profileUseFlags.yes, "yes", false,
		"Skip the confirmation prompt for --scope global.")
	profileUseCmd.Flags().BoolVarP(&profileUseFlags.verbose, "verbose", "v", false,
		"List each session's individual outcome when a live-swap occurs.")
	profileUseCmd.Flags().BoolVar(&profileUseFlags.noLive, "no-live", false,
		"Skip the live-swap. Updates the state-file default only — running sessions keep their current profile.")
	profileUseCmd.Flags().StringVar(&profileUseFlags.coordinator, "coordinator", "",
		"Explicit coordinator session to route the live-swap through (e.g. nixos-config@main). Default: auto-discover from cwd or active sessions.")
}

func runProfileUse(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate the profile name exists regardless of scope — we do not want to
	// write the state file or make API calls for an unknown profile name.
	pf, err := config.LoadProfiles()
	if err != nil {
		return err
	}

	// Validate the profile name before any side-effects.
	if _, ok := pf.Profiles[name]; !ok {
		availableNames := config.AvailableProfileNames(pf)
		if len(availableNames) == 0 {
			return fmt.Errorf("no profiles are configured — run the system rebuild to generate ~/.config/prism/profiles.json")
		}
		return fmt.Errorf("--profile must be one of: %s (got: %q)",
			strings.Join(availableNames, ", "), name)
	}

	// Determine flags.
	scope := profileUseFlags.scope
	noLive := profileUseFlags.noLive
	verbose := profileUseFlags.verbose

	// Mutually exclusive: --no-live and --scope cannot be combined.
	if noLive && scope != "" {
		return fmt.Errorf("--no-live cannot be combined with --scope")
	}

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

	// When scope is unset and --no-live is not set, we use the new default:
	// live-swap with coordinator scope (same as --scope all), via auto-discovered
	// coordinator. Treat this as an implicit "all" that sets apiScope=coordinator.
	defaultLive := scope == "" && !noLive
	if defaultLive {
		apiScope = "coordinator"
	}

	// Detect whether we are running inside a container or from the host.
	hostAPIURL := sandboxenv.HostAPISocket()

	// When a live-swap is going to happen (any scope or default live), perform
	// preflight checks and coordinator resolution.
	//
	// Live-swap happens when: defaultLive OR scope != "".
	willLive := defaultLive || scope != ""

	// resolvedCoordinator is the coordinator session we'll dial (host path only).
	// We resolve it now for both the preflight guard and the dial step.
	var resolvedCoordinator string

	if willLive && hostAPIURL == "" {
		// Enforce worker guard: if PRISM_SESSION_NAME is set and the caller is
		// NOT a coordinator, reject coordinator/global/all-scoped live swaps.
		// This guard is specifically about agent authorisation (workers must not
		// escalate), not human-shell usability.
		envSession := os.Getenv("PRISM_SESSION_NAME")
		if envSession != "" && (apiScope == "coordinator" || apiScope == "global") {
			d, dbErr := openDB()
			if dbErr == nil {
				defer d.Close()
				if !session.IsCoordinatorSession(envSession, d) {
					displayScope := scope
					if displayScope == "" {
						displayScope = "all"
					}
					return fmt.Errorf("prism profile use --scope %s: this scope is for coordinator sessions only.\n\nWorkers must not perform %s-scoped profile swaps directly.", displayScope, displayScope)
				}
				// Caller is a coordinator — resolve it as our target.
				resolvedCoordinator = envSession
			}
			// If DB open failed we fall through to auto-discovery.
		}

		// For scopes that need a coordinator socket (coordinator/global/default live),
		// resolve the coordinator session if not already resolved.
		if resolvedCoordinator == "" && (apiScope == "coordinator" || apiScope == "global") {
			d, dbErr := openDB()
			if dbErr != nil {
				return fmt.Errorf("prism profile use: open db: %w", dbErr)
			}
			defer d.Close()
			coord, coordErr := resolveCoordinatorSession(d, profileUseFlags.coordinator)
			if coordErr != nil {
				return coordErr
			}
			resolvedCoordinator = coord
		}
	}

	// For scope=session=<name>, verify the session exists and is active
	// before sending any API call.
	if apiScope == "session" && hostAPIURL == "" {
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
			return fmt.Errorf("prism profile use: session %q not found — run `prism sessions list` to see active sessions", sessionTarget)
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

	// Update the state file for the default live case (implicit "all") or
	// when scope=all is explicit, or when --no-live is set (state-file only).
	if defaultLive || scope == "all" || noLive {
		if err := config.SetActiveProfile(pf, name); err != nil {
			return err
		}
		path, _ := config.ActiveProfilePath()
		fmt.Fprintf(cmd.OutOrStdout(), "active profile set to %q (state file: %s)\n", name, path)
	}

	// No live-swap when --no-live is set.
	if noLive {
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
			displayScope := scope
			if displayScope == "" {
				displayScope = "all"
			}
			return fmt.Errorf("prism profile use --scope %s: %w", displayScope, err)
		}
	} else {
		// On the host, dial the coordinator's own sidecar socket.
		// For session= scope, the caller's session can handle it directly;
		// for coordinator/global, we use the resolved coordinator.
		var targetSession string
		if apiScope == "session" {
			// For single-session scope, route through the caller or any active
			// coordinator. Use the parent session if available, else auto-discover.
			callerSession := review.LookupParentSession()
			if callerSession != "" {
				targetSession = callerSession
			} else {
				// Auto-discover for session scope too.
				d, dbErr := openDB()
				if dbErr != nil {
					return fmt.Errorf("prism profile use: open db: %w", dbErr)
				}
				defer d.Close()
				coord, coordErr := resolveCoordinatorSession(d, profileUseFlags.coordinator)
				if coordErr != nil {
					return coordErr
				}
				targetSession = coord
			}
		} else {
			targetSession = resolvedCoordinator
		}
		sockPath, sockErr := session.SidecarHostAPIPath(targetSession)
		if sockErr != nil {
			displayScope := scope
			if displayScope == "" {
				displayScope = "all"
			}
			return fmt.Errorf("prism profile use --scope %s: host-API socket: %w", displayScope, sockErr)
		}
		apiURL := "unix://" + sockPath
		if err := proxyToHostAPI(apiURL, "/apply-profile", reqBody, &resp); err != nil {
			displayScope := scope
			if displayScope == "" {
				displayScope = "all"
			}
			return fmt.Errorf("prism profile use --scope %s: %w", displayScope, err)
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

// resolveCoordinatorSession picks the coordinator session to route the
// /apply-profile request through. Precedence:
//
//  1. --coordinator <name> flag if non-empty.
//  2. review.LookupParentSession() if it returns a coordinator session
//     (validated via session.IsCoordinatorSession against the DB).
//  3. deriveSessionNameFromCWD(cwd) → if it resolves to a "<repo>@main"
//     session that is active in the DB, return it.
//  4. d.AllActiveStatus() filtered to names ending in "@main", filtered
//     to rows with EndedAt == nil. If exactly one, return it. If zero,
//     return an error suggesting `prism switch`. If multiple, return an
//     error listing them and pointing at --coordinator.
//
// Returns the resolved session name or an actionable error.
func resolveCoordinatorSession(d *db.DB, coordinatorFlag string) (string, error) {
	// 1. Explicit --coordinator flag takes highest precedence.
	if coordinatorFlag != "" {
		// Validate that the specified session is active.
		st, err := d.CurrentStatus(coordinatorFlag)
		if err != nil {
			return "", fmt.Errorf("prism profile use: look up coordinator session %q: %w", coordinatorFlag, err)
		}
		if st == nil || st.EndedAt != nil {
			return "", fmt.Errorf("prism profile use: coordinator session %q is not active — run `prism sessions list` to see active sessions", coordinatorFlag)
		}
		return coordinatorFlag, nil
	}

	// 2. Check if PRISM_SESSION_NAME or tmux current session is a coordinator.
	parentSession := review.LookupParentSession()
	if parentSession != "" && session.IsCoordinatorSession(parentSession, d) {
		return parentSession, nil
	}

	// 3. Derive coordinator name from cwd.
	cwd, err := os.Getwd()
	if err == nil {
		candidate := deriveSessionNameFromCWD(cwd)
		if candidate != "" {
			// deriveSessionNameFromCWD returns "<repo>@<branch>"; the coordinator
			// is "<repo>@main". Reconstruct the coordinator name from the repo part.
			repoName := ""
			if atIdx := strings.Index(candidate, "@"); atIdx >= 0 {
				repoName = candidate[:atIdx]
			}
			if repoName != "" {
				coordinatorName := repoName + "@main"
				st, stErr := d.CurrentStatus(coordinatorName)
				if stErr == nil && st != nil && st.EndedAt == nil {
					return coordinatorName, nil
				}
			}
		}
	}

	// 4. Enumerate all active sessions and filter to @main sessions.
	all, err := d.AllActiveStatus()
	if err != nil {
		return "", fmt.Errorf("prism profile use: enumerate active sessions: %w", err)
	}

	var coordinators []string
	for _, st := range all {
		if strings.HasSuffix(st.SessionName, "@main") && st.EndedAt == nil {
			coordinators = append(coordinators, st.SessionName)
		}
	}

	switch len(coordinators) {
	case 0:
		callerName := parentSession
		if callerName == "" {
			callerName = "none"
		}
		return "", fmt.Errorf(
			"prism profile use: requires a coordinator session.\nCaller: %s (not a coordinator).\nNo active coordinator session found.\n\nEither:\n  - start a coordinator with: prism switch <repo>\n  - or specify one explicitly: prism profile use <profile> --coordinator <session>",
			callerName,
		)
	case 1:
		return coordinators[0], nil
	default:
		list := "  " + strings.Join(coordinators, "\n  ")
		return "", fmt.Errorf(
			"prism profile use: multiple coordinator sessions are active:\n%s\nSpecify which one to route through with --coordinator <session>.",
			list,
		)
	}
}

// profileSlotJSON is the snake_case JSON shape for a single role slot.
type profileSlotJSON struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

// profileJSON is the snake_case JSON shape for a single profile entry. The
// `slots` map is keyed by role name ("coordinator", "worker", "plan", ...).
type profileJSON struct {
	Name   string                     `json:"name"`
	Active bool                       `json:"active"`
	Slots  map[string]profileSlotJSON `json:"slots"`
}

// buildProfileJSON converts a config.ProfileEntry to the snake_case JSON
// shape, with `active` set when the profile is the resolved active one.
func buildProfileJSON(name string, entry config.ProfileEntry, active string) profileJSON {
	slots := make(map[string]profileSlotJSON, len(entry))
	for role, slot := range entry {
		slots[role] = profileSlotJSON{
			Provider: slot.Provider,
			Model:    slot.Model,
			Thinking: slot.Thinking,
		}
	}
	return profileJSON{
		Name:   name,
		Active: name == active && active != "",
		Slots:  slots,
	}
}

func runProfileList(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")

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

	if jsonMode {
		rows := make([]profileJSON, 0, len(pf.Profiles))
		for _, name := range config.AvailableProfileNames(pf) {
			rows = append(rows, buildProfileJSON(name, pf.Profiles[name], active))
		}
		data, mErr := json.Marshal(rows)
		if mErr != nil {
			return fmt.Errorf("prism profile list --json: marshal: %w", mErr)
		}
		return printJSON(data)
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
	jsonMode, _ := cmd.Flags().GetBool("json")

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

	if jsonMode {
		active, _, _ := config.ResolveActiveProfile(pf, "")
		obj := buildProfileJSON(name, entry, active)
		data, mErr := json.Marshal(obj)
		if mErr != nil {
			return fmt.Errorf("prism profile show --json: marshal: %w", mErr)
		}
		return printJSON(data)
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
