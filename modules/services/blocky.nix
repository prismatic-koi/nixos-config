{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.services.blocky.enable =
      lib.mkEnableOption "Add a local blocky DNS server and set it as the default nameserver"
      // {
        default = false;
      };
  };
  config = lib.mkIf (config.nx.services.blocky.enable && pkgs.stdenv.isLinux) {
    environment.systemPackages = with pkgs; [
      blocky # nice to also have the cli tool
    ];
    networking.nameservers = [
      "127.0.0.1"
    ];
    services.blocky = {
      enable = true;
      settings = {
        ports = {
          dns = "127.0.0.1:53";
          http = "127.0.0.1:4000";
        };
        bootstrapDns = {
          upstream = "https://one.one.one.one/dns-query";
          ips = [
            "1.1.1.1"
            "1.0.0.1"
          ];
        };
        upstreams.groups.default = [
          "tcp-tls:one.one.one.one:853"
          "tcp-tls:dns.quad9.net:853"
        ];
        blocking = {
          loading = {
            # download more lists in parallel
            concurrency = 10;
          };
          denylists = {
            suspicious = [
              "https://raw.githubusercontent.com/PolishFiltersTeam/KADhosts/master/KADhosts.txt"
              "https://raw.githubusercontent.com/FadeMind/hosts.extras/master/add.Spam/hosts"
              "https://v.firebog.net/hosts/static/w3kbl.txt"
            ];
            ads = [
              "https://adaway.org/hosts.txt"
              "https://v.firebog.net/hosts/AdguardDNS.txt"
              "https://v.firebog.net/hosts/Admiral.txt"
              "https://raw.githubusercontent.com/anudeepND/blacklist/master/adservers.txt"
              "https://s3.amazonaws.com/lists.disconnect.me/simple_ad.txt"
              "https://v.firebog.net/hosts/Easylist.txt"
              "https://pgl.yoyo.org/adservers/serverlist.php?hostformat=hosts&showintro=0&mimetype=plaintext"
              "https://raw.githubusercontent.com/FadeMind/hosts.extras/master/UncheckyAds/hosts"
              "https://raw.githubusercontent.com/bigdargon/hostsVN/master/hosts"
            ];
            trackers = [
              "https://v.firebog.net/hosts/Prigent-Ads.txt"
              "https://raw.githubusercontent.com/FadeMind/hosts.extras/master/add.2o7Net/hosts"
              "https://raw.githubusercontent.com/crazy-max/WindowsSpyBlocker/master/data/hosts/spy.txt"
              "https://hostfiles.frogeye.fr/firstparty-trackers-hosts.txt"
            ];
            misc = [
              "https://osint.digitalside.it/Threat-Intel/lists/latestdomains.txt"
              "https://s3.amazonaws.com/lists.disconnect.me/simple_malvertising.txt"
              "https://v.firebog.net/hosts/Prigent-Crypto.txt"
              "https://raw.githubusercontent.com/FadeMind/hosts.extras/master/add.Risk/hosts"
              "https://v.firebog.net/hosts/RPiList-Malware.txt"
              "https://v.firebog.net/hosts/RPiList-Phishing.txt"
              "https://raw.githubusercontent.com/Spam404/lists/master/main-blacklist.txt"
              "https://raw.githubusercontent.com/AssoEchap/stalkerware-indicators/master/generated/hosts"
            ];
          };
          allowlists = {
            suspicious = [
              "https://raw.githubusercontent.com/anudeepND/whitelist/master/domains/whitelist.txt"
            ];
            ads = [
              "https://raw.githubusercontent.com/anudeepND/whitelist/master/domains/whitelist.txt"
            ];
            trackers = [
              "https://raw.githubusercontent.com/anudeepND/whitelist/master/domains/whitelist.txt"
            ];
            misc = [
              "https://raw.githubusercontent.com/anudeepND/whitelist/master/domains/whitelist.txt"
            ];
          };
          clientGroupsBlock.default = [
            "suspicious"
            "ads"
            "trackers"
            "misc"
          ];
        };
      };
    };
    # don't start blocky until network is online
    systemd.services.blocky = {
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
    };
    home-manager.users.ben.home = {
      # making sure scripts are on path if not set elsewhere
      sessionPath = [ "$HOME/.local/scripts" ];
      # utility scripts for blocky
      file.".local/scripts/services.blocky.pause" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            response=$(${pkgs.blocky}/bin/blocky blocking disable --duration 10m --groups default 2>&1)
            cleaned_response=$(echo "$response" | awk '{print $NF}')

            if [ "$cleaned_response" = "OK" ]; then
              notify-send --expire-time 5000 "Blocky" "Blocky is disabled for 10 minutes"
            else
              notify-send "Blocky" "Error: $response"
            fi
          '';
      };
    };
  };
}
