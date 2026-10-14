---
name: prism-container
description: |
  How to run a container, build an image, or start a service container
  (for example a database) from a prism session with `prism container`.
  Load this skill before you run, build, or start a container, and when a
  `prism container` command is refused, times out, or gives output that
  you do not expect. `prism container` is the way to run containers from a
  prism sandbox, in place of `podman` and `docker`. The skill covers the
  command surface, the fixed options, the limits, ownership, cleanup, and
  the audit log.
---

# Run containers, build images, and start services with `prism container`

`prism container` is the way to run a container, build an image, or start
a service container from a prism sandbox. Use it in place of `podman` and
`docker`. In a host shell outside prism, use podman, not docker.

The agent does not get a container API socket. The agent gives prism a
small set of inputs. Prism builds the `podman` command on the host and sets
every other option itself. The agent cannot add an option, remove an
option, or change an option.

The sandbox also blocks a direct connection to the container runtime. On
macOS, a connect to a Unix domain socket is a `network-outbound` operation
that carries the socket path. The sandbox-exec profile denies
`network-outbound` to every Unix socket except an allowlist: the host-API
socket, DNS, the system log, the nix daemon, the session work dir, the
worktree, the bare repo, and the per-session TMPDIR. The podman machine API
socket sits under the per-user `$TMPDIR`, or under `/private/tmp/podman`
when `TMPDIR` is unset. A connect to it fails with a sandbox denial
(`EPERM`) in either location. A TCP connect carries an IP endpoint, not a
path, so this deny leaves normal network use untouched. The same deny stops every other host
socket in `/private/tmp` or in the per-user `$TMPDIR`: the tmux control
socket, the user ssh-agent, the AeroSpace window-manager socket, the .NET
diagnostic IPC socket, the nvim RPC socket, and any socket that the profile
does not name. On Linux, the bwrap sandbox binds no container socket, so the
same connection is absent. Use `prism container` for every container action
in place of a direct socket.

In a sandbox-exec session, `TMPDIR` is a per-session dir,
`/tmp/prism-<hash>`. It is the only temp dir that the session can write.
The session cannot create, rename, or delete a file in `/tmp`,
`/private/tmp`, or the per-user `$TMPDIR`, and cannot read the TMPDIR of
another session. Put temp files in `$TMPDIR`, not in `/tmp`. `prism
cleanup` removes the dir.

The command works for every session role and in every isolation mode:
`bwrap`, `sandbox-exec`, and `host`. It needs no spawn flag.

## Command surface

| Subcommand | Purpose |
|---|---|
| `run` | Run one command in a new container, return the output and the exit code, and remove the container. |
| `build` | Build an image from a Containerfile in the worktree, and print the image name. See "Build an image". |
| `start` | Start a service container that runs until you stop it, and print its handle and its published ports. See "Service containers". |
| `exec` | Run one command in a service container. |
| `logs` | Print the logs of a service container. |
| `stop` | Stop and remove a service container. |
| `list` | List the containers and the built images of this session. |

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
| `rw` | The worktree is read-write at `/workspace`. The working directory is `/workspace`. A file that the container writes appears in the worktree. Prism refuses `rw` for a session with a read-only worktree (a review agent or an investigator). |
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

### What a build can read

A build cannot read host files other than the build context. This is the
security boundary of `prism container build`:

- On Linux, `podman build` runs in its own mount namespace and pid
  namespace. The namespace holds only the podman binaries and config, the
  podman image store, the build copy, and the cgroup tree of the build.
  Your home directory, `/tmp`, and the other files of the host are not
  there. Prism makes the namespace with `podman unshare` and `bwrap`.
- On macOS, the build runs in the podman machine VM. Before each build,
  prism reads the mounts of the default machine. If the machine mounts a
  Mac path that is not in the allowlist, prism refuses the build. See
  "Platform note".

