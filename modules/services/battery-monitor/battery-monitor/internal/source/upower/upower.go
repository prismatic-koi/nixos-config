// Package upower implements a Source backed by org.freedesktop.UPower.
//
// We subscribe to PropertiesChanged on the laptop battery device on
// the system bus. UPower already owns the kernel-event handling, so
// we react to its signals rather than polling sysfs or using udev.
//
// On bus drop the source reconnects with exponential backoff (capped
// at 30s) and resumes signal handling without exiting Run — the
// daemon never has to restart its process.
package upower

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/prismatic-koi/battery-monitor/internal/state"
)

const (
	upowerBus              = "org.freedesktop.UPower"
	upowerPath             = "/org/freedesktop/UPower"
	upowerIface            = "org.freedesktop.UPower"
	upowerDeviceIface      = "org.freedesktop.UPower.Device"
	propsIface             = "org.freedesktop.DBus.Properties"
	displayDevicePath      = "/org/freedesktop/UPower/devices/DisplayDevice"
	enumerateDevicesMethod = "org.freedesktop.UPower.EnumerateDevices"

	// UPower device type: 2 = Battery, 1 = Line Power. We pick the
	// first Battery returned by EnumerateDevices in preference to
	// DisplayDevice, because DisplayDevice merges peripherals and
	// can mask the laptop battery on systems with multiple
	// batteries reported to UPower.
	deviceTypeBattery uint32 = 2

	// UPower state codes (org.freedesktop.UPower.Device.State).
	stateCharging        uint32 = 1
	stateDischarging     uint32 = 2
	stateEmpty           uint32 = 3
	stateFullyCharged    uint32 = 4
	statePendingCharge   uint32 = 5
	statePendingDischrg  uint32 = 6
	_                           = statePendingCharge
	_                           = statePendingDischrg
	_                           = stateEmpty
)

// UPower is a Source that watches the laptop battery via UPower.
type UPower struct {
	name string
	log  *slog.Logger
}

// New constructs a UPower source.
func New(name string, log *slog.Logger) *UPower {
	return &UPower{name: name, log: log}
}

// Name implements source.Source.
func (u *UPower) Name() string { return u.name }

