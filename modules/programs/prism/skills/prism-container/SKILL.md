---
name: prism-container
description: |
  How to run a container or build an image from a prism session with
  `prism container`. Load this skill before you run a container or build an
  image, and when a `prism container` command is refused, times out, or
  gives output that you do not expect. `prism container` is the way to run
  a container or build an image from a prism sandbox, in place of `podman`
  and `docker`. The skill covers the command surface, the fixed options,
  the limits, ownership, cleanup, and the audit log.
---

# Run containers and build images with `prism container`

`prism container` is the way to run a container or build an image from a
prism sandbox. Use it in place of `podman` and `docker`. In a host shell
outside prism, use podman, not docker.

The agent does not get a container API socket. The agent gives prism a
small set of inputs. Prism builds the `podman` command on the host and sets
every other option itself. The agent cannot add an option, remove an
option, or change an option.

The command works for every session role and in every isolation mode:
`bwrap`, `sandbox-exec`, and `host`. It needs no spawn flag.

## Command surface

| Subcommand | Purpose |
|---|---|
| `run` | Run one command in a new container, return the output and the exit code, and remove the container. |
| `build` | Build an image from a Containerfile in the worktree, and print the image name. See "Build an image". |

### `prism container run`

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

## Build an image

`prism container build` builds an image from a Containerfile in the
worktree. On success, it prints the image name on stdout. Give that name to
`prism container run`.

```bash
prism container build [flags] [CONTEXT]

# Examples
prism container build
IMAGE=$(prism container build --tag myapp:v1)
prism container run "$IMAGE" -- myapp --version
prism container build --file docker/test.containerfile --build-arg GO_VERSION=1.23 docker
```

| Input | Default | Meaning |
|---|---|---|
| `CONTEXT` | the worktree root | The build context directory, relative to the worktree. |
| `--file PATH`, `-f PATH` | `Containerfile`, then `Dockerfile`, in `CONTEXT` | The Containerfile, relative to the worktree (not relative to `CONTEXT`). |
| `--build-arg KEY=VALUE` | none | Sets one build argument. Repeat the flag for more arguments. The key must match `^[A-Za-z_][A-Za-z0-9_]*$`, and the `=` is mandatory. |
| `--tag NAME[:TAG]`, `-t` | a generated name | The image name after the session prefix. See "The image name". |
| `--timeout DURATION` | `30m` | Prism stops the build after this time. The maximum is `60m`. The time includes the copy of the context. |

`prism agent-context` lists the same flags in machine-readable form.

### Paths

- Give `CONTEXT` and `--file` relative to the worktree. Prism refuses an
  absolute path, and a path that goes out of the worktree with `..`.
- Prism follows every symlink in `CONTEXT` and in `--file`. If the result
  is outside the worktree, prism refuses the request before podman runs.

### The context copy

Prism copies the Containerfile and the context into a directory on the
host that no sandbox can write. Podman builds from this copy, not from the
worktree. Thus a change in the worktree during the build does not change
the build.

- The copy keeps regular files, directories, and symlinks. It keeps the
  permission bits and the modification time of each file. It does not keep
  the setuid, setgid, and sticky bits, or the owner.
- Prism does not copy a symlink that resolves outside `CONTEXT`. This
  includes a symlink to an absolute path. The target of such a symlink
  never gets into the image. The build output names each symlink that
  prism did not copy.
- Prism does not copy a FIFO, a socket, or a device file. The build output
  names each one.
- The copy reads `CONTEXT/.containerignore`, or else
  `CONTEXT/.dockerignore`. It leaves out the paths that a literal pattern
  names (for example `target` or `**/node_modules`). Podman applies the
  full file to the copy again, so the result is the same as a podman build
  of the worktree. If the file has a `!` exception, the copy leaves out
  nothing. If `<Containerfile>.containerignore` or
  `<Containerfile>.dockerignore` exists, prism copies it next to the
  Containerfile and the copy leaves out nothing.
- Prism refuses a context with more than 4 GiB of file data. To make it
  smaller, give a smaller `CONTEXT`, or list large directories in
  `.containerignore`.
- If the worktree changes while prism copies it, prism refuses the request.
  Try again.

### The image name

The image name is `localhost/prism-<instance token>-<folded session
name>-<NAME>`. `NAME` is the `--tag` value, or 8 random hex characters when
you give no `--tag`. In the folded session name, every character that is
not a lower-case letter or a digit becomes `-`.

