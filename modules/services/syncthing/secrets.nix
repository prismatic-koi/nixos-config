{
  config,
  lib,
  pkgs,
  isLinux,
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

  # ── GUI basic-auth credential (issue #2698) ─────────────────────
  #
  # Issue #2461 pinned a Bearer token so Alloy's /metrics scrape had
  # the correct long-term credential, but left it non-load-bearing:
  # no GUI user/password meant Syncthing never installed
  # `basicAuthAndSessionMiddleware`, so /metrics answered any local
  # request with no auth at all (#2698). This closes that gap.
  #
  # Reuses `hasApiKey`/`apiKeyHosts` (navi, tui) as the scope — the
  # same two hosts, no reason for a second host list.
  #
  # The password must NOT go through `services.syncthing.settings.gui`
  # (that whole attrset, apart from `guiPasswordFile` itself, is
  # embedded as JSON directly into the world-readable
  # `merge-syncthing-config` script — see the nixpkgs syncthing
  # module). `guiPasswordFile` is a nixpkgs-native escape hatch: it
  # takes a *path*, read at activation time by `merge-syncthing-config`
  # to bcrypt-hash the password and PATCH it into Syncthing over the
  # REST API. The path itself is fine in the store; the file it points
  # at is the sops-decrypted runtime secret, never the store.
  #
  # `gui.user` is not a secret -- a username carries no sensitive
  # value -- so it is set directly in `settings.gui.user` below and is
  # fine to embed in the store script alongside the rest of
  # `cleanedConfig`.
  guiPasswordSecret = "${hostname}/gui-password";

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

          # GUI basic-auth credential (issue #2698). The password file
          # is owned by the syncthing user itself: `merge-syncthing-config`
          # runs as `cfg.user` (see the upstream module's `updateConfig`
          # systemd unit), so it needs read access, not the alloy-facing
          # `apiKeyGroup`.
          sops.secrets.${guiPasswordSecret} = {
            sopsFile = ./secrets/gui-password.sops.yaml;
            owner = username;
            mode = "0400";
          };

          services.syncthing = {
            settings.gui.user = username;
            guiPasswordFile = config.sops.secrets.${guiPasswordSecret}.path;
          };
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
