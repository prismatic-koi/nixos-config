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
//	--isolation <mode>              isolation mode: bwrap, sandbox-exec, or host

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

// prReadOnlyGuidance is injected into every `prism pr <number>` session's
// prompt, regardless of whether the caller supplied --prompt / --prompt-file.
// `prism pr` always targets a pre-existing PR (Case 1 in agents/coordinator.md),
// so the session never authored the PR and must default to review-only. The
// command cannot establish who authored the PR branch, so this guidance is
// injected unconditionally (issue #2633) — fail safe toward read-only rather
// than attempt to infer authorship from the branch or PR metadata. Only an
// explicit operator instruction given during the session lifts this.
const prReadOnlyGuidance = `IMPORTANT — this session was started with ` + "`prism pr <number>`" + `, which always
targets a pre-existing PR that this session did not author. Treat this PR as
read-only:

- Review the PR and report findings only.
- Do NOT commit, push, or otherwise mutate this PR.
- Do NOT run ` + "`prism merge`" + ` on it — you do not decide when it lands.
- The only thing that lifts this constraint is an explicit instruction from
  the operator, given during this session. Do not infer authorship from the
  branch name or PR author and treat that as permission to edit.

See "Case 1" in agents/coordinator.md and agents/worker.md for the full
review-only flow this session should follow.`

// withPRReadOnlyGuidance prepends the read-only guidance to a caller-supplied
// prompt (preserving it, per issue #2633 AC2) or returns the guidance alone
// when the caller passed neither --prompt nor --prompt-file.
//
// Idempotent: there are exactly two call sites, this one and the one in
// cmd/spawn.go's runSpawn. A container-routed `prism pr <number>` hits both
// — once here (client-side, before the request is proxied), then again
// host-side when the host-API /spawn handler shells out to
// `prism spawn --pr <n>` (a second, separate process invocation of this
// binary). That double-application is the common case for every sandboxed
// coordinator, not an edge case — see TestPrCmd_ContainerMode_
// InjectsReadOnlyGuidance's count assertion, which simulates it. If the
// guidance is already present, the prompt is returned unchanged rather than
// wrapped a second time.
//
// Every other path applies this helper exactly once and cannot double-apply:
// a direct (non-proxied) `prism pr <number>` never calls runSpawn (this
// file's RunE drives its own worktree creation via ensureAndSwitch), and a
// direct `prism spawn --pr <number>` only ever reaches the spawn.go call
// site. This holds structurally — grep `withPRReadOnlyGuidance` and there
// are no other call sites to re-derive this from.
func withPRReadOnlyGuidance(callerPrompt string) string {
	if strings.Contains(callerPrompt, prReadOnlyGuidance) {
		return callerPrompt
	}
	if callerPrompt == "" {
		return prReadOnlyGuidance
	}
	return prReadOnlyGuidance + "\n\n---\n\n" + callerPrompt
}

