package cmd

// prism spawn — create a new timestamped worktree from the current (or named)
// repo and switch to it immediately. Bound to prefix+a.
//
// Flags:
//
//	--branch <name>              use a specific branch name instead of a timestamp
//	--pr <number>                check out the branch for a given PR number
//	--repo <name>                repo shorthand name or absolute path (default: inferred from current pane)
//	--prompt <text>              pass an initial prompt to the agent on launch
//	--prompt-file <path>         read the initial prompt from a file
//	--agent <name>               agent to use (default: "coordinator" on main, "worker" otherwise)
//	--profile <name>             model profile to use from ~/.config/prism/profiles.json
//	--model <name>               model identifier override (overrides profile's primary model)
//	--variant <name>             model variant override (overrides all agents' variant)
//	--model-override role=model  per-role model override (repeatable); overrides --model for that role
//	--isolation <mode>           isolation mode: podman, bwrap, sandbox-exec, or host (default: from config.json)
//	--harness <name>             agent harness to use (default: "pi")

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/skills"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// proxySpawn forwards a spawn request to the host-API sidecar when running
// inside a container (PRISM_HOST_API is set). It reads the same flags as
// runSpawn and POSTs them to /spawn, then prints the returned session name.
//
// The "repo" field is intentionally omitted from the request: the sidecar
// derives the repo from its own session name, so a client running inside a
// container where PRISM_BARE_ROOT is a mount-path name (e.g. "/prism-git")
// does not need to supply the correct repo name. See issue #616.
//
// The "isolation" field forwards the --isolation flag value when explicitly
// set. Validation of unknown values happens client-side here so the error
// surfaces immediately at the proxy boundary with the same error message
// shape as the direct (host-shell) path; see issue #1059. Without this, a
// coordinator running inside a container could silently drop --isolation host
// (or any other valid value) because the proxy never read the flag — the
// sidecar fell back to its default mode and the user only noticed by reading
// the sidecar log line.
func proxySpawn(apiURL string, cmd *cobra.Command) error {
	branchFlag, _ := cmd.Flags().GetString("branch")
	prFlag, _ := cmd.Flags().GetString("pr")
	agentFlag, _ := cmd.Flags().GetString("agent")
	profileFlag, _ := cmd.Flags().GetString("profile")
	abtestFlag, _ := cmd.Flags().GetStringArray("abtest")
	modelFlag, _ := cmd.Flags().GetString("model")
	variantFlag, _ := cmd.Flags().GetString("variant")
	harnessFlag, _ := cmd.Flags().GetString("harness")
	ignoreConcurrencyCapFlag, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	isolationFlag, _ := cmd.Flags().GetString("isolation")
	modelOverrideFlag, _ := cmd.Flags().GetStringArray("model-override")
	reuseFlag, _ := cmd.Flags().GetBool("reuse")

	isolationChanged := cmd.Flags().Changed("isolation")

	// Validate --isolation client-side so unknown values fail fast with the
	// same error message as the direct path. The platform guards in
	// resolveIsolationMode (bwrap on non-Linux, sandbox-exec on non-Darwin)
	// are intentionally NOT applied here: the proxy is running inside a
	// container, but the spawned session lands on the host, so the host's
	// platform — not the proxy's — is what matters. The host-side prism spawn
	// re-runs resolveIsolationMode and will reject an incompatible mode there.
	if isolationChanged {
		if !config.IsValidIsolationMode(isolationFlag) {
			valid := make([]string, len(config.ValidIsolationModes))
			for i, m := range config.ValidIsolationModes {
				valid[i] = string(m)
			}
			return fmt.Errorf("unknown isolation mode %q; valid values: %s", isolationFlag, strings.Join(valid, ", "))
		}
	}

	// --abtest and --profile are mutually exclusive — enforce client-side for
	// fast feedback before the round-trip to the host API.
	if len(abtestFlag) > 0 && cmd.Flags().Changed("profile") {
		return fmt.Errorf("--abtest and --profile are mutually exclusive")
	}
	// Validate --abtest count early: we only allow 0 or 2 values.
	if len(abtestFlag) == 1 || len(abtestFlag) > 2 {
		return fmt.Errorf("--abtest requires exactly two profile names (got %d)", len(abtestFlag))
	}

	// Resolve --pr to a branch client-side when running inside a container.
	// git is accessible from containers (PRISM_BARE_ROOT is set), so we can
	// look up the PR branch here and forward it as --branch to the host API.
	// The --pr flag itself is not forwarded because the host-side handler only
	// accepts a branch name.
	if prFlag != "" {
		if branchFlag != "" {
			return fmt.Errorf("--pr and --branch are mutually exclusive")
		}
		bareRoot, bareErr := resolveBareRoot("")
		if bareErr != nil {
			return bareErr
		}
		resolvedBranch, branchErr := resolveBranch(bareRoot, "", prFlag)
		if branchErr != nil {
			return branchErr
		}
		branchFlag = resolvedBranch
	}

	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}
	// Keybind carve-out (issue #2063 — parity with the host-side carve-out
	// added for issue #2012). The tmux Prefix+a keybind invokes `prism spawn
	// --attach` with no --prompt because the operator types the initial
	// prompt to the live agent after the popup attaches.
	//
	// The host-side runSpawn uses `PRISM_SPAWN_PATH != ""` as the keybind
	// discriminator, but inside a container PRISM_SPAWN_PATH is set
	// UNCONDITIONALLY by every sandbox (bwrap.go:496-503,
	// agent_run_sandbox_exec_darwin.go:284) — it is documented as a
	// working-directory hint, not a sandbox sentinel
	// (internal/sandboxenv/sandboxenv.go). Using it as the sole discriminator
	// here would fire on every container-originated `prism spawn`, not just
	// keybind spawns, and the resulting `from_keybind: true` would flip the
	// host-side child's `headless := !fromKeybind && !attachFlag` from true
	// to false on every container worker-spawn flow — causing the host child
	// to call session.Attach against whatever tmux client the sidecar
	// inherited.
	//
	// Narrow the carve-out to the only case where it materially matters: an
	// EMPTY prompt. That is the exact symptom #2012/#2063 set out to fix
	// (Prefix+a popup flash-close from an unconditional empty-prompt reject),
	// and it cannot break a non-empty-prompt invocation because the
	// non-empty path is byte-identical to today. A coordinator/worker that
	// passes `--prompt "X"` from inside a container hits the same code path
	// it did pre-PR.
	fromKeybind := os.Getenv("PRISM_SPAWN_PATH") != "" && promptFlag == ""
	// Reject an empty prompt at the operator boundary (layers 1+2 of issue
	// #1891). Without this, an empty --prompt-file, --prompt "", or empty
	// stdin produces a session that is created successfully on every
	// observable surface but never receives a prompt and sits idle forever.
	// The host-API /spawn handler has a defence-in-depth check too (layer 3);
	// this surfaces the error in the caller's stderr instead of an HTTP 400.
	if promptFlag == "" && !fromKeybind {
		return emptyPromptError(cmd, "prism spawn")
	}

	harnessChanged := cmd.Flags().Changed("harness")

	// Abtest path: POST with abtest field and parse two session names from response.
	if len(abtestFlag) == 2 {
		var resp struct {
			SessionNames []string `json:"session_names"`
		}
		body := map[string]any{
			"branch":                 branchFlag,
			"prompt":                 promptFlag,
			"agent":                  agentFlag,
			"model":                  modelFlag,
			"variant":                variantFlag,
			"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
			"abtest":                 abtestFlag,
		}
		// Forward the keybind discriminator so the host-side /spawn handler
		// permits an empty prompt on this request. The layer-3 handler still
		// rejects empty prompts from arbitrary HTTP callers that omit this
		// field — see issue #2063.
		if fromKeybind {
			body["from_keybind"] = true
		}
		// Only forward "harness" when explicitly set. When absent, the host-side
		// spawn derives the harness from the profile slot as designed (#1421).
		if harnessChanged {
			body["harness"] = harnessFlag
		}
		if isolationChanged {
			body["isolation"] = isolationFlag
		}
		if len(modelOverrideFlag) > 0 {
			modelsByRole, parseErr := parseModelOverrides(modelOverrideFlag)
			if parseErr != nil {
				return parseErr
			}
			if len(modelsByRole) > 0 {
				if encoded, encErr := json.Marshal(modelsByRole); encErr == nil {
					body["model_variant_overrides"] = string(encoded)
				}
			}
		}
		if err := proxyToHostAPI(apiURL, "/spawn", body, &resp); err != nil {
			return err
		}
		for _, sn := range resp.SessionNames {
			fmt.Printf("session %q created\n", sn)
		}
		// --wait on the abtest path is not supported — there are two
		// sessions and no single terminal definition. Surface this rather
		// than silently dropping the flag (issue #1500 review-code feedback).
		if waitFlag, _ := cmd.Flags().GetBool("wait"); waitFlag {
			fmt.Fprintln(os.Stderr, "prism spawn --wait: not supported with --abtest (two sessions, no single terminal); skipping wait")
		}
		return nil
	}

	var resp struct {
		SessionName string `json:"session_name"`
	}
	body := map[string]any{
		"branch":                 branchFlag,
		"prompt":                 promptFlag,
		"agent":                  agentFlag,
		"profile":                profileFlag,
		"model":                  modelFlag,
		"variant":                variantFlag,
		"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
		"reuse":                  reuseFlag,
	}
	// Forward the keybind discriminator so the host-side /spawn handler
	// permits an empty prompt on this request. The layer-3 handler still
	// rejects empty prompts from arbitrary HTTP callers that omit this
	// field — see issue #2063.
	if fromKeybind {
		body["from_keybind"] = true
	}
	if len(modelOverrideFlag) > 0 {
		modelsByRole, parseErr := parseModelOverrides(modelOverrideFlag)
		if parseErr != nil {
			return parseErr
		}
		if len(modelsByRole) > 0 {
			if encoded, err := json.Marshal(modelsByRole); err == nil {
				body["model_variant_overrides"] = string(encoded)
			}
		}
	}
	// Only forward "harness" when explicitly set. When absent, the host-side
	// spawn derives the harness from the profile slot as designed (#1421).
	if harnessChanged {
		body["harness"] = harnessFlag
	}
	// Only forward "isolation" when explicitly set. An empty value would tell
	// the host-side spawn to fall back to config.json, which is correct only
	// when the user really did not pass the flag — we must distinguish the
	// "absent" and "empty" cases here.
	if isolationChanged {
		body["isolation"] = isolationFlag
	}
	if err := proxyToHostAPI(apiURL, "/spawn", body, &resp); err != nil {
		return err
	}
	// --wait: route through waitForSpawnTerminal using the sandbox-aware
	// probe. Without this the proxy path silently dropped --wait and
	// returned immediately even though the caller asked for synchronous
	// behaviour (issue #1500 review-code feedback).
	waitFlag, _ := cmd.Flags().GetBool("wait")
	if waitFlag {
		jsonFlag, _ := cmd.Flags().GetBool("json")
		waitTimeout, _ := cmd.Flags().GetDuration("wait-timeout")
		if !jsonFlag {
			fmt.Printf("session %q spawned; waiting for terminal state...\n", resp.SessionName)
		}
		return waitForSpawnTerminal(resp.SessionName, jsonFlag, waitTimeout)
	}
	fmt.Printf("session %q created\n", resp.SessionName)
	return nil
}

