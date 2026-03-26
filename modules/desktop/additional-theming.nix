{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf pkgs.stdenv.isLinux {
    home-manager.users.${config.nx.username} = {
      # set up the cusors and icon themes the way I like it
      home.pointerCursor = {
        gtk.enable = true;
        name = "breeze_cursors";
        # this is the cursor I like, from plasma 6
        package = pkgs.kdePackages.breeze;
        size = 24;
      };
      gtk = {
        enable = true;
        iconTheme = {
          name = "Papirus-Dark";
          package = pkgs.papirus-icon-theme;
        };
        cursorTheme = {
          name = "breeze_cursors";
          # this is the cursor I like, from plasma 6
          package = pkgs.kdePackages.breeze;
        };
        gtk2 = {
          # not in home dir please
          configLocation = "${config.home-manager.users.${config.nx.username}.xdg.configHome}/gtk-2.0/gtkrc";
        };
        gtk3 = lib.mkIf (config.theme.type == "dark") {
          extraConfig.gtk-application-prefer-dark-theme = true;
        };
        gtk4.theme = null;
      };
      qt = {
        enable = true;
        platformTheme.name = "adwaita";
        style =
          if config.theme.type == "dark" then
            {
              name = "adwaita-dark";
              package = pkgs.adwaita-qt;
            }
          else
            {
              name = "adwaita";
              package = pkgs.adwaita-qt;
            };
      };
    };
  };
}
