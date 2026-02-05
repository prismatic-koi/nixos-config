{
  pkgs,
  lib,
  ...
}:
let
  locale = "en_NZ.UTF-8";
in
{
  config = lib.mkMerge [
    # Time zone works on both platforms
    {
      time.timeZone = "Pacific/Auckland";
    }
    # i18n options are Linux-only
    (lib.mkIf pkgs.stdenv.isLinux {
      i18n.defaultLocale = locale;
      i18n.extraLocaleSettings = {
        LC_ADDRESS = locale;
        LC_IDENTIFICATION = locale;
        LC_MEASUREMENT = locale;
        LC_MONETARY = locale;
        LC_NAME = locale;
        LC_NUMERIC = locale;
        LC_PAPER = locale;
        LC_TELEPHONE = locale;
        LC_TIME = locale;
      };
    })
  ];
}
