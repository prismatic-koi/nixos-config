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

      home.file.".local/scripts/cli.git.worktreeClone" = {
        executable = true;
        text =
          # python
          ''
            #!/usr/bin/env python3
            import sys
            import subprocess
            import os
            import re
            import shutil

            def extract_repo_name(url):
                """Extract repository name from git URL (HTTPS or SSH format)"""
                # Match HTTPS: https://github.com/user/repo.git or https://github.com/user/repo
                https_match = re.search(r'https?://[^/]+/[^/]+/([^/]+?)(?:\.git)?$', url)
                if https_match:
                    return https_match.group(1)
                
                # Match SSH: git@github.com:user/repo.git or git@github.com:user/repo
                ssh_match = re.search(r'git@[^:]+:(?:[^/]+/)?([^/]+?)(?:\.git)?$', url)
                if ssh_match:
                    return ssh_match.group(1)
                
                return None

            def check_git_installed():
                """Check if git is available"""
                try:
                    subprocess.run(
                        ['git', '--version'],
                        capture_output=True,
                        check=True
                    )
                except (subprocess.CalledProcessError, FileNotFoundError):
                    print("Error: git is not installed or not in PATH", file=sys.stderr)
                    sys.exit(1)

            def main():
                if len(sys.argv) < 2:
                    print("Usage: cli.git.worktreeClone <repo-url> [directory-name]", file=sys.stderr)
                    sys.exit(1)

                repo_url = sys.argv[1]
                
                # Extract repo name for default directory
                repo_name = extract_repo_name(repo_url)
                if not repo_name:
                    print(f"Error: Could not parse repository name from URL: {repo_url}", file=sys.stderr)
                    sys.exit(1)
                
                # Use custom directory name if provided, otherwise use repo name
                target_dir = sys.argv[2] if len(sys.argv) > 2 else repo_name
                
                # Check if directory already exists
                if os.path.exists(target_dir):
                    print(f"Error: Directory '{target_dir}' already exists", file=sys.stderr)
                    sys.exit(1)
                
                # Check git is installed
                check_git_installed()
                
                bare_dir = os.path.join(target_dir, '.bare')
                git_file = os.path.join(target_dir, '.git')
                
                try:
                    # Create target directory
                    os.makedirs(target_dir, exist_ok=False)
                    
                    # Clone bare repository
                    print(f"Cloning {repo_url}...")
                    result = subprocess.run(
                        ['git', 'clone', '--bare', repo_url, bare_dir],
                        capture_output=True,
                        text=True
                    )
                    if result.returncode != 0:
                        raise Exception(f"Git clone failed: {result.stderr}")
                    
                    # Configure fetch refspec for remote tracking
                    result = subprocess.run(
                        ['git', '--git-dir', bare_dir, 'config', 'remote.origin.fetch', '+refs/heads/*:refs/remotes/origin/*'],
                        capture_output=True,
                        text=True
                    )
                    if result.returncode != 0:
                        raise Exception(f"Failed to configure fetch refspec: {result.stderr}")
                    
                    # Fetch to populate remote-tracking branches
                    result = subprocess.run(
                        ['git', '--git-dir', bare_dir, 'fetch', 'origin'],
                        capture_output=True,
                        text=True
                    )
                    if result.returncode != 0:
                        raise Exception(f"Failed to fetch from origin: {result.stderr}")
                    
                    # Create .git file pointing to .bare
                    with open(git_file, 'w') as f:
                        f.write('gitdir: ./.bare\n')
                    
                    # Detect default branch by reading HEAD
                    head_file = os.path.join(bare_dir, "HEAD")
                    with open(head_file, "r") as f:
                        head_content = f.read().strip()
                    
                    # HEAD content looks like: ref: refs/heads/main
                    if not head_content.startswith("ref: refs/heads/"):
                        raise Exception(f"Unexpected HEAD format: {head_content}")
                    
                    default_branch = head_content.replace("ref: refs/heads/", "")
                    
                    # Set up tracking for the default branch
                    print(f"Setting up tracking for branch '{default_branch}'...")
                    result = subprocess.run(
                        ['git', '--git-dir', bare_dir, 'branch', '--set-upstream-to', f'origin/{default_branch}', default_branch],
                        capture_output=True,
                        text=True
                    )
                    if result.returncode != 0:
                        raise Exception(f"Failed to set up tracking: {result.stderr}")
                    
                    # Create worktree for default branch
                    print(f"Creating worktree for branch '{default_branch}'...")
                    worktree_dir = os.path.join(target_dir, default_branch)
                    result = subprocess.run(
                        ['git', '--git-dir', bare_dir, 'worktree', 'add', worktree_dir, default_branch],
                        capture_output=True,
                        text=True
                    )
                    if result.returncode != 0:
                        raise Exception(f"Failed to create worktree: {result.stderr}")
                    
                    print(f"✓ Successfully created worktree clone in '{target_dir}'")
                    print(f"✓ Default branch '{default_branch}' checked out in '{worktree_dir}'")
                    
                except Exception as e:
                    # Clean up on failure
                    if os.path.exists(target_dir):
                        shutil.rmtree(target_dir)
                    print(f"Error: {e}", file=sys.stderr)
                    sys.exit(1)

            if __name__ == '__main__':
                main()
          '';
      };

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
                SWITCHER_CMD="cli.tmux.contextSwitcher --path \"$PATH_ARG\""
            else
                SWITCHER_CMD="cli.tmux.contextSwitcher"
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

      home.file.".local/scripts/cli.tmux.dashboard" = {
        executable = true;
        text =
          with config.theme;
          let
            python = "${pkgs.python3}/bin/python3";
            tmux = "${pkgs.tmux}/bin/tmux";
            git = "${pkgs.git}/bin/git";
          in
          # python
          ''
            #!${python}
            """
            cli.tmux.dashboard — read-only Prism agent status board.
            Displays all tmux sessions with their agent state and changed files.
            Invoked inside a tmux display-popup; press q/Escape/Enter to dismiss.
            """
            import subprocess
            import sys
            import os

            # ── ANSI colours (from theme) ──────────────────────────────────────────────
            RESET   = "\033[0m"
            BOLD    = "\033[1m"
            DIM     = "\033[2m"

            def hex_to_ansi_fg(hex_color):
                """Convert a #rrggbb hex colour to an ANSI 24-bit foreground escape."""
                h = hex_color.lstrip("#")
                r, g, b = int(h[0:2], 16), int(h[2:4], 16), int(h[4:6], 16)
                return f"\033[38;2;{r};{g};{b}m"

            C_ACTIVE     = hex_to_ansi_fg("${purple}")
            C_WAITING    = hex_to_ansi_fg("${yellow}")
            C_FINISHED   = hex_to_ansi_fg("${green}")
            C_COMPACTING = hex_to_ansi_fg("${blue}")
            C_ERROR      = hex_to_ansi_fg("${red}")
            C_DIM        = hex_to_ansi_fg("${secondary}")
            C_HEADER     = hex_to_ansi_fg("${primary}")

            STATE_COLOUR = {
                "active":     C_ACTIVE,
                "waiting":    C_WAITING,
                "finished":   C_FINISHED,
                "compacting": C_COMPACTING,
                "error":      C_ERROR,
            }

            STATE_LABEL = {
                "active":     "active    ",
                "waiting":    "waiting   ",
                "finished":   "finished  ",
                "compacting": "compacting",
                "error":      "error     ",
                "":           "idle      ",
            }

            # ── tmux queries ───────────────────────────────────────────────────────────

            def tmux(*args):
                result = subprocess.run(
                    ["${tmux}"] + list(args),
                    capture_output=True, text=True
                )
                return result.stdout.strip()

            def get_sessions():
                """Return list of session names."""
                out = tmux("list-sessions", "-F", "#{session_name}")
                return [s for s in out.split("\n") if s]

            def get_agent_state(session):
                """Return the @agent_state value for the 'agent' window of a session."""
                out = tmux(
                    "list-windows", "-t", session,
                    "-F", "#{window_name}|#{@agent_state}|#{pane_current_path}"
                )
                for line in out.split("\n"):
                    parts = line.split("|", 2)
                    if len(parts) == 3 and parts[0] == "agent":
                        return parts[1], parts[2]   # (state, path)
                return "", ""

            # ── git helpers ────────────────────────────────────────────────────────────

            def changed_files(path):
                """Return list of files changed vs HEAD in the given directory."""
                if not path or not os.path.isdir(path):
                    return []
                result = subprocess.run(
                    ["${git}", "-C", path, "diff", "--name-only", "HEAD"],
                    capture_output=True, text=True
                )
                files = [f for f in result.stdout.strip().split("\n") if f]
                # Also include staged changes not yet committed
                result2 = subprocess.run(
                    ["${git}", "-C", path, "diff", "--name-only", "--cached"],
                    capture_output=True, text=True
                )
                staged = [f for f in result2.stdout.strip().split("\n") if f]
                # Deduplicate, preserve order
                seen = set()
                combined = []
                for f in files + staged:
                    if f not in seen:
                        seen.add(f)
                        combined.append(f)
                return combined

            # ── rendering ──────────────────────────────────────────────────────────────

            def truncate(s, n):
                return s if len(s) <= n else s[:n-1] + "…"

            def render():
                try:
                    cols = os.get_terminal_size().columns
                except OSError:
                    cols = 80

                sessions = get_sessions()

                # Separate prism project sessions from others
                project_sessions = []
                other_sessions = []
                for s in sessions:
                    if s in ("scratchpad",):
                        other_sessions.append(s)
                    else:
                        project_sessions.append(s)

                if not project_sessions and not other_sessions:
                    print(f"{C_DIM}no sessions{RESET}")
                    return

                # Column widths
                SESSION_W = 32
                STATE_W   = 10
                FILES_W   = max(cols - SESSION_W - STATE_W - 6, 20)

                header = (
                    f"{BOLD}{C_HEADER}"
                    f"{'session':<{SESSION_W}}  {'state':<{STATE_W}}  {'changed files':<{FILES_W}}"
                    f"{RESET}"
                )
                divider = C_DIM + "─" * min(cols, SESSION_W + STATE_W + FILES_W + 4) + RESET

                print()
                print(header)
                print(divider)

                for session in sorted(project_sessions):
                    state, path = get_agent_state(session)
                    colour = STATE_COLOUR.get(state, C_DIM)
                    label  = STATE_LABEL.get(state, f"{state:<10}")

                    files = changed_files(path)
                    if files:
                        files_str = ", ".join(
                            os.path.basename(f) for f in files[:6]
                        )
                        if len(files) > 6:
                            files_str += f" +{len(files)-6}"
                    else:
                        files_str = DIM + "—" + RESET

                    session_display = truncate(session, SESSION_W)
                    files_display   = truncate(files_str, FILES_W)

                    print(
                        f"{colour}{session_display:<{SESSION_W}}{RESET}  "
                        f"{colour}{label:<{STATE_W}}{RESET}  "
                        f"{files_display}"
                    )

                if other_sessions:
                    print(divider)
                    for session in sorted(other_sessions):
                        print(f"{C_DIM}{session}{RESET}")

                print()
                print(f"{C_DIM}  press any key to close{RESET}")

            # ── entry point ────────────────────────────────────────────────────────────

            if __name__ == "__main__":
                render()
                # Block until keypress so the popup stays open
                try:
                    import tty, termios
                    fd = sys.stdin.fileno()
                    old = termios.tcgetattr(fd)
                    try:
                        tty.setraw(fd)
                        sys.stdin.read(1)
                    finally:
                        termios.tcsetattr(fd, termios.TCSADRAIN, old)
                except Exception:
                    # Fallback if terminal manipulation fails
                    input()
          '';
      };

      home.file.".local/scripts/cli.tmux.worktreeCleanup" = {
        executable = true;
        text =
          let
            python = "${pkgs.python3}/bin/python3";
            tmux = "${pkgs.tmux}/bin/tmux";
            git = "${pkgs.git}/bin/git";
          in
          # python
          ''
            #!${python}
            """
            cli.tmux.worktreeCleanup — tear down the current project@worktree session.

            1. Detects the current tmux session name; aborts if it is not project@worktree.
            2. Finds the worktree path via the agent window's pane_current_path.
            3. Confirms with the user (y/n).
            4. Optionally offers to delete the git branch.
            5. Switches to scratchpad (creating it if needed), then kills the session.
            6. Removes the git worktree.
            """
            import subprocess
            import sys
            import os
            import tty
            import termios
            from pathlib import Path

            RESET = "\033[0m"
            BOLD  = "\033[1m"
            RED   = "\033[31m"
            YELLOW = "\033[33m"
            GREEN  = "\033[32m"
            DIM    = "\033[2m"

            def tmux_q(*args):
                r = subprocess.run(["${tmux}"] + list(args), capture_output=True, text=True)
                return r.stdout.strip()

            def tmux_run(*args):
                subprocess.run(["${tmux}"] + list(args), check=True)

            def read_key(prompt):
                """Print prompt and read a single keypress. Returns the character."""
                print(prompt, end="", flush=True)
                fd = sys.stdin.fileno()
                old = termios.tcgetattr(fd)
                try:
                    tty.setraw(fd)
                    ch = sys.stdin.read(1)
                finally:
                    termios.tcsetattr(fd, termios.TCSADRAIN, old)
                print()  # newline after keypress
                return ch

            def wait_key(prompt=None):
                """Block until a keypress, optionally printing a prompt first."""
                if prompt:
                    print(f"\n{DIM}{prompt}{RESET}", flush=True)
                fd = sys.stdin.fileno()
                old = termios.tcgetattr(fd)
                try:
                    tty.setraw(fd)
                    sys.stdin.read(1)
                finally:
                    termios.tcsetattr(fd, termios.TCSADRAIN, old)

            def abort(msg):
                """Print an error, wait for keypress, then exit. Keeps popup visible."""
                print()
                print(f"{RED}{msg}{RESET}")
                wait_key("press any key to close")
                sys.exit(1)

            def get_worktree_path(session):
                """Find the pane_current_path for the agent window in this session."""
                out = tmux_q(
                    "list-windows", "-t", session,
                    "-F", "#{window_name}|#{pane_current_path}"
                )
                for line in out.split("\n"):
                    parts = line.split("|", 1)
                    if len(parts) == 2 and parts[0] == "agent":
                        return parts[1]
                # Fallback: use term window
                for line in out.split("\n"):
                    parts = line.split("|", 1)
                    if len(parts) == 2 and parts[0] == "term":
                        return parts[1]
                return None

            def find_bare_root(worktree_path):
                """Walk up from worktree_path to find the parent with a .bare dir."""
                p = Path(worktree_path)
                for candidate in [p.parent, p.parent.parent]:
                    if (candidate / ".bare").is_dir():
                        return str(candidate)
                return None

            def get_default_branch(bare_path):
                """Return the default branch name for a bare repo."""
                bare_git = str(Path(bare_path) / ".bare")
                ref = subprocess.run(
                    ["${git}", "--git-dir", bare_git,
                     "symbolic-ref", "refs/remotes/origin/HEAD"],
                    capture_output=True, text=True
                )
                if ref.returncode == 0:
                    return ref.stdout.strip().split("/")[-1]
                for branch in ("main", "master"):
                    chk = subprocess.run(
                        ["${git}", "--git-dir", bare_git,
                         "rev-parse", "--verify", f"refs/heads/{branch}"],
                        capture_output=True
                    )
                    if chk.returncode == 0:
                        return branch
                return None

            def branch_exists(bare_path, branch):
                r = subprocess.run(
                    ["${git}", "--git-dir", str(Path(bare_path) / ".bare"),
                     "rev-parse", "--verify", f"refs/heads/{branch}"],
                    capture_output=True
                )
                return r.returncode == 0

            def branch_is_merged(bare_path, branch, default_branch):
                """Return True if branch is fully merged into default_branch."""
                bare_git = str(Path(bare_path) / ".bare")
                r = subprocess.run(
                    ["${git}", "--git-dir", bare_git,
                     "branch", "--merged", default_branch],
                    capture_output=True, text=True
                )
                merged = [b.strip().lstrip("* ") for b in r.stdout.splitlines()]
                return branch in merged

            def delete_branch(bare_path, branch, default_branch):
                bare_git = str(Path(bare_path) / ".bare")
                # cwd=bare_path so git never tries to stat the (possibly deleted) worktree dir
                r = subprocess.run(
                    ["${git}", "--git-dir", bare_git, "branch", "-d", branch],
                    capture_output=True, text=True, cwd=bare_path
                )
                if r.returncode == 0:
                    return
                # Branch not merged — confirm force delete
                print(f"{YELLOW}branch '{branch}' is not fully merged{RESET}")
                key = read_key(f"{YELLOW}force delete anyway? [y/N] {RESET}")
                if key.lower() == "y":
                    r2 = subprocess.run(
                        ["${git}", "--git-dir", bare_git, "branch", "-D", branch],
                        capture_output=True, text=True, cwd=bare_path
                    )
                    if r2.returncode != 0:
                        print(f"{YELLOW}warning: could not delete branch: {r2.stderr.strip()}{RESET}")
                else:
                    print(f"{DIM}branch kept{RESET}")

            def remove_worktree(bare_path, worktree_path):
                r = subprocess.run(
                    ["${git}", "--git-dir", str(Path(bare_path) / ".bare"),
                     "worktree", "remove", "--force", worktree_path],
                    capture_output=True, text=True, cwd=bare_path
                )
                if r.returncode != 0:
                    print(f"{YELLOW}warning: git worktree remove failed: {r.stderr.strip()}{RESET}")
                    print(f"{DIM}you may need to clean up {worktree_path} manually{RESET}")

            def ensure_scratchpad():
                r = subprocess.run(
                    ["${tmux}", "has-session", "-t", "scratchpad"],
                    capture_output=True
                )
                if r.returncode != 0:
                    tmux_run("new-session", "-ds", "scratchpad", "-c", os.path.expanduser("~"))
                    tmux_run("rename-window", "-t", "scratchpad:0", "term")

            def main():
                # $TMUX is set inside display-popup shells; use it to verify we're in tmux.
                # display-message without -t targets the current client = the calling session.
                if not os.environ.get("TMUX"):
                    abort("not running inside tmux — invoke via the tmux binding (prefix+W)")
                session = tmux_q("display-message", "-p", "#{session_name}")

                # Guard: must be a project@worktree session
                if "@" not in session:
                    abort(
                        f"'{session}' is not a worktree session\n"
                        f"  prefix+W only works in project@branch sessions"
                    )

                project_name, worktree_name = session.split("@", 1)

                worktree_path = get_worktree_path(session)
                if not worktree_path:
                    abort("could not determine worktree path from session windows")

                bare_root = find_bare_root(worktree_path)
                if not bare_root:
                    abort(f"could not find bare repo root above {worktree_path}")

                # Guard: refuse to clean up the default branch
                default_branch = get_default_branch(bare_root)
                if default_branch and worktree_name == default_branch:
                    abort(
                        f"refusing to remove the default branch worktree '{worktree_name}'\n"
                        f"  switch to a feature branch session first"
                    )

                print()
                print(f"{BOLD}prism worktree cleanup{RESET}")
                print(f"  session  : {BOLD}{session}{RESET}")
                print(f"  worktree : {DIM}{worktree_path}{RESET}")
                print(f"  branch   : {BOLD}{worktree_name}{RESET}")
                print()

                key = read_key(f"{YELLOW}remove this worktree and session? [y/N] {RESET}")
                if key.lower() != "y":
                    print(f"{DIM}cancelled{RESET}")
                    wait_key("press any key to close")
                    sys.exit(0)

                # Offer branch deletion
                delete_branch_flag = False
                if branch_exists(bare_root, worktree_name):
                    key2 = read_key(f"{YELLOW}also delete branch '{worktree_name}'? [y/N] {RESET}")
                    delete_branch_flag = key2.lower() == "y"

                # Do all destructive work, then switch away and kill session last.
                # (switching first would close the popup before we're done)
                print(f"{DIM}removing worktree...{RESET}", flush=True)
                remove_worktree(bare_root, worktree_path)

                if delete_branch_flag:
                    print(f"{DIM}deleting branch {worktree_name}...{RESET}", flush=True)
                    delete_branch(bare_root, worktree_name, default_branch or worktree_name)

                print(f"{GREEN}done{RESET}")
                wait_key("press any key to close")

                # Switch only the current client (not all clients) then kill session.
                # Using -c client_name prevents other attached sessions from being disturbed.
                client = tmux_q("display-message", "-p", "#{client_name}")
                ensure_scratchpad()
                if client:
                    tmux_run("switch-client", "-c", client, "-t", "scratchpad")
                else:
                    tmux_run("switch-client", "-t", "scratchpad")
                subprocess.run(["${tmux}", "kill-session", "-t", session], capture_output=True)

            if __name__ == "__main__":
                main()
          '';
      };

      programs.zsh.shellAliases = {
        gwc = "cli.git.worktreeClone";
      };
    };
  };
}
