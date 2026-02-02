{
  config,
  lib,
  pkgs,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.hyprland.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben = {
      home.sessionPath = [ "$HOME/.local/scripts" ];
      home.file.".local/scripts/cli.system.setHyprGaps" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Get monitor width with error handling
            width=$(hyprctl monitors -j 2>/dev/null | jq -r '.[0].width // empty' 2>/dev/null)

            # Check if we got a valid width
            if [ -z "$width" ] || [ "$width" = "null" ]; then
              echo "Error: Could not get monitor width" >&2
              exit 1
            fi

            # Check if running in super-ultrawide
            if [ "$width" -gt 5000 ]; then
              hyprctl keyword workspace "w[t1], gapsout:5 $((width / 4))" || echo "Warning: Failed to set workspace gaps" >&2
              hyprctl keyword workspace "s[true], gapsout:50 $((width / 4))" || echo "Warning: Failed to set workspace gaps" >&2
            else
              # unsetting is not possible, for now set to default
              # https://github.com/hyprwm/Hyprland/issues/5691
              hyprctl keyword workspace "w[t1], gapsout:5 5" || echo "Warning: Failed to set workspace gaps" >&2
              hyprctl keyword workspace "s[true], gapsout:50 50" || echo "Warning: Failed to set workspace gaps" >&2
            fi
          '';
      };
      home.file.".local/scripts/system.inputs.toggleTouchpad" = lib.mkIf config.nx.isLaptop {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            export STATUS_FILE="$XDG_RUNTIME_DIR/touchpad_status"
            
            # Notification ID file
            ID_FILE="$XDG_RUNTIME_DIR/touchpad_notification_id"
            
            if ! [ -f "$STATUS_FILE" ]; then
              # disable touchpad
              hyprctl keyword 'device[asup1415:00-093a:300c-touchpad]:enabled' false > /dev/null
              touch "$STATUS_FILE"
              echo "disabled" > "$STATUS_FILE"
              
              # Send notification
              if [ -f "$ID_FILE" ]; then
                notify_id=$(cat "$ID_FILE")
                ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i input-touchpad-off "Touchpad" "Disabled" > "$ID_FILE"
              else
                ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i input-touchpad-off "Touchpad" "Disabled" > "$ID_FILE"
              fi
            elif [ "$(cat $STATUS_FILE)" = "enabled" ]; then
              # disable touchpad
              hyprctl keyword 'device[asup1415:00-093a:300c-touchpad]:enabled' false > /dev/null
              echo "disabled" > "$STATUS_FILE"
              
              # Send notification
              if [ -f "$ID_FILE" ]; then
                notify_id=$(cat "$ID_FILE")
                ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i input-touchpad-off "Touchpad" "Disabled" > "$ID_FILE"
              else
                ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i input-touchpad-off "Touchpad" "Disabled" > "$ID_FILE"
              fi
            elif [ "$(cat $STATUS_FILE)" = "disabled" ]; then
              # enable touchpad
              hyprctl keyword 'device[asup1415:00-093a:300c-touchpad]:enabled' true > /dev/null
              echo "enabled" > "$STATUS_FILE"
              
              # Send notification
              if [ -f "$ID_FILE" ]; then
                notify_id=$(cat "$ID_FILE")
                ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i input-touchpad-on "Touchpad" "Enabled" > "$ID_FILE"
              else
                ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i input-touchpad-on "Touchpad" "Enabled" > "$ID_FILE"
              fi
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

            # Determine icon based on volume level
            if [ "$volume" -eq 0 ]; then
              icon="audio-volume-muted"
            elif [ "$volume" -le 33 ]; then
              icon="audio-volume-low"
            elif [ "$volume" -le 66 ]; then
              icon="audio-volume-medium"
            else
              icon="audio-volume-high"
            fi

            # Notification ID file
            ID_FILE="$XDG_RUNTIME_DIR/volume_notification_id"

            # Send notification and store/reuse ID
            if [ -f "$ID_FILE" ]; then
              notify_id=$(cat "$ID_FILE")
              ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i "$icon" "Volume" "Raised to ''${volume}%" > "$ID_FILE"
            else
              ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i "$icon" "Volume" "Raised to ''${volume}%" > "$ID_FILE"
            fi
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

            # Determine icon based on volume level
            if [ "$volume" -eq 0 ]; then
              icon="audio-volume-muted"
            elif [ "$volume" -le 33 ]; then
              icon="audio-volume-low"
            elif [ "$volume" -le 66 ]; then
              icon="audio-volume-medium"
            else
              icon="audio-volume-high"
            fi

            # Notification ID file
            ID_FILE="$XDG_RUNTIME_DIR/volume_notification_id"

            # Send notification and store/reuse ID
            if [ -f "$ID_FILE" ]; then
              notify_id=$(cat "$ID_FILE")
              ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i "$icon" "Volume" "Lowered to ''${volume}%" > "$ID_FILE"
            else
              ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i "$icon" "Volume" "Lowered to ''${volume}%" > "$ID_FILE"
            fi
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

            # Notification ID file (same as volume scripts)
            ID_FILE="$XDG_RUNTIME_DIR/volume_notification_id"

            # Send notification and store/reuse ID
            if echo "$mute_status" | grep -q "MUTED"; then
              if [ -f "$ID_FILE" ]; then
                notify_id=$(cat "$ID_FILE")
                ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i audio-volume-muted "Volume" "Muted" > "$ID_FILE"
              else
                ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i audio-volume-muted "Volume" "Muted" > "$ID_FILE"
              fi
            else
              volume=$(echo "$mute_status" | awk '{print int($2 * 100)}')

              # Determine icon based on volume level
              if [ "$volume" -eq 0 ]; then
                icon="audio-volume-muted"
              elif [ "$volume" -le 33 ]; then
                icon="audio-volume-low"
              elif [ "$volume" -le 66 ]; then
                icon="audio-volume-medium"
              else
                icon="audio-volume-high"
              fi

              if [ -f "$ID_FILE" ]; then
                notify_id=$(cat "$ID_FILE")
                ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i "$icon" "Volume" "Unmuted at ''${volume}%" > "$ID_FILE"
              else
                ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i "$icon" "Volume" "Unmuted at ''${volume}%" > "$ID_FILE"
              fi
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

            # Determine icon based on brightness level
            if [ "$brightness_percent" -le 50 ]; then
              icon="brightness-low"
            else
              icon="brightness-high"
            fi

            # Notification ID file
            ID_FILE="$XDG_RUNTIME_DIR/brightness_notification_id"

            # Send notification and store/reuse ID
            if [ -f "$ID_FILE" ]; then
              notify_id=$(cat "$ID_FILE")
              ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i "$icon" "Brightness" "Raised to ''${brightness_percent}%" > "$ID_FILE"
            else
              ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i "$icon" "Brightness" "Raised to ''${brightness_percent}%" > "$ID_FILE"
            fi
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

            # Determine icon based on brightness level
            if [ "$brightness_percent" -le 50 ]; then
              icon="brightness-low"
            else
              icon="brightness-high"
            fi

            # Notification ID file
            ID_FILE="$XDG_RUNTIME_DIR/brightness_notification_id"

            # Send notification and store/reuse ID
            if [ -f "$ID_FILE" ]; then
              notify_id=$(cat "$ID_FILE")
              ${pkgs.libnotify}/bin/notify-send -r "$notify_id" -p -t 2000 -u low -i "$icon" "Brightness" "Lowered to ''${brightness_percent}%" > "$ID_FILE"
            else
              ${pkgs.libnotify}/bin/notify-send -p -t 2000 -u low -i "$icon" "Brightness" "Lowered to ''${brightness_percent}%" > "$ID_FILE"
            fi
          '';
      };
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
      home.file.".local/scripts/cli.hyprland.switchWorkspaceOnWindowClose" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh

            # Cleanup function
            cleanup() {
              echo "Script interrupted, cleaning up..."
              exit 0
            }

            # Trap signals for clean exit
            trap cleanup INT TERM

            function handle {
              if [[ "$1" == closewindow* ]]; then
                echo "Close Window detected"

                # Get workspace info with error handling
                active_workspace=$(hyprctl activeworkspace -j 2>/dev/null | jq -r '.id // empty' 2>/dev/null)
                if [ -z "$active_workspace" ] || [ "$active_workspace" = "null" ]; then
                  echo "Warning: Could not get active workspace" >&2
                  return
                fi

                if [[ "$active_workspace" -ne 1 ]]; then
                  windows_count=$(hyprctl activeworkspace -j 2>/dev/null | jq -r '.windows // empty' 2>/dev/null)
                  if [ -z "$windows_count" ] || [ "$windows_count" = "null" ]; then
                    echo "Warning: Could not get window count" >&2
                    return
                  fi

                  if [[ "$windows_count" -eq 0 ]]; then
                    echo "$windows_count"
                    echo "Empty workspace detected"
                    hyprctl dispatch workspace m-1 || echo "Warning: Failed to switch workspace" >&2
                  fi
                fi
              fi
            }

            # Main loop with error recovery
            while true; do
              if ! ${pkgs.socat}/bin/socat - "UNIX-CONNECT:$XDG_RUNTIME_DIR/hypr/$HYPRLAND_INSTANCE_SIGNATURE/.socket2.sock" 2>/dev/null | while read -r line; do handle "$line"; done; then
                echo "Socket connection lost, retrying in 5 seconds..." >&2
                sleep 5
              else
                break
              fi
            done
          '';
      };
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
              hyprctl dispatch exec hyprlock

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

            pkill -SIGUSR2 waybar

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
