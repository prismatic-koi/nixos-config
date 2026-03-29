{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  isDarwin = pkgs.stdenv.isDarwin;
  isLinux = pkgs.stdenv.isLinux;
  background = if type == "dark" then bg0 else bg_dim;

  kittyconf = ''
    symbol_map U+1f636,U+200D,U+1F32B,U+FE0F Noto Color Emoji
    prefer_color_emoji yes
    hide_window_decorations titlebar-only
    window_margin_width 10
    window_padding_width 0
    confirm_os_window_close 0
    background_opacity ${if isLinux then "0.8" else "1"}
    enable_audio_bell no
    paste_actions no-op
    cursor_trail 1
    cursor_trail_decay 0.05 0.2

    ${lib.optionalString isLinux ''
      # for zen-mode.nvim
      listen_on unix:/tmp/kitty
      allow_remote_control socket-only
    ''}

    ${lib.optionalString isDarwin ''
      # pass these through to Aerospace (they are bound as window manager shortcuts)
      map cmd+n no_op
      map cmd+o no_op
      map cmd+p no_op
    ''}

    foreground                 ${foreground}
    background                 ${background}
    selection_foreground       ${grey2}
    selection_background       ${bg_visual}

    cursor                     ${if isLinux then green else foreground}
    cursor_text_color          ${bg1}

    url_color                  ${blue}

    active_border_color        ${green}
    inactive_border_color      ${bg5}
    bell_border_color          ${orange}
    visual_bell_color          none

    wayland_titlebar_color     system
    macos_titlebar_color       system

    active_tab_background      ${bg0}
    active_tab_foreground      ${foreground}
    inactive_tab_background    ${bg2}
    inactive_tab_foreground    ${grey2}
    tab_bar_background         ${bg1}
    tab_bar_margin_color       none

    mark1_foreground           ${bg0}
    mark1_background           ${blue}
    mark2_foreground           ${bg0}
    mark2_background           ${foreground}
    mark3_foreground           ${bg0}
    mark3_background           ${purple}

    #: black
    color0                     ${bg1}
    color8                     ${bg2}

    #: red
    color1                     ${red}
    color9                     ${red}

    #: green
    color2                     ${green}
    color10                    ${green}

    #: yellow
    color3                     ${yellow}
    color11                    ${yellow}

    #: blue
    color4                     ${blue}
    color12                    ${blue}

    #: magenta
    color5                     ${purple}
    color13                    ${purple}

    #: cyan
    color6                     ${aqua}
    color14                    ${aqua}

    #: white
    color7                     ${grey1}
    color15                    ${grey2}
  '';
in
{
  options = {
    nx.programs.kitty.enable = lib.mkEnableOption "enables kitty" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.kitty.enable {
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
