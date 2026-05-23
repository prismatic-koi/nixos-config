// Package config defines the on-disk JSON configuration consumed by the
// battery-monitor daemon and emitted by the Nix module.
//
// The Nix module (modules/services/battery-monitor.nix) writes a single
// JSON file into the nix store and passes its path on the systemd
// ExecStart line as `--config <path>`. The daemon reads it once at
// startup; there is no live reload (a NixOS rebuild restarts the unit).
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// DeviceKind discriminates how a device's battery state is read.
type DeviceKind string

const (
	// KindLaptop reads from UPower over the system bus
	// (org.freedesktop.UPower / DisplayDevice or the named battery).
	KindLaptop DeviceKind = "laptop"
	// KindRazer reads from /sys/bus/hid/devices/*1532*/charge_level
	// (and charge_status) on a fixed interval, with an openrazer
	// D-Bus fallback if sysfs is unreadable.
	KindRazer DeviceKind = "razer"
)

// Device describes a single battery to monitor.
type Device struct {
	// Name is the human-readable identifier used in notifications and
	// in slog `device=<name>` fields. Matches the attribute name in
	// `nx.services.batteryMonitor.devices.<name>`.
	Name string `json:"name"`

	// Kind selects the data source (laptop UPower vs Razer sysfs).
	Kind DeviceKind `json:"kind"`

	// LowThreshold (0..100). At or below this level on a discharging
	// device, the low-battery notification fires.
	LowThreshold int `json:"lowThreshold"`

	// FullThreshold (0..100). At or above this level on a charging
	// device, the fully-charged notification fires (once per charge
	// session — see state machine for the Discharging→Charging reset).
	FullThreshold int `json:"fullThreshold"`

	// DismissThreshold (0..100). When a low notification is active
	// and the level rises to this value, the notification is closed
	// via CloseNotification.
	DismissThreshold int `json:"dismissThreshold"`

	// IgnoreZero, if true, suppresses readings of exactly 0% — useful
	// for devices (Razer mouse) that intermittently report 0 when
	// asleep. The reading is dropped before the state machine sees it.
	IgnoreZero bool `json:"ignoreZero"`
}

// Config is the top-level configuration shape.
type Config struct {
	Devices []Device `json:"devices"`
}

// Load reads and parses a Config from the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks invariants the daemon relies on.
func (c *Config) Validate() error {
	seen := map[string]bool{}
	for i, d := range c.Devices {
		if d.Name == "" {
			return fmt.Errorf("devices[%d]: name is empty", i)
		}
		if seen[d.Name] {
			return fmt.Errorf("devices[%d]: duplicate name %q", i, d.Name)
		}
		seen[d.Name] = true
		switch d.Kind {
		case KindLaptop, KindRazer:
		default:
			return fmt.Errorf("devices[%d] (%s): unknown kind %q", i, d.Name, d.Kind)
		}
		if d.LowThreshold < 0 || d.LowThreshold > 100 {
			return fmt.Errorf("devices[%d] (%s): lowThreshold %d out of range", i, d.Name, d.LowThreshold)
		}
		if d.FullThreshold < 0 || d.FullThreshold > 100 {
			return fmt.Errorf("devices[%d] (%s): fullThreshold %d out of range", i, d.Name, d.FullThreshold)
		}
		if d.DismissThreshold < 0 || d.DismissThreshold > 100 {
			return fmt.Errorf("devices[%d] (%s): dismissThreshold %d out of range", i, d.Name, d.DismissThreshold)
		}
		if d.DismissThreshold <= d.LowThreshold {
			return fmt.Errorf("devices[%d] (%s): dismissThreshold (%d) must exceed lowThreshold (%d)", i, d.Name, d.DismissThreshold, d.LowThreshold)
		}
	}
	return nil
}
