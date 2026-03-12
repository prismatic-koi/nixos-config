{
  config,
  pkgs,
  lib,
  ...
}:
let
  username = config.nx.username;
  isLinux = pkgs.stdenv.isLinux;
  isDarwin = pkgs.stdenv.isDarwin;
in
{
  options = {
    nx.programs.gitlab-cli.enable = lib.mkEnableOption "enables GitLab CLI (glab)" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.gitlab-cli.enable (
    lib.mkMerge [
      {
        home-manager.users.${username} = {
          home.packages = [ pkgs.glab ];
        };
      }

      # Linux: system-level sops
      (lib.mkIf isLinux {
        sops.secrets.gitlab_token = {
          owner = username;
          mode = "0600";
          sopsFile = ./secrets/gitlab.sops.yaml;
        };

        environment.sessionVariables = {
          GITLAB_TOKEN = "$(cat ${config.sops.secrets.gitlab_token.path})";
        };
      })

      # Darwin: home-manager sops
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets.gitlab_token = {
            sopsFile = ./secrets/gitlab.sops.yaml;
          };

          home.sessionVariables = {
            GITLAB_TOKEN = "$(cat ${config.home-manager.users.${username}.sops.secrets.gitlab_token.path})";
          };
        };
      })
    ]
  );
}
