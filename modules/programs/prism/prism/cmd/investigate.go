package cmd

// prism investigate — spawn a read-only investigate-agent session and return
// the session name immediately (async).
//
// Surface:
//
//	prism investigate --prompt "<question>"
//	prism investigate --prompt-file <path>
//
// The spawned session is named <invoker>~investigate-<slug>, where slug is a
// short kebab-case token derived from the prompt. The sidecar's
// investigateAgentInvokerSession function derives the invoker from this name
// and routes per-turn notifications back correctly — no extra DB fields needed.
//
// This command is inherently async: the invoker receives per-turn
// notifications via the sidecar. There is no --wait flag.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	investigatepkg "github.com/prismatic-koi/prism/internal/investigate"
	"github.com/prismatic-koi/prism/internal/profile"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
)

// investigateSpawnSessionFn is the function used to actually spawn the
// investigator session. It defaults to session.SpawnSession; tests can swap
// it out to capture the SpawnOpts that would be passed to SpawnSession
// without performing the (tmux + sidecar) side-effects.
var investigateSpawnSessionFn = session.SpawnSession

var investigateCmd = &cobra.Command{
	Use:   "investigate",
	Short: "Spawn a read-only investigate-agent session and return immediately",
	Long: `Spawn a new investigate-agent session named <invoker>~investigate-<slug>
and return the session name immediately. The agent runs against the invoker's
worktree in read-only mode.

Per-turn notifications are delivered back to the invoker session automatically
via the sidecar. No --wait flag is provided — this command is always async.

The --name flag sets the slug portion of the session name directly:

    prism investigate --name my-analysis --prompt "..."

results in a session named <invoker>~investigate-my-analysis.

Validation rules for --name:
  - Only lowercase alphanumerics and dashes ([a-z0-9-]) are allowed.
  - Must not start or end with a dash.
  - Maximum 40 characters.

When --name is omitted, the slug is derived automatically from the prompt text.`,
	Args: cobra.NoArgs,
	RunE: runInvestigate,
}

func init() {
	addPromptFlags(investigateCmd)
	investigateCmd.Flags().String("name", "", "Human-readable slug for the session name (only [a-z0-9-], max 40 chars, no leading/trailing dash)")
	rootCmd.AddCommand(investigateCmd)
}

func runInvestigate(cmd *cobra.Command, args []string) error {
	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	suppliedName, err := cmd.Flags().GetString("name")
	if err != nil {
		return err
	}
	suppliedName = strings.TrimSpace(suppliedName)
	if suppliedName != "" {
		if err := validateInvestigateName(suppliedName); err != nil {
			return err
		}
	}

	// Container path: when running inside a sandboxed session, proxy to the
	// host sidecar's /spawn endpoint with agent set to "investigate" and the
	// session name pre-computed.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyInvestigate(apiURL, promptText, suppliedName)
	}

	// Resolve the invoker session name.
	invokerSession := os.Getenv("PRISM_SESSION_NAME")
	if invokerSession == "" {
		cwd, _ := os.Getwd()
		invokerSession = deriveSessionNameFromCWD(cwd)
	}
	if invokerSession == "" {
		return fmt.Errorf(
			"prism investigate: could not derive invoker session — " +
				"run from inside a prism session, or set PRISM_SESSION_NAME",
		)
	}

	return spawnInvestigateSession(invokerSession, promptText, suppliedName)
}

// validateInvestigateName is a thin wrapper around the shared validation helper.
func validateInvestigateName(name string) error {
	return investigatepkg.ValidateName(name)
}

// spawnInvestigateSession is the testable core of runInvestigate.
func spawnInvestigateSession(invokerSession, promptText, suppliedName string) error {
	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()
	return spawnInvestigateSessionWithDB(database, invokerSession, promptText, suppliedName)
}

