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
        home-manager.users.${config.nx.username} = {
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
                git = "${pkgs.git}/bin/git";
                worktreeExcludePyList = config.nx.programs.prism._internal.worktreeExcludePyList;
              in
              # python
              ''
                #!${python}
                import os
                import re
                import subprocess
                import sys
                from pathlib import Path

                # Repo directory names that should never be auto-converted to bare+worktree
                WORKTREE_EXCLUDE = ${worktreeExcludePyList}

                # ── Git helpers ────────────────────────────────────────────────────────────

                def is_bare_repo(directory):
                    """Check if directory contains a .bare subdirectory (prism bare layout)."""
                    return (Path(directory) / ".bare").is_dir()

                def is_regular_git_repo(directory):
                    """Check if directory is a regular (non-bare) git repo."""
                    dot_git = Path(directory) / ".git"
                    # .git can be a file (submodule) or a directory
                    return dot_git.exists() and not is_bare_repo(directory)

                def is_excluded(directory):
                    """Return True if this repo should be skipped for worktree conversion."""
                    name = Path(directory).name
                    return name in WORKTREE_EXCLUDE

                def get_default_branch(bare_path):
                    """Return the default branch name for a bare repo."""
                    ref = subprocess.run(
                        ["${git}", "--git-dir", str(bare_path), "symbolic-ref", "refs/remotes/origin/HEAD"],
                        capture_output=True, text=True
                    )
                    if ref.returncode == 0:
                        return ref.stdout.strip().split("/")[-1]
                    # Fallback
                    for branch in ("main", "master"):
                        chk = subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "rev-parse", "--verify", f"refs/heads/{branch}"],
                            capture_output=True
                        )
                        if chk.returncode == 0:
                            return branch
                    return None

                def get_worktrees(project_path):
                    """Return list of worktree paths for a bare repo, default branch first."""
                    bare_path = Path(project_path) / ".bare"
                    try:
                        result = subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "worktree", "list", "--porcelain"],
                            capture_output=True, text=True, check=True
                        )
                    except subprocess.CalledProcessError:
                        return []

                    worktrees = []
                    current = None
                    is_bare = False
                    for line in result.stdout.strip().split("\n"):
                        if line.startswith("worktree "):
                            if current and not is_bare:
                                worktrees.append(current)
                            current = line[9:]
                            is_bare = False
                        elif line.strip() == "bare":
                            is_bare = True
                    if current and not is_bare:
                        worktrees.append(current)

                    default_branch = get_default_branch(bare_path)
                    if default_branch:
                        default_path = str(Path(project_path) / default_branch)
                        others = [w for w in worktrees if w != default_path]
                        if default_path in worktrees:
                            worktrees = [default_path] + others

                    return worktrees

                def step(msg):
                    """Print a progress step, flushing immediately so it appears during long ops."""
                    print(msg, flush=True)

                def convert_to_bare(directory):
                    """
                    Convert a regular git repo in-place to the prism bare+worktree layout.
                    Prints progress steps throughout. Returns the worktree path, or None on failure.
                    """
                    import shutil
                    dir_path = Path(directory)
                    bare_path = dir_path / ".bare"
                    git_file = dir_path / ".git"
                    orig_git = dir_path / ".git.orig"

                    step(f"converting {dir_path.name} to bare+worktree layout...")

                    # Detect default branch before we touch anything
                    head = subprocess.run(
                        ["${git}", "-C", str(dir_path), "symbolic-ref", "--short", "HEAD"],
                        capture_output=True, text=True
                    )
                    default_branch = head.stdout.strip() if head.returncode == 0 else "main"
                    step(f"  default branch: {default_branch}")

                    # Get remote URL
                    remote = subprocess.run(
                        ["${git}", "-C", str(dir_path), "remote", "get-url", "origin"],
                        capture_output=True, text=True
                    )
                    if remote.returncode != 0:
                        print("error: could not get remote URL", file=sys.stderr)
                        return None

                    repo_url = remote.stdout.strip()
                    step(f"  remote: {repo_url}")

                    # Move existing .git out of the way temporarily
                    git_file.rename(orig_git)

                    try:
                        # Clone bare — this is the slow network step, show clearly
                        step(f"  cloning bare repo (this may take a moment)...")
                        result = subprocess.run(
                            ["${git}", "clone", "--bare", repo_url, str(bare_path)],
                            capture_output=True, text=True
                        )
                        if result.returncode != 0:
                            raise Exception(f"bare clone failed: {result.stderr.strip()}")
                        step("  bare clone done")

                        # Configure fetch refspec
                        step("  configuring remote tracking refs...")
                        subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "config",
                             "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"],
                            check=True, capture_output=True
                        )
                        subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "fetch", "origin"],
                            check=True, capture_output=True
                        )

                        # Write .git file pointer
                        git_file.write_text("gitdir: ./.bare\n")

                        # Set upstream tracking for default branch
                        subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "branch",
                             "--set-upstream-to", f"origin/{default_branch}", default_branch],
                            capture_output=True
                        )

                        # Move existing working tree contents into <default_branch>/
                        step(f"  moving working tree into {default_branch}/...")
                        worktree_path = dir_path / default_branch
                        worktree_path.mkdir()
                        for item in dir_path.iterdir():
                            if item.name in (".bare", ".git", ".git.orig", default_branch):
                                continue
                            item.rename(worktree_path / item.name)

                        # Manually register the worktree rather than using `git worktree add
                        # --force`, which refuses to set up a .git file in a non-empty directory.
                        # We write the .git pointer into the worktree dir, then register the
                        # worktree metadata in .bare/worktrees/<name>/.
                        step("  registering worktree...")

                        # .git file inside the worktree pointing back at the bare
                        worktree_git_file = worktree_path / ".git"
                        # The path stored in .git must be relative or absolute to .bare
                        # Use absolute path for reliability
                        worktrees_dir = bare_path / "worktrees" / default_branch
                        worktrees_dir.mkdir(parents=True, exist_ok=True)

                        # gitdir file inside worktree: points at the worktrees entry
                        worktree_git_file.write_text(
                            f"gitdir: {worktrees_dir}/gitdir\n"
                        )

                        # gitdir file inside .bare/worktrees/<name>/: points at worktree .git
                        (worktrees_dir / "gitdir").write_text(
                            f"{worktree_git_file}\n"
                        )

                        # commondir: points back at the bare repo
                        (worktrees_dir / "commondir").write_text("../..\n")

                        # HEAD: records which branch this worktree is on
                        (worktrees_dir / "HEAD").write_text(
                            f"ref: refs/heads/{default_branch}\n"
                        )

                        # Remove the backed-up .git (may be at original location or,
                        # if a previous run moved it, inside the worktree dir)
                        for candidate in [orig_git, worktree_path / ".git.orig"]:
                            if candidate.exists():
                                shutil.rmtree(str(candidate))

                        # Prune stale worktree entries and verify registration
                        subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "worktree", "prune"],
                            capture_output=True
                        )

                        step(f"  done — worktree at {worktree_path}")
                        return str(worktree_path)

                    except Exception as e:
                        # Roll back: restore original .git
                        if orig_git.exists() and not git_file.exists():
                            orig_git.rename(git_file)
                        print(f"error during conversion: {e}", file=sys.stderr)
                        return None

                def sanitise_branch_name(raw):
                    """
                    Convert free-form text to a valid git branch name.
                    e.g. 'Fix login Bug' → 'fix-login-bug'
                    """
                    name = raw.strip().lower()
                    # Replace whitespace and common separators with dashes
                    name = re.sub(r"[\s/_]+", "-", name)
                    # Strip chars not valid in branch names
                    name = re.sub(r"[^a-z0-9\-.]", "", name)
                    # Collapse multiple dashes
                    name = re.sub(r"-{2,}", "-", name)
                    # Strip leading/trailing dashes or dots
                    name = name.strip("-.")
                    return name

                def create_worktree(project_path, worktree_name):
                    """Create a new git worktree for worktree_name (already sanitised)."""
                    bare_path = Path(project_path) / ".bare"
                    worktree_path = Path(project_path) / worktree_name

                    try:
                        branch_exists = subprocess.run(
                            ["${git}", "--git-dir", str(bare_path), "rev-parse",
                             "--verify", f"refs/heads/{worktree_name}"],
                            capture_output=True
                        )
                        if branch_exists.returncode == 0:
                            subprocess.run(
                                ["${git}", "--git-dir", str(bare_path), "worktree",
                                 "add", str(worktree_path), worktree_name],
                                check=True
                            )
                        else:
                            remote_exists = subprocess.run(
                                ["${git}", "--git-dir", str(bare_path), "rev-parse",
                                 "--verify", f"refs/remotes/origin/{worktree_name}"],
                                capture_output=True
                            )
                            if remote_exists.returncode == 0:
                                subprocess.run(
                                    ["${git}", "--git-dir", str(bare_path), "worktree",
                                     "add", str(worktree_path), "-b", worktree_name,
                                     f"origin/{worktree_name}"],
                                    check=True
                                )
                            else:
                                default_branch = get_default_branch(bare_path) or "HEAD"
                                subprocess.run(
                                    ["${git}", "--git-dir", str(bare_path), "worktree",
                                     "add", "-b", worktree_name, str(worktree_path), default_branch],
                                    check=True
                                )
                        return str(worktree_path)
                    except subprocess.CalledProcessError:
                        return None

                # ── tmux helpers ───────────────────────────────────────────────────────────

                def create_or_switch_session(selected_path, project_root=None):
                    """Create (or attach to) a tmux session for selected_path."""
                    if not selected_path:
                        print("Error: no path provided", file=sys.stderr)
                        return

                    if selected_path == "[scratchpad]":
                        session_name = "scratchpad"
                        directory = os.path.expanduser("~")
                        is_scratchpad = True
                    else:
                        directory = os.path.expanduser(selected_path)
                        if not os.path.isdir(directory):
                            print(f"Error: directory does not exist: {directory}", file=sys.stderr)
                            return
                        if project_root:
                            project_name = Path(project_root).name.replace(".", "_")
                            worktree_name = Path(directory).name.replace(".", "_")
                            session_name = f"{project_name}@{worktree_name}"
                        else:
                            session_name = Path(directory).name.replace(".", "_")
                        is_scratchpad = False

                    session_exists = subprocess.run(
                        ["${tmux}", "has-session", "-t", session_name],
                        capture_output=True
                    ).returncode == 0

                    if not session_exists:
                        subprocess.run(
                            ["${tmux}", "new-session", "-ds", session_name, "-c", directory],
                            check=True
                        )

                        if is_scratchpad:
                            subprocess.run(
                                ["${tmux}", "rename-window", "-t", f"{session_name}:0", "term"],
                                check=True
                            )
                        else:
                            subprocess.run(
                                ["${tmux}", "rename-window", "-t", f"{session_name}:0", "edit"],
                                check=True
                            )

                            selected_file = None
                            dir_path = Path(directory)
                            files = [f for f in dir_path.glob("*") if f.is_file()]
                            if len(files) == 1:
                                selected_file = str(files[0])
                            elif "obsidian" in directory:
                                landing = dir_path / "notes" / "landingpage.md"
                                if landing.exists():
                                    selected_file = str(landing)
                            else:
                                readme = dir_path / "README.md"
                                if readme.exists():
                                    selected_file = str(readme)

                            nvim_cmd = f"nvim '{selected_file}'" if selected_file else "nvim"
                            subprocess.run(
                                ["${tmux}", "send-keys", "-t", f"{session_name}:0", nvim_cmd, "C-m"],
                                check=True
                            )

                            agentEnvPrefix = "${config.nx.programs.prism._internal.agentEnvPrefix}"
                            subprocess.run(
                                ["${tmux}", "new-window", "-t", f"{session_name}:1", "-n", "agent", "-c", directory],
                                check=True
                            )
                            subprocess.run(
                                ["${tmux}", "send-keys", "-t", f"{session_name}:1",
                                 f"{agentEnvPrefix} opencode", "C-m"],
                                check=True
                            )

                            subprocess.run(
                                ["${tmux}", "new-window", "-t", f"{session_name}:2", "-n", "term", "-c", directory],
                                check=True
                            )

                            focus = 0 if "obsidian" in directory else 1
                            subprocess.run(
                                ["${tmux}", "select-window", "-t", f"{session_name}:{focus}"],
                                check=True
                            )

                    subprocess.run(["${tmux}", "switch-client", "-t", session_name], check=True)

                def open_path(path):
                    """Open a specific path directly, handling bare repos by auto-selecting default branch."""
                    directory = os.path.expanduser(path)
                    if not os.path.isdir(directory):
                        print(f"Error: directory does not exist: {directory}", file=sys.stderr)
                        sys.exit(1)
                    if is_bare_repo(directory):
                        worktrees = get_worktrees(directory)
                        if not worktrees:
                            print(f"Error: no worktrees found in {directory}", file=sys.stderr)
                            sys.exit(1)
                        create_or_switch_session(worktrees[0], directory)
                    else:
                        create_or_switch_session(directory, None)

                # ── project list ───────────────────────────────────────────────────────────

                def get_project_list():
                    try:
                        result = subprocess.run(
                            ["cli.tmux.projectGetter"],
                            capture_output=True, text=True, check=True
                        )
                        projects = [p for p in result.stdout.strip().split("\n") if p]
                        projects.insert(0, "[scratchpad]")
                        projects.insert(0, "[dashboard]")
                        return projects
                    except subprocess.CalledProcessError:
                        return ["[dashboard]", "[scratchpad]"]

                # ── fzy helpers ────────────────────────────────────────────────────────────

                def fzy_pick(items, prompt=None):
                    """Show fzy picker and return selected item, or None if cancelled."""
                    cmd = ["${fzy}"]
                    if prompt:
                        cmd += ["-p", prompt]
                    try:
                        result = subprocess.run(
                            cmd,
                            input="\n".join(items),
                            capture_output=True, text=True, check=True
                        )
                        return result.stdout.strip() or None
                    except subprocess.CalledProcessError:
                        return None

                # ── main flow ──────────────────────────────────────────────────────────────

                def handle_bare_repo(selected):
                    """Second-level picker for bare repos: existing worktrees + create-new option."""
                    worktrees = get_worktrees(selected)
                    CREATE_NEW = "[+ create new worktree]"
                    display_items = [Path(w).name for w in worktrees] + [CREATE_NEW]

                    chosen = fzy_pick(display_items, prompt="worktree> ")
                    if not chosen:
                        return

                    if chosen == CREATE_NEW:
                        # Ask user to type a branch name
                        raw = fzy_pick([""], prompt="branch name> ")
                        if not raw:
                            return
                        branch = sanitise_branch_name(raw)
                        if not branch:
                            print("Error: branch name is empty after sanitisation", file=sys.stderr)
                            sys.exit(1)
                        worktree_path = create_worktree(selected, branch)
                        if worktree_path:
                            create_or_switch_session(worktree_path, selected)
                    else:
                        # Map display name back to full path
                        match = next((w for w in worktrees if Path(w).name == chosen), None)
                        if match:
                            create_or_switch_session(match, selected)

                def handle_regular_repo(selected):
                    """Offer to convert a regular repo to bare+worktree layout, or just open it."""
                    if is_excluded(selected):
                        create_or_switch_session(selected, None)
                        return

                    OPEN_DIRECTLY = "[open directly (no worktrees)]"
                    CONVERT = "[convert to bare+worktree layout]"
                    choice = fzy_pick([OPEN_DIRECTLY, CONVERT],
                                      prompt=f"{Path(selected).name} is a regular repo> ")
                    if not choice:
                        return

                    if choice == CONVERT:
                        worktree_path = convert_to_bare(selected)
                        if worktree_path:
                            create_or_switch_session(worktree_path, selected)
                        else:
                            print("Conversion failed, opening directly", file=sys.stderr)
                            create_or_switch_session(selected, None)
                    else:
                        create_or_switch_session(selected, None)

                def ensure_dashboard_session():
                    """Ensure the prism-dashboard session exists in the background."""
                    r = subprocess.run(
                        ["${tmux}", "has-session", "-t", "prism-dashboard"],
                        capture_output=True
                    )
                    if r.returncode != 0:
                        subprocess.Popen(
                            ["${tmux}", "new-session", "-ds", "prism-dashboard",
                             "-n", "dashboard", "while true; do prism dashboard --popup; done"],
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL,
                        )

                def main():
                    if len(sys.argv) >= 3 and sys.argv[1] == "--path":
                        open_path(sys.argv[2])
                        return

                    # Ensure dashboard session is running in the background
                    ensure_dashboard_session()

                    projects = get_project_list()
                    selected = fzy_pick(projects, prompt="project> ")
                    if not selected:
                        sys.exit(0)

                    if selected == "[dashboard]":
                        ensure_dashboard_session()
                        subprocess.run(["${tmux}", "switch-client", "-t", "prism-dashboard"])
                    elif selected == "[scratchpad]":
                        create_or_switch_session("[scratchpad]")
                    elif is_bare_repo(selected):
                        handle_bare_repo(selected)
                    elif is_regular_git_repo(selected):
                        handle_regular_repo(selected)
                    else:
                        # Plain directory (no git), open directly
                        create_or_switch_session(selected, None)

                if __name__ == "__main__":
                    main()
              '';
          };
        };
      };
}
