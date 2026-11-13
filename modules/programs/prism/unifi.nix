{
  config,
  lib,
  pkgs,
  ...
}:
# Gives the UniFi credential to the agent sessions of a set of repos (issues
# #3472 and #3476), and gives those sessions the unifi repo skill. The
# credential is the password of a local UniFi OS user with the permissions
# "Network View Only" and "Control Plane View Only". An API key is not used:
# only an admin can create one, and an admin key can write.
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
        Repos whose agent sessions get the UniFi credential (issues #3472
        and #3476). Each entry is a repo directory name. This option is the
        only repo list: a change to it adds or removes a repo with no change
        to the prism source. An empty list gives the credential to no
        session, and declares no UniFi secret.

        A session of a repo in this list gets three environment variables:

        - UNIFI_PASSWORD_FILE: the path of the decrypted unifi_password
          secret. No variable holds the password itself. The bwrap isolator
          binds the file read-only. The sandbox-exec profile admits a read
          of the file and no write.
        - UNIFI_USERNAME: the value of nx.programs.prism.unifi.username.
        - UNIFI_HOST: the value of the unifi_host secret.

        Each repo in this list also gets the repo skill
        repo-skills/unifi through nx.programs.prism.repoSkills. The skill
        tells the agent how to log in and which endpoints to read.

        The coordinator, the workers, and the investigators of the repo get
        the credential. The review agents do not get it: a review agent
        reviews a diff on a read-only worktree, and has no use for the UniFi
        API.

        prism finds the repo on the host, with the repo identity of the
        project-trust decision: the session name, the start directory, and
        the project roots in config.json. If a secret file is missing or
        holds no usable value, the session starts with no UniFi variable,
        and the agent-run log of the session gives the reason.

        The redactor reads the password file, so the password value is
        masked as [redacted:UNIFI_PASSWORD] in tool results, in the frames
        on the prism socket, and in prism.db.

        Limit of host mode: a host-mode session has no filesystem
        isolation. prism sets the variables only for the repos in this
        list, but a host-mode session of any repo can read the sops path.

        A change takes effect after a home-manager switch, and then at the
        next start of a session.

        Rendered to ~/.config/prism/unifi.json with the username and the two
        secret paths.
        Neither the bwrap nor the sandbox-exec isolator gives a sandbox
        access to that file, so no sandboxed session can read or change it.
      '';
    };

    nx.programs.prism.unifi.username = lib.mkOption {
      type = lib.types.str;
      default = "agent-readonly";
      description = ''
        The local UniFi OS user of the credential (issue #3476). A session of
        a repo in nx.programs.prism.unifi.repos gets it as UNIFI_USERNAME.
        The unifi_password secret holds the password of this user. The user
        must have read access only. If the value is empty or holds a space
        or a control character, no session gets a UniFi variable.
      '';
    };
  };

  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      (lib.mkIf (enabled && !isDarwin) {
        sops.secrets = {
          unifi_password = {
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
          unifi_password = {
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
        nx.programs.prism.repoSkills = lib.genAttrs cfg.repos (_: [ ./repo-skills/unifi ]);

        # ~/.config/prism/ is persisted on impermanence systems by
        # profiles.nix, so this module declares the file alone.
        home-manager.users.${username}.xdg.configFile."prism/unifi.json".text = builtins.toJSON {
          repos = cfg.repos;
          username = cfg.username;
          password_path = if enabled then secretPath "unifi_password" else "";
          host_path = if enabled then secretPath "unifi_host" else "";
        };
      }
    ]
  );
}
