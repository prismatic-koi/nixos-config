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

      # System-level zsh, both platforms. Keep enableCompletion on so the
      # system completion functions stay in fpath. Turn off the global
      # compinit call and the promptinit/suse prompt. The home-manager
      # completionInit runs compinit one time, and starship sets the prompt.
      {
        programs.zsh = {
          enableCompletion = true;
          enableGlobalCompInit = false;
          promptInit = "";
        };
      }

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
                format = "$os$directory$git_branch$character";
                right_format = lib.concatStrings [
                  "$status$jobs$direnv$python$nodejs$kubernetes"
                  "\${env_var.NX_SHOW_AWS}\${env_var.NX_SHOW_AZURE}$gcloud"
                  "$container$username$hostname$nix_shell"
                ];
                os = {
                  disabled = false;
                  format = "[$symbol]($style)";
                  style = "";
                  symbols = {
                    NixOS = "${glyph "f313"} ";
                    Macos = "${glyph "f179"} ";
                    Linux = "${glyph "f17c"} ";
                  };
                };
                # Starship has no unique-prefix mode. Show the last two
                # folders in full, and shorten each parent folder to one
                # character.
                directory = {
                  style = "blue";
                  truncate_to_repo = false;
                  truncation_length = 2;
                  fish_style_pwd_dir_length = 1;
                  format = "[$path]($style) ";
                };
                git_branch = {
                  symbol = "${glyph "f126"} ";
                  style = "green";
                  format = "[$symbol$branch]($style) ";
                };
                # git_status is disabled (#3411). Its gitoxide status runs
                # the filter drivers that a planted repo config defines, at
                # each prompt. use_git_executable does not stop this,
                # because git status also runs them. Do not enable
                # git_status, git_metrics, or another module that reads the
                # work-tree status, without a security review.
                git_status.disabled = true;
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
                # Show-on-command (#3408): the zle hook in starshipInit sets
                # these variables while the first word of the command line
                # matches showOn. aws and azure have no detect_env_vars, so
                # the hook puts their module output in NX_SHOW_AWS and
                # NX_SHOW_AZURE, and env_var shows it. Do not use a custom
                # module with a `when` command: it starts sh at each prompt.
                kubernetes = {
                  disabled = false;
                  detect_env_vars = [ "NX_SHOW_KUBE" ];
                  style = "5";
                  format = "[$symbol$context( \\($namespace\\))]($style)";
                };
                gcloud.detect_env_vars = [ "NX_SHOW_GCLOUD" ];
                env_var.NX_SHOW_AWS.format = "$env_value";
                env_var.NX_SHOW_AZURE.format = "$env_value";
                azure = {
                  disabled = false;
                  format = "on [$symbol($subscription)]($style)";
                };
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
              ignorePatterns = [
                # Token prefixes
                "*sk-ant-*"
                "*sk-or-v1-*"
                "*glpat-*"
                "*hskey-auth-*"
                "*ghp_*"
                "*gho_*"
                "*github_pat_*"
                # Assignments with a literal value
                "*BW_SESSION=*"
                "*_API_KEY=[^\$\"']*"
                "*_API_KEY=[\"'][^\$]*"
                "*_TOKEN=[^\$\"']*"
                "*_TOKEN=[\"'][^\$]*"
                # Headers and cookies
                "*Bearer [^\$]*"
                "*x-api-key: [^\$]*"
                "*Subscription-Key*"
                "*-b '*=*"
                "*[Cc]ookie: *"
                # Passwords and card data
                "*input_pw*"
                "*mosquitto_passwd*-b*"
                "*\"cvv\"*"
              ];
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
              # nvim
              v = "nvim";
              # youtube music download script
              # yt-dlp does not expand `~` in the chromium profile path, so use the absolute path.
              ytm-download = "yt-dlp ${lib.optionalString config.nx.programs.qutebrowser.enable "--cookies-from-browser chromium:${homeDir}/.local/share/qutebrowser"} --add-metadata --format m4a -i -o '~/music/%(artist)s/%(album)s/%(title)s.%(ext)s' --sponsorblock-remove 'music_offtopic' --";
              # git aliases
              gP = "git pull";
              gcb = "git checkout -b";
            };

            # Set ZVM_INIT_MODE for zsh-vi-mode plugin to not break custom keybindings
            sessionVariables = {
              ZVM_INIT_MODE = "sourcing";
            };

            initContent =
              let
                # Show-on-command lists (#3408): a segment shows only while
                # the first word of the command line matches its list.
                showOn = {
                  kube = "kubectl|helm|kubens|kubectx|oc|istioctl|kogito|k9s|helmfile|flux|fluxctl|stern|kubeseal|skaffold";
                  aws = "aws|awless|pulumi";
                  azure = "az|pulumi";
                  gcloud = "gcloud|gcs|gsutil";
                };

                # Runs after vi-mode. zsh-vi-mode keeps $KEYMAP at "main",
                # so the prompt reads $_nx_keymap, set by the zsh-vi-mode
                # hook.
                starshipInit = ''
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
                  _nx_full_prompt=$PROMPT
                  _nx_full_rprompt=$RPROMPT
                  _nx_transient_finish() {
                    PROMPT="$(starship module character --status "''${STARSHIP_CMD_STATUS:-0}")"
                    RPROMPT=
                    zle .reset-prompt
                  }
                  _nx_transient_restore() {
                    PROMPT=$_nx_full_prompt
                    RPROMPT=$_nx_full_rprompt
                  }
                  # Show-on-command (#3408): match the first word of the
                  # buffer against showOn. Set the variables and render the
                  # prompt again only when the match result changes. A
                  # keystroke that does not change it starts no process.
                  # The aws and azure values hold the module output. With
                  # STARSHIP_SHELL empty, the module does not escape %, so
                  # starship escapes it one time.
                  _nx_showon_sig=
                  _nx_showon_update() {
                    local -a ws=(''${(z)BUFFER})
                    local w=$ws[1] sig=
                    if [[ -n $w ]]; then
                      [[ $w == (${showOn.kube}) ]] && sig+=k
                      [[ $w == (${showOn.aws}) ]] && sig+=a
                      [[ $w == (${showOn.azure}) ]] && sig+=z
                      [[ $w == (${showOn.gcloud}) ]] && sig+=g
                    fi
                    [[ $sig == "$_nx_showon_sig" ]] && return
                    _nx_showon_sig=$sig
                    unset NX_SHOW_KUBE NX_SHOW_AWS NX_SHOW_AZURE NX_SHOW_GCLOUD
                    [[ $sig == *k* ]] && export NX_SHOW_KUBE=1
                    [[ $sig == *a* ]] && export NX_SHOW_AWS="$(STARSHIP_SHELL= starship module aws)"
                    [[ $sig == *z* ]] && export NX_SHOW_AZURE="$(STARSHIP_SHELL= starship module azure)"
                    [[ $sig == *g* ]] && export NX_SHOW_GCLOUD=1
                    zle .reset-prompt
                  }
                  _nx_showon_reset() {
                    _nx_showon_sig=
                    unset NX_SHOW_KUBE NX_SHOW_AWS NX_SHOW_AZURE NX_SHOW_GCLOUD
                  }
                  autoload -Uz add-zle-hook-widget
                  add-zle-hook-widget line-pre-redraw _nx_showon_update
                  add-zle-hook-widget line-finish _nx_transient_finish
                  precmd_functions+=(_nx_showon_reset)
                  precmd_functions+=(_nx_transient_restore)
                '';

                # Common init
                commonInit = ''
                  # Search the full history. `history` lists the last 16 events only.
                  # On Darwin, the oh-my-zsh history plugin defines `alias hs`.
                  # Remove it first, or the definition below is a parse error.
                  unalias hs 2>/dev/null
                  hs() { fc -ln 1 | rg "$@"; }

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
              lib.mkOrder 1000 (commonInit + darwinInit + starshipInit);
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
