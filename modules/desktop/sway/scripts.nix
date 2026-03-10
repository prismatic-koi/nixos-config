{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.sway.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username}.home = {
      # my scripts relevant to sway
      sessionPath = [ "$HOME/.local/scripts" ];
      file.".local/scripts/cli.system.setSwayGaps" = {
        executable = true;
        text = ''
          #!/bin/sh
          width=$(swaymsg -t get_outputs | jq '.[0].rect.width' | xargs printf "%.0f\n")
          # Check if running in super-ultrawide
          if [ $width -gt 5000 ]; then
            swaymsg smart_gaps inverse_outer
            swaymsg gaps horizontal $(($width/4))
            swaymsg gaps horizontal all set $(($width/4))
          else
            swaymsg smart_gaps off
            swaymsg gaps horizontal 0
            swaymsg gaps horizontal all set 0
          fi
        '';
      };
    };
  };
}
