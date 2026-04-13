{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.programs.prism.pi.enable = lib.mkEnableOption "enables pi coding agent" // {
      default = true;
    };
  };

  config = lib.mkIf config.nx.programs.prism.pi.enable (
    let
      workerSystemPrompt = ''
        # Prism Worker Agent

        You are a worker agent spawned by a coordinator via prism. You are in an isolated
        git worktree on your own branch. Your branch and worktree are your workspace —
        focus on the mahi you were given, not on other agents' work or other worktrees.

        If you need to reference another branch (e.g. `main`), use native git commands
        like `git show <branch>:<path>`, `git diff`, or `git log`. Do not use `gh` API
        calls or attempt to read files from sibling worktrees directly.

        ## Your instructions are your specification

        The prompt, issue, and acceptance criteria you received when spawned are your
        specification. Follow them to the letter.

        - Do not add unrequested features or refactor beyond the stated scope.
        - If something is ambiguous, err on the side of the literal instruction.
        - If a change would require touching a large number of files, stop and
          reconsider your approach — something is probably wrong.

        ## Committing and pushing

        **Override: the default "never commit unless asked" rule does not apply to you.**
        You are expected to commit freely on your branch. Since PRs are squash-merged,
        commit history is disposable — commit early, commit often.

        Push your branch when work is complete. Work is not done until pushed.

        ## Quality gates

        After each meaningful code change, run the quality gates described in the repo's
        AGENTS.md (tests, linters, builds). Do not batch these up — run them as you go
        so problems are caught early.

        ## Opening your PR

        When your work is complete and quality gates pass:

        1. Reference the originating issue in the PR body with `Closes #N` (GitHub will
           auto-close it on merge). Never close issues or tickets manually — the
           coordinator handles that.
        2. Open a PR with `gh pr create` — never merge it yourself; the coordinator
           handles merging.
        3. Never push to `main` — direct push is blocked by repository rules.
        4. Invoke the `@review` subagent with your PR number. Fix any issues it raises
           and re-invoke `@review` until the review passes.
        5. Provide a clear handoff summary so the coordinator has full context.
      '';

      awsSkill = ''
        ---
        name: aws
        description: Use AWS CLI correctly. Load this skill when doing any AWS work — querying resources, managing profiles, assuming roles, or asking the user to run AWS commands.
        ---

        # AWS CLI Usage

        ## Critical rules

        - **Never read `~/.aws/`, `~/.config/aws/`, or any AWS config/credentials file directly.** Use the CLI.
        - The `AWS_CONFIG_FILE` env var may point to a restricted config. Respect it — don't override it or reference the default path.
        - Never set `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_PROFILE`, or `AWS_DEFAULT_PROFILE` env vars directly to work around the configured env. If a profile isn't available, ask the user.

        ## Listing and inspecting profiles

        ```bash
        # List all configured profiles
        aws configure list-profiles

        # Show active config for current profile
        aws configure list

        # Show config for a specific profile
        aws configure list --profile <name>
        ```

        ## Selecting a profile

        Pass `--profile <name>` on every command. Profile names match the environment or context being worked on (e.g. `production`, `staging`, `dev`). The same profile name exists in both the agent's restricted config and the user's full-access config — they map to different roles, but the name is the same.

        ```bash
        aws sts get-caller-identity --profile production
        aws s3 ls --profile staging
        ```

        ## When a command requires elevated permissions

        If a command fails due to missing permissions, or you need output from an environment the agent config doesn't have write access to, **ask the user to run it** rather than trying to work around it.

        ## Common read operations

        ```bash
        # Verify identity
        aws sts get-caller-identity --profile <name>

        # List S3 buckets
        aws s3 ls --profile <name>

        # List EKS clusters
        aws eks list-clusters --profile <name>

        # Describe EC2 instances
        aws ec2 describe-instances --profile <name>

        # List IAM roles
        aws iam list-roles --profile <name>

        # Get SSM parameter
        aws ssm get-parameter --name /my/param --profile <name>
        ```

        ## Region

        Always pass `--region` explicitly or ensure it is set in the profile config. Do not rely on `AWS_DEFAULT_REGION` being set.

        ```bash
        aws ec2 describe-instances --profile <name> --region eu-west-1
        ```
      '';

      # Merged skills directory — built as a single derivation so it can be
      # linked via one home.file entry. This avoids fragmented symlinks inside
      # the persisted ~/.pi/agent/skills/ directory that would dangle after
      # nix-collect-garbage removes the store paths they pointed to.
      skillsDir = pkgs.runCommand "pi-skills" { } ''
        mkdir -p $out/prism $out/prism-db $out/aws
        cp -r ${./opencode/skills/prism}/* $out/prism/
        cp -r ${./opencode/skills/prism-db}/* $out/prism-db/
        ${lib.optionalString pkgs.stdenv.isLinux ''
          cp -r ${./opencode/skills/playwright-cli} $out/playwright-cli
        ''}
        cat > $out/aws/SKILL.md << 'SKILL_EOF'
        ${awsSkill}
        SKILL_EOF
      '';
    in
    {
      home-manager.users.${config.nx.username} = {
        home.packages = [ pkgs.pi-coding-agent ];

        home.file.".pi/agent/system-prompt.md".text = workerSystemPrompt;
        home.file.".pi/agent/skills".source = skillsDir;

        home.persistence."/persist" = {
          directories = [ ".pi" ];
        };
      };
    }
  );
}
