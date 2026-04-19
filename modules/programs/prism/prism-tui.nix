{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  gitCfg = config.home-manager.users.${config.nx.username}.programs.git;
  # Extract the first includes entry that has user.name and user.email set.
  # This mirrors the values defined in modules/programs/git.nix.
  gitUserName =
    let
      names = lib.concatMap (
        inc:
        lib.optional (
          inc ? contents && inc.contents ? user && inc.contents.user ? name
        ) inc.contents.user.name
      ) gitCfg.includes;
    in
    if names != [ ] then builtins.head names else "";
  gitUserEmail =
    let
      emails = lib.concatMap (
        inc:
        lib.optional (
          inc ? contents && inc.contents ? user && inc.contents.user ? email
        ) inc.contents.user.email
      ) gitCfg.includes;
    in
    if emails != [ ] then builtins.head emails else "";
  isolationDefault = config.nx.programs.prism.agent.isolation.default;
  prismConfig = {
    color_primary = primary;
    color_secondary = secondary;
    color_purple = purple;
    color_yellow = yellow;
    color_green = green;
    color_blue = blue;
    color_red = red;
    color_foreground = foreground;
    color_bg0 = bg0;
    kitty_bin = "${pkgs.kitty}/bin/kitty";
    # default_isolation_mode is the authoritative field; container_mode is
    # derived from it for back-compat with Go code that reads the old field.
    default_isolation_mode = isolationDefault;
    container_mode = isolationDefault == "podman";
    sidecar_plugin_path = "${
      config.home-manager.users.${config.nx.username}.xdg.configHome
    }/opencode/plugins/prism-hooks.ts";
    worktree_exclude = config.nx.programs.prism.worktreeExclude;
    project_locations = config.nx.programs.prism.projects.locations;
    project_specific = config.nx.programs.prism.projects.specific;
    git_user_name = gitUserName;
    git_user_email = gitUserEmail;
    ssh_access_key_name = config.nx.programs.prism.sshAccessKeyName;
    ssh_signing_key_name = config.nx.programs.prism.sshSigningKeyName;
    restore_stagger_delay_ms = config.nx.programs.prism.restoreStaggerDelayMs;
    sidecar_circuit_breaker_threshold = config.nx.programs.prism.sidecarCircuitBreakerThreshold;
  };
in
{
  options = {
    nx.programs.prism.tui.enable = lib.mkEnableOption "enables prism Go TUI binary" // {
      default = true;
    };

    nx.programs.prism.sshAccessKeyName = lib.mkOption {
      type = lib.types.str;
      default = "prismatic-koi-ed25519";
      description = ''
        Filename (not full path) of the SSH access key in ~/.ssh/ to mount
        into prism containers. Defaults to "prismatic-koi-ed25519".
      '';
    };

    nx.programs.prism.sshSigningKeyName = lib.mkOption {
      type = lib.types.str;
      default = "prismatic-koi-ed25519-signingkey";
      description = ''
        Base filename (not full path) of the SSH signing key in ~/.ssh/ to
        mount into prism containers. The public key is derived by appending
        ".pub". Defaults to "prismatic-koi-ed25519-signingkey".
      '';
    };

    nx.programs.prism.restoreStaggerDelayMs = lib.mkOption {
      type = lib.types.int;
      default = 0;
      description = ''
        Delay in milliseconds inserted between successive session creates in
        `prism restore`, to flatten the podman startup burst on machines with
        many sessions. 0 means use the compiled-in default (500ms). Set to a
        negative value to disable the stagger entirely.
      '';
    };

    nx.programs.prism.sidecarCircuitBreakerThreshold = lib.mkOption {
      type = lib.types.int;
      default = 0;
      description = ''
        Number of consecutive non-successful sidecar exits that causes
        `prism restore` to skip re-spawning a session. 0 means use the
        compiled-in default (3). Set to a negative value to disable the
        circuit breaker entirely.
      '';
    };
  };

  config = lib.mkIf (config.nx.programs.prism.tui.enable && config.nx.programs.prism.enable) {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        (pkgs.callPackage ../../../pkgs/prism.nix { })
      ]
      # bubblewrap is required for bwrap isolation mode on Linux.
      # Included whenever the prism TUI is enabled on Linux so that the
      # binary is available even before a user opts in to bwrap mode.
      ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.bubblewrap ];

      xdg.configFile."prism/config.json".text = builtins.toJSON prismConfig;

      programs.zsh.shellAliases = {
        gwc = "prism clone";
      };
    };
  };
}
