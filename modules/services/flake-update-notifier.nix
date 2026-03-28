{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.nx.services.flakeUpdateNotifier;

  check-script = pkgs.writeShellScript "flake-update-notifier" ''
    REPO="$HOME/code/nixos-config"

    local_hash=$(${pkgs.git}/bin/git -C "$REPO" log -1 --format="%H" -- flake.lock 2>/dev/null)
    remote_hash=$(${pkgs.curl}/bin/curl -sf \
      "https://api.github.com/repos/lucidph3nx/nixos-config/commits?path=flake.lock&per_page=1" \
      | ${pkgs.jq}/bin/jq -r '.[0].sha')

    [ -z "$local_hash" ] || [ -z "$remote_hash" ] && exit 0
    [ "$local_hash" = "$remote_hash" ] && exit 0

    TITLE="Nix Config Update Available"
    MSG="flake.lock is out of date on $(${pkgs.hostname}/bin/hostname). Run: nh os switch"

    if command -v ${pkgs.libnotify}/bin/notify-send &>/dev/null; then
      ${pkgs.libnotify}/bin/notify-send -u normal -t 0 "$TITLE" "$MSG"
    elif command -v osascript &>/dev/null; then
      osascript -e "display notification \"$MSG\" with title \"$TITLE\""
    fi
  '';

  # System-level wrapper that dispatches the user service on wake from sleep.
  # Uses machinectl to bridge the system suspend event into the user session,
  # matching the pattern established in battery-notifier.nix.
  wake-dispatcher = pkgs.writeShellScript "flake-update-notifier-wake" ''
    ${pkgs.coreutils}/bin/sleep 10
    ${pkgs.systemd}/bin/machinectl shell \
      ${config.nx.username}@.host \
      /bin/sh -c "systemctl --user start flake-update-notifier.service"
  '';
in
{
  options.nx.services.flakeUpdateNotifier = {
    enable = lib.mkEnableOption "flake.lock staleness notifier";
    notifyOnWake = lib.mkEnableOption "also trigger check on resume from sleep";
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      # ── Linux ──────────────────────────────────────────────────────────────
      (lib.mkIf pkgs.stdenv.isLinux {
        # User-level service: fires at graphical session start (login/boot).
        home-manager.users.${config.nx.username}.systemd.user.services.flake-update-notifier = {
          Unit = {
            Description = "Check if flake.lock is up to date";
            After = [ "graphical-session.target" ];
          };
          Service = {
            Type = "oneshot";
            # Short delay so the notification daemon is ready before we fire.
            ExecStartPre = "${pkgs.coreutils}/bin/sleep 5";
            ExecStart = "${check-script}";
          };
          Install = {
            WantedBy = [ "graphical-session.target" ];
          };
        };

        # System-level service: dispatches the user service after resume from sleep.
        systemd.services.flake-update-notifier-wake = lib.mkIf cfg.notifyOnWake {
          description = "Trigger flake update notifier for user on resume from sleep";
          after = [ "suspend.target" ];
          wantedBy = [ "suspend.target" ];
          serviceConfig = {
            Type = "oneshot";
            ExecStart = "${wake-dispatcher}";
          };
        };
      })

      # ── Darwin ─────────────────────────────────────────────────────────────
      (lib.mkIf pkgs.stdenv.isDarwin {
        home-manager.users.${config.nx.username}.launchd.agents.flake-update-notifier = {
          enable = true;
          config = {
            ProgramArguments = [ "${check-script}" ];
            RunAtLoad = true;
            StandardOutPath = "/tmp/flake-update-notifier.log";
            StandardErrorPath = "/tmp/flake-update-notifier.log";
          };
        };
      })
    ]
  );
}
