{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.projectTrust = {
      trustedRepos = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [
          "home-ops"
          "obsidian"
        ];
        example = [
          "home-ops"
          "obsidian"
          "dotfiles"
        ];
        description = ''
          Repo directory names whose prism sessions load pi project resources
          (issue #3216). Project resources are the files that pi loads from
          its working directory: .pi/extensions, .pi/skills, .pi/prompts,
          .pi/themes, .pi/settings.json, .pi/mcp.json, .pi/SYSTEM.md,
          .pi/APPEND_SYSTEM.md, and .agents/skills in the directory and its
          ancestors. An extension runs with the rights of the session.

          prism starts every pi with --approve or --no-approve. It decides on
          the host, when it builds the pi command line. pi gets --approve only
          when all of these are true:

          - The session starts in a worktree root of a bare+worktree layout,
            or in the directory of a project with no worktrees.
          - That project is a prism project: a directory in
            nx.programs.prism.projects.locations, or a path in
            nx.programs.prism.projects.specific.
          - The repo part of the session name matches the name of the project
            directory.
          - The name of the project directory is in this list.
          - The session is not a PR-branch session (see below).

          Every other pi that prism starts gets --no-approve. A command-line
          flag wins over every other trust source of pi, so an entry in
          ~/.pi/agent/trust.json does not change the decision for a session
          that prism starts. pi that a person starts outside prism follows
          defaultProjectTrust in pi.nix ("ask").

          Known limit: in a trusted repo, a worker can write .pi resources in
          its own worktree, and the sessions that start there (the worker and
          its review agents) load them. A worker cannot write the other
          worktrees of its repo or .agents/skills at the project directory
          (issue #3293). It can write the shared git object store, so a later
          checkout in another worktree can write a file that it changed
          (issue #3317). Under bwrap, a worker can also write .bare/config
          and .bare/hooks (issue #3315).

          PR-branch sessions (issue #3287): `prism pr` and `prism spawn --pr`
          start a session on the branch of an existing PR. prism cannot know
          who wrote that branch: the author can be Renovate, a fork, or a
          colleague. So that session gets --no-approve, also in a repo in
          this list. Its review agents, its investigators, and each other
          session in its worktree also get --no-approve. The session keeps
          --no-approve after prism restore. prism records the session in
          prism.db on the host, before the agent starts. No sandbox can
          reach prism.db. A later `prism spawn` of the same session name
          with no --pr removes the record.

          Rendered to ~/.config/prism/project-trust.json. Neither the bwrap
          nor the sandbox-exec isolator gives a sandbox access to that file,
          so no sandboxed session can read or change it. An empty list makes
          every pi that prism starts get --no-approve.
        '';
      };
    };
  };

  config = lib.mkIf config.nx.programs.prism.enable {
    # ~/.config/prism/ is persisted on impermanence systems by profiles.nix,
    # so this module declares the file alone.
    home-manager.users.${config.nx.username} = {
      xdg.configFile."prism/project-trust.json".text = builtins.toJSON {
        trusted_repos = config.nx.programs.prism.projectTrust.trustedRepos;
      };
    };
  };
}
