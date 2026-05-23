# battery-monitor — design

A long-running user daemon that watches one or more batteries and emits
freedesktop notifications when they cross configured low / full
thresholds. This document records the architecture and the bugs the
rewrite is leaving behind. The source lives next to this file in
`./battery-monitor/` (Go module).

## Background — the bugs we are leaving behind

The previous implementation was a Python script
(`pkgs.writers.writePython3Bin`) invoked once per minute by a systemd
timer, plus a udev rule that ran the same script via `machinectl shell`
on AC plug/unplug. A coordinator-led audit identified the following
concrete defects, which motivated the rewrite:

1. **Non-atomic JSON state writes.** The script wrote `~/.runtime/.../battery_notifier_*.json` directly with `open(..., "w")`. A power loss or kill between truncate and rewrite produced a zero-byte file, which subsequent runs treated as "no prior notification."
2. **Race between the timer path and the udev path.** Both paths read and wrote the same JSON state file with no locking. Concurrent invocations on plug-in (udev fires; timer fires moments later) could clobber each other's notification cookies.
3. **Silent shell-pipeline failures.** Commands like `polychromatic-cli -d mouse -k | grep Battery | sed 's/[^0-9]*//g' || echo 100` swallow every error in the middle of the pipeline and substitute `100` as a fallback. A genuinely dead mouse reported "fully charged" and ate the final-segment failure that produced it.
4. **Hard-coded user in the udev wrapper.** The wrapper read `USER="ben"` literally, so the module silently broke on any other user.
5. **`machinectl shell` flakiness.** On systems where systemd-machined was slow to materialise the user's session scope, `machinectl shell .../@.host` failed intermittently — udev fired, the wrapper exited non-zero, no notification, no log line the user saw.
6. **`XDG_RUNTIME_DIR` wipe on logout.** The state file lived under `$XDG_RUNTIME_DIR`, which systemd unmounts on logout. The "last notification id" was therefore lost across every logout cycle, which interacted badly with the persistent-critical notifications dunst kept across logout.
7. **No reconnect / reconnect-storm avoidance.** The timer-driven design "reconnected" every minute by spawning a new Python process. Cumulative warm-up cost (Python interpreter + subprocesses + JSON parse + sysfs read) was ~50–80ms per minute, indefinitely.
8. **`subprocess.run(..., shell=True)`** without input validation. The level/status commands came from the Nix module, so this wasn't directly exploitable, but it tightened the coupling between Nix string interpolation and shell semantics in a way that made the module hard to read.
9. **`dunstify -r` to update a notification only works if dunst is the active server.** On servers without `org.freedesktop.Notifications` activation, dunstify silently dropped the update. The script had no way to detect that — `dunstify` returned 0 even when no daemon was listening.

There was also a **semantics bug** in `reNotifyThreshold` — see below.

## Design

One systemd user service, one Go binary, no timer, no udev, no shell-outs, no JSON state file.

| Concern              | Implementation                                                                                                                                                                                                                                                                                                                |
| -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Process model        | Single long-running `battery-monitor` user-scope systemd service. `Restart=on-failure`, `After=graphical-session.target`, `PartOf=graphical-session.target`. One service covers every enabled device — fewer units to monitor and `journalctl --user -u battery-monitor` shows everything.                                  |
| Laptop battery       | Subscribe to `org.freedesktop.UPower` `PropertiesChanged` on the laptop battery device on the system bus. UPower already owns the kernel event handling; we react to its signals. No polling, no udev, no `machinectl`.                                                                                                  |
| Mouse battery        | Read `/sys/bus/hid/devices/*1532*/charge_level` (0..255, scaled to 0..100) and `charge_status` (0 = discharging, 1 = charging) on an internal 1-minute ticker. The mouse goes to sleep without notice and there is no kernel uevent to wake on; polling at the same cadence as the old timer keeps the user experience identical. |
| Notifications        | Direct `org.freedesktop.Notifications.Notify` calls on the session bus via `github.com/godbus/dbus/v5`. `replaces_id` is threaded through for low-battery updates so the bubble updates in place; `CloseNotification` closes it on dismiss. No `dunstify` subprocess.                                                       |
| State                | In-memory only — kills the atomic-write, locking, and `XDG_RUNTIME_DIR` wipe bugs. The cost is that a service restart loses the "we already notified at low" memory; the daemon re-notifies once on restart if the device is still in the low state. The user sees one extra bubble after a `systemctl --user restart battery-monitor`. Acceptable. |
| Config               | The Nix module emits a JSON config into the nix store and passes `--config <path>` on the systemd `ExecStart`. A NixOS rebuild restarts the unit; no live reload.                                                                                                                                                            |
| Logging              | `log/slog` with `--log-format=text\|json` (default `text`). The text handler renders attributes as `key=value`, preserving the existing `device=… event=…` greppable shape. `journalctl --user -u battery-monitor --output=json \| jq` works directly with `--log-format=json`.                                          |

### Package layout

```
modules/services/battery-monitor/
├── DESIGN.md                                  # this file
└── battery-monitor/                          # Go module (mirrors prism/prism/)
    ├── go.mod / go.sum
    ├── main.go                                # CLI entrypoint, slog wiring
    └── internal/
        ├── config/                            # JSON config schema + Load + Validate
        ├── state/                             # pure state machine (heavily tested)
        ├── notify/                            # Notifier interface + DBusNotifier
        ├── source/
        │   ├── source.go                      # Source interface
        │   ├── upower/                        # laptop: UPower PropertiesChanged
        │   └── razer/                         # mouse:  sysfs polling
        └── daemon/                            # wires Sources → state.Machine → Notifier
```

