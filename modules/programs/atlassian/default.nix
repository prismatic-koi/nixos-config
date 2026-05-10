{
  config,
  lib,
  pkgs,
  ...
}:
let
  username = config.nx.username;
in
{
  options = {
    nx.programs.atlassian = {
      enable = lib.mkEnableOption "enables the atlassian CLI" // {
        default = true;
      };
      site = lib.mkOption {
        type = lib.types.str;
        default = "tinfoilforest.atlassian.net";
        description = "Atlassian cloud site hostname (e.g. mycompany.atlassian.net)";
      };
      email = lib.mkOption {
        type = lib.types.str;
        default = "ben@tinfoilforest.nz";
        description = "Atlassian account email address";
      };
    };
  };

  config = lib.mkIf config.nx.programs.atlassian.enable (
    let
      isLinux = pkgs.stdenv.isLinux;
      isDarwin = pkgs.stdenv.isDarwin;
    in
    lib.mkMerge [
      # Install the atlassian binary for all platforms
      {
        home-manager.users.${username} = {
          home.packages = [ pkgs.atlassian ];
        };
      }

      # Linux: sops secret via system config
      (lib.mkIf isLinux {
        sops.secrets = {
          "atlassian_token" = {
            owner = username;
            mode = "0600";
            sopsFile = ../secrets/atlassian.sops.yaml;
          };
        };

        environment.sessionVariables = {
          ATLASSIAN_SITE = config.nx.programs.atlassian.site;
          ATLASSIAN_EMAIL = config.nx.programs.atlassian.email;
          ATLASSIAN_API_TOKEN = "$(cat ${config.sops.secrets.atlassian_token.path})";
        };
      })

      # Darwin: sops secret via home-manager
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets = {
            "atlassian_token" = {
              sopsFile = ../secrets/atlassian.sops.yaml;
            };
          };

          home.sessionVariables = {
            ATLASSIAN_SITE = config.nx.programs.atlassian.site;
            ATLASSIAN_EMAIL = config.nx.programs.atlassian.email;
            ATLASSIAN_API_TOKEN = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.atlassian_token.path
            })";
          };
        };
      })
    ]
  );
}
