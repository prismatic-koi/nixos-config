{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.repoSkills = lib.mkOption {
      type = lib.types.attrsOf (lib.types.listOf lib.types.path);
      default = {
        nixos-config = [ ./repo-skills/site-theming ];
      };
      example = lib.literalExpression ''
        {
          nixos-config = [ ./repo-skills/site-theming ];
          home-ops = [ ./repo-skills/flux ];
        }
      '';
      description = ''
        Skills for each repo (issue #3463). Each key is a repo directory
        name. Each value is a list of skill directories, as nix paths. A
        session of the repo gets these skills. A session of another repo does
        not get them, and does not see their names in its context.

        prism starts every pi of a session of the repo with one
        --skill <store-path> for each skill: the coordinator, the workers,
        the review agents, the investigators, and the PR-branch sessions.
        pi loads a --skill path also with --no-approve, because that path is
        not a project resource. So these skills do not change the
        project-trust decision (projectTrust.trustedRepos).

        prism finds the repo on the host, with the repo identity of the
        project-trust decision: the session name, the start directory, and
        the project roots in config.json. The skill content comes from the
        nix store, not from the worktree or the branch. prism drops each path
        that is not in the nix store, or that does not exist, and logs a
        warning in the agent-run log of the session.

        A change to a skill takes effect after a home-manager switch, and
        then at the next start of a session.

        Rendered to ~/.config/prism/repo-skills.json as a map of repo names
        to nix store paths. Neither the bwrap nor the sandbox-exec isolator
        gives a sandbox access to that file, so no sandboxed session can read
        or change it.
      '';
    };
  };

  config = lib.mkIf config.nx.programs.prism.enable {
    # ~/.config/prism/ is persisted on impermanence systems by profiles.nix,
    # so this module declares the file alone. builtins.toJSON copies each
    # path to the nix store, and the store path in the file text keeps the
    # skill alive through garbage collection.
    home-manager.users.${config.nx.username} = {
      xdg.configFile."prism/repo-skills.json".text = builtins.toJSON config.nx.programs.prism.repoSkills;
    };
  };
}
