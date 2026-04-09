{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf (config.nx.programs.prism.opencode.enable && config.nx.programs.prism.enable) (
    let
      # Re-use the same model/command sets as opencode.nix so configs stay in sync.
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
        # Prism session management
        "prism spawn *" = "allow";
        "prism checkin" = "allow";
        "prism checkin *" = "allow";
        "prism list-sessions" = "allow";
        "prism prompt *" = "allow";
      };

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

      # Additional container-specific deny commands (defence-in-depth)
      containerDenyCommands = {
        "nixos-rebuild *" = "deny";
        "sudo nixos-rebuild *" = "deny";
      };

      # Model identifiers — must stay in sync with opencode.nix.
      providerModels = {
        anthropic = {
          primary = "anthropic/claude-opus-4-6";
          secondary = "anthropic/claude-sonnet-4-6";
          lightweight = "anthropic/claude-haiku-4-5";
        };
        anthropic-opus = {
          primary = "anthropic/claude-opus-4-6";
          secondary = "anthropic/claude-opus-4-6";
          lightweight = "anthropic/claude-haiku-4-5";
        };
        github-copilot = {
          primary = "github-copilot/claude-sonnet-4.6";
          secondary = "github-copilot/claude-sonnet-4.6";
          lightweight = "github-copilot/claude-haiku-4.5";
        };
        google = {
          primary = "google/gemini-3-flash-preview";
          secondary = "google/gemini-3.1-flash-lite-preview";
          lightweight = "google/gemini-3.1-flash-lite-preview";
        };
      };
      models = providerModels.${config.nx.programs.prism.opencode.provider};

      # Providers to expose in containers — restricts model list to these 3.
      enabledProviders = [
        "anthropic"
        "github-copilot"
        "google"
      ];

      # Provider blocks (same as host — empty config, auth via plugins).
      providerSettings = {
        anthropic = { };
        github-copilot = { };
        google = { };
      };

      # Plugins for containers: claude-auth only (no gemini-auth noise).
      containerPlugins = [
        "opencode-claude-auth@latest"
        "./plugins/prism-hooks"
      ];

      agentInstructions = config.home-manager.users.${config.nx.username}.programs.opencode.rules;

      # Atlassian MCP definition (Darwin only).
      atlassianMcp = lib.optionalAttrs pkgs.stdenv.isDarwin {
        atlasian = {
          type = "local";
          enabled = true;
          command = [ "./mcp-atlassian-slim-proxy.mjs" ];
          environment = {
            ATLASSIAN_MCP_URL = "https://mcp.atlassian.com/v1/mcp";
          };
        };
      };

      # Atlassian MCP permissions (same as host).
      atlassianPermissions = {
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
      };

      # skillsDir reuses the same derivation logic as opencode.nix.
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

      skillsDir = pkgs.runCommand "opencode-skills" { } ''
        mkdir -p $out/prism $out/aws
        cp -r ${./opencode/skills/prism}/* $out/prism/
        ${lib.optionalString pkgs.stdenv.isLinux ''
          cp -r ${./opencode/skills/playwright-cli} $out/playwright-cli
        ''}
        cat > $out/aws/SKILL.md << 'SKILL_EOF'
        ${awsSkill}
        SKILL_EOF
      '';

      # Helper: write a JSON config object to a string
      toJson = builtins.toJSON;

      # Shared tui.json content
      tuiJson = toJson {
        "$schema" = "https://opencode.ai/tui.json";
        theme = config.theme.opencodename;
      };

      # Build a complete container config directory derivation.
      # role: "worker" | "coordinator"
      # opencodeJson: attribute set to serialise as opencode.json
      mkContainerConfig =
        role: opencodeJson:
        pkgs.runCommand "opencode-container-config-${role}" { } ''
          mkdir -p $out/agents $out/plugins

          # opencode.json
          cat > $out/opencode.json << 'JSON_EOF'
          ${toJson opencodeJson}
          JSON_EOF

          # tui.json
          cat > $out/tui.json << 'JSON_EOF'
          ${tuiJson}
          JSON_EOF

          # AGENTS.md (agent instructions)
          cat > $out/AGENTS.md << 'AGENTS_EOF'
          ${agentInstructions}
          AGENTS_EOF

          # agents/ directory — copy all agent markdown files
          cp -r ${./opencode/agents}/* $out/agents/

          # command/ directory — copy command workflow guides
          cp -r ${./opencode/command} $out/command

          # skills/ directory
          cp -r ${skillsDir} $out/skills

          # plugins/prism-hooks.ts — the slimmed plugin (sidecar will overlay its own copy)
          cp ${./opencode/plugins/prism-hooks.ts} $out/plugins/prism-hooks.ts

          ${lib.optionalString pkgs.stdenv.isDarwin ''
            # MCP proxy script (Darwin only)
            cp ${./opencode/mcp-atlassian-slim-proxy.mjs} $out/mcp-atlassian-slim-proxy.mjs
            chmod +x $out/mcp-atlassian-slim-proxy.mjs
          ''}
        '';

      # Worker opencode.json config.
      workerOpencodeJson = {
        "$schema" = "https://opencode.ai/opencode.json";
        autoupdate = false;
        default_agent = "worker";
        enabled_providers = enabledProviders;
        model = models.secondary;
        agent = {
          worker = {
            model = models.secondary;
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
              // containerDenyCommands
              // tmuxDenyCommands;
            };
          };
          review = {
            model = models.secondary;
          };
          ac = {
            model = models.secondary;
          };
          explore = {
            model = models.lightweight;
          };
          title = {
            model = models.lightweight;
          };
          summary = {
            model = models.lightweight;
          };
          compaction = {
            model = models.lightweight;
          };
        };
        mcp = atlassianMcp;
        permission = {
          edit = "allow";
          webfetch = "allow";
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
            # git push still requires confirmation
            "git push" = "ask";
            "git push *" = "ask";
            # defence-in-depth: deny nixos-rebuild inside containers
            "nixos-rebuild *" = "deny";
            "sudo nixos-rebuild *" = "deny";
          }
          // tmuxDenyCommands;
        }
        // atlassianPermissions;
        plugin = containerPlugins;
        provider = providerSettings;
      };

      # Coordinator opencode.json config.
      coordinatorOpencodeJson = {
        "$schema" = "https://opencode.ai/opencode.json";
        autoupdate = false;
        default_agent = "coordinator";
        enabled_providers = enabledProviders;
        model = models.primary;
        agent = {
          coordinator = {
            model = models.primary;
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
          plan = {
            model = models.primary;
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
          review = {
            model = models.secondary;
          };
          ac = {
            model = models.secondary;
          };
          explore = {
            model = models.lightweight;
          };
          title = {
            model = models.lightweight;
          };
          summary = {
            model = models.lightweight;
          };
          compaction = {
            model = models.lightweight;
          };
        };
        mcp = atlassianMcp;
        permission = {
          edit = "deny";
          webfetch = "allow";
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
        }
        // atlassianPermissions;
        plugin = containerPlugins;
        provider = providerSettings;
      };

      containerWorkerConfig = mkContainerConfig "worker" workerOpencodeJson;
      containerCoordinatorConfig = mkContainerConfig "coordinator" coordinatorOpencodeJson;
    in
    {
      # Expose derivation paths for prism-tui.nix to reference.
      nx.programs.prism._internal.containerWorkerConfig = containerWorkerConfig;
      nx.programs.prism._internal.containerCoordinatorConfig = containerCoordinatorConfig;
    }
  );
}
