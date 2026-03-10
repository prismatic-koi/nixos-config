{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  isDarwin = pkgs.stdenv.isDarwin;
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
    home-manager.users.${config.nx.username} = {
      home.packages = [ pkgs.choose-gui ];

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
