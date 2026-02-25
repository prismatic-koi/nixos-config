{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.programs.rclone.enable = lib.mkEnableOption "rclone cloud storage client";
  };

  config = lib.mkIf config.nx.programs.rclone.enable {
    home-manager.users.ben = {
      home.packages = with pkgs; [ rclone ];
    };

    # Persist rclone configuration directory
    environment.persistence."/nix/persist" = {
      users.ben = {
        directories = [ ".config/rclone" ];
      };
    };
  };
}