// spawnInvestigateSessionWithDB is the DB-injected core of
// spawnInvestigateSession, extracted in #2113 so tests can drive the
// SpawnOpts construction against an isolated DB (sidecartest.NewIsolated)
// without going through openDB() / a real prism.db.
func spawnInvestigateSessionWithDB(database *db.DB, invokerSession, promptText, suppliedName string) error {
	spawnOpts, isoMode, harnessName, err := buildInvestigateSpawnOpts(database, invokerSession, promptText, suppliedName)
	if err != nil {
		return err
	}

	// For socket-pipe harnesses (e.g. "pi") in host isolation mode, pre-compute
	// the Unix socket path so agentPaneEnvVars can inject PRISM_HARNESS_PIPE
	// into the tmux pane. bwrap and sandbox-exec set PRISM_HARNESS_PIPE via
	// their own paths (bwrap.go --setenv, sandbox-exec profile, podman --env);
	// only inject here for host mode. Mirrors the same block in spawn.go,
	// switch.go, and restore.go (issue #2111).
	if hShape, hShapeOK := harness.ShapeOf(harnessName); hShapeOK && hShape == harness.TransportSocketPipe && string(isoMode) == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(spawnOpts.SessionName); pipeErr == nil {
			spawnOpts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism investigate] warning: could not resolve harness pipe path for %q: %v\n", spawnOpts.SessionName, pipeErr)
		}
	}

	if err := investigateSpawnSessionFn(database, spawnOpts); err != nil {
		return fmt.Errorf("prism investigate: spawn session: %w", err)
	}

	fmt.Println(spawnOpts.SessionName)
	return nil
}

// buildInvestigateSpawnOpts resolves everything spawnInvestigateSessionWithDB
// needs to know about the child session BEFORE the heavyweight
// session.SpawnSession call (which touches tmux, sidecar, and port
// allocation). Extracted from spawnInvestigateSessionWithDB in #2097 so the
// profile-inheritance path can be unit-tested without spinning up the real
// spawn machinery — the test calls this helper with a seeded DB and asserts
// on the returned SpawnOpts.ProfileName.
//
// The returned SpawnOpts.ProfileName carries the resolved profile from the
// invoker session via profile.InheritFromParent — see the inline comment
// block below for the precedence rationale.
//
// isolation mode and harness name are returned alongside the SpawnOpts so
// the #2111 HarnessPipeSockPath pre-computation step (which keys off both)
// stays in spawnInvestigateSessionWithDB without re-resolving them.
func buildInvestigateSpawnOpts(database *db.DB, invokerSession, promptText, suppliedName string) (session.SpawnOpts, config.IsolationMode, string, error) {
	status, err := database.CurrentStatus(invokerSession)
	if err != nil {
		return session.SpawnOpts{}, "", "", fmt.Errorf("prism investigate: read invoker status: %w", err)
	}
	if status == nil {
		return session.SpawnOpts{}, "", "", fmt.Errorf("prism investigate: invoker session %q has no agent_status row", invokerSession)
	}

	repo := status.Repo
	worktree := status.Worktree

	var slug string
	if suppliedName != "" {
		slug = suppliedName
	} else {
		slug = investigateSlug(promptText)
	}
	sessionName := invokerSession + "~investigate-" + slug

	cfg := config.Load()

	// Resolve isolation mode from the invoker session's DB row.
	isoMode := config.IsolationMode(status.IsolationMode)
	if isoMode == "" || isoMode == "podman" {
		isoMode = config.IsolationMode(cfg.DefaultIsolationMode)
	}

	// Resolve the profile the child investigate session should inherit
	// from its invoker (issue #2097). The precedence is identical to the
	// worker layer's #2092 chain:
	//
	//   1. Invoker's `spawn_inputs.profile_name` (highest, e.g. an
	//      `--abtest` leg's per-leg profile).
	//   2. Runtime state file `$XDG_STATE_HOME/prism/active-profile`.
	//   3. `pf.Default` — the nix-configured default, lowest.
	//
	// profiles.json is loaded best-effort: a missing file is non-fatal on
	// host-mode setups (mirrors the cmd/spawn.go pattern), and
	// ResolveActiveProfile (called inside InheritFromParent) tolerates a
	// nil ProfilesFile by falling through to the spawn-time or state-file
	// value. Errors from the state-file read path are surfaced so a
	// corrupt active-profile file does not silently pin every investigate
	// to nix-default.
	pf, _ := config.LoadProfiles()
	resolvedProfile, profErr := profile.InheritFromParent(database, invokerSession, pf)
	if profErr != nil {
		return session.SpawnOpts{}, "", "", fmt.Errorf("prism investigate: resolve active profile: %w", profErr)
	}

	// Pi is the sole harness. Use harness.Lookup("pi") as the single source of truth.
	harnessName := "pi"
	h, _ := harness.New(harnessName, "", nil, "", "")

	spawnOpts := session.SpawnOpts{
		SessionName:  sessionName,
		Repo:         repo,
		Worktree:     worktree,
		AgentRole:    "investigate",
		Prompt:       promptText,
		PromptSource: "cli-positional",
		// InvokerSession is guaranteed non-empty for `prism investigate`:
		// runInvestigate rejects an empty invoker before we get here (the
		// name is validated up front). Feeding it into SpawnOpts lets the
		// spawn_intent / spawn_failed events written by SpawnSession name
		// the invoker in their payload and lets the failure path address a
		// bus_messages audit row back to them (#2364).
		InvokerSession: invokerSession,
		// spawn_inputs audit (#2087): record the agent role on the audit row
		// so investigate spawns show up in `prism stats` group-by queries
		// alongside `prism spawn` / `prism pr` rows.
		AgentFlag:   "investigate",
		HarnessFlag: harnessName,
		// ProfileName carries the inherited profile through to the child's
		// `spawn_inputs.profile_name` row (issue #2097). The child's
		// runtime populatePIConfig reads that column via the #2092 chain
		// and resolves to the same models the invoker was spawned with,
		// instead of the host default.
		ProfileName: resolvedProfile,
		// spawn_inputs.isolation_flag: record the resolved isolation mode
		// so investigate spawns surface alongside `prism spawn` rows in the
		// stats compare "Spawn Inputs" block (issue #2102 Layer 2).
		IsolationFlag:    string(isoMode),
		Layout:           session.LayoutAgentOnly,
		IsolationMode:    string(isoMode),
		PluginHostPath:   cfg.SidecarPluginPath,
		ConfigEnvVarName: h.ConfigEnvVar(),
		RuntimeEnvVars:   h.RuntimeEnv(),
		HarnessName:      harnessName,
		ForceFresh:       true,
		WorktreeReadOnly: true,
		// PIExtensionDir for host-mode pi launches (#2065).
		PIExtensionDir: cfg.PIExtensionDir,
	}

	return spawnOpts, isoMode, harnessName, nil
}