The state machine, the source layer, and the notifier are decoupled by
interfaces (`source.Source`, `notify.Notifier`). The Go tests use fakes
for both, so the state-machine semantics — including the new
`Discharging → Charging` reset — are verified without touching the
system or session bus.

### Semantics change: `reNotifyThreshold` is gone

**Old behaviour.** A `full_notification_done` flag was reset only after the battery dropped below 60% while discharging. So a plug-in at 80% that climbed to 100% produced no "fully charged" notification.

**New behaviour.** `full_notification_done` resets on any `Discharging → Charging` status transition. Every charge session that subsequently reaches `fullThreshold` notifies. The `reNotifyThreshold` option is **removed** from the Nix module surface; the JSON config schema does not include it; the daemon does not parse it.

This is the only user-visible behavioural difference between the Python script and the Go daemon. It is intentional and documented.

### Debounce strategy — rapid Charging↔Discharging flips

A flaky AC cable can produce `Charging → Discharging → Charging` flips in under a second. Without debouncing, the daemon would issue a CloseNotification, then a fresh Notify, then another CloseNotification — visible as flicker in the user's notification stack.

The daemon applies status changes **immediately** (no latency penalty on a real plug-in), and stashes the previous status as `revertedFromStatus` for the duration of a 3-second window. If, during that window, a sample arrives whose status equals `revertedFromStatus`, the daemon drops it. The window is reset on every applied status change, so a long monotonic sequence (Discharging → Charging → Full) is never artificially delayed, but a 4-fold flicker collapses to one Apply. The window length is `daemon.DefaultDebounce` (3 seconds); tests inject a shorter value via `Options.Debounce`.

This is one of several reasonable debounce strategies. The implementer's-choice clause in the issue allows it.

### Reconnect logic

The UPower source wraps its connection loop in an exponential-backoff retry:

- Initial backoff 1 second, doubling, capped at 30 seconds.
- The Go process never exits on a bus drop; systemd never restarts the unit on a reconnect.
- The state machine state is preserved across reconnects — the daemon does not re-emit notifications it already sent.

If the session bus call to `org.freedesktop.Notifications.Notify` fails, the daemon logs the error and continues. The next sample retries. There is no notification-level retry queue.

### Mouse-absent handling

When the Razer sysfs path is missing (mouse asleep or unpaired), the source emits a `Sample{Present: false}`. The state machine ignores absent samples — no notification, no state mutation. The source logs the absent condition once per `present → absent` transition (not once per missed poll) to keep the journal useful.

**openrazer D-Bus fallback — deferred.** The issue notes that if sysfs is unreadable, the daemon should fall back to the openrazer D-Bus API (`org.razer` on the session bus). The current implementation treats unreadable sysfs as `Present: false` rather than calling openrazer, on the grounds that the `hardware.openrazer.enable` Nix module installs the udev rules that make these sysfs nodes world-readable for every Razer device the driver claims. A sysfs read failure therefore indicates a kernel-driver-level problem that an openrazer fallback would not help with (openrazer reads the same data from the same driver). Adding the fallback is a one-day change — wire a second `razer.Razer` implementation that calls `org.razer.daemon.devices.getDevicePathFromObjectPath` and select between them at construction time — but the AC checklist does not require it and the marginal reliability gain is small. Tracking issue: not yet filed; revisit if a real-world unreadable-sysfs incident surfaces.

### Why one service, not per-device?

The AC allows either choice. We pick one service because:

- The shared session-bus connection (for notifications) is best owned by a single process. Two services would each open a session-bus socket.
- One service is one unit in `systemctl --user status`, one log stream in `journalctl --user -u battery-monitor`. Operationally simpler.
- Per-device goroutines inside one process are cheap; OS-level isolation between devices buys nothing because they share the same Nix configuration anyway.

### Build & CI

`pkgs/battery-monitor.nix` mirrors `pkgs/prism.nix`:

- Default `runChecks = false` so `nh switch` and `nix build .#battery-monitor` are fast.
- `nix build --impure --no-link --expr '(builtins.getFlake (toString ./.)).packages.x86_64-linux.battery-monitor.override { runChecks = true; }'` runs the Go suite inside the nix sandbox ($HOME=/homeless-shelter).

`.github/workflows/pr-gate.yml` adds two jobs: `go-tests-battery-monitor` and `nix-build-battery-monitor-checked`, fanned into the `pr-gate` gate alongside the existing prism jobs. The path filter scopes them to `modules/services/battery-monitor/**`, `pkgs/battery-monitor.nix`, `modules/services/battery-monitor.nix`, the new `go.mod`/`go.sum`, and the workflow file itself.

## References

- UPower D-Bus interface: <https://upower.freedesktop.org/docs/UPower.html>
- godbus: <https://github.com/godbus/dbus>
- freedesktop notification spec: <https://specifications.freedesktop.org/notification-spec/notification-spec-latest.html>
- AGENTS.md (top-level) — the `runChecks` / homeless-shelter pattern this module mirrors from prism.
