{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.bitwarden.enable = lib.mkEnableOption "enables bitwarden-desktop" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.bitwarden.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        bitwarden-desktop
        bitwarden-cli
      ];
      home.persistence."/persist" = {
        directories = [
          ".config/Bitwarden"
          ".config/Bitwarden CLI"
        ];
      };
      xdg.desktopEntries.bitwarden = lib.mkIf pkgs.stdenv.isLinux {
        name = "Bitwarden";
        comment = "Secure and free password manager for all of your devices";
        icon = "bitwarden";
        # force wayland
        exec = "bitwarden --enable-features=UseOzonePlatform --ozone-platform=wayland";
        mimeType = [ "x-scheme-handler/bitwarden" ];
      };
    };
  };
}
