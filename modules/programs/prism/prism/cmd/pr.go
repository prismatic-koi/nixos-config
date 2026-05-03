package cmd

// prism pr <number> — shorthand for `prism spawn --pr <number>`.
//
// Checks out the branch for the given PR number in the current (or named) repo,
// creates a worktree for it, and switches to a new tmux session.
//
// Additional flags mirror prism spawn:
//
//	--repo <name>                   target repo by folder name under ~/code (or full path)
//	--prompt <text>                 pass an initial prompt to opencode on launch
//	--prompt-file <path>            read the initial prompt from a file
//	--agent <name>                  opencode agent to use (default: "coordinator" on main, "worker" otherwise)
//	--attach                        switch the current tmux client to the new session
//	--profile <name>                model profile to use from ~/.config/prism/profiles.json
//	--model <name>                  model identifier override (overrides profile's primary model)
//	--variant <name>                model variant override (overrides all agents' variant)
//	--model-override role=model     per-role model override (repeatable)
//	--harness <name>                agent harness to use (default: from profile slot or "opencode")
//	--isolation <mode>              isolation mode: podman, bwrap, sandbox-exec, or host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
)

var prCmd = &cobra.Command{
	Use:   "pr <number>",
	Short: "Check out a PR branch as a new worktree and switch to it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber := args[0]
		repoFlag, _ := cmd.Flags().GetString("repo")
		agentFlag, _ := cmd.Flags().GetString("agent")
		attachFlag, _ := cmd.Flags().GetBool("attach")
		profileFlag, _ := cmd.Flags().GetString("profile")
		modelFlag, _ := cmd.Flags().GetString("model")
		variantFlag, _ := cmd.Flags().GetString("variant")
		harnessFlag, _ := cmd.Flags().GetString("harness")
		isolationFlag, _ := cmd.Flags().GetString("isolation")
		ignoreConcurrencyCapFlag, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
		modelOverrideFlag, _ := cmd.Flags().GetStringArray("model-override")

		promptFlag, err := resolvePrompt(cmd)
		if err != nil {
			return err
		}

		bareRoot, err := resolveBareRoot(repoFlag)
		if err != nil {
			return err
		}

		// In container mode, proxy the spawn to the host API after resolving
		// the PR number to a branch locally (git is accessible from containers).
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			branch, branchErr := resolveBranch(bareRoot, "", prNumber)
			if branchErr != nil {
				return branchErr
			}
			repo := filepath.Base(bareRoot)
			var resp struct {
				SessionName string `json:"session_name"`
			}
			body := map[string]any{
				"repo":                   repo,
				"branch":                 branch,
				"prompt":                 promptFlag,
				"agent":                  agentFlag,
				"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
				"profile":                profileFlag,
				"model":                  modelFlag,
				"variant":                variantFlag,
				"harness":                harnessFlag,
			}
			if cmd.Flags().Changed("isolation") {
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
			if proxyErr := proxyToHostAPI(apiURL, "/spawn", body, &resp); proxyErr != nil {
				return proxyErr
			}
			fmt.Printf("session %q created\n", resp.SessionName)
			return nil
		}

		cfg := config.Load()
		isoMode, isoErr := container.Resolve(container.ResolveInput{
			IsolationFlag:        isolationFlag,
			IsolationFlagChanged: cmd.Flags().Changed("isolation"),
			ConfigDefault:        cfg.DefaultIsolationMode,
		})
		if isoErr != nil {
			return isoErr
		}

		// Look up the isolation capabilities for this mode. All per-mode branching
		// below reads from isoCaps rather than comparing against raw mode constants.
		isoCaps := container.CapabilitiesFor(isoMode)

		// Concurrency cap checks: BEFORE any container-creation side effects
		// (no worktree, no tmux session, no DB row on refusal).
		// A.3 (#1134): unified cap via iso.Cap(ctx, dbPath).Check(ignoreCap).
		if err := checkConcurrencyCap(cmd, "pr", isoMode); err != nil {
			return err
		}

		branch, err := resolveBranch(bareRoot, "", prNumber)
		if err != nil {
			return err
		}

		fmt.Printf("checking out PR #%s → branch %q\n", prNumber, branch)

		worktreePath, err := git.CreateWorktree(bareRoot, branch)
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}

		// Resolve the active profile (flag → state file → nix default), mirroring
		// the pattern in spawn.go so that --profile and `prism profile use` work.
		var pf *config.ProfilesFile
		{
			var pfErr error
			pf, pfErr = config.LoadProfiles()
			if pfErr != nil {
				if isoCaps.NeedsConfigBlob || profileFlag != "" {
					return pfErr
				}
				fmt.Fprintf(os.Stderr, "[prism pr] warning: could not load profiles.json (agent env vars will not be injected): %v\n", pfErr)
				pf = nil
			}
		}

		resolvedProfile, _, profErr := config.ResolveActiveProfile(pf, profileFlag)
		if profErr != nil {
			return profErr
		}

		// In container/bwrap/sandbox-exec mode, inject the role-specific
		// opencode.json blob as the harness config env var so it takes precedence
		// (level 6) over any project-level opencode.jsonc. This mirrors the pattern
		// in spawn.go.
		//
		// This block fires for podman, bwrap, and sandbox-exec isolation modes.
		// Host-mode sessions skip it because they run opencode directly with the
		// host's real ~/.config/opencode/opencode.json via xdg.configFile.
		var configContent string
		if isoCaps.NeedsConfigBlob && pf != nil {
			effectiveRole := session.DefaultAgent(worktreePath, agentFlag)
			// Non-worktree paths (effectiveRole == "") use the coordinator config blob
			// so that build/plan agents are available, but pass no --agent flag.
			lookupRole := effectiveRole
			if lookupRole == "" {
				lookupRole = "coordinator"
			}
			roleConfig, roleErr := config.ContainerConfigForRole(pf, lookupRole)
			if roleErr != nil {
				return roleErr
			}
			if roleConfig != "" {
				// Role config governs agent identity and permissions. Overlay
				// the runtime active profile (#1207) so `prism profile use
				// <name>` flows through to `prism pr` spawns. ApplyProfileToBlob
				// is a no-op when the resolved profile matches pf.Default.
				profiled, profileErr := config.ApplyProfileToBlob(roleConfig, resolvedProfile, pf)
				if profileErr != nil {
					return profileErr
				}
				// Re-apply any --model/--variant overrides on top so they are
				// not lost.
				patched, patchErr := config.ApplyModelOverrides(profiled, resolvedProfile, modelFlag, variantFlag, pf)
				if patchErr != nil {
					return patchErr
				}
				configContent = patched
			} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
				fmt.Fprintf(os.Stderr, "[prism pr] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", effectiveRole)
			}
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. prism agent-run
		// reconstructs a container.Manager from DB state (which does not carry
		// ConfigContent), so the file must be written here via the deterministic
		// temp path. The bwrap.go mount-emission block checks file existence
		// (os.Stat) rather than cfg.ConfigContent, so it picks this up correctly.
		//
		// Podman mode does NOT need this write — the sidecar's Create() path
		// already writes the file before the container starts. Host mode does
		// NOT need this write — it uses ~/.config/opencode/opencode.json
		// directly via xdg.configFile. sandbox-exec mode does NOT yet use this
		// path — config delivery for sandbox-exec is deferred to #1016 (no
		// bwrap-equivalent mount mechanism exists yet).
		//
		// IMPORTANT: the path key used here must match the one used by Manager
		// internally. Manager.name = container.NameForSession(tmuxSessionName),
		// and Manager.opencodeConfigFilePath() calls OpencodeConfigFilePath(m.name).
		// Isolator.WriteHarnessConfigBlob translates the prism session name to the
		// container name internally so this call site stays mode-agnostic (D3,
		// issue #1133). Mirrors the pattern in spawn.go.
		if isoCaps.NeedsConfigBlob && configContent != "" {
			tmuxSessionName := session.NameFor(worktreePath, bareRoot)
			iso, isoIsoErr := container.For(isoMode, container.ConstructorOpts{Name: tmuxSessionName})
			if isoIsoErr != nil {
				return fmt.Errorf("prism pr: %w", isoIsoErr)
			}
			if err := iso.WriteHarnessConfigBlob(tmuxSessionName, configContent); err != nil {
				return fmt.Errorf("prism pr: %w", err)
			}
		}

		// Resolve the effective harness: --harness flag takes precedence; when
		// absent, derive from the active profile slot (matching spawn.go #1328).
		// Default to "opencode" when no profile/slot is configured.
		effectiveHarness := harnessFlag
		if !cmd.Flags().Changed("harness") && pf != nil && resolvedProfile != "" {
			plannedRole := agentFlag
			if plannedRole == "" {
				if branch == "main" {
					plannedRole = "coordinator"
				} else {
					plannedRole = "worker"
				}
			}
			if slot, ok := config.SlotForRole(pf, resolvedProfile, plannedRole); ok {
				slotHarness := config.HarnessForSlot(slot)
				if slotHarness != "" {
					effectiveHarness = slotHarness
				}
			}
		}
		if effectiveHarness == "" {
			effectiveHarness = "opencode"
		}
		if _, ok := harness.Lookup(effectiveHarness); !ok {
			return fmt.Errorf("unknown harness %q: valid harnesses: %s", effectiveHarness, strings.Join(harness.Names(), ", "))
		}

		modelsByRole, modelOverrideErr := parseModelOverrides(modelOverrideFlag)
		if modelOverrideErr != nil {
			return modelOverrideErr
		}

		prHarness, _ := harness.New(effectiveHarness, "", nil, "", "")
		opts := session.Opts{
			Prompt:           promptFlag,
			Agent:            agentFlag,
			Headless:         !attachFlag,
			IsolationMode:    string(isoMode),
			PluginHostPath:   cfg.SidecarPluginPath,
			ConfigContent:    configContent,
			ConfigEnvVarName: prHarness.ConfigEnvVar(),
			RuntimeEnvVars:   prHarness.RuntimeEnv(),
			ModelsByRole:     modelsByRole,
			// ForceFresh=true: prism pr creates a new worktree; if a session
			// with the same name exists it is a stale zombie and should be
			// killed, matching the same semantics as prism spawn.
			ForceFresh: true,
		}
		return ensureAndSwitch(worktreePath, bareRoot, opts)
	},
}

func init() {
	prCmd.Flags().String("repo", "", "Target repo name under ~/code, or full path")
	addPromptFlags(prCmd)
	prCmd.Flags().String("agent", "", `Opencode agent to use (default: "coordinator" on main, "worker" otherwise)`)
	prCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	prCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	prCmd.Flags().String("profile", "", "Model profile name from ~/.config/prism/profiles.json (e.g. anthropic, gemini-hybrid)")
	prCmd.Flags().String("model", "", "Model identifier override (e.g. anthropic/claude-sonnet-4-6); overrides profile's primary model")
	prCmd.Flags().String("variant", "", "Model variant override for all agents (e.g. high, max, minimal)")
	prCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable, e.g. review-context=google/gemini-2.5-pro)")
	prCmd.Flags().String("harness", "", "Agent harness to use (default: from profile slot, or opencode)")
	prCmd.Flags().String("isolation", "", "Isolation mode: podman, bwrap, sandbox-exec, or host (default: from ~/.config/prism/config.json)")
	rootCmd.AddCommand(prCmd)
}
