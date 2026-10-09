---
name: prism-container
description: |
  How to run a container from a prism session with `prism container`. Load
  this skill before you run a container, and when a `prism container`
  command is refused, times out, or gives output that you do not expect.
  `prism container` is the way to run a container from a prism sandbox, in
  place of `podman` and `docker`. The skill covers the command surface, the
  fixed options, the limits, ownership, cleanup, and the audit log.
---

# Run containers with `prism container`

`prism container` is the way to run a container from a prism sandbox. Use
it in place of `podman` and `docker`. In a host shell outside prism, use
podman, not docker.

The agent does not get a container API socket. The agent gives prism a
small set of inputs. Prism builds the `podman` command on the host and sets
every other option itself. The agent cannot add an option, remove an
option, or change an option.

The command works for every session role and in every isolation mode:
`bwrap`, `sandbox-exec`, and `host`. It needs no spawn flag.

## Command surface

The only subcommand is `run`. It runs one command in a new container,
returns the output and the exit code, and removes the container.

```bash
prism container run [flags] IMAGE [-- COMMAND [ARG...]]

# Examples
prism container run alpine -- echo hello
prism container run --mount rw golang:1.23 -- go test ./...
prism container run --env CGO_ENABLED=0 --timeout 30m golang:1.23 -- go build ./...
prism container run --mount none alpine -- sh -c 'apk add --no-cache curl && curl -sI https://example.com'
```

| Flag | Default | Meaning |
|---|---|---|
| `--mount none\|ro\|rw` | `ro` | How the session worktree appears at `/workspace`. See "The worktree mount". |
| `--env KEY=VALUE` | none | Sets one environment variable in the container. Repeat the flag for more variables. The key must match `^[A-Za-z_][A-Za-z0-9_]*$`, and the `=` is mandatory. |
| `--timeout DURATION` | `10m` | Prism stops and removes the container after this time. The maximum is `60m`. The time includes an image pull. |

`prism agent-context` lists the same flags in machine-readable form.

### Rules for the arguments

- Put all flags before the image. Prism reads every argument after the
  image as part of the command.
- Put `--` between the image and the command. Without `--`, prism refuses
  the request.
- Prism gives the command to the container as an argument vector. No shell
  parses it, so `-- echo '$(id)'` prints `$(id)`. To use shell syntax, run
  a shell in the container: `-- sh -c 'echo $HOME'`.
- A short image name refers to Docker Hub. `alpine` means
  `docker.io/library/alpine`, and `bitnami/redis` means
  `docker.io/bitnami/redis`. For a different registry, give the full
  reference, for example `quay.io/org/image:tag`.
- Prism refuses an image reference that starts with `-`. Prism also refuses
  a reference that names an image transport, for example `oci-archive:` or
  `docker-archive:`. Transports can read files on the host.

### Output and exit codes

- The container stdout goes to the stdout of `prism container run`. The
  container stderr goes to its stderr.
- The output arrives when the container stops. It does not stream.
- If one output stream is more than 1 MiB, prism keeps the last 1 MiB of
  it. Prism then writes a line to stderr that says the stream was
  truncated.

| Exit code | Meaning |
|---|---|
| The exit code of the command | The container command ran to completion. |
| `124` | The `--timeout` expired. Prism stopped and removed the container. |
| `125` | Prism refused the request, the image pull failed, or podman failed. The last line on stderr gives the reason. |

## The worktree mount

| `--mount` | Result |
|---|---|
| `ro` | The worktree is read-only at `/workspace`. The working directory is `/workspace`. A write under `/workspace` fails. |
| `rw` | The worktree is read-write at `/workspace`. The working directory is `/workspace`. A file that the container writes appears in the worktree. |
| `none` | The container has no `/workspace`. The working directory is the default of the image. |

The mount source is always the worktree of the calling session. Prism
resolves the worktree on the host. No input names a host path.

Podman follows a symlink in any part of the mount source. Thus prism
refuses the request when the worktree path goes through a symlink. Prism
checks the path when the request arrives and again just before podman
starts. On macOS, the sandbox-exec profile also stops the agent from
renaming, removing, or replacing the worktree directory or any parent
directory of it.

With `--mount rw`, a container that runs as root writes files that the host
user owns. You can then edit or delete those files from the sandbox. Commit
only the files that you intend to change.

The container cannot see the bare repository. Thus `git` in the container
does not work on `/workspace`. Run `git` in the sandbox.

## Options that prism always sets

| Option | Value | Reason |
|---|---|---|
| `--rm` | | Podman removes the container when it exits. |
| `--memory` | `4g` | Memory limit. |
| `--memory-swap` | `4g` | Equal to the memory limit, so the container gets no extra swap. |
| `--cpus` | `2` | CPU limit. |
| `--pids-limit` | `1024` | Process limit. |
| `--security-opt` | `no-new-privileges` | A process cannot get more privileges than its parent. |
| `--label` | `prism.instance-id=<instance ID>` | The owner of the container. Prism finds and counts containers by this label. |
| `--name` | `prism-<instance token>-<folded session name>-<8 hex>` | The session prefix, for a human who reads `podman ps`. |
| `--timeout` | The request timeout plus 60 seconds | Podman stops the container even if prism stops first. |
| `--pull` | `never` | Prism pulls a missing image before the run, with `podman pull`. |
| `--cidfile` | A file in the prism-container state dir on the host | Prism holds the host lock until podman writes this file. No sandbox can write the directory. |
| `--volume`, `--workdir` | The worktree at `/workspace` | Only with `--mount ro` or `--mount rw`. |

