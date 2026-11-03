{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.nx.programs.zsh;
  isDarwin = pkgs.stdenv.hostPlatform.isDarwin;
  isLinux = pkgs.stdenv.hostPlatform.isLinux;

  # For NixOS, we use home-manager.users.${username}
  # For darwin with home-manager via nix-darwin, we also use home-manager.users.${username}
  hmConfig = config.home-manager.users.${config.nx.username};

  homeDir = hmConfig.home.homeDirectory;
in
{
  options = {
    nx.programs.zsh.enable = lib.mkEnableOption "enables zsh" // {
      default = true;
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      # NixOS-specific: System-level zsh bootstrap
      # for darwin we do it in the machine currently
      (lib.mkIf isLinux {
        programs.zsh = {
          enable = true;
          shellInit = ''
            export ZDOTDIR=$HOME/.local/share/zsh
          '';
        };
      })

      # Common home-manager config
      {
        home-manager.users.${config.nx.username} = {
          # On NixOS we set ZDOTDIR at system level, so we don't need .zshenv
          # On Darwin we need .zshenv to bootstrap ZDOTDIR
          home.file.".zshenv".enable = isDarwin;

          programs.starship = {
            enable = true;
            enableZshIntegration = false;
            settings =
              let
                glyph = code: builtins.fromJSON ''"\u${code}"'';
              in
              {
                add_newline = false;
                format = "$os$directory$git_branch$git_status$character";
                right_format = lib.concatStrings [
                  "$status$jobs$direnv$python$nodejs$kubernetes$aws$azure$gcloud"
                  "$container$username$hostname$nix_shell"
                ];
                os = {
                  disabled = false;
                  format = "[$symbol]($style)";
                  style = "white";
                  symbols = {
                    NixOS = "${glyph "f313"} ";
                    Macos = "${glyph "f179"} ";
                    Linux = "${glyph "f17c"} ";
                  };
                };
                directory = {
                  style = "blue";
                  format = "[$path]($style) ";
                };
                git_branch = {
                  symbol = "${glyph "f126"} ";
                  style = "green";
                  format = "[$symbol$branch]($style) ";
                };
                git_status = {
                  style = "yellow";
                  format = "([$all_status$ahead_behind]($style) )";
                };
                character = {
                  success_symbol = "[❯](green)";
                  error_symbol = "[❯](red)";
                  vimcmd_symbol = "[❮](green)";
                };
                status = {
                  disabled = false;
                  format = "[✘ $status]($style) ";
                };
                jobs.style = "red";
                direnv.disabled = false;
                kubernetes.disabled = false;
                azure.disabled = false;
              };
          };

          programs.direnv = {
            enable = true;
            enableZshIntegration = true;
            nix-direnv.enable = true;
          };

          programs.zsh = {
            enable = true;
            dotDir = "${hmConfig.xdg.dataHome}/zsh";

            # Completion initialization
            # On NixOS with impermanence, use /persist path
            # On darwin, use regular xdg path
            completionInit =
              let
                dumpFile =
                  if isLinux then
                    "/persist/${homeDir}/.local/share/zsh/.zcompdump"
                  else
                    "${hmConfig.xdg.dataHome}/zsh/.zcompdump";
              in
              ''
                autoload -U compinit
                if [[ -f ${dumpFile} ]]; then
                  compinit -d ${dumpFile}
                else
                  compinit
                fi
              '';

            history = {
              size = 10000;
              expireDuplicatesFirst = true;
              path = "${homeDir}/.local/state/zsh/history";
            };

            autosuggestion = {
              enable = true;
              # Use theme color on NixOS, default on darwin
              # v1 grey1 (dim text) -> theme neutrals.foreground_dim (issue #2814)
              highlight = lib.mkIf (isLinux && config ? theme) "fg=${config.theme.neutrals.foreground_dim}";
            };

            syntaxHighlighting.enable = true;
            # zprof.enable = true; # for troubleshooting startup
            plugins = [
              # Use zsh-vi-mode on all platforms for consistent vi-mode experience
              {
                name = "vi-mode";
                src = pkgs.zsh-vi-mode;
                file = "share/zsh-vi-mode/zsh-vi-mode.plugin.zsh";
              }

            ];

            # Darwin still uses oh-my-zsh for git plugins
            oh-my-zsh = lib.mkIf isDarwin {
              enable = true;
              plugins = [
                "git"
                "history"
              ];
            };

            shellAliases = {
              # preserve env when using sudo
              # sudo = "sudo -E -s";
              # color terminals for ssh targets that don't know kitty
              ssh = "TERM=xterm-color ssh";
              # eza instead of ls
              ls = "eza";
              l = "eza -la";
              tree = "eza --tree -la";
              # ripgrep instead of grep
              grep = "rg";
              # nvim
              v = "nvim";
              # youtube music download script
              ytm-download = "yt-dlp  --add-metadata --format m4a -i -o '~/music/%(artist)s/%(album)s/%(title)s.%(ext)s' --sponsorblock-remove 'music_offtopic' --";
              # git aliases
              gP = "git pull";
              gcb = "git checkout -b";
              # history search
              hs = "history | grep";
            };

            # Set ZVM_INIT_MODE for zsh-vi-mode plugin to not break custom keybindings
            sessionVariables = {
              ZVM_INIT_MODE = "sourcing";
            };

            initContent =
              let
                # p10k loads here (order 899), right before the plugin block
                # (order 900), so it keeps its place ahead of vi-mode.
                # NX_PROMPT=starship skips it; see starshipInit below.
                p10kInit = ''
                  if [[ "$NX_PROMPT" != starship ]]; then
                    source ${pkgs.zsh-powerlevel10k}/share/zsh-powerlevel10k/powerlevel10k.zsh-theme
                    source ${lib.cleanSource ./files}/p10k.zsh
                  fi
                '';

                # Opt-in starship prompt (#3405). Runs after vi-mode.
                # zsh-vi-mode keeps $KEYMAP at "main", so the prompt reads
                # $_nx_keymap, set by the zsh-vi-mode hook.
                starshipInit = ''
                  if [[ "$NX_PROMPT" == starship ]]; then
                    eval "$(starship init zsh)"
                    _nx_keymap=
                    local _from='$'"{KEYMAP:-}" _to='$'"{_nx_keymap:-}"
                    PROMPT="''${PROMPT//$_from/$_to}"
                    RPROMPT="''${RPROMPT//$_from/$_to}"

                    zvm_after_select_vi_mode() {
                      case $ZVM_MODE in
                        $ZVM_MODE_NORMAL) _nx_keymap=vicmd ;;
                        *) _nx_keymap= ;;
                      esac
                    }

                    # Transient prompt: when a line is accepted, redraw the
                    # prompt as the character only. precmd restores it.
                    _nx_prompt_full=$PROMPT
                    _nx_rprompt_full=$RPROMPT
                    _nx_transient_finish() {
                      PROMPT="$(starship module character --status "''${STARSHIP_CMD_STATUS:-0}")"
                      RPROMPT=
                      zle .reset-prompt
                    }
                    _nx_transient_restore() {
                      PROMPT=$_nx_prompt_full
                      RPROMPT=$_nx_rprompt_full
                    }
                    autoload -Uz add-zle-hook-widget
                    add-zle-hook-widget line-finish _nx_transient_finish
                    precmd_functions+=(_nx_transient_restore)
                  fi
                '';

                # Common init
                commonInit = ''
                  # History beginning search — matches commands that start with what's typed
                  autoload -U history-search-end
                  zle -N history-beginning-search-backward-end history-search-end
                  zle -N history-beginning-search-forward-end history-search-end
                  bindkey '^[[A' history-beginning-search-backward-end
                  bindkey '^[[B' history-beginning-search-forward-end

                  # Custom keybindings
                  bindkey -s ^v "nvim\n"
                  bindkey -s ^o "prism launch --in-terminal --path ~/documents/obsidian\n"

                  _prism_launch() {
                    _PRISM_LAUNCH_PENDING=1
                    zle accept-line
                  }
                  zle -N _prism_launch
                  bindkey ^p _prism_launch

                  _prism_launch_precmd() {
                    if [[ -n "$_PRISM_LAUNCH_PENDING" ]]; then
                      unset _PRISM_LAUNCH_PENDING
                      prism launch --in-terminal
                    fi
                  }
                  precmd_functions+=(_prism_launch_precmd)
                '';

                # Darwin self-heal for the sops GITHUB_TOKEN decrypt race
                # (#2029). hm-session-vars.sh exports
                # GITHUB_TOKEN="$(cat <secret>)" once per shell. A
                # darwin-rebuild switch tears down and re-bootstraps the
                # sops-nix launchd agent, which re-decrypts asynchronously; a
                # shell sourcing hm-session-vars.sh during that window freezes
                # GITHUB_TOKEN="" (empty, not unset). This guard re-reads the
                # secret file when the var is empty and the file is readable,
                # bypassing the source-once cache. It uses the SAME sops secret
                # path as the home.sessionVariables in modules/programs/git.nix
                # so the two stay in lockstep. Linux is unaffected: there the
                # var is set via environment.sessionVariables (normally
                # non-empty), so the guard is a harmless no-op.
                darwinInit = lib.optionalString isDarwin ''
                  if [ -z "$GITHUB_TOKEN" ] && [ -r "${hmConfig.sops.secrets.github_token.path}" ]; then
                    export GITHUB_TOKEN="$(cat ${hmConfig.sops.secrets.github_token.path})"
                    export GITHUB_PACKAGES_TOKEN="$GITHUB_TOKEN"
                  fi
                '';
              in
              lib.mkMerge [
                (lib.mkOrder 899 p10kInit)
                (lib.mkOrder 1000 (commonInit + darwinInit + starshipInit))
              ];
          };

          # NixOS-specific: Impermanence support
          home.persistence."/persist" = {
            directories = [
              ".local/share/zsh"
              ".local/state/zsh"
            ];
          };
        };
      }
    ]
  );
}
