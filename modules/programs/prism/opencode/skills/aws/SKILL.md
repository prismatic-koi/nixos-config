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

Provide a ready-to-run command wrapped in `()` piped to the clipboard so the user can run it and paste back the result:

```bash
(aws <command> --profile <name>) | @clipboardCmd@
```

Example prompt to user:
> I need the output of the following command. Please run it and paste the result here:
> ```bash
> (aws sts get-caller-identity --profile production) | @clipboardCmd@
> ```

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
