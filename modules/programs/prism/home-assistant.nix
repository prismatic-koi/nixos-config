{ lib, ... }:
# The Home Assistant credential (issue #3478), an entry of
# nx.programs.prism.agentCredentials (agent-credentials.nix). The credential
# is a long-lived access token of a non-admin Home Assistant user. The token
# can read state and call services. It cannot change the system config.
#
# A session of a listed repo gets two variables:
#   HASS_TOKEN_FILE - the path of the decrypted hass_agent_api_key secret,
#                     masked as [redacted:HASS_TOKEN].
#   HASS_DOMAIN     - the value of the hass_domain secret: the host name of
#                     Home Assistant.
#
# home-automation.nix also declares hass_domain. Its owner and mode are the
# final values, so the secret stays at mode 0600 on Linux.
#
# The repos are at default priority. To change them, or to give the
# credential to no repo, set them in a host config:
#   nx.programs.prism.agentCredentials.home-assistant.repos = [ ];
let
  secretsFile = ../secrets/home-assistant.sops.yaml;
in
{
  nx.programs.prism.agentCredentials.home-assistant = {
    repos = lib.mkDefault [
      "home-ops"
      "nixos-config"
      "hass-config"
      "obsidian"
    ];
    files.HASS_TOKEN_FILE = {
      sopsFile = secretsFile;
      key = "hass_agent_api_key";
      marker = "HASS_TOKEN";
    };
    values.HASS_DOMAIN = {
      sopsFile = secretsFile;
      key = "hass_domain";
    };
    skills = [ ./repo-skills/home-assistant ];
  };
}
