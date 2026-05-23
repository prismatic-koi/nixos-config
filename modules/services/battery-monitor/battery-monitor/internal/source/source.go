// Package source defines battery data Sources.
//
// A Source produces Samples on a channel. The daemon owns one Source
// per device and drains its channel concurrently. Implementations:
//
//   - upower.UPower — laptop battery via the system bus; pushes a
//     Sample whenever UPower fires PropertiesChanged on the battery
//     device, plus one initial Sample on startup. No polling.
//   - razer.Razer — Razer mouse via /sys/bus/hid/devices/*1532*/charge_*
//     on a 1-minute ticker, with optional openrazer D-Bus fallback.
//
// The Source interface lives here so the daemon can depend on it
// without importing the implementation packages (which pull in
// godbus). Tests pass their own Source implementations.
package source

import (
	"context"

	"github.com/prismatic-koi/battery-monitor/internal/state"
)

// Source produces Samples for one device.
type Source interface {
	// Name is the device name (matches the Nix attribute key and
	// the slog `device=` field).
	Name() string
	// Run begins emitting samples and blocks until ctx is cancelled
	// or an unrecoverable error occurs. It must close the channel
	// it sends on when it exits.
	//
	// The channel is owned by the Source so that Source-level
	// reconnect logic (e.g. UPower bus drop) does not require the
	// daemon to re-subscribe. The Source is responsible for its
	// own backoff.
	Run(ctx context.Context, out chan<- state.Sample) error
}
