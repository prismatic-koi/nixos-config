{
  config,
  lib,
  pkgs,
  ...
}:
let
  homeDir = config.home-manager.users.ben.home.homeDirectory;
  isLinux = pkgs.stdenv.isLinux;
  isDarwin = pkgs.stdenv.isDarwin;
in
{
  config = lib.mkIf config.nx.programs.qutebrowser.enable (
    lib.mkMerge [
      # Linux: system-level sops
      (lib.mkIf isLinux {
        sops.secrets = {
          "bookmarks.sops" = {
            # using binary format to preserve multiline strings
            format = "binary";
            owner = "ben";
            mode = "0600";
            sopsFile = ./secrets/bookmarks.sops;
            path = "${homeDir}/.config/qutebrowser/bookmarks/urls";
          };
          bitwarden_password = {
            owner = "ben";
            mode = "0600";
            sopsFile = ./secrets/bitwarden.sops.yaml;
          };
        };
        system.activationScripts.qutebrowserFolderPermissions = ''
          mkdir -p ${homeDir}/.config/qutebrowser
          chown -R ben:users ${homeDir}/.config/qutebrowser
        '';
        environment.sessionVariables = {
          BITWARDEN_PASSWORD = "$(cat ${config.sops.secrets.bitwarden_password.path})";
        };
      })

      # Darwin: home-manager sops
      (lib.mkIf isDarwin {
        home-manager.users.ben.sops.secrets = {
          "bookmarks.sops" = {
            # using binary format to preserve multiline strings
            format = "binary";
            sopsFile = ./secrets/bookmarks.sops;
            path = "${homeDir}/.config/qutebrowser/bookmarks/urls";
          };
          bitwarden_password = {
            sopsFile = ./secrets/bitwarden.sops.yaml;
          };
        };
        environment.sessionVariables = {
          BITWARDEN_PASSWORD = "$(cat ${config.home-manager.users.ben.sops.secrets.bitwarden_password.path})";
        };
      })
    ]
  );
}
