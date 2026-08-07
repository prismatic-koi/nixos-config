---
name: infrastructure-as-code
description: |
  Rules for authoring Infrastructure-as-Code (IaC) and for delegating IaC work
  to another agent. Load this skill when you write, edit, or review Terraform /
  OpenTofu (`.tf`), CloudFormation (YAML/JSON), Pulumi, or CDK, when you write
  any string value that a cloud provider API will validate, or when you prepare
  a spawn prompt, acceptance criteria, or a review for work that touches IaC.
  Covers the ASCII-only rule, the shared OpenTofu CI workflow, and which
  commands an agent may and may not run.
---

# Infrastructure-as-Code authoring

## String values: ASCII only

When authoring string *values* in Infrastructure-as-Code that will be sent to a cloud provider API — Terraform/OpenTofu `.tf` files, CloudFormation YAML/JSON, Pulumi, CDK — stick to ASCII. Use hyphen-minus (`-`), straight quotes (`"` and `'`), and three literal dots (`...`) instead of smart punctuation. Cloud provider APIs frequently enforce regex validation on these fields and reject Unicode punctuation. AWS IAM description fields are the loudest offender: a single em-dash in a role description silently breaks every subsequent `tofu apply` in the affected repo until the character is removed. Em-dash (U+2014), en-dash (U+2013), curly single/double quotes (U+2018/U+2019/U+201C/U+201D), and ellipsis (U+2026) are all known offenders — treat the whole class as unsafe.

The rule targets IaC string *values* only. It does NOT apply to:

- Comments in `.tf` / `.tofu` / YAML files (UTF-8 is fine there).
- Markdown, PR descriptions, commit messages, ticket bodies — stay expressive.
- Nix files, application source code, documentation.
- Te reo Māori with macrons anywhere except IaC-string-value context — the Te Reo Māori guidance in the global instructions remains authoritative for prose.

If in doubt about whether a field is user-facing prose or an API payload, assume API payload and stick to ASCII.

## The shared OpenTofu workflow

The Thankyou Payroll IaC repos share one reusable workflow:

```
thankyou-payroll/github-actions/.github/workflows/opentofu.yml
```

It is `on: workflow_call`. Required inputs are `environment`, `working_directory`, and `aws_role_arn`.

To recognise a repo that uses it, look in `.github/workflows/ci.yml` for:

```yaml
uses: thankyou-payroll/github-actions/.github/workflows/opentofu.yml@<ref>
secrets: inherit
```

The ref is a commit SHA in most callers and `@main` in others. Do not use the pin style as part of the test.

Known callers, as a starting point only. The recognition rule above is authoritative, because this list goes stale:

| Repo | OpenTofu roots |
|---|---|
| `aws-landing-zone` | seven, one per account |
| `aws-identity` | seven, one per account |
| `aws-kubernetes` | staging and production, `infrastructure/` only |
| `aws-databases` | staging and production |
| `dns-management` | `route53/staging`, `route53/production` |
| `github-management` | one root |
| `kohi` | `.infra/staging` |
| `staging-db-refresh` | `.infra/shared`, dev-dump |

Callers use `dorny/paths-filter` to detect changed directories, then call the shared workflow once per environment. A change under `modules/**` counts as a change to every environment in that repo.

## Who runs plan and apply

CI does. You do not.

| Stage | Action | Trigger |
|---|---|---|
| plan | `dflook/tofu-plan` | `pull_request` against `main`. Posts the diff as a PR comment. No approval gate. |
| apply | `dflook/tofu-apply` | Push to `main` only, gated by a GitHub Environment. |

The plan exists after the pull request opens. There is no plan before that. If you need plan output, open the PR and read the comment.

## Why a local plan is impossible, not merely discouraged

Every root assumes a role in both the backend and the provider:

```hcl
assume_role = {
  role_arn = "arn:aws:iam::<account>:role/OpenTofuRole-<repo>"
}
```

CI reaches that role through OIDC: GitHub mints a token, assumes `GitHubActionsRole-<repo>` in the management account (`767189252487`), and chains into `OpenTofuRole-<repo>` in the target account. Only GitHub Actions can mint that token.

An agent session does hold live AWS credentials. They are the wrong ones:

```
$ aws sts get-caller-identity --profile production
arn:aws:sts::746956280090:assumed-role/AWSReservedSSO_platform-agent-readonly_.../...

$ aws sts assume-role --role-arn arn:aws:iam::746956280090:role/OpenTofuRole-identity --profile production
AccessDenied: not authorized to perform: sts:AssumeRole
```

