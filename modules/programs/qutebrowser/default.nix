{
  config,
  pkgs,
  lib,
  ...
}:
{
  imports = [
    ./clear-history-task.nix
    ./colours.nix
    ./greasemonkey.nix
    ./keybindings.nix
    ./secrets.nix
  ];
  options = {
    nx.programs.qutebrowser.enable = lib.mkEnableOption "enables qutebrowser" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.qutebrowser.enable (
    let
      isLinux = pkgs.stdenv.isLinux;
      isDarwin = pkgs.stdenv.isDarwin;
      homeDir = config.home-manager.users.ben.home.homeDirectory;

      qutebrowser-setup = pkgs.writeShellScript "qutebrowser-setup" (
        builtins.concatStringsSep "\n" (
          builtins.map (
            n:
            "${config.home-manager.users.ben.programs.qutebrowser.package}/share/qutebrowser/scripts/dictcli.py install ${n}"
          ) config.home-manager.users.ben.programs.qutebrowser.settings.spellcheck.languages
        )
      );

      # macOS-specific helper scripts
      macos-dmenu = pkgs.writeShellScriptBin "macos-dmenu" ''
        # macOS dmenu replacement using choose-gui (native GUI, keyboard-friendly)
        # choose-gui reads from stdin and outputs selection to stdout, just like dmenu
        ${pkgs.choose-gui}/bin/choose -f 'JetbrainsMono Nerd Font' -c '${
          builtins.substring 1 6 config.theme.green
        }' -b '${builtins.substring 1 6 config.theme.bg2}' -s 20
      '';

      macos-password-prompt = pkgs.writeShellScriptBin "macos-password-prompt" ''
        # macOS password prompt using osascript

        # Extract the prompt from arguments (look for -p flag)
        prompt="Enter Password"
        while [[ $# -gt 0 ]]; do
            case $1 in
                -p)
                    prompt="$2"
                    shift 2
                    ;;
                *)
                    shift
                    ;;
            esac
        done

        # Show password dialog
        /usr/bin/osascript -e "text returned of (display dialog \"$prompt\" default answer \"\" with hidden answer)" 2>/dev/null
      '';

      # Unwrapped bitwarden userscript (the actual Python script)
      bitwarden-userscript-unwrapped = pkgs.writers.writePython3Bin "bitwarden-unwrapped" {
        flakeIgnore = [
          "E126"
          "E302"
          "E501"
          "E402"
          "E265"
          "W503"
          "W293"
        ];
        libraries = with pkgs.python313Packages; [
          tldextract
          pyperclip
        ];
      } (builtins.readFile ./userscripts/bitwarden);

      # Platform-aware wrapper for bitwarden userscript
      bitwarden-userscript = pkgs.writeShellScriptBin "bitwarden" (
        if isDarwin then
          ''
            # Get the directory where this script is located (the userscripts directory)
            SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

            # Add bitwarden-cli to PATH
            export PATH="${pkgs.bitwarden-cli}/bin:$PATH"

            # Default to macOS-specific tools if not overridden
            # Escape spaces in path for shlex.split() compatibility
            SCRIPT_DIR_ESCAPED="''${SCRIPT_DIR// /\\ }"
            DMENU_DEFAULT="$SCRIPT_DIR_ESCAPED/macos-dmenu -p Bitwarden"
            PASSWORD_PROMPT_DEFAULT="$SCRIPT_DIR_ESCAPED/macos-password-prompt -p Master_Password"

            # Load Bitwarden password from sops secret if available and not already set
            if [ -z "$BITWARDEN_PASSWORD" ]; then
              BW_SECRET_PATH="${config.home-manager.users.ben.sops.secrets.bitwarden_password.path}"
              if [ -f "$BW_SECRET_PATH" ]; then
                export BITWARDEN_PASSWORD="$(cat "$BW_SECRET_PATH")"
              fi
            fi

            # Build arguments array
            ARGS=()
            HAS_DMENU=0
            HAS_PASSWORD=0

            while [[ $# -gt 0 ]]; do
              case "$1" in
                --dmenu-invocation|-d)
                  HAS_DMENU=1
                  ARGS+=("$1" "$2")
                  shift 2
                  ;;
                --password-prompt-invocation|-p)
                  HAS_PASSWORD=1
                  ARGS+=("$1" "$2")
                  shift 2
                  ;;
                *)
                  ARGS+=("$1")
                  shift
                  ;;
              esac
            done

            # Add defaults if not provided
            if [ $HAS_DMENU -eq 0 ]; then
              ARGS+=("--dmenu-invocation" "$DMENU_DEFAULT")
            fi
            if [ $HAS_PASSWORD -eq 0 ]; then
              ARGS+=("--password-prompt-invocation" "$PASSWORD_PROMPT_DEFAULT")
            fi

            exec ${bitwarden-userscript-unwrapped}/bin/bitwarden-unwrapped "''${ARGS[@]}"
          ''
        else
          ''
            # Linux: use rofi as default dmenu
            export PATH="${pkgs.bitwarden-cli}/bin:$PATH"

            # Build arguments array
            ARGS=()
            HAS_DMENU=0

            while [[ $# -gt 0 ]]; do
              case "$1" in
                --dmenu-invocation|-d)
                  HAS_DMENU=1
                  ARGS+=("$1" "$2")
                  shift 2
                  ;;
                *)
                  ARGS+=("$1")
                  shift
                  ;;
              esac
            done

            # Add rofi default if not provided
            if [ $HAS_DMENU -eq 0 ]; then
              ARGS+=("--dmenu-invocation" "rofi -dmenu -i -p Bitwarden")
            fi

            exec ${bitwarden-userscript-unwrapped}/bin/bitwarden-unwrapped "''${ARGS[@]}"
          ''
      );

      bitwarden-prefetch = pkgs.writers.writePython3Bin "bitwarden-prefetch" {
        flakeIgnore = [
          "E501"
          "E265"
          "E302"
          "W292"
        ];
        libraries = with pkgs.python313Packages; [ ];
      } (builtins.readFile ./userscripts/bitwarden-prefetch);
    in
    lib.mkMerge [
      # Common configuration for both platforms
      {
        home-manager.users.ben = {
          programs.qutebrowser = {
            enable = true;
            settings = {
              url = {
                start_pages = [
                  "qute://start"
                ];
                default_page = "qute://start";
                open_base_url = true; # open base url when a search engine is used without a query
              };
              downloads = {
                location = {
                  directory = "~/downloads";
                  prompt = false;
                };
              };
              content = {
                # disable autoplay globally
                autoplay = false;
                geolocation = false;
                # turn off notifications
                notifications = {
                  enabled = false;
                };
                javascript = {
                  clipboard = "access";
                };
                tls = {
                  certificate_errors = "ask-block-thirdparty";
                };
                # don't register handlers for things like mail and calendar
                register_protocol_handler = false;
              };
              confirm_quit = [
                "downloads"
              ];
              completion.open_categories = [
                "searchengines"
                "quickmarks"
                "bookmarks"
                "history"
              ];
              scrolling = {
                bar = "when-searching";
              };
              spellcheck = {
                languages = [
                  "en-AU"
                ];
              };
              editor.command = [
                "${pkgs.kitty}/bin/kitty"
                "--class"
                "qute-editor"
                "-e"
                "nvim"
                "{}"
              ];
              fonts = {
                default_family = "Noto Sans Medium";
                default_size = "12pt";
              };
              # false beacuse it causes a weird colour shift
              # https://github.com/qutebrowser/qutebrowser/issues/5528
              window.hide_decoration = false;
              tabs = {
                position = "top";
                # only show if there are multiple tabs
                show = "multiple";
                favicons = {
                  scale = 1;
                };
                # close window if last tab is closed
                last_close = "close";
              };
              hints.radius = 0; # no rounded corners on hints
              fileselect = {
                handler = "external";
                single_file.command = [
                  "${pkgs.kitty}/bin/kitty"
                  "--class"
                  "qute-filepicker"
                  "-e"
                  "${pkgs.lf}/bin/lf"
                  "-selection-path"
                  "{}"
                ];
                multiple_files.command = [
                  "${pkgs.kitty}/bin/kitty"
                  "--class"
                  "qute-filepicker"
                  "-e"
                  "${pkgs.lf}/bin/lf"
                  "-selection-path"
                  "{}"
                ];
              };
            };
            extraConfig =
              # python
              ''
                # enable geolocation on some sites
                config.set("content.geolocation", True, "https://www.bunnings.co.nz")
                config.set("content.geolocation", True, "https://www.metlink.org.nz")
                config.set("content.geolocation", True, "https://www.newworld.co.nz")
                config.set("content.geolocation", True, "https://www.pbtech.co.nz")
                # tab padding
                c.tabs.padding = {
                    "bottom": 5,
                    "left": 5,
                    "top": 5,
                    "right": 5,
                }
              ''
              + lib.optionalString isLinux ''

                # prefetch bitwarden cache on startup (synchronous to ensure cache is ready)
                import subprocess
                import os
                import threading
                import time


                def prefetch_with_timeout():
                    try:
                        session_file = os.path.join(
                            os.getenv("XDG_RUNTIME_DIR", "/tmp"), "bw_session_key"
                        )
                        if os.path.exists(session_file):
                            # Run prefetch with a reasonable timeout
                            result = subprocess.run(
                                ["${bitwarden-prefetch}/bin/bitwarden-prefetch"],
                                timeout=10,  # 10 second timeout
                                capture_output=True,
                            )
                    except:
                        pass  # silently ignore errors including timeouts


                # Run prefetch in background thread to avoid blocking qutebrowser startup
                # but with a timeout to ensure it completes reasonably quickly
                try:
                    prefetch_thread = threading.Thread(target=prefetch_with_timeout, daemon=True)
                    prefetch_thread.start()
                    # Give it a moment to start, but don't block qutebrowser startup entirely
                    time.sleep(0.5)
                except:
                    pass
              '';
            quickmarks = {
              "fm" = "messenger.com";
              "gc" = "calendar.google.com";
              "gh" = "github.com";
              "gm" = "maps.google.com";
              "gp" = "photos.google.com";
              "ww" = "https://www.metservice.com/towns-cities/locations/wellington";
              "yt" = "youtube.com";
            };
            searchEngines = {
              "DEFAULT" = "https://search.tinfoilforest.nz/search?q={}";
              "ax" = "https://www.aliexpress.com/wholesale?SearchText={}";
              "gg" = "https://google.com/search?hl=en&q={}";
              "gm" = "https://www.google.com/maps/search/{}";
              "gh" = "https://github.com/search?q={}";
              "ghnx" = "https://github.com/search?q={}+language%3ANix&type=code&l=Nix";
              "goodreads" = "https://www.goodreads.com/search?q={}";
              "hm" = "https://home-manager-options.extranix.com/?query={}";
              "nixopt" =
                "https://search.nixos.org/options?channel=unstable&from=0&size=50&sort=relevance&type=packages&query={}";
              "nixpkgs" =
                "https://search.nixos.org/packages?channel=unstable&from=0&size=50&sort=relevance&type=packages&query={}";
              "nw" = "https://www.newworld.co.nz/shop/search?q={}";
              "protondb" = "https://www.protondb.com/search?q={}";
              "reo" =
                "https://maoridictionary.co.nz/search?idiom=&phrase=&proverb=&loan=&histLoanWords=&keywords={}";
              "sx" = "https://search.tinfoilforest.nz/search?q={}";
              "wk" = "https://en.wikipedia.org/w/index.php?search={}&title=Special%3ASearch&ns0=1";
              "yt" = "https://www.youtube.com/results?search_query={}";
            };
          };
        };
      }

      # Linux-specific configuration
      (lib.mkIf isLinux {
        home-manager.users.ben = {
          # systemd unit to prefetch bitwarden cache
          systemd.user.services.bitwarden-prefetch = {
            Unit = {
              Description = "Prefetch Bitwarden vault cache for faster qutebrowser access";
              After = [ "network-online.target" ];
              Wants = [ "network-online.target" ];
            };
            Service = {
              Type = "oneshot";
              # Use a wrapper script to properly pass the password from the secret file
              ExecStart = pkgs.writeShellScript "bitwarden-prefetch-wrapper" ''
                export PATH="${pkgs.bitwarden-cli}/bin:$PATH"
                export BITWARDEN_PASSWORD="$(cat ${config.sops.secrets.bitwarden_password.path})"
                exec ${bitwarden-prefetch}/bin/bitwarden-prefetch
              '';
              # Allow the service to exit successfully even when no session exists
              # This prevents the timer from showing as failed when BW is not unlocked
              SuccessExitStatus = "0 1";
            };
            Install = {
              WantedBy = [ "default.target" ];
            };
          };

          # systemd timer to refresh bitwarden cache periodically
          systemd.user.timers.bitwarden-prefetch = {
            Unit = {
              Description = "Timer for Bitwarden vault cache refresh";
              Requires = [ "bitwarden-prefetch.service" ];
            };
            Timer = {
              OnBootSec = "5min"; # Run 5 minutes after boot
              OnUnitActiveSec = "30min"; # Then every 30 minutes
              Unit = "bitwarden-prefetch.service";
            };
            Install = {
              WantedBy = [ "timers.target" ];
            };
          };

          # systemd unit to fetch qutebrowser dicts
          systemd.user.services.qutebrowser-setup = {
            Unit = {
              Description = "Fetch qutebrowser dicts for my languages";
              After = [ "network-online.target" ];
              Wants = [ "network-online.target" ];
            };
            Service = {
              Type = "oneshot";
              ExecStart = qutebrowser-setup;
              Restart = "on-failure";
              RestartSec = "30s";
            };
            Install = {
              WantedBy = [ "default.target" ];
            };
          };

          # Install userscripts via xdg.dataFile
          xdg.dataFile."qutebrowser/userscripts/bitwarden" = {
            source = "${bitwarden-userscript}/bin/bitwarden";
          };
          xdg.dataFile."qutebrowser/userscripts/open-firefox" = {
            source = ./userscripts/open-firefox;
          };
          xdg.dataFile."qutebrowser/userscripts/open-bitwarden" = {
            source = ./userscripts/open-bitwarden;
          };
          xdg.dataFile."qutebrowser/userscripts/ytm-download" = {
            source = ./userscripts/ytm-download;
          };

          # Impermanence
          home.persistence."/persist" = {
            directories = [
              ".local/share/qutebrowser"
            ];
          };

          # MIME associations
          xdg.mimeApps.defaultApplications =
            lib.mkIf (config.nx.programs.defaultWebBrowser == "qutebrowser")
              {
                "text/html" = [ "org.qutebrowser.qutebrowser.desktop" ];
                "text/xml" = [ "org.qutebrowser.qutebrowser.desktop" ];
                "x-scheme-handler/http" = [ "org.qutebrowser.qutebrowser.desktop" ];
                "x-scheme-handler/https" = [ "org.qutebrowser.qutebrowser.desktop" ];
              };

          # Hyprland window rules
          wayland.windowManager.hyprland.settings =
            lib.mkIf (config.home-manager.users.ben.wayland.windowManager.hyprland.enable)
              {
                windowrule = [
                  # floating filepickers and editors
                  "float on, match:class qute-filepicker"
                  "size 800 480, match:class qute-filepicker"
                  "stay_focused on, match:class qute-filepicker"
                  "float on, match:class qute-editor"
                  "size 800 480, match:class qute-editor"
                  "stay_focused on, match:class qute-editor"
                  # fake fullscreen, good for youtube etc
                  "sync_fullscreen 0, match:class org.qutebrowser.qutebrowser"
                ];
              };
        };
      })

      # Darwin-specific configuration
      (lib.mkIf isDarwin {
        home-manager.users.ben = {
          # macOS launchd service to prefetch bitwarden cache periodically
          launchd.agents.bitwarden-prefetch = {
            enable = true;
            config = {
              ProgramArguments = [
                "${pkgs.writeShellScript "bitwarden-prefetch-wrapper" ''
                  export PATH="${pkgs.bitwarden-cli}/bin:$PATH"
                  export BITWARDEN_PASSWORD="$(cat ${config.home-manager.users.ben.sops.secrets.bitwarden_password.path})"
                  exec ${bitwarden-prefetch}/bin/bitwarden-prefetch
                ''}"
              ];
              StartInterval = 1800; # Run every 30 minutes (1800 seconds)
              RunAtLoad = true; # Run on login
              StandardOutPath = "/tmp/bitwarden-prefetch.log";
              StandardErrorPath = "/tmp/bitwarden-prefetch.error.log";
            };
          };

          # Install userscripts via home.file
          home.file."Library/Application Support/qutebrowser/userscripts/bitwarden" = {
            source = "${bitwarden-userscript}/bin/bitwarden";
          };
          home.file."Library/Application Support/qutebrowser/userscripts/macos-dmenu" = {
            source = "${macos-dmenu}/bin/macos-dmenu";
          };
          home.file."Library/Application Support/qutebrowser/userscripts/macos-password-prompt" = {
            source = "${macos-password-prompt}/bin/macos-password-prompt";
          };
          home.file."Library/Application Support/qutebrowser/userscripts/open-firefox" = {
            source = ./userscripts/open-firefox;
          };
          home.file."Library/Application Support/qutebrowser/userscripts/open-bitwarden" = {
            source = ./userscripts/open-bitwarden;
          };
          home.file."Library/Application Support/qutebrowser/userscripts/ytm-download" = {
            source = ./userscripts/ytm-download;
          };

          # Add bitwarden-cli to packages so it's available globally
          home.packages = [
            pkgs.bitwarden-cli
          ];
        };
      })
    ]
  );
}
