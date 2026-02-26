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

  # For NixOS, we use home-manager.users.ben
  # For darwin with home-manager via nix-darwin, we also use home-manager.users.ben
  hmConfig = config.home-manager.users.ben;

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
        home-manager.users.ben = {
          # On NixOS we set ZDOTDIR at system level, so we don't need .zshenv
          # On Darwin we need .zshenv to bootstrap ZDOTDIR
          home.file.".zshenv".enable = isDarwin;

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
              highlight = lib.mkIf (isLinux && config ? theme) "fg=${config.theme.grey1}";
            };

            syntaxHighlighting.enable = true;
            # zprof.enable = true; # for troubleshooting startup
            plugins = [
              {
                name = "powerlevel10k";
                src = pkgs.zsh-powerlevel10k;
                file = "share/zsh-powerlevel10k/powerlevel10k.zsh-theme";
              }
              {
                name = "powerlevel10k-config";
                src = lib.cleanSource ./files;
                file = "p10k.zsh";
              }
              # Use zsh-vi-mode on all platforms for consistent vi-mode experience
              {
                name = "vi-mode";
                src = pkgs.zsh-vi-mode;
                file = "share/zsh-vi-mode/zsh-vi-mode.plugin.zsh";
              }
              {
                name = "zsh-history-substring-search";
                src = pkgs.zsh-history-substring-search;
                file = "share/zsh-history-substring-search/zsh-history-substring-search.zsh";
              }
            ];

            # Darwin still uses oh-my-zsh for git plugins
            oh-my-zsh = lib.mkIf isDarwin {
              enable = true;
              plugins = [
                "git"
                "git-auto-fetch"
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
                # Common init
                commonInit = ''
                  # zsh-history-substring-search configuration
                  bindkey '^[[A' history-substring-search-up # or '\eOA'
                  bindkey '^[[B' history-substring-search-down # or '\eOB'
                  HISTORY_SUBSTRING_SEARCH_ENSURE_UNIQUE=1

                  # Custom keybindings
                  bindkey -s ^v "nvim\n"
                  bindkey -s ^o "cli.prism.launch --in-terminal --path ~/documents/obsidian\n"

                  _prism_launch() {
                    _PRISM_LAUNCH_PENDING=1
                    zle accept-line
                  }
                  zle -N _prism_launch
                  bindkey ^p _prism_launch

                  _prism_launch_precmd() {
                    if [[ -n "$_PRISM_LAUNCH_PENDING" ]]; then
                      unset _PRISM_LAUNCH_PENDING
                      cli.prism.launch --in-terminal
                    fi
                  }
                  precmd_functions+=(_prism_launch_precmd)
                '';
              in
              lib.mkOrder 1000 commonInit;
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
