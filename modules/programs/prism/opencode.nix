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

      awsSkill = /* markdown */ ''
        ---
        name: aws
        description: Use AWS CLI correctly. Load this skill when doing any AWS work — querying resources, managing profiles, assuming roles, or asking the user to run AWS commands.
        ---

        # AWS CLI Usage

        ## Critical rules

        - **Never read `~/.aws/`, `~/.config/aws/`, or any AWS config/credentials file directly.** Use the CLI.
        - The `AWS_CONFIG_FILE` env var may point to a restricted config. Respect it — don't override it or reference the default path.
        - Never set `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_PROFILE`, or `AWS_DEFAULT_PROFILE` env vars directly to work around the configured env. If a profile isn't available, ask the user.

        ## Listing and inspecting profiles

        ```bash
        # List all configured profiles
        aws configure list-profiles

        # Show active config for current profile
        aws configure list

        # Show config for a specific profile
        aws configure list --profile <name>
        ```

        ## Selecting a profile

        Pass `--profile <name>` on every command. Profile names match the environment or context being worked on (e.g. `production`, `staging`, `dev`). The same profile name exists in both the agent's restricted config and the user's full-access config — they map to different roles, but the name is the same.

        ```bash
        aws sts get-caller-identity --profile production
        aws s3 ls --profile staging
        ```

        ## When a command requires elevated permissions

        If a command fails due to missing permissions, or you need output from an environment the agent config doesn't have write access to, **ask the user to run it** rather than trying to work around it.

        Provide a ready-to-run command wrapped in `()` piped to the clipboard so the user can run it and paste back the result:

        ```bash
        (aws <command> --profile <name>) | ${clipboardCmd}
        ```

        Example prompt to user:
        > I need the output of the following command. Please run it and paste the result here:
        > ```bash
        > (aws sts get-caller-identity --profile production) | ${clipboardCmd}
        > ```

        ## Common read operations

        ```bash
        # Verify identity
        aws sts get-caller-identity --profile <name>

        # List S3 buckets
        aws s3 ls --profile <name>

        # List EKS clusters
        aws eks list-clusters --profile <name>

        # Describe EC2 instances
        aws ec2 describe-instances --profile <name>

        # List IAM roles
        aws iam list-roles --profile <name>

        # Get SSM parameter
        aws ssm get-parameter --name /my/param --profile <name>
        ```

        ## Region

        Always pass `--region` explicitly or ensure it is set in the profile config. Do not rely on `AWS_DEFAULT_REGION` being set.

        ```bash
        aws ec2 describe-instances --profile <name> --region eu-west-1
        ```
      '';

      # Merged skills directory — built as a single derivation so it can be
      # mounted via one xdg.configFile entry (like agents/ and command/).
      # This avoids fragmented xdg.configFile entries that create symlinks
      # inside the persisted ~/.config/opencode/skills/ directory — those
      # symlinks dangle after nix-collect-garbage removes the store paths
      # they pointed to.
      skillsDir = pkgs.runCommand "opencode-skills" { } ''
        mkdir -p $out/prism $out/prism-db $out/aws
        cp -r ${./opencode/skills/prism}/* $out/prism/
        cp -r ${./opencode/skills/prism-db}/* $out/prism-db/
        ${lib.optionalString pkgs.stdenv.isLinux ''
          cp -r ${./opencode/skills/playwright-cli} $out/playwright-cli
        ''}
        cat > $out/aws/SKILL.md << 'SKILL_EOF'
        ${awsSkill}
        SKILL_EOF
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

        After opening a pull request, always invoke the `@review` subagent, passing it the PR number.
        The review agent will check the PR for bugs, structural issues, and requirement gaps and report back.
        If it identifies issues, fix them and then invoke `@review` again with the same PR number.
        Repeat this cycle until the review passes with no issues before considering the work complete.

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
    in
    lib.mkMerge [
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
            # set environment variables for opencode
            opencode = "${envPrefix} opencode";
          };
          programs.neovim.initLua =
            lib.mkAfter
              # lua
              ''
                -- open current project in new kitty window with opencode
                -- disabled in favor of tmux shortcut (leader a)
                -- vim.keymap.set(
                --   "n",
                --   "<leader>oa",
                --   ":!kitty -d $(pwd) env ${envPrefix} opencode . &<CR><CR>",
                --   { silent = true, desc = "[O]pen project with [A]I agent" }
                -- )
              '';
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
              agent = config.nx.programs.prism.profiles.applyProfile config.nx.programs.prism.opencode.provider {
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
                review = {
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
              };
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
                // {
                  # deny PR merge — merging is a coordinator responsibility, never a worker's
                  "gh pr merge" = "deny";
                  "gh pr merge *" = "deny";
                }
                // tmuxDenyCommands;
              };
              plugin = [
                # Gemini auth is always loaded — it enables Google Gemini as an
                # alternative provider regardless of which provider is the primary.
                # It does not conflict with the provider-specific auth in authPlugins.
                "opencode-gemini-auth@latest"
                # tmux window status colours based on agent state
                "./plugins/prism-hooks"
              ]
              ++ authPlugins;
              provider = providerSettings;
            };
            rules = agentInstructions;
          };
          # Copy the MCP proxy script
          xdg.configFile."opencode/mcp-atlassian-slim-proxy.mjs" = {
            source = ./opencode/mcp-atlassian-slim-proxy.mjs;
            executable = true;
          };
          # Copy command workflow guides
          xdg.configFile."opencode/command".source = ./opencode/command;
          # Custom agents
          xdg.configFile."opencode/agents".source = ./opencode/agents;
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
