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
      };

      # Additional write operations for build agent
      writeBashCommands = {
        # git write operations
        "git *" = "allow";
        "git commit *" = "allow";
        "git add*" = "allow";
        "git push *" = "ask";
        "git push" = "ask";
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
        "pyhton *" = "allow";
        # Kubernetes write operations
        "flux *" = "allow";
        "helm *" = "allow";
        "kubectl *" = "allow";
        "helm dependency update" = "allow";
        # Go write operations
        "go run *" = "allow";
        "go install *" = "allow";
        "goimports *" = "allow";
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

      agentInstructions = /* markdown */ ''
        # Global Agent Instructions

        ## Skills
        When working in environments with domain-specific skills available (via the `skill` tool), err on the side of loading them. If a conversation touches a domain that has a skill, load it – even if you think you know the conventions from other context sources.
        Skills exist to prevent context drift and ensure consistency, not just for when you're uncertain. Loading a skill is cheap; missing domain-specific conventions or creating inconsistency is expensive.

        ## Web Fetching

        When the webfetch tool fails with a 403 Forbidden error or similar access restrictions, use playwright-cli via the Bash tool to fetch the content with a real browser instead.
        There is a skill for playwright-cli, activate it if you need it.

        After using playwright-cli, delete the .playwright-cli/ directory as soon as the results are no longer needed – don't wait until the end of the session.

        ## Local Environment Instructions

        Avoid excessive use of `cd` commands at the start of your commands, if you are already in the right working directory, there is no need to `cd` into it before your command.

        Use podman, not docker.${lib.optionalString pkgs.stdenv.isDarwin " Before use, always run `podman machine start`"}
      '';
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
              model = "anthropic/claude-sonnet-4-6";
              agent = {
                build = {
                  description = "Default build agent with full tool access";
                  mode = "primary";
                  color = config.theme.red;
                  permission = {
                    bash = {
                      # default for any command not listed is ask (MUST be first - last match wins)
                      "*" = "ask";
                    }
                    // readOnlyBashCommands
                    // writeBashCommands;
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
                    // readOnlyBashCommands;
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
                // writeBashCommands;
              };
              plugin = [
                # a plugin to use Gemini auth for LLM access
                "opencode-gemini-auth@latest"
                # tmux window status colours based on agent state
                "./plugins/prism-hooks"
                # use existing Claude Code credentials (via claude login OAuth)
                # no separate proxy or API key needed
                "opencode-claude-auth@latest"
              ];
              provider = {
                anthropic = { };
              };
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
          # tmux status plugin
          xdg.configFile."opencode/plugins/prism-hooks.ts" = {
            source = ./opencode/plugins/prism-hooks.ts;
          };
          # playwright-cli global skill (Linux-only: playwright-cli depends on chromium)
          xdg.configFile."opencode/skills/playwright-cli" = lib.mkIf pkgs.stdenv.isLinux {
            source = ./opencode/skills/playwright-cli;
          };
          # AWS skill — generated so clipboard command is platform-correct
          xdg.configFile."opencode/skills/aws/SKILL.md".text = awsSkill;
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
