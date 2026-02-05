{
  pkgs,
  lib,
  config,
  ...
}:
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
      home-manager.users.ben = {
        xdg = {
          dataHome = "${config.home-manager.users.ben.home.homeDirectory}/.local/share";
          stateHome = "${config.home-manager.users.ben.home.homeDirectory}/.local/state";
          configHome = "${config.home-manager.users.ben.home.homeDirectory}/.config";
          cacheHome = "${config.home-manager.users.ben.home.homeDirectory}/.cache";
        };
        # XDG env vars in home-manager (works on both platforms)
        home.sessionVariables = {
          XDG_CONFIG_HOME = "$HOME/.config";
          XDG_DATA_HOME = "$HOME/.local/share";
          XDG_STATE_HOME = "$HOME/.local/state";
          XDG_CACHE_HOME = "$HOME/.cache";
        };
      };
    }
    # Linux-only: system-level environment variables
    (lib.mkIf pkgs.stdenv.isLinux {
      environment.sessionVariables = {
        # General XDG variables
        XDG_CONFIG_HOME = "$HOME/.config";
        XDG_DATA_HOME = "$HOME/.local/share";
        XDG_STATE_HOME = "$HOME/.local/state";
        XDG_CACHE_HOME = "$HOME/.cache";
      };
    })
  ];
}
