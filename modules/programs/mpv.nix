{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.mpv.enable = lib.mkEnableOption "enables mpv" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.mpv.enable {
    home-manager.users.ben = {
      programs.mpv = {
        enable = true;
        package = pkgs.mpv.override {
          # Disable yt-dlp on macOS due to D-Bus dependencies (jeepney/secretstorage)
          youtubeSupport = pkgs.stdenv.isLinux;
          scripts =
            with pkgs.mpvScripts;
            [
              thumbnail
              sponsorblock
            ]
            ++ lib.optionals pkgs.stdenv.isLinux [ pkgs.mpvScripts.mpris ];
        };
        config = {
          osc = "no";
        };
      };
    };
  };
}