The Containerfile check below and the signature policy refuse most host
sources earlier, with a clear message. They are not the boundary.

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
  backslash. On `FROM`, `COPY`, `ADD`, and `RUN`, prism refuses a flag
  name that is not literal (for example `--fr"om"=` or `--from$X=`),
  because podman expands the flag before it reads the name. The value of
  other flags can hold variables, for example `COPY --chown=${UID}:${UID}`.
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
- An instruction that continues over more than 200 lines. Split it into
  smaller instructions.

Prism reads an instruction from every line of the Containerfile: the
line, joined with its continuation lines. Thus it can refuse a line that
podman does not read as an instruction: a line of shell or SQL text in a
`RUN` continuation or in a heredoc. Such a line is refused when it starts
with `FROM` and its next word is not literal, or when it starts with
`COPY`, `ADD`, or `RUN` and a flag that the rules above refuse. A line
that starts with another word, or with a flag, is not read as an
instruction. For example:

```dockerfile
# Refused: line 3 starts with FROM, and users" is not literal.
RUN psql -c "SELECT * \
FROM users"

# Passes: the line does not start with FROM.
RUN psql -c "SELECT * FROM users"
```

`from typing import List` in a heredoc passes, because `typing` is
literal. The refusal names the line. Restructure the text so that the line
does not start that way.

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
same on Linux and on macOS. These facts are different.

On Linux:

- Each build runs in a systemd user scope. At a timeout, and at
  `prism cleanup`, prism kills the scope. No process of the build
  continues. If prism stops during a build, systemd stops the scope when
  the timeout plus 60 seconds has passed.
- `podman build` runs in a mount namespace that holds only an allowlist of
  host paths (see "What a build can read"). A source that names a host
  path, also in an `ONBUILD` instruction of a base image, fails because
  the path does not exist there. The allowlist includes the podman image
  store, so a build can use every local image of the host user.
- The build also has a signature policy that refuses every image source
  other than a registry and the local image store.
- A `RUN --mount=type=cache` directory lasts for one build only.

On macOS:

- Before each build, prism reads the mounts of the default podman machine
  from its config file. Prism refuses the build when a mount source is
  not one of the allowlisted Mac paths or inside one. The allowlist is the
  prism project locations (for example `~/code`), unless the Nix option
  `nx.programs.prism.containerMachineMountAllowlist` sets it. Prism also
  refuses the build when it cannot read the mounts.
- This repository generates the `volumes` setting from the allowlist on
  Darwin. It writes `~/.config/containers/containers.conf`, and each
  entry maps a path to the same path in the machine.
- By default, a podman machine mounts `/Users`, `/private`, and
  `/var/folders`, so prism refuses every build until the user recreates
  the machine. Tell the user. The refusal names each mount that is not
  allowed. After a rebuild writes `volumes`, the steps are:
  `podman machine stop`, `podman machine rm`, `podman machine init`, and
  `podman machine start`. `podman machine rm` deletes the images and
  containers in the machine.
- Each build runs under an anchor container. Before `podman build`
  starts, prism starts a container of the pinned image
  `registry.k8s.io/pause` with the fixed limits and the label of the
  session. Prism gives `podman build` the option `--cgroup-parent` in the
  systemd scope of the anchor, so every build step runs in that scope.
- At a timeout, prism stops the `podman build` client first, so that the
  VM starts no further build step. Then prism removes the anchor. systemd
  stops the scope and kills every process of the build in the VM. No
  build step continues after a timeout.
- `prism cleanup` pauses the anchor. The prism process of the build then
  stops its client and removes the anchor. If that process does not
  remove the anchor in 15 seconds, cleanup removes it.
- If prism stops during a build, a guard stops the `podman build` client
  too. The anchor has a podman `--timeout` of the build timeout plus 60
  seconds. When it expires, the scope stops, and no process of the build
  continues. Until then, the anchor counts toward the per-session limit
  and the host-wide limit.
- Prism pulls the anchor image at the first build. It stays in the image
  store of the machine.
