{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  isDarwin = pkgs.stdenv.hostPlatform.isDarwin;
  isLinux = pkgs.stdenv.hostPlatform.isLinux;

  inherit (config.nx.colourScheme.visualiserGradient) baseIndex colours;

  # Truecolor visualiser ramp: color16 .. color(baseIndex + len - 1), one
  # line per entry, in order. Shared with modules/programs/ncmpcpp.nix via
  # modules/colour-scheme/gradient.nix — see there for rationale.
  visualiserGradientConf = lib.concatStrings (
    lib.imap0 (
      i: colour: "    color${builtins.toString (baseIndex + i)}                 ${colour}\n"
    ) colours
  );

  kittyconf = ''
    # Disable config auto-reload entirely (issue #2198, #2180-class FD-exhaustion
    # incidents). kitty >= 0.47.1 spawns a `kitten __watch_conf__` watcher that
    # resolves the HM config symlink into /nix/store and, on nixpkgs no-cgo
    # Darwin builds, kqueue-watches the entire store — one FD per store entry,
    # ~115k FDs per kitty instance — exhausting the host FD pool (the same bug
    # manifests as inotify-watch exhaustion on Linux, hence no platform gate).
    # The upstream 0.47.2 fix does NOT help nixpkgs Darwin builds (fswatcher's
    # kqueue backend ignores the depth option), and auto-reload never worked
    # with store-immutable configs anyway. Manual reload: ctrl+shift+f5.
    auto_reload_config -1

    symbol_map U+1f636,U+200D,U+1F32B,U+FE0F Noto Color Emoji
    # nf-cod-* verdict icons (review_summary.go) are Private Use Area with
    # a wide aspect ratio in JetBrainsMono Nerd Font. Force kitty to always
    # spread them across 2 cells rather than deciding per repaint based on
    # trailing spaces (kitty/options/definition.py:92) -- see the comment
    # on renderIconCell for the full mechanism.
    narrow_symbols U+EA60-U+EC1E 2
    prefer_color_emoji yes
    hide_window_decorations titlebar-only
    window_margin_width 10
    window_padding_width 0
    confirm_os_window_close 0
    background_opacity ${if isLinux then "0.95" else "1"}
    enable_audio_bell no
    paste_actions no-op
    cursor_trail 1
    cursor_trail_decay 0.05 0.2

    ${lib.optionalString isLinux ''
      # for zen-mode.nvim
      listen_on unix:/tmp/kitty
      allow_remote_control socket-only
    ''}





    foreground                 ${neutrals.foreground}
    background                 ${neutrals.background_0}
    selection_foreground       ${neutrals.foreground_dim}
    selection_background       ${roles.selection}

    cursor                     ${if isLinux then hues.green else roles.cursor}
    cursor_text_color          ${neutrals.background_1}

    url_color                  ${hues.blue}

    active_border_color        ${hues.green}
    inactive_border_color      ${roles.border}
    bell_border_color          ${hues.orange}
    visual_bell_color          none

    wayland_titlebar_color     system
    macos_titlebar_color       system

    active_tab_background      ${neutrals.background_0}
    active_tab_foreground      ${neutrals.foreground}
    inactive_tab_background    ${neutrals.background_2}
    inactive_tab_foreground    ${neutrals.foreground_dim}
    tab_bar_background         ${neutrals.background_1}
    tab_bar_margin_color       none

    mark1_foreground           ${neutrals.background_0}
    mark1_background           ${hues.blue}
    mark2_foreground           ${neutrals.background_0}
    mark2_background           ${neutrals.foreground}
    mark3_foreground           ${neutrals.background_0}
    mark3_background           ${hues.purple}

    #: black
    color0                     ${neutrals.background_1}
    color8                     ${neutrals.background_2}

    #: red
    color1                     ${hues.red}
    color9                     ${brights.bright_red}

    #: green
    color2                     ${hues.green}
    color10                    ${brights.bright_green}

    #: yellow
    color3                     ${hues.yellow}
    color11                    ${brights.bright_yellow}

    #: blue
    color4                     ${hues.blue}
    color12                    ${brights.bright_blue}

    #: magenta
    color5                     ${hues.purple}
    color13                    ${brights.bright_magenta}

    #: cyan
    color6                     ${hues.cyan}
    color14                    ${brights.bright_cyan}

    #: white
    color7                     ${neutrals.foreground_dim}
    color15                    ${neutrals.foreground}

    #: ncmpcpp visualiser gradient (color16-color39) — see
    #: modules/colour-scheme/gradient.nix for the single shared source of
    #: truth this ramp is generated from.
    ${visualiserGradientConf}
  '';
in
{
  options = {
    nx.programs.kitty.enable = lib.mkEnableOption "enables kitty" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.kitty.enable {
    # Make kitty terminfo available system-wide so the xterm-kitty entry
    # is present in all terminfo search paths (especially important for
    # sudo and root contexts, and for cross-platform coverage).
    environment.systemPackages = [
      pkgs.kitty.terminfo
    ];

    home-manager.users.${config.nx.username} = {
      programs.kitty = {
        enable = true;
        font = {
          name = if isDarwin then "JetBrainsMono Nerd Font Mono Medium" else "JetBrainsMono Nerd Font";
          size = if isDarwin then 14.0 else 12.0;
        };
        extraConfig = kittyconf;
      };
    };
  };
}
