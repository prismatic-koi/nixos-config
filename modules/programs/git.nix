{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
in
{
  options = {
    nx.programs.git.enable = lib.mkEnableOption "enables git" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.git.enable {
    home-manager.users.ben = {
      programs.ssh = {
        enable = true;
        matchBlocks = {
          "github.com" = {
            user = "git";
            hostname = "github.com";
            port = 22;
            identityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
          };
        };
      };
      programs.git = {
        enable = true;
        includes = [
          {
            contents = {
              user = {
                name = "prismatic-koi";
                email = "ben@tinfoilforest.nz";
                signingKey = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey.pub";
              };
              push = {
                autoSetupRemote = true;
              };
              init = {
                defaultBranch = "main";
              };
              gpg = {
                format = "ssh";
              };
              commit = {
                gpgsign = true;
              };
            };
          }
        ];
      };
      home.packages = with pkgs; [
        gh
      ];
    };
    # ssh signing keys
    sops.secrets."ssh/prismatic-koi-ed25519-signingkey" = {
      owner = "ben";
      mode = "0600";
      path = "/home/ben/.ssh/prismatic-koi-ed25519-signingkey";
      sopsFile = ./secrets/ssh.sops.yaml;
    };
    sops.secrets."ssh/prismatic-koi-ed25519-signingkey.pub" = {
      owner = "ben";
      mode = "0600";
      path = "/home/ben/.ssh/prismatic-koi-ed25519-signingkey.pub";
      sopsFile = ./secrets/ssh.sops.yaml;
    };
    system.activationScripts.signingKeysFolderPermissions = ''
      mkdir -p /home/ben/.ssh
      chown ben:users /home/ben/.ssh
    '';
    # github api token
    sops.secrets."github_token" = {
      owner = "ben";
      mode = "0600";
      sopsFile = ./secrets/github.sops.yaml;
    };
    environment.sessionVariables = {
      GITHUB_TOKEN = "$(cat ${config.sops.secrets.github_token.path})";
      GITHUB_PACKAGES_TOKEN = "$(cat ${config.sops.secrets.github_token.path})";
    };
  };
}