var prCmd = &cobra.Command{
	Use:   "pr <number>",
	Short: "Check out a PR branch as a new worktree and switch to it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) (retErr error) {
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
		// prism pr always targets a pre-existing PR (Case 1), so the session
		// is review-only by default regardless of --prompt / --prompt-file.
		// See prReadOnlyGuidance above for the rationale (issue #2633).
		promptFlag = withPRReadOnlyGuidance(promptFlag)

		bareRoot, err := resolveBareRoot(repoFlag)
		if err != nil {
			return err
		}

		// In container mode, proxy the spawn to the host API. The raw PR
		// number is forwarded as "pr" — NOT resolved to a branch locally — so
		// the host-side prism spawn (which runs the correct FetchRemote +
		// PRBranch + CreateWorktree-tracking-origin path) preserves the real
		// PR head ref end-to-end. Resolving the branch client-side and
		// forwarding only a sanitised branch name silently forked a new
		// branch from the default branch whenever the PR head ref contained a
		// slash (issue #2432).
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			repo := filepath.Base(bareRoot)
			var resp struct {
				SessionName string `json:"session_name"`
				// Warning carries the sidecar's prism-binary staleness
				// diagnostic (issue #2742), set only when the sidecar that
				// handled this spawn launched from a binary a switch has
				// since replaced. Empty in the common case; the field is
				// simply absent from the JSON then.
				Warning string `json:"warning"`
			}
			body := map[string]any{
				"repo":                   repo,
				"pr":                     prNumber,
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
			if resp.Warning != "" {
				fmt.Fprintln(os.Stderr, resp.Warning)
			}
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

		created, err := git.CreateWorktree(bareRoot, branch)
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}
		worktreePath := created.Path
		// Caller-level rollback (#2363): a failure in any step between
		// worktree creation and ensureAndSwitch success removes the freshly
		// created worktree. A pre-existing PR branch is never deleted —
		// CreateWorktree marks only freshly forked branches for deletion —
		// so only the worktree created for it is unwound. Rollback failures
		// are logged, never returned.
		createdWorktree := &created
		defer func() {
			if retErr != nil {
				rollbackCreatedWorktree(bareRoot, createdWorktree, "prism pr")
			}
		}()

		// Resolve the active profile (flag → state file → nix default), mirroring
		// the pattern in spawn.go so that --profile and `prism profile use` work.
		var pf *config.ProfilesFile
		{
			var pfErr error
			pf, pfErr = config.LoadProfiles()
			if pfErr != nil {
				if isoCaps.RequiresProfilesFile || profileFlag != "" {
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

		// Validate the resolved profile before any session state is created.
		//
		// This replaces the validation that config.BuildConfigContent supplied
		// as a side effect before #2854 retired it. It is deliberately NOT a
		// like-for-like restoration — RequireSlot is strictly stronger:
		//
		//   - BuildConfigContent rejected a nil profiles file paired with a
		//     non-empty profile name, and rejected an unknown profile name.
		//     RequireSlot rejects both, so the guard stays keyed on
		//     resolvedProfile alone: a state file naming a profile while
		//     profiles.json failed to load must still fail here, as it did
		//     before.
		//   - BuildConfigContent did NOT check slot presence. It looked the
		//     role up with a comma-ok and silently emitted an empty model on a
		//     miss. RequireSlot errors instead, so a profile with no slot for
		//     this session's root role is now rejected where it previously
		//     proceeded. That is intentional: it matches the gate cmd/spawn.go
		//     already applies, and a session with no slot for its own role has
		//     no model to run on.
		//
		// The slot's model, provider, and thinking level reach pi over argv —
		// resolved at agent-run time by populatePIConfig (bwrap /
		// sandbox-exec) or emitted by buildDirectAgentCmd (host).
		effectiveRole := session.DefaultAgent(worktreePath, agentFlag)
		lookupRole := effectiveRole
		if lookupRole == "" {
			lookupRole = "coordinator"
		}
		if resolvedProfile != "" {
			if err := config.RequireSlot(pf, resolvedProfile, lookupRole); err != nil {
				return err
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
			Prompt:         promptFlag,
			Agent:          agentFlag,
			Headless:       !attachFlag,
			IsolationMode:  string(isoMode),
			PluginHostPath: cfg.SidecarPluginPath,
			RuntimeEnvVars: prHarness.RuntimeEnv(),
			ModelsByRole:   modelsByRole,
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
		// The session is live: disarm the worktree rollback. Nothing after
		// this point may tear down the worktree of a running session.
		createdWorktree = nil

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
	worktreePath  string
	bareRoot      string
	agentRole     string
	profileName   string
	modelFlag     string
	variantFlag   string
	agentFlag     string
	harnessFlag   string
	isolationFlag string
	// isolationMode is the resolved effective isolation mode for the
	// session — always populated (#2105) so spawn_inputs.isolation_mode
	// reflects what the session actually ran under, even when --isolation
	// was omitted and isolationFlag is therefore "".
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

	si := buildPRSpawnInputs(a, *st.InstanceID, skillsManifestHash, agentPromptHash, time.Now().UnixMilli())
	if err := d.InsertSpawnInputs(si); err != nil {
		proglog.Warnf("[prism pr] warning: could not write spawn_inputs: %v\n", err)
	}
}

// buildPRSpawnInputs builds the db.SpawnInputs row written by the `prism pr`
// path from its audit-field args. Factored out of writeSpawnInputsForPR so
// the flag-to-column mapping is testable without spinning up tmux / git /
// the cobra command. Mirrors spawnInputsFromOpts in internal/session/spawn.go
// — the two writers must stay in sync because both front doors share the
// same spawn_inputs schema. Issue #2105.
func buildPRSpawnInputs(a prSpawnInputsArgs, instanceID, skillsManifestHash, agentPromptHash string, createdAt int64) db.SpawnInputs {
	si := db.SpawnInputs{
		InstanceID: instanceID,
		CreatedAt:  createdAt,
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
	if a.isolationFlag != "" {
		si.IsolationFlag = &a.isolationFlag
	}
	// isolationMode mirrors spawn_inputs.isolation_mode — the resolved
	// effective mode the session ran under, distinct from the raw
	// --isolation flag value. Always populated post-#2105 so the
	// `prism stats compare` Spawn Inputs block surfaces a meaningful value
	// even when --isolation was omitted (the common case).
	if a.isolationMode != "" {
		si.IsolationMode = &a.isolationMode
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
	return si
}

func init() {
	prCmd.Flags().String("repo", "", "Target repo name under ~/code, or full path")
	addPromptFlags(prCmd)
	prCmd.Flags().String("agent", "", `Agent to use (default: "coordinator" on main, "worker" otherwise)`)
	prCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	prCmd.Flags().Bool("ignore-concurrency-cap", false, config.IgnoreConcurrencyCapHelp)
	prCmd.Flags().String("profile", "", "Model profile name from ~/.config/prism/profiles.json (e.g. anthropic, gemini-hybrid)")
	prCmd.Flags().String("model", "", "Model identifier override (e.g. anthropic/claude-sonnet-4-6); overrides profile's primary model")
	prCmd.Flags().String("variant", "", "Model variant override for all agents (e.g. high, max, minimal)")
	prCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable, e.g. review-context=google/gemini-2.5-pro)")
	prCmd.Flags().String("harness", "", "Agent harness to use (default: from profile slot, or 'pi')")
	prCmd.Flags().String("isolation", "", "Isolation mode: bwrap, sandbox-exec, or host (default: from ~/.config/prism/config.json)")
	rootCmd.AddCommand(prCmd)
}
