---
name: infrastructure-as-code
description: |
  Rules for authoring Infrastructure-as-Code (IaC). Load this skill when you
  write or edit Terraform / OpenTofu (`.tf`), CloudFormation (YAML/JSON),
  Pulumi, or CDK, or any string value that a cloud provider API will validate.
  Covers the ASCII-only rule for IaC string values and its exact exceptions.
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
