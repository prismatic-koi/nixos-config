{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
  cloudflaredBlock = {
    proxyCommand = "${pkgs.cloudflared}/bin/cloudflared access ssh --hostname %h.$CLOUDFLARED_DOMAIN";
    user = "ben";
    port = 22;
    identityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
  };
in
{
  options = {
    nx.programs.ssh.enable = lib.mkEnableOption "Configuration to support outbound ssh" // {
      default = true;
    };
  };

  config = lib.mkIf config.nx.programs.ssh.enable {
    home-manager.users.ben = {
      home.packages = with pkgs; [
        cloudflared
      ];
      programs.ssh = {
        enable = true;
        # https://github.com/nix-community/home-manager/blob/77f348da3176dc68b20a73dab94852a417daf361/modules/programs/ssh.nix#L633C17-L641
        enableDefaultConfig = false; # deprecated, setting to false silences warning
        matchBlocks = {
          "*" = {
            # don't ask to check host key for new hosts
            extraOptions = {
              StrictHostKeyChecking = "accept-new";
            };
          };
          "node0" = cloudflaredBlock;
          "node1" = cloudflaredBlock;
          "node2" = cloudflaredBlock;
          "node3" = cloudflaredBlock;
          "nas0" = {
            hostname = "10.87.42.200";
            port = 220;
            user = "ben";
            identityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
          };
        };
      };
      home.persistence."/persist" = {
        directories = [
          ".ssh"
        ];
      };
      # a script to open ssh connections to all nodes
      home.file.".local/scripts/home.ssh.allNodes" = {
        executable = true;
        text =
          # sh
          ''
            #!/bin/sh
            kitty ssh node0 &
            kitty ssh node1 &
            kitty ssh node2 &
            kitty ssh node3
          '';
      };
    };
    # ssh secrets
    sops.secrets =
      let
        sopsFile = ./secrets/ssh.sops.yaml;
      in
      {
        "ssh/prismatic-koi-ed25519" = {
          owner = "ben";
          mode = "0600";
          path = "/home/ben/.ssh/prismatic-koi-ed25519";
          sopsFile = sopsFile;
        };
        "ssh/prismatic-koi-ed25519.pub" = {
          owner = "ben";
          mode = "0600";
          path = "/home/ben/.ssh/prismatic-koi-ed25519.pub";
          sopsFile = sopsFile;
        };
        "ssh/prismatic-koi-rsa" = {
          owner = "ben";
          mode = "0600";
          path = "/home/ben/.ssh/prismatic-koi-rsa";
          sopsFile = sopsFile;
        };
        "ssh/prismatic-koi-rsa.pub" = {
          owner = "ben";
          mode = "0600";
          path = "/home/ben/.ssh/prismatic-koi-rsa.pub";
          sopsFile = sopsFile;
        };
        "config/cloudflared_domain" = {
          owner = "ben";
          mode = "0600";
          sopsFile = sopsFile;
        };
      };
    environment.sessionVariables = {
      CLOUDFLARED_DOMAIN = "$(cat ${config.sops.secrets."config/cloudflared_domain".path})";
    };
    system.activationScripts.sshKeysFolderPermissions = ''
      mkdir -p /home/ben/.ssh
      chown ben:users /home/ben/.ssh
    '';
  };
}