var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Create a new worktree from the current (or named) repo and switch to it",
	RunE:  runSpawn,
}

func init() {
	spawnCmd.Flags().String("branch", "", "Branch name (default: timestamped)")
	spawnCmd.Flags().String("pr", "", "PR number — check out its branch")
	spawnCmd.Flags().String("repo", "", "Repo shorthand name or absolute path (default: inferred from current pane)")
	addPromptFlags(spawnCmd)
	spawnCmd.Flags().String("agent", "", `Agent to use (default: "coordinator" on main, "worker" otherwise)`)
	spawnCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	spawnCmd.Flags().String("profile", "", "Model profile name from ~/.config/prism/profiles.json (e.g. anthropic, gemini-hybrid)")
	spawnCmd.Flags().StringArray("abtest", nil, "A/B test: spawn two sessions with the given profile names (e.g. --abtest profileA --abtest profileB); mutually exclusive with --profile")
	spawnCmd.Flags().String("model", "", "Model identifier override (e.g. anthropic/claude-sonnet-4-6); overrides profile's primary model")
	spawnCmd.Flags().String("variant", "", "Model variant override for all agents (e.g. high, max, minimal)")
	spawnCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable, e.g. review-context=google/gemini-2.5-pro)")
	spawnCmd.Flags().String("isolation", "", "Isolation mode: podman, bwrap, sandbox-exec, or host (default: from ~/.config/prism/config.json)")
	spawnCmd.Flags().String("harness", "pi", "Agent harness to use; valid values are determined by registered harnesses")
	spawnCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	spawnCmd.Flags().Bool("wait", false, "Block until the spawned agent finishes its initial prompt. Without --wait, returns immediately.")
	spawnCmd.Flags().Duration("wait-timeout", defaultSpawnWaitTimeout, "Timeout for --wait. Ignored when --wait is not set.")
	spawnCmd.Flags().Bool("json", false, "Emit the terminal status as a JSON object on stdout (only useful with --wait). Suppresses textual output.")
	spawnCmd.Flags().Bool("reuse", false, "If a healthy session already exists on the requested branch, return its details and exit 0 instead of failing")
	// --prompt-source is an internal flag used by the host-API /spawn handler
	// to override the auto-detected prompt source (C.4.SRC, issue #1148).
	// It is hidden from --help because end users should never pass it directly.
	spawnCmd.Flags().String("prompt-source", "", "")
	_ = spawnCmd.Flags().MarkHidden("prompt-source")
	rootCmd.AddCommand(spawnCmd)
}

