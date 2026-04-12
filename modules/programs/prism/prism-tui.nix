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
    # Disabled pending container-mode fixes — see #595, #596, #597
    container_mode = false;
    sidecar_plugin_path = "${
      config.home-manager.users.${config.nx.username}.xdg.configHome
    }/opencode/plugins/prism-hooks.ts";
    container_worker_config = config.nx.programs.prism.containerWorkerConfigPath;
    container_coordinator_config = config.nx.programs.prism.containerCoordinatorConfigPath;
    worktree_exclude = config.nx.programs.prism.worktreeExclude;
    project_locations = config.nx.programs.prism.projects.locations;
    project_specific = config.nx.programs.prism.projects.specific;
    git_user_name = gitUserName;
    git_user_email = gitUserEmail;
    ssh_access_key_name = config.nx.programs.prism.sshAccessKeyName;
    ssh_signing_key_name = config.nx.programs.prism.sshSigningKeyName;
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
  };

  config = lib.mkIf (config.nx.programs.prism.tui.enable && config.nx.programs.prism.enable) {
    home-manager.users.${config.nx.username} = {
      home.packages = [
        (pkgs.callPackage ../../../pkgs/prism.nix { })
      ];

      xdg.configFile."prism/config.json".text = builtins.toJSON prismConfig;

      programs.zsh.shellAliases = {
        gwc = "prism clone";
      };
    };
  };
}
