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
          "atlassian_site" = {
            owner = username;
            mode = "0600";
            sopsFile = ../secrets/atlassian.sops.yaml;
          };
          "atlassian_email" = {
            owner = username;
            mode = "0600";
            sopsFile = ../secrets/atlassian.sops.yaml;
          };
          "atlassian_token" = {
            owner = username;
            mode = "0600";
            sopsFile = ../secrets/atlassian.sops.yaml;
          };
        };

        environment.sessionVariables = {
          ATLASSIAN_SITE = "$(cat ${config.sops.secrets.atlassian_site.path})";
          ATLASSIAN_EMAIL = "$(cat ${config.sops.secrets.atlassian_email.path})";
          ATLASSIAN_API_TOKEN = "$(cat ${config.sops.secrets.atlassian_token.path})";
        };
      })

      # Darwin: sops secret via home-manager
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets = {
            "atlassian_site" = {
              sopsFile = ../secrets/atlassian.sops.yaml;
            };
            "atlassian_email" = {
              sopsFile = ../secrets/atlassian.sops.yaml;
            };
            "atlassian_token" = {
              sopsFile = ../secrets/atlassian.sops.yaml;
            };
          };

          home.sessionVariables = {
            ATLASSIAN_SITE = "$(cat ${config.home-manager.users.${username}.sops.secrets.atlassian_site.path})";
            ATLASSIAN_EMAIL = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.atlassian_email.path
            })";
            ATLASSIAN_API_TOKEN = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.atlassian_token.path
            })";
          };
        };
      })
    ]
  );
}
