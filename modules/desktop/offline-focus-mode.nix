{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.desktop.offline-focus-mode.enable = lib.mkEnableOption "Enable offline focus mode" // {
      default = false;
    };
  };
  config = lib.mkIf (config.nx.desktop.offline-focus-mode.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben = {
      home.sessionPath = [ "$HOME/.local/scripts" ];
      # I'm going to call this thing 'system mode'
      # I will consist of a normal running mode and an 'offline focus mode'
      home.file.".local/scripts/cli.desktop.getSystemMode" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            MODE_FILE="$XDG_RUNTIME_DIR/system_mode"
            MODE_TEXT=""
            MODE_JSON=""

            if ! [ -f "$MODE_FILE" ]; then
              # if no file, consider self in normal mode
              touch "$MODE_FILE"
              echo "normal" > "$MODE_FILE"
              MODE_TEXT="Normal Mode"
              MODE_JSON='{ "text": "Normal Mode", "class": "normal" }'
              # just in case networking is off
              nmcli networking on

            elif [ "$(cat $MODE_FILE)" = "normal" ]; then
              echo "normal" > "$MODE_FILE"
              MODE_TEXT="Normal Mode"
              MODE_JSON='{ "text": "Normal Mode", "class": "normal" }'

            elif [ "$(cat $MODE_FILE)" = "offline-focus" ]; then
              echo "offline-focus" > "$MODE_FILE"
              MODE_TEXT="Offline Focus Mode"
              MODE_JSON='{ "text": "Offline Focus Mode", "class": "offline-focus" }'
            fi

            case $1 in
              json)
                echo "$MODE_JSON"
                ;;
              *)
                echo "$MODE_TEXT"
                ;;
             esac
          '';
      };
      home.file.".local/scripts/desktop.system.setOfflineFocusMode" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            export MODE_FILE="$XDG_RUNTIME_DIR/system_mode"
            nmcli networking off
            echo "offline-focus" > "$MODE_FILE"
          '';
      };
      home.file.".local/scripts/desktop.system.setNormalMode" = {
        executable = true;
        text =
          # bash
          ''
            #!/bin/sh
            export MODE_FILE="$XDG_RUNTIME_DIR/system_mode"
            nmcli networking on
            echo "normal" > "$MODE_FILE"
          '';
      };
    };
  };
}
