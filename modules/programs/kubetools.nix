{
  config,
  pkgs,
  lib,
  ...
}:
let
  kubeDir = "${config.home-manager.users.ben.home.homeDirectory}/.config/kube";
  isLinux = pkgs.stdenv.isLinux;
  isDarwin = pkgs.stdenv.isDarwin;
in
{
  options = {
    nx.programs.kubetools.enable =
      lib.mkEnableOption "enables some cli tools for managing kubernetes"
      // {
        default = true;
      };
  };
  config = lib.mkIf config.nx.programs.kubetools.enable (
    lib.mkMerge [
      # Common configuration (both platforms)
      {
        home-manager.users.ben = {
          home.packages = with pkgs; [
            kubectl
            kubernetes-helm
            kubelogin-oidc
            kubelogin
            fluxcd
            krew
          ];
          home.sessionVariables = {
            KUBECONFIG = "${kubeDir}/config";
          };
        };
      }

      # Linux: system-level sops with admin and agents kubeconfig
      (lib.mkIf isLinux {
        # admin kubeconfig
        sops.secrets.kubeconfig = {
          owner = "ben";
          mode = "0600";
          path = "${kubeDir}/config";
          sopsFile = ./secrets/kubeconfig.sops.yaml;
        };
        # agents readonly kubeconfig
        sops.secrets.agents-kubeconfig = {
          owner = "ben";
          mode = "0600";
          path = "${kubeDir}/agents-config";
          sopsFile = ./secrets/kubeconfig.sops.yaml;
        };
        system.activationScripts.kubeConfigFolderPermissions = ''
          mkdir -p ${kubeDir}
          chown ben:users ${config.home-manager.users.ben.home.homeDirectory}/.config
          chown ben:users ${kubeDir}
        '';
      })

      # Darwin: home-manager sops with work kubeconfigs only
      (lib.mkIf isDarwin {
        home-manager.users.ben.sops.secrets = {
          workkube = {
            path = "${kubeDir}/config";
            sopsFile = ./secrets/kubeconfig.sops.yaml;
          };
          workreadonlykube = {
            path = "${kubeDir}/agents-config";
            sopsFile = ./secrets/kubeconfig.sops.yaml;
          };
        };
      })
    ]
  );
}
