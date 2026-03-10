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
        options = lib.mkIf pkgs.stdenv.isLinux {
          selection-clipboard = "clipboard";
          default-bg = bg_dim;
          default-fg = foreground;
        };
      };
      # XDG MIME associations are Linux-only
      xdg.mimeApps.defaultApplications = lib.mkIf pkgs.stdenv.isLinux {
        "application/pdf" = [ "org.pwmt.zathura.desktop" ];
      };
    };
  };
}
