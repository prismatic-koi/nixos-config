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
          # Darwin + sandbox-exec is deliberately unsupported in v1 — the SBPL
          # profile denies the entire secrets.d subtree with a hand-maintained
          # allowlist in collectSecretsDAllowlistNames (sandbox_exec.go §3c) that
          # does NOT include grafana. Adding it there is a follow-up that needs
          # a positive+negative sandbox-exec integration test pair per the
          # sandbox-exec testing convention. Darwin host-mode does not have this
          # constraint (no SBPL) and is allowed.
          {
            assertions = [
              {
                assertion =
                  !(pkgs.stdenv.isDarwin && config.nx.programs.prism.agent.isolation.default == "sandbox-exec");
                message = ''
                  nx.programs.prism.pi.grafana.enable = true is not yet supported on
                  Darwin under sandbox-exec isolation. The pi grafana extension reads
                  a sops-decrypted secret file whose path lives under
                  ~/.config/sops-nix/secrets/, which the sandbox-exec profile denies
                  by default (see collectSecretsDAllowlistNames in
                  internal/container/sandbox_exec.go). Adding grafana to that
                  allowlist is a follow-up requiring a paired positive+negative
                  integration test per the sandbox-exec testing convention. Options:
                    - Use host isolation on this machine
                      (nx.programs.prism.agent.isolation.default = "host").
                    - Leave grafana disabled on this Darwin host.
                '';
              }
            ];
          }
          # Linux: system-level sops. /run/secrets/<name> is a tmpfs symlink into
          # the concrete /run/secrets.d/<N>/<name> generated at activation. The
          # tmpfs guarantees the decrypted content never touches persistent
          # storage; the file is bound into prism-spawned bwrap sandboxes by
          # internal/container/bwrap.go when cfg.AgentEnvVars["GRAFANA_MCP_CONFIG_PATH"]
          # is set.
          (lib.mkIf pkgs.stdenv.isLinux {
            sops.secrets.${secretName} = {
              owner = username;
              mode = "0600";
              sopsFile = secretsFile;
            };
          })

          # Darwin: home-manager sops. Same shape as container-tokens.nix. Note
          # that the Darwin sandbox-exec profile denies reads on the entire
          # ~/.config/sops-nix/secrets tree with a hand-maintained allowlist in
          # collectSecretsDAllowlistNames (sandbox_exec.go §3c) — grafana is NOT
          # on that allowlist in v1, so pi.grafana.enable = true on Darwin under
          # sandbox-exec is rejected by an assertion in pi.nix. Host-mode Darwin
          # sessions can still read the file, so we still emit the secret so the
          # option shape is symmetric across platforms.
          (lib.mkIf pkgs.stdenv.isDarwin {
            home-manager.users.${username}.sops.secrets.${secretName} = {
              sopsFile = secretsFile;
            };
          })
        ]
      );
}
