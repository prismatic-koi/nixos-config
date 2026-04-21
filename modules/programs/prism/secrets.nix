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
  secretsFile = ../secrets/openrouter.sops.yaml;
in
{
  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      # Linux: system-level sops
      (lib.mkIf isLinux {
        sops.secrets.openrouter_token = {
          owner = username;
          mode = "0600";
          sopsFile = secretsFile;
        };

        environment.sessionVariables = {
          OPENROUTER_API_KEY = "$(cat ${config.sops.secrets.openrouter_token.path})";
        };
      })

      # Darwin: home-manager sops
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets.openrouter_token = {
            sopsFile = secretsFile;
          };

          home.sessionVariables = {
            OPENROUTER_API_KEY = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.openrouter_token.path
            })";
          };
        };
      })
    ]
  );
}
