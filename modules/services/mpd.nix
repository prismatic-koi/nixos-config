{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.services.mpd.enable = lib.mkEnableOption "enables mpd service" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.services.mpd.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        mpc # for commandline control
      ];
      services.mpd = {
        enable = true;
        musicDirectory = "/home/${config.nx.username}/music";
        network.listenAddress = "0.0.0.0";
        extraConfig = ''
          db_file            "~/.local/share/mpd/database"
          log_file           "syslog"
          auto_update "yes"
          playlist_directory "~/music/playlists"
          state_file         "~/.local/share/mpd/state"
          sticker_file       "~/.local/share/mpd/sticker.sql"
          restore_paused "yes"
          port "6600"
          audio_output {
                  type            "pulse"
                  name            "pulse audio"
          }
          audio_output {
              type                    "fifo"
              name                    "my_fifo"
              path                    "/tmp/mpd.fifo"
              format                  "44100:16:2"
          }
        '';
      };
      # needed for playerctl to access mpd
      services.mpdris2 = {
        enable = true;
        mpd.host = "0.0.0.0";
      };
      home.persistence."/persist" = {
        directories = [
          ".local/share/mpd"
        ];
      };
      # turn on the radio
      home.file.".local/scripts/ambient.play.radioactivefm" = {
        executable = true;
        text =
          # sh
          ''
            #!/bin/sh
            mpc clear
            mpc insert http://radio123-gecko.radioca.st/radioactivefm
            mpc next
            mpc play
          '';
      };
      # play rain sounds
      home.file.".local/scripts/ambient.play.rain" = {
        executable = true;
        text =
          # sh
          ''
            #!/bin/sh
            mpc clear
            mpc insert NA/NA/rain.m4a
            mpc next
            mpc play
          '';
      };
    };
  };
}
