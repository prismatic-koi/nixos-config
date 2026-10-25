{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
{
  options = {
    nx.programs.zathura.enable = lib.mkEnableOption "enables zathura" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.zathura.enable {
    home-manager.users.${config.nx.username} = {
      programs.zathura = {
        enable = true;
        options = lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
          selection-clipboard = "clipboard";
          default-bg = neutrals.background_dim;
          default-fg = neutrals.foreground;
          statusbar-bg = neutrals.background_1;
          statusbar-fg = neutrals.foreground;
          inputbar-bg = neutrals.background_0;
          inputbar-fg = neutrals.foreground;
          notification-bg = neutrals.background_1;
          notification-fg = neutrals.foreground;
          notification-error-bg = hues.red;
          notification-error-fg = neutrals.background_dim;
          notification-warning-bg = hues.yellow;
          notification-warning-fg = neutrals.background_dim;
          completion-bg = neutrals.background_1;
          completion-fg = neutrals.foreground;
          completion-group-bg = neutrals.background_2;
          completion-group-fg = neutrals.foreground_dim;
          completion-highlight-bg = roles.selection;
          completion-highlight-fg = neutrals.foreground;
          highlight-color = hues.yellow;
          highlight-active-color = hues.orange;
          highlight-fg = neutrals.background_dim;
          index-bg = neutrals.background_0;
          index-fg = neutrals.foreground;
          index-active-bg = roles.selection;
          index-active-fg = neutrals.foreground;
          recolor-lightcolor = neutrals.background_dim;
          recolor-darkcolor = neutrals.foreground;
        };
      };
      # XDG MIME associations are Linux-only
      xdg.mimeApps.defaultApplications = lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
        "application/pdf" = [ "org.pwmt.zathura.desktop" ];
      };
    };
  };
}
