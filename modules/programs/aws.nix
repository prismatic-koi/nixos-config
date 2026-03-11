{
  config,
  pkgs,
  lib,
  ...
}:
let
  username = config.nx.username;
  awsDir = "${config.home-manager.users.${username}.home.homeDirectory}/.config/aws";
  isLinux = pkgs.stdenv.isLinux;
  isDarwin = pkgs.stdenv.isDarwin;
in
{
  options = {
    nx.programs.aws.enable = lib.mkEnableOption "enables AWS CLI with XDG-compliant config";
  };
  config = lib.mkIf config.nx.programs.aws.enable (
    lib.mkMerge [
      # Common configuration (both platforms)
      {
        home-manager.users.${username} = {
          home.packages = with pkgs; [
            awscli2
            ssm-session-manager-plugin
          ];
          home.sessionVariables = {
            AWS_CONFIG_FILE = "${awsDir}/config";
            AWS_SHARED_CREDENTIALS_FILE = "${awsDir}/credentials";
            AWS_CLI_HISTORY_FILE = "$HOME/.local/state/aws/history.db";
          };
        };
      }

      # Linux: system-level sops
      (lib.mkIf isLinux {
        sops.secrets.aws-config = {
          owner = username;
          mode = "0600";
          path = "${awsDir}/config";
          sopsFile = ./secrets/awsconfig.sops.yaml;
        };
        sops.secrets.aws-readonly-config = {
          owner = username;
          mode = "0600";
          path = "${awsDir}/readonly-config";
          sopsFile = ./secrets/awsconfig.sops.yaml;
        };
        system.activationScripts.awsConfigFolderPermissions = ''
          mkdir -p ${awsDir}
          chown ${username}:users ${config.home-manager.users.${username}.home.homeDirectory}/.config
          chown ${username}:users ${awsDir}
        '';
      })

      # Darwin: home-manager sops
      (lib.mkIf isDarwin {
        home-manager.users.${username}.sops.secrets = {
          aws-config = {
            path = "${awsDir}/config";
            sopsFile = ./secrets/awsconfig.sops.yaml;
          };
          aws-readonly-config = {
            path = "${awsDir}/readonly-config";
            sopsFile = ./secrets/awsconfig.sops.yaml;
          };
        };
      })
    ]
  );
}