- The build has no signature policy, because the macOS podman client does
  not send `--signature-policy`. The Containerfile check refuses a
  transport source in your Containerfile. An `ONBUILD` instruction of a
  base image is not in your Containerfile, and prism cannot read it
  before the build, because `podman image inspect` does not show it.
  Such an instruction can name a transport source. A transport source
  can read an image archive, an image layout, or a tar archive
  (`tarball:`) in the VM or in an allowlisted Mac path, for example
  another repository in `~/code`.

## Service containers

A service container runs in the background, for example a database for
your tests. It has no timeout. It runs until `prism container stop` or
`prism cleanup` removes it.

```bash
prism container start [flags] IMAGE [-- COMMAND [ARG...]]
prism container exec [--timeout DURATION] HANDLE -- COMMAND [ARG...]
prism container logs HANDLE
prism container stop HANDLE
prism container list
```

### `prism container start`

| Flag | Default | Meaning |
|---|---|---|
| `--port N` | none | Publishes container port `N` (TCP) on `127.0.0.1` with a random host port. `N` must be an integer from 1 to 65535. Repeat the flag for more ports, up to 16. |
| `--mount none\|ro\|rw` | `ro` | The same as for `run`. See "The worktree mount". |
| `--env KEY=VALUE` | none | The same as for `run`. |

The image, the `--` rule, and the command are the same as for `run`.
`start` has no `--timeout`. Prism stops the image pull and the container
create after 10 minutes.

On success, `start` exits 0 while the container keeps running. stdout
holds one `handle` line, then one `port` line for each `--port`:

```text
handle 3f9a1c2e
port 5432 127.0.0.1:41873
```

The handle names the service container for `exec`, `logs`, and `stop`.
Connect to the host address of the `port` line from the sandbox. The host
port is different for each start, so always read it from the output.

### `prism container exec`

`exec` runs one command in a running service container, and gives its
stdout, its stderr, and its exit code, as `run` does. Put `--` between the
handle and the command. No shell parses the command. `exec` starts no
container, so no limit refuses it.

`--timeout` has the default `10m` and the maximum `60m`. At the timeout,
prism stops `podman exec` and exits 124. The command can continue in the
service container until the container stops.

### `prism container logs` and `prism container stop`

- `logs` prints the logs of the service container. The container stdout
  goes to stdout, and the container stderr goes to stderr. Prism keeps the
  last 1 MiB of each stream.
- `stop` stops and removes the service container, with its anonymous
  volumes (for example the `VOLUME` of `postgres:16`). Podman gives the
  service 10 seconds to stop, then kills it. After `stop`, the handle does
  not name a container.

### `prism container list`

`list` shows the containers of this session, also a container that has
exited, and the images that this session built:

```text
HANDLE    KIND     STATE    IMAGE                          PORTS
3f9a1c2e  service  running  docker.io/library/postgres:16  5432->127.0.0.1:41873

BUILT IMAGE
localhost/prism-1a2b3c4d-nixos-config-my-branch-app:latest
```

For a service container, HANDLE is its handle. For a container of `run`,
HANDLE is the container name, and KIND is `run`. For the anchor container
of a macOS build (see "Platform note"), HANDLE is the container name, and
KIND is `build`.

### Which containers `exec`, `logs`, and `stop` accept

You can give the handle, the container name, the container ID, or an ID
prefix of 12 or more characters. Prism refuses each of these:

- A container that does not carry the label of this session.
- A container that has no prism label.
- A container of this session that `start` did not make, for example a
  container of `run`.

Prism does not change the container that it refuses.

### Example: tests against a database

```bash
out=$(prism container start --mount none --env POSTGRES_PASSWORD=test --port 5432 postgres:16)
handle=$(echo "$out" | awk '$1 == "handle" {print $2}')
addr=$(echo "$out" | awk '$1 == "port" && $2 == 5432 {print $3}')

# Wait up to 60 seconds until postgres accepts connections on its TCP port.
for i in $(seq 60); do
  prism container exec "$handle" -- pg_isready -h 127.0.0.1 -U postgres && break
  sleep 1
done

DATABASE_URL="postgres://postgres:test@$addr/postgres" go test ./...

prism container stop "$handle"
```

