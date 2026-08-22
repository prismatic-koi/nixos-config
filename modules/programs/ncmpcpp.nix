{
  config,
  pkgs,
  lib,
  ...
}:
let
  colourLib = import ../colour-scheme/lib.nix;
  inherit (colourLib) mix nearestXterm256;

  # Gradient hue order for the visualiser ramp: all 18 Tailwind-inspired
  # config.themev2.hues slots, taken directly (one hue per band, no
  # interpolation between a handful of anchors).
  #
  # A small "sensible-looking" anchor list (e.g. blue/cyan/green/yellow/
  # orange/red) is unsafe: many hue slots are darken/lighten DERIVATIONS of
  # a shared upstream colour rather than perceptually independent hues (e.g.
  # everforest's cyan = lighten blue 12, edge's blue = darken sky 12). Such a
  # list can land entirely on derived slots for a given scheme and collapse
  # to a handful of near-duplicate xterm-256 indices after quantisation (as
  # low as 5 distinct bands out of 18, measured on everforest) — fewer bands
  # than the six-name ncmpcpp default this replaces.
  #
  # The order below is NOT the natural rainbow sequence. It interleaves hues
  # that are frequently derived from one another in the scheme files
  # (emerald/teal/cyan, indigo/violet/purple, orange/amber, rose/brown) so
  # they land far apart in the sequence. Combined with the monotonic
  # luminance climb below, that spread pushes same-cluster hues onto
  # different points of the climb, which reliably breaks the quantisation
  # ties a naive rainbow order leaves in place. Chosen and verified
  # empirically against all seven scheme files (eight variants, counting
  # gruvbox's light and dark) — see the distinct-band table in the PR
  # description — not by reasoning about which anchors "should" work.
  visualiserHueOrder = [
    "red"
    "teal"
    "fuchsia"
    "yellow"
    "blue"
    "brown"
    "emerald"
    "purple"
    "amber"
    "sky"
    "rose"
    "green"
    "violet"
    "orange"
    "cyan"
    "pink"
    "lime"
    "indigo"
  ];

  visualiserSteps = builtins.length visualiserHueOrder; # 18

  # Secondary luminance axis: darken the low (bottom) end of the ramp and
  # lighten the high (top) end, mixing straight toward black/white rather
  # than the multiplicative darken/lighten so the shift behaves predictably
  # on both very dark and very light schemes. +-30% was the smallest
  # magnitude tried that cleared >=14 distinct bands of 18 on every scheme
  # file.
  visualiserLuminanceMagnitude = 30;

  rampColors =
    hues: hueOrder: n:
    map (
      i:
      let
        name = builtins.elemAt hueOrder i;
        base = hues.${name};
        pct = ((i / (n - 1.0)) * 2 - 1) * visualiserLuminanceMagnitude;
      in
      if pct < 0 then
        mix base "#000000" (builtins.floor (-pct))
      else
        mix base "#ffffff" (builtins.floor pct)
    ) (lib.range 0 (n - 1));

  visualizerColor = lib.concatMapStringsSep "," (
    color: builtins.toString (nearestXterm256 color + 1)
  ) (rampColors config.themev2.hues visualiserHueOrder visualiserSteps);
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
