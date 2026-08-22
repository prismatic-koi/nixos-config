{
  config,
  pkgs,
  lib,
  ...
}:
let
  colourLib = import ../colour-scheme/lib.nix;
  inherit (colourLib) mix nearestXterm256;

  # Gradient anchors for the visualiser ramp, taken from config.themev2.hues.
  # Bottom-to-top reads cool-to-warm (blue..red), re-deriving the classic
  # six-colour ncmpcpp default from the active scheme instead of hard-coded
  # ANSI names.
  visualiserAnchors = with config.themev2.hues; [
    blue
    cyan
    green
    yellow
    orange
    red
  ];

  # Number of gradient bands/entries in visualizer_color. Upstream's sample
  # config uses 12; 18 gives a visibly finer ramp across the 5 anchor
  # segments above.
  visualiserSteps = 18;

  # Interpolate `n` evenly spaced colours along a piecewise-linear ramp
  # through `anchors` (a non-empty list of "#RRGGBB" strings).
  rampColors =
    anchors: n:
    let
      segments = builtins.length anchors - 1;
      colorAt =
        i:
        let
          t = i * segments / (n - 1.0);
          seg = if i == n - 1 then segments - 1 else builtins.floor t;
          localPct = builtins.floor ((t - seg) * 100);
        in
        mix (builtins.elemAt anchors seg) (builtins.elemAt anchors (seg + 1)) localPct;
    in
    map colorAt (lib.range 0 (n - 1));

  visualizerColor = lib.concatMapStringsSep "," (
    color: builtins.toString (nearestXterm256 color + 1)
  ) (rampColors visualiserAnchors visualiserSteps);
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
