{
  pkgs,
  lib,
  config,
  ...
}:
let
  username = config.nx.username;
in
{
  imports = [
    ./hardware-boot-switch.nix
    ./impermanence.nix
    ./localisation.nix
    ./networking.nix
    ./nfs-mounts.nix
    ./nix-options.nix
    ./sops.nix
    ./users.nix
  ];
  config = lib.mkMerge [
    # Cross-platform: home-manager XDG configuration
    {
      home-manager.users.${username} = {
        # Enable XDG base directory support
        # This sets the directories and environment variables automatically
        xdg.enable = true;
      };
    }
    # Darwin-specific: ensure services.openssh.enable is false by default
    # This is needed by sops-nix which checks for openssh.enable
    (lib.mkIf pkgs.stdenv.isDarwin {
      services.openssh.enable = lib.mkDefault false;
    })
    # Linux-only: system-level environment variables
    (lib.mkIf pkgs.stdenv.isLinux {
      environment.sessionVariables = {
        # General XDG variables
        XDG_CONFIG_HOME = "$HOME/.config";
        XDG_DATA_HOME = "$HOME/.local/share";
        XDG_STATE_HOME = "$HOME/.local/state";
        XDG_CACHE_HOME = "$HOME/.cache";
        # Disable CGO globally — all non-Nix Go projects are pure Go, and Nix
        # builds manage their own hermetic environment so this has no effect there.
        CGO_ENABLED = "0";
      };
    })
  ];
}