- `NAME` is a lower-case name of at most 64 characters: letters, digits,
  and the separators `.`, `_`, `__`, and `-`. It starts and ends with a
  letter or a digit.
- An optional `:TAG` follows: at most 128 letters, digits, `_`, `.`, and
  `-`, and the first character is not `.` or `-`.
- Prism refuses any other `--tag` value before podman runs. A `/`, a
  digest, and an upper-case letter in `NAME` are refused.
- A build with the same `--tag` as an earlier build of the session moves
  the name to the new image.
- The `localhost/` domain is part of the name. `prism container run` uses
  the image from the local store and does not pull it.

### Output and exit codes

- On success, stdout holds the image name only, so that `$(...)` reads it.
  The build output goes to stderr.
- The output arrives when the build stops. It does not stream. If the
  output is more than 1 MiB, prism keeps the last 1 MiB.

| Exit code | Meaning |
|---|---|
| `0` | The build succeeded. |
| The exit code of `podman build` | The build failed. The build output on stderr gives the cause. |
| `124` | The `--timeout` expired. Prism stopped the build. |
| `125` | Prism refused the request, or podman failed. The last line on stderr gives the reason. |

### The image sources of a build

A build reads images for `FROM`, for `COPY --from` and `ADD --from`, and
for `RUN --mount=from=`. Podman reads a source that starts with a
transport name and `:` from that transport, and several transports read a
path on the host. For example, `tarball:` takes any tar archive as a
layer, and `atomic:` reads the kubeconfig of the host. Thus, before podman
runs, prism reads the copy of the Containerfile and checks each image
source:

- Write each image source literally. It can hold only letters, digits,
  `.`, `_`, `-`, `/`, `:`, and `@`, with an optional `docker://` at the
  start. Prism refuses a `$`, a quote, or a backslash there, because
  podman expands `ARG` and `ENV` values, quotes, and escapes before it
  reads the source. For example, prism refuses `FROM golang:${GO_VERSION}`.
  Write `FROM golang:1.23`.
- Prism refuses a source that names one of these transports: `atomic`,
  `containers-storage`, `dir`, `docker-archive`, `docker-daemon`, `oci`,
  `oci-archive`, `ostree`, `sif`, and `tarball`.
- A registry image (`alpine`, `docker://alpine`, `quay.io/org/img:1`), a
  local image (`localhost/prism-...`), a stage name (`--from=build`), and a
  stage index (`--from=0`) pass.
- Prism refuses a `--mount` value that holds a `$`, a quote, or a
  backslash, and a flag name that is not literal (for example
  `--fr"om"=`). Other flags can hold variables, for example
  `COPY --chown=${UID}:${UID}`.
- Write every flag (a word that starts with `--` before the arguments of
  an instruction) in ASCII. Prism refuses a flag with a character that is
  not ASCII. The Dockerfile parser reads flags byte by byte, and some
  bytes inside a UTF-8 character count as spaces there. Text that is not
  ASCII in other places, for example in a `RUN` command, passes.
- `--platform=$BUILDPLATFORM` on `FROM` is not an image source, so prism
  does not check it.

Prism also refuses these:

- A parser directive `# escape=` with a character other than `\`.
- A `--file` whose name ends in `.in`. Podman runs such a file through the
  C preprocessor on the host, and an `#include` then reads a host file.
  Prism always gives podman the copy under the name `Containerfile`.

The check reads more than podman does. Thus it can refuse a line that
podman does not read as an image source. For example, a heredoc line that
starts with `FROM` or `from` must have a literal first word:
`from typing import List` passes, and `FROM $BASE` in a heredoc is
refused. The refusal names the line.

### What the Containerfile can do

The Containerfile is yours, so prism does not limit its other
instructions. These facts apply:

- Each build step (`RUN`) has the memory, swap, CPU, and process limits of
  "Options that prism always sets".
- `RUN --network=host` puts the step in the network namespace of the host.
  Both sandboxes share the host network, so this gives no access that the
  sandbox does not have.
- `RUN --mount=type=secret` and `RUN --mount=type=ssh` fail, because prism
  gives no secret and no SSH agent to the build.

### Platform note

The command, the flags, the checks, the output, and the exit codes are the
same on Linux and on macOS. One fact is different:

