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
{
  config = lib.mkIf (config.nx.programs.prism.enable && pkgs.stdenv.isLinux) {
    sops.secrets = {
      "github_token_prismatic_koi_coordinator" = {
        owner = config.nx.username;
        mode = "0600";
        sopsFile = ../secrets/github.sops.yaml;
      };
      "github_token_prismatic_koi_worker" = {
        owner = config.nx.username;
        mode = "0600";
        sopsFile = ../secrets/github.sops.yaml;
      };
      "github_token_thankyou_payroll_coordinator" = {
        owner = config.nx.username;
        mode = "0600";
        sopsFile = ../secrets/github.sops.yaml;
      };
      "github_token_thankyou_payroll_worker" = {
        owner = config.nx.username;
        mode = "0600";
        sopsFile = ../secrets/github.sops.yaml;
      };
    };

    environment.sessionVariables = {
      PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR = "$(cat ${config.sops.secrets.github_token_prismatic_koi_coordinator.path})";
      PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER = "$(cat ${config.sops.secrets.github_token_prismatic_koi_worker.path})";
      PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR = "$(cat ${config.sops.secrets.github_token_thankyou_payroll_coordinator.path})";
      PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER = "$(cat ${config.sops.secrets.github_token_thankyou_payroll_worker.path})";
    };
  };
}