The argument vector never holds `--privileged`, `--cap-add`, `--device`, a
host namespace option, or a host path mount other than the worktree. No
input can add one.

The container uses the default rootless network of podman. It can open
outbound connections. It cannot publish a port.

On macOS, the host `podman` CLI talks to the podman machine. The size of
the machine VM is a second limit on memory and CPU.

## Limits

- **Per session.** One session runs at most one container at a time. Prism
  refuses a second request from the same session at once. The error names
  the per-session limit. Wait until the first container stops.
- **Host-wide.** At most N prism containers run at the same time across all
  sessions. The Nix option `nx.programs.prism.containerHostLimit` sets N.
  The default is 4. Prism refuses a request over the limit at once. The
  error names the limit and the Nix option.
- Prism does not queue a refused request. Try again later.
- A container that has exited does not count against a limit.
- A host-side lock covers the limit check and the container create. Two
  sessions cannot both take the last free place.

## Ownership and cleanup

- Prism finds the containers of a session by the `prism.instance-id` label.
  Prism never decides ownership by the container name.
- `prism container run` removes its container before it returns. This also
  applies after a timeout and after a cancelled request.
- `prism cleanup` removes every container that carries the label of any
  incarnation of the session. That is the current incarnation and each
  earlier incarnation that the database holds. Cleanup issues podman
  commands only when one of these incarnations has an audit directory.
- Cleanup of a parent session also removes the containers of its review
  agents.
- If prism stops before it removes a container, the podman `--timeout`
  stops the container and `--rm` removes it.
- Images that prism pulls stay in the shared image store of the host. Prism
  does not remove them.

## Audit log

Each request writes one JSON line to an audit log on the host:

```text
$XDG_STATE_HOME/prism/prism-container/audit/<instance ID>/audit.log
```

The line holds these fields:

- `time`, `session`, `instance_id`, and `command` (`run`).
- `image`, with the Docker Hub prefix added to a short name.
- `args` (the container command) and `env_keys`. The log never holds an
  `--env` value, because a value can be a secret.
- `mount`, `timeout_seconds`, and `container` (the container name).
- `decision`: `allowed`, `refused`, or `error`. `reason` gives the cause.
- `exit_code`: the exit code, or `null` when no command ran to completion.

No sandbox can write the audit directory. bwrap does not bind it, and the
sandbox-exec profile grants no write access in it.

`prism cleanup` keeps every line in the session archive:

1. Cleanup of a parent session appends the log of each review agent to the
   log of the parent. A review agent has no archive of its own.
2. Cleanup appends the log of each earlier incarnation of the session to
   the log of the current incarnation.
3. Cleanup copies the log into the session archive as
   `prism-container-audit.log`, then removes the audit directories.

If an append fails, cleanup keeps the source directory on disk and writes
a warning that names it.

The prism-container state tree also holds the host-wide lock file and the
per-run cidfile directories. Like the audit directory, no sandbox can
write them.

## Errors

| Error text | What to do |
|---|---|
| `refused: this session already has a running container` | Wait until your other container stops, then try again. |
| `refused: N prism containers are running on this host` | Try again later. If the limit is too low, tell the user about `nx.programs.prism.containerHostLimit`. |
| `podman is not reachable` | Tell the user. On macOS, the user runs `podman machine start`. On Linux, the user runs `systemctl --user status podman.socket`. You cannot correct this from the sandbox. |
| `the --timeout of ... expired` | Increase `--timeout` (maximum `60m`), or make the command faster. |
| `the podman pull of image ... failed` | Read the podman error above this line. The image name or the tag is wrong, or the registry is not reachable. |
| `put -- between the image and the command` | Add `--` after the image. |
| `refused: --env ...` | Use `KEY=VALUE` with a valid key. |

## How it works

- In a `bwrap` or `sandbox-exec` session, the `prism` CLI sends the request
  to `POST /container/run` on the sidecar of the session. The sidecar runs
  on the host. It takes the session name, the instance ID, and the worktree
  from its own configuration, never from the request.
- In a `host` session, the CLI runs the same code in its own process. It
  reads the instance ID and the worktree of the session from the prism
  database.
- Both routes call one function that builds the podman argument vector.
  Thus the two routes cannot differ.
- The source is in `modules/programs/prism/prism/internal/prismcontainer/`.

## The podman proxy

`prism spawn --containers` and the `podman-proxy` skill describe an older
mechanism: a filtered podman API socket. Do not spawn new sessions with
`--containers`. Use `prism container`.
