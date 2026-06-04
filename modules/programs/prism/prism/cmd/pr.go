package cmd

// prism pr <number> — shorthand for `prism spawn --pr <number>`.
//
// Checks out the branch for the given PR number in the current (or named) repo,
// creates a worktree for it, and switches to a new tmux session.
//
// Additional flags mirror prism spawn:
//
//	--repo <name>                   target repo by folder name under ~/code (or full path)
//	--prompt <text>                 pass an initial prompt to the agent on launch
//	--prompt-file <path>            read the initial prompt from a file
//	--agent <name>                  agent to use (default: "coordinator" on main, "worker" otherwise)
//	--attach                        switch the current tmux client to the new session
//	--profile <name>                model profile to use from ~/.config/prism/profiles.json
//	--model <name>                  model identifier override (overrides profile's primary model)
//	--variant <name>                model variant override (overrides all agents' variant)
//	--model-override role=model     per-role model override (repeatable)
//	--harness <name>                agent harness to use (default: from profile slot, or "pi")
//	--isolation <mode>              isolation mode: podman, bwrap, sandbox-exec, or host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/skills"
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
				proglog.Warnf("[prism pr] warning: could not load profiles.json (agent env vars will not be injected): %v\n", pfErr)
				pf = nil
			}
		}

		resolvedProfile, _, profErr := config.ResolveActiveProfile(pf, profileFlag)
		if profErr != nil {
			return profErr
		}

		// Generate the pi harness config for this session's root role.
		effectiveRole := session.DefaultAgent(worktreePath, agentFlag)
		lookupRole := effectiveRole
		if lookupRole == "" {
			lookupRole = "coordinator"
		}
		configContent, err := config.BuildConfigContent(pf, resolvedProfile, lookupRole, modelFlag, variantFlag)
		if err != nil {
			return err
		}

		// For bwrap sessions, write the harness config file to disk now
		// so it is present before the agent pane opens. prism agent-run
		// reconstructs a container.Manager from DB state (which does not carry
		// ConfigContent), so the file must be written here via the deterministic
		// temp path. The bwrap.go mount-emission block checks file existence
		// (os.Stat) rather than cfg.ConfigContent, so it picks this up correctly.
		//
		// Podman mode does NOT need this write — the sidecar's Create() path
		// already writes the file before the container starts. Host mode does
		// NOT need this write — it uses the host harness config
		// directly via xdg.configFile. sandbox-exec mode does NOT yet use this
		// path — config delivery for sandbox-exec is deferred to #1016 (no
		// bwrap-equivalent mount mechanism exists yet).
		//
		// IMPORTANT: the path key used here must match the one used by Manager
		// internally. Manager.name = container.NameForSession(tmuxSessionName),
		// and Manager.harnessConfigFilePath() calls HarnessConfigFilePath(m.name).
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

		// Pi is the sole harness. Use it directly unless --harness was explicitly set.
		effectiveHarness := harnessFlag
		if !cmd.Flags().Changed("harness") {
			effectiveHarness = "pi"
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
			// PIExtensionDir for host-mode pi launches (#2065).
			PIExtensionDir: cfg.PIExtensionDir,
			// CLI overrides (issue #2086) flow through to the tmux pane
			// command so `prism agent-run` (bwrap / sandbox-exec) and direct
			// pi (host) receive --model / --variant on the final argv.
			// `prism pr` is a sibling front door to `prism spawn` and
			// already accepts these flags (see the flag registration below),
			// so the same wire-through is required here.
			Model:   modelFlag,
			Variant: variantFlag,
			// ForceFresh=true: prism pr creates a new worktree; if a session
			// with the same name exists it is a stale zombie and should be
			// killed, matching the same semantics as prism spawn.
			ForceFresh: true,
		}
		if err := ensureAndSwitch(worktreePath, bareRoot, opts); err != nil {
			return err
		}

		// Write spawn_inputs (#2087). prism pr does not go through
		// SpawnSession — it uses ensureAndSwitch → session.Create — so the
		// audit-row writer fires here, mirroring the centralised writer in
		// SpawnSession with the same column mapping.
		writeSpawnInputsForPR(cmd, prSpawnInputsArgs{
			worktreePath:         worktreePath,
			bareRoot:             bareRoot,
			agentRole:            session.DefaultAgent(worktreePath, agentFlag),
			profileName:          resolvedProfile,
			modelFlag:            modelFlag,
			variantFlag:          variantFlag,
			agentFlag:            agentFlag,
			harnessFlag:          effectiveHarness,
			isolationFlag:        isolationFlag,
			isolationMode:        string(isoMode),
			prNumber:             prNumber,
			ignoreConcurrencyCap: ignoreConcurrencyCapFlag,
			modelsByRole:         modelsByRole,
			promptText:           promptFlag,
			promptSource:         "cli-positional",
		})
		return nil
	},
}