The tests run in the sandbox. They connect to `127.0.0.1` and the host
port. Stop the container when the tests are done: while it runs, it holds
the one container place of the session, so a `run` or a `build` of the
session is refused.

On macOS, the podman machine forwards the host port to the VM. The
address is also `127.0.0.1`.

### Output and exit codes of the service commands

| Command | Exit code |
|---|---|
| `start` | `0` when the container started. `125` when prism refuses the request, the pull fails, or podman fails. |
| `exec` | The exit code of the command. `124` when `--timeout` expires. `125` when prism refuses the request or podman fails. |
| `logs`, `stop`, `list` | `0`, or `125` when prism refuses the request or podman fails. |

The last line on stderr gives the reason for a `125`.

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

### For `start`

`start` sets the options of `run`, with these differences:

| Option | Value | Reason |
|---|---|---|
| `--detach` | | In place of `--rm`. `stop` or `prism cleanup` removes the container. |
| `--timeout` | none | A service container runs until it is stopped. |
| `--label` | `prism.service=<handle>` | In addition to `prism.instance-id`. It marks a service container, and holds its handle. |
| `--publish` | `127.0.0.1::<port>/tcp` | One for each `--port`. The host address is always `127.0.0.1`. Podman picks the host port. |

After the start, prism reads the published ports from podman. If podman
reports a port on an address other than `127.0.0.1`, prism removes the
container and refuses the request.

The argument vector of `exec` is `podman exec <container ID> COMMAND...`.
Prism resolves the container ID. Podman reads every argument after the
container ID as the command, so the command cannot add an option such as
`--privileged`, `--user`, or `--cap-add`.

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
| `--cgroup-parent` | `<scope>/build` | Every build step runs in a systemd scope that prism stops. On Linux, the scope of the build. On macOS, the scope of the anchor container. See "Platform note". |
| `--signature-policy` | A prism policy file on the host | Linux only. Refuses every image source other than a registry and the local store. See "The image sources of a build". |
| `podman unshare`, `bwrap` | The allowlist of host paths | Linux only. The mount namespace of the build. See "What a build can read". |

The process limit depends on the platform, because `podman build` has no
`--pids-limit`:

- On Linux, the systemd scope of the build has `TasksMax=1024`. This is a
  limit of 1024 tasks for the whole build. Prism also gives podman
  `--cgroup-parent` in the scope, so that every build step is in the scope.
- On macOS, the anchor container has `--pids-limit 1024`. This is a limit
  of 1024 processes for the scope of the anchor: the pause process of the
  anchor and every build step. Processes of other containers do not count,
  so a build cannot make a fork fail in another container.

On macOS, the anchor container has the memory, swap, CPU, and process
limits of `run`, and `--security-opt no-new-privileges`, `--cap-drop all`,
`--read-only`, and `--network none`. It has the labels
`prism.instance-id=<instance ID>` and `prism.build=<build name>`, and a
podman `--timeout` of the build timeout plus 60 seconds.
`--stop-signal KILL` makes the stop of the scope kill each build step at
once.

The argument vector never holds `--volume`, `--secret`, `--ssh`,
`--network`, `--device`, `--cap-add`, or `--build-context`. No input can
add one.

On macOS, the host `podman` CLI talks to the podman machine. The size of
the machine VM is a second limit on memory and CPU.

## Automatic test run: the `go-tests-container-live` job