// investigateSlug derives a short kebab-case slug from the prompt text.
// Rules: lowercase, strip punctuation, replace spaces/underscores with "-",
// truncate to ≤30 chars, trim trailing "-".
func investigateSlug(prompt string) string {
	// Normalise: lowercase.
	s := strings.ToLower(prompt)

	// Replace underscores and spaces with "-".
	s = strings.Map(func(r rune) rune {
		if r == '_' || unicode.IsSpace(r) {
			return '-'
		}
		return r
	}, s)

	// Strip non-alphanumeric, non-dash characters.
	var b strings.Builder
	for _, r := range s {
		if r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s = b.String()

	// Collapse multiple consecutive dashes.
	multiDash := regexp.MustCompile(`-{2,}`)
	s = multiDash.ReplaceAllString(s, "-")

	// Strip leading dash.
	s = strings.TrimLeft(s, "-")

	// Truncate to 30 chars at a word boundary (last "-" at or before 30).
	if len(s) > 30 {
		cap := s[:30]
		if idx := strings.LastIndex(cap, "-"); idx >= 0 {
			s = s[:idx]
		} else {
			s = cap
		}
	}

	// Trim trailing dash.
	s = strings.TrimRight(s, "-")

	if s == "" {
		return "query"
	}
	return s
}

// proxyInvestigate forwards the investigate request to the host sidecar via
// a dedicated /investigate endpoint that shells out to `prism investigate`
// on the host with PRISM_SESSION_NAME set to the invoker session.
func proxyInvestigate(apiURL, promptText, suppliedName string) error {
	invokerSession := os.Getenv("PRISM_SESSION_NAME")
	if invokerSession == "" {
		cwd, _ := os.Getwd()
		invokerSession = deriveSessionNameFromCWD(cwd)
	}
	if invokerSession == "" {
		return fmt.Errorf(
			"prism investigate: could not derive invoker session — " +
				"run from inside a prism session, or set PRISM_SESSION_NAME",
		)
	}

	var resp struct {
		SessionName string `json:"session_name"`
	}
	body := map[string]any{
		"prompt": promptText,
		"from":   invokerSession,
	}
	if suppliedName != "" {
		body["name"] = suppliedName
	}
	if err := proxyToHostAPI(apiURL, "/investigate", body, &resp); err != nil {
		return err
	}
	fmt.Println(resp.SessionName)
	return nil
}