// resolveIsolationMode returns the effective isolation mode for a spawn
// invocation, applying flag precedence and validation via registry.Resolve:
//
//  1. --isolation flag (explicit override), validated against known values
//  2. cfg.DefaultIsolationMode (from config.json; compiled-in default "host")
//
// Returns an error if --isolation has an unknown value, or if the resolved
// mode is "bwrap" on a non-Linux platform, or if the resolved mode is
// "sandbox-exec" on a non-Darwin platform.
//
// D1 (issue #1133): platform availability is checked via the registered
// Isolator's Available() method — but only for non-container modes. The
// podman binary/socket/image checks live in the runSpawn caller (gated by
// isoCaps.IsContainer) so resolveIsolationMode keeps its pre-refactor
// surface (no podman daemon required to resolve the mode under test).
func resolveIsolationMode(cmd *cobra.Command, cfg config.Config) (config.IsolationMode, error) {
	isolationFlag, _ := cmd.Flags().GetString("isolation")

	mode, err := container.Resolve(container.ResolveInput{
		IsolationFlag:        isolationFlag,
		IsolationFlagChanged: cmd.Flags().Changed("isolation"),
		ConfigDefault:        cfg.DefaultIsolationMode,
	})
	if err != nil {
		return "", err
	}
	// Skip Available() for container modes — the container availability
	// check (podman binary, socket, image) runs later in runSpawn so it
	// happens after the worktree-irrelevant pre-flight is complete and so
	// resolveIsolationMode itself stays usable on hosts without podman.
	if container.CapabilitiesFor(mode).IsContainer {
		return mode, nil
	}
	iso, err := container.For(mode, container.ConstructorOpts{})
	if err != nil {
		return "", err
	}
	if err := iso.Available(); err != nil {
		return "", err
	}
	return mode, nil
}

