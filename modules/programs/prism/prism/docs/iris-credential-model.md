# Iris credential model

**Status:** Implemented in D-7 (#1638) on top of the D-5 (#1636) per-tool bash
sandbox. Companion to the design doc — see [§7 of the daemon-mode design
doc](./daemon-mode-design.md#7-per-tool-credential-brokering) for the threat
model context.

## 1. What this document is

This is the operator-facing reference for which credentials reach which tool
subprocesses in iris daemon mode. It answers four questions:

1. **The matrix** — for each tool, what credentials are injected and what is
   omitted.
2. **The source** — for each credential, where the value comes from on the
   host and how it is resolved.
3. **The audit surface** — how to inspect after the fact what each tool call
   actually had access to.
4. **The fallback rules** — what happens when a credential is unresolvable
   (no host token, missing AWS files, unknown role, etc.).

If you only want to look something up, jump to:

- [§2 Credential matrix](#2-credential-matrix-per-tool)
- [§6 Audit log: `iris stats credentials`](#6-audit-log)

If you are modifying iris and need to change credential plumbing, read all
six sections plus design-doc §7.

## 2. Credential matrix (per tool)

| Tool | Credentials injected into subprocess | Mounts available |
|---|---|---|
| `read`, `edit`, `write`, `grep`, `find`, `ls` | **None.** | Worktree (RO or RW depending on tool), per-session `/tmp`, standard system read-only roots. |
| `bash` | `GITHUB_TOKEN` (role-scoped, with fallback — see §3.1). `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` always pointed at in-sandbox paths so the AWS CLI's file-based credential chain works (§3.2). | Worktree (RW), per-session `/tmp`, system roots, `~/.cache/nix` (RW), `/nix/var/nix/daemon-socket` (RW), synthesised `~/.gitconfig` (RO), `~/.ssh/{access-key,signing-key,signing-key.pub,allowed_signers,known_hosts}` (RO, conditional), synthesised `~/.ssh/config` (RO), `~/.aws/{config,credentials,sso,cli}` (mixed RO/RW, conditional), `~/.kube/config` (RO, conditional). |

**Tools / extensions not in this matrix:** MCP-bridge tools (Atlassian etc.)
authenticate via OAuth tokens that the extension manages — iris does not
mediate them. See design-doc §5 for the extension authentication model.

## 3. Credentials in detail

### 3.1 `GITHUB_TOKEN`

The bash subprocess receives a role-scoped GitHub Personal Access Token (PAT)
chosen by the **4-PAT architecture** (account × role → token). The selection
key is `PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>`, where:

- `<ACCOUNT>` is derived from the bare repo's `origin` remote URL (e.g.
  `prismatic-koi`, `thankyou-payroll`), upper-cased with `-` → `_`.
- `<ROLE>` is the session's `agent_role` (e.g. `WORKER`, `COORDINATOR`).

Concretely the broker looks for env vars of the form:

```
PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR
PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER
PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR
PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER
```

**Resolution algorithm:**

1. If the session has a bare repo root and the role is `worker` or
   `coordinator`, look up `PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>`. If set and
   non-empty → use it. Audit name: `GITHUB_TOKEN`.
2. Otherwise fall back to host `GITHUB_TOKEN`. If set and non-empty → use
   it. Audit name: `GITHUB_TOKEN(fallback:host)`.
3. Otherwise neither is set → no `GITHUB_TOKEN` is injected into the bash
   subprocess. The audit log records the absence (no `GITHUB_TOKEN*` entry
   in `credentials_injected`). This is **not an error**; some sessions
   legitimately have no GitHub access.

**Edge cases:**

- **Empty / unknown role.** Treated as the no-role case: the broker skips
  the role-scoped lookup and goes straight to the host fallback (step 2).
  Audit name on a hit is `GITHUB_TOKEN(fallback:host)`. No crash.
- **Empty `BareRoot`.** Same as empty role — the broker cannot derive an
  account, so it skips step 1.
- **Role-scoped var set to empty string.** Treated the same as unset.

**Raw `PRISM_GITHUB_TOKEN_*` env vars are NEVER propagated to the subprocess
verbatim.** Only the resolved `GITHUB_TOKEN` reaches the bash subprocess.

### 3.2 AWS credentials

AWS credentials are **file-mount based, not env-based.** The bash sandbox
mounts the host's AWS config/credentials files (RO) at the canonical
in-sandbox paths and points the AWS CLI at them via:

```
AWS_CONFIG_FILE=~/.aws/config
AWS_SHARED_CREDENTIALS_FILE=~/.aws/credentials
```

**Resolution algorithm:**

1. The broker always sets `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE`
   in the env — these are harmless when the underlying files do not exist
   (the AWS CLI will fall through to other auth sources).
2. The audit record reflects host file presence. If any of the source paths
   (`~/.config/aws/readonly-config`, `~/.config/aws/credentials`,
   `~/.aws/config`, `~/.aws/credentials`) exists, the audit log records
   `AWS_*` in `credentials_injected`. Otherwise the marker is omitted.

**Edge case: no AWS files anywhere.** The subprocess starts with the env
vars set but no underlying files; AWS CLI calls will fail with "unable to
locate credentials" (the normal AWS error). No iris error is raised, and
the audit log clearly shows `AWS_*` was not in scope.

### 3.3 Non-credential env vars (always present in bash)

The broker also forwards the following non-credential plumbing so bash
subprocesses behave like a normal shell:

```
PATH                    (host value, or fallback to nix profiles)
HOME, USER, LOGNAME     (host value if set)
LANG, LC_ALL, TERM      (host value if set)
GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME, GIT_COMMITTER_EMAIL
NIX_CONFIG=store = daemon
```

These are not credentials and never appear in `credentials_injected`.

## 4. What is NEVER injected

The broker is also defined by what it refuses to forward. None of the
following appear in any bash (or file-tool) subprocess env, regardless of
host state:

**LLM / provider API keys.** Bash subprocesses must not make LLM calls.

```
ANTHROPIC_API_KEY
OPENROUTER_API_KEY
OPENAI_API_KEY
GEMINI_API_KEY
GOOGLE_API_KEY
GITHUB_COPILOT_TOKEN
DEEPSEEK_API_KEY
```

The exhaustive list is exported as `iris.ForbiddenLLMKeyNames()` so tests
can stay in sync.

**Raw role-scoped tokens.** No `PRISM_GITHUB_TOKEN_*` env var ever appears
in the subprocess; only the resolved `GITHUB_TOKEN` does (§3.1).

**Pi-process credential paths.** The bash sandbox mount layout deliberately
omits:

```
~/.claude
~/.mcp-auth
~/.pi/agent/*
~/.cache/bun
~/.config/pi/*
```

These are pi's own credential surface (see design-doc §7.3). In daemon mode
pi runs unsandboxed and reads them directly; tool subprocesses must not.

## 5. The CredentialBroker type

The credential resolution and audit logic lives in
`internal/iris/credential_broker.go`:

```go
type CredentialBroker struct{}

func NewCredentialBroker() *CredentialBroker

type BashResolution struct {
    Env   []string  // KEY=VAL pairs to inject
    Names []string  // audit-only names (no values)
}

func (b *CredentialBroker) ResolveBash(role, bareRoot string) BashResolution
```

The broker is stateless and safe for concurrent use. Each tool call
constructs a broker (or reuses one), resolves credentials at dispatch time
(`harness_socket.go::dispatchToolExec`), and passes the env list into the
platform-specific bash sandbox builder.

**Why per-call, not per-session.** Resolving every call means a daemon
restart, a sops re-decrypt, or a `prism` config reload cannot leave stale
credentials behind — the source of truth is always the current host state.
The design-doc §7.2 calls this out explicitly: credentials live in the
host environment / file system, not in the daemon's memory.

## 6. Audit log

Every `tool_call` event written by the harness socket carries two D-7
fields in its JSON payload:

```json
{
  "id": "<tool exec id>",
  "name": "bash",
  "args": { ... },
  "credentials_injected": ["GITHUB_TOKEN", "AWS_*"],
  "agent_role": "worker"
}
```

`credentials_injected` is always an array — `[]` means the call ran with no
credentials in scope (the normal case for read/edit/write/grep/find/ls).

**Inspecting a session:**

```
iris stats credentials <session-name>
```

prints one line per tool call:

```
2026-05-16T14:22:31+13:00  tool=bash  role=worker  credentials_injected=[GITHUB_TOKEN(fallback:host),AWS_*]
2026-05-16T14:22:35+13:00  tool=read  role=worker  credentials_injected=[]
2026-05-16T14:22:36+13:00  tool=bash  role=worker  credentials_injected=[GITHUB_TOKEN]
```

Names are stable identifiers. The full set the broker emits is:

| Audit name | Meaning |
|---|---|
| `GITHUB_TOKEN` | Role-scoped `PRISM_GITHUB_TOKEN_<account>_<role>` was set and used. |
| `GITHUB_TOKEN(fallback:host)` | Role-scoped was unset; host `GITHUB_TOKEN` was used. |
| `AWS_*` | At least one of the AWS config/credentials source files was present on the host. |

A `tool_call` event with `credentials_injected: []` and `name: bash` means
the bash subprocess ran with no `GITHUB_TOKEN` and no AWS credentials — for
example, a host with neither configured. This is the legitimate no-credentials
state, not an error.

## 7. Fallback summary (decision table)

| Scenario | `GITHUB_TOKEN` in env | Audit `credentials_injected` |
|---|---|---|
| Role-scoped var set | resolved value | `["GITHUB_TOKEN", ...]` |
| Role-scoped unset, host `GITHUB_TOKEN` set | host value | `["GITHUB_TOKEN(fallback:host)", ...]` |
| Role empty / unknown, host `GITHUB_TOKEN` set | host value | `["GITHUB_TOKEN(fallback:host)", ...]` |
| Neither set | not present | `[...]` (GitHub omitted; other creds may still appear) |
| AWS files present on host | env vars set | `[..., "AWS_*"]` |
| AWS files absent | env vars still set (harmless) | `[...]` (AWS omitted) |

No row produces an error: missing credentials are an expected operational
state, and the audit log makes them visible to the operator.

## 8. Where to look in the code

| Concern | File |
|---|---|
| Broker type and resolution logic | `internal/iris/credential_broker.go` |
| Bash subprocess env (thin shim) | `internal/iris/bash_credentials.go` |
| Per-call audit log write | `internal/iris/harness_socket.go::dispatchToolExec` |
| Linux mount layout | `internal/iris/bash_sandbox_linux.go::bashToolMountsLinux` |
| macOS SBPL profile | `internal/iris/bash_sandbox_darwin.go::GenerateBashSBPLProfile` |
| 4-PAT account derivation | `internal/container/credentials.go::GithubAccountFromBareRoot` |
| `iris stats credentials` CLI | `cmd/iris/stats.go` |
| Exhaustive negative tests | `internal/iris/credential_broker*_test.go` |

## 9. Lineage

- **D-5 (#1636)** introduced the bash sandbox and inline credential
  injection.
- **D-7 (#1638)** — this document — extracted the inline logic into the
  `CredentialBroker`, added the audit field, added the `iris stats
  credentials` subcommand, and pinned the negative tests as ACs.
- Future work: extend the broker to handle additional tools that need
  scoped credentials (e.g. a future `gh` tool with a finer-grained GitHub
  token); document any new audit names here.