- On Linux, each build runs in a systemd user scope. At a timeout, and
  at `prism cleanup`, prism kills the scope. No process of the build
  continues. If prism stops during a build, systemd stops the scope when
  the timeout plus 60 seconds has passed.
  The build also has a signature policy that refuses every image source
  other than a registry and the local image store.
- On macOS, the build runs in the podman machine VM. If a build step runs
  when the timeout expires, the step can continue inside the VM until it
  ends. Its memory and CPU limits still apply, and the VM size is the
  upper limit. `podman machine stop` removes it. `prism cleanup` cannot
  stop it. If prism stops during a build, the build continues in the VM
  and no longer counts toward a limit.
- On macOS, the build has no signature policy. An `ONBUILD` instruction of
  a base image runs instructions that are not in your Containerfile, so
  the check cannot see them. Such an instruction can read a host path
  through a transport. The podman machine mounts the macOS home directory
  into the VM by default, so this can include macOS files.

Issue #3070 tracks the two macOS differences.

## Options that prism always sets

### For `run`

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

### For `build`

| Option | Value | Reason |
|---|---|---|
| `--memory` | `4g` | Memory limit of each build step. |
| `--memory-swap` | `4g` | Equal to the memory limit, so a build step gets no extra swap. |
| `--cpu-period`, `--cpu-quota` | `100000`, `200000` | A limit of 2 CPUs. `podman build` has no `--cpus`. |
| `--security-opt` | `no-new-privileges` | A process cannot get more privileges than its parent. The macOS podman client does not send this option for a build. |
| `--label` | `prism.instance-id=<instance ID>` | The owner of the image. Podman adds it as the last instruction, so a `LABEL` in the Containerfile cannot change it. |
| `--layer-label` | `prism.instance-id=<instance ID>` | The owner of each intermediate image. |
| `--tag` | `localhost/prism-...-<NAME>` | See "The image name". |
| `--file`, `CONTEXT` | The copy on the host | See "The context copy". |
| `--signature-policy` | A prism policy file on the host | Linux only. Refuses every image source other than a registry and the local store. See "The image sources of a build". |

The process limit depends on the platform, because `podman build` has no
`--pids-limit`:

- On Linux, the systemd scope of the build has `TasksMax=1024`. This is a
  limit of 1024 tasks for the whole build. Prism also gives podman
  `--cgroup-parent` in the scope, so that every build step is in the scope.
- On macOS, prism gives `--ulimit nproc=1024:1024`. This limit counts every
  process of one user in the rootless user namespace of the VM. Thus the
  processes of other prism containers, also of other sessions, count too.
  A build can make a fork fail in another container.

The argument vector never holds `--volume`, `--secret`, `--ssh`,
`--network`, `--device`, `--cap-add`, or `--build-context`. No input can
add one.

On macOS, the host `podman` CLI talks to the podman machine. The size of
the machine VM is a second limit on memory and CPU.

## Limits

A build counts as one container. Thus a `run` and a `build` of one
session cannot run at the same time.

- **Per session.** One session runs at most one container or build at a
  time. Prism refuses a second request from the same session at once. The
  error names the per-session limit. Wait until the first one stops.
- **Host-wide.** At most N prism containers and builds run at the same
  time across all sessions. The Nix option
  `nx.programs.prism.containerHostLimit` sets N. The default is 4. Prism
  refuses a request over the limit at once. The error names the limit and
  the Nix option.
- Prism does not queue a refused request. Try again later.
- A container that has exited does not count against a limit.
- A host-side lock covers the limit check and the container create. Two
  sessions cannot both take the last free place.
- Podman does not list a build container, so prism counts builds with a
  marker file. A build holds a lock on its marker while it runs. The
  marker of a build whose prism process stopped does not count.

## Ownership and cleanup

- Prism finds the containers and the built images of a session by the
  `prism.instance-id` label. Prism never decides ownership by the name.
- `prism container run` removes its container before it returns. This also
  applies after a timeout and after a cancelled request.
- `prism cleanup` does three steps for each incarnation of the session.
  That is the current incarnation and each earlier incarnation that the
  database holds.
  1. It stops every running build. On macOS it cannot stop a build step
     in the VM (see "Platform note").
  2. It removes every container that carries the label.
  3. It force-removes every image, intermediate images included, that
     carries the label. This also removes a container of any session that
     uses the image. Do not run an image that another session built.
