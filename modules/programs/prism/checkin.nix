{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.checkin = {
      privilegedRepos = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ "nixos-config" ];
        example = [
          "nixos-config"
          "platform-infra"
        ];
        description = ''
          Repo names whose coordinator carries the tier-3 `prism checkin`
          troubleshooting privilege (issue #2587).

          `prism checkin` has three permission tiers. A worker reads the review
          agents of its own session only. A coordinator reads its own repo plus
          the coordinators of other repos. A coordinator of a repo named here
          reads any session in any repo, including another coordinator's
          workers and review agents.

          Entries are REPO names, not session names. The privilege attaches to
          the coordinator of each named repo. Keying on repo derives the
          session precisely and keeps wildcards off a privilege list.

          The grant covers `prism checkin` only. It does not extend to
          `prism db query`, `prism spawn`, `prism merge`, or any other verb.
          Every access the privilege admits writes an audit event that records
          the caller, the target, and the time. Read those events with
          `prism audit`, from a host shell or from inside a sandbox. A
          sandboxed caller reads them through the coordinator-only `/audit`
          host-API endpoint (issue #2618).

          The tiers apply on both routes of the verb (issue #2619). A
          sandboxed session meets them on the host-API route; a host-mode
          session has no socket and meets the same predicate on the direct
          CLI route. A privileged read writes an audit event either way.

          An empty list grants the privilege to nobody, which is the behaviour
          prism had before the option existed.

          Rendered to ~/.config/prism/checkin-privileged-repos.json in the same
          manner as profiles.json. Two host-side readers consume it: the
          sidecar at start, for the host-API route, and the prism CLI per
          invocation, for the direct route. The file is deliberately absent
          from every sandbox: the bwrap and sandbox-exec isolators bind only
          agents/ and profiles.json out of ~/.config/prism/, so no agent can
          read or edit its own privilege.
        '';
      };
    };
  };

  config = lib.mkIf config.nx.programs.prism.enable {
    # Write checkin-privileged-repos.json to ~/.config/prism/. The directory is
    # already persisted on impermanence systems by profiles.nix, so this module
    # declares the file alone.
    home-manager.users.${config.nx.username} = {
      xdg.configFile."prism/checkin-privileged-repos.json".text = builtins.toJSON {
        privileged_repos = config.nx.programs.prism.checkin.privilegedRepos;
      };
    };
  };
}
