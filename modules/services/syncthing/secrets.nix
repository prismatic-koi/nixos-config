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
  # Scope is navi and tui (both Linux). m4mac carries its own key in
  # its own sops file — see the Darwin block below.
  apiKeyHosts = [
    "navi"
    "tui"
  ];
  hasApiKey = builtins.elem hostname apiKeyHosts;
  apiKeySecret = "${hostname}/gui-apikey";
  apiKeyEnvTemplate = "syncthing-gui-apikey.env";

  # ── Darwin REST API key delivery (issue #2697) ──────────────────
  #
  # Same goal as the Linux block above — hand Syncthing a pinned,
  # sops-encrypted REST API key so Alloy can present it as a Bearer
  # token on /metrics — but launchd has none of the systemd machinery
  # that made this easy on navi and tui. There is no launchd
  # `EnvironmentFile`.
  #
  # CHOSEN MECHANISM: wrap the syncthing binary.
  # `services.syncthing.package` becomes a symlinkJoin of the real
  # package whose `bin/syncthing` is a makeWrapper shim. The shim
  # reads the sops-decrypted key file at start time and exports
  # STGUIAPIKEY before it execs the real binary. Syncthing accepts
  # that variable as a valid API key (`IsValidAPIKey` in
  # lib/config/guiconfiguration.go tests it alongside the config.xml
  # value). Only the *path* of the key file ever enters the Nix
  # store. The key itself exists in the sops runtime file (0400,
  # owned by the user) and in the process environment of syncthing,
  # which on macOS only the same user or root can read. That is the
  # exact analogue of the systemd `EnvironmentFile` route used on
  # Linux, which also ends with the value in the process environment.
  #
  # Wrapping the package, rather than overriding the launchd agent's
  # `ProgramArguments`, keeps home-manager's own `copyKeys` step and
  # its `syncthing serve ...` argument list as the single source of
  # truth. Overriding `ProgramArguments` would mean copying both into
  # this repo and re-syncing them on every home-manager bump.
  #
  # REJECTED ALTERNATIVES, and why:
  #
  #   * launchd `EnvironmentVariables` — the closest launchd analogue
  #     of a systemd EnvironmentFile. home-manager renders agents
  #     into ~/Library/LaunchAgents/*.plist at mode 0644, so the key
  #     would be readable by every local user. That is the same leak
  #     class #2461 rejected for `settings.gui.apikey`.
  #   * `services.syncthing.settings.gui.apikey` — home-manager
  #     embeds `settings` as JSON in the world-readable
  #     `merge-syncthing-config` script in /nix/store. Rejected for
  #     navi and tui by #2461 for exactly this reason.
  #   * `syncthing serve --gui-apikey=<key>` via `extraOptions` —
  #     puts the key in the process argument vector, which `ps` shows
  #     to every local user.
  #   * `launchctl setenv STGUIAPIKEY` — imperative, not declarative,
  #     session-wide, and inherited by every later process of that
  #     user rather than by syncthing alone.
  #   * Reading the key Syncthing generates for itself out of
  #     config.xml — needs no secret at all, but the key would then be
  #     host-local mutable state rather than sops-encrypted, which the
  #     acceptance criteria for #2697 rule out.
  #
  # NOT SOLVED HERE: Syncthing installs its auth middleware only when
  # a GUI user and password are set (`guiCfg.IsAuthEnabled()` in
  # lib/api/api.go), so /metrics on m4mac still answers unauthenticated
  # localhost requests. That is the m4mac half of #2698, which was
  # scoped to navi and tui. The Bearer token wired below is correct
  # and future-proof, exactly as it was on Linux before #2698 closed
  # the gap there.
  darwinApiKeyHosts = [ "m4mac" ];
  hasDarwinApiKey = builtins.elem hostname darwinApiKeyHosts;
  darwinApiKeySecretName = "${hostname}-syncthing-apikey";
  darwinApiKeyPath =
    config.home-manager.users.${username}.sops.secrets.${darwinApiKeySecretName}.path;

  # Bounded wait before reading the key, for `syncthing serve` only.
  # sops-nix decrypts user-scope secrets from its own launchd agent,
  # and launchd gives no ordering guarantee between that agent and the
  # syncthing one. A syncthing that wins the race would start with no
  # STGUIAPIKEY and keep its self-generated key until the next restart,
  # which would make every Alloy scrape fail with 403 once the m4mac
  # half of #2698 lands.
  #
  # Every other subcommand (`syncthing cli ...` and friends) skips the
  # wait. They still get the key when it is there, but they must not
  # hang for a minute on a host where sops has not run yet -- this
  # wrapper is what `syncthing` resolves to in the user's PATH.
  darwinApiKeyShim = pkgs.writeShellScript "syncthing-stguiapikey" ''
    key=${lib.escapeShellArg darwinApiKeyPath}

    if [ "''${1-}" = serve ]; then
      waited=0
      while [ ! -r "$key" ] && [ "$waited" -lt 60 ]; do
        ${pkgs.coreutils}/bin/sleep 1
        waited=$((waited + 1))
      done
      if [ ! -r "$key" ]; then
        echo "syncthing: $key not readable after ''${waited}s; starting without the pinned API key" >&2
      fi
    fi

    if [ -r "$key" ]; then
      STGUIAPIKEY=$(${pkgs.coreutils}/bin/cat "$key")
      export STGUIAPIKEY
    fi
  '';

  # `.` (source) rather than a nested exec, so the export survives
  # into the syncthing process that makeWrapper's wrapper execs.
  darwinSyncthingPackage = pkgs.symlinkJoin {
    name = "syncthing-pinned-api-key-${pkgs.syncthing.version}";
    paths = [ pkgs.syncthing ];
    nativeBuildInputs = [ pkgs.makeWrapper ];
    postBuild = ''
      wrapProgram $out/bin/syncthing --run ". ${darwinApiKeyShim}"
    '';
    meta.mainProgram = "syncthing";
  };

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

      # Darwin: pin the Syncthing REST API key from sops (issue #2697).
      # See the "Darwin REST API key delivery" comment above for the
      # chosen mechanism and the rejected alternatives.
      #
      # `optionalAttrs` (not `mkIf`) does the platform split, for the
      # same reason as the Linux block above: the home-manager sops
      # module is imported on Darwin only (modules/system/sops.nix),
      # so `home-manager.users.<user>.sops` does not exist on NixOS and
      # `mkIf` would still register the option path there.
      (lib.optionalAttrs (!isLinux) (
        lib.mkIf hasDarwinApiKey {
          home-manager.users.${username} = {
            sops.secrets.${darwinApiKeySecretName} = {
              sopsFile = ./secrets/m4mac-gui-apikey.sops.yaml;
              key = apiKeySecret;
              mode = "0400";
            };

            services.syncthing.package = darwinSyncthingPackage;
          };

          # Alloy runs as a root launchd daemon on Darwin, so it reads
          # this user-owned 0400 file directly. There is no DynamicUser
          # and so no group to grant — `apiKeyGroup` stays null here,
          # unlike on Linux.
          nx.services.syncthing.apiKeyFile = darwinApiKeyPath;
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
