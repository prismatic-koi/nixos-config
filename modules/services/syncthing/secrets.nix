{
  config,
  lib,
  pkgs,
  isLinux,
  ...
}:
let
  hostname = config.networking.hostName;
  configDir = config.nx.services.syncthing.configDir;
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

  # ── Pinned REST API key (issue #2461) ──────────────────────────
  #
  # Syncthing serves its Prometheus `/metrics` endpoint on the REST API
  # port and requires the REST API key to read it. Left alone, Syncthing
  # generates a random key into config.xml on first run: host-local
  # mutable state, different on every host, and unknowable to Nix. So we
  # pin a key from sops instead.
  #
  # The key must NOT go through `services.syncthing.settings.gui.apikey`.
  # The upstream nixpkgs module pushes `settings.gui` into the
  # world-readable `merge-syncthing-config` script in /nix/store, so that
  # route would publish the key to every local user. We hand it to
  # Syncthing through STGUIAPIKEY in a systemd EnvironmentFile that
  # sops-nix renders at runtime.
  #
  # Hosts listed here must have a `<host>/gui-apikey` entry in
  # ./secrets/gui-apikey.sops.yaml, or sops-nix fails at activation.
  # Scope is navi and tui (both Linux); m4mac waits on #2694.
  apiKeyHosts = [
    "navi"
    "tui"
  ];
  hasApiKey = builtins.elem hostname apiKeyHosts;
  apiKeySecret = "${hostname}/gui-apikey";
  apiKeyEnvTemplate = "syncthing-gui-apikey.env";

  # Group that owns the decrypted key file. Alloy runs under systemd
  # DynamicUser, so it has no stable user to name as the file owner; it
  # joins this group through SupplementaryGroups instead. See
  # modules/services/alloy/default.nix.
  apiKeyGroup = "syncthing-api";
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

      # Linux: pin the Syncthing REST API key from sops.
      #
      # `optionalAttrs` (not `mkIf`) does the platform split, because
      # `sops.templates`, `sops.placeholder`, and `systemd.services` do
      # not exist on Darwin and `mkIf` still registers the option paths.
      # The inner `mkIf` is safe: every option it touches exists on
      # NixOS.
      (lib.optionalAttrs isLinux (
        lib.mkIf hasApiKey {
          users.groups.${apiKeyGroup} = { };

          # Raw key file. Alloy reads this as a bearer token on every
          # scrape, so a key change needs no restart of alloy.
          sops.secrets.${apiKeySecret} = {
            sopsFile = ./secrets/gui-apikey.sops.yaml;
            group = apiKeyGroup;
            mode = "0440";
          };

          # Same key, rendered as an EnvironmentFile for syncthing.
          # systemd reads it as root before it drops privileges, so the
          # file stays root-only.
          sops.templates.${apiKeyEnvTemplate} = {
            content = "STGUIAPIKEY=${config.sops.placeholder.${apiKeySecret}}";
            mode = "0400";
            restartUnits = [ "syncthing.service" ];
          };

          nx.services.syncthing = {
            apiKeyFile = config.sops.secrets.${apiKeySecret}.path;
            inherit apiKeyGroup;
          };

          systemd.services.syncthing.serviceConfig.EnvironmentFile = [
            config.sops.templates.${apiKeyEnvTemplate}.path
          ];
        }
      ))

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
