{
  config,
  lib,
  ...
}:
{
  config = lib.mkIf config.nx.services.syncthing.enable {
    # certs and keys from sops secret file
    sops =
      let
        hostname = config.networking.hostName;
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
        secrets = builtins.listToAttrs (
          lib.concatMap (
            host:
            map (file: {
              name = "${host}/${file}";
              value = mkSecret host;
            }) files
          ) hosts
        );
      };
    # apply relevant secret to syncthing service
    services.syncthing =
      let
        host = config.networking.hostName;
      in
      {
        cert = config.sops.secrets."${host}/cert.pem".path;
        key = config.sops.secrets."${host}/key.pem".path;
      };
  };
}
