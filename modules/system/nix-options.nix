{
  pkgs,
  lib,
  ...
}:
{
  # nix options for all machines
  nix = lib.mkMerge [
    # Cross-platform settings
    {
      settings = {
        experimental-features = [
          "nix-command"
          "flakes"
        ];
        trusted-users = [
          "root"
          "@wheel"
        ];
        trusted-substituters = [
          "https://nix-community.cachix.org"
          "https://lucidph3nx-nixos-config.cachix.org"
        ];
        trusted-public-keys = [
          "nix-community.cachix.org-1:mB9FSh9qf2dCimDSUo8Zy7bkq5CX+/rkCWyvRCYg3Fs="
          "lucidph3nx-nixos-config.cachix.org-1:gXiGMMDnozkXCjvOs9fOwKPZNIqf94ZA/YksjrKekHE="
        ];
      };
      # automatic garbage collection
      gc = {
        automatic = true;
        options = "--delete-older-than 15d";
      };
    }
    # NixOS-specific settings
    (lib.optionalAttrs pkgs.stdenv.isLinux {
      settings.auto-optimise-store = true;
      gc.dates = "daily";
    })
    # Darwin-specific settings
    (lib.optionalAttrs pkgs.stdenv.isDarwin {
      optimise.automatic = true;
      gc.interval = {
        Weekday = 0;
        Hour = 0;
        Minute = 0;
      };
    })
  ];
}
