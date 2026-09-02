{
  config,
  lib,
  pkgs,
  ...
}:
let
  username = config.nx.username;
  homeDir = config.home-manager.users.${username}.home.homeDirectory;
in
{
  options = {
    nx.programs.ssh.enable = lib.mkEnableOption "Configuration to support outbound ssh" // {
      default = true;
    };
    nx.programs.ssh.enableWorkKeys = lib.mkEnableOption "Enable work SSH keys and config" // {
      default = false;
    };
  };

  config = lib.mkIf config.nx.programs.ssh.enable (
    let
      isLinux = pkgs.stdenv.hostPlatform.isLinux;
      isDarwin = pkgs.stdenv.hostPlatform.isDarwin;
    in
    lib.mkMerge [
      # Common configuration (both platforms)
      {
        home-manager.users.${username} = {
          programs.ssh = {
            enable = true;
            # https://github.com/nix-community/home-manager/blob/77f348da3176dc68b20a73dab94852a417daf361/modules/programs/ssh.nix#L633C17-L641
            enableDefaultConfig = false; # deprecated, setting to false silences warning
            settings = {
              "*" = lib.mkMerge [
                {
                  # don't ask to check host key for new hosts
                  StrictHostKeyChecking = "accept-new";
                }
                (lib.mkIf config.nx.programs.ssh.enableWorkKeys {
                  Include = "${homeDir}/.ssh/workconfig";
                })
              ];
              "node0" = {
                HostName = "10.87.42.100";
                User = username;
                Port = 22;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
              "node1" = {
                HostName = "10.87.42.101";
                User = username;
                Port = 22;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
              "node2" = {
                HostName = "10.87.42.102";
                User = username;
                Port = 22;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
              "node3" = {
                HostName = "10.87.42.103";
                User = username;
                Port = 22;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
              "nas0" = {
                HostName = "10.87.42.200";
                Port = 220;
                User = username;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
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
      }

      # Linux-specific (sops via system, activation scripts)
      (lib.mkIf isLinux {
        sops.secrets =
          let
            sopsFile = ./secrets/ssh.sops.yaml;
            mkSecret = name: {
              owner = username;
              mode = "0600";
              path = "${homeDir}/.ssh/${name}";
              sopsFile = sopsFile;
            };
          in
          {
            # Personal SSH keys (always included)
            "ssh/prismatic-koi-ed25519" = mkSecret "prismatic-koi-ed25519";
            "ssh/prismatic-koi-ed25519.pub" = mkSecret "prismatic-koi-ed25519.pub";
            "ssh/prismatic-koi-rsa" = mkSecret "prismatic-koi-rsa";
            "ssh/prismatic-koi-rsa.pub" = mkSecret "prismatic-koi-rsa.pub";
          };

        system.activationScripts.sshKeysFolderPermissions = ''
          mkdir -p ${homeDir}/.ssh
          chown ${username}:users ${homeDir}/.ssh
        '';
      })

      # Darwin-specific (sops via home-manager, no activation scripts)
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets =
            let
              sopsFile = ./secrets/ssh.sops.yaml;
              mkSecret = name: {
                path = "${homeDir}/.ssh/${name}";
                sopsFile = sopsFile;
              };
            in
            {
              # Personal SSH keys (always included)
              "ssh/prismatic-koi-ed25519" = mkSecret "prismatic-koi-ed25519";
              "ssh/prismatic-koi-ed25519.pub" = mkSecret "prismatic-koi-ed25519.pub";
              "ssh/prismatic-koi-rsa" = mkSecret "prismatic-koi-rsa";
              "ssh/prismatic-koi-rsa.pub" = mkSecret "prismatic-koi-rsa.pub";
            };
        };
      })
    ]
  );
}