// prSpawnInputsArgs bundles the fields needed to write spawn_inputs from the
// `prism pr` path. Kept separate from the centralised SpawnSession write
// because prism pr does not flow through SpawnSession (it uses
// ensureAndSwitch → session.Create), and we need to mirror SpawnSession's
// column mapping without duplicating the SpawnOpts struct.
type prSpawnInputsArgs struct {
	worktreePath         string
	bareRoot             string
	agentRole            string
	profileName          string
	modelFlag            string
	variantFlag          string
	agentFlag            string
	harnessFlag          string
	isolationFlag        string
	isolationMode        string
	prNumber             string
	ignoreConcurrencyCap bool
	modelsByRole         map[string]string
	promptText           string
	promptSource         string
}

// writeSpawnInputsForPR writes the spawn_inputs row for a `prism pr` spawn.
// All errors are non-fatal and logged — the session is already live and the
// row is best-effort telemetry.
func writeSpawnInputsForPR(cmd *cobra.Command, a prSpawnInputsArgs) {
	d, dbErr := openDB()
	if dbErr != nil {
		proglog.Warnf("[prism pr] warning: could not open DB for spawn_inputs: %v\n", dbErr)
		return
	}
	defer d.Close()

	sessionName := session.NameFor(a.worktreePath, a.bareRoot)
	st, lookupErr := d.CurrentStatus(sessionName)
	if lookupErr != nil || st == nil || st.InstanceID == nil || *st.InstanceID == "" {
		proglog.Warnf("[prism pr] warning: could not resolve instance_id for spawn_inputs (%v)\n", lookupErr)
		return
	}

	// Compute skills and agent-role hashes the same way cmd/spawn.go does so
	// the audit columns line up across the two CLI front doors.
	skillsDir := prismSkillsDir()
	skillsManifestHash, hashErr := skills.ComputeManifest(skillsDir)
	if hashErr != nil {
		proglog.Warnf("[prism pr] warning: could not compute skills manifest hash: %v\n", hashErr)
		skillsManifestHash = ""
	}
	agentPromptHash, hashErr := skills.ComputeAgentPromptHash(prismAgentRolePath(a.agentRole))
	if hashErr != nil {
		proglog.Warnf("[prism pr] warning: could not compute agent prompt hash: %v\n", hashErr)
		agentPromptHash = ""
	}

	si := db.SpawnInputs{
		InstanceID: *st.InstanceID,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if a.profileName != "" {
		si.ProfileName = &a.profileName
	}
	if a.modelFlag != "" {
		si.ModelFlag = &a.modelFlag
	}
	if a.variantFlag != "" {
		si.VariantFlag = &a.variantFlag
	}
	if a.agentFlag != "" {
		si.AgentFlag = &a.agentFlag
	}
	if a.harnessFlag != "" {
		si.HarnessFlag = &a.harnessFlag
	}
	// Mirror spawnInputsFromOpts (#2102): record the raw --isolation flag when
	// passed, else fall back to the resolved effective mode so the column is
	// always populated for the compare Spawn Inputs block.
	if a.isolationFlag != "" {
		si.IsolationFlag = &a.isolationFlag
	} else if a.isolationMode != "" {
		si.IsolationFlag = &a.isolationMode
	}
	if a.prNumber != "" {
		if n, convErr := strconv.Atoi(a.prNumber); convErr == nil {
			si.PRNumber = &n
		}
	}
	si.IgnoreConcurrencyCap = a.ignoreConcurrencyCap
	if len(a.modelsByRole) > 0 {
		if encoded, encErr := json.Marshal(a.modelsByRole); encErr == nil {
			s := string(encoded)
			si.ModelVariantOverrides = &s
		}
	}
	if skillsManifestHash != "" {
		si.SkillsManifestHash = &skillsManifestHash
	}
	if agentPromptHash != "" {
		si.AgentPromptHash = &agentPromptHash
	}
	if a.promptText != "" {
		si.PromptText = &a.promptText
	}
	if a.promptSource != "" {
		si.PromptSource = &a.promptSource
	}

	if err := d.InsertSpawnInputs(si); err != nil {
		proglog.Warnf("[prism pr] warning: could not write spawn_inputs: %v\n", err)
	}
}

func init() {
	prCmd.Flags().String("repo", "", "Target repo name under ~/code, or full path")
	addPromptFlags(prCmd)
	prCmd.Flags().String("agent", "", `Agent to use (default: "coordinator" on main, "worker" otherwise)`)
	prCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	prCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	prCmd.Flags().String("profile", "", "Model profile name from ~/.config/prism/profiles.json (e.g. anthropic, gemini-hybrid)")
	prCmd.Flags().String("model", "", "Model identifier override (e.g. anthropic/claude-sonnet-4-6); overrides profile's primary model")
	prCmd.Flags().String("variant", "", "Model variant override for all agents (e.g. high, max, minimal)")
	prCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable, e.g. review-context=google/gemini-2.5-pro)")
	prCmd.Flags().String("harness", "", "Agent harness to use (default: from profile slot, or 'pi')")
	prCmd.Flags().String("isolation", "", "Isolation mode: podman, bwrap, sandbox-exec, or host (default: from ~/.config/prism/config.json)")
	rootCmd.AddCommand(prCmd)
}
