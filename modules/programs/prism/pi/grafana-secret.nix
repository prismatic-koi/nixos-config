{
  config,
  pkgs,
  lib,
  ...
}:
# Declares the sops-nix secret backing the pi Grafana MCP extension. The
# bundle name (grafana_config_<value>) is chosen by
# nx.programs.prism.pi.grafana.config; the extension itself reads the
# decrypted file at runtime via GRAFANA_MCP_CONFIG_PATH.
#
# Platform split mirrors modules/programs/prism/container-tokens.nix:
#   Linux  — sops-nix system secrets under /run/secrets/.
#   Darwin — home-manager sops secrets under ~/.config/sops-nix/secrets/.
#
# The bundle format (dotenv-style KEY=VALUE lines with GRAFANA_URL and
# GRAFANA_API_KEY) is described in
# modules/programs/prism/pi/extensions/grafana/UPSTREAM.md.
let
  username = config.nx.username;
  cfg = config.nx.programs.prism.pi.grafana;
  secretsFile = ../secrets/grafana.sops.yaml;
  secretName = "grafana_config_${cfg.config}";
in
{
  config =
    lib.mkIf (config.nx.programs.prism.enable && config.nx.programs.prism.pi.enable && cfg.enable)
      (
        lib.mkMerge [
          # Linux: system-level sops. /run/secrets/<name> is a tmpfs symlink into
          # the concrete /run/secrets.d/<N>/<name> generated at activation. The
          # tmpfs guarantees the decrypted content never touches persistent
          # storage; the file is bound into prism-spawned bwrap sandboxes by
          # internal/container/bwrap.go when cfg.AgentEnvVars["GRAFANA_MCP_CONFIG_PATH"]
          # is set.
          (lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
            sops.secrets.${secretName} = {
              owner = username;
              mode = "0600";
              sopsFile = secretsFile;
            };
          })

          # Darwin: home-manager sops. Same shape as container-tokens.nix. The
          # secret lands at ~/.config/sops-nix/secrets/<name>, a symlink into
          # the per-user TMPDIR secrets.d/<N>/<name> generation dir.
          #
          # The Darwin sandbox-exec profile denies reads on that whole
          # secrets.d tree with a hand-maintained allowlist in
          # collectSecretsDAllowlistNames (sandbox_exec.go §3c). Since issue
          # #2746 that allowlist admits THIS bundle by name, gated on
          # Config.GrafanaConfigPath — the same path prism injects as
          # GRAFANA_MCP_CONFIG_PATH — so a sandbox-exec session can perform the
          # readFileSync the extension needs. The gate is per-session and
          # role-aware: a host with grafana disabled, or a review role (whose
          # GRAFANA_MCP_CONFIG_PATH is stripped by
          # internal/config/agent_env_roles.go), emits no exception and the
          # bundle stays denied. Darwin host-mode sessions have no SBPL profile
          # and can always read the file.
          (lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
            home-manager.users.${username}.sops.secrets.${secretName} = {
              sopsFile = secretsFile;
            };
          })
        ]
      );
}
