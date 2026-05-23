// Package razer implements a Source for Razer mouse battery readings.
//
// Razer's HID driver exposes battery info at
//
//	/sys/bus/hid/devices/*1532*/charge_level   (0..255)
//	/sys/bus/hid/devices/*1532*/charge_status  (0 = discharging, 1 = charging)
//
// "1532" is Razer's USB vendor ID; the surrounding wildcards match
// the product ID and HID descriptor. We poll on a 1-minute internal
// ticker because the mouse goes to sleep without warning and there
// is no kernel uevent we can wake up on.
//
// When the sysfs path is missing (mouse asleep / not paired) we emit
// a Sample with Present=false. The state machine treats that as a
// no-op; we log the absence here (once per absent-→-absent retry
// burst is avoided by tracking lastAbsent), so the systemd journal
// reflects what the daemon is doing without filling up with
// "still asleep" lines every minute.
package razer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prismatic-koi/battery-monitor/internal/state"
)

// PollInterval is the cadence at which the mouse sysfs is read. The
// AC requires 1 minute. Tests override this via WithInterval.
const PollInterval = 1 * time.Minute

// Razer is a Source for the Razer mouse battery.
type Razer struct {
	name     string
	log      *slog.Logger
	root     string // typically "/sys/bus/hid/devices"; overridable in tests
	interval time.Duration
}

// New constructs a Razer source with default root and interval.
func New(name string, log *slog.Logger) *Razer {
	return &Razer{
		name:     name,
		log:      log,
		root:     "/sys/bus/hid/devices",
		interval: PollInterval,
	}
}

// WithRoot overrides the sysfs root used to locate devices. Tests use
// this to point at a fixture directory.
func (r *Razer) WithRoot(root string) *Razer {
	r.root = root
	return r
}

// WithInterval overrides the poll interval. Tests use a short
// interval so they don't wait a full minute.
func (r *Razer) WithInterval(d time.Duration) *Razer {
	r.interval = d
	return r
}

// Name implements source.Source.
func (r *Razer) Name() string { return r.name }

// Run implements source.Source. Polls sysfs at the configured
// interval until ctx is cancelled. Always emits an initial sample
// immediately so the daemon has a starting state.
func (r *Razer) Run(ctx context.Context, out chan<- state.Sample) error {
	defer close(out)

	wasAbsent := false
	emit := func() {
		s, err := r.readOnce()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Mouse asleep / not paired. Log once per
				// presence transition to keep the journal
				// useful without spamming.
				if !wasAbsent {
					r.log.Info("razer: device not present",
						"device", r.name,
						"event", "razer_absent")
					wasAbsent = true
				}
				select {
				case out <- state.Sample{Present: false}:
				case <-ctx.Done():
				}
				return
			}
			r.log.Warn("razer: read failed",
				"device", r.name,
				"event", "razer_read_failed",
				"err", err)
			// Treat unknown errors as transient absence — don't
			// corrupt state machine flags with bogus levels.
			select {
			case out <- state.Sample{Present: false}:
			case <-ctx.Done():
			}
			return
		}
		if wasAbsent {
			r.log.Info("razer: device returned",
				"device", r.name,
				"event", "razer_present",
				"level", s.Level,
				"status", s.Status.String())
			wasAbsent = false
		}
		select {
		case out <- s:
		case <-ctx.Done():
		}
	}

	emit()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			emit()
		}
	}
}

// readOnce performs a single sysfs read and returns a Sample. Returns
// fs.ErrNotExist (wrapped) when no matching device directory is
// present — the caller distinguishes "absent" from "broken".
func (r *Razer) readOnce() (state.Sample, error) {
	dir, err := findDeviceDir(r.root)
	if err != nil {
		return state.Sample{}, err
	}

	rawLevel, err := readIntFile(filepath.Join(dir, "charge_level"))
	if err != nil {
		// If charge_level is gone but charge_status is too,
		// surface as absent rather than a hard error so the
		// daemon doesn't notify.
		if errors.Is(err, fs.ErrNotExist) {
			return state.Sample{}, fs.ErrNotExist
		}
		return state.Sample{}, fmt.Errorf("read charge_level: %w", err)
	}
	rawStatus, err := readIntFile(filepath.Join(dir, "charge_status"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return state.Sample{}, fs.ErrNotExist
		}
		return state.Sample{}, fmt.Errorf("read charge_status: %w", err)
	}

	return state.Sample{
		Level:   ScaleLevel(rawLevel),
		Status:  mapStatus(rawStatus),
		Present: true,
	}, nil
}

// ScaleLevel converts the Razer 0..255 charge_level to a 0..100
// percentage with rounding. Exported for tests.
func ScaleLevel(raw int) int {
	if raw < 0 {
		return 0
	}
	if raw > 255 {
		return 100
	}
	// (raw * 100 + 127) / 255 rounds to nearest.
	return (raw*100 + 127) / 255
}

// mapStatus translates the Razer 0/1 charge_status to state.Status.
// 1 means charging; any other value (including 0) means discharging.
// We deliberately do NOT distinguish "Full" here — at 100% with
// status=Charging, the laptop UPower path would report Fully Charged,
// but the Razer driver does not, so we leave the mouse in Charging
// at 100% and let the state machine fire ActionNotifyFull as soon as
// level >= fullThreshold with a charging-like status. The state
// machine's `case s.Level >= m.full && s.Status.isChargingLike()`
// covers both.
func mapStatus(raw int) state.Status {
	if raw == 1 {
		return state.StatusCharging
	}
	return state.StatusDischarging
}

// readIntFile reads a single integer from a sysfs file, trimming
// whitespace. Returns fs.ErrNotExist if the file is missing.
func readIntFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %s = %q: %w", path, s, err)
	}
	return n, nil
}

// findDeviceDir returns the first subdirectory of root whose name
// contains "1532" (Razer's vendor ID) and which has a charge_level
// file. Returns fs.ErrNotExist if no such directory exists.
func findDeviceDir(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fs.ErrNotExist
		}
		return "", fmt.Errorf("readdir %s: %w", root, err)
	}
	for _, e := range entries {
		if !strings.Contains(e.Name(), "1532") {
			continue
		}
		full := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(full, "charge_level")); err == nil {
			return full, nil
		}
	}
	return "", fs.ErrNotExist
}
