{
  config,
  lib,
  pkgs,
  ...
}:
# Gives the UniFi API key to the agent sessions of a set of repos (issue
# #3472). The key is for the local UniFi OS user agent-readonly, which has the
# permissions "Network View Only" and "Control Plane View Only".
#
# Platform split mirrors modules/programs/home-automation.nix:
#   Linux  - sops-nix system secrets under /run/secrets/.
#   Darwin - home-manager sops secrets under ~/.config/sops-nix/secrets/.
let
  username = config.nx.username;
  cfg = config.nx.programs.prism.unifi;
  isDarwin = pkgs.stdenv.hostPlatform.isDarwin;
  secretsFile = ../secrets/unifi.sops.yaml;
  enabled = config.nx.programs.prism.enable && cfg.repos != [ ];
  secretPath =
    name:
    if isDarwin then
      config.home-manager.users.${username}.sops.secrets.${name}.path
    else
      config.sops.secrets.${name}.path;
in
{
  options = {
    nx.programs.prism.unifi.repos = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "home-ops"
        "nixos-config"
        "hass-config"
        "obsidian"
      ];
      example = [ "home-ops" ];
      description = ''
        Repos whose agent sessions get the UniFi API key (issue #3472). Each
        entry is a repo directory name. This option is the only repo list: a
        change to it adds or removes a repo with no change to the prism
        source. An empty list gives the key to no session, and declares no
        UniFi secret.

        A session of a repo in this list gets two environment variables:

        - UNIFI_API_KEY_FILE: the path of the decrypted unifi_api_key
          secret. No variable holds the key itself. The bwrap isolator binds
          the file read-only. The sandbox-exec profile admits a read of the
          file and no write.
        - UNIFI_HOST: the value of the unifi_host secret.

        The coordinator, the workers, and the investigators of the repo get
        the key. The review agents do not get it: a review agent reviews a
        diff on a read-only worktree, and has no use for the UniFi API.

        prism finds the repo on the host, with the repo identity of the
        project-trust decision: the session name, the start directory, and
        the project roots in config.json. If a secret file is missing or
        holds no usable value, the session starts with neither variable,
        and the agent-run log of the session gives the reason.

        The redactor reads the key file, so the key value is masked as
        [redacted:UNIFI_API_KEY] in tool results, in the frames on the
        prism socket, and in prism.db.

        Limit of host mode: a host-mode session has no filesystem
        isolation. prism sets the variables only for the repos in this
        list, but a host-mode session of any repo can read the sops path.

        A change takes effect after a home-manager switch, and then at the
        next start of a session.

        Rendered to ~/.config/prism/unifi.json with the two secret paths.
        Neither the bwrap nor the sandbox-exec isolator gives a sandbox
        access to that file, so no sandboxed session can read or change it.
      '';
    };
  };

  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      (lib.mkIf (enabled && !isDarwin) {
        sops.secrets = {
          unifi_api_key = {
            owner = username;
            mode = "0400";
            sopsFile = secretsFile;
          };
          unifi_host = {
            owner = username;
            mode = "0400";
            sopsFile = secretsFile;
          };
        };
      })

      (lib.mkIf (enabled && isDarwin) {
        home-manager.users.${username}.sops.secrets = {
          unifi_api_key = {
            mode = "0400";
            sopsFile = secretsFile;
          };
          unifi_host = {
            mode = "0400";
            sopsFile = secretsFile;
          };
        };
      })

      {
        # ~/.config/prism/ is persisted on impermanence systems by
        # profiles.nix, so this module declares the file alone.
        home-manager.users.${username}.xdg.configFile."prism/unifi.json".text = builtins.toJSON {
          repos = cfg.repos;
          api_key_path = if enabled then secretPath "unifi_api_key" else "";
          host_path = if enabled then secretPath "unifi_host" else "";
        };
      }
    ]
  );
}