The readonly SSO role is not in the trust policy of `OpenTofuRole-*`. So `tofu init` fails on the backend before a plan is ever computed.

Exporting credentials does not help. `aws configure export-credentials --profile staging` exports the same readonly session that was denied above. The block is a trust policy, not credential plumbing. Do not spend a cycle on it.

This overrides repo documentation. `aws-identity/README.md` has a section titled "Local usage" that documents `tofu init` and `tofu plan`, and `BOOTSTRAP.md` says to run import commands "locally with admin credentials". Both are correct for a human with an admin SSO session. Neither applies to an agent. If you find a repo doc that tells you to run a plan, this skill wins.

## Rules for coordinators

Two failures start upstream of the worker, so they belong here.

1. Never write "run `tofu plan`" into a spawn prompt, an acceptance criterion, or a review comment. The worker cannot do it. It will burn a cycle proving that, then either escalate or invent something. If you want plan evidence, write the AC against the artefact that exists: "the plan comment on the PR shows no change to `aws_iam_role.foo`", which the worker reads with `gh pr view --comments` once the PR is open.

2. Never re-run a failed apply job. See the next section. Retrying is not a recovery, and each retry costs a cycle and teaches nothing.

## A failed apply on main: do not re-run the job

`dflook/tofu-apply` does not consume a stored plan artefact. It recomputes the plan and verifies it against the plan recorded in the pull request comment. If the two differ, it refuses.

A failed apply has usually applied part of the change, so state has moved. The recomputed plan no longer matches the recorded plan comment, and the job fails again for that reason on every retry. That refusal is the safety property working: it stops an apply that nobody reviewed. It is not a flake.

Recovery: open a fresh pull request. It produces a new plan comment against current state, which apply can verify on merge.

The new PR must change a file under the affected environment's path filter. An empty commit changes nothing, so `dorny/paths-filter` reports no change and no plan job runs at all.

The alternative recovery is a human applying directly with admin credentials. That is a human decision, not something to assume. Escalate rather than plan around it.

## Local commands

Required before you commit any `.tf` change:

```bash
tofu fmt -recursive -check -diff .   # from the repo root: shows what would change
tofu fmt -recursive .                # writes the changes
```

CI runs `dflook/tofu-fmt-check` with `path: .`, which is recursive from the repo root. Plain `tofu fmt` is not recursive and covers only the current directory, so a check scoped to one environment directory can pass while CI fails. Skipping this step means the lint gate fails after the PR opens and costs an extra review round.

Optional, and creditless:

```bash
rm -rf .terraform
tofu init -backend=false -lockfile=readonly
tofu validate
```

`-backend=false` skips the S3 backend, so no AWS credentials are needed. `-lockfile=readonly` is not optional: without it `tofu init` rewrites `.terraform.lock.hcl` and adds platform hashes, which lands as an unrelated diff in your PR. A stale `.terraform/` directory makes `-backend=false` contact S3 anyway and fail, so delete it first.

`tofu validate` is deliberately not run in CI. The lint workflows record the omission: `tofu plan` covers everything validate does.

Never run: `tofu plan`, `tofu apply`, `tofu import`, `tofu destroy`, `tofu refresh`, `tofu state`, or `tofu init` without `-backend=false`.

If `git status` shows a modified `.terraform.lock.hcl` that you did not intend, revert it before committing.

## A repo that uses OpenTofu without the shared workflow

Read its workflows before you act. Do not carry the plan and apply mechanics above into it.

The local-command rules still hold, because they follow from the credentials the session has, not from the workflow. Ask the user what the plan and apply path is rather than inferring one.

## Layout gotchas

- Each `<environment>/<region>/` directory is an independent root with its own backend, provider, and lock file. Editing one plans that one only, unless the change is under `modules/**`.
- `aws-kubernetes` CI plans `<environment>/ap-southeast-2/infrastructure` only. The sibling `apps/`, `flux/`, `bootstrap/`, and `rbac/` directories are Flux GitOps, not OpenTofu. No plan comment will ever appear for a change to them.
- The execution role is `OpenTofuRole-<repo>`, one per account. Docs that name `TYP-TerraformExecutionRole` are stale, including some repo READMEs and the header comment of the shared workflow itself.