func runSpawn(cmd *cobra.Command, args []string) error {
	// Silence the cobra usage block for runtime errors. Flag parse errors
	// (unknown flags, wrong argument count) are handled before RunE is called
	// and still print usage — this only silences errors returned from RunE.
	cmd.SilenceUsage = true

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxySpawn(apiURL, cmd)
	}

	// --abtest is mutually exclusive with --profile. Check before any
	// side-effects so the error surfaces immediately.
	abtestProfiles, _ := cmd.Flags().GetStringArray("abtest")
	profileFlag, _ := cmd.Flags().GetString("profile")
	if len(abtestProfiles) > 0 && cmd.Flags().Changed("profile") {
		return fmt.Errorf("--abtest and --profile are mutually exclusive")
	}
	// Route to the abtest path when --abtest is provided with exactly two profiles.
	if len(abtestProfiles) > 0 {
		if len(abtestProfiles) != 2 {
			return fmt.Errorf("--abtest requires exactly two profile names (got %d)", len(abtestProfiles))
		}
		return runAbtestSpawn(cmd, abtestProfiles[0], abtestProfiles[1])
	}
	_ = profileFlag // used below

	branchFlag, _ := cmd.Flags().GetString("branch")
	prFlag, _ := cmd.Flags().GetString("pr")
	repoFlag, _ := cmd.Flags().GetString("repo")
	agentFlag, _ := cmd.Flags().GetString("agent")
	// profileFlag was already read above for the abtest mutual-exclusion check.
	modelFlag, _ := cmd.Flags().GetString("model")
	variantFlag, _ := cmd.Flags().GetString("variant")
	harnessFlag, _ := cmd.Flags().GetString("harness")
	modelOverrideRaw, _ := cmd.Flags().GetStringArray("model-override")
	modelsByRole, err := parseModelOverrides(modelOverrideRaw)
	if err != nil {
		return err
	}

	// Validate harness BEFORE any session state is created (no worktree, no
	// tmux session, no DB row).
	// Note: harness resolution from profile slot happens later (after the profile
	// and planned role are resolved), so validation runs a second time there too.
	if _, ok := harness.Lookup(harnessFlag); !ok {
		return fmt.Errorf("unknown harness %q: valid harnesses: %s", harnessFlag, strings.Join(harness.Names(), ", "))
	}

	promptText, promptSource, err := resolvePromptWithSource(cmd)
	if err != nil {
		return err
	}
	// --prompt-source is a hidden internal flag set by the host-API /spawn
	// handler so that proxy-spawned sessions carry "proxy-spawn" instead of
	// the auto-detected "cli-positional" (C.4.SRC, issue #1148).
	if overrideSource, _ := cmd.Flags().GetString("prompt-source"); overrideSource != "" {
		promptSource = overrideSource
	}
	attachFlag, _ := cmd.Flags().GetBool("attach")
	// headless when invoked from a shell/agent rather than the tmux keybinding.
	// The keybinding sets PRISM_SPAWN_PATH; --attach overrides to force a switch.
	fromKeybind := os.Getenv("PRISM_SPAWN_PATH") != ""
	// Reject an empty prompt at the operator boundary (issue #1891). When the
	// hidden --prompt-source flag is set we are running as the child of the
	// host-API /spawn handler, which has already validated that req.Prompt is
	// non-empty (layer 3); skipping the check there keeps the proxy's own
	// 400 surface as the source of truth for that path.
	//
	// Keybind carve-out (issue #2012): the tmux Prefix+a keybind invokes
	// `prism spawn --attach` with no --prompt because the operator types the
	// initial prompt to the live agent after the popup attaches. The keybind
	// sets PRISM_SPAWN_PATH (the `fromKeybind` discriminator), so an empty
	// prompt on that path is legitimate and must be allowed through.
	if promptText == "" && promptSource != "proxy-spawn" && !fromKeybind {
		return emptyPromptError(cmd, "prism spawn")
	}
	cfg := config.Load()

	// Resolve the effective isolation mode. This validates --isolation
	// and falls back to config.json.
	// Done BEFORE any side effects (no worktree, no tmux session, no DB row).
	//
	// Note: project_isolation_overrides is intentionally NOT consulted here.
	// prism spawn creates a new worktree in a git repo and should respect the
	// explicit --isolation flag and machine default only. The per-path override
	// is applied by prism switch (opening an existing path) and prism restore
	// (re-creating a session for a stored path). This matches the edge-case AC
	// in issue #1404: "prism spawn from inside a session does not consult
	// project_isolation_overrides — it uses the caller's isolation mode
	// resolution path unchanged."
	isolationMode, err := resolveIsolationMode(cmd, cfg)
	if err != nil {
		return err
	}

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isolationMode)

	// Container availability check: when container mode is active verify
	// podman is available before touching anything. resolveIsolationMode
	// has already validated platform availability for bwrap / sandbox-exec
	// (D1: iso.Available()); the podman binary/socket/image checks remain
	// here because they fire only when isoCaps.IsContainer.
	if isoCaps.IsContainer {
		iso, isoErr := container.For(isolationMode, container.ConstructorOpts{})
		if isoErr != nil {
			return isoErr
		}
		if err := iso.Available(); err != nil {
			return err
		}
	}

	// Concurrency cap checks: BEFORE any container-creation side effects
	// (no worktree, no tmux session, no DB row on refusal).
	// The podman cap guards host memory against container overhead.
	// The bwrap cap guards against process-count exhaustion from uncapped
	// bwrap sessions (each is a host process with no per-session memory ceil).
	// The sandbox-exec cap mirrors the bwrap cap for Darwin sessions.
	//
	// A.3 (#1134): unified cap via iso.Cap(ctx, dbPath).Check(ignoreCap).
	if err := checkConcurrencyCap(cmd, "spawn", isolationMode); err != nil {
		return err
	}

	// Load the profiles file. It carries container role configs, model profile
	// overrides, and agent_env_vars for host-mode injection. Always attempt to
	// load; treat missing file as fatal only when container mode or --profile
	// is active (those paths strictly require the file). For host-mode sessions
	// without a profile flag, a missing file is non-fatal — agent env vars
	// simply won't be injected.
	var pf *config.ProfilesFile
	{
		var loadErr error
		pf, loadErr = config.LoadProfiles()
		if loadErr != nil {
			if isoCaps.NeedsConfigBlob || profileFlag != "" {
				return loadErr
			}
			proglog.Warnf("[prism spawn] warning: could not load profiles.json (agent env vars will not be injected): %v\n", loadErr)
			pf = nil
		}
	}
	// Resolve the active profile applying the runtime-state file precedence
	// from #1207:
	//
	//   1. Explicit --profile flag (highest)
	//   2. Runtime state file at $XDG_STATE_HOME/prism/active-profile
	//   3. pf.Default (the nix-configured default, lowest)
	//
	// Errors from the state-file read path are surfaced — corrupt state is
	// a real problem, not a fallthrough condition.
	resolvedProfile, profileSource, err := config.ResolveActiveProfile(pf, profileFlag)
	if err != nil {
		return err
	}
	_ = profileSource // intentionally unused — kept for future debug logging
	// configContent is populated below, after the session role is determined.
	var configContent string

	// Resolve the bare repo root from the current pane path (or --repo flag).
	bareRoot, err := resolveBareRoot(repoFlag)
	if err != nil {
		return err
	}

	// Resolve the branch name.
	branch, err := resolveBranch(bareRoot, branchFlag, prFlag)
	if err != nil {
		return err
	}

	// Pre-flight dedupe check: refuse (or reuse) when an active session already
	// exists for this repo+branch. This prevents git/tmux/DB conflicts from
	// a half-failed retry and gives the caller a structured error pointing at
	// recovery options. The check is a single DB query — no worktree, no tmux
	// session, no DB row is created before this point.
	reuseFlag, _ := cmd.Flags().GetBool("reuse")
	{
		d, dbErr := openDB()
		if dbErr == nil {
			defer d.Close()
			repoName := strings.TrimSuffix(filepath.Base(bareRoot), ".git")
			existing, lookupErr := d.ActiveStatusForRepoBranch(repoName, branch)
			if lookupErr == nil && existing != nil {
				// There is an active session for this branch.
				// Healthy = state not "error" and not "deleted".
				broken := existing.State == "error" || existing.State == "deleted"
				if reuseFlag {
					if broken {
						return fmt.Errorf(
							"prism spawn --reuse: existing session %q is in a broken state (%s)\n"+
								"run: prism cleanup --yes --session %s",
							existing.SessionName, existing.State, existing.SessionName)
					}
					// Healthy session — emit its details and exit 0.
					agentName := ""
					if existing.AgentName != nil {
						agentName = *existing.AgentName
					}
					port := 0
					if existing.HarnessPort != nil {
						port = *existing.HarnessPort
					}
					fmt.Fprintf(cmd.OutOrStdout(), "reuse: existing session %q (agent: %s, port: %d)\n",
						existing.SessionName, agentName, port)
					return nil
				}
				// No --reuse: refuse with a structured error.
				return fmt.Errorf(
					"prism spawn: branch %q already has an active session %q\n"+
						"to clean it up: prism cleanup --yes --session %s\n"+
						"to reuse it: prism spawn --branch %s --reuse",
					branch, existing.SessionName, existing.SessionName, branch)
			}
		}
	}

	// Validate the active profile defines a slot for the session's role
	// before spending I/O on worktree creation (#1206 edge case AC). The
	// active profile here is whatever ResolveActiveProfile returned above —
	// flag → state file → nix default — so a stale runtime state file
	// pointing at an invalid profile is rejected here with the same shape
	// of error as a bad --profile flag (#1207 edge-case AC).
	//
	// The session role is the explicit --agent flag when set, otherwise
	// inferred from the branch name (main → coordinator, anything else →
	// worker, mirroring session.DefaultAgent).
	if pf != nil && resolvedProfile != "" {
		plannedRole := agentFlag
		if plannedRole == "" {
			if branch == "main" {
				plannedRole = "coordinator"
			} else {
				plannedRole = "worker"
			}
		}
		if err := config.RequireSlot(pf, resolvedProfile, plannedRole); err != nil {
			// When the failure stems from the state file (not the flag),
			// add a hint pointing at `prism profile use` so users have a
			// clear recovery path (#1207 edge-case AC).
			if profileFlag == "" && profileSource == "state-file" {
				return fmt.Errorf("%w\nhint: the active profile is set via the runtime state file ($XDG_STATE_HOME/prism/active-profile). Run `prism profile use <name>` to switch to a valid profile",
					err)
			}
			return err
		}

		// Generate the pi harness config for this session's root role.
		// BuildConfigContent applies the profile's slot model/variant and
		// honours any --model/--variant overrides on the root role only.
		var bccErr error
		configContent, bccErr = config.BuildConfigContent(pf, resolvedProfile, plannedRole, modelFlag, variantFlag)
		if bccErr != nil {
			return bccErr
		}

		// Pi is the sole harness. Use it directly unless --harness was
		// explicitly set by the caller.
		if !cmd.Flags().Changed("harness") {
			harnessFlag = "pi"
		}
	}

	// If no profile is active but --model or --variant were passed, still
	// generate a minimal config so the overrides take effect.
	if configContent == "" && (modelFlag != "" || variantFlag != "") {
		rootRole := agentFlag
		if rootRole == "" {
			if branch == "main" {
				rootRole = "coordinator"
			} else {
				rootRole = "worker"
			}
		}
		var bccErr error
		configContent, bccErr = config.BuildConfigContent(pf, resolvedProfile, rootRole, modelFlag, variantFlag)
		if bccErr != nil {
			return bccErr
		}
	}

	// Create the worktree (handles local, remote-tracking, and new branches).
	worktreePath, err := git.CreateWorktree(bareRoot, branch)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// For bwrap sessions, write the opencode.json config file to disk now so
	// it is present before the agent pane opens. prism agent-run reconstructs
	// a container.Manager from DB state (which does not carry ConfigContent),
	// so the file must be written here at spawn time via the deterministic temp
	// path. The bwrap.go mount-emission block checks file existence (os.Stat)
	// rather than cfg.ConfigContent, so it picks this up correctly.
	//
	// Podman mode does NOT need this write — the sidecar's Create() path at
	// container.go:580 already writes the file before the container starts.
	// Host mode does NOT need this write — it uses ~/.config/opencode/opencode.json
	// directly via xdg.configFile.
	//
	// IMPORTANT: the path key used here must match the one used by Manager
	// internally. Manager.name = container.NameForSession(tmuxSessionName)
	// (e.g. "prism-nixos-config-feat"), and Manager.harnessConfigFilePath()
	// calls HarnessConfigFilePath(m.name). The Isolator.WriteHarnessConfigBlob
	// method translates the prism session name to the container name internally
	// so this call site stays mode-agnostic (D3, issue #1133).
	if isoCaps.NeedsConfigBlob && configContent != "" {
		tmuxSessionName := session.NameFor(worktreePath, bareRoot)
		iso, isoErr := container.For(isolationMode, container.ConstructorOpts{Name: tmuxSessionName})
		if isoErr != nil {
			return fmt.Errorf("spawn: %w", isoErr)
		}
		if err := iso.WriteHarnessConfigBlob(tmuxSessionName, configContent); err != nil {
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// Build SpawnOpts and call session.SpawnSession — the single shared
	// primitive for creating a prism session end-to-end (port allocation,
	// DB seed with root_agent_name, tmux session creation, sidecar startup).
	// See #849 §3.1 and #859.
	sessionName := session.NameFor(worktreePath, bareRoot)
	// DefaultAgent (not DefaultAgentForSession) is intentional here: at spawn
	// time the session does not yet have a DB row, so there is no root_agent_name
	// to read back. This call ASSIGNS the agent type for the new session (written
	// to DB via SpawnOpts.AgentRole → UpsertStatusSeedRootAgentName). Reads of an
	// existing session's type use DefaultAgentForSession (e.g. in restore.go).
	agentRole := session.DefaultAgent(worktreePath, agentFlag)
	headless := !fromKeybind && !attachFlag

	// Resolve harness-specific env var names and runtime env vars.
	// These are populated from the harness adapter and threaded through
	// SpawnOpts → Opts → buildDirectOpencodeCmd so that no
	// harness-specific string literals appear in the session package.
	// harnessFlag was already validated above, so the error is unreachable.
	h, _ := harness.New(harnessFlag, "", nil, "", "")
	// Keybind carve-out (issue #2012): when the spawn was initiated by the
	// tmux Prefix+a keybind and no prompt was supplied, opt out of the layer-4
	// empty-prompt guard in SpawnSession. The operator types the initial
	// prompt to the live agent after the popup attaches. Any other combination
	// (non-keybind, or keybind with an explicit --prompt) keeps the original
	// #1891 guard active.
	allowEmptyPrompt := fromKeybind && promptText == ""
	spawnOpts := session.SpawnOpts{
		SessionName:      sessionName,
		Repo:             deriveRepo(worktreePath),
		Worktree:         worktreePath,
		AgentRole:        agentRole,
		Prompt:           promptText,
		PromptSource:     promptSource,
		ConfigContent:    configContent,
		Layout:           session.LayoutFull,
		IsolationMode:    string(isolationMode),
		PluginHostPath:   cfg.SidecarPluginPath,
		ConfigEnvVarName: h.ConfigEnvVar(),
		RuntimeEnvVars:   h.RuntimeEnv(),
		HarnessName:      harnessFlag,
		ModelsByRole:     modelsByRole,
		AllowEmptyPrompt: allowEmptyPrompt,
		// PIExtensionDir is the host-side prism PI extension directory
		// (populated by Nix into config.json). Forwarded so that host-mode
		// pi launches pass --extension <dir>/prism.ts (#2065).
		PIExtensionDir: cfg.PIExtensionDir,
		// ForceFresh=true: spawn always wants a new instance. If a session
		// with the same name already exists it is a stale zombie and should
		// be killed.
		ForceFresh: true,
		Headless:   headless,
		// WorktreeReadOnly: mount the worktree read-only for investigate sessions
		// (defence in depth — denylist prevents writes at the bash level;
		// read-only mount ensures even a denylist gap cannot modify the repo).
		WorktreeReadOnly: agentRole == "investigate",
		// ReadinessTimeout=DefaultReadinessTimeout (30s) gates SpawnSession's
		// return on the agent actually binding its port (#1051 AC-14).
		// Single-worker spawns benefit from the same readiness check that
		// review fan-outs do: an operator running `prism spawn --branch foo`
		// sees a clear "failed to start: not ready within 30s" instead of
		// "session created" followed by a session that idles forever
		// because the agent never came up.
		ReadinessTimeout: session.DefaultReadinessTimeout,
	}
	// For socket-pipe harnesses (e.g. "pi") in host isolation mode, pre-compute
	// the Unix socket path so agentPaneEnvVars can inject PRISM_HARNESS_PIPE
	// into the tmux pane. bwrap and sandbox-exec set PRISM_HARNESS_PIPE via
	// their own paths (bwrap.go --setenv); only inject here for host mode.
	if hShape, hShapeOK := harness.ShapeOf(harnessFlag); hShapeOK && hShape == harness.TransportSocketPipe && string(isolationMode) == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(sessionName); pipeErr == nil {
			spawnOpts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism spawn] warning: could not resolve harness pipe path for %q: %v\n", sessionName, pipeErr)
		}
	}
	// AgentEnvVars only applies to host-mode sessions; container sessions
	// receive env vars via podman --env flags in the sidecar.
	if pf != nil && !isoCaps.IsContainer {
		spawnOpts.AgentEnvVars = pf.AgentEnvVars
	}

	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("spawn: open db: %w", dbErr)
	}
	defer d.Close()

	// C4.SK: compute skills manifest hash before spawn so it is available for
	// spawn_inputs. Read skills from XDG_CONFIG_HOME/prism/skills/ (with the
	// standard ~/.config fallback). Errors are non-fatal: a missing or
	// unreadable skills directory produces an empty hash (caller writes NULL).
	skillsDir := prismSkillsDir()
	skillsManifestHash, skillsHashErr := skills.ComputeManifest(skillsDir)
	if skillsHashErr != nil {
		proglog.Warnf("[prism spawn] warning: could not compute skills manifest hash: %v\n", skillsHashErr)
		skillsManifestHash = ""
	}

	// C4.AP: compute agent role file hash. The role file is resolved from
	// XDG_CONFIG_HOME/prism/agents/<role>.md. Errors are non-fatal.
	agentRoleFilePath := prismAgentRolePath(agentRole)
	agentPromptHash, agentHashErr := skills.ComputeAgentPromptHash(agentRoleFilePath)
	if agentHashErr != nil {
		proglog.Warnf("[prism spawn] warning: could not compute agent prompt hash: %v\n", agentHashErr)
		agentPromptHash = ""
	}

	if err := session.SpawnSession(d, spawnOpts); err != nil {
		return err
	}

	// Write spawn_inputs row. This is best-effort: a failure is logged but
	// does not roll back the session (the session is already live).
	// We read instance_id back from the DB because the sidecar mints it
	// at startup and we do not want to thread it back through SpawnOpts just
	// for this write path.
	writeSpawnInputs(d, spawnInputsArgs{
		sessionName:        sessionName,
		profileName:        resolvedProfile,
		modelFlag:          modelFlag,
		variantFlag:        variantFlag,
		agentFlag:          agentFlag,
		harnessFlag:        harnessFlag,
		cmd:                cmd,
		prFlag:             prFlag,
		branchFlag:         branchFlag,
		skillsManifestHash: skillsManifestHash,
		agentPromptHash:    agentPromptHash,
		promptText:         promptText,
		promptSource:       promptSource,
		modelsByRole:       modelsByRole,
	})

	waitFlag, _ := cmd.Flags().GetBool("wait")
	jsonFlag, _ := cmd.Flags().GetBool("json")
	waitTimeout, _ := cmd.Flags().GetDuration("wait-timeout")
	if waitFlag {
		// In --wait + --json mode, suppress the "session %q created" line
		// so stdout is JSON-only at the end of the wait.
		if !jsonFlag {
			fmt.Printf("session %q spawned; waiting for terminal state...\n", sessionName)
		}
		return waitForSpawnTerminal(sessionName, jsonFlag, waitTimeout)
	}

	if headless {
		fmt.Printf("session %q created\n", sessionName)
		return nil
	}
	return session.Attach(sessionName)
}

