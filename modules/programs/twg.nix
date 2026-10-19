{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.twg = {
      enable = lib.mkEnableOption "enables the Atlassian Teamwork Graph CLI (twg)" // {
        default = false;
      };

      envVars = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        internal = true;
        readOnly = true;
        # The host shell (home.sessionVariables below) and prism agents
        # (agent.envVars in prism/default.nix) both read this attrset. Do not
        # copy the values into a second place: if the copies differ, a login
        # on one side can select the Keychain store.
        default = {
          # Keeps credentials in the plain-file store under ~/.config/twg.
          # The other value, "keychain", puts the root key in the macOS
          # Keychain. The owner does not want Keychain storage.
          TWG_SECRET_STORE = "file";
          # twg reads exactly "1" as the switch that stops runtime telemetry.
          DO_NOT_TRACK = "1";
          # Nix supplies upgrades, so the background update notifier has no use.
          TWG_BACKGROUND_UPDATE_CHECK = "false";
          # twg keeps its help index and last-error.json here. The
          # sandbox-exec profile does not grant the macOS default,
          # ~/Library/Caches/twg. Without this value, twg in an agent session
          # rebuilds the help index on each call. The prism RW grants for
          # ~/.cache/twg (internal/container: mounts.go, sandbox_exec.go)
          # must name the same path. Do not persist this directory: it is a
          # cache.
          TWG_CACHE_DIR = "$HOME/.cache/twg";
        };
        description = "Environment variables for twg, set in the host shell and for prism agents.";
      };
    };
  };
  config = lib.mkIf config.nx.programs.twg.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        pkgs.twg
      ];
      home.sessionVariables = config.nx.programs.twg.envVars;
      home.persistence."/persist" = {
        directories = [
          # OAuth credentials. twg refreshes the 8-hour access token on
          # demand, so the directory must stay writable and survive reboots.
          ".config/twg"
        ];
      };
    };
  };
}