The `go-tests-container-live` job in `.github/workflows/pr-gate.yml` is the automatic run of the `prism container` integration tests (#3115): `TestIntegration_*` in `internal/prismcontainer` and `TestServicePortBwrap_*` in `internal/container`. It uses rootless podman on an ubuntu runner and runs on PRs that touch `internal/prismcontainer/`, `internal/container/`, or `cmd/container*.go`. It fails on a SKIP and on zero tests run, and it is part of the `pr-gate` fan-in. A worker cannot run these tests, so read the job result on the PR.

## Limits

A build counts as one container, and a running service container counts
as one container. Thus a `run`, a `build`, and a service container of one
session cannot run at the same time. `exec`, `logs`, `stop`, and `list`
start no container, so no limit refuses them.

- **Per session.** One session runs at most one container or build at a
  time. Prism refuses a second request from the same session at once. The
  error names the per-session limit. Wait until a `run` or a `build`
  stops. A service container does not stop by itself: find its handle
  with `prism container list`, then run `prism container stop HANDLE`.
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
- On macOS, the anchor container of a build counts only when no prism
  process holds the marker of the build. Thus a build counts once, and a
  build whose prism process stopped counts until its anchor stops.

## Ownership and cleanup

- Prism finds the containers and the built images of a session by the
  `prism.instance-id` label. Prism never decides ownership by the name.
- `prism container run` removes its container before it returns. This also
  applies after a timeout and after a cancelled request.
- A service container stays until `prism container stop` or
  `prism cleanup` removes it. It has no podman `--timeout`.
- `prism cleanup` does three steps for each incarnation of the session.
  That is the current incarnation and each earlier incarnation that the
  database holds.
  1. It stops every running build, build steps included (see "Platform
     note").
  2. It removes every container that carries the label, with its
     anonymous volumes.
  3. It force-removes every image, intermediate images included, that
     carries the label. This also removes a container of any session that
     uses the image. Do not run an image that another session built.
- Cleanup issues podman commands only when one of these incarnations has
  an audit directory.
- Cleanup removes the audit directory of an incarnation only after the
  sweep of that incarnation succeeds. If the sweep fails, the directory
  stays, and cleanup writes a warning. A later `prism cleanup` of the
  session then sweeps the incarnation again. `prism reset` applies the
  same rule.
- Cleanup of a parent session does the same three steps for each of its
  review agents: it stops their builds, and it removes their containers
  and their images.
- If prism stops before it removes a `run` container, the podman
  `--timeout` stops the container and `--rm` removes it.
- Images that prism pulls stay in the shared image store of the host. Prism
  does not remove them. The base images that a build pulls stay too.

## Audit log

Each request writes one JSON line to an audit log on the host:

```text
$XDG_STATE_HOME/prism/prism-container/audit/<instance ID>/audit.log
```

The line holds these fields:

- `time`, `session`, `instance_id`, and `command` (`run`, `build`,
  `start`, `exec`, `logs`, `stop`, or `list`).
- `image`: for `run`, the image with the Docker Hub prefix added to a
  short name. For `build`, the name of the image that the build makes.
- For `run`: `args` (the container command), `env_keys`, `mount`, and
  `container` (the container name). The log never holds an `--env` value,
  because a value can be a secret.
- For `build`: `context`, `file`, and `build_arg_keys`. The log never holds
  a `--build-arg` value.
- For `start`: the fields of `run`, and `ports` and `handle`.
- For `exec`, `logs`, and `stop`: `handle` (the value that you gave),
  `container`, and `image`. For `exec`, also `args`.
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

An append moves the lines: cleanup removes the source log after the
append, so a later cleanup does not copy the lines again. If an append
fails, cleanup keeps the source directory on disk and writes a warning
that names it.

The prism-container state tree also holds the host-wide lock file, the
per-run cidfile directories, the per-build context copies, and the build
markers. Like the audit directory, no sandbox can write them.

## Errors

| Error text | What to do |
|---|---|
| `refused: this session already has a running container or build` | If it is a `run` or a `build`, wait until it stops, then try again. If it is a service container, it runs until you stop it: find its handle with `prism container list`, then run `prism container stop HANDLE`. |
| `refused: N prism containers and builds are running on this host` | Try again later. If the limit is too low, tell the user about `nx.programs.prism.containerHostLimit`. |
| `podman is not reachable` | Tell the user. On macOS, the user runs `podman machine start`. On Linux, the user runs `systemctl --user status podman.socket`. You cannot correct this from the sandbox. |
| `the --timeout of ... expired` | Increase `--timeout` (maximum `60m`), or make the command faster. |
| `the podman pull of image ... failed` | Read the podman error above this line. The image name or the tag is wrong, or the registry is not reachable. |
| `put -- between the image and the command` | Add `--` after the image. |
| `refused: --env ...` | Use `KEY=VALUE` with a valid key. |
| `refused: --port "..." is not valid` | Give a container port, an integer from 1 to 65535. |
| `refused: service container "..." does not exist in this session` | The container was stopped or removed, or the handle is wrong. Run `prism container list`. |
| `refused: container "..." does not carry the label of this session` | The container belongs to another session. Use a handle that your `start` printed. |
| `refused: container "..." was not created by prism container start` | `exec`, `logs`, and `stop` act only on a service container. |
| `podman run exited N, so the service container did not start` | Read the podman error above this line. |
| `can only create exec sessions on running containers` | The service container stopped. Read `prism container logs HANDLE`, then `stop` it and start it again. |
| `the --timeout of ... expired, so prism stopped the build` | Increase `--timeout` (maximum `60m`), or make the build faster. |
| `the build failed: podman build exited N` | Read the build output above this line. |
| `refused: CONTEXT ...` or `refused: --file ...` | Give a path in the worktree that exists. A symlink must resolve inside the worktree. |
| `refused: --tag ... is not a valid image tag` | Use a lower-case `NAME` or `NAME:TAG`. See "The image name". |
| `refused: the build context holds more than 4 GiB` | Give a smaller `CONTEXT`, or list large directories in `.containerignore`. |
| `refused: the build context changed while prism copied it` | Try again when nothing writes to the context. |
| `podman build did not start: ...` | Prism could not set up the build on the host, so podman build did not run. Tell the user, and give the full message: it holds the cause. You cannot correct this from the sandbox. |
| `refused: podman machine "..." mounts Mac paths that are not in the allowlist` | Tell the user. The message gives the `containers.conf` setting and the commands that recreate the machine. You cannot correct this from the sandbox. |
| `refused: the Containerfile cannot be built: line N: ...` | Read the reason. Write each image source literally: a registry image, a local image, or a build stage, with no variable, quote, or backslash. If line N is shell or SQL text, restructure it. See "The image sources of a build". |
| `The signature policy of the build refused an image source` | A base image has an `ONBUILD` instruction that names a transport. Use a different base image. |

## How it works

- In a `bwrap` or `sandbox-exec` session, the `prism` CLI sends the request
  to the sidecar of the session: `POST /container/run`, `/container/build`,
  `/container/start`, `/container/exec`, `/container/logs`,
  `/container/stop`, or `/container/list`. The sidecar runs on the host. It takes the session name, the
  instance ID, and the worktree from its own configuration, never from the
  request.
- In a `host` session, the CLI runs the same code in its own process. It
  reads the instance ID and the worktree of the session from the prism
  database.
- Both routes call one function that builds the podman argument vector.
  Thus the two routes cannot differ.
- For `build`, one internal part, the build executor, differs between
  Linux and macOS. It checks the host before the build, and it adds
  options to the argument vector of the shared builder. It changes
  nothing else:
  - Linux: `--signature-policy` after `build`. The command that runs is
    `podman unshare bwrap <allowlist> -- podman --cgroup-manager=cgroupfs
    build --cgroup-parent <scope>/build ...`, in a systemd scope with
    `TasksMax`, `Delegate`, and `RuntimeMaxSec`. Before the scope starts,
    prism runs `podman unshare true`, so that the pause process of podman
    is not in the scope.
  - macOS: the machine mount check before the build. Prism starts the
    anchor container, then runs `podman build --cgroup-parent <anchor
    scope>/build ...`. A shell guard runs the `podman build` client and
    kills it when the prism process stops. Prism stops the client before
    it removes the anchor, because the VM starts no build step after the
    client stops, and a build step that starts after the scope is gone
    runs outside every scope.
- The source is in `modules/programs/prism/prism/internal/prismcontainer/`.
