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
    nx.programs.git.enable = lib.mkEnableOption "enables git" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.git.enable (
    let
      isLinux = pkgs.stdenv.isLinux;
      isDarwin = pkgs.stdenv.isDarwin;
    in
    lib.mkMerge [
      # Common git and ssh configuration for both platforms
      {
        home-manager.users.${username} = {
          programs.ssh = {
            enable = true;
            matchBlocks = {
              "github.com" = {
                user = "git";
                hostname = "github.com";
                port = 22;
                identityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
              "gitlab.com" = {
                user = "git";
                hostname = "gitlab.com";
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
      }

      # Linux: sops secrets via system config
      (lib.mkIf isLinux {
        sops.secrets = {
          "ssh/prismatic-koi-ed25519-signingkey" = {
            owner = username;
            mode = "0600";
            path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey";
            sopsFile = ./secrets/ssh.sops.yaml;
          };
          "ssh/prismatic-koi-ed25519-signingkey.pub" = {
            owner = username;
            mode = "0600";
            path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey.pub";
            sopsFile = ./secrets/ssh.sops.yaml;
          };
          "github_token" = {
            owner = username;
            mode = "0600";
            sopsFile = ./secrets/github.sops.yaml;
          };
        };

        # Linux-only: system activation scripts (for impermanence)
        system.activationScripts.signingKeysFolderPermissions = ''
          mkdir -p ${homeDir}/.ssh
          chown ${username}:users ${homeDir}/.ssh
        '';

        # Environment variables for GitHub token
        environment.sessionVariables = {
          GITHUB_TOKEN = "$(cat ${config.sops.secrets.github_token.path})";
          GITHUB_PACKAGES_TOKEN = "$(cat ${config.sops.secrets.github_token.path})";
        };
      })

      # Darwin: sops secrets via home-manager
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets = {
            "ssh/prismatic-koi-ed25519-signingkey" = {
              path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey";
              sopsFile = ./secrets/ssh.sops.yaml;
            };
            "ssh/prismatic-koi-ed25519-signingkey.pub" = {
              path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey.pub";
              sopsFile = ./secrets/ssh.sops.yaml;
            };
            "github_token" = {
              sopsFile = ./secrets/github.sops.yaml;
            };
          };

          # Environment variables for GitHub token
          home.sessionVariables = {
            GITHUB_TOKEN = "$(cat ${config.home-manager.users.${username}.sops.secrets.github_token.path})";
            GITHUB_PACKAGES_TOKEN = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.github_token.path
            })";
          };
        };
      })
    ]
  );
}
