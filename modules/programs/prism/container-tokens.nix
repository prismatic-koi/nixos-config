{
  config,
  lib,
  pkgs,
  ...
}:
# Surfaces the four fine-grained GitHub PATs (account × role) to prism as
# absolute FILE PATHS in prism's config.json (github_token_paths).  The Go side
# (internal/container/credentials.go) reads the file contents at spawn / use
# time so that token resolution has NO dependency on shell expansion — which
# means the boot-restore path (issue #2340 / PR #2342), where the tmux server
# is started from a systemd user unit without a login shell, produces working
# credentials just like an interactive-shell launch does.
#
# Historical wart being removed here (issue #2348): earlier revisions of this
# module ALSO surfaced the four tokens via `environment.sessionVariables`
# (Linux) / `home.sessionVariables` (Darwin) as `"$(cat <path>)"` strings.
# Command substitution in those values only happens when a login shell sources
# /etc/set-environment (or the HM equivalent).  With tmux launched from a
# systemd unit the literal `$(cat /run/secrets/…)` string propagated into the
# tmux server env and then into every session's GITHUB_TOKEN via
# credentialEnvVars — every gh call 401'd until a `prism restart`.  No other
# consumer read those env vars, so removing the sessionVariables block is a
# pure fix.
#
# Token key format: PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>, where ACCOUNT is the
# GitHub account name with hyphens replaced by underscores and uppercased, and
# ROLE is WORKER or COORDINATOR.  This matches the runtime env-var naming in
# internal/container/credentials.go — the same key is used to look up the file
# path in cfg.GitHubTokenPaths and (as a legacy fallback) the PRISM_GITHUB_TOKEN_*
# env var.
#
# Platform split:
#   Linux  — sops-nix system secrets under /run/secrets/.
#   Darwin — home-manager sops secrets under ~/.config/sops-nix/secrets/.
let
  username = config.nx.username;
  secretsFile = ../secrets/github.sops.yaml;
  tokenNames = {
    PRISMATIC_KOI_COORDINATOR = "github_token_prismatic_koi_coordinator";
    PRISMATIC_KOI_WORKER = "github_token_prismatic_koi_worker";
    THANKYOU_PAYROLL_COORDINATOR = "github_token_thankyou_payroll_coordinator";
    THANKYOU_PAYROLL_WORKER = "github_token_thankyou_payroll_worker";
  };
in
{
  options = {
    nx.programs.prism.githubTokenPaths = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      internal = true;
      description = ''
        Absolute host paths to the four sops-decrypted GitHub token files,
        keyed by <ACCOUNT>_<ROLE> (matching the PRISM_GITHUB_TOKEN_* env-var
        naming convention).  Populated by container-tokens.nix and consumed
        by prism-tui.nix, which serialises the map into config.json under
        the `github_token_paths` key.  Never set by user configuration —
        the source of truth is the sops.secrets definitions in
        container-tokens.nix.
      '';
    };
  };

  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      # Linux: system-level sops secrets.  Nothing consumes an env var for
      # these tokens on Linux any more — prism reads the file directly.
      (lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
        sops.secrets = lib.mapAttrs' (
          _key: name:
          lib.nameValuePair name {
            owner = username;
            mode = "0600";
            sopsFile = secretsFile;
          }
        ) tokenNames;

        nx.programs.prism.githubTokenPaths = lib.mapAttrs (
          _key: name: config.sops.secrets.${name}.path
        ) tokenNames;
      })

      # Darwin: home-manager sops secrets.  Same story — the paths are
      # threaded into prism's config.json and read at spawn/use time.
      (lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
        home-manager.users.${username} = {
          sops.secrets = lib.mapAttrs' (
            _key: name:
            lib.nameValuePair name {
              sopsFile = secretsFile;
            }
          ) tokenNames;
        };

        nx.programs.prism.githubTokenPaths = lib.mapAttrs (
          _key: name: config.home-manager.users.${username}.sops.secrets.${name}.path
        ) tokenNames;
      })
    ]
  );
}
