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

      programs.zsh.shellAliases = {
        gwc = "cli.git.worktreeClone";
      };
    };
  };
}
