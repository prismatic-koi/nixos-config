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
    nx.programs.ssh.enableWorkKeys = lib.mkEnableOption "Enable work SSH keys and config" // {
      default = false;
    };
  };

  config = lib.mkIf config.nx.programs.ssh.enable (
    let
      isLinux = pkgs.stdenv.isLinux;
      isDarwin = pkgs.stdenv.isDarwin;
    in
    lib.mkMerge [
      # Common configuration (both platforms)
      {
        home-manager.users.ben = {
          home.packages = with pkgs; [
            cloudflared
          ];
          programs.ssh = {
            enable = true;
            # https://github.com/nix-community/home-manager/blob/77f348da3176dc68b20a73dab94852a417daf361/modules/programs/ssh.nix#L633C17-L641
            enableDefaultConfig = false; # deprecated, setting to false silences warning
            matchBlocks = {
              "*" = lib.mkMerge [
                {
                  # don't ask to check host key for new hosts
                  extraOptions = {
                    StrictHostKeyChecking = "accept-new";
                  };
                }
                (lib.mkIf config.nx.programs.ssh.enableWorkKeys {
                  extraOptions = {
                    Include = "${homeDir}/.ssh/workconfig";
                  };
                })
              ];
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
      }

      # Linux-specific (sops via system, activation scripts)
      (lib.mkIf isLinux {
        sops.secrets =
          let
            sopsFile = ./secrets/ssh.sops.yaml;
            mkSecret = name: {
              owner = "ben";
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
            "config/cloudflared_domain" = {
              owner = "ben";
              mode = "0600";
              sopsFile = sopsFile;
            };
          }
          // lib.optionalAttrs config.nx.programs.ssh.enableWorkKeys {
            # Work SSH keys (opt-in)
            "ssh/work-prismatic-koi-ed25519" = mkSecret "work-prismatic-koi-ed25519";
            "ssh/work-prismatic-koi-ed25519.pub" = mkSecret "work-prismatic-koi-ed25519.pub";
            "ssh/work-rsa" = mkSecret "work-rsa";
            "ssh/work-rsa.pub" = mkSecret "work-rsa.pub";
            "ssh/work-ed25519" = mkSecret "work-ed25519";
            "ssh/work-ed25519.pub" = mkSecret "work-ed25519.pub";
            "ssh/workconfig" = {
              owner = "ben";
              mode = "0600";
              path = "${homeDir}/.ssh/workconfig";
              format = "binary";
              sopsFile = ./secrets/worksshconfig;
            };
          };

        environment.sessionVariables = {
          CLOUDFLARED_DOMAIN = "$(cat ${config.sops.secrets."config/cloudflared_domain".path})";
        };

        system.activationScripts.sshKeysFolderPermissions = ''
          mkdir -p ${homeDir}/.ssh
          chown ben:users ${homeDir}/.ssh
        '';
      })

      # Darwin-specific (sops via home-manager, no activation scripts)
      (lib.mkIf isDarwin {
        home-manager.users.ben.sops.secrets =
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
            "config/cloudflared_domain" = {
              sopsFile = sopsFile;
            };
          }
          // lib.optionalAttrs config.nx.programs.ssh.enableWorkKeys {
            # Work SSH keys (opt-in)
            "ssh/work-prismatic-koi-ed25519" = mkSecret "work-prismatic-koi-ed25519";
            "ssh/work-prismatic-koi-ed25519.pub" = mkSecret "work-prismatic-koi-ed25519.pub";
            "ssh/work-rsa" = mkSecret "work-rsa";
            "ssh/work-rsa.pub" = mkSecret "work-rsa.pub";
            "ssh/work-ed25519" = mkSecret "work-ed25519";
            "ssh/work-ed25519.pub" = mkSecret "work-ed25519.pub";
            "ssh/workconfig" = {
              path = "${homeDir}/.ssh/workconfig";
              format = "binary";
              sopsFile = ./secrets/worksshconfig;
            };
          };

        environment.sessionVariables = {
          CLOUDFLARED_DOMAIN = "$(cat ${
            config.home-manager.users.ben.sops.secrets."config/cloudflared_domain".path
          })";
        };
      })
    ]
  );
}