- Cleanup issues podman commands only when one of these incarnations has
  an audit directory.
- Cleanup of a parent session does the same three steps for each of its
  review agents: it stops their builds, and it removes their containers
  and their images.
- If prism stops before it removes a container, the podman `--timeout`
  stops the container and `--rm` removes it.
- Images that prism pulls stay in the shared image store of the host. Prism
  does not remove them. The base images that a build pulls stay too.

## Audit log

Each request writes one JSON line to an audit log on the host:

```text
$XDG_STATE_HOME/prism/prism-container/audit/<instance ID>/audit.log
```

The line holds these fields:

- `time`, `session`, `instance_id`, and `command` (`run` or `build`).
- `image`: for `run`, the image with the Docker Hub prefix added to a
  short name. For `build`, the name of the image that the build makes.
- For `run`: `args` (the container command), `env_keys`, `mount`, and
  `container` (the container name). The log never holds an `--env` value,
  because a value can be a secret.
- For `build`: `context`, `file`, and `build_arg_keys`. The log never holds
  a `--build-arg` value.
- `timeout_seconds`.
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

The prism-container state tree also holds the host-wide lock file, the
per-run cidfile directories, the per-build context copies, and the build
markers. Like the audit directory, no sandbox can write them.

## Errors

| Error text | What to do |
|---|---|
| `refused: this session already has a running container or build` | Wait until your other container or build stops, then try again. |
| `refused: N prism containers and builds are running on this host` | Try again later. If the limit is too low, tell the user about `nx.programs.prism.containerHostLimit`. |
| `podman is not reachable` | Tell the user. On macOS, the user runs `podman machine start`. On Linux, the user runs `systemctl --user status podman.socket`. You cannot correct this from the sandbox. |
| `the --timeout of ... expired` | Increase `--timeout` (maximum `60m`), or make the command faster. |
| `the podman pull of image ... failed` | Read the podman error above this line. The image name or the tag is wrong, or the registry is not reachable. |
| `put -- between the image and the command` | Add `--` after the image. |
| `refused: --env ...` | Use `KEY=VALUE` with a valid key. |
| `the --timeout of ... expired, so prism stopped the build` | Increase `--timeout` (maximum `60m`), or make the build faster. |
| `the build failed: podman build exited N` | Read the build output above this line. |
| `refused: CONTEXT ...` or `refused: --file ...` | Give a path in the worktree that exists. A symlink must resolve inside the worktree. |
| `refused: --tag ... is not a valid image tag` | Use a lower-case `NAME` or `NAME:TAG`. See "The image name". |
| `refused: the build context holds more than 4 GiB` | Give a smaller `CONTEXT`, or list large directories in `.containerignore`. |
| `refused: the build context changed while prism copied it` | Try again when nothing writes to the context. |
| `refused: the Containerfile cannot be built: line N: ...` | Read the reason. Write each image source literally: a registry image, a local image, or a build stage, with no variable, quote, or backslash. See "The image sources of a build". |
| `The signature policy of the build refused an image source` | A base image has an `ONBUILD` instruction that names a transport. Use a different base image. |

## How it works

- In a `bwrap` or `sandbox-exec` session, the `prism` CLI sends the request
  to `POST /container/run` or `POST /container/build` on the sidecar of the
  session. The sidecar runs on the host. It takes the session name, the
  instance ID, and the worktree from its own configuration, never from the
  request.
- In a `host` session, the CLI runs the same code in its own process. It
  reads the instance ID and the worktree of the session from the prism
  database.
- Both routes call one function that builds the podman argument vector.
  Thus the two routes cannot differ.
- For `build`, one internal part, the build executor, differs between
  Linux and macOS. It adds options to the argument vector of the shared
  builder, and changes nothing else:
  - Linux: `--signature-policy` after `build`. The scope script then adds
    the global option `--cgroup-manager=cgroupfs` before `build`, and
    `--cgroup-parent <scope>/build` after `build`. The scope itself sets
    `TasksMax`, `Delegate`, and `RuntimeMaxSec`.
  - macOS: `--ulimit nproc=1024:1024` after `build`.
- The source is in `modules/programs/prism/prism/internal/prismcontainer/`.

## The podman proxy

`prism spawn --containers` and the `podman-proxy` skill describe an older
mechanism: a filtered podman API socket. Do not spawn new sessions with
`--containers`. Use `prism container`.
