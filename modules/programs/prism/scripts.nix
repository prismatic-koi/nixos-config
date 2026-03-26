{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.programs.prism.scripts.enable = lib.mkEnableOption "enables prism helper scripts" // {
      default = true;
    };
  };

  config = lib.mkIf (config.nx.programs.prism.scripts.enable && config.nx.programs.prism.enable) {
    home-manager.users.${config.nx.username} = {
      home.sessionPath = [ "$HOME/.local/scripts" ];

      home.file.".local/scripts/cli.prism.launch" = {
        executable = true;
        text =
          let
            tmux = "${pkgs.tmux}/bin/tmux";
            kitty = "${pkgs.kitty}/bin/kitty";
          in
          # bash
          ''
            #!/usr/bin/env bash
            # Launch Prism with scratchpad and context switcher
            # --in-terminal: attach in the current terminal instead of spawning a new kitty window
            # --path <dir>: skip the interactive picker and open a specific directory

            IN_TERMINAL=0
            PATH_ARG=""

            while [[ $# -gt 0 ]]; do
                case "$1" in
                    --in-terminal) IN_TERMINAL=1; shift ;;
                    --path) PATH_ARG="$2"; shift 2 ;;
                    *) shift ;;
                esac
            done

            if [ -n "$PATH_ARG" ]; then
                SWITCHER_CMD="prism switch --path \"$PATH_ARG\""
            else
                SWITCHER_CMD="prism switch"
            fi

            # Check if we're already in tmux
            if [ -n "$TMUX" ]; then
                # Inside tmux - check if scratchpad session exists
                if ${tmux} has-session -t scratchpad 2>/dev/null; then
                    # Switch to scratchpad session
                    ${tmux} switch-client -t scratchpad
                else
                    # Create scratchpad session
                    ${tmux} new-session -ds scratchpad -c "$HOME"
                    ${tmux} rename-window -t scratchpad:0 term
                    ${tmux} switch-client -t scratchpad
                fi

                # Small delay to let terminal settle, then open context switcher
                sleep 0.1
                ${tmux} display-popup -w 80% -h 80% -E "$SWITCHER_CMD"
            elif [ "$IN_TERMINAL" = "1" ]; then
                # In a terminal but not tmux - attach in-place
                if ! ${tmux} has-session -t scratchpad 2>/dev/null; then
                    ${tmux} new-session -ds scratchpad -c "$HOME"
                    ${tmux} rename-window -t scratchpad:0 term
                fi
                # Fire context switcher once after attach, then remove the hook
                ${tmux} set-hook -t scratchpad client-attached \
                    "run-shell 'sleep 0.1' ; display-popup -w 80% -h 80% -E '$SWITCHER_CMD' ; set-hook -u client-attached"
                exec ${tmux} new-session -As scratchpad
            else
                # Outside tmux - launch in new kitty window with delay before popup
                ${kitty} --title "Prism" ${tmux} new-session -As scratchpad -c "$HOME" \; \
                    rename-window -t scratchpad:0 term \; \
                    run-shell "sleep 0.2" \; \
                    display-popup -w 80% -h 80% -E "$SWITCHER_CMD" &
            fi
          '';
      };

      home.file.".local/scripts/cli.tmux.setStatus" = {
        executable = true;
        text =
          with config.theme;
          # bash
          ''
            #!/usr/bin/env bash

            # Exit silently if not in tmux
            if [ -z "$TMUX" ]; then
              exit 0
            fi

            # Read JSON from stdin (required by hooks)
            HOOK_JSON=$(cat)

            ACTION="''${1:-}"

            # Get the window ID for the pane where this hook is running
            WINDOW_ID=$(${pkgs.tmux}/bin/tmux display-message -t "$TMUX_PANE" -p '#{window_id}')

            # State colours (from theme)
            # active   = purple (agent is working)
            # waiting  = yellow (waiting for user input)
            # finished = green  (idle, ready for next prompt)
            # compact  = blue   (compacting context)
            # error    = red    (error / retrying)

            ACTIVE_FMT='#[fg=${purple}]#I:#W#{?window_flags,#{window_flags}, }'
            WAITING_FMT='#[fg=${yellow}]#I:#W#{?window_flags,#{window_flags}, }'
            FINISHED_FMT='#[fg=${green}]#I:#W#{?window_flags,#{window_flags}, }'
            COMPACT_FMT='#[fg=${blue}]#I:#W#{?window_flags,#{window_flags}, }'
            ERROR_FMT='#[fg=${red}]#I:#W#{?window_flags,#{window_flags}, }'

            set_status() {
              local state="$1" fmt="$2"
              ${pkgs.tmux}/bin/tmux set-window-option -t "$WINDOW_ID" window-status-format "$fmt"
              ${pkgs.tmux}/bin/tmux set-window-option -t "$WINDOW_ID" window-status-current-format "$fmt"
              # store state for choose-tree -F to read
              ${pkgs.tmux}/bin/tmux set-window-option -t "$WINDOW_ID" @agent_state "$state"
            }

            case "$ACTION" in
              set-active)    set_status "active"     "$ACTIVE_FMT" ;;
              set-waiting)   set_status "waiting"    "$WAITING_FMT" ;;
              set-finished)  set_status "finished"   "$FINISHED_FMT" ;;
              set-compacting) set_status "compacting" "$COMPACT_FMT" ;;
              set-error)     set_status "error"      "$ERROR_FMT" ;;
              clear)
                # Unset per-window overrides to fall back to global config
                ${pkgs.tmux}/bin/tmux set-window-option -t "$WINDOW_ID" -u window-status-format
                ${pkgs.tmux}/bin/tmux set-window-option -t "$WINDOW_ID" -u window-status-current-format
                ${pkgs.tmux}/bin/tmux set-window-option -t "$WINDOW_ID" -u @agent_state
                ;;
            esac

            exit 0
          '';
      };

      programs.zsh.shellAliases = {
        gwc = "prism clone";
      };
    };
  };
}
