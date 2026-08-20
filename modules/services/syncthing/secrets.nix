{
  config,
  lib,
  pkgs,
  ...
}:
let
  hostname = config.networking.hostName;
  username = config.nx.username;
  hosts = [
    "navi"
    "tui"
    "m4mac"
  ];
  files = [
    "cert.pem"
    "key.pem"
  ];
  mkSecret =
    host:
    lib.mkIf (hostname == host) {
      owner = username;
      mode = "0600";
      sopsFile = ./secret.sops.yaml;
    };

  # ── No GUI auth, no pinned REST API key (issue #2787) ────────────
  #
  # This module used to pin a sops-encrypted Syncthing REST API key on
  # every host (#2461 on navi and tui, #2697 / #2782 on m4mac) and set
  # a GUI user and password on navi and tui (#2698). All of that is
  # removed. Only the device cert and key remain here. The reversal is
  # recorded here on purpose: a silent revert is the failure #2697 was
  # filed to correct.
  #
  # Why the removal is safe:
  #
  #   * Localhost is the boundary. Syncthing binds its GUI and REST
  #     API to 127.0.0.1:8384 — set explicitly in ./default.nix so the
  #     property is declared, not inherited from an upstream default.
  #     Port 8384 is never opened in a firewall (only 22000 and 21027,
  #     for sync and discovery), so nothing off the host can reach it.
  #   * These are single-user personal machines. GUI auth and the
  #     pinned key defended only against a second local user who reads
  #     /metrics or drives the web UI. There is no second local user.
  #   * Metrics leave the host on an authenticated outbound path, not
  #     an inbound scrape. Alloy runs locally, dials
  #     127.0.0.1:8384/metrics, and pushes to ts-metrics-ingest over
  #     Tailscale (modules/services/alloy). No central Prometheus
  #     reaches in, so /metrics only ever needs to be readable from
  #     localhost.
  #   * The key was load-bearing only while GUI auth was on. Syncthing
  #     wraps /metrics with `basicAuthAndSessionMiddleware` only when
  #     `guiCfg.IsAuthEnabled()` is true (lib/api/api.go), and
  #     /metrics sits outside the `/rest` prefix the CSRF manager
  #     guards. With GUI auth off, the Bearer token is ignored.
  #   * The cost was real and daily: a login prompt on the web UI, for
  #     no gain.
  #
  # This reverses #2461, #2698, and #2782, and supersedes #2783. Do
  # not re-add GUI auth or a pinned key without first changing the
  # bind address away from loopback — the loopback bind is the fact
  # the whole decision rests on.
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
        home-manager.users.${username} = {
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
            cert = config.home-manager.users.${username}.sops.secrets."${hostname}-syncthing-cert".path;
            key = config.home-manager.users.${username}.sops.secrets."${hostname}-syncthing-key".path;
          };
        };
      })
    ]
  );
}
