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
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/session"
)

var investigateCmd = &cobra.Command{
	Use:   "investigate",
	Short: "Spawn a read-only investigate-agent session and return immediately",
	Long: `Spawn a new investigate-agent session named <invoker>~investigate-<slug>
and return the session name immediately. The agent runs against the invoker's
worktree in read-only mode.

Per-turn notifications are delivered back to the invoker session automatically
via the sidecar. No --wait flag is provided — this command is always async.`,
	Args: cobra.NoArgs,
	RunE: runInvestigate,
}

func init() {
	addPromptFlags(investigateCmd)
	rootCmd.AddCommand(investigateCmd)
}

func runInvestigate(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	// Container path: when running inside a sandboxed session, proxy to the
	// host sidecar's /spawn endpoint with agent set to "investigate" and the
	// session name pre-computed.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyInvestigate(apiURL, promptText)
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

	return spawnInvestigateSession(invokerSession, promptText)
}

// spawnInvestigateSession is the testable core of runInvestigate.
func spawnInvestigateSession(invokerSession, promptText string) error {
	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	status, err := database.CurrentStatus(invokerSession)
	if err != nil {
		return fmt.Errorf("prism investigate: read invoker status: %w", err)
	}
	if status == nil {
		return fmt.Errorf("prism investigate: invoker session %q has no agent_status row", invokerSession)
	}

	repo := status.Repo
	worktree := status.Worktree

	slug := investigateSlug(promptText)
	sessionName := invokerSession + "~investigate-" + slug

	cfg := config.Load()

	// Load profiles best-effort; non-fatal if absent.
	pf, _ := config.LoadProfiles()

	// Resolve isolation mode from the invoker session's DB row.
	isoMode := config.IsolationMode(status.IsolationMode)
	if isoMode == "" || isoMode == "podman" {
		isoMode = config.IsolationMode(cfg.DefaultIsolationMode)
	}

	// Use the default harness, falling back to "opencode".
	harnessName := "opencode"
	if pf != nil && pf.DefaultHarness != "" {
		harnessName = pf.DefaultHarness
	}
	h, _ := harness.New(harnessName, "", nil, "", "")

	spawnOpts := session.SpawnOpts{
		SessionName:      sessionName,
		Repo:             repo,
		Worktree:         worktree,
		AgentRole:        "investigate",
		Prompt:           promptText,
		PromptSource:     "cli-positional",
		Layout:           session.LayoutAgentOnly,
		IsolationMode:    string(isoMode),
		PluginHostPath:   cfg.SidecarPluginPath,
		ConfigEnvVarName: h.ConfigEnvVar(),
		RuntimeEnvVars:   h.RuntimeEnv(),
		HarnessName:      harnessName,
		ForceFresh:       true,
		WorktreeReadOnly: true,
	}

	if err := session.SpawnSession(database, spawnOpts); err != nil {
		return fmt.Errorf("prism investigate: spawn session: %w", err)
	}

	fmt.Println(sessionName)
	return nil
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

	// Truncate to 30 chars.
	if len(s) > 30 {
		s = s[:30]
	}

	// Trim trailing dash.
	s = strings.TrimRight(s, "-")

	if s == "" {
		return "query"
	}
	return s
}

// proxyInvestigate forwards the investigate request to the host sidecar via
// the /spawn endpoint with agent="investigate" and a pre-computed session name.
func proxyInvestigate(apiURL, promptText string) error {
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

	slug := investigateSlug(promptText)
	sessionName := invokerSession + "~investigate-" + slug

	var resp struct {
		SessionName string `json:"session_name"`
	}
	body := map[string]any{
		"prompt":       promptText,
		"agent":        "investigate",
		"session_name": sessionName,
	}
	if err := proxyToHostAPI(apiURL, "/spawn", body, &resp); err != nil {
		return err
	}
	name := resp.SessionName
	if name == "" {
		name = sessionName
	}
	fmt.Println(name)
	return nil
}
