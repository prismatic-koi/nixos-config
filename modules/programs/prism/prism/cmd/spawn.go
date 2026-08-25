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
//	--provider <name>            routing provider override (overrides the profile slot's provider; pi harness only)
//	--model-override role=model  per-role model override (repeatable); overrides --model for that role
//	--isolation <mode>           isolation mode: bwrap, sandbox-exec, or host (default: from config.json)
//	--containers                 enable the per-session filtering podman API socket proxy (default: off)
//	--harness <name>             agent harness to use (default: "pi")

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/prismatic-koi/prism/internal/forge"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/gitlab"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
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
	providerFlag, _ := cmd.Flags().GetString("provider")
	harnessFlag, _ := cmd.Flags().GetString("harness")
	ignoreConcurrencyCapFlag, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	isolationFlag, _ := cmd.Flags().GetString("isolation")
	containersFlag, _ := cmd.Flags().GetBool("containers")
	modelOverrideFlag, _ := cmd.Flags().GetStringArray("model-override")
	reuseFlag, _ := cmd.Flags().GetBool("reuse")

	isolationChanged := cmd.Flags().Changed("isolation")
	// Only forward "containers" when explicitly set so an unset child does
	// not accidentally inherit a parent's enabled state. Mirrors
	// isolationChanged — see body["isolation"] below for the same pattern.
	containersChanged := cmd.Flags().Changed("containers")

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
	// --provider carries the same restrictions as on the direct path (issue
	// #2852). Both are validated client-side so the error surfaces at the
	// proxy boundary with the same wording as the host-shell path, matching
	// the --isolation precedent above.
	if err := validateProviderFlag(providerFlag, harnessFlag, len(abtestFlag) > 0); err != nil {
		return err
	}
	// Validate --abtest count early: we only allow 0 or 2 values.
	if len(abtestFlag) == 1 || len(abtestFlag) > 2 {
		return fmt.Errorf("--abtest requires exactly two profile names (got %d)", len(abtestFlag))
	}

	// --pr and --branch are mutually exclusive. Unlike the pre-#2432 code,
	// --pr is NOT resolved to a branch client-side here — the raw PR number
	// is forwarded as "pr" to the host API, and the host-side prism spawn
	// resolves it via its own resolveBranch/PRBranch path (FetchRemote +
	// CreateWorktree tracking origin/<real-head>). Resolving client-side and
	// forwarding only a branch name lost the PR's real head ref the moment
	// it contained a slash: the host-side resolveBranch would then run it
	// through git.SanitiseBranch ("/" → "-"), find no matching local or
	// origin branch, and fork a new branch from the default branch instead
	// of checking out the PR's commits (issue #2432).
	if prFlag != "" && branchFlag != "" {
		return fmt.Errorf("--pr and --branch are mutually exclusive")
	}

	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}
	// Keybind carve-out (issues #2063, #2073). The tmux Prefix+a keybind
	// invokes `prism spawn --attach` with no --prompt because the operator
	// types the initial prompt to the live agent after the popup attaches.
	//
	// History: the original carve-out (#2063) reused PRISM_SPAWN_PATH as the
	// discriminator, but that env var is set UNCONDITIONALLY by every
	// sandbox (bwrap.go:~500, cmd/agent_run_sandbox_exec_darwin.go:~284) as
	// a working-directory hint (see internal/sandboxenv/sandboxenv.go). To
	// stop ordinary container worker-spawn flows from being misclassified
	// as keybind spawns, the proxy check had to be narrowed to
	// `PRISM_SPAWN_PATH != "" && promptFlag == ""`, with an extra layer of
	// narrowing in the sidecar's /spawn handler. That conflation is finally
	// retired by #2073: the keybind sets a dedicated sentinel
	// PRISM_KEYBIND_SPAWN=1 that no sandbox injects, so the discriminator
	// can be a single env-var check on both the host and proxy paths — no
	// `promptFlag == ""` narrowing needed, no risk of leakage from sandbox
	// env-injection. PRISM_SPAWN_PATH keeps its working-directory-hint job
	// and nothing else.
	fromKeybind := os.Getenv("PRISM_KEYBIND_SPAWN") != ""
	// Reject an empty prompt at the operator boundary (layers 1+2 of issue
	// #1891). Without this, an empty --prompt-file, --prompt "", or empty
	// stdin produces a session that is created successfully on every
	// observable surface but never receives a prompt and sits idle forever.
	// The host-API /spawn handler has a defence-in-depth check too (layer 3);
	// this surfaces the error in the caller's stderr instead of an HTTP 400.
	// PR carve-out (issue #2633): an empty prompt is also legitimate when
	// --pr is set — the host-side runSpawn injects read-only guidance into
	// the prompt itself (see the withPRReadOnlyGuidance call site below), so
	// this client-side check must not reject it. The host-API layer-3 check
	// carries the matching carve-out.
	if promptFlag == "" && !fromKeybind && prFlag == "" {
		return emptyPromptError(cmd, "prism spawn")
	}

	harnessChanged := cmd.Flags().Changed("harness")

	// Abtest path: POST with abtest field and parse two session names from response.
	if len(abtestFlag) == 2 {
		var resp struct {
			SessionNames []string `json:"session_names"`
			// Warning carries the sidecar's prism-binary staleness diagnostic
			// (issue #2742), set only when the sidecar that handled this spawn
			// launched from a binary a switch has since replaced. Empty in the
			// common case; the field is simply absent from the JSON then.
			Warning string `json:"warning"`
		}
		body := map[string]any{
			"prompt":                 promptFlag,
			"agent":                  agentFlag,
			"model":                  modelFlag,
			"variant":                variantFlag,
			"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
			"abtest":                 abtestFlag,
		}
		// Unreachable in practice — validateProviderFlag above rejects
		// --provider alongside --abtest — but forwarding keeps the two body
		// builders symmetric if that rule is ever relaxed.
		if providerFlag != "" {
			body["provider"] = providerFlag
		}
		// See the non-abtest body construction below for why --pr is forwarded
		// as "pr" rather than resolved to a branch client-side (issue #2432).
		if prFlag != "" {
			body["pr"] = prFlag
		} else {
			body["branch"] = branchFlag
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
		if containersChanged {
			body["containers"] = containersFlag
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
		if resp.Warning != "" {
			fmt.Fprintln(os.Stderr, resp.Warning)
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
		// Warning carries the sidecar's prism-binary staleness diagnostic
		// (issue #2742), set only when the sidecar that handled this spawn
		// launched from a binary a switch has since replaced. Empty in the
		// common case; the field is simply absent from the JSON then.
		Warning string `json:"warning"`
	}
	body := map[string]any{
		"prompt":                 promptFlag,
		"agent":                  agentFlag,
		"profile":                profileFlag,
		"model":                  modelFlag,
		"variant":                variantFlag,
		"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
		"reuse":                  reuseFlag,
	}
	// Only forward "provider" when the user actually passed a value. An empty
	// string must never reach the host-side spawn as an explicit override —
	// the slot provider has to stay in effect (issue #2852).
	if providerFlag != "" {
		body["provider"] = providerFlag
	}
	// Forward --pr as "pr" (preserving PR identity end-to-end, issue #2432)
	// rather than resolving it to a branch client-side. Otherwise forward the
	// (possibly empty, meaning "host picks a timestamped default") branch.
	if prFlag != "" {
		body["pr"] = prFlag
	} else {
		body["branch"] = branchFlag
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
	// Only forward "containers" when explicitly set. Mirroring the
	// isolationChanged pattern above: an unset child does not inherit a
	// parent's enabled state by accident (#2323 cross-spawn forwarding AC).
	if containersChanged {
		body["containers"] = containersFlag
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
			if resp.Warning != "" {
				fmt.Fprintln(os.Stderr, resp.Warning)
			}
		}
		return waitForSpawnTerminal(resp.SessionName, jsonFlag, waitTimeout)
	}
	fmt.Printf("session %q created\n", resp.SessionName)
	if resp.Warning != "" {
		fmt.Fprintln(os.Stderr, resp.Warning)
	}
	return nil
}

// validateProviderFlag enforces the two restrictions on `prism spawn
// --provider <name>` (issue #2852). Both the direct (host-shell) path and the
// proxy path call it before any session state is created, so a rejected
// combination leaves no worktree, no tmux session, and no DB row behind.
//
//  1. A non-pi --harness is rejected, and the error names both flags. This is
//     a deliberate deviation from --model / --variant, which are silently
//     scoped to pi: provider decides routing and billing, so silently
//     ignoring it would produce a confidently wrong user.
//  2. --abtest is rejected, matching the existing --profile rule. Each abtest
//     arm draws its provider from its own profile slot by design.
//
// An empty providerFlag always passes: the flag was not given, so the profile
// slot's provider stays in effect.
func validateProviderFlag(providerFlag, harnessFlag string, abtest bool) error {
	if providerFlag == "" {
		return nil
	}
	if abtest {
		return fmt.Errorf("--abtest and --provider are mutually exclusive")
	}
	if !container.IsPIHarness(harnessFlag) {
		return fmt.Errorf(
			"--provider %q requires the pi harness, but --harness %q was given: "+
				"drop --provider or use --harness pi",
			providerFlag, harnessFlag)
	}
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
	spawnCmd.Flags().String("provider", "", "Routing provider override for all agents (e.g. anthropic, openrouter); overrides the profile slot's provider. Pi harness only; mutually exclusive with --abtest. When --model also carries a provider prefix, make the two agree — pi only strips a matching prefix.")
	spawnCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable, e.g. review-context=google/gemini-2.5-pro)")
	spawnCmd.Flags().String("isolation", "", "Isolation mode: bwrap, sandbox-exec, or host (default: from ~/.config/prism/config.json)")
	spawnCmd.Flags().Bool("containers", false, "Enable the per-session filtering podman API socket proxy (containers feature, #2317). Default: off. Combine with bwrap or sandbox-exec isolation; host mode bypasses the proxy.")
	spawnCmd.Flags().String("harness", "pi", "Agent harness to use; valid values are determined by registered harnesses")
	spawnCmd.Flags().Bool("ignore-concurrency-cap", false, config.IgnoreConcurrencyCapHelp)
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

// warnContainersWithHostMode emits an informational warning to w when the
// caller passed --containers AND the resolved isolation mode is "host".
//
// The combo is intentionally not an error: users may legitimately combine the
// two to audit what containers a host-mode agent spawned via the
// spawn_inputs.containers_flag audit trail. Host-mode agents already have
// direct podman access via the host's CONTAINER_HOST / DOCKER_HOST, so the
// filtering socket proxy adds no value at runtime — the warning makes that
// trade-off explicit (#2317 / #2323).
//
// The check fires on the *resolved* isolation mode rather than just
// `--isolation host`, so the message is consistent whether the user named
// host explicitly or arrived at it via the config.json default.
//
// Extracted to a helper so the warning text is testable without going
// through the full runSpawn / runAbtestSpawn path. Returns true when the
// warning was emitted, for tests that want to assert presence directly.
func warnContainersWithHostMode(w io.Writer, containers bool, isolation string) bool {
	if !containers || isolation != "host" {
		return false
	}
	fmt.Fprintln(w, "prism spawn: --containers with --isolation host: host mode bypasses the proxy (host-mode agents already have direct podman access); --containers is recorded in spawn_inputs.containers_flag for audit, but agent_status.containers_enabled is still set")
	return true
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
// container availability checks live in the runSpawn caller (gated by
// isoCaps.IsContainer, always false today) so resolveIsolationMode keeps its
// pre-refactor surface.
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
	// Skip Available() for container modes (none in current code) — the
	// container availability check runs later in runSpawn so it
	// happens after the worktree-irrelevant pre-flight is complete.
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

func runSpawn(cmd *cobra.Command, args []string) (retErr error) {
	// Note: rootCmd sets SilenceUsage + SilenceErrors globally; RunE errors
	// no longer dump the usage block or double-print (issue #2362).
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
	// --provider validation (issue #2852). Both checks run here, before any
	// side effect, so a rejected combination creates no worktree, no tmux
	// session, and no DB row. The harness check deliberately precedes the
	// harness-registry lookup further down so the error names BOTH flags
	// rather than reporting an unknown harness.
	providerFlag, _ := cmd.Flags().GetString("provider")
	providerHarnessFlag, _ := cmd.Flags().GetString("harness")
	if err := validateProviderFlag(providerFlag, providerHarnessFlag, len(abtestProfiles) > 0); err != nil {
		return err
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
	// providerFlag was already read above for the --provider validation.
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
	// `prism spawn --pr <number>` always targets a pre-existing PR (Case 1 in
	// agents/coordinator.md), so it never authored the PR. This is the single
	// host-side injection point for the read-only guidance (issue #2633): the
	// host-API /spawn handler always shells out to `prism spawn --pr <n>` for
	// a sandboxed caller, whether the caller ran `prism pr <number>` or
	// `prism spawn --pr <number>` — both funnel through here. A direct
	// (non-proxied) `prism spawn --pr <number>` on the host hits this same
	// line. withPRReadOnlyGuidance is idempotent, so a prompt that already
	// carries the guidance (forwarded from cmd/pr.go's own client-side
	// injection) is not wrapped twice.
	if prFlag != "" {
		promptText = withPRReadOnlyGuidance(promptText)
	}
	attachFlag, _ := cmd.Flags().GetBool("attach")
	// headless when invoked from a shell/agent rather than the tmux keybinding.
	// The keybinding sets PRISM_KEYBIND_SPAWN=1; --attach overrides to force a switch.
	fromKeybind := os.Getenv("PRISM_KEYBIND_SPAWN") != ""
	// Reject an empty prompt at the operator boundary (issue #1891). When the
	// hidden --prompt-source flag is set we are running as the child of the
	// host-API /spawn handler, which has already validated that req.Prompt is
	// non-empty (layer 3); skipping the check there keeps the proxy's own
	// 400 surface as the source of truth for that path.
	//
	// Keybind carve-out (issues #2012, #2073): the tmux Prefix+a keybind
	// invokes `prism spawn --attach` with no --prompt because the operator
	// types the initial prompt to the live agent after the popup attaches.
	// The keybind sets PRISM_KEYBIND_SPAWN=1 (the `fromKeybind`
	// discriminator — a dedicated sentinel that no sandbox injects), so an
	// empty prompt on that path is legitimate and must be allowed through.
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

	// --containers + host isolation: informational warning (#2317 / #2323).
	// See warnContainersWithHostMode for the rationale.
	containersFlag, _ := cmd.Flags().GetBool("containers")
	warnContainersWithHostMode(os.Stderr, containersFlag, string(isolationMode))

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isolationMode)

	// Container availability check: when container mode is active (never, in
	// current code) verify the runtime is available before touching anything.
	// resolveIsolationMode
	// has already validated platform availability for bwrap / sandbox-exec
	// (D1: iso.Available()); this branch remains
	// because it fires only when isoCaps.IsContainer.
	if isoCaps.IsContainer {
		iso, isoErr := container.For(isolationMode, container.ConstructorOpts{})
		if isoErr != nil {
			return isoErr
		}
		if err := iso.Available(); err != nil {
			return err
		}
	}

	// Concurrency cap checks: BEFORE any session-creation side effects
	// (no worktree, no tmux session, no DB row on refusal).
	// The bwrap cap guards against process-count exhaustion from uncapped
	// bwrap sessions (each is a host process with no per-session memory ceil).
	// The sandbox-exec cap mirrors the bwrap cap for Darwin sessions.
	//
	// A.3 (#1134): unified cap via iso.Cap(ctx, dbPath).Check(ignoreCap).
	if err := checkConcurrencyCap(cmd, "spawn", isolationMode); err != nil {
		return err
	}

	// Load the profiles file. It carries the per-role slots (model, provider,
	// thinking) and agent_env_vars for host-mode injection. Always attempt to
	// load; treat a missing file as fatal only when a sandboxed mode or
	// --profile is active (those paths strictly require the file). For
	// host-mode sessions without a profile flag, a missing file is non-fatal —
	// agent env vars simply won't be injected.
	var pf *config.ProfilesFile
	{
		var loadErr error
		pf, loadErr = config.LoadProfiles()
		if loadErr != nil {
			if isoCaps.RequiresProfilesFile || profileFlag != "" {
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
	//
	// mainCoordinatorReuse (#2352): `prism spawn --branch main` defaults to
	// reuse semantics. The bare+worktree layout means the main worktree
	// already exists at `<bareRoot>/main/`, and there is at most one
	// coordinator per repo (invariant relied on by `prism escalate`
	// discovery and the merge queue), so treating a repeat spawn as
	// "ensure coordinator, then tell it" is unambiguous. The special-case
	// uses the same literal `branch == "main"` check spawn already uses for
	// coordinator role inference. Explicit --reuse alongside --branch main
	// is a harmless no-op.
	reuseFlag, _ := cmd.Flags().GetBool("reuse")
	mainCoordinatorReuse := branch == "main"
	reuseEffective := reuseFlag || mainCoordinatorReuse
	{
		d, dbErr := openDB()
		if dbErr == nil {
			defer d.Close()
			repoName := strings.TrimSuffix(filepath.Base(bareRoot), ".git")
			// Dedupe against the full worktree path — the value production
			// writers store in `agent_status.worktree` (SpawnOpts.Worktree =
			// worktreePath at cmd/spawn.go's SpawnSession call site, and
			// `event tmux-session-start --worktree <dir>` from every pane).
			// Pre-#2352 this call passed the branch name and coincidentally
			// tested-passed on rows seeded with worktree="main", but never
			// matched a real DB row — so the reuse dedupe silently didn't fire
			// and the caller fell through to a duplicate-tmux-session failure.
			existing, lookupErr := d.ActiveStatusForRepoWorktree(repoName, filepath.Join(bareRoot, branch))
			// ActiveStatusForRepoWorktree filters by ended_at IS NULL, so a
			// session whose row was just `prism cleanup`’d (ended_at
			// stamped) is invisible here — a re-spawn on the same branch
			// proceeds and the state-machine table allows the row to be
			// re-seeded from any non-deleted terminal state (error /
			// finished / interrupted) back to idle. That is why both
			// recovery messages below truthfully point at `prism cleanup`
			// (issue #2094 — prerequisite for any future tightening of
			// checkTransition raised by #2081).
			if lookupErr == nil && existing != nil {
				// There is an active session for this branch.
				// Healthy = state not "error" and not "deleted".
				broken := existing.State == "error" || existing.State == "deleted"
				if reuseEffective {
					if broken {
						return fmt.Errorf(
							"prism spawn --reuse: existing session %q is in a broken state (%s)\n"+
								"run: prism cleanup --yes --session %s",
							existing.SessionName, existing.State, existing.SessionName)
					}
					// --branch main reuse with --prompt: deliver the prompt to
					// the running coordinator so `prism spawn --repo <x>
					// --branch main --prompt '...'` is a one-shot "ensure
					// coordinator, then tell it" (#2352 AC4). The waiting-state
					// guard mirrors `prism prompt` (#2352 AC5): a paused
					// coordinator is expecting direct human input, so a
					// programmatic prompt would corrupt the input field.
					//
					// The guard is scoped to mainCoordinatorReuse so the
					// legacy `--reuse` (feature-branch) path stays a pure
					// details-print no-op, unchanged.
					if mainCoordinatorReuse && promptText != "" {
						if existing.State == "waiting" {
							return waitingStateError(existing.SessionName)
						}
						if deliverErr := promptdelivery.DeliverToSession(existing.SessionName, existing, promptText, buildPromptBody, "", "steer"); deliverErr != nil {
							return fmt.Errorf("deliver prompt to %s: %w", existing.SessionName, deliverErr)
						}
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

		// Pi is the sole harness. Use it directly unless --harness was
		// explicitly set by the caller.
		if !cmd.Flags().Changed("harness") {
			harnessFlag = "pi"
		}
	}

	// Create the worktree (handles local, remote-tracking, and new branches).
	//
	// mainCoordinatorReuse path (#2352): `prism spawn --branch main` on a
	// repo whose main worktree already exists at `<bareRoot>/main/` (the
	// bare+worktree default) skips `git.CreateWorktree` — which would
	// otherwise fail with a git "already checked out" error — and starts
	// the coordinator on the existing worktree. When no main worktree is
	// registered (e.g. repo default branch is not "main" or the repo has
	// not been converted), we fall through to CreateWorktree so pre-#2352
	// behaviour is preserved.
	var worktreePath string
	// createdWorktree is non-nil only when THIS spawn created the worktree
	// — never on the mainCoordinatorReuse fast path (#2352), which reuses
	// the existing main worktree. It arms the deferred caller-level
	// rollback below and is disarmed once SpawnSession succeeds.
	var createdWorktree *git.CreatedWorktree
	if mainCoordinatorReuse {
		if wt, ok := existingWorktreeForBranch(bareRoot, branch); ok {
			worktreePath = wt
		}
	}
	if worktreePath == "" {
		created, wtErr := git.CreateWorktree(bareRoot, branch)
		if wtErr != nil {
			return fmt.Errorf("create worktree: %w", wtErr)
		}
		worktreePath = created.Path
		createdWorktree = &created
	}
	// Caller-level rollback (#2363): a failure in any step between worktree
	// creation and SpawnSession success removes the freshly created worktree
	// (and deletes the branch only when it was freshly forked by this spawn
	// and still has no commits beyond its fork point). Rollback failures are
	// logged, never returned — the original error must not be masked.
	defer func() {
		if retErr != nil {
			rollbackCreatedWorktree(bareRoot, createdWorktree, "prism spawn")
		}
	}()

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

	// C4.SK / C4.AP: compute the skills manifest hash and the agent role file
	// hash before building SpawnOpts so they land on the spawn_inputs row
	// written by SpawnSession (issue #2087 centralisation). Errors are
	// non-fatal: a missing or unreadable input produces an empty hash (which
	// SpawnSession then writes as NULL).
	skillsDir := prismSkillsDir()
	skillsManifestHash, skillsHashErr := skills.ComputeManifest(skillsDir)
	if skillsHashErr != nil {
		proglog.Warnf("[prism spawn] warning: could not compute skills manifest hash: %v\n", skillsHashErr)
		skillsManifestHash = ""
	}
	agentRoleFilePath := prismAgentRolePath(agentRole)
	agentPromptHash, agentHashErr := skills.ComputeAgentPromptHash(agentRoleFilePath)
	if agentHashErr != nil {
		proglog.Warnf("[prism spawn] warning: could not compute agent prompt hash: %v\n", agentHashErr)
		agentPromptHash = ""
	}

	// spawn_inputs audit fields (issue #2087): collected here, written by
	// SpawnSession via SpawnOpts. Raw user-passed flag values — isolationMode
	// above is the *resolved* value; this is the raw --isolation flag.
	isolationFlagRaw, _ := cmd.Flags().GetString("isolation")
	prNumber := 0
	if prFlag != "" {
		if n, convErr := strconv.Atoi(prFlag); convErr == nil {
			prNumber = n
		}
	}

	spawnOpts := session.SpawnOpts{
		SessionName:  sessionName,
		Repo:         repoFromWorktreePath(worktreePath),
		Worktree:     worktreePath,
		AgentRole:    agentRole,
		Prompt:       promptText,
		PromptSource: promptSource,
		// InvokerSession populates the from_session field of the durable
		// session.spawn_intent / session.spawn_failed events written by
		// SpawnSession (#2364). Sourced from PRISM_SESSION_NAME so both
		// tmux-attached spawns (coordinator running `prism spawn`) and
		// host-API-shelled spawns (sidecar sets PRISM_SESSION_NAME to the
		// invoker) carry it. Bare CLI spawns run outside a session and
		// leave this empty — SpawnSession then writes the durable rows
		// without an invoker field and skips the bus_messages notification.
		InvokerSession:   os.Getenv("PRISM_SESSION_NAME"),
		Layout:           session.LayoutFull,
		IsolationMode:    string(isolationMode),
		PluginHostPath:   cfg.SidecarPluginPath,
		RuntimeEnvVars:   h.RuntimeEnv(),
		HarnessName:      harnessFlag,
		ModelsByRole:     modelsByRole,
		AllowEmptyPrompt: allowEmptyPrompt,
		// CLI overrides (issue #2086) flow through to the tmux pane command
		// so `prism agent-run` (bwrap / sandbox-exec) and direct pi (host)
		// receive --model / --variant on the final argv.
		Model:   modelFlag,
		Variant: variantFlag,
		// Provider override (issue #2852). Validated above as pi-only, so
		// every downstream emit site sees a pi harness when it is non-empty.
		Provider: providerFlag,
		// ── spawn_inputs audit fields (#2087) ─────────────────────────
		ProfileName:          resolvedProfile,
		ModelFlag:            modelFlag,
		VariantFlag:          variantFlag,
		ProviderFlag:         providerFlag,
		AgentFlag:            agentFlag,
		HarnessFlag:          harnessFlag,
		IsolationFlag:        isolationFlagRaw,
		PRNumber:             prNumber,
		BranchFlag:           branchFlag,
		IgnoreConcurrencyCap: ignoreConcurrencyCapFlagFromCmd(cmd),
		SkillsManifestHash:   skillsManifestHash,
		AgentPromptHash:      agentPromptHash,
		ContainersFlag:       containersFlag,
		// ────────────────────────────────────────────────────────
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
	// AgentEnvVars only applies to host-mode sessions; sandboxed sessions
	// receive env vars via their own injection paths. The map is filtered by
	// role before the SpawnOpts are constructed (issue #2533) — the same
	// filter the sandboxed dispatch paths apply in cmd/agent_run.go.
	if pf != nil && !isoCaps.IsContainer {
		spawnOpts.AgentEnvVars = config.FilterAgentEnvVarsForRole(agentRole, pf.AgentEnvVars)
	}

	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("spawn: open db: %w", dbErr)
	}
	defer d.Close()

	// SpawnSession is the canonical spawn_inputs writer (#2087): it builds
	// the row from SpawnOpts fields populated above and inserts it after the
	// sessions row exists. No post-spawn writer is needed here.
	if err := session.SpawnSession(d, spawnOpts); err != nil {
		return err
	}
	// The session is live: disarm the worktree rollback. Later failures
	// (--wait terminal-state polling, tmux attach) happen against a
	// successfully spawned session and must not tear down its worktree.
	createdWorktree = nil

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

// existingWorktreeForBranch returns the path of a worktree that is already
// registered for branch under bareRoot, or ("", false) when no such worktree
// exists. Used by the `prism spawn --branch main` reuse path (#2352) so a
// headless coordinator can start on the existing main worktree instead of
// failing at `git.CreateWorktree`.
//
// The match is on the exact path `<bareRoot>/<branch>/` — the layout every
// prism bare+worktree repo uses. A worktree registered elsewhere (e.g. via
// `git worktree add /tmp/other main`) is intentionally NOT matched because
// the session naming derives the branch component from the worktree's own
// git symbolic-ref via `session.NameFor`, and we want the resulting session
// name to be `<repo>@main` — the coordinator name the escalate discovery
// and merge queue expect.
//
// The comparison uses filepath.EvalSymlinks on both sides so that a bareRoot
// under a symlinked prefix (e.g. `/tmp/...` → `/private/tmp/...` on macOS)
// still matches the paths git records in `worktree list --porcelain`.
// EvalSymlinks failure (e.g. a missing path) falls back to the raw string
// so the function remains safe when the on-disk state is partially set up.
func existingWorktreeForBranch(bareRoot, branch string) (string, bool) {
	expected := filepath.Join(bareRoot, branch)
	expectedResolved := resolveSymlinksOrRaw(expected)
	for _, w := range git.Worktrees(bareRoot) {
		if w == expected || resolveSymlinksOrRaw(w) == expectedResolved {
			return expected, true
		}
	}
	return "", false
}

// resolveSymlinksOrRaw returns filepath.EvalSymlinks(p) or p on error.
func resolveSymlinksOrRaw(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
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

// ignoreConcurrencyCapFlagFromCmd reads --ignore-concurrency-cap if present.
// Safe to call on commands that do not register the flag (returns false).
// Used to populate SpawnOpts.IgnoreConcurrencyCap for the spawn_inputs audit
// row written by SpawnSession (issue #2087).
func ignoreConcurrencyCapFlagFromCmd(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	v, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	return v
}

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
		// On a gitlab.com remote, resolve and fetch the MR source branch via
		// the GitLab head ref instead of the GitHub gh path. Detection is by
		// origin remote URL; any non-gitlab.com remote keeps the unchanged
		// GitHub flow below.
		if remoteURL, _ := git.OriginRemoteURL(bareRoot); forge.IsGitLab(remoteURL) {
			fmt.Printf("fetching gitlab.com MR !%s source branch for %s...\n", prFlag, filepath.Base(bareRoot))
			branch, err := git.FetchGitLabMRBranch(bareRoot, prFlag, func(iid string) (string, error) {
				mr, mrErr := gitlab.ViewMR(remoteURL, iid)
				if mrErr != nil {
					return "", mrErr
				}
				return mr.SourceBranch, nil
			})
			if err != nil {
				return "", fmt.Errorf("resolve MR branch: %w", err)
			}
			return branch, nil
		}

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
	containersFlag, _ := cmd.Flags().GetBool("containers")
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

	// --containers + host isolation: informational warning, mirroring runSpawn
	// (#2317 / #2323). Both legs of an abtest pair share the same resolved
	// isolation mode, so a single warning covers both. The flag is recorded in
	// each leg's spawn_inputs.containers_flag for audit symmetry.
	warnContainersWithHostMode(os.Stderr, containersFlag, string(isolationMode))

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
				containersFlag:     containersFlag,
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
	// containersFlag mirrors `prism spawn --containers` and is threaded to
	// each abtest leg so both sibling sessions opt into the proxy
	// symmetrically (#2317 / #2323).
	containersFlag bool
	d              *db.DB
}

// spawnOneAbtest spawns a single leg of an --abtest pair. Returns the session
// name, the worktree path (populated even on partial failure for cleanup), and
// any error.
func spawnOneAbtest(cmd *cobra.Command, a spawnOneAbtestArgs) (sessionName, worktreePath string, err error) {
	// Create the worktree.
	created, err := git.CreateWorktree(a.bareRoot, a.branch)
	if err != nil {
		return "", "", fmt.Errorf("create worktree for branch %q: %w", a.branch, err)
	}
	worktreePath = created.Path

	// No profile validation here: runAbtestSpawn already ran
	// config.RequireSlot(pf, profileName, plannedRole) for BOTH legs before
	// any side effect, and plannedRole is assigned unconditionally upstream.
	// A second check on this path could never fire (#2854 review round 1).

	sessionName = session.NameFor(worktreePath, a.bareRoot)
	agentRole := session.DefaultAgent(worktreePath, a.agentFlag)

	h, _ := harness.New(a.harnessFlag, "", nil, "", "")
	isolationFlagRaw, _ := cmd.Flags().GetString("isolation")
	prNumber := 0
	if a.prFlag != "" {
		if n, convErr := strconv.Atoi(a.prFlag); convErr == nil {
			prNumber = n
		}
	}
	spawnOpts := session.SpawnOpts{
		SessionName:  sessionName,
		Repo:         repoFromWorktreePath(worktreePath),
		Worktree:     worktreePath,
		AgentRole:    agentRole,
		Prompt:       a.promptText,
		PromptSource: a.promptSource,
		// InvokerSession for the durable spawn_intent / spawn_failed events
		// SpawnSession writes at the chokepoint (#2364). See the main
		// spawn path above for the full rationale.
		InvokerSession: os.Getenv("PRISM_SESSION_NAME"),
		Layout:         session.LayoutFull,
		IsolationMode:  string(a.isolationMode),
		PluginHostPath: a.cfg.SidecarPluginPath,
		RuntimeEnvVars: h.RuntimeEnv(),
		HarnessName:    a.harnessFlag,
		ModelsByRole:   a.modelsByRole,
		// CLI overrides flow through to the tmux pane command (issue #2086).
		Model:      a.modelFlag,
		Variant:    a.variantFlag,
		ForceFresh: true,
		Headless:   true,
		// WorktreeReadOnly: mount the worktree read-only for investigate sessions.
		WorktreeReadOnly: agentRole == "investigate",
		// PIExtensionDir for host-mode pi launches (#2065).
		PIExtensionDir:   a.cfg.PIExtensionDir,
		ReadinessTimeout: session.DefaultReadinessTimeout,
		// ── spawn_inputs audit fields (#2087) ─────────────────────────
		ProfileName:          a.profileName,
		ModelFlag:            a.modelFlag,
		VariantFlag:          a.variantFlag,
		AgentFlag:            a.agentFlag,
		HarnessFlag:          a.harnessFlag,
		IsolationFlag:        isolationFlagRaw,
		PRNumber:             prNumber,
		BranchFlag:           a.branchFlag,
		IgnoreConcurrencyCap: ignoreConcurrencyCapFlagFromCmd(cmd),
		SkillsManifestHash:   a.skillsManifestHash,
		AgentPromptHash:      a.agentPromptHash,
		AbtestPairID:         a.pairID,
		ContainersFlag:       a.containersFlag,
		// ────────────────────────────────────────────────────────
	}
	// Role-filtered before the SpawnOpts are constructed (issue #2533).
	if a.pf != nil && !a.isoCaps.IsContainer {
		spawnOpts.AgentEnvVars = config.FilterAgentEnvVarsForRole(agentRole, a.pf.AgentEnvVars)
	}
	if hShape, hShapeOK := harness.ShapeOf(a.harnessFlag); hShapeOK && hShape == harness.TransportSocketPipe && string(a.isolationMode) == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(sessionName); pipeErr == nil {
			spawnOpts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism spawn --abtest] warning: could not resolve harness pipe path for %q: %v\n", sessionName, pipeErr)
		}
	}

	// SpawnSession is the canonical spawn_inputs writer (#2087) and includes
	// the abtest_pair_id from SpawnOpts.AbtestPairID. Both legs of the abtest
	// pair share the same a.pairID minted by the caller.
	if err := session.SpawnSession(a.d, spawnOpts); err != nil {
		return "", worktreePath, err
	}

	return sessionName, worktreePath, nil
}