// Run implements source.Source. It connects to the system bus, finds
// a battery device, subscribes to PropertiesChanged, and emits Samples
// on out. Closes out when it returns.
func (u *UPower) Run(ctx context.Context, out chan<- state.Sample) error {
	defer close(out)

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := u.runOnce(ctx, out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			// Normal exit (ctx cancelled inside runOnce).
			return nil
		}
		u.log.Warn("upower: bus connection lost; retrying",
			"device", u.name, "event", "upower_reconnect",
			"err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce establishes one connection and pumps signals until the
// connection drops or ctx is cancelled.
func (u *UPower) runOnce(ctx context.Context, out chan<- state.Sample) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("connect system bus: %w", err)
	}
	// SystemBus returns a shared connection; we cannot Close() it
	// without breaking other consumers in-process. We rely on
	// signal delivery to surface drops via a closed signal channel.

	devicePath, err := u.findBatteryPath(conn)
	if err != nil {
		return fmt.Errorf("find battery device: %w", err)
	}
	u.log.Info("upower: tracking device",
		"device", u.name, "event", "upower_attached",
		"path", string(devicePath))

	// Subscribe to PropertiesChanged on the battery device.
	matchRule := fmt.Sprintf(
		"type='signal',sender='%s',interface='%s',member='PropertiesChanged',path='%s'",
		upowerBus, propsIface, devicePath,
	)
	if call := conn.BusObject().Call(
		"org.freedesktop.DBus.AddMatch", 0, matchRule,
	); call.Err != nil {
		return fmt.Errorf("AddMatch: %w", call.Err)
	}
	// Best-effort removal on exit. If the bus connection is dead,
	// RemoveMatch will fail and we ignore it.
	defer conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, matchRule)

	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	// Emit an initial sample so the state machine sees the current
	// state on daemon startup (this is what lets the post-restart
	// "still in low → notify once" edge case work).
	if s, err := u.readSample(conn, devicePath); err == nil {
		select {
		case out <- s:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		u.log.Warn("upower: initial sample failed",
			"device", u.name, "event", "upower_initial_sample_failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sig, ok := <-signals:
			if !ok {
				// Channel closed by godbus — bus connection
				// went away. Surface as error so the outer
				// loop can reconnect with backoff.
				return fmt.Errorf("signal channel closed")
			}
			if sig == nil {
				continue
			}
			// We requested only PropertiesChanged on the
			// battery device, but be defensive.
			if sig.Name != propsIface+".PropertiesChanged" {
				continue
			}
			if sig.Path != devicePath {
				continue
			}
			// Re-read the canonical state via Properties.GetAll
			// rather than relying on the changed dict, because
			// UPower sometimes ships partial PropertiesChanged
			// updates (e.g. only Percentage) without the State
			// the state machine needs.
			s, err := u.readSample(conn, devicePath)
			if err != nil {
				u.log.Warn("upower: re-read failed",
					"device", u.name, "event", "upower_read_failed", "err", err)
				continue
			}
			select {
			case out <- s:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// findBatteryPath asks UPower for its devices and returns the first
// Battery. Falls back to DisplayDevice if EnumerateDevices yields
// nothing usable.
func (u *UPower) findBatteryPath(conn *dbus.Conn) (dbus.ObjectPath, error) {
	obj := conn.Object(upowerBus, upowerPath)
	var paths []dbus.ObjectPath
	if err := obj.Call(enumerateDevicesMethod, 0).Store(&paths); err != nil {
		return "", fmt.Errorf("EnumerateDevices: %w", err)
	}
	for _, p := range paths {
		dev := conn.Object(upowerBus, p)
		v, err := dev.GetProperty(upowerDeviceIface + ".Type")
		if err != nil {
			continue
		}
		t, ok := v.Value().(uint32)
		if !ok {
			continue
		}
		if t == deviceTypeBattery {
			return p, nil
		}
	}
	// Fallback — DisplayDevice is the aggregated battery view.
	return displayDevicePath, nil
}

// readSample reads Percentage + State from the device and maps them
// to a state.Sample. UPower's percentage is a float64 0..100.
func (u *UPower) readSample(conn *dbus.Conn, devicePath dbus.ObjectPath) (state.Sample, error) {
	dev := conn.Object(upowerBus, devicePath)
	pctV, err := dev.GetProperty(upowerDeviceIface + ".Percentage")
	if err != nil {
		return state.Sample{}, fmt.Errorf("Get Percentage: %w", err)
	}
	stV, err := dev.GetProperty(upowerDeviceIface + ".State")
	if err != nil {
		return state.Sample{}, fmt.Errorf("Get State: %w", err)
	}
	pct, ok := pctV.Value().(float64)
	if !ok {
		return state.Sample{}, fmt.Errorf("Percentage not float64: %T", pctV.Value())
	}
	st, ok := stV.Value().(uint32)
	if !ok {
		return state.Sample{}, fmt.Errorf("State not uint32: %T", stV.Value())
	}
	return state.Sample{
		Level:   int(pct + 0.5),
		Status:  mapUPowerState(st),
		Present: true,
	}, nil
}

// mapUPowerState translates UPower's State codes to our state.Status.
// "Empty", "PendingCharge", "PendingDischarge" are treated
// conservatively as Discharging — the state machine only acts on
// changes, so this is safe.
func mapUPowerState(st uint32) state.Status {
	switch st {
	case stateCharging:
		return state.StatusCharging
	case stateDischarging:
		return state.StatusDischarging
	case stateFullyCharged:
		return state.StatusFull
	default:
		return state.StatusDischarging
	}
}
