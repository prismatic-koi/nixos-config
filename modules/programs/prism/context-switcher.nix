{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.contextSwitcher.enable = lib.mkEnableOption "enables tmux context switcher" // {
      default = true;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.programs.prism.contextSwitcher.enable
        # no point in installing if tmux is not
        && config.nx.programs.prism.tmux.enable
      )
      {
        home-manager.users.ben = {
          # making sure scripts are on path if not set elsewhere
          home.sessionPath = [ "$HOME/.local/scripts" ];

          # Python-based context switcher that opens tmux sessions via fzy popup
          home.file.".local/scripts/cli.tmux.contextSwitcher" = {
            executable = true;
            text =
              let
                tmux = "${pkgs.tmux}/bin/tmux";
                python = "${pkgs.python3}/bin/python3";
                fzy = "${pkgs.fzy}/bin/fzy";
              in
              # python
              ''
                #!${python}
                import os
                import subprocess
                import sys
                from pathlib import Path

                def get_project_list():
                    """Get list of available projects from projectGetter script."""
                    try:
                        result = subprocess.run(
                            ["cli.tmux.projectGetter"],
                            capture_output=True,
                            text=True,
                            check=True
                        )
                        projects = result.stdout.strip().split('\n')
                        # Filter out empty strings
                        projects = [p for p in projects if p]
                        # Add scratchpad option at the top
                        projects.insert(0, "[scratchpad]")
                        return projects
                    except subprocess.CalledProcessError:
                        return ["[scratchpad]"]

                def get_worktrees(project_path):
                    """Get list of worktrees for a bare repository, with default branch at top."""
                    bare_path = Path(project_path) / ".bare"

                    # Get existing worktrees
                    try:
                        result = subprocess.run(
                            ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "worktree", "list", "--porcelain"],
                            capture_output=True,
                            text=True,
                            check=True
                        )

                        worktrees = []
                        current_worktree = None
                        is_bare = False

                        for line in result.stdout.strip().split('\n'):
                            if line.startswith('worktree '):
                                # Save current worktree if it's not bare
                                if current_worktree and not is_bare:
                                    worktrees.append(current_worktree)

                                # Start new worktree
                                current_worktree = line[9:]  # Remove 'worktree ' prefix
                                is_bare = False
                            elif line.strip() == 'bare':
                                # This worktree is the bare repo itself, skip it
                                is_bare = True

                        # Add the last worktree if not bare
                        if current_worktree and not is_bare:
                            worktrees.append(current_worktree)

                        # Sort worktrees, putting default branch at top
                        # Get default branch
                        default_ref = subprocess.run(
                            ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "symbolic-ref", "refs/remotes/origin/HEAD"],
                            capture_output=True,
                            text=True
                        )

                        default_branch = None
                        if default_ref.returncode == 0:
                            default_branch = default_ref.stdout.strip().split('/')[-1]
                        else:
                            # Fallback to main or master
                            for branch in ["main", "master"]:
                                check = subprocess.run(
                                    ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{branch}"],
                                    capture_output=True
                                )
                                if check.returncode == 0:
                                    default_branch = branch
                                    break

                        if default_branch:
                            default_path = str(Path(project_path) / default_branch)
                            # Remove default from list if present, then add at top
                            other_worktrees = [w for w in worktrees if w != default_path]
                            if default_path in worktrees:
                                worktrees = [default_path] + other_worktrees

                        return worktrees
                    except subprocess.CalledProcessError:
                        return []

                def is_bare_repo(directory):
                    """Check if directory contains a .bare subdirectory."""
                    bare_path = Path(directory) / ".bare"
                    return bare_path.is_dir()

                def create_worktree(project_path, worktree_name):
                    """Create a new git worktree."""
                    bare_path = Path(project_path) / ".bare"
                    worktree_path = Path(project_path) / worktree_name

                    try:
                        # Check if branch exists remotely or locally
                        branch_exists = subprocess.run(
                            ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{worktree_name}"],
                            capture_output=True
                        )

                        if branch_exists.returncode == 0:
                            # Branch exists, just create worktree
                            subprocess.run(
                                ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "worktree", "add", str(worktree_path), worktree_name],
                                check=True
                            )
                        else:
                            # Check if branch exists on remote
                            remote_exists = subprocess.run(
                                ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/remotes/origin/{worktree_name}"],
                                capture_output=True
                            )

                            if remote_exists.returncode == 0:
                                # Remote branch exists, track it
                                subprocess.run(
                                    ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "worktree", "add", str(worktree_path), "-b", worktree_name, f"origin/{worktree_name}"],
                                    check=True
                                )
                            else:
                                # Create new branch - find default branch
                                default_ref = subprocess.run(
                                    ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "symbolic-ref", "refs/remotes/origin/HEAD"],
                                    capture_output=True,
                                    text=True
                                )

                                if default_ref.returncode == 0:
                                    default_branch = default_ref.stdout.strip().split('/')[-1]
                                else:
                                    # Fallback to main or master
                                    for branch in ["main", "master"]:
                                        check = subprocess.run(
                                            ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{branch}"],
                                            capture_output=True
                                        )
                                        if check.returncode == 0:
                                            default_branch = branch
                                            break
                                    else:
                                        default_branch = "HEAD"

                                # Create new branch from default
                                subprocess.run(
                                    ["${pkgs.git}/bin/git", "--git-dir", str(bare_path), "worktree", "add", "-b", worktree_name, str(worktree_path), default_branch],
                                    check=True
                                )

                        return str(worktree_path)
                    except subprocess.CalledProcessError as e:
                        return None

                def create_or_switch_session(selected_path, project_root=None):
                    """Create a new tmux session or switch to existing one.

                    Args:
                        selected_path: The directory path to open (worktree or regular project)
                        project_root: Optional root project directory (for worktree session naming)
                    """
                    if not selected_path:
                        print("Error: No path provided to create_or_switch_session", file=sys.stderr)
                        return

                    # Handle scratchpad specially
                    if selected_path == "[scratchpad]":
                        session_name = "scratchpad"
                        directory = os.path.expanduser("~")
                        is_scratchpad = True
                    else:
                        # Expand path and verify it exists
                        directory = os.path.expanduser(selected_path)
                        if not os.path.isdir(directory):
                            print(f"Error: Directory does not exist: {directory}", file=sys.stderr)
                            return

                        # For worktrees, create session name as "project@worktree"
                        if project_root:
                            project_name = Path(project_root).name.replace('.', '_')
                            worktree_name = Path(directory).name.replace('.', '_')
                            session_name = f"{project_name}@{worktree_name}"
                        else:
                            session_name = Path(directory).name.replace('.', '_')

                        is_scratchpad = False

                    # Check if session already exists
                    check_session = subprocess.run(
                        ["${tmux}", "has-session", "-t", session_name],
                        capture_output=True
                    )
                    session_exists = check_session.returncode == 0

                    if not session_exists:
                        # Create new session
                        subprocess.run(
                            ["${tmux}", "new-session", "-ds", session_name, "-c", directory],
                            check=True
                        )

                        if is_scratchpad:
                            # Scratchpad only needs the term window
                            subprocess.run(
                                ["${tmux}", "rename-window", "-t", f"{session_name}:0", "term"],
                                check=True
                            )
                        else:
                            # Regular project gets the full window setup
                            # Rename first window to "edit" and launch nvim
                            subprocess.run(
                                ["${tmux}", "rename-window", "-t", f"{session_name}:0", "edit"],
                                check=True
                            )

                            # Check for specific files to open
                            selected_file = None
                            dir_path = Path(directory)

                            # Check for single file in directory
                            files = list(dir_path.glob('*'))
                            files = [f for f in files if f.is_file()]

                            if len(files) == 1:
                                selected_file = str(files[0])
                            else:
                                # Check for landing pages
                                if "obsidian" in directory:
                                    landing_page = dir_path / "notes" / "landingpage.md"
                                    if landing_page.exists():
                                        selected_file = str(landing_page)
                                else:
                                    readme = dir_path / "README.md"
                                    if readme.exists():
                                        selected_file = str(readme)

                            # Launch nvim
                            if selected_file:
                                subprocess.run(
                                    ["${tmux}", "send-keys", "-t", f"{session_name}:0", f"nvim '{selected_file}'", "C-m"],
                                    check=True
                                )
                            else:
                                subprocess.run(
                                    ["${tmux}", "send-keys", "-t", f"{session_name}:0", "nvim", "C-m"],
                                    check=True
                                )

                            # Create agent window
                            subprocess.run(
                                ["${tmux}", "new-window", "-t", f"{session_name}:1", "-n", "agent", "-c", directory],
                                check=True
                            )
                            agentEnvPrefix = "${config.nx.programs.prism._internal.agentEnvPrefix}"
                            subprocess.run(
                                ["${tmux}", "send-keys", "-t", f"{session_name}:1", f"{agentEnvPrefix} opencode", "C-m"],
                                check=True
                            )

                            # Create term window
                            subprocess.run(
                                ["${tmux}", "new-window", "-t", f"{session_name}:2", "-n", "term", "-c", directory],
                                check=True
                            )

                            # Select the edit window
                            subprocess.run(
                                ["${tmux}", "select-window", "-t", f"{session_name}:0"],
                                check=True
                            )

                    # Switch to the session
                    subprocess.run(
                        ["${tmux}", "switch-client", "-t", session_name],
                        check=True
                    )

                def main():
                    """Main function to run the context switcher."""
                    projects = get_project_list()

                    # Create a formatted list for fzy
                    project_list = '\n'.join(projects)

                    # Run fzy to select a project
                    try:
                        result = subprocess.run(
                            ["${fzy}"],
                            input=project_list,
                            capture_output=True,
                            text=True,
                            check=True
                        )
                        selected = result.stdout.strip()

                        if not selected:
                            sys.exit(0)

                        # Check if selected project has worktrees
                        if selected != "[scratchpad]" and is_bare_repo(selected):
                            # Get worktrees and show in fzy
                            worktrees = get_worktrees(selected)

                            if not worktrees:
                                sys.exit(1)

                            worktree_list = '\n'.join(worktrees)

                            # Call fzy exactly like the project selection does
                            try:
                                result = subprocess.run(
                                    ["${fzy}"],
                                    input=worktree_list,
                                    capture_output=True,
                                    text=True,
                                    check=True
                                )
                                selected_worktree = result.stdout.strip()

                                if not selected_worktree:
                                    sys.exit(0)

                                # Check if this is an existing worktree path or a new name
                                if os.path.isdir(selected_worktree):
                                    # Existing worktree - use it directly
                                    create_or_switch_session(selected_worktree, selected)
                                else:
                                    # New worktree name typed - create it
                                    worktree_path = create_worktree(selected, selected_worktree)
                                    if worktree_path:
                                        create_or_switch_session(worktree_path, selected)
                                    else:
                                        sys.exit(1)
                            except subprocess.CalledProcessError:
                                # User cancelled
                                sys.exit(0)
                            except subprocess.CalledProcessError as e:
                                # fzy failed with error
                                print(f"Error running fzy: {e}", file=sys.stderr)
                                sys.exit(1)
                        else:
                            # No worktrees, proceed normally
                            create_or_switch_session(selected, None)
                    except subprocess.CalledProcessError:
                        # User cancelled or fzy failed
                        sys.exit(0)

                if __name__ == "__main__":
                    main()
              '';
          };

          # Agent-only session launcher for bead workflows
          # Creates a background tmux session with only an agent window for focused bead work
          home.file.".local/scripts/cli.prism.agentSession" = {
            executable = true;
            text =
              let
                tmux = "${pkgs.tmux}/bin/tmux";
                git = "${pkgs.git}/bin/git";
                python = "${pkgs.python3}/bin/python3";
              in
              # python
              ''
                #!${python}
                import os
                import subprocess
                import sys
                from pathlib import Path

                def is_bare_repo(directory):
                    """Check if directory contains a .bare subdirectory."""
                    bare_path = Path(directory) / ".bare"
                    return bare_path.is_dir()

                def validate_bead_id(bead_id):
                    """Validate bead ID format (prefix-id)."""
                    import re
                    # Bead IDs follow pattern: <prefix>-<id> where both parts are alphanumeric
                    return bool(re.match(r'^[a-z0-9]+-[a-z0-9]+$', bead_id))

                def create_worktree(project_path, worktree_name):
                    """Create a new git worktree."""
                    bare_path = Path(project_path) / ".bare"
                    worktree_path = Path(project_path) / worktree_name

                    try:
                        # Check if branch exists remotely or locally
                        branch_exists = subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{worktree_name}"],
                            capture_output=True
                        )

                        if branch_exists.returncode == 0:
                            # Branch exists, just create worktree
                            subprocess.run(
                                ["${git}", "--git-dir", str(bare_path), "worktree", "add", str(worktree_path), worktree_name],
                                check=True,
                                capture_output=True
                            )
                        else:
                            # Check if branch exists on remote
                            remote_exists = subprocess.run(
                                ["${git}", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/remotes/origin/{worktree_name}"],
                                capture_output=True
                            )

                            if remote_exists.returncode == 0:
                                # Remote branch exists, track it
                                subprocess.run(
                                    ["${git}", "--git-dir", str(bare_path), "worktree", "add", str(worktree_path), "-b", worktree_name, f"origin/{worktree_name}"],
                                    check=True,
                                    capture_output=True
                                )
                            else:
                                # Create new branch - find default branch
                                default_ref = subprocess.run(
                                    ["${git}", "--git-dir", str(bare_path), "symbolic-ref", "refs/remotes/origin/HEAD"],
                                    capture_output=True,
                                    text=True
                                )

                                if default_ref.returncode == 0:
                                    default_branch = default_ref.stdout.strip().split('/')[-1]
                                    # Use origin/branch to get latest from remote
                                    start_point = f"origin/{default_branch}"
                                else:
                                    # Fallback to main or master
                                    for branch in ["main", "master"]:
                                        check = subprocess.run(
                                            ["${git}", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{branch}"],
                                            capture_output=True
                                        )
                                        if check.returncode == 0:
                                            default_branch = branch
                                            start_point = branch
                                            break
                                    else:
                                        default_branch = "HEAD"
                                        start_point = "HEAD"

                                # Create new branch from default (using origin to get latest)
                                subprocess.run(
                                    ["${git}", "--git-dir", str(bare_path), "worktree", "add", "-b", worktree_name, str(worktree_path), start_point],
                                    check=True,
                                    capture_output=True
                                )

                        return str(worktree_path)
                    except subprocess.CalledProcessError as e:
                        return None

                def main():
                    """Main function to create agent-only session for bead work."""
                    if len(sys.argv) < 3:
                        print("Usage: cli.prism.agentSession <repo-path> <bead-id>", file=sys.stderr)
                        print("", file=sys.stderr)
                        print("Example: cli.prism.agentSession ~/code/myproject beads-abc123", file=sys.stderr)
                        sys.exit(1)

                    repo_path = sys.argv[1]
                    bead_id = sys.argv[2]

                    # Expand and validate repository path
                    repo_path = os.path.expanduser(repo_path)
                    if not os.path.isdir(repo_path):
                        print(f"Error: Repository path does not exist: {repo_path}", file=sys.stderr)
                        sys.exit(1)

                    if not is_bare_repo(repo_path):
                        print(f"Error: Repository is not a bare repo (missing .bare directory): {repo_path}", file=sys.stderr)
                        sys.exit(1)

                    # Validate bead ID format
                    if not validate_bead_id(bead_id):
                        print(f"Error: Invalid bead ID format: {bead_id}", file=sys.stderr)
                        print("Bead ID must match pattern: <prefix>-<id> (e.g., main-abc123)", file=sys.stderr)
                        sys.exit(1)

                    # Build session name
                    project_name = Path(repo_path).name.replace('.', '_')
                    worktree_name = bead_id.replace('.', '_')
                    session_name = f"{project_name}@{worktree_name}"

                    # Check if session already exists
                    check_session = subprocess.run(
                        ["${tmux}", "has-session", "-t", session_name],
                        capture_output=True
                    )
                    if check_session.returncode == 0:
                        print(f"Error: Session '{session_name}' already exists for bead {bead_id}", file=sys.stderr)
                        print(f"Use 'tmux attach-session -t {session_name}' to connect", file=sys.stderr)
                        sys.exit(1)

                    # Check if worktree exists or create it
                    worktree_path = Path(repo_path) / bead_id
                    if not worktree_path.is_dir():
                        print(f"Creating worktree for {bead_id}...")
                        result = create_worktree(repo_path, bead_id)
                        if not result:
                            print(f"Error: Failed to create worktree for {bead_id}", file=sys.stderr)
                            sys.exit(1)
                        worktree_path = Path(result)
                    else:
                        print(f"Using existing worktree: {worktree_path}")

                    # Setup shared beads via redirect file
                    bare_path = Path(repo_path) / ".bare"
                    beads_dir = worktree_path / ".beads"
                    beads_dir.mkdir(exist_ok=True)
                    
                    redirect_file = beads_dir / "redirect"
                    if not redirect_file.exists():
                        print(f"Setting up shared beads via redirect...")
                        # Find default branch name dynamically
                        default_ref = subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "symbolic-ref", "refs/remotes/origin/HEAD"],
                            capture_output=True,
                            text=True
                        )
                        
                        if default_ref.returncode == 0:
                            default_branch = default_ref.stdout.strip().split('/')[-1]
                        else:
                            # Fallback to main or master
                            for branch in ["main", "master"]:
                                check = subprocess.run(
                                    ["${git}", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{branch}"],
                                    capture_output=True
                                )
                                if check.returncode == 0:
                                    default_branch = branch
                                    break
                            else:
                                default_branch = "main"  # Final fallback
                        
                        # Point to default branch worktree's .beads directory
                        redirect_file.write_text(f"../{default_branch}/.beads\n")
                    else:
                        print(f"Using existing beads redirect: {redirect_file.read_text().strip()}")

                    # Assign work bead to agent atomically
                    print(f"Assigning bead {bead_id} to agent...")
                    agent_id = f"{project_name}-agent-{worktree_name}"
                    
                    # Update work bead: set status=hooked and assignee
                    assign_result = subprocess.run(
                        ["bd", "update", bead_id, "--status=hooked", f"--assignee={agent_id}"],
                        cwd=str(repo_path),
                        capture_output=True,
                        text=True
                    )
                    if assign_result.returncode != 0:
                        print(f"Warning: Failed to assign bead: {assign_result.stderr}", file=sys.stderr)
                    else:
                        print(f"✓ Bead assigned to agent: {agent_id}")

                    # Create tmux session in background (detached)
                    print(f"Creating agent session '{session_name}'...")
                    subprocess.run(
                        ["${tmux}", "new-session", "-ds", session_name, "-c", str(worktree_path)],
                        check=True
                    )

                    # Rename window to "agent"
                    subprocess.run(
                        ["${tmux}", "rename-window", "-t", f"{session_name}:0", "agent"],
                        check=True
                    )

                    # Launch opencode with soku agent and initial prompt
                    agentEnvPrefix = "${config.nx.programs.prism._internal.agentEnvPrefix}"
                    initial_prompt = f"Your bead to work on is {bead_id}. You can read about it with: bd show {bead_id} --json"
                    subprocess.run(
                        ["${tmux}", "send-keys", "-t", f"{session_name}:0", f"{agentEnvPrefix} opencode --agent soku --prompt '{initial_prompt}'", "C-m"],
                        check=True
                    )

                    print(f"✓ Agent session created: {session_name}")
                    print(f"  Working on bead: {bead_id}")
                    print(f"  Worktree: {worktree_path}")
                    print(f"  Connect with: tmux attach-session -t {session_name}")

                if __name__ == "__main__":
                    main()
              '';
          };
        };
      };
}