// prismSkillsDir returns the path to the prism skills directory,
// respecting XDG_CONFIG_HOME with a ~/.config fallback.
func prismSkillsDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "prism", "skills")
}

// prismAgentRolePath returns the path to the prism agent role file for
// the given role name, respecting XDG_CONFIG_HOME with a ~/.config fallback.
func prismAgentRolePath(role string) string {
	if role == "" {
		return ""
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "prism", "agents", role+".md")
}

// spawnInputsArgs bundles the flag values needed to build a db.SpawnInputs row.
type spawnInputsArgs struct {
	sessionName        string
	profileName        string
	modelFlag          string
	variantFlag        string
	agentFlag          string
	harnessFlag        string
	cmd                *cobra.Command
	prFlag             string
	branchFlag         string
	skillsManifestHash string
	agentPromptHash    string
	promptText         string
	promptSource       string
	modelsByRole       map[string]string
	// abtestPairID is non-empty for sessions spawned via --abtest.
	abtestPairID string
}

// writeSpawnInputs looks up the instance_id for sessionName and inserts a row
// into spawn_inputs. All errors are non-fatal and logged to stderr.
func writeSpawnInputs(d *db.DB, args spawnInputsArgs) {
	st, err := d.CurrentStatus(args.sessionName)
	if err != nil || st == nil || st.InstanceID == nil || *st.InstanceID == "" {
		proglog.Warnf("[prism spawn] warning: could not read instance_id for spawn_inputs: %v\n", err)
		return
	}

	si := db.SpawnInputs{
		InstanceID: *st.InstanceID,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if args.promptSource != "" {
		si.PromptSource = spawnStrPtr(args.promptSource)
	}

	if args.profileName != "" {
		si.ProfileName = &args.profileName
	}
	if args.modelFlag != "" {
		si.ModelFlag = &args.modelFlag
	}
	if args.variantFlag != "" {
		si.VariantFlag = &args.variantFlag
	}
	if args.agentFlag != "" {
		si.AgentFlag = &args.agentFlag
	}
	if args.harnessFlag != "" {
		si.HarnessFlag = &args.harnessFlag
	}
	if isolationFlag, _ := args.cmd.Flags().GetString("isolation"); isolationFlag != "" {
		si.IsolationFlag = &isolationFlag
	}
	if hostModeFlag, _ := args.cmd.Flags().GetBool("host-mode"); hostModeFlag {
		si.HostModeFlag = true
	}
	if args.prFlag != "" {
		if n, err := strconv.Atoi(args.prFlag); err == nil {
			si.PRNumber = &n
		}
	}
	if args.branchFlag != "" {
		si.BranchFlag = &args.branchFlag
	}
	if ignoreCap, _ := args.cmd.Flags().GetBool("ignore-concurrency-cap"); ignoreCap {
		si.IgnoreConcurrencyCap = true
	}
	if args.skillsManifestHash != "" {
		si.SkillsManifestHash = &args.skillsManifestHash
	}
	if args.agentPromptHash != "" {
		si.AgentPromptHash = &args.agentPromptHash
	}
	if args.promptText != "" {
		si.PromptText = &args.promptText
	}
	if len(args.modelsByRole) > 0 {
		if encoded, encErr := json.Marshal(args.modelsByRole); encErr == nil {
			s := string(encoded)
			si.ModelVariantOverrides = &s
		}
	}
	if args.abtestPairID != "" {
		si.AbtestPairID = &args.abtestPairID
	}

	if err := d.InsertSpawnInputs(si); err != nil {
		proglog.Warnf("[prism spawn] warning: could not write spawn_inputs: %v\n", err)
	}
}

// spawnStrPtr returns a pointer to s, for optional string fields in SpawnInputs.
func spawnStrPtr(s string) *string { return &s }

// resolveBareRoot returns the bare repo root to operate on.
// If repoFlag is set, it is resolved as a shorthand name under ~/code or as a
// full path. If not set, the current pane path is used (existing behaviour).
func resolveBareRoot(repoFlag string) (string, error) {
	if repoFlag != "" {
		return resolveRepo(repoFlag)
	}

	// Fall back to inferring from the current tmux pane path.
	// PRISM_BARE_ROOT is injected by the sidecar into container environments
	// where the bare repo is mounted at a fixed path (/prism-git) that is not
	// a parent of the worktree (/workspace). The parent-walk heuristic below
	// cannot find it, so honour this override directly. Accepts both the prism
	// project root (containing .bare/) and a raw bare git dir (e.g. /prism-git
	// itself in container mode).
	if bareRootEnv := os.Getenv("PRISM_BARE_ROOT"); bareRootEnv != "" {
		if git.IsBareRepo(bareRootEnv) || git.IsRawBareGitDir(bareRootEnv) {
			return bareRootEnv, nil
		}
		return "", fmt.Errorf("PRISM_BARE_ROOT=%q is not a prism bare repo", bareRootEnv)
	}

	panePathRaw := os.Getenv("PRISM_SPAWN_PATH")
	if panePathRaw == "" {
		var err error
		panePathRaw, err = tmux.CurrentPanePath()
		if err != nil || panePathRaw == "" {
			return "", fmt.Errorf("could not determine current pane path")
		}
	}

	bareRoot := git.BareRoot(panePathRaw)
	if bareRoot == "" {
		if git.IsBareRepo(panePathRaw) {
			bareRoot = panePathRaw
		}
	}
	if bareRoot == "" {
		if git.IsInsideRegularRepo(panePathRaw) {
			return "", fmt.Errorf("this repo isn't using the worktree layout yet\nuse C-f to convert it first")
		}
		return "", fmt.Errorf("not inside a git repo")
	}
	return bareRoot, nil
}

// resolveRepo resolves a repo shorthand (e.g. "nixos-config") to a full path
// under ~/code, or accepts an absolute path directly.
func resolveRepo(nameOrPath string) (string, error) {
	// Absolute or home-relative path — use as-is.
	candidate := expandHome(nameOrPath)
	if filepath.IsAbs(candidate) {
		if git.IsBareRepo(candidate) {
			return candidate, nil
		}
		return "", fmt.Errorf("not a prism bare repo: %s", candidate)
	}

	// Shorthand: look under the configured project locations.
	for _, loc := range switchProjectLocations() {
		p := filepath.Join(expandHome(loc), nameOrPath)
		if git.IsBareRepo(p) {
			return p, nil
		}
	}

	return "", fmt.Errorf(
		"repo %q not found under ~/code\nhint: run `prism clone <url>` to add it",
		nameOrPath,
	)
}

// resolveBranch determines the branch name to use for the new worktree.
// Priority: --pr flag > --branch flag > timestamped default.
func resolveBranch(bareRoot, branchFlag, prFlag string) (string, error) {
	if prFlag != "" {
		// Fetch latest remote refs so the PR branch is available.
		fmt.Printf("fetching origin for %s...\n", filepath.Base(bareRoot))
		if err := git.FetchRemote(bareRoot); err != nil {
			return "", fmt.Errorf("fetch: %w", err)
		}
		branch, err := git.PRBranch(bareRoot, prFlag)
		if err != nil {
			return "", fmt.Errorf("resolve PR branch: %w", err)
		}
		return branch, nil
	}

	if branchFlag != "" {
		sanitised := git.SanitiseBranch(branchFlag)
		if sanitised == "" {
			return "", fmt.Errorf("branch name %q is empty after sanitisation", branchFlag)
		}
		return sanitised, nil
	}

	// Default: zettelkasten timestamp.
	return time.Now().Format("20060102T1504"), nil
}

// parseModelOverrides converts a slice of "role=model" strings into a map.
// Returns an error if any entry is malformed (missing "=", empty role, or empty
// model). Returns nil map when raw is empty.
func parseModelOverrides(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(raw))
	for _, entry := range raw {
		role, model, ok := strings.Cut(entry, "=")
		if !ok || role == "" || model == "" {
			return nil, fmt.Errorf("invalid --model-override %q: expected role=model (e.g. review-context=google/gemini-2.5-pro)", entry)
		}
		m[role] = model
	}
	return m, nil
}

