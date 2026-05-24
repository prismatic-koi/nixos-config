{
  config,
  lib,
  pkgs,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.hyprland.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username} = {
      home.sessionPath = [ "$HOME/.local/scripts" ];
      # NOTE: `cli.system.setHyprGaps` previously lived here and ran on
      # session start to install workspace gap rules via `hyprctl keyword
      # workspace ...`. Under the lua parser that command fails with
      # "keyword can't work with non-legacy parsers. Use eval." — so the
      # rules now live declaratively in the hyprland module instead:
      #   - module:  modules/desktop/hyprland/default.nix (workspace_rule
      #              extension point)
      #   - per-host overrides (e.g. the ultrawide on navi):
      #              machines/navi/configuration.nix
      # See the matching comment block in default.nix near `workspace_rule`.
      home.file.".local/scripts/system.inputs.toggleTouchpad" = lib.mkIf config.nx.isLaptop {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            export STATUS_FILE="$XDG_RUNTIME_DIR/touchpad_status"

            # Mutating `device[...]:enabled` at runtime under the lua
            # parser. `hyprctl keyword` is rejected by the lua parser
            # ("keyword can't work with non-legacy parsers. Use eval.")
            # and `hyprctl eval` does not exist as a subcommand on
            # hyprland 0.55.1 — so we drive the device API directly via
            # `hyprctl dispatch '<lua>'`.
            #
            # `hyprctl dispatch` evaluates its argument as a lua
            # expression and feeds the result to `hl.dispatch(...)`,
            # whose typedef is `fun(dispatcher: HL.Dispatcher|function)`
            # — so passing a `function() ... end` literal lets us run
            # arbitrary lua side-effects (here: `hl.device({...})`)
            # without needing the result to be a dispatcher value. The
            # device API's `enabled` field is the lua-native replacement
            # for the hyprlang `device[NAME]:enabled` keyword and is
            # declared in the type stubs at
            # `share/hypr/stubs/hl.meta.lua` (`HL.DeviceSpec.enabled`).
            set_touchpad() {
              # $1 is the lua boolean literal (`true` or `false`).
              hyprctl dispatch \
                "function() hl.device({ name = \"asup1415:00-093a:300c-touchpad\", enabled = $1 }) end" \
                > /dev/null
            }

            if ! [ -f "$STATUS_FILE" ]; then
              # disable touchpad
              set_touchpad false
              touch "$STATUS_FILE"
              echo "disabled" > "$STATUS_FILE"
              hyprctl dispatch 'hl.dsp.event("quickshell:osd:touchpad:off")' > /dev/null
            elif [ "$(cat $STATUS_FILE)" = "enabled" ]; then
              # disable touchpad
              set_touchpad false
              echo "disabled" > "$STATUS_FILE"
              hyprctl dispatch 'hl.dsp.event("quickshell:osd:touchpad:off")' > /dev/null
            elif [ "$(cat $STATUS_FILE)" = "disabled" ]; then
              # enable touchpad
              set_touchpad true
              echo "enabled" > "$STATUS_FILE"
              hyprctl dispatch 'hl.dsp.event("quickshell:osd:touchpad:on")' > /dev/null
            fi
          '';
      };
      home.file.".local/scripts/system.audio.volumeUp" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Raise volume
            wpctl set-volume -l 1.5 @DEFAULT_AUDIO_SINK@ 5%+

            # Get current volume percentage
            volume=$(wpctl get-volume @DEFAULT_AUDIO_SINK@ | awk '{print int($2 * 100)}')

            # Fire OSD event
            hyprctl dispatch 'hl.dsp.event("quickshell:osd:volume:'"''${volume}"'")' > /dev/null
          '';
      };
      home.file.".local/scripts/system.audio.volumeDown" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Lower volume
            wpctl set-volume -l 1.5 @DEFAULT_AUDIO_SINK@ 5%-

            # Get current volume percentage
            volume=$(wpctl get-volume @DEFAULT_AUDIO_SINK@ | awk '{print int($2 * 100)}')

            # Fire OSD event
            hyprctl dispatch 'hl.dsp.event("quickshell:osd:volume:'"''${volume}"'")' > /dev/null
          '';
      };
      home.file.".local/scripts/system.audio.toggleMute" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Toggle mute
            wpctl set-mute @DEFAULT_AUDIO_SINK@ toggle

            # Check if muted
            mute_status=$(wpctl get-volume @DEFAULT_AUDIO_SINK@)

            # Fire OSD event
            if echo "$mute_status" | grep -q "MUTED"; then
              hyprctl dispatch 'hl.dsp.event("quickshell:osd:volume:muted")' > /dev/null
            else
              volume=$(echo "$mute_status" | awk '{print int($2 * 100)}')
              hyprctl dispatch 'hl.dsp.event("quickshell:osd:volume:'"''${volume}"'")' > /dev/null
            fi
          '';
      };
      home.file.".local/scripts/system.display.brightnessUp" = lib.mkIf config.nx.isLaptop {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Raise brightness
            ${pkgs.brightnessctl}/bin/brightnessctl s +10%

            # Get current brightness percentage
            brightness=$(${pkgs.brightnessctl}/bin/brightnessctl g)
            max_brightness=$(${pkgs.brightnessctl}/bin/brightnessctl m)
            brightness_percent=$(( brightness * 100 / max_brightness ))

            # Fire OSD event
            hyprctl dispatch 'hl.dsp.event("quickshell:osd:brightness:'"''${brightness_percent}"'")' > /dev/null
          '';
      };
      home.file.".local/scripts/system.display.brightnessDown" = lib.mkIf config.nx.isLaptop {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Lower brightness
            ${pkgs.brightnessctl}/bin/brightnessctl s 10%-

            # Get current brightness percentage
            brightness=$(${pkgs.brightnessctl}/bin/brightnessctl g)
            max_brightness=$(${pkgs.brightnessctl}/bin/brightnessctl m)
            brightness_percent=$(( brightness * 100 / max_brightness ))

            # Fire OSD event
            hyprctl dispatch 'hl.dsp.event("quickshell:osd:brightness:'"''${brightness_percent}"'")' > /dev/null
          '';
      };
      # Live caller: modules/desktop/hyprland/hyprlock.nix renders the battery
      # icon + percentage on the lockscreen via a `cmd[update:5000]` label in
      # the laptop-only block, polling this script every 5 seconds.
      #
      # Intentionally kept (not orphaned): the survey in closed issue #1961
      # flagged this script as potentially unused after battery-notifier was
      # rewritten as a Go daemon in #1955, but the hyprlock label above is a
      # live dependency. See issue #1980 for the reachability check and
      # decision to retain.
      home.file.".local/scripts/cli.system.batteryStatus" = lib.mkIf config.nx.isLaptop {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Get battery capacity
            capacity=$(cat /sys/class/power_supply/BAT0/capacity 2>/dev/null || echo "0")

            # Check if charging
            status=$(cat /sys/class/power_supply/BAT0/status 2>/dev/null || echo "Unknown")

            # Determine icon based on charging status and capacity
            if [ "$status" = "Charging" ] || [ "$status" = "Full" ]; then
              icon="󰂄"
            elif [ "$capacity" -le 10 ]; then
              icon="󰂎"
            elif [ "$capacity" -le 20 ]; then
              icon="󰁺"
            elif [ "$capacity" -le 30 ]; then
              icon="󰁻"
            elif [ "$capacity" -le 40 ]; then
              icon="󰁼"
            elif [ "$capacity" -le 50 ]; then
              icon="󰁽"
            elif [ "$capacity" -le 60 ]; then
              icon="󰁾"
            elif [ "$capacity" -le 70 ]; then
              icon="󰁿"
            elif [ "$capacity" -le 80 ]; then
              icon="󰂀"
            elif [ "$capacity" -le 90 ]; then
              icon="󰂁"
            else
              icon="󰂂"
            fi

            echo "''${icon} ''${capacity}%"
          '';
      };
      # NOTE: `cli.hyprland.switchWorkspaceOnWindowClose` previously lived
      # here — a long-running socat daemon that watched the hyprland
      # event socket and dispatched a workspace switch when the last
      # window on a non-1 workspace closed. Under the lua parser the
      # whole thing collapses into a native `hl.on("window.close", ...)`
      # handler in the hyprland config; see the matching entry in
      # `modules/desktop/hyprland/default.nix` under the `on` list.
      # (issue #1961)
      home.file.".local/scripts/cli.system.suspend" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Prevent multiple instances of this script
            lockfile="/tmp/suspend_script.lock"
            if [ -f "$lockfile" ]; then
              exit 0
            fi
            touch "$lockfile"

            # Cleanup function
            cleanup() {
              rm -f "$lockfile"
            }
            trap cleanup EXIT

            # Start hyprlock if not already running
            if ! pgrep -x hyprlock > /dev/null; then
              hyprctl dispatch 'hl.dsp.exec_cmd("hyprlock")'

              # Wait for hyprlock to actually start (check for process)
              for i in {1..20}; do
                if pgrep -x hyprlock > /dev/null; then
                  break
                fi
                sleep 0.1
              done

              # Give hyprlock a moment to fully initialize
              sleep 0.5
            else
              # If hyprlock is already running, just wait a bit to ensure stability
              sleep 0.2
            fi

            # Add a small delay before suspending to let everything settle
            sleep 0.3
            systemctl suspend
          '';
      };
      home.file.".local/scripts/cli.system.inhibitIdle" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Use XDG_RUNTIME_DIR for better security and automatic cleanup
            LOCKFILE="$XDG_RUNTIME_DIR/systemd_inhibit.lock"

            start_inhibit() {
                # Clean up any stale processes first
                if [[ -f "$LOCKFILE" ]]; then
                    old_pid=$(cat "$LOCKFILE" 2>/dev/null)
                    if [ -n "$old_pid" ] && ! kill -0 "$old_pid" 2>/dev/null; then
                        echo "Cleaning up stale lockfile"
                        rm -f "$LOCKFILE"
                    fi
                fi

                if [[ -f "$LOCKFILE" ]]; then
                    echo "Inhibit already running."
                    return
                fi

                systemd-inhibit --what=idle --why="Preventing idle for a task" sleep infinity &
                echo $! > "$LOCKFILE"
                hyprctl dispatch 'hl.dsp.event("quickshell:inhibit-on")' >/dev/null 2>&1 || true
            }

            stop_inhibit() {
                if [[ -f "$LOCKFILE" ]]; then
                    pid=$(cat "$LOCKFILE" 2>/dev/null)
                    if [ -n "$pid" ]; then
                        # Kill the process gracefully
                        if kill "$pid" 2>/dev/null; then
                            echo "Stopped inhibit process $pid"
                        else
                            echo "Warning: Could not stop process $pid (may have already exited)"
                        fi
                    fi
                    rm -f "$LOCKFILE"
                fi
                hyprctl dispatch 'hl.dsp.event("quickshell:inhibit-off")' >/dev/null 2>&1 || true
            }

            status_inhibit() {
                if [[ -f "$LOCKFILE" ]]; then
                    pid=$(cat "$LOCKFILE" 2>/dev/null)
                    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                        echo "Inhibit Active (PID: $pid)"
                    else
                        echo "Inhibit Inactive (stale lockfile)"
                        rm -f "$LOCKFILE"
                    fi
                else
                    echo "Inhibit Inactive"
                fi
            }

            case $1 in
                start)
                    start_inhibit
                    if [[ -f "$LOCKFILE" ]]; then
                        echo "Inhibit started."
                    else
                        echo "Failed to start inhibit."
                        exit 1
                    fi
                    ;;
                stop)
                    stop_inhibit
                    echo "Inhibit stopped."
                    ;;
                status)
                    status_inhibit
                    ;;
                statusjson)
                    if [[ -f "$LOCKFILE" ]]; then
                        pid=$(cat "$LOCKFILE" 2>/dev/null)
                        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                            echo '{"text": "IDLE INHIBIT", "class": "active"}'
                        else
                            rm -f "$LOCKFILE"
                            echo '{"text": "", "class": "inactive"}'
                        fi
                    else
                        echo '{"text": "", "class": "inactive"}'
                    fi
                    ;;
                toggle)
                    if [[ -f "$LOCKFILE" ]]; then
                        pid=$(cat "$LOCKFILE" 2>/dev/null)
                        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                            stop_inhibit
                            echo "Inhibit toggled off."
                        else
                            rm -f "$LOCKFILE"
                            start_inhibit
                            echo "Inhibit toggled on."
                        fi
                    else
                        start_inhibit
                        echo "Inhibit toggled on."
                    fi
                    ;;
                *)
                    echo "Usage: $0 {start|stop|status|toggle}"
                    exit 1
                    ;;
            esac
          '';
      };
    };
  };
}
