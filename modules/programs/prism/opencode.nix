{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.opencode.enable = lib.mkEnableOption "enables opencode" // {
      default = true;
    };
    nx.programs.prism.opencode.provider = lib.mkOption {
      type = lib.types.enum (builtins.attrNames config.nx.programs.prism.profiles.data.profiles);
      default = "anthropic";
      description = ''
        The model profile to use for baked-in opencode agent config.
        This selects the default profile recorded in profiles.json and drives
        the model strings written into opencode.json. All provider auth plugins
        are always present so any provider can be used mid-session regardless
        of which profile is active.
      '';
    };
    # Serialised opencode.json blobs for container roles.
    # Set by opencode.nix and consumed by profiles.nix to embed them into
    # profiles.json under container_worker_config / container_coordinator_config.
    # The Go CLI reads those keys and passes the appropriate blob to the sidecar
    # as --config-content, which writes it to a temp file and mounts it as
    # /root/.config/opencode/opencode.json inside the container.
    nx.programs.prism.opencode.containerWorkerConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for worker containers (mounted as config file).";
    };
    nx.programs.prism.opencode.containerCoordinatorConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for coordinator containers (mounted as config file).";
    };
    # Per-agent review container config blobs — one per review agent.
    # Each declares only its own agent; all other agents are disabled.
    # Hardened: write/edit/patch denied, task tool disabled,
    # prism review bash commands denied to prevent recursion.
    nx.programs.prism.opencode.containerReviewGoalConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for review-goal containers.";
    };
    nx.programs.prism.opencode.containerReviewCodeConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for review-code containers.";
    };
    nx.programs.prism.opencode.containerReviewSecurityConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for review-security containers.";
    };
    nx.programs.prism.opencode.containerReviewQaConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for review-qa containers.";
    };
    nx.programs.prism.opencode.containerReviewContextConfigJson = lib.mkOption {
      type = lib.types.str;
      default = "";
      internal = true;
      description = "Serialised opencode.json blob for review-context containers.";
    };
    nx.programs.prism.opencode.enhancedReview = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Enable 5-agent parallel review system instead of single @review.";
    };
  };
  config = lib.mkIf config.nx.programs.prism.opencode.enable (
    let
      # Use shared environment variables from prism config
      envPrefix = config.nx.programs.prism._internal.agentEnvPrefix;
      # Define read-only bash commands that can be shared across agents
      readOnlyBashCommands = {
        # file reading/viewing
        "cat *" = "allow";
        "head *" = "allow";
        "less *" = "allow";
        "more *" = "allow";
        "strings *" = "allow";
        "tail *" = "allow";
        # file/directory listing and searching
        "file *" = "allow";
        "find *" = "allow";
        "ls *" = "allow";
        "tree *" = "allow";
        # text processing/searching
        "awk *" = "allow";
        "comm *" = "allow";
        "cut *" = "allow";
        "diff *" = "allow";
        "grep *" = "allow";
        "rg *" = "allow";
        "sed *" = "allow";
        "sort *" = "allow";
        "uniq *" = "allow";
        "wc *" = "allow";
        "xargs *" = "allow";
        # system information (read-only)
        "date *" = "allow";
        "env *" = "allow";
        "hostname *" = "allow";
        "id *" = "allow";
        "printenv *" = "allow";
        "pwd *" = "allow";
        "uname *" = "allow";
        "whoami *" = "allow";
        # json/yaml processing
        "jq *" = "allow";
        "yq *" = "allow";
        "yq eval *" = "allow";
        "yq eval*" = "allow";
        # utilities
        "basename *" = "allow";
        "basename*" = "allow";
        "command *" = "allow";
        "command*" = "allow";
        "dirname *" = "allow";
        "dirname*" = "allow";
        "echo *" = "allow";
        "echo*" = "allow";
        "printf *" = "allow";
        "printf*" = "allow";
        "sleep *" = "allow";
        "sleep*" = "allow";
        "type *" = "allow";
        "type*" = "allow";
        "which *" = "allow";
        "which*" = "allow";
        # git operations (permissive with push gated)
        "git *" = "allow";
        "git push *" = "ask";
        "git push" = "ask";
        # cover `git -C <path> push` and `git --git-dir=... push` variants
        "git -C * push" = "ask";
        "git -C * push *" = "ask";
        "git --git-dir* push" = "ask";
        "git --git-dir* push *" = "ask";
        # cover `git send-pack` and `git receive-pack` (low-level push equivalents)
        "git send-pack" = "ask";
        "git send-pack *" = "ask";
        "git receive-pack *" = "ask";
        # GitHub CLI read operations
        "gh issue view *" = "allow";
        "gh issue list *" = "allow";
        "gh pr view *" = "allow";
        "gh pr list *" = "allow";
        "gh pr diff *" = "allow";
        "gh pr checks *" = "allow";
        "gh repo view *" = "allow";
        "gh release list *" = "allow";
        "gh release view *" = "allow";
        "gh run view *" = "allow";
        "gh run list *" = "allow";
        "gh run watch *" = "allow";
        # Kubernetes read operations
        "kubectl get*" = "allow";
        "kubectl describe*" = "allow";
        "kubectl logs*" = "allow";
        "helm template *" = "allow";
        # Nix read operations
        "nix flake show*" = "allow";
        "nix flake metadata*" = "allow";
        "nix build *" = "allow";
        "nix flake check *" = "allow";
        "nix eval *" = "allow";
        # Go operations
        "go version*" = "allow";
        "go env*" = "allow";
        "go list *" = "allow";
        "go doc *" = "allow";
        "go vet *" = "allow";
        "go test *" = "allow";
        "go get *" = "allow";
        "go mod *" = "allow";
        "go generate *" = "allow";
        "go fmt *" = "allow";
        "gopls *" = "allow";
        "go build *" = "allow";
        # AWS CLI (read-only config)
        "aws *" = "allow";
        # playwright-cli browser automation
        "playwright-cli *" = "allow";
        # pdf text extraction
        "pdftotext *" = "allow";
        # manual pages
        "man *" = "allow";
      };

      # Additional write operations for worker agent
      writeBashCommands = {
        # git write operations
        "git *" = "allow";
        "git commit *" = "allow";
        "git add*" = "allow";
        "git push *" = "ask";
        "git push" = "ask";
        # cover `git -C <path> push` and `git --git-dir=... push` variants
        "git -C * push" = "ask";
        "git -C * push *" = "ask";
        "git --git-dir* push" = "ask";
        "git --git-dir* push *" = "ask";
        # cover `git send-pack` and `git receive-pack` (low-level push equivalents)
        "git send-pack" = "ask";
        "git send-pack *" = "ask";
        "git receive-pack *" = "ask";
        # deny PR merge — merging is a coordinator responsibility, never a worker's
        "gh pr merge" = "deny";
        "gh pr merge *" = "deny";
        # file operations that modify
        "mkdir *" = "allow";
        "rm *" = "allow";
        "mv *" = "allow";
        # nix commands that modify
        "nh os build" = "allow";
        "nh os switch" = "ask";
        "nixfmt *" = "allow";
        # other dev tools
        "npm *" = "allow";
        "podman machine start" = "allow";
        "source *" = "allow";
        "pytest *" = "allow";
        "python3 *" = "allow";
        "python *" = "allow";
        # Kubernetes write operations
        "flux *" = "allow";
        "helm *" = "allow";
        "kubectl *" = "allow";
        "helm dependency update" = "allow";
        # OpenTofu/Terraform operations
        "tofu fmt *" = "allow";
        # Go write operations
        "go run *" = "allow";
        "go install *" = "allow";
        "goimports *" = "allow";
        # Prism session management (worker-safe operations only)
        # Note: prism spawn is intentionally excluded — spawning agents is a
        # coordinator responsibility. Workers must never spawn new sessions (#557).
        "prism checkin" = "allow";
        "prism checkin *" = "allow";
        "prism list-sessions" = "allow";
        "prism prompt *" = "allow";
      };

      # Bash commands for the coordinator agent — inverted model: ask by default,
      # only specific read/orchestration ops allowed without prompting.
      coordinatorBashCommands = {
        # git: pull and read-only inspection only; everything else falls through to ask
        "git pull" = "allow";
        "git pull *" = "allow";
        "git status" = "allow";
        "git status *" = "allow";
        "git log *" = "allow";
        "git diff *" = "allow";
        "git show *" = "allow";
        "git branch" = "allow";
        "git branch *" = "allow";
        "git remote" = "allow";
        "git remote *" = "allow";
        # prism session management (full suite)
        "prism spawn *" = "allow";
        "prism checkin" = "allow";
        "prism checkin *" = "allow";
        "prism list-sessions" = "allow";
        "prism prompt *" = "allow";
        "prism cleanup *" = "allow";
        "prism pr *" = "allow";
        # GitHub: PR/issue lifecycle
        "gh pr merge *" = "ask";
        "gh pr edit *" = "allow";
        "gh pr close *" = "allow";
        "gh issue close *" = "allow";
        "gh issue edit *" = "allow";
        "gh issue comment *" = "allow";
        # nix build validation (read-only result)
        "nix build *" = "allow";
        # system switch — requires sudo, will be caught by permission prompt
        "sudo nixos-rebuild *" = "ask";
      };

      # tmux deny commands to prevent bypassing UI permission guards
      tmuxDenyCommands = {
        "tmux send-keys *" = "deny";
        "tmux run-shell *" = "deny";
        "tmux kill-session" = "deny";
        "tmux kill-server" = "deny";
        "tmux select-window" = "deny";
        "tmux rename-window" = "deny";
        "tmux respawn-window" = "deny";
        "tmux new-window" = "deny";
        "tmux attach-session *" = "deny";
        "tmux switch-client *" = "deny";
      };

      clipboardCmd = if pkgs.stdenv.isDarwin then "pbcopy" else "wl-copy";

      awsSkillFile = pkgs.replaceVars ./opencode/skills/aws/SKILL.md {
        inherit clipboardCmd;
      };

      # Merged skills directory — built as a single derivation so it can be
      # mounted via one xdg.configFile entry (like agents/ and command/).
      # This avoids fragmented xdg.configFile entries that create symlinks
      # inside the persisted ~/.config/opencode/skills/ directory — those
      # symlinks dangle after nix-collect-garbage removes the store paths
      # they pointed to.
      skillsDir = pkgs.runCommand "opencode-skills" { } ''
        mkdir -p $out/prism $out/aws
        cp -r ${./opencode/skills/prism}/* $out/prism/
        ${lib.optionalString pkgs.stdenv.isLinux ''
          cp -r ${./opencode/skills/playwright-cli} $out/playwright-cli
        ''}
        cp ${awsSkillFile} $out/aws/SKILL.md
      '';

      agentInstructions = /* markdown */ ''
        # Global Agent Instructions

        ## Skills
        When working in environments with domain-specific skills available (via the `skill` tool), err on the side of loading them. If a conversation touches a domain that has a skill, load it – even if you think you know the conventions from other context sources.
        Skills exist to prevent context drift and ensure consistency, not just for when you're uncertain. Loading a skill is cheap; missing domain-specific conventions or creating inconsistency is expensive.

        ## Web Fetching

        When the webfetch tool fails with a 403 Forbidden error or similar access restrictions, use playwright-cli via the Bash tool to fetch the content with a real browser instead.
        There is a skill for playwright-cli, activate it if you need it.

        After using playwright-cli, delete the .playwright-cli/ directory as soon as the results are no longer needed – don't wait until the end of the session.

        ## Pull Request Reviews

          ${
            if config.nx.programs.prism.opencode.enhancedReview then
              ''
                After opening a pull request, invoke ALL 5 review agents **in parallel** before announcing completion:
                `@review-goal`, `@review-code`, `@review-security`, `@review-qa`, and `@review-context`.
                Pass the PR number to each. All 5 must return `<verdict>PASS</verdict>` for the review to pass.
                If ANY agent returns FAIL, fix all blocking issues, push, and re-run all 5 agents.
                After 3 full review cycles without convergence, stop and escalate — do not run a 4th cycle.
                Preferred invocation: `prism review <pr-number>` (spawns all 5 agents automatically, provides dashboard observability).
                Fallback: invoke `@review-goal`, `@review-code`, `@review-security`, `@review-qa`, and `@review-context` directly as parallel Task calls.''
            else
              ''
                After opening a pull request, always invoke the `@review` subagent, passing it the PR number.
                The review agent will check the PR for bugs, structural issues, and requirement gaps and report back.
                If it identifies issues, fix them and then invoke `@review` again with the same PR number.
                Repeat this cycle until the review passes with no issues before considering the work complete.''
          }

        ## Search Scope

        When asked to find something without an explicit scope, ALWAYS search within the working directory only. NEVER traverse to parent directories unless the user explicitly instructs you to. If you cannot find something in the working directory, say so — do not expand the search scope on your own.

        ## Local Environment Instructions

        Avoid excessive use of `cd` commands at the start of your commands, if you are already in the right working directory, there is no need to `cd` into it before your command.

        Use podman, not docker.${lib.optionalString pkgs.stdenv.isDarwin " Before use, always run `podman machine start`"}

        ## Te Reo Māori Integration

        Ben is based in Aotearoa New Zealand and is actively building Te Reo Māori into his everyday vocabulary. Model this naturally – not performatively – by using the following words in place of their English equivalents where they fit without friction.

        ### Core substitutions

        | Use this | Instead of |
        |---|---|
        | Kia ora | Hello / Hi |
        | Tēnā koe | Formal greeting |
        | Ka pai | Good / Great / Well done |
        | Āe | Yes |
        | Kāo | No |
        | Ngā mihi | Thanks / Cheers |

        ### Normalised vocabulary

        Use these inline without translation – treat them as shared vocabulary:

        - mahi – work, tasks, activity ("the mahi here is…")
        - kōrero – talk, discussion, conversation
        - whakaaro – thought, idea, intention

        ### Guidelines

        - One or two per response is plenty. Don't pepper sentences.
        - Don't translate inline unless context genuinely demands it.
        - If Ben uses Te Reo in a prompt, mirror it back. If he doesn't, still lead occasionally.
        - Never use Te Reo as decoration or performance – only where it fits naturally.
      '';
      currentProfile =
        config.nx.programs.prism.profiles.data.profiles.${config.nx.programs.prism.opencode.provider};
      models = {
        primary = currentProfile.primary.model;
        secondary = currentProfile.secondary.model;
        lightweight = currentProfile.lightweight.model;
      };

      # Authentication plugins — all provider auth plugins are always loaded so
      # any provider can be used mid-session regardless of which provider is the
      # active default. This mirrors what was done for providerSettings in #370.
      providerPlugins = {
        anthropic = [
          # use existing Claude Code credentials (via claude login OAuth)
          # no separate proxy or API key needed
          "opencode-claude-auth@latest"
        ];
        anthropic-opus = [
          "opencode-claude-auth@latest"
        ];
        # gemini-hybrid uses both Anthropic (Opus primary) and Google (Gemini secondary).
        # Both auth plugins are needed; they are both loaded globally anyway.
        gemini-hybrid = [
          "opencode-claude-auth@latest"

        ];
        github-copilot = [ ];
        google = [ ];
      };
      authPlugins = lib.lists.unique (lib.concatLists (lib.attrValues providerPlugins));

      # All provider blocks are always present so that models from any
      # provider can be used manually regardless of which provider is the
      # active default (which controls only model strings).
      # Note: anthropic-opus shares the anthropic provider block — no separate entry needed.
      providerSettings = {
        anthropic = { };
        github-copilot = { };
        google = { };
      };
      hmUser = config.home-manager.users.${config.nx.username};

      # Container-specific permission sets (fresh design — not derived from host).
      # These are independent of the host command sets above.

      # Worker container: almost fully open because the sandbox is the safety net.
      # bash default = allow-all; only deny coordinator-domain operations.
      containerWorkerBashCommands = {
        # Default: allow everything not explicitly denied
        "*" = "allow";
        # Coordinator-domain operations — deny
        "prism spawn" = "deny";
        "prism spawn *" = "deny";
        "prism pr" = "deny";
        "prism pr *" = "deny";
        "gh pr merge" = "deny";
        "gh pr merge *" = "deny";
        "nixos-rebuild *" = "deny";
        "sudo nixos-rebuild *" = "deny";
      };

      # Coordinator container: strict deny-by-default allowlist.
      containerCoordinatorBashCommands = {
        # Default: deny everything not explicitly allowed
        "*" = "deny";
        # GitHub CLI — full suite (token scoping is the safety net)
        "gh *" = "allow";
        # Prism — full orchestration suite
        "prism *" = "allow";
        # Nix validation
        "nix build *" = "allow";
        # Git read-only operations
        "git pull" = "allow";
        "git pull *" = "allow";
        "git log *" = "allow";
        "git diff *" = "allow";
        "git show *" = "allow";
        "git status" = "allow";
        "git status *" = "allow";
        "git fetch *" = "allow";
        "git branch" = "allow";
        "git branch *" = "allow";
        "git remote" = "allow";
        "git remote *" = "allow";
        # Standard read-only tools
        "cat *" = "allow";
        "ls *" = "allow";
        "ls" = "allow";
        "grep *" = "allow";
        "rg *" = "allow";
        "find *" = "allow";
        "jq *" = "allow";
        "yq *" = "allow";
        # Cloud/cluster inspection — config is the safety net (readonly mounts)
        "aws *" = "allow";
        "kubectl *" = "allow";
        "date *" = "allow";
        "date" = "allow";
        "echo *" = "allow";
        "echo" = "allow";
        "basename *" = "allow";
        "dirname *" = "allow";
        "wc *" = "allow";
        "diff *" = "allow";
        "head *" = "allow";
        "tail *" = "allow";
        "sort *" = "allow";
        "sed *" = "allow";
        "awk *" = "allow";
        "cut *" = "allow";
        "tr *" = "allow";
        "xargs *" = "allow";
        "env *" = "allow";
        "printenv *" = "allow";
        "printenv" = "allow";
        "pwd" = "allow";
        "pwd *" = "allow";
        "which *" = "allow";
        "type *" = "allow";
        "uname *" = "allow";
        # Belt-and-suspenders denies over deny-all
        "nixos-rebuild *" = "deny";
        "sudo nixos-rebuild *" = "deny";
        "rm *" = "deny";
        "mv *" = "deny";
        "mkdir *" = "deny";
      };

      # Providers to expose in containers — restricts model list to these 3.
      containerEnabledProviders = [
        "anthropic"
        "github-copilot"
        "google"
      ];

      # Relative paths resolve from the opencode.json config file location
      # (/root/.config/opencode/) where the plugins/ directory is mounted.
      containerPlugins = [
        "opencode-claude-auth@latest"
        "./plugins/prism-hooks.ts"
      ];

      # Worker container opencode.json blob.
      # All tools enabled; bash is allow-all with explicit denies for
      # coordinator-domain operations. No tmux deny commands (no tmux in container).
      workerContainerOpencodeJson = {
        "$schema" = "https://opencode.ai/opencode.json";
        autoupdate = false;
        default_agent = "worker";
        enabled_providers = containerEnabledProviders;
        model = models.secondary;
        agent =
          let
            profiledAgents =
              config.nx.programs.prism.profiles.applyProfile config.nx.programs.prism.opencode.provider
                (
                  {
                    worker = {
                      description = "Default worker agent with full tool access";
                      mode = "primary";
                      color = config.theme.red;
                      permission = {
                        bash = containerWorkerBashCommands;
                      };
                    };
                    ac = { };
                    explore = { };
                    title = { };
                    summary = { };
                    compaction = { };
                    coordinator = {
                      disable = true;
                    };
                    build = {
                      disable = true;
                    };
                    plan = {
                      disable = true;
                    };
                  }
                  // (
                    if config.nx.programs.prism.opencode.enhancedReview then
                      {
                        review-goal = { };
                        review-code = { };
                        review-security = { };
                        review-qa = { };
                        review-context = { };
                      }
                    else
                      {
                        review = { };
                      }
                  )
                );
            # Strip variant from disabled agents so their primary-role "medium"
            # doesn't poison the model's thinking level for the worker.
            sanitisedAgents = lib.mapAttrs (
              _name: cfg: if cfg ? disable && cfg.disable then builtins.removeAttrs cfg [ "variant" ] else cfg
            ) profiledAgents;
          in
          sanitisedAgents;
        # lib.mkIf cannot be used here — this is serialised with builtins.toJSON,
        # not processed by the module system. Use optionalAttrs so the key is
        # absent entirely on Linux rather than emitting a malformed _type object.
        mcp = lib.optionalAttrs pkgs.stdenv.isDarwin {
          atlasian = {
            type = "local";
            enabled = true;
            command = [ "/root/.config/opencode/mcp-atlassian-slim-proxy.mjs" ];
            environment = {
              ATLASSIAN_MCP_URL = "https://mcp.atlassian.com/v1/mcp";
            };
          };
        };
        permission = {
          edit = "allow";
          webfetch = "allow";
          # Atlassian MCP permissions — workers read freely, no writes
          "atlasian_*" = "deny";
          "atlasian_atlassianUserInfo" = "allow";
          "atlasian_get*" = "allow";
          "atlasian_lookup*" = "allow";
          "atlasian_search*" = "allow";
          "atlasian_fetch" = "allow";
          "atlasian_fetchAtlassian" = "allow";
          bash = containerWorkerBashCommands;
          external_directory = "allow"; # safe because inside container
        };
        plugin = containerPlugins;
        provider = providerSettings;
      };

      # Coordinator container opencode.json blob.
      # The coordinator and plan agents are read-only (write/edit tools disabled,
      # deny-by-default bash). The build agent has full write access with an
      # ask-default bash, matching host worker permissions.
      coordinatorContainerOpencodeJson = {
        "$schema" = "https://opencode.ai/opencode.json";
        autoupdate = false;
        default_agent = "coordinator";
        enabled_providers = containerEnabledProviders;
        model = models.primary;
        agent = config.nx.programs.prism.profiles.applyProfile config.nx.programs.prism.opencode.provider (
          {
            coordinator = {
              description = "Repo coordinator — orchestrates agents, reviews PRs, merges work";
              mode = "primary";
              color = config.theme.purple;
              tools = {
                read = true;
                grep = true;
                glob = true;
                list = true;
                webfetch = true;
                bash = true;
                write = false;
                edit = false;
                patch = false;
              };
              permission = {
                bash = containerCoordinatorBashCommands;
              };
            };
            plan = {
              description = "Planning and analysis agent with read-only access";
              mode = "primary";
              color = config.theme.blue;
              tools = {
                read = true;
                grep = true;
                glob = true;
                list = true;
                webfetch = true;
                bash = true;
                write = false;
                edit = false;
                patch = false;
              };
              permission = {
                bash = {
                  # Default deny everything else for plan agent (MUST be first - last match wins)
                  "*" = "deny";
                }
                // readOnlyBashCommands
                // tmuxDenyCommands;
              };
            };
            build = {
              description = "Implementation agent with full write access";
              mode = "primary";
              color = config.theme.red;
              # No tools block: all tools enabled by default (unlike coordinator/plan
              # which explicitly disable write/edit). Full access is intentional here.
              permission = {
                edit = "allow";
                webfetch = "allow";
                # Atlassian MCP permissions — same ask set as host coordinator
                "atlasian_*" = "ask";
                "atlasian_atlassianUserInfo" = "allow";
                "atlasian_get*" = "allow";
                "atlasian_lookup*" = "allow";
                "atlasian_search*" = "allow";
                "atlasian_fetch" = "allow";
                "atlasian_fetchAtlassian" = "allow";
                "atlasian_create*" = "ask";
                "atlasian_edit*" = "ask";
                "atlasian_update*" = "ask";
                "atlasian_add*" = "ask";
                "atlasian_transition*" = "ask";
                bash = {
                  # default for any command not listed is ask (MUST be first - last match wins)
                  "*" = "ask";
                }
                // readOnlyBashCommands
                // writeBashCommands
                // {
                  # Belt-and-suspenders: explicitly deny PR merge even though writeBashCommands
                  # already includes these denies — merging is a coordinator responsibility.
                  "gh pr merge" = "deny";
                  "gh pr merge *" = "deny";
                  # Spawning agents is a coordinator responsibility, never a worker's (#557).
                  "prism spawn" = "deny";
                  "prism spawn *" = "deny";
                  "prism pr" = "deny";
                  "prism pr *" = "deny";
                }
                // tmuxDenyCommands;
              };
            };
            explore = { };
            ac = { };
            title = { };
            summary = { };
            compaction = { };
          }
          // (
            if config.nx.programs.prism.opencode.enhancedReview then
              {
                review-goal = { };
                review-code = { };
                review-security = { };
                review-qa = { };
                review-context = { };
              }
            else
              {
                review = { };
              }
          )
        );
        # lib.mkIf cannot be used here — this is serialised with builtins.toJSON,
        # not processed by the module system. Use optionalAttrs so the key is
        # absent entirely on Linux rather than emitting a malformed _type object.
        mcp = lib.optionalAttrs pkgs.stdenv.isDarwin {
          atlasian = {
            type = "local";
            enabled = true;
            command = [ "/root/.config/opencode/mcp-atlassian-slim-proxy.mjs" ];
            environment = {
              ATLASSIAN_MCP_URL = "https://mcp.atlassian.com/v1/mcp";
            };
          };
        };
        permission = {
          edit = "deny";
          webfetch = "allow";
          # Atlassian MCP permissions — same ask set as host coordinator
          "atlasian_*" = "ask";
          "atlasian_atlassianUserInfo" = "allow";
          "atlasian_get*" = "allow";
          "atlasian_lookup*" = "allow";
          "atlasian_search*" = "allow";
          "atlasian_fetch" = "allow";
          "atlasian_fetchAtlassian" = "allow";
          "atlasian_create*" = "ask";
          "atlasian_edit*" = "ask";
          "atlasian_update*" = "ask";
          "atlasian_add*" = "ask";
          "atlasian_transition*" = "ask";
          bash = containerCoordinatorBashCommands;
          external_directory = "allow"; # safe because inside container
        };
        plugin = containerPlugins;
        provider = providerSettings;
      };

      # Shared deny-by-default bash base for all review agents.
      # "prism review" recursion prevention is applied here; each per-agent set
      # adds only the commands that specific agent needs.
      containerReviewBaseBashCommands = {
        # Default: deny everything not explicitly allowed
        "*" = "deny";
        # prism review recursion prevention (belt-and-suspenders over deny-all)
        "prism review" = "deny";
        "prism review *" = "deny";
        # Standard read-only file/text operations
        "cat *" = "allow";
        "head *" = "allow";
        "less *" = "allow";
        "more *" = "allow";
        "strings *" = "allow";
        "tail *" = "allow";
        "file *" = "allow";
        "find *" = "allow";
        "ls *" = "allow";
        "tree *" = "allow";
        "awk *" = "allow";
        "comm *" = "allow";
        "cut *" = "allow";
        "diff *" = "allow";
        "grep *" = "allow";
        "rg *" = "allow";
        "sed *" = "allow";
        "sort *" = "allow";
        "uniq *" = "allow";
        "wc *" = "allow";
        "xargs *" = "allow";
        "date *" = "allow";
        "env *" = "allow";
        "hostname *" = "allow";
        "id *" = "allow";
        "printenv *" = "allow";
        "pwd *" = "allow";
        "uname *" = "allow";
        "whoami *" = "allow";
        "jq *" = "allow";
        "yq *" = "allow";
        "yq eval *" = "allow";
        "yq eval*" = "allow";
        "basename *" = "allow";
        "basename*" = "allow";
        "command *" = "allow";
        "command*" = "allow";
        "dirname *" = "allow";
        "dirname*" = "allow";
        "echo *" = "allow";
        "echo*" = "allow";
        "printf *" = "allow";
        "printf*" = "allow";
        "sleep *" = "allow";
        "sleep*" = "allow";
        "type *" = "allow";
        "type*" = "allow";
        "which *" = "allow";
        "which*" = "allow";
        # git read-only operations (no push)
        "git log *" = "allow";
        "git diff *" = "allow";
        "git show *" = "allow";
        "git status" = "allow";
        "git status *" = "allow";
        "git branch" = "allow";
        "git branch *" = "allow";
        "git remote" = "allow";
        "git remote *" = "allow";
        "git fetch *" = "allow";
        # prism read-only introspection
        "prism checkin" = "allow";
        "prism checkin *" = "allow";
        "prism list-sessions" = "allow";
      };

      # review-code, review-goal, review-security: read-only bash.
      # No test runners, no gh CLI (they don't need issue/PR context fetching).
      containerReviewReadOnlyBashCommands = containerReviewBaseBashCommands;

      # review-qa: adds test-runner commands on top of the read-only base.
      containerReviewQaBashCommands = containerReviewBaseBashCommands // {
        "go test *" = "allow";
        "go build *" = "allow";
        "go vet *" = "allow";
        "nix build *" = "allow";
        "nix flake check *" = "allow";
      };

      # review-context: adds gh issue/PR read commands and git log/show/diff
      # on top of the read-only base.
      containerReviewContextBashCommands = containerReviewBaseBashCommands // {
        "gh issue view *" = "allow";
        "gh issue list *" = "allow";
        "gh pr view *" = "allow";
        "gh pr list *" = "allow";
        "gh pr diff *" = "allow";
        "gh pr checks *" = "allow";
        "gh repo view *" = "allow";
        "gh run view *" = "allow";
        "gh run list *" = "allow";
      };

      # Shared hardened permission block for all per-agent review containers.
      # Write operations are denied (belt-and-braces: worktree is mounted read-only).
      containerReviewPermission = bashCmds: {
        edit = "deny";
        patch = "deny";
        write = "deny";
        webfetch = "allow";
        # Atlassian MCP permissions — review agents read only, no writes
        "atlasian_*" = "deny";
        "atlasian_atlassianUserInfo" = "allow";
        "atlasian_get*" = "allow";
        "atlasian_lookup*" = "allow";
        "atlasian_search*" = "allow";
        "atlasian_fetch" = "allow";
        "atlasian_fetchAtlassian" = "allow";
        bash = bashCmds;
        external_directory = "allow"; # safe because inside container
      };

      # Helper to build a per-agent review opencode.json blob.
      # agentName: the single review agent to declare (e.g. "review-goal").
      # otherAgents: the other 4 review agents to disable.
      # bashCmds: the bash permission map to use.
      makeReviewAgentBlob =
        agentName: otherAgents: bashCmds:
        let
          # Build the agent map: agentName gets task=false; all others are disabled.
          baseAgentMap = {
            worker = {
              disable = true;
            };
            coordinator = {
              disable = true;
            };
            build = {
              disable = true;
            };
            plan = {
              disable = true;
            };
            ac = {
              disable = true;
            };
            explore = {
              disable = true;
            };
            title = {
              disable = true;
            };
            summary = {
              disable = true;
            };
            compaction = {
              disable = true;
            };
          }
          // lib.genAttrs otherAgents (_: {
            disable = true;
          })
          // {
            ${agentName} = {
              tools = {
                task = false;
              };
            };
          };
          profiledAgents = config.nx.programs.prism.profiles.applyProfile config.nx.programs.prism.opencode.provider baseAgentMap;
          # Strip variant from disabled agents
          sanitisedAgents = lib.mapAttrs (
            _name: cfg: if cfg ? disable && cfg.disable then builtins.removeAttrs cfg [ "variant" ] else cfg
          ) profiledAgents;
        in
        {
          "$schema" = "https://opencode.ai/opencode.json";
          autoupdate = false;
          default_agent = agentName;
          enabled_providers = containerEnabledProviders;
          model = models.secondary;
          agent = sanitisedAgents;
          # Atlassian MCP may be present (read-only, for context lookup).
          # lib.mkIf cannot be used here — serialised with builtins.toJSON.
          mcp = lib.optionalAttrs pkgs.stdenv.isDarwin {
            atlasian = {
              type = "local";
              enabled = true;
              command = [ "/root/.config/opencode/mcp-atlassian-slim-proxy.mjs" ];
              environment = {
                ATLASSIAN_MCP_URL = "https://mcp.atlassian.com/v1/mcp";
              };
            };
          };
          permission = containerReviewPermission bashCmds;
          plugin = containerPlugins;
          provider = providerSettings;
        };

      # Five per-agent review container opencode.json blobs.
      # Each declares ONLY its own agent; all others (including the other 4
      # review agents and worker/coordinator/build/plan/ac/explore/title/
      # summary/compaction) are explicitly disabled.
      reviewGoalContainerOpencodeJson = makeReviewAgentBlob "review-goal" [
        "review-code"
        "review-security"
        "review-qa"
        "review-context"
      ] containerReviewReadOnlyBashCommands;

      reviewCodeContainerOpencodeJson = makeReviewAgentBlob "review-code" [
        "review-goal"
        "review-security"
        "review-qa"
        "review-context"
      ] containerReviewReadOnlyBashCommands;

      reviewSecurityContainerOpencodeJson = makeReviewAgentBlob "review-security" [
        "review-goal"
        "review-code"
        "review-qa"
        "review-context"
      ] containerReviewReadOnlyBashCommands;

      reviewQaContainerOpencodeJson = makeReviewAgentBlob "review-qa" [
        "review-goal"
        "review-code"
        "review-security"
        "review-context"
      ] containerReviewQaBashCommands;

      reviewContextContainerOpencodeJson = makeReviewAgentBlob "review-context" [
        "review-goal"
        "review-code"
        "review-security"
        "review-qa"
      ] containerReviewContextBashCommands;
    in
    lib.mkMerge [
      # Set container config JSON blobs as options so profiles.nix can embed
      # them into profiles.json under container_worker_config /
      # container_coordinator_config / container_review_config.
      {
        nx.programs.prism.opencode.containerWorkerConfigJson = builtins.toJSON workerContainerOpencodeJson;
        nx.programs.prism.opencode.containerCoordinatorConfigJson =
          builtins.toJSON coordinatorContainerOpencodeJson;
        nx.programs.prism.opencode.containerReviewGoalConfigJson =
          builtins.toJSON reviewGoalContainerOpencodeJson;
        nx.programs.prism.opencode.containerReviewCodeConfigJson =
          builtins.toJSON reviewCodeContainerOpencodeJson;
        nx.programs.prism.opencode.containerReviewSecurityConfigJson =
          builtins.toJSON reviewSecurityContainerOpencodeJson;
        nx.programs.prism.opencode.containerReviewQaConfigJson =
          builtins.toJSON reviewQaContainerOpencodeJson;
        nx.programs.prism.opencode.containerReviewContextConfigJson =
          builtins.toJSON reviewContextContainerOpencodeJson;
      }

      # When enhanced review is on, inject ENHANCED_REVIEW=true into the agent
      # env prefix so the prism-hooks plugin can read it at runtime.
      (lib.mkIf config.nx.programs.prism.opencode.enhancedReview {
        nx.programs.prism.agent.envVars = {
          ENHANCED_REVIEW = "true";
        };
      })

      # Common configuration for both platforms
      {
        home-manager.users.${config.nx.username} = {
          home.packages =
            with pkgs;
            [
              # need npx on path for memory mcp
              nodejs_24
              # pdf text extraction for agent use
              poppler-utils
            ]
            ++ lib.optionals pkgs.stdenv.isLinux [
              # playwright-cli depends on chromium which is Linux-only
              playwright-cli
            ];
          programs.zsh.shellAliases = {
            # set environment variables for opencode when run directly (not via prism spawn)
            opencode = "${envPrefix} opencode";
          };
          # Theme is configured in tui.json, not opencode.json
          xdg.configFile."opencode/tui.json".text = builtins.toJSON {
            "$schema" = "https://opencode.ai/tui.json";
            theme = config.theme.opencodename;
          };
          programs.opencode = {
            enable = true;
            settings = {
              autoupdate = false;
              model = models.primary;
              agent = config.nx.programs.prism.profiles.applyProfile config.nx.programs.prism.opencode.provider (
                {
                  worker = {
                    description = "Default worker agent with full tool access";
                    mode = "primary";
                    color = config.theme.red;
                    permission = {
                      bash = {
                        # default for any command not listed is ask (MUST be first - last match wins)
                        "*" = "ask";
                      }
                      // readOnlyBashCommands
                      // writeBashCommands
                      // tmuxDenyCommands;
                    };
                  };
                  plan = {
                    description = "Planning and analysis agent with read-only access";
                    mode = "primary";
                    color = config.theme.blue;
                    tools = {
                      read = true;
                      grep = true;
                      glob = true;
                      list = true;
                      webfetch = true;
                      bash = true;
                      # Disable write operations
                      write = false;
                      edit = false;
                    };
                    permission = {
                      bash = {
                        # Default deny everything else for plan agent (MUST be first - last match wins)
                        "*" = "deny";
                      }
                      // readOnlyBashCommands
                      // tmuxDenyCommands;
                    };
                  };
                  coordinator = {
                    description = "Repo coordinator — orchestrates agents, reviews PRs, merges work";
                    mode = "primary";
                    color = config.theme.purple;
                    tools = {
                      read = true;
                      grep = true;
                      glob = true;
                      list = true;
                      webfetch = true;
                      bash = true;
                      write = false;
                      edit = false;
                    };
                    permission = {
                      bash = {
                        # inverted model: everything not explicitly listed → ask
                        "*" = "ask";
                      }
                      // readOnlyBashCommands
                      // {
                        # override the broad "git *" = "allow" from readOnlyBashCommands —
                        # coordinator only gets specific git read ops, not the full suite
                        "git *" = "ask";
                        # override aws/playwright from readOnlyBashCommands — coordinator
                        # is orchestration-only, these are not in its remit
                        "aws *" = "ask";
                        "playwright-cli *" = "ask";
                      }
                      // coordinatorBashCommands
                      // tmuxDenyCommands;
                    };
                  };
                  ac = {
                  };
                  # Lightweight built-in subagents — use a cheaper/faster model since these
                  # do simple, mechanical tasks that don't require deep reasoning.
                  explore = {
                  };
                  title = {
                  };
                  summary = {
                  };
                  compaction = {
                  };
                  retro = {
                    description = "Analyses agent sessions for quality patterns and improvement opportunities";
                    mode = "subagent";
                    tools = {
                      read = true;
                      grep = true;
                      glob = true;
                      list = true;
                      bash = true;
                      # No write/edit tools — retro is read-only
                      write = false;
                      edit = false;
                      patch = false;
                      webfetch = false;
                    };
                    permission = {
                      bash = {
                        # Default deny (MUST be first - last match wins)
                        "*" = "deny";
                      }
                      // readOnlyBashCommands
                      // {
                        # Prism read-only commands
                        "prism stats" = "allow";
                        "prism stats *" = "allow";
                        "prism checkin" = "allow";
                        "prism checkin *" = "allow";
                        "prism list-sessions" = "allow";
                        # SQLite read-only access to prism DB — the ?mode=ro flag enforces
                        # read-only at the SQLite engine level, rejecting all write operations.
                        # The path is baked in at Nix eval time via hmUser.xdg.stateHome.
                        "sqlite3 file:${hmUser.xdg.stateHome}/prism/prism.db?mode=ro *" = "allow";
                      }
                      // tmuxDenyCommands;
                    };
                  };
                }
                // (
                  if config.nx.programs.prism.opencode.enhancedReview then
                    {
                      review-goal = {
                      };
                      review-code = {
                      };
                      review-security = {
                      };
                      review-qa = {
                      };
                      review-context = {
                      };
                    }
                  else
                    {
                      review = {
                      };
                    }
                )
              );
              mcp = {
                atlasian = lib.mkIf pkgs.stdenv.isDarwin {
                  type = "local";
                  enabled = true;
                  command = [
                    "${
                      config.home-manager.users.${config.nx.username}.xdg.configHome
                    }/opencode/mcp-atlassian-slim-proxy.mjs"
                  ];
                  environment = {
                    ATLASSIAN_MCP_URL = "https://mcp.atlassian.com/v1/mcp";
                    # MCP_SLIM_DISABLE can be set in shell to disable slimming (defaults to false/enabled)
                  };
                };
              };
              permission = {
                edit = "allow";
                webfetch = "allow";
                # Atlassian MCP permissions
                # fallback to ask
                "atlasian_*" = "ask";
                # Read operations (allow)
                "atlasian_atlassianUserInfo" = "allow";
                "atlasian_get*" = "allow";
                "atlasian_lookup*" = "allow";
                "atlasian_search*" = "allow";
                "atlasian_fetch" = "allow";
                "atlasian_fetchAtlassian" = "allow";
                # Write operations (ask)
                "atlasian_create*" = "ask";
                "atlasian_edit*" = "ask";
                "atlasian_update*" = "ask";
                "atlasian_add*" = "ask";
                "atlasian_transition*" = "ask";
                # Bash permissions are now defined per-agent
                bash = {
                  # default for any command not listed is ask (MUST be first - last match wins)
                  "*" = "ask";
                }
                // readOnlyBashCommands
                // writeBashCommands
                // tmuxDenyCommands;
              };
              plugin = [
                # Gemini auth is always loaded — it enables Google Gemini as an
                # alternative provider regardless of which provider is the primary.
                # It does not conflict with the provider-specific auth in authPlugins.
                "opencode-gemini-auth@latest"
                "opencode-claude-auth@latest"
                # tmux window status colours based on agent state
                "./plugins/prism-hooks"
              ]
              ++ authPlugins;
              provider = providerSettings;
            };
            context = agentInstructions;
          };
          # Copy the MCP proxy script
          xdg.configFile."opencode/mcp-atlassian-slim-proxy.mjs" = {
            source = ./opencode/mcp-atlassian-slim-proxy.mjs;
            executable = true;
          };
          # Copy command workflow guides
          xdg.configFile."opencode/command".source = ./opencode/command;
          # Custom agents — switch directory based on enhanced review flag
          xdg.configFile."opencode/agents".source =
            if config.nx.programs.prism.opencode.enhancedReview then
              ./opencode/agents-enhanced
            else
              ./opencode/agents;
          # tmux status plugin
          xdg.configFile."opencode/plugins/prism-hooks.ts" = {
            source = ./opencode/plugins/prism-hooks.ts;
          };
          # Skills — mounted as a single directory (like agents/) so
          # impermanence does not create dangling symlinks after GC (#501).
          xdg.configFile."opencode/skills".source = skillsDir;
          # Model profiles — written to ~/.config/prism/profiles.json.
          # The Go CLI reads this at runtime when --profile is passed to
          # prism spawn. It contains all profile definitions and the role-to-agent
          # mapping, and records which profile is the current default.
          xdg.configFile."prism/profiles.json".text = config.nx.programs.prism.profiles.json;
          home.persistence."/persist" = {
            directories = [
              ".config/opencode"
              ".local/share/opencode"
              ".local/state/opencode"
            ];
          };
        };
      }

    ]
  );
}