// generateAbtestPairID returns a random 16-byte hex string to use as a shared
// abtest_pair_id for an --abtest pair. Panics on entropy failure (should never
// happen; os.Getentropy is always available on Linux/Darwin).
func generateAbtestPairID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("prism: entropy failure generating abtest_pair_id: %v", err))
	}
	return hex.EncodeToString(b)
}

// runAbtestSpawn implements `prism spawn --abtest profileA profileB`.
//
// Pre-flight (no side effects):
//  1. Validate harness.
//  2. Load profiles; verify both profile names exist and each has a slot for
//     the planned session role.
//  3. Resolve bare root + branch. The branch is suffixed with the profile name
//     for each session (e.g. my-branch-profileA, my-branch-profileB). If the
//     user passed --branch, that value is the base; otherwise a timestamp is used.
//  4. Resolve isolation mode + concurrency cap.
//
// Execution:
//
//	Two goroutines each call spawnOneAbtest. Both-or-neither semantics: if
//	either goroutine fails, the other's session (if started) is torn down via
//	session.EndSession before the error is returned to the caller.
func runAbtestSpawn(cmd *cobra.Command, profileA, profileB string) error {
	branchFlag, _ := cmd.Flags().GetString("branch")
	prFlag, _ := cmd.Flags().GetString("pr")
	repoFlag, _ := cmd.Flags().GetString("repo")
	agentFlag, _ := cmd.Flags().GetString("agent")
	modelFlag, _ := cmd.Flags().GetString("model")
	variantFlag, _ := cmd.Flags().GetString("variant")
	harnessFlag, _ := cmd.Flags().GetString("harness")
	modelOverrideRaw, _ := cmd.Flags().GetStringArray("model-override")
	modelsByRole, err := parseModelOverrides(modelOverrideRaw)
	if err != nil {
		return err
	}

	if _, ok := harness.Lookup(harnessFlag); !ok {
		return fmt.Errorf("unknown harness %q: valid harnesses: %s", harnessFlag, strings.Join(harness.Names(), ", "))
	}

	promptText, promptSource, err := resolvePromptWithSource(cmd)
	if err != nil {
		return err
	}
	if overrideSource, _ := cmd.Flags().GetString("prompt-source"); overrideSource != "" {
		promptSource = overrideSource
	}
	// Reject an empty prompt at the operator boundary (issue #1891). See
	// the matching check in runSpawn for the proxy-spawn carve-out.
	if promptText == "" && promptSource != "proxy-spawn" {
		return emptyPromptError(cmd, "prism spawn")
	}

	cfg := config.Load()

	isolationMode, err := resolveIsolationMode(cmd, cfg)
	if err != nil {
		return err
	}
	isoCaps := container.CapabilitiesFor(isolationMode)

	if isoCaps.IsContainer {
		iso, isoErr := container.For(isolationMode, container.ConstructorOpts{})
		if isoErr != nil {
			return isoErr
		}
		if err := iso.Available(); err != nil {
			return err
		}
	}

	if err := checkConcurrencyCap(cmd, "spawn", isolationMode); err != nil {
		return err
	}

	// Load profiles — required for --abtest.
	pf, loadErr := config.LoadProfiles()
	if loadErr != nil {
		return fmt.Errorf("--abtest: could not load profiles.json: %w", loadErr)
	}

	// Resolve bare root and base branch.
	bareRoot, err := resolveBareRoot(repoFlag)
	if err != nil {
		return err
	}

	baseBranch, err := resolveBranch(bareRoot, branchFlag, prFlag)
	if err != nil {
		return err
	}

	// The planned session role (for slot validation and agent assignment).
	plannedRole := agentFlag
	if plannedRole == "" {
		if baseBranch == "main" {
			plannedRole = "coordinator"
		} else {
			plannedRole = "worker"
		}
	}

	// Validate both profiles exist and have the required slot BEFORE any
	// side effects (no worktree, no tmux session, no DB row on failure).
	// Also resolve per-leg harness from each profile's slot (#1328):
	// --harness flag overrides; when absent, the slot's harness is used.
	type abtestLeg struct {
		profileName string
		branch      string
		harnessName string
	}
	legs := make([]abtestLeg, 0, 2)
	for _, profileName := range []string{profileA, profileB} {
		if err := config.RequireSlot(pf, profileName, plannedRole); err != nil {
			return fmt.Errorf("--abtest profile %q: %w", profileName, err)
		}
		// Pi is the sole harness. Use it directly unless --harness was explicitly set.
		legHarness := harnessFlag
		if !cmd.Flags().Changed("harness") {
			legHarness = "pi"
		}
		if _, ok := harness.Lookup(legHarness); !ok {
			return fmt.Errorf("--abtest profile %q role %q declares unknown harness %q: valid harnesses: %s",
				profileName, plannedRole, legHarness, strings.Join(harness.Names(), ", "))
		}
		legs = append(legs, abtestLeg{
			profileName: profileName,
			branch:      git.SanitiseBranch(baseBranch + "-" + profileName),
			harnessName: legHarness,
		})
	}

	pairID := generateAbtestPairID()

	// Shared result type for the goroutines.
	type legResult struct {
		sessionName  string
		worktreePath string // populated even on partial success for cleanup
		err          error
	}

	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("spawn --abtest: open db: %w", dbErr)
	}
	defer d.Close()

	results := make([]legResult, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex // guards results slice writes from goroutines

	skillsDir := prismSkillsDir()
	skillsManifestHash, skillsHashErr := skills.ComputeManifest(skillsDir)
	if skillsHashErr != nil {
		proglog.Warnf("[prism spawn] warning: could not compute skills manifest hash: %v\n", skillsHashErr)
		skillsManifestHash = ""
	}
	agentRoleFilePath := prismAgentRolePath(plannedRole)
	agentPromptHash, agentHashErr := skills.ComputeAgentPromptHash(agentRoleFilePath)
	if agentHashErr != nil {
		proglog.Warnf("[prism spawn] warning: could not compute agent prompt hash: %v\n", agentHashErr)
		agentPromptHash = ""
	}

	for i, leg := range legs {
		i, leg := i, leg // capture
		wg.Add(1)
		go func() {
			defer wg.Done()
			sn, wt, spawnErr := spawnOneAbtest(cmd, spawnOneAbtestArgs{
				profileName:        leg.profileName,
				branch:             leg.branch,
				pairID:             pairID,
				bareRoot:           bareRoot,
				agentFlag:          agentFlag,
				plannedRole:        plannedRole,
				promptText:         promptText,
				promptSource:       promptSource,
				modelFlag:          modelFlag,
				variantFlag:        variantFlag,
				harnessFlag:        leg.harnessName,
				isolationMode:      isolationMode,
				isoCaps:            isoCaps,
				cfg:                cfg,
				pf:                 pf,
				modelsByRole:       modelsByRole,
				skillsManifestHash: skillsManifestHash,
				agentPromptHash:    agentPromptHash,
				branchFlag:         branchFlag,
				prFlag:             prFlag,
				d:                  d,
			})
			mu.Lock()
			results[i] = legResult{sessionName: sn, worktreePath: wt, err: spawnErr}
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Both-or-neither: if either leg failed, tear down any session that did start.
	if results[0].err != nil || results[1].err != nil {
		for _, r := range results {
			if r.err == nil && r.sessionName != "" {
				// Best-effort teardown of the session that did start.
				_ = tmux.KillSession(r.sessionName)
				session.KillSidecar(r.sessionName)
				if killDBErr := d.SetEnded(r.sessionName); killDBErr != nil {
					proglog.Warnf("[prism spawn] warning: cleanup DB for %q failed: %v\n", r.sessionName, killDBErr)
				}
			}
			// Remove the worktree even when sessionName is empty — the
			// worktree may have been created before the session started.
			if r.worktreePath != "" {
				if rmErr := git.RemoveWorktree(bareRoot, r.worktreePath); rmErr != nil {
					proglog.Warnf("[prism spawn] warning: cleanup worktree %q failed: %v\n", r.worktreePath, rmErr)
				}
			}
		}
		// Return the first non-nil error.
		if results[0].err != nil {
			return results[0].err
		}
		return results[1].err
	}

	fmt.Printf("abtest pair %s spawned:\n", pairID[:8])
	for _, r := range results {
		fmt.Printf("  session %q created\n", r.sessionName)
	}
	return nil
}

// spawnOneAbtestArgs bundles the arguments for spawnOneAbtest.
type spawnOneAbtestArgs struct {
	profileName        string
	branch             string
	pairID             string
	bareRoot           string
	agentFlag          string
	plannedRole        string
	promptText         string
	promptSource       string
	modelFlag          string
	variantFlag        string
	harnessFlag        string
	isolationMode      config.IsolationMode
	isoCaps            container.Capabilities
	cfg                config.Config
	pf                 *config.ProfilesFile
	modelsByRole       map[string]string
	skillsManifestHash string
	agentPromptHash    string
	branchFlag         string
	prFlag             string
	d                  *db.DB
}

// spawnOneAbtest spawns a single leg of an --abtest pair. Returns the session
// name, the worktree path (populated even on partial failure for cleanup), and
// any error.
func spawnOneAbtest(cmd *cobra.Command, a spawnOneAbtestArgs) (sessionName, worktreePath string, err error) {
	// Create the worktree.
	worktreePath, err = git.CreateWorktree(a.bareRoot, a.branch)
	if err != nil {
		return "", "", fmt.Errorf("create worktree for branch %q: %w", a.branch, err)
	}

	rootRole := a.plannedRole
	if rootRole == "" {
		rootRole = "coordinator"
	}
	configContent, err := config.BuildConfigContent(a.pf, a.profileName, rootRole, a.modelFlag, a.variantFlag)
	if err != nil {
		return "", worktreePath, err
	}

	sessionName = session.NameFor(worktreePath, a.bareRoot)
	agentRole := session.DefaultAgent(worktreePath, a.agentFlag)

	if a.isoCaps.NeedsConfigBlob && configContent != "" {
		iso, isoErr := container.For(a.isolationMode, container.ConstructorOpts{Name: sessionName})
		if isoErr != nil {
			return "", worktreePath, fmt.Errorf("spawn --abtest: %w", isoErr)
		}
		if err := iso.WriteHarnessConfigBlob(sessionName, configContent); err != nil {
			return "", worktreePath, fmt.Errorf("spawn --abtest: %w", err)
		}
	}

	h, _ := harness.New(a.harnessFlag, "", nil, "", "")
	spawnOpts := session.SpawnOpts{
		SessionName:      sessionName,
		Repo:             deriveRepo(worktreePath),
		Worktree:         worktreePath,
		AgentRole:        agentRole,
		Prompt:           a.promptText,
		PromptSource:     a.promptSource,
		ConfigContent:    configContent,
		Layout:           session.LayoutFull,
		IsolationMode:    string(a.isolationMode),
		PluginHostPath:   a.cfg.SidecarPluginPath,
		ConfigEnvVarName: h.ConfigEnvVar(),
		RuntimeEnvVars:   h.RuntimeEnv(),
		HarnessName:      a.harnessFlag,
		ModelsByRole:     a.modelsByRole,
		ForceFresh:       true,
		Headless:         true,
		// WorktreeReadOnly: mount the worktree read-only for investigate sessions.
		WorktreeReadOnly: agentRole == "investigate",
		// PIExtensionDir for host-mode pi launches (#2065).
		PIExtensionDir:   a.cfg.PIExtensionDir,
		ReadinessTimeout: session.DefaultReadinessTimeout,
	}
	if a.pf != nil && !a.isoCaps.IsContainer {
		spawnOpts.AgentEnvVars = a.pf.AgentEnvVars
	}
	if hShape, hShapeOK := harness.ShapeOf(a.harnessFlag); hShapeOK && hShape == harness.TransportSocketPipe && string(a.isolationMode) == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(sessionName); pipeErr == nil {
			spawnOpts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism spawn --abtest] warning: could not resolve harness pipe path for %q: %v\n", sessionName, pipeErr)
		}
	}

	if err := session.SpawnSession(a.d, spawnOpts); err != nil {
		return "", worktreePath, err
	}

	// Write spawn_inputs including abtest_pair_id.
	writeSpawnInputs(a.d, spawnInputsArgs{
		sessionName:        sessionName,
		profileName:        a.profileName,
		modelFlag:          a.modelFlag,
		variantFlag:        a.variantFlag,
		agentFlag:          a.agentFlag,
		harnessFlag:        a.harnessFlag,
		cmd:                cmd,
		prFlag:             a.prFlag,
		branchFlag:         a.branchFlag,
		skillsManifestHash: a.skillsManifestHash,
		agentPromptHash:    a.agentPromptHash,
		promptText:         a.promptText,
		promptSource:       a.promptSource,
		modelsByRole:       a.modelsByRole,
		abtestPairID:       a.pairID,
	})

	return sessionName, worktreePath, nil
}
