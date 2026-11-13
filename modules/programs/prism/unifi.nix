{ lib, ... }:
# The UniFi credential (issues #3472 and #3476), the first entry of
# nx.programs.prism.agentCredentials (agent-credentials.nix). The credential
# is the password of a local UniFi OS user with the permissions "Network View
# Only" and "Control Plane View Only". An API key is not used: only an admin
# can create one, and an admin key can write.
#
# A session of a listed repo gets three variables:
#   UNIFI_PASSWORD_FILE - the path of the decrypted unifi_password secret,
#                         masked as [redacted:UNIFI_PASSWORD].
#   UNIFI_HOST          - the value of the unifi_host secret: the host name of
#                         a reverse proxy with a valid certificate.
#   UNIFI_USERNAME      - the local UniFi OS user.
#
# The repos and the username are at default priority. To change them, or to
# give the credential to no repo, set them in a host config:
#   nx.programs.prism.agentCredentials.unifi.repos = [ ];
let
  secretsFile = ../secrets/unifi.sops.yaml;
in
{
  nx.programs.prism.agentCredentials.unifi = {
    repos = lib.mkDefault [
      "home-ops"
      "nixos-config"
      "hass-config"
      "obsidian"
    ];
    files.UNIFI_PASSWORD_FILE = {
      sopsFile = secretsFile;
      key = "unifi_password";
      marker = "UNIFI_PASSWORD";
    };
    values.UNIFI_HOST = {
      sopsFile = secretsFile;
      key = "unifi_host";
    };
    literals.UNIFI_USERNAME = lib.mkDefault "agent-readonly";
    skills = [ ./repo-skills/unifi ];
  };
}
