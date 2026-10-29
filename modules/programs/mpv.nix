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
    home-manager.users.${config.nx.username} = {
      programs.mpv = {
        enable = true;
        package = pkgs.mpv.override {
          # Disable yt-dlp on macOS due to D-Bus dependencies (jeepney/secretstorage)
          youtubeSupport = pkgs.stdenv.hostPlatform.isLinux;
          scripts =
            with pkgs.mpvScripts;
            [
              thumbnail
              sponsorblock
            ]
            ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.mpvScripts.mpris ];
        };
        config = {
          osc = "no";
        };
      };
      xdg.mimeApps.defaultApplications = lib.mkIf pkgs.stdenv.hostPlatform.isLinux (
        lib.genAttrs [
          # Canonical shared-mime-info names
          "video/matroska"
          "audio/matroska"
          "application/smil+xml"
          "audio/x-ms-asx"
          # Old aliases listed by mpv.desktop
          "video/x-matroska"
          "audio/x-matroska"
          "application/x-matroska"
          # Common video
          "video/mp4"
          "video/webm"
          "video/x-msvideo"
          "video/avi"
          "video/quicktime"
          "video/ogg"
          "video/mpeg"
          # Common audio
          "audio/mpeg"
          "audio/mp3"
          "audio/flac"
          "audio/x-flac"
          "audio/ogg"
          "audio/opus"
          "audio/x-opus+ogg"
          "audio/mp4"
          "audio/x-m4a"
          "audio/aac"
          "audio/x-wav"
          "audio/wav"
        ] (_: [ "mpv.desktop" ])
      );
    };
  };
}
