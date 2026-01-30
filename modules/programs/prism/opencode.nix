{
  config,
  pkgs,
  lib,
  ...
}: {
  options = {
    nx.programs.prism.opencode.enable =
      lib.mkEnableOption "enables opencode"
      // {
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
        # git read operations
        "git diff *" = "allow";
        "git status*" = "allow";
        "git log*" = "allow";
        "git show*" = "allow";
        "git branch*" = "allow";
        # GitHub CLI read operations
        "gh issue view *" = "allow";
        "gh issue list *" = "allow";
        "gh pr view *" = "allow";
        "gh pr list *" = "allow";
        "gh repo view *" = "allow";
        "gh release list *" = "allow";
        "gh release view *" = "allow";
        # Kubernetes read operations
        "kubectl get*" = "allow";
        "kubectl describe*" = "allow";
        "kubectl logs*" = "allow";
        "flux *" = "allow";
        "helm template *" = "allow";
        # Nix read operations
        "nix flake show*" = "allow";
        "nix flake metadata*" = "allow";
        "nix build *" = "allow";
        "nix flake check *" = "allow";
        # Beads read operations
        "bd show*" = "allow";
        "bd list*" = "allow";
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
        # Kubernetes write operations
        "flux *" = "allow";
        "helm *" = "allow";
        "kubectl *" = "allow";
        "helm dependency update" = "allow";
        # Beads write operations
        "bd*" = "allow";
      };

      sokuPrompt = ''
        You are the soku agent - a specialized worker agent for beads workflow management.

        ## 🚨 YOUR ROLE 🚨

        You work on a SINGLE assigned bead until complete. NO browsing for other work. NO getting distracted by other issues.

        ## STARTUP PROTOCOL

        When you start, beads context is AUTOMATICALLY injected via hooks.

        **If you see "ASSIGNED WORK DETECTED" in your context:**
        → BEGIN WORK IMMEDIATELY without asking permission
        → DO NOT wait for human confirmation
        → This is the "propulsion principle": If work is assigned, YOU RUN IT

        **Check your assigned bead:**
        ```bash
        bd show <bead-id> --json
        ```

        ## CLAIMING WORK

        When you begin working, claim the bead:
        ```bash
        bd update <bead-id> --status=in_progress
        ```

        ## DOING WORK

        - Follow the bead's description and requirements
        - Make commits as you progress: `git add . && git commit -m "..."`
        - If you discover NEW work, create child beads with dependencies:
          ```bash
          bd create --title="..." --type=task --priority=2
          bd dep add <new-bead> <parent-bead>
          ```
        - DO NOT work on discovered issues yourself - file them and stay focused

        ## 🚨 COMPLETION PROTOCOL 🚨

        When your work is done, follow this EXACT checklist:

        [ ] 1. Ensure all changes are committed
        [ ] 2. Close the bead: `bd close <bead-id>`
        [ ] 3. Push your branch: `git push -u origin <branch-name>`
        [ ] 4. Create PR with meaningful summary:
            ```bash
            gh pr create \
              --title "Brief description of what you did" \
              --body "## Summary
        - List the changes made
        - Explain why they were needed

        Closes <bead-id>"
            ```

            **CRITICAL:** Review your commits before creating PR.
            - Title describes WHAT you did (e.g., "Add shared beads via redirect files")
            - Body explains WHY and lists key changes
            - Must include "Closes <bead-id>" for traceability

        [ ] 5. Exit session

        **Your work is NOT complete until the PR is created.** The local branch is not landed.

        ## FORBIDDEN BEHAVIORS

        - DO NOT browse for other work while assigned to a bead
        - DO NOT work on unassigned beads
        - DO NOT ask permission to start if work is assigned via hook
        - DO NOT skip PR creation - the work is not landed without it
        - DO NOT get distracted by other issues - file them as beads and continue

        ## LIFECYCLE

        - **Session**: Your OpenCode instance (ephemeral, can restart)
        - **Sandbox**: Your git worktree (persists across session restarts)
        - **Beads**: Shared database via redirect file (all agents see same state)

        The hooks automatically sync beads when your session ends.
      '';

      agentInstructions =
        /*
        markdown
        */
        ''
          # Global Agent Instructions

          ## Skills
          When working in environments with domain-specific skills available (via the `skill` tool), err on the side of loading them. If a conversation touches a domain that has a skill, load it – even if you think you know the conventions from other context sources.
          Skills exist to prevent context drift and ensure consistency, not just for when you're uncertain. Loading a skill is cheap; missing domain-specific conventions or creating inconsistency is expensive.

          ## Web Fetching

          When the `webfetch` tool fails with a 403 Forbidden error or similar access restrictions, use a subagent with Playwright to fetch the content with a real browser instead.

          ### Usage

          If webfetch returns a 403 error:
          ```
          Error: HTTP 403 Forbidden
          ```

          Do NOT use the playwright_* tools directly in the main conversation, as they generate very large outputs that quickly fill the context window.

          Instead, use the Task tool to launch a subagent that will use Playwright to extract the content and return only the relevant information:
          ```
          Launch a general subagent with a prompt like:
          "Use the Playwright MCP server to navigate to [URL], extract [specific content needed], and return only the extracted information as markdown. Do not include full page snapshots or accessibility trees in your response to me."
          ```

          The subagent will handle all the verbose Playwright interactions in its own context, and only return the clean, extracted content back to you.

          ## Local Environment Instructions

          Avoid excessive use of `cd` commands at the start of your commands, if you are already in the right working directory, there is no need to `cd` into it before your command.

          Use podman, not docker.${
            lib.optionalString pkgs.stdenv.isDarwin " Before use, always run `podman machine start`"
          }
        '';
    in {
      home-manager.users.ben = {
        home.packages = with pkgs; [
          # need npx on path for memory mcp
          nodejs_24
          beads
        ];
        programs.zsh.shellAliases = {
          # set environment variables for opencode
          opencode = "${envPrefix} opencode";
        };
        programs.tmux.extraConfig =
          # tmux
          ''
            # new window with opencode
            bind a new-window "${envPrefix} opencode"
            # opencode scrolling keybinds (only active when opencode is running)
            bind -n C-u if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys C-M-u' 'send-keys C-u'
            bind -n C-d if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys C-M-d' 'send-keys C-d'
            bind -n C-g if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys Home'
            bind -n C-M-g if-shell '[ "#{pane_current_command}" = "opencode" ]' 'send-keys End'
          '';
        programs.neovim.extraLuaConfig =
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
        programs.opencode = {
          enable = true;
          settings = {
            theme = config.theme.opencodename;
            agent = {
              soku = {
                description = "Beads workflow agent with automated context loading";
                mode = "primary";
                prompt = sokuPrompt;
                color = config.theme.orange;
                model = "github-copilot/claude-haiku-4.5";
                permission = {
                  bash = "allow";
                };
              };
              build = {
                description = "Default build agent with full tool access";
                mode = "primary";
                permission = {
                  bash =
                    {
                      # default for any command not listed is ask (MUST be first - last match wins)
                      "*" = "ask";
                    }
                    // readOnlyBashCommands // writeBashCommands;
                };
              };
              plan = {
                description = "Planning and analysis agent with read-only access";
                mode = "primary";
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
                  bash =
                    {
                      # Default deny everything else for plan agent (MUST be first - last match wins)
                      "*" = "deny";
                    }
                    // readOnlyBashCommands;
                };
              };
            };
            mcp = {
              playwright = {
                type = "local";
                command =
                  [
                    "${pkgs.playwright-mcp}/bin/mcp-server-playwright"
                    "--executable-path"
                    (
                      if pkgs.stdenv.isDarwin then
                        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
                      else
                        "${pkgs.chromium}/bin/chromium"
                    )
                    "--headless"
                  ];
                enabled = true;
              };
              atlasian = {
                type = "local";
                enabled = true;
                command = [
                  "${config.home-manager.users.ben.xdg.configHome}/opencode/mcp-atlassian-slim-proxy.mjs"
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
              # Write operations (ask)
              "atlasian_create*" = "ask";
              "atlasian_edit*" = "ask";
              "atlasian_update*" = "ask";
              "atlasian_add*" = "ask";
              "atlasian_transition*" = "ask";
              # Bash permissions are now defined per-agent
              bash =
                {
                  # default for any command not listed is ask (MUST be first - last match wins)
                  "*" = "ask";
                }
                // readOnlyBashCommands
                // writeBashCommands;
            };
            plugin = [
              # a plugin to use Gemini auth for LLM access
              "opencode-gemini-auth@latest"
            ];
          };
          rules = agentInstructions;
        };
        # Copy the plugin directory for local plugins
        xdg.configFile."opencode/plugins".source = ./opencode/plugin;
        # Copy the MCP proxy script
        xdg.configFile."opencode/mcp-atlassian-slim-proxy.mjs" = {
          source = ./opencode/mcp-atlassian-slim-proxy.mjs;
          executable = true;
        };
        home.persistence."/persist" = {
          directories = [
            ".config/opencode"
            ".local/share/opencode"
            ".local/state/opencode"
          ];
        };
      };
    }
  );
}
