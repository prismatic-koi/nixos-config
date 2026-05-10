{
  config,
  lib,
  pkgs,
  ...
}:
# Surfaces 4 fine-grained GitHub PATs as session environment variables so that
# the container manager (container.go credentialEnvVars) can select the
# correct token based on the GitHub account and agent role.
#
# Token env var naming convention:
#   PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>
# where ACCOUNT is the GitHub account name with hyphens replaced by underscores,
# uppercased, and ROLE is either WORKER or COORDINATOR.
#
# The Go code in container.go derives the account from the repo's git remote URL
# and builds the env var name at runtime to look up the correct token.
#
# Platform split:
#   Linux  — sops-nix system secrets + environment.sessionVariables (NixOS/systemd).
#   Darwin — home-manager sops secrets + home.sessionVariables (nix-darwin).
let
  username = config.nx.username;
  secretsFile = ../secrets/github.sops.yaml;
in
{
  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      # Linux: system-level sops + environment.sessionVariables
      (lib.mkIf pkgs.stdenv.isLinux {
        sops.secrets = {
          "github_token_prismatic_koi_coordinator" = {
            owner = username;
            mode = "0600";
            sopsFile = secretsFile;
          };
          "github_token_prismatic_koi_worker" = {
            owner = username;
            mode = "0600";
            sopsFile = secretsFile;
          };
          "github_token_thankyou_payroll_coordinator" = {
            owner = username;
            mode = "0600";
            sopsFile = secretsFile;
          };
          "github_token_thankyou_payroll_worker" = {
            owner = username;
            mode = "0600";
            sopsFile = secretsFile;
          };
        };

        environment.sessionVariables = {
          PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR = "$(cat ${config.sops.secrets.github_token_prismatic_koi_coordinator.path})";
          PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER = "$(cat ${config.sops.secrets.github_token_prismatic_koi_worker.path})";
          PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR = "$(cat ${config.sops.secrets.github_token_thankyou_payroll_coordinator.path})";
          PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER = "$(cat ${config.sops.secrets.github_token_thankyou_payroll_worker.path})";
        };
      })

      # Darwin: home-manager sops secrets + home.sessionVariables
      (lib.mkIf pkgs.stdenv.isDarwin {
        home-manager.users.${username} = {
          sops.secrets = {
            "github_token_prismatic_koi_coordinator" = {
              sopsFile = secretsFile;
            };
            "github_token_prismatic_koi_worker" = {
              sopsFile = secretsFile;
            };
            "github_token_thankyou_payroll_coordinator" = {
              sopsFile = secretsFile;
            };
            "github_token_thankyou_payroll_worker" = {
              sopsFile = secretsFile;
            };
          };

          home.sessionVariables = {
            PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.github_token_prismatic_koi_coordinator.path
            })";
            PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.github_token_prismatic_koi_worker.path
            })";
            PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.github_token_thankyou_payroll_coordinator.path
            })";
            PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.github_token_thankyou_payroll_worker.path
            })";
          };
        };
      })
    ]
  );
}
