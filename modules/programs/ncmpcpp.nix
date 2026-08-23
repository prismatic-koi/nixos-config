{
  config,
  pkgs,
  lib,
  ...
}:
let
  inherit (config.nx.colourScheme.visualiserGradient) baseIndex colours;

  # ncmpcpp's visualizer_color list value is the palette index + 1 (off by
  # one). The colour list and base index come from
  # modules/colour-scheme/gradient.nix — the single shared source of truth
  # also read by modules/programs/kitty.nix.
  visualizerColor = lib.concatMapStringsSep "," (i: builtins.toString (baseIndex + i + 1)) (
    lib.range 0 (builtins.length colours - 1)
  );
in
{
  options = {
    nx.programs.ncmpcpp.enable = lib.mkEnableOption "enables ncmpcpp" // {
      default = true;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.programs.ncmpcpp.enable
        # no point in installing if mpd is not
        && config.nx.services.mpd.enable
        # mpd service only works on Linux
        && pkgs.stdenv.hostPlatform.isLinux
      )
      {
        home-manager.users.${config.nx.username} = {
          programs.ncmpcpp = {
            enable = true;
            package = pkgs.ncmpcpp.override { visualizerSupport = true; };
            mpdMusicDir = config.home-manager.users.${config.nx.username}.services.mpd.musicDirectory;
            settings = {
              display_bitrate = "yes";
              user_interface = "alternative";
              visualizer_output_name = "my_fifo";
              visualizer_in_stereo = "yes";
              visualizer_type = "spectrum";
              visualizer_spectrum_smooth_look = "yes";
              visualizer_color = visualizerColor;
              main_window_color = 5;
              color1 = 3;
              color2 = 2;
              statusbar_color = 7;
              empty_tag_color = 7;
              playlist_display_mode = "classic";
              song_list_format = "%t $R %a   %l";
              now_playing_prefix = "$(green)$b";
              now_playing_suffix = "$/b$(end)";
              current_item_prefix = "$(green)$r";
              current_item_suffix = "$/r$(end)";
              current_item_inactive_column_prefix = "$5$r";
              media_library_primary_tag = "album_artist";
              # I don't use this, but i don't want it in my home directory
              lyrics_directory = "~/.config/ncmpcpp/lyrics";
            };
            bindings = [
              {
                key = "l";
                command = "next_column";
              }
              {
                key = "h";
                command = "previous_column";
              }
              {
                key = "k";
                command = "scroll_up";
              }
              {
                key = "j";
                command = "scroll_down";
              }
              {
                key = "G";
                command = "move_end";
              }
              {
                key = "g";
                command = "move_home";
              }
              {
                key = "ctrl-u";
                command = "page_up";
              }
              {
                key = "ctrl-d";
                command = "page_down";
              }
              {
                key = "n";
                command = "next_found_item";
              }
              {
                key = "N";
                command = "previous_found_item";
              }
              {
                key = "+";
                command = "show_clock";
              }
              {
                key = "=";
                command = "volume_up";
              }
              {
                key = "-";
                command = "volume_down";
              }
              {
                key = "d";
                command = "delete_playlist_items";
              }
              {
                key = "v";
                command = "show_visualizer";
              }
              {
                key = "f";
                command = "change_browse_mode";
              }
              {
                key = "m";
                command = "show_media_library";
              }
              {
                key = "u";
                command = "update_database";
              }
              # unbinds
              {
                key = "left";
                command = "dummy";
              }
              {
                key = "right";
                command = "dummy";
              }
              {
                key = "x";
                command = "dummy";
              }
            ];
          };
        };
      };
}
