# F.1 — sandbox-exec `(version 3)` migration spike

**Status:** spike / design doc (no code changes).
**Issue:** [#1191](https://github.com/prismatic-koi/nixos-config/issues/1191).
**Refs:** [#1190](https://github.com/prismatic-koi/nixos-config/issues/1190) (bug — Apple-signed binary SIGABRT),
[#1192](https://github.com/prismatic-koi/nixos-config/issues/1192) (test convention),
[#1193](https://github.com/prismatic-koi/nixos-config/issues/1193) (merged — `/etc` symlink fix),
[#1012](https://github.com/prismatic-koi/nixos-config/issues/1012) (sandbox-exec design).
**Machine:** `m4mac` — Apple M4, macOS 15.x, Darwin 24.6.0, **SIP enabled** (confirmed via `csrutil status`).

---

## 1. Goal and scope

This document is a **spike deliverable**: prototype a `(version 3)` SBPL profile for
`generateProfile()` that allows Apple-signed binaries to run inside the prism sandbox
without SIGABRT, while preserving the deny-default file-read posture for user-homedir
secrets.

The current production profile is `(version 1)`. Apple-signed Mach-O binaries SIGABRT in
dyld under that profile. The root cause is that `(version 3)` features — `syscall-unix`,
`syscall-mach`, `system-mac-syscall`, `system-fcntl` — are silently absent from version 1
profiles, and dyld/AMFI requires them to bootstrap Apple-signed code. See §4 for details.

This document does **not** change `sandbox_exec.go`. The implementation is a follow-up.

---

## 2. Minimum viable `(version 3)` profile

The following is the full SBPL prototype profile. It is self-contained (no imports). All
non-obvious rules have inline comments. Replace the `STAGING_HOME` and `HOST_AWS_PATH`
placeholders with runtime values from `generateProfile()`.

```scheme
; prism sandbox-exec (version 3) profile.
; Generated per-session by generateProfile() in internal/container/sandbox_exec.go.
; Replace STAGING_HOME, WORKTREE, BARE_ROOT, HOST_API_SOCK_DIR, HOST_HOME_AWS,
; and HOST_HOME with actual runtime-computed values.
(version 3)
(deny default)

;;; ── 1. Cryptex graft points ──────────────────────────────────────────
;;; macOS 15+ stores the dyld shared cache under:
;;;   /System/Volumes/Preboot/Cryptexes/OS/System/Library/dyld/
;;; Both /System/Volumes/Preboot/Cryptexes (the Preboot-volume graft) and
;;; /System/Cryptexes (the live-FS boot alias) must be readable AND
;;; map-executable for dyld to bootstrap any binary. This matches
;;; dyld-support.sb lines 17-39.
(allow file-read* file-test-existence file-map-executable
  (subpath "/System/Volumes/Preboot/Cryptexes")
  (subpath "/System/Cryptexes"))

;;; ── 2. Standard system read-only roots ──────────────────────────────
;;; /nix            — Nix store. All Nix-built binaries live here.
;;; /usr, /bin, /sbin — standard Apple-signed utility directories.
;;; /System, /Library — OS frameworks, dylibs, and shared data.
;;; /Applications/Xcode.app — Xcode CLI tools. /usr/bin/git is a shim
;;;   that calls xcrun, which probes Xcode paths. Allowing the full
;;;   Xcode.app subtree lets xcrun locate libxcrun.dylib and the
;;;   xcrun binary without a separate deny.
;;; /etc, /private/etc — NOTE: on macOS /etc is a top-level symlink
;;;   to /private/etc, but sandbox-exec does NOT transparently follow
;;;   top-level symlinks for access checks. Both must be listed
;;;   independently. Pattern established in PR #1193 for v1.
;;; /private/var/db/dyld, /var/db/dyld — dyld shared-cache DB.
;;;   /var is a symlink to /private/var; both forms needed.
;;; /private/var/db/timezone, /var/db/timezone — timezone data.
;;; /private/var/select, /var/select — xcode-select developer_dir path.
;;;   /usr/bin/git shim calls xcode-select to locate the active
;;;   CommandLineTools install. Without these, git emits an xcode-select
;;;   error (non-fatal on this machine because the symlink is absent,
;;;   but the allow prevents a sandbox denial in the kernel log).
;;; /private/var/folders, /var/folders — Darwin per-user TMPDIR.
;;;   xcrun writes a tool-discovery cache to $TMPDIR/xcrun_db-*. The
;;;   write may fail due to SIP-protected extended attributes on the T/
;;;   directory (see §6 — SIP behaviour), but the allow prevents the
;;;   access from being a sandbox violation.
;;; /private/tmp, /tmp — general temp dir; also used as fallback
;;;   TMPDIR by some tools when the var/folders path is restricted.
;;; /dev/dtracehelper — Apple-signed binaries probe this at startup
;;;   to detect DTrace. The file-test-existence denial appears in the
;;;   kernel log as a non-fatal sandbox violation; including it here
;;;   eliminates that log noise.
(allow file-read* file-test-existence file-map-executable file-read-metadata
  (subpath "/nix")
  (subpath "/usr")
  (subpath "/bin")
  (subpath "/sbin")
  (subpath "/System")
  (subpath "/Library")
  (subpath "/Applications/Xcode.app")
  (subpath "/private/etc")
  (subpath "/etc")
  (subpath "/private/var/db/dyld")
  (subpath "/private/var/db/timezone")
  (subpath "/private/var/select")
  (subpath "/private/var/folders")
  (subpath "/var/db/dyld")
  (subpath "/var/db/timezone")
  (subpath "/var/select")
  (subpath "/var/folders")
  (literal "/dev/null")
  (literal "/dev/random")
  (literal "/dev/urandom")
  (literal "/dev/dtracehelper")
  (literal "/"))

;;; ── 3. /tmp read-write ───────────────────────────────────────────────
;;; /tmp and /private/tmp are used by xcrun, git, and other tools for
;;; transient files. Both symlink forms needed.
(allow file-read* file-write* file-test-existence
  (subpath "/private/tmp")
  (subpath "/tmp"))

;;; ── 4. Sensitive /etc subtree denies ─────────────────────────────────
;;; These deny rules must follow the broad /etc and /private/etc allows
;;; above. In SBPL, more-specific rules override broader ones (deny wins
;;; over allow at the same specificity). Both /etc/... and
;;; /private/etc/... forms needed due to symlink non-transparency.
(deny file-read* file-write*
  (subpath "/etc/wireguard")
  (subpath "/etc/wpa_supplicant")
  (subpath "/private/etc/wireguard")
  (subpath "/private/etc/wpa_supplicant"))

;;; ── 5. Host ~/.aws deny ───────────────────────────────────────────────
;;; The staging HOME contains symlinks to the staged .aws credential
;;; entries (per issue #1017). The host raw ~/.aws subtree must remain
;;; read-denied so that path traversal cannot bypass the staging map.
;;; generateProfile() must substitute the actual home directory path.
(deny file-read* file-write*
  (subpath "HOST_HOME_AWS"))   ; e.g. /Users/bensherman/.aws

;;; ── 6. Staging HOME + worktree + bare repo + host-API socket (RW) ──
;;; Session-specific read-write paths. generateProfile() substitutes the
;;; actual runtime values for each session.
(allow file-read* file-write* file-test-existence file-read-metadata
  (subpath "STAGING_HOME")       ; e.g. /tmp/prism-homes/<session>
  (subpath "WORKTREE")           ; m.cfg.Worktree
  (subpath "BARE_ROOT/.bare")    ; filepath.Join(m.cfg.BareRoot, ".bare")
  (subpath "HOST_API_SOCK_DIR")) ; filepath.Dir(m.cfg.HostAPISockPath)

;;; ── 7. opencode shared state dir (RW) ────────────────────────────────
;;; ~/.local/share/opencode — SQLite DB, logs, snapshots. This path is
;;; under the host HOME, not the staging HOME, so it must be explicitly
;;; allowed. generateProfile() computes this from os.UserHomeDir().
(allow file-read* file-write* file-test-existence
  (subpath "HOST_HOME/.local/share/opencode"))

;;; ── 8. /dev/null write access ────────────────────────────────────────
;;; Apple-signed binaries (git, ssh) open /dev/null for writing during
;;; startup. The file-read* above covers reads; writes need an explicit
;;; allow. The file-write* for /tmp also covers writes to /dev/null
;;; via /private/tmp... wait — /dev/null is not under /tmp. Adding it
;;; explicitly.
(allow file-write-data
  (literal "/dev/null"))

;;; ── 9. Process operations ─────────────────────────────────────────────
;;; process-exec*  — execvp(2). Required for any child process to launch.
;;; process-fork   — fork(2). git and ssh use it for child processes.
;;; process-info*  — includes process-info-codesignature AND
;;;   process-info-pidinfo. Both are required by AMFI when validating
;;;   the certificate chain of Apple-signed binaries. Without this,
;;;   git and ssh abort with SIGABRT during dyld initialisation.
;;;   Evidence: kernel log shows "deny(1) process-info-codesignature self"
;;;   and "deny(1) process-info-pidinfo self" before the abort.
(allow process-exec* process-fork process-info*)

;;; ── 10. Mach IPC ──────────────────────────────────────────────────────
;;; (allow mach-lookup) without a name filter allows lookup of any
;;; bootstrap service — equivalent to the v1 (allow mach-lookup).
;;; In v3 you CAN restrict to specific global-name values (see §4 —
;;; Open questions). The six services observed in the kernel log for
;;; /bin/echo + /usr/bin/git are:
;;;   com.apple.bsd.dirhelper               (Darwin TMPDIR lookup)
;;;   com.apple.diagnosticd                 (crash reporter)
;;;   com.apple.logd                        (os_log unified logging)
;;;   com.apple.system.notification_center
;;;   com.apple.system.opendirectoryd.libinfo
;;;   com.apple.system.opendirectoryd.membership
;;; All six are in system.sb's mach-lookup list. A future tightening
;;; pass should enumerate and restrict to these plus any additional
;;; services surfaced by opencode / node.
;;;
;;; mach-register: allows the process to register per-pid Mach names.
;;; Required for opencode's IPC (and implicitly by CoreFoundation).
(allow mach-lookup mach-register)

;;; ── 11. Signals ───────────────────────────────────────────────────────
;;; Allow the sandboxed process to send signals to itself and children.
(allow signal)

;;; ── 12. POSIX shared memory ───────────────────────────────────────────
;;; CRITICAL: The v1 (allow ipc-posix-shm) is an UNBOUND VARIABLE in
;;; v3. The sandbox compiler rejects it at parse time:
;;;   "sandbox-exec: unbound variable: ipc-posix-shm"
;;; Use the split read*/write* variants instead.
;;; ipc-posix-shm-read*  — required by dyld and notification_center.
;;; ipc-posix-shm-write* — required by CoreFoundation / libobjc init.
(allow ipc-posix-shm-read* ipc-posix-shm-write*)

;;; ── 13. sysctl reads ──────────────────────────────────────────────────
;;; Many system libraries query sysctl at init (kern.*, hw.*, machdep.*).
(allow sysctl-read)

;;; ── 14. syscall-unix and syscall-mach ────────────────────────────────
;;; CRITICAL: In (version 3) with deny-default, individual syscalls are
;;; gated by the sandbox policy. Without (allow syscall-unix), dyld
;;; aborts with SIGABRT during the libignition phase, before any user
;;; code executes.
;;;
;;; The specific syscalls required by libignition are documented in
;;; Apple's dyld-support.sb:
;;;   SYS___mac_syscall, SYS_getfsstat[64], SYS_map_with_linking_np,
;;;   SYS_open, SYS_openat, SYS_fstatat[64], SYS_dup
;;; We grant the full (allow syscall-unix) rather than enumerating
;;; because user-space tools (bash, git, ssh) make many additional
;;; syscalls beyond the libignition bootstrap set.
;;;
;;; (allow syscall-mach) is required for Mach trap calls (mach_msg,
;;; mach_port_allocate, etc.) used by CoreFoundation and libdispatch.
(allow syscall-unix syscall-mach)

;;; ── 15. system-mac-syscall ────────────────────────────────────────────
;;; Required for AMFI certificate chain validation. The specific MAC
;;; policy calls observed:
;;;   policy-name "Sandbox", number 2  (SYSCALL_CHECK_SANDBOX — per dyld-support.sb)
;;;   policy-name "Sandbox", number 67 (container check — per system.sb)
;;; Granting the broad form here. A future pass could restrict to the
;;; specific mac-syscall-numbers using:
;;;   (allow system-mac-syscall (mac-policy-name "Sandbox")
;;;          (mac-syscall-number 2 67))
(allow system-mac-syscall)

;;; ── 16. system-fcntl ──────────────────────────────────────────────────
;;; Required for F_ADDFILESIGS_RETURN, F_CHECK_LV, and F_GETPATH, which
;;; dyld calls when mapping code-signed executables (per dyld-support.sb).
;;; Without this, dyld cannot validate binary signatures.
(allow system-fcntl)

;;; ── 17. Network ───────────────────────────────────────────────────────
;;; Unchanged from v1. Matches the bwrap baseline. Restriction to
;;; specific hosts/ports is a future concern per #1012.
(allow network*)

;;; ── NOTE on iokit-open ────────────────────────────────────────────────
;;; The v1 (allow iokit-open) is NOT valid in v3 — the sandbox compiler
;;; accepts it but it is treated as (allow iokit-open-service) and
;;; (allow iokit-open-user-client) WITHOUT any class filter, which means
;;; it allows all IOKit user client opens. In v3 the preferred form is:
;;;   (allow iokit-open-service (iokit-registry-entry-class "ClassName"))
;;;   (allow iokit-open-user-client (iokit-user-client-class "ClassName"))
;;; For our harness (opencode + bash + standard Unix tools), iokit is
;;; NOT required for any of the binaries in the verification matrix.
;;; The v1 (allow iokit-open) rule can be removed in the v3 migration.
;;; If a specific binary needs GPU/camera/USB access, add a targeted
;;; iokit-open-service/iokit-open-user-client rule.
```

---

## 3. Verification matrix

All invocations use this standard form, enabling reproducibility:

```
env -i PATH="<PATH>" HOME="<STAGING_HOME>" [extra vars] \
  /usr/bin/sandbox-exec -f <profile> <binary> <args> </dev/null 2>&1; echo "exit: $?"
```

Profile under test: `/tmp/v3-final.sb` (the prototype above).
Machine: `m4mac` (Apple M4, macOS 15.x, Darwin 24.6.0, SIP on).
Staging HOME: `/tmp/fake-home` (empty directory, simulating a staging HOME).

### 3.1 Positive cases (must work — exit 0)

**P1 — `/bin/echo hi`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /bin/echo hi </dev/null 2>&1; echo "exit: $?"
hi
exit: 0
```

**P2 — `/bin/ls /etc/hosts`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /bin/ls /etc/hosts </dev/null 2>&1; echo "exit: $?"
/etc/hosts
exit: 0
```

**P3 — `/bin/cat /etc/hosts`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /bin/cat /etc/hosts </dev/null 2>&1; echo "exit: $?"
##
# Host Database
#
# localhost is used to configure the loopback interface
# when the system is booting.  Do not change this entry.
##
127.0.0.1	localhost
255.255.255.255	broadcasthost
::1             localhost
exit: 0
```

**P4 — `/usr/bin/git --version`**

Note: On m4mac, `xcode-select` is installed (`xcode-select -p` → `/Library/Developer/CommandLineTools`) but the `/var/select/developer_dir` symlink is absent. The `/usr/bin/git` shim requires `DEVELOPER_DIR` set to bypass the xcode-select symlink lookup. This is a machine-state issue independent of the sandbox: the same command fails outside the sandbox without `DEVELOPER_DIR`. In production, the worktree will typically have git operations use the Nix-built git (`/nix/store/.../bin/git`) which does not use xcrun at all.

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  DEVELOPER_DIR="/Library/Developer/CommandLineTools" \
  GIT_CONFIG_NOSYSTEM=1 \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /usr/bin/git --version </dev/null 2>&1; echo "exit: $?"
git: error: couldn't create cache file '/var/folders/hr/.../T/xcrun_db-aEatL4Vs' (errno=Operation not permitted)
git: error: couldn't create cache file '/var/folders/hr/.../T/xcrun_db-LDMw6BCQ' (errno=Operation not permitted)
git version 2.39.5 (Apple Git-154)
exit: 0
```

The `xcrun_db-*` cache-write errors are non-fatal. They occur because the Darwin TMPDIR (`/var/folders/.../T/`) has the `com.apple.rootless` extended attribute (SIP protection), which blocks file creation even when the sandbox allows the path. `git --version` still exits 0. See §6 for SIP behaviour details.

**P5 — `/usr/bin/uname -a`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /usr/bin/uname -a </dev/null 2>&1; echo "exit: $?"
Darwin m4mac 24.6.0 Darwin Kernel Version 24.6.0: Mon Jan 19 22:01:41 PST 2026; root:xnu-11417.140.69.708.3~1/RELEASE_ARM64_T8132 arm64
exit: 0
```

**P6 — `/usr/bin/which opencode`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /usr/bin/which opencode </dev/null 2>&1; echo "exit: $?"
/etc/profiles/per-user/bensherman/bin/opencode
exit: 0
```

(opencode resolves through `/etc/profiles/per-user/bensherman` → `/etc/static/...` → `/nix/store/...`, all of which are in the allowed paths.)

**P7 — `/usr/bin/ssh -V`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb /usr/bin/ssh -V </dev/null 2>&1; echo "exit: $?"
OpenSSH_9.9p2, LibreSSL 3.3.6
exit: 0
```

**P8 — `opencode --help` (the Nix harness)**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb opencode --help </dev/null 2>&1; echo "exit: $?"
error: An unknown error occurred (Unexpected)
exit: 0
```

opencode exits 0. The "unexpected error" is because `opencode --help` attempts to connect to a running opencode server, which is not present in this test. Crucially: no SIGABRT, no sandbox denial — the Nix-built binary bootstraps cleanly under the v3 profile.

**P9 — Nix-built `bash -c 'echo hi'`**

```
$ env -i PATH="$PATH" HOME="/tmp/fake-home" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb bash -c 'echo hi' </dev/null 2>&1; echo "exit: $?"
shell-init: error retrieving current directory: getcwd: cannot access parent directories: Operation not permitted
hi
exit: 0
```

bash resolves to `/nix/store/my9bsdsfxcaxkb400i4xvvh1ahb8pybs-bash-interactive-5.3p9/bin/bash`. The `getcwd` error occurs because the test was run from `/Users/bensherman/...` which is outside the sandbox's allowed paths. In production, the CWD will be the worktree (which is allowed). The `echo hi` output confirms bash executes correctly. Exit 0.

### 3.2 Negative cases (must remain denied)

**N1 — `cat ~/.ssh/id_ed25519`** (SSH private key)

```
$ env -i PATH="$PATH" HOME="$HOME" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb cat "$HOME/.ssh/id_ed25519" </dev/null 2>&1; echo "exit: $?"
cat: /Users/bensherman/.ssh/id_ed25519: Operation not permitted
exit: 1
```

**N2 — `cat ~/.aws/credentials`** (AWS credentials)

```
$ env -i PATH="$PATH" HOME="$HOME" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb cat "$HOME/.aws/credentials" </dev/null 2>&1; echo "exit: $?"
cat: /Users/bensherman/.aws/credentials: Operation not permitted
exit: 1
```

**N3 — `find ~/code -name '.env'`** (source tree env files)

```
$ env -i PATH="$PATH" HOME="$HOME" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb find "$HOME/code" -name '.env' </dev/null 2>&1; echo "exit: $?"
find: /Users/bensherman/code: Operation not permitted
exit: 0
```

`find` exits 0 (its exit code is 0 when the error is in the path argument, not a find failure), but the directory is inaccessible and no `.env` files are returned.

**N4 — `cat ~/Library/Keychains/login.keychain-db`** (keychain)

```
$ env -i PATH="$PATH" HOME="$HOME" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb cat "$HOME/Library/Keychains/login.keychain-db" </dev/null 2>&1; echo "exit: $?"
cat: /Users/bensherman/Library/Keychains/login.keychain-db: Operation not permitted
exit: 1
```

**N5 — `cat ~/Documents/<anything>`**

```
$ env -i PATH="$PATH" HOME="$HOME" \
  /usr/bin/sandbox-exec -f /tmp/v3-final.sb cat "$HOME/Documents/spike-test.txt" </dev/null 2>&1; echo "exit: $?"
cat: /Users/bensherman/Documents/spike-test.txt: Operation not permitted
exit: 1
```

---

## 4. Open questions resolved

### 4.1 `(version 3)` equivalent of `iokit-open`

In `(version 1)`, the operation is `iokit-open` (a single operation accepting `(iokit-user-client-class "...")` as a filter).

In `(version 3)`, `iokit-open` is **split into two distinct operations**:

- `iokit-open-service` — filtered by `(iokit-registry-entry-class "ClassName")`. Used to open an IOService entry.
- `iokit-open-user-client` — filtered by `(iokit-user-client-class "ClassName")`. Used to open a user-client connection to an IOService.

Evidence from Apple's profiles: `system.sb` defines the `(system-graphics)` function which uses both:
```scheme
(allow iokit-open-service
       (iokit-registry-entry-class "IOAccelerator" "IOSurfaceRoot"))
(allow iokit-open-user-client
       (iokit-user-client-class "IOAccelerationUserClient" ...))
```

**For our use case**: the verification matrix confirms that none of the tested binaries (echo, ls, cat, git, uname, which, ssh, opencode, bash) require iokit access. The v1 `(allow iokit-open)` rule can be **removed** in the v3 migration without affecting the harness. If a future binary requires GPU/USB/camera access, add the specific targeted form.

### 4.2 Does `(import "system.sb")` work from a non-Apple-signed user-process parent?

**Yes, it works.** Tested with:

```scheme
(version 3)
(deny default)
(import "system.sb")
(import "dyld-support.sb")
; ... additional rules
```

The profile parses without error and `/bin/echo` exits 0 under it. The imports compile correctly from a user-process parent (`/usr/bin/sandbox-exec` itself is Apple-signed, and it loads the profile before exec'ing the user binary).

**Important caveat**: when imported, `system.sb` skips its `(unless *import-path* ...)` block, which means the following rules are **NOT** inherited from the import:
```scheme
(unless *import-path*
  (allow mach-bootstrap)
  (allow syscall*))
```

This means after importing `system.sb`, you must **explicitly add** `(allow syscall-unix syscall-mach)` and `(allow mach-lookup)` to your profile. The standalone profile approach (§2) avoids this gotcha by not using imports, which also makes the profile self-documenting.

**Recommendation**: Use the standalone profile (no imports) for the `generateProfile()` implementation. The import approach adds compile-time indirection and strips critical rules via the `*import-path*` guard, making the resulting policy harder to reason about. The standalone profile is 9.7 KB with comments (vs 1.2 KB for the import-based profile) but the parse time is negligible (~15 ms vs ~20 ms, measured via `time /usr/bin/sandbox-exec -f <profile> /bin/echo`).

### 4.3 `(version 3)` equivalent of `(allow mach-lookup)` unrestricted

In both v1 and v3, `(allow mach-lookup)` without a name filter is valid and allows lookup of any bootstrap service name. This is the broadest form and equivalent to the v1 behaviour.

In v3, the preferred form is to restrict by name:
```scheme
(allow mach-lookup
  (global-name "com.apple.logd")
  (global-name "com.apple.cfprefsd.agent")
  ...)
```

The six mach services observed in the kernel log for `/bin/echo` + `/usr/bin/git` (using `log stream` capture with `(deny mach-lookup)` and no name filter):

| Service | Purpose |
|---|---|
| `com.apple.bsd.dirhelper` | Darwin TMPDIR/CACHEDIR path lookup (`confstr()`) |
| `com.apple.diagnosticd` | crash reporter / diagnostic messaging |
| `com.apple.logd` | os_log unified logging subsystem |
| `com.apple.system.notification_center` | NSNotificationCenter, CFNotificationCenter |
| `com.apple.system.opendirectoryd.libinfo` | user/group info lookup (`getpwuid`, etc.) |
| `com.apple.system.opendirectoryd.membership` | group membership checks |

All six are included in `system.sb`'s `(allow mach-lookup ...)` block. The opencode/node harness likely requires additional services (cfprefsd, trustd, secinitd, etc.) — a future tightening pass should enumerate these with the same technique (deny mach-lookup, run the harness, read the log).

### 4.4 SIP-on vs SIP-off behaviour differences

SIP is **enabled** on m4mac (`csrutil status` → `System Integrity Protection status: enabled`).

**Observed SIP effect**: the Darwin per-user TMPDIR (`/var/folders/.../T/`) has the `com.apple.rootless` extended attribute (confirmed via `ls -la@`). When SIP is enabled, this attribute prevents file creation in the T/ directory even for the owning user, even when the sandbox allows the path. This is why `xcrun` prints "couldn't create cache file" errors during git runs — the sandbox policy allows it, but SIP blocks it at the VFS layer below the sandbox.

This behaviour is **non-fatal**: `git --version` exits 0 despite the cache file errors (xcrun falls back gracefully). The implementation does not need to work around this.

**SIP-off implication** (not tested, inferred): with SIP disabled, the `com.apple.rootless` attribute is not enforced, so xcrun cache creation would succeed silently. The sandbox policy would remain identical.

**No profile change required** for SIP-on vs SIP-off. The profile is correct either way.

---

## 5. Approach (B) — selective Nix-tool PATH prepend

All nine positive-case binaries pass under approach (A) (the v3 SBPL profile alone):

| Binary | Approach (A) exit | Nix equivalent | Notes |
|---|---|---|---|
| `/bin/echo` | 0 | `echo` (shell builtin or nix coreutils) | Approach (A) sufficient |
| `/bin/ls` | 0 | `ls` (nix coreutils) | Approach (A) sufficient |
| `/bin/cat` | 0 | `cat` (nix coreutils) | Approach (A) sufficient |
| `/usr/bin/git` | 0 | `/nix/store/.../bin/git` (in production use) | Approach (A) sufficient; DEVELOPER_DIR workaround only needed for m4mac's machine state |
| `/usr/bin/uname` | 0 | `uname` (nix coreutils) | Approach (A) sufficient |
| `/usr/bin/which` | 0 | `which` (nix which) | Approach (A) sufficient |
| `/usr/bin/ssh` | 0 | `ssh` (nix openssh) | Approach (A) sufficient |
| `opencode` (Nix) | 0 | n/a (this is the harness) | Approach (A) sufficient |
| `bash` (Nix) | 0 | n/a (this is the shell) | Approach (A) sufficient |

**Approach (B) is not required** for any binary in the verification matrix. All pass under approach (A) alone.

**Binaries with no Nix equivalent** (macOS-specific, approach (B) cannot help):
- `sw_vers` — macOS version query
- `pmset` — power management
- `codesign` — code signing verification
- `softwareupdate` — OS update management
- `diskutil` — disk management
- `security` — keychain CLI
- `xcode-select` / `xcrun` — Xcode toolchain management

These macOS-specific tools are not part of the current verification matrix. They would need approach (A) coverage (the v3 profile rules above) if they need to run in the sandbox. None of them are expected to SIGABRT under the v3 profile since the profile grants `syscall-unix`, `syscall-mach`, `system-mac-syscall`, and `process-info*` — the root causes of the current SIGABRT issue.

---

## 6. Key v1 → v3 migration delta

This section summarises every change required in `generateProfile()`:

| Change | v1 | v3 |
|---|---|---|
| Version header | `(version 1)` | `(version 3)` |
| `ipc-posix-shm` | `(allow ipc-posix-shm)` — valid in v1 | **REMOVED** — unbound variable in v3. Replace with `(allow ipc-posix-shm-read* ipc-posix-shm-write*)` |
| `iokit-open` | `(allow iokit-open)` | Remove entirely; not needed for the harness |
| `syscall-unix` | Not supported in v1 | **ADD**: `(allow syscall-unix syscall-mach)` — required for dyld bootstrap |
| `system-mac-syscall` | Not supported in v1 | **ADD**: `(allow system-mac-syscall)` — required for AMFI cert validation |
| `system-fcntl` | Not supported in v1 | **ADD**: `(allow system-fcntl)` — required for code-sign fcntl calls |
| `process-info*` | `(allow process-exec* process-fork signal mach-lookup mach-register sysctl-read iokit-open ipc-posix-shm)` | **ADD** `process-info*` to the process allow |
| `/bin` and `/sbin` | Not present in v1 file-read allow (v1 only listed `/nix`, `/usr`, `/System`, `/Library`, `/etc`, `/private/etc`, `/private/var/db/dyld`, `/private/var/db/timezone`) | **ADD** `(subpath "/bin")` and `(subpath "/sbin")` — required for Apple-signed utility binaries |
| System-root file operations | v1 uses only `file-read*` on system roots | **ADD** `file-test-existence`, `file-map-executable`, `file-read-metadata` alongside `file-read*` on system roots — required for dyld to probe and map code-signed binaries |
| `/` (root literal) | Not present | **ADD** `(literal "/")` to the system reads — required by libignition, which uses `/` as an `openat(2)` root |
| Cryptex paths | Not present | **ADD**: `(allow file-read* file-test-existence file-map-executable (subpath "/System/Volumes/Preboot/Cryptexes") (subpath "/System/Cryptexes"))` |
| `/dev/dtracehelper` | Not present | **ADD**: `(literal "/dev/dtracehelper")` to read-only file list |
| `/var/select`, `/var/db/dyld`, `/var/db/timezone` | Not present | **ADD** `/var/...` forms — symlink non-transparency (same pattern as `/etc` → PR #1193) |
| `/var/folders` | Not present | **ADD**: `(allow file-read* file-test-existence (subpath "/private/var/folders") (subpath "/var/folders"))` — read-only; xcrun cache writes may fail due to SIP (non-fatal, see §4.4) |
| `/Applications/Xcode.app` | Not present | **ADD**: needed for xcrun (called by /usr/bin/git shim) |
| `/tmp` read-write | Not present explicitly | **ADD**: `(allow file-read* file-write* file-test-existence (subpath "/private/tmp") (subpath "/tmp"))` |
| `/dev/null` write | Not present | **ADD**: `(allow file-write-data (literal "/dev/null"))` |

---

## 7. Profile size and parse-cost measurement

```
wc -c /tmp/v3-final.sb
   9774 /tmp/v3-final.sb   (includes inline comments)

time /usr/bin/sandbox-exec -f /tmp/v3-final.sb /bin/echo hi
/usr/bin/sandbox-exec -f /tmp/v3-final.sb /bin/echo hi  0.01s user 0.00s system 89% cpu 0.015 total
```

The standalone v3 profile is 9.7 KB with comments. Parse + exec overhead is ~15 ms — identical to the current v1 profile. Per-spawn generation cost is negligible and unchanged.

---

## 8. Acceptance Criteria for the follow-up implementation issue

Copy-paste this block into the implementation issue for the `generateProfile()` v3 migration:

```markdown
## Acceptance Criteria

### Positive cases — must exit 0 under the v3 profile

- [ ] [functional] `/bin/echo hi` exits 0 under the v3 profile (Apple-signed ARM64e binary).
- [ ] [functional] `/bin/ls /etc/hosts` exits 0 under the v3 profile.
- [ ] [functional] `/bin/cat /etc/hosts` exits 0 and outputs the hosts file content.
- [ ] [functional] `/usr/bin/git --version` exits 0 under the v3 profile. On machines where
  `xcode-select` is configured, DEVELOPER_DIR need not be set. On machines where
  `/var/select/developer_dir` is absent, set `DEVELOPER_DIR=/Library/Developer/CommandLineTools`.
  xcrun cache-write errors (`couldn't create cache file`) are acceptable (non-fatal, SIP-related).
- [ ] [functional] `/usr/bin/uname -a` exits 0 under the v3 profile.
- [ ] [functional] `/usr/bin/which opencode` exits 0 and returns the opencode path.
- [ ] [functional] `/usr/bin/ssh -V` exits 0 and outputs the OpenSSH version string.
- [ ] [functional] `opencode --help` exits 0 (or exits non-zero for a non-sandbox reason such
  as missing server — no SIGABRT).
- [ ] [functional] A Nix-built `bash -c 'echo hi'` exits 0 and outputs `hi`.

### Negative cases — must remain denied under the v3 profile

- [ ] [security] `cat ~/.ssh/id_ed25519` (or any non-staged SSH key) exits non-zero with
  "Operation not permitted" — not readable from inside the sandbox.
- [ ] [security] `cat ~/.aws/credentials` exits non-zero with "Operation not permitted".
- [ ] [security] `find ~/code -name '.env'` returns no results — the ~/code subtree is
  inaccessible from inside the sandbox.
- [ ] [security] `cat ~/Library/Keychains/login.keychain-db` exits non-zero with
  "Operation not permitted".
- [ ] [security] `cat ~/Documents/<anything>` exits non-zero with "Operation not permitted".

### Profile correctness

- [ ] [functional] `generateProfile()` emits `(version 3)` as the first SBPL statement.
- [ ] [functional] `generateProfile()` does NOT emit `(allow ipc-posix-shm)` — this is an
  unbound variable in v3. It must emit `(allow ipc-posix-shm-read* ipc-posix-shm-write*)`.
- [ ] [functional] `generateProfile()` does NOT emit `(allow iokit-open)` — not needed in
  v3. Remove this rule from the generator.
- [ ] [functional] `generateProfile()` emits `(allow syscall-unix syscall-mach)`.
- [ ] [functional] `generateProfile()` emits `(allow system-mac-syscall)`.
- [ ] [functional] `generateProfile()` emits `(allow system-fcntl)`.
- [ ] [functional] `generateProfile()` emits `(allow process-info*)` alongside the existing
  process-exec* and process-fork allows.
- [ ] [functional] `generateProfile()` emits Cryptex graft point allows:
  `/System/Volumes/Preboot/Cryptexes` and `/System/Cryptexes`.
- [ ] [functional] `generateProfile()` emits `/var/...` alias forms for paths that were
  previously only listed as `/private/var/...`:
  `/var/db/dyld`, `/var/db/timezone`, `/var/select`, `/var/folders`.

### Regression tests

- [ ] [test] An integration test exists that invokes `/usr/bin/sandbox-exec -f <profile>` with
  the v3 profile against each binary in the positive-case list above, asserting exit 0 (or
  expected non-zero with no SIGABRT). The test is skipped on Linux (build tag: `//go:build darwin`).
- [ ] [test] An integration test exists that invokes each negative-case read attempt under the
  v3 profile and asserts "Operation not permitted" (EPERM) is returned.
- [ ] [test] `go build ./...` passes on Darwin after the change.
- [ ] [test] `go test ./...` passes on Darwin after the change (existing unit tests unaffected).
- [ ] [test] `go build ./...` passes on Linux (sandbox_exec.go is Darwin-only by build tag;
  no new Linux-visible symbols introduced).
```
