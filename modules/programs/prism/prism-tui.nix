{
  config,
  lib,
  pkgs,
  ...
}:
with config.themev2;
let
  username = config.nx.username;
  # Absolute path to the sops-decrypted GitHub token secret file. Threaded into
  # config.json (github_token_path) so prism's credentialEnvVars can read the
  # token directly when the inherited GITHUB_TOKEN env var is empty — the Darwin
  # sops launchd decrypt race freezes an empty value into the tmux server env
  # (#2029). Platform split mirrors modules/programs/git.nix:
  #   Linux  — system sops secret.
  #   Darwin — home-manager sops secret.
  githubTokenPath =
    if pkgs.stdenv.hostPlatform.isDarwin then
      config.home-manager.users.${username}.sops.secrets.github_token.path
    else
      config.sops.secrets.github_token.path;
  # Absolute path to the sops-decrypted GitLab token secret file, or "" when
  # the host does not enable the GitLab CLI. Threaded into config.json
  # (gitlab_token_path) so prism reads the token at spawn time and injects
  # GITLAB_TOKEN into the agent sandbox as a value — the host env var is
  # rendered as a shell command substitution and only expands under a login
  # shell (#2348). The same path admits `gitlab_token` to the sandbox-exec
  # secrets.d allowlist. See issue #2668. The platform split mirrors
  # githubTokenPath above.
  gitlabTokenPath =
    if !config.nx.programs.gitlab-cli.enable then
      ""
    else if pkgs.stdenv.hostPlatform.isDarwin then
      config.home-manager.users.${username}.sops.secrets.gitlab_token.path
    else
      config.sops.secrets.gitlab_token.path;
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
    color_primary = roles.primary;
    color_secondary = roles.secondary;
    color_purple = hues.purple;
    color_yellow = hues.yellow;
    color_green = hues.green;
    color_blue = hues.blue;
    color_red = hues.red;
    color_foreground = neutrals.foreground;
    color_bg0 = neutrals.background_0;
    kitty_bin = "${pkgs.kitty}/bin/kitty";
    default_isolation_mode = isolationDefault;
    # sidecar_plugin_path: unused since container isolation removal; kept for forward compat.
    sidecar_plugin_path = "";
    worktree_exclude = config.nx.programs.prism.worktreeExclude;
    project_locations = config.nx.programs.prism.projects.locations;
    project_specific = config.nx.programs.prism.projects.specific;
    project_isolation_overrides = config.nx.programs.prism.projects.isolationOverrides;
    git_user_name = gitUserName;
    git_user_email = gitUserEmail;
    ssh_access_key_name = config.nx.programs.prism.sshAccessKeyName;
    ssh_signing_key_name = config.nx.programs.prism.sshSigningKeyName;
    # ssh_bin: absolute path to Nix-built openssh. Used as GIT_SSH_COMMAND in
    # sandbox-exec sessions to bypass Apple's libnetwork.dylib (which needs
    # system-network sandbox rules that are hard to replicate). Nix openssh
    # uses its own libresolv/libldns and resolves hostnames without any of that.
    ssh_bin = "${pkgs.openssh}/bin/ssh";
    restore_stagger_delay_ms = config.nx.programs.prism.restoreStaggerDelayMs;
    bwrap_concurrency_cap = config.nx.programs.prism.bwrapConcurrencyCap;
    sandbox_exec_concurrency_cap = config.nx.programs.prism.sandboxExecConcurrencyCap;
    # agent_max_open_files_soft/hard: per-process RLIMIT_NOFILE caps applied
    # to agent processes spawned via the bwrap and sandbox-exec exec paths
    # (Layer 1 FD isolation, #2190). Kernel-enforced — the agent cannot raise
    # the hard cap from inside its sandbox. Host-mode agents are not capped.
    agent_max_open_files_soft = config.nx.programs.prism.agentMaxOpenFilesSoft;
    agent_max_open_files_hard = config.nx.programs.prism.agentMaxOpenFilesHard;
    pi_extension_dir = config.nx.programs.prism.piExtensionDir;
    # github_token_path: absolute path to the sops-decrypted GitHub token file.
    # Last-resort fallback read by credentialEnvVars when the inherited
    # GITHUB_TOKEN is empty (Darwin sops decrypt race, #2029).
    github_token_path = githubTokenPath;
    # github_token_paths: absolute paths to the four fine-grained PAT files
    # keyed by <ACCOUNT>_<ROLE> (PRISMATIC_KOI_WORKER, THANKYOU_PAYROLL_COORDINATOR,
    # …).  credentialEnvVars reads the file at spawn time so token resolution
    # never depends on shell expansion — the root cause of #2348, where the
    # boot-restore path started tmux from a systemd unit and every session's
    # GITHUB_TOKEN was frozen to the literal string $(cat /run/secrets/…).
    github_token_paths = config.nx.programs.prism.githubTokenPaths;
    # gitlab_token_path: absolute path to the sops-decrypted GitLab token
    # file, or "" when nx.programs.gitlab-cli.enable is false. Read by
    # credentialEnvVars at spawn time to inject GITLAB_TOKEN into the agent
    # sandbox, and used by the sandbox-exec profile generator as the source
    # that admits `gitlab_token` to the secrets.d allowlist (#2668).
    gitlab_token_path = gitlabTokenPath;
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
        `prism restore`, to flatten the sidecar startup burst on machines with
        many sessions. 0 means use the compiled-in default (500ms). Set to a
        negative value to disable the stagger entirely.
      '';
    };

    nx.programs.prism.bwrapConcurrencyCap = lib.mkOption {
      type = lib.types.int;
      default = 50;
      description = ''
        Maximum number of concurrent bwrap sessions (agent_status rows with
        ended_at IS NULL AND isolation_mode = 'bwrap') before new bwrap spawns
        are refused. 0 means uncapped. Default of 50 is conservative enough for
        any machine without an explicit per-machine override.
      '';
    };

    nx.programs.prism.sandboxExecConcurrencyCap = lib.mkOption {
      type = lib.types.int;
      default = 50;
      description = ''
        Maximum number of concurrent sandbox-exec sessions (agent_status rows
        with ended_at IS NULL AND isolation_mode = 'sandbox-exec') before new
        sandbox-exec spawns are refused. 0 means uncapped. Default of 50
        mirrors bwrapConcurrencyCap. Darwin-only isolation mode; this option
        is rendered into config.json on all machines but only used on Darwin.
      '';
    };

    nx.programs.prism.agentMaxOpenFilesSoft = lib.mkOption {
      type = lib.types.int;
      default = 8192;
      description = ''
        Soft RLIMIT_NOFILE cap applied to agent processes spawned via the
        bwrap and sandbox-exec exec paths (Layer 1 FD isolation, issue
        #2190). Written to config.json as agent_max_open_files_soft. Must
        not exceed agentMaxOpenFilesHard (prism clamps it down at exec time
        if it does). Zero or negative values fall back to the compiled-in
        default (8192). Host-mode agents are not capped.
      '';
    };

    nx.programs.prism.agentMaxOpenFilesHard = lib.mkOption {
      type = lib.types.int;
      default = 16384;
      description = ''
        Hard RLIMIT_NOFILE cap applied to agent processes spawned via the
        bwrap and sandbox-exec exec paths (Layer 1 FD isolation, issue
        #2190). Kernel-enforced: the agent cannot raise it from inside its
        sandbox (ulimit -n above this value fails with EPERM). Written to
        config.json as agent_max_open_files_hard. Values above the host's
        hard limit are clamped to the host hard limit with a warning in the
        agent-run log. Zero or negative values fall back to the compiled-in
        default (16384). Host-mode agents are not capped.
      '';
    };

    nx.programs.prism.piExtensionDir = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''
        Absolute host path to the directory containing the prism PI extension
        file(s) (prism.ts). When non-empty, this path is written to config.json
        as pi_extension_dir and bind-mounted read-only into the bwrap sandbox
        at /etc/prism/pi-extensions/ for harness=pi sessions. Must be set when
        using harness=pi isolation. Defaults to "" (unset; PI sessions will fail
        with a clear error until this is configured).
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

      # Go module cache — persist across the boot wipe (issue #2731).
      #
      # A bwrap sandbox binds ~/go/pkg/mod (GOMODCACHE) and ~/.cache/go-build
      # (GOCACHE) read-write, so a session reuses the host caches instead of
      # rebuilding cold. That grant needs the host directories to survive a
      # reboot, and only one of the two does by default: modules/system/
      # impermanence.nix persists ".cache" wholesale, so GOCACHE is covered,
      # while ~/go is not persisted at all. The root btrfs subvolume is
      # recreated on every boot (the wipe-root service), so without this entry
      # GOMODCACHE is destroyed at each boot and every session after a reboot
      # re-downloads the whole module set — and each boot's copy is retained
      # in old_roots for 14 days.
      #
      # The LEAF only, never ~/go. ~/go/bin holds `go install` output that is
      # typically on the host PATH. The sandbox grant is the leaf for that
      # reason (internal/container/go_cache.go), and the persistence entry
      # holds the same line.
      #
      # Declared here rather than in modules/system/impermanence.nix to match
      # the repo pattern: each module persists the paths it owns (pi.nix
      # persists .pi, anki.nix persists .local/share/Anki2). On Darwin the
      # option is a declared no-op (modules/darwin/impermanence-stub.nix) and
      # there is no boot wipe, so no guard is needed.
      home.persistence."/persist" = {
        directories = [ "go/pkg/mod" ];
      };

      programs.zsh.shellAliases = {
        gwc = "prism clone";
      };
    };
  };
}
