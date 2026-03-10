{
  config,
  pkgs,
  lib,
  ...
}:
let
  username = config.nx.username;
in
{
  config = lib.mkIf pkgs.stdenv.isLinux {
    # Set up main user account
    # Define a user account. Don't forget to set a password with 'passwd'.
    users.users.${username} = {
      isNormalUser = true;
      hashedPasswordFile = config.sops.secrets.ben_hashed_password.path;
      description = username;
      extraGroups = [
        "wheel"
        (lib.mkIf config.networking.networkmanager.enable "networkmanager")
        (lib.mkIf config.hardware.openrazer.enable "openrazer")
      ];
      shell = pkgs.zsh;
    };
    # password
    sops.secrets.ben_hashed_password = {
      neededForUsers = true;
      sopsFile = ./secrets/passwords.sops.yaml;
    };
    # no password for sudo
    security.sudo.wheelNeedsPassword = false;

    # increase ulimit
    security.pam.loginLimits = [
      {
        domain = "*";
        type = "soft";
        item = "nofile";
        value = "65536";
      }
    ];
  };
}
