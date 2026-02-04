{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  isDarwin = pkgs.stdenv.isDarwin;
  terminal = "${pkgs.kitty}/bin/kitty";
  choose-options = "-f 'JetBrainsMono Nerd Font' -c '${builtins.substring 1 6 green}' -b '${builtins.substring 1 6 bg2}' -s 20";
in
{
  options = {
    nx.programs.choose = {
      enable = lib.mkEnableOption "choose-gui launcher for Darwin" // {
        default = false; # Will be auto-enabled by prism conditionally
      };
    };
  };

  config = lib.mkIf (config.nx.programs.choose.enable && isDarwin) {
    # Assertions
    assertions = [
      {
        assertion = config.nx.programs.prism.sessioniser.enable;
        message = "nx.programs.choose requires nx.programs.prism.sessioniser to be enabled";
      }
    ];

    home-manager.users.ben = {
      home.packages = [ pkgs.choose-gui ];

      # Session launcher script
      home.file.".local/scripts/application.nvim.sessionLauncher" = {
        executable = true;
        text = ''
          #!/bin/zsh
          # Workaround for Kitty focus grip issue with choose-gui (AeroSpace issue #754)
          # Launch choose with a helper to force activation after window appears

          # Start choose in background with PID capture
          (
            # Give choose a moment to spawn its window
            sleep 0.1
            # Force choose to front - try multiple methods
            osascript -e 'tell application "System Events" to set frontmost of first application process whose name is "choose" to true' 2>/dev/null
            # Fallback: try activating by finding the choose window
            osascript -e 'tell application "System Events" to perform action "AXRaise" of (first window of (first application process whose name is "choose"))' 2>/dev/null
          ) &
          focus_helper_pid=$!

          # Run choose normally (blocking, outputs to stdout)
          selection=$(~/.local/scripts/cli.tmux.projectGetter | ${pkgs.choose-gui}/bin/choose ${choose-options})

          # Clean up background helper
          kill $focus_helper_pid 2>/dev/null
          wait $focus_helper_pid 2>/dev/null

          if [ -n "$selection" ]; then
            # Run the tmux sessioniser with the selected session
            xargs -I{} ${terminal} ~/.local/scripts/cli.tmux.projectSessioniser "{}" 2> /dev/null <<<"$selection"
          fi
        '';
      };

      # Scripts launcher
      home.file.".local/scripts/application.scripts.launcher" = {
        executable = true;
        text = ''
          #!/bin/zsh
          scripts=$(ls ~/.local/scripts)
          scripts=$(echo "$scripts" | grep -v '^cli.')
          selection=$(echo "$scripts" | ${pkgs.choose-gui}/bin/choose ${choose-options})
          echo $selection
          if [ -n "$selection" ]; then
            /bin/bash -c ~/.local/scripts/$selection
          fi
        '';
      };
    };
  };
}
