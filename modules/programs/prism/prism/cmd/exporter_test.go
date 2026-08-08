package cmd

import (
	"strconv"
	"testing"

	"github.com/prismatic-koi/prism/internal/exporter"
)

func TestExporterCmd_IsRegisteredAndVisible(t *testing.T) {
	var found bool
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "exporter" {
			found = true
			if sub.Hidden {
				t.Error("prism exporter is hidden; it is an operator-facing daemon command")
			}
			if sub.Short == "" {
				t.Error("prism exporter has no short description")
			}
		}
	}
	if !found {
		t.Fatal("prism exporter is not registered on the root command")
	}
}

// The flag defaults are what a systemd unit inherits when the NixOS module
// passes nothing. They must match the package constants #2701 points at.
func TestExporterCmd_FlagDefaultsMatchThePackageConstants(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want string
	}{
		{"listen", exporter.DefaultListenHost},
		{"port", strconv.Itoa(exporter.DefaultPort)},
		{"db", ""},
		{"state", ""},
	} {
		f := exporterCmd.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("--%s is not defined", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}
}

// An out-of-range port must be rejected before anything is opened or bound,
// so the check has to come first in RunE.
func TestExporterCmd_RejectsAnOutOfRangePort(t *testing.T) {
	originalPort := exporterFlagPort
	t.Cleanup(func() { exporterFlagPort = originalPort })

	for _, port := range []int{-1, 65536, 1 << 20} {
		exporterFlagPort = port
		if err := runExporter(exporterCmd, nil); err == nil {
			t.Errorf("--port %d was accepted, want an error", port)
		}
	}
}

// A daemon told to read a database that is not there must fail with a clear
// error rather than serve an empty exposition forever.
func TestExporterCmd_FailsClearlyWhenTheDatabaseIsAbsent(t *testing.T) {
	originalDB, originalState, originalPort := exporterFlagDBPath, exporterFlagStatePath, exporterFlagPort
	t.Cleanup(func() {
		exporterFlagDBPath, exporterFlagStatePath, exporterFlagPort = originalDB, originalState, originalPort
	})

	dir := t.TempDir()
	exporterFlagDBPath = dir + "/absent.db"
	exporterFlagStatePath = dir + "/exporter-state.json"
	exporterFlagPort = 0

	err := runExporter(exporterCmd, nil)
	if err == nil {
		t.Fatal("runExporter succeeded with no database present")
	}
}
