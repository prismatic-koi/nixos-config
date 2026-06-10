# FD isolation Layer 2 (issue #2191, parent #2181): raise the Darwin
# file-descriptor sysctls so legitimate host workload plus burst load
# (nix builds, darwin-rebuild switches) cannot exhaust the system-wide
# FD pool — the #2180-class failure mode where kern.num_files reaches
# kern.maxfiles and every open() on the host fails.
#
# This is headroom, not the leak fix: the root cause of the 2026-06-11
# incidents is kitty's kitten config watcher recursively kqueue-watching
# /nix/store (issue #2198). Do not raise these values further to absorb
# that leak — fix #2198 instead.
#
# Values were proven good on m4mac on 2026-06-11: after a manual
# `sysctl -w kern.maxfiles=524288 kern.maxfilesperproc=262144`, a
# previously-failing `darwin-rebuild switch` completed cleanly.
# (Darwin defaults observed beforehand: kern.maxfiles=122880,
# kern.maxfilesperproc=61440.)
{ pkgs, lib, ... }:
let
  maxFiles = 524288;
  maxFilesPerProc = 262144;

  # Idempotent and never lowers: each sysctl is written only when the
  # running kernel value is below the target, so re-running (a second
  # switch, every boot) is a no-op once the values are in place, and a
  # kernel already at or above target is left alone.
  raiseFdSysctls = pkgs.writeShellScript "raise-fd-sysctls" ''
    set -eu

    raise() {
      key=$1
      target=$2
      current=$(/usr/sbin/sysctl -n "$key")
      if [ "$current" -lt "$target" ]; then
        /usr/sbin/sysctl -w "$key=$target"
      fi
    }

    # Order matters: kern.maxfilesperproc may not exceed kern.maxfiles,
    # so raise the system-wide cap first.
    raise kern.maxfiles ${toString maxFiles}
    raise kern.maxfilesperproc ${toString maxFilesPerProc}
  '';
in
{
  # Persistence across reboots. /etc/sysctl.conf is NOT read at boot on
  # modern macOS — launchd dropped that behaviour when it went
  # closed-source after 10.9 (the sysctl.conf(5) man page is stale
  # FreeBSD-inherited documentation, and the file does not exist on the
  # host). A launchd daemon running `sysctl -w` at boot (RunAtLoad, as
  # root) is the persistence mechanism instead.
  # /bin/wait4path guards against the daemon starting before the /nix
  # APFS volume is mounted at boot (the script's interpreter lives in
  # the store). Logs go to /var/log, not /tmp: a root daemon writing a
  # predictable /tmp path would be a symlink-attack hazard.
  launchd.daemons.fd-sysctls = {
    serviceConfig = {
      ProgramArguments = [
        "/bin/sh"
        "-c"
        "/bin/wait4path ${raiseFdSysctls} && exec ${raiseFdSysctls}"
      ];
      RunAtLoad = true;
      KeepAlive = false;
      StandardOutPath = "/var/log/fd-sysctls.log";
      StandardErrorPath = "/var/log/fd-sysctls.log";
    };
  };

  # Apply during `darwin-rebuild switch` itself so the first activation
  # needs no reboot. nix-darwin activation runs as root, which
  # `sysctl -w` requires.
  system.activationScripts.extraActivation.text = lib.mkAfter ''
    echo "ensuring kern.maxfiles >= ${toString maxFiles} and kern.maxfilesperproc >= ${toString maxFilesPerProc} (FD isolation Layer 2, #2191)..."
    ${raiseFdSysctls}
  '';
}
