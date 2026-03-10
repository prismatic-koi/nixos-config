{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.programs.wgnord.enable = lib.mkEnableOption "wgnord NordVPN WireGuard client";
  };

  config = lib.mkIf config.nx.programs.wgnord.enable {
    # Ensure WireGuard is available
    networking.wireguard.enable = true;

    # Allow traffic on wgnord interface
    networking.firewall.trustedInterfaces = [ "wgnord" ];

    # Make wgnord available system-wide
    environment.systemPackages = [
      pkgs.wgnord
    ];

    # Helper scripts for VPN management
    home-manager.users.${config.nx.username} = {
      home.sessionPath = [ "$HOME/.local/scripts" ];

      home.file.".local/scripts/cli.system.vpnStatus" = {
        executable = true;
        text = /* bash */ ''
          #!/bin/sh

          # Returns 0 if VPN is genuinely passing traffic, 1 otherwise.
          # Two checks must both pass:
          #   1. WireGuard last-handshake is within 3 minutes (stale = lost connectivity)
          #   2. NordVPN DNS (inside the tunnel) responds to a ping via the wgnord interface

          NORDVPN_DNS="103.86.96.100"
          HANDSHAKE_MAX_AGE=180  # seconds

          check_vpn() {
            # Interface must exist
            if ! ${pkgs.iproute2}/bin/ip link show wgnord > /dev/null 2>&1; then
              echo "no_interface"
              return 1
            fi

            # Must have an endpoint configured
            server_ip="$(sudo ${pkgs.wireguard-tools}/bin/wg show wgnord endpoints 2>/dev/null \
              | ${pkgs.gawk}/bin/awk '{print $2}' \
              | ${pkgs.coreutils}/bin/cut -d: -f1)"
            if [ -z "$server_ip" ]; then
              echo "no_endpoint"
              return 1
            fi

            # Handshake must be recent (wg reports seconds-since-epoch of last handshake)
            last_handshake="$(sudo ${pkgs.wireguard-tools}/bin/wg show wgnord latest-handshakes 2>/dev/null \
              | ${pkgs.gawk}/bin/awk '{print $2}')"
            if [ -z "$last_handshake" ] || [ "$last_handshake" -eq 0 ]; then
              echo "no_handshake"
              return 1
            fi
            now="$(${pkgs.coreutils}/bin/date +%s)"
            age=$(( now - last_handshake ))
            if [ "$age" -gt "$HANDSHAKE_MAX_AGE" ]; then
              echo "stale_handshake:$age"
              return 1
            fi

            # Ping NordVPN DNS through the wgnord interface to confirm traffic routing
            if ! ${pkgs.iputils}/bin/ping -I wgnord -c 1 -W 3 "$NORDVPN_DNS" > /dev/null 2>&1; then
              echo "ping_failed"
              return 1
            fi

            echo "ok:$server_ip"
            return 0
          }

          result="$(check_vpn)"
          status=$?

          if [ "$1" = "json" ]; then
            # JSON output for waybar
            server_ip="$(sudo ${pkgs.wireguard-tools}/bin/wg show wgnord endpoints 2>/dev/null \
              | ${pkgs.gawk}/bin/awk '{print $2}' \
              | ${pkgs.coreutils}/bin/cut -d: -f1)"
            if [ $status -eq 0 ]; then
              printf '{"text": "󰌾", "class": "connected", "tooltip": "VPN Connected: %s"}' "$server_ip"
            else
              case "$result" in
                no_interface)   tooltip="VPN Disconnected" ;;
                no_endpoint)    tooltip="Interface up but no endpoint" ;;
                no_handshake)   tooltip="No WireGuard handshake yet" ;;
                stale_handshake:*) age="''${result#stale_handshake:}"; tooltip="Handshake stale (''${age}s ago) — reconnecting?" ;;
                ping_failed)    tooltip="Interface up but traffic not routing through VPN" ;;
                *)              tooltip="VPN status unknown" ;;
              esac
              if [ "$result" = "no_interface" ]; then
                printf '{"text": "󰌿", "class": "disconnected", "tooltip": "%s"}' "$tooltip"
              else
                printf '{"text": "󰌾", "class": "error", "tooltip": "%s"}' "$tooltip"
              fi
            fi
          else
            # Human-readable output
            if [ $status -eq 0 ]; then
              server_ip="''${result#ok:}"
              printf "\033[32mConnected to: %s\n\033[0m" "$server_ip"
              exit 0
            else
              case "$result" in
                no_interface)
                  printf "\033[31mNo active VPN connection\n\033[0m"
                  ;;
                no_endpoint)
                  printf "\033[33mInterface up but no endpoint configured\n\033[0m"
                  ;;
                no_handshake)
                  printf "\033[33mNo WireGuard handshake established yet\n\033[0m"
                  ;;
                stale_handshake:*)
                  age="''${result#stale_handshake:}"
                  printf "\033[33mHandshake stale (%ss ago) — likely lost connectivity\n\033[0m" "$age"
                  ;;
                ping_failed)
                  printf "\033[31mInterface up but traffic is NOT routing through VPN\n\033[0m"
                  ;;
                *)
                  printf "\033[31mVPN status unknown\n\033[0m"
                  ;;
              esac
              exit 1
            fi
          fi
        '';
      };

      home.file.".local/scripts/system.networking.vpnConnect" = {
        executable = true;
        text = /* bash */ ''
          #!/bin/sh
          sudo ${pkgs.wgnord}/bin/wgnord c nz
          ${pkgs.libnotify}/bin/notify-send -i security-high -t 2000 -e NordVPN "Connected to vpn"
        '';
      };

      home.file.".local/scripts/system.networking.vpnDisconnect" = {
        executable = true;
        text = /* bash */ ''
          #!/bin/sh
          sudo ${pkgs.wgnord}/bin/wgnord d
          ${pkgs.libnotify}/bin/notify-send -i security-low -t 2000 -e NordVPN "Disconnected from vpn"
        '';
      };
    };

    # Create wgnord configuration directory with proper permissions
    systemd.tmpfiles.rules = [
      "d /var/lib/wgnord 0755 root root - -"
    ];

    # Create default template.conf
    environment.etc."wgnord-template.conf" = {
      text = ''
        [Interface]
        PrivateKey = PRIVKEY
        Address = 10.5.0.2/32
        DNS = 103.86.96.100,103.86.99.100
        PostUp = ip route add SERVER_IP via $(ip route | awk '/default/ { print $3 }') dev $(ip route | awk '/default/ { print $5 }') || true; ip route add 127.0.0.0/8 dev lo table main priority 1 || true
        PostDown = ip route del SERVER_IP || true

        [Peer]
        PublicKey = SERVER_PUBKEY
        AllowedIPs = 0.0.0.0/1, 128.0.0.0/1, ::/0
        Endpoint = SERVER_IP:51820
        PersistentKeepalive = 25
      '';
      mode = "0644";
    };

    # Copy template and countries files to wgnord directory on activation
    system.activationScripts.wgnord-setup = /* bash */ ''
      if [ ! -f /var/lib/wgnord/template.conf ]; then
        cp /etc/wgnord-template.conf /var/lib/wgnord/template.conf
        chmod 644 /var/lib/wgnord/template.conf
      fi

      # Copy countries files if they don't exist
      if [ ! -f /var/lib/wgnord/countries.txt ]; then
        cp ${pkgs.wgnord}/share/countries.txt /var/lib/wgnord/countries.txt 2>/dev/null || true
      fi
      if [ ! -f /var/lib/wgnord/countries_iso31662.txt ]; then
        cp ${pkgs.wgnord}/share/countries_iso31662.txt /var/lib/wgnord/countries_iso31662.txt 2>/dev/null || true
      fi
    '';
  };
}
