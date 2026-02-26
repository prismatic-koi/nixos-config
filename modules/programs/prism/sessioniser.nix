{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.sessioniser.enable = lib.mkEnableOption "enables tmux sessioniser" // {
      default = true;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.programs.prism.sessioniser.enable
        # no point in installing if tmux is not
        && config.nx.programs.prism.tmux.enable
      )
      {
        home-manager.users.ben = {
          # making sure scripts are on path if not set elsewhere
          home.sessionPath = [ "$HOME/.local/scripts" ];

          # This script returns a \n separated list of locations for tmuxSessioniser to open
          # they could be directories or 'locations' where every sub folder is available
          home.file.".local/scripts/cli.tmux.projectGetter" = {
            executable = true;
            text =
              # bash
              ''
                #!/bin/sh

                locations=(
                  "~/code"
                  # "~/.config"
                )
                specific_folders=(
                  # "~/.local/scripts/"
                  # "~/.ssh/"
                  "~/documents/obsidian/"
                )

                for location in "''${locations[@]}"; do
                  find "$(eval echo $location)" -mindepth 1 -maxdepth 1 -type d
                done
                for folder in "''${specific_folders[@]}"; do
                  find "$(eval echo $folder)" -mindepth 0 -maxdepth 0 -type d
                done
              '';
          };
        };
      };
}
