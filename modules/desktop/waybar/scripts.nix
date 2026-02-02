{
  config,
  pkgs,
  lib,
  ...
}:
{
  config = lib.mkIf (config.nx.desktop.waybar.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben.home = {
      file.".local/scripts/cli.mpd.nowPlaying" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh

            fields=$(mpc -f 'name=%name%\nartist=%artist%\nalbum=%album%\ntitle=%title%\ntime=%time%' | sed 's/^\n//' | head -n 5)
            name=$(echo "$fields" | grep -oP 'name=\K.*')
            artist=$(echo "$fields" | grep -oP 'artist=\K.*')
            album=$(echo "$fields" | grep -oP 'album=\K.*')
            title=$(echo "$fields" | grep -oP 'title=\K.*')
            time=$(echo "$fields" | grep -oP 'time=\K.*')
            status=$(mpc status | sed -n '2p')  # Second line contains the playback status and timing info
            player=$(mpc status | sed -n '3p') # Third line contains volume, repeat, random, etc.

            # Use name if artist is not available
            if [ -n "$artist" ]; then
                display_artist="$artist"
            else
                display_artist="$name"
            fi

            # Parse the playback status
            playing=$(echo "$status" | grep -oP '^\[.*\]')
            elapsed_time=$(echo "$status" | grep -oP '\d+:\d+' | head -n 1)
            total_time=$(echo "$status" | grep -oP '\d+:\d+' | tail -n 1)

            # Determine the now playing icon
            if echo "$playing" | grep -q "\[playing\]"; then
                play_icon=""
            elif echo "$playing" | grep -q "\[paused\]"; then
                play_icon=""
            else
                play_icon="" # Default icon for stopped
            fi

            if [ "$total_time" != "0:00" ] && [ -n "$total_time" ]; then
                time_info=" ($elapsed_time/$total_time)"
            else
                time_info=""
            fi

            # Format the output
            if [ -n "$display_artist" ] && [ -n "$title" ]; then
                now_playing="$play_icon $display_artist - $title$time_info 󰝚"
            else
                now_playing=""
            fi

            # replace ampersands with html entities
            now_playing=$(echo "$now_playing" | sed 's/&/\&amp;/g')
            # Output the friendly now playing message
            echo "$now_playing"
          '';
      };
      file.".local/scripts/cli.audio.outputEnabled" = lib.mkIf config.nx.externalAudio.enable {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # Find the nick of the current default sink
            current_sink_=$(wpctl inspect @DEFAULT_SINK@ | grep -m 1 'node.nick' | awk -F ' = ' '{gsub(/"/, "", $2); print $2}')

            if [ "$current_sink_" = "FiiO K7" ]; then
                echo "󰕾"
            else
                echo "󰖁"
            fi
          '';
      };
      file.".local/scripts/cli.audio.getOutput" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            # a handy little lookup table to provide icons
            get_alt() {
                local text="$1"
                declare -A alt_lookup
                alt_lookup=(
                    ["Razer BlackShark V2 Pro"]="󰋋"
                    ["Razer Nari"]="󰋋"
                    ["ALC1220 Analog"]="󰓃"
                    ["FiiO K7"]="󰤽"
                    # Add more entries here as needed
                )
                echo ''${alt_lookup[$text]}
            }

            # Find the ID of the current default sink
            current_sink_id=$(wpctl inspect @DEFAULT_SINK@ | awk 'NR==1 {gsub(/,/, "", $2); print $2}')

            # Get the node.nick of the current default sink
            current_sink_nick=$(pw-dump | jq -r '.[] | select(.id == '$current_sink_id') | .info.props."node.nick"')
            alt=$(get_alt "$current_sink_nick")

            # Format the output as JSON
            output=$(printf '{"text": "%s", "alt": "%s", "tooltip": "%s"}' "$current_sink_nick" "$alt" "$current_sink_nick")
            echo "$output"
          '';
      };
    };
  };
}
