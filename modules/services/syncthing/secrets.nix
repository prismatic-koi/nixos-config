{
  config,
  lib,
  pkgs,
  ...
}:
let
  hostname = config.networking.hostName;
  configDir = config.nx.services.syncthing.configDir;
  hosts = [
    "navi"
    "tui"
    "m1mac"
  ];
  files = [
    "cert.pem"
    "key.pem"
  ];
  mkSecret =
    host:
    lib.mkIf (hostname == host) {
      owner = "ben";
      mode = "0600";
      sopsFile = ./secret.sops.yaml;
    };
in
{
  config = lib.mkIf config.nx.services.syncthing.enable (
    lib.mkMerge [
      # Common: sops secrets configuration
      {
        sops.secrets = builtins.listToAttrs (
          lib.concatMap (
            host:
            map (file: {
              name = "${host}/${file}";
              value = mkSecret host;
            }) files
          ) hosts
        );
        services.syncthing = {
          cert = config.sops.secrets."${hostname}/cert.pem".path;
          key = config.sops.secrets."${hostname}/key.pem".path;
        };
      }

      # Darwin: home-manager sops with certs placed directly in syncthing config directory
      (lib.mkIf pkgs.stdenv.isDarwin {
        home-manager.users.ben = {
          sops.secrets = {
            "${hostname}-syncthing-cert" = {
              sopsFile = ./secret.sops.yaml;
              key = "${hostname}/cert.pem";
            };
            "${hostname}-syncthing-key" = {
              sopsFile = ./secret.sops.yaml;
              key = "${hostname}/key.pem";
            };
          };
          services.syncthing = {
            cert = config.home-manager.users.ben.sops.secrets."${hostname}-syncthing-cert".path;
            key = config.home-manager.users.ben.sops.secrets."${hostname}-syncthing-key".path;
          };
        };
      })
    ]
  );
}
