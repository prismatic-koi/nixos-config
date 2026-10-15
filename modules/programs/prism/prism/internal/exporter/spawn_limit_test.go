package exporter_test

import (
	"bytes"
	"log"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

func spawnLimitHarness(t *testing.T, spawnLimit func() int) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:         t,
		dir:       dir,
		dbPath:    filepath.Join(dir, "prism.db"),
		statePath: filepath.Join(dir, "exporter-state.json"),
		usageDir:  filepath.Join(dir, "usage"),
		logBuf:    &bytes.Buffer{},
	}
	h.writeDB = sidecartest.OpenDB(t, h.dbPath)
	t.Cleanup(func() { _ = h.writeDB.Close() })
	e, err := exporter.New(exporter.Config{
		DBPath:     h.dbPath,
		StatePath:  h.statePath,
		UsageDir:   h.usageDir,
		ListenAddr: "127.0.0.1:0",
		Logger:     log.New(h.logBuf, "", 0),
		SpawnLimit: spawnLimit,
	})
	if err != nil {
		t.Fatalf("exporter.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	h.exp = e
	h.start(e)
	return h
}

// prism_spawn_limit is one unlabelled gauge with the value of the setting,
// read again on every scrape.
func TestExporter_SpawnLimitGauge_ReportsTheSettingOnEveryScrape(t *testing.T) {
	var limit atomic.Int64
	limit.Store(120)
	h := spawnLimitHarness(t, func() int { return int(limit.Load()) })

	family := h.scrape(h.exp).Family(t, exporter.MetricSpawnLimit)
	if family.Type != "gauge" {
		t.Errorf("%s is a %s, want gauge", exporter.MetricSpawnLimit, family.Type)
	}
	if family.Help == "" {
		t.Errorf("%s has no HELP text", exporter.MetricSpawnLimit)
	}
	if len(family.Samples) != 1 || len(family.Samples[0].Labels) != 0 {
		t.Fatalf("%s samples = %+v, want exactly one sample with no labels", exporter.MetricSpawnLimit, family.Samples)
	}
	if got := family.Samples[0].Value; got != 120 {
		t.Errorf("%s = %v, want 120", exporter.MetricSpawnLimit, got)
	}

	limit.Store(90)
	if got, _ := h.scrape(h.exp).Value(exporter.MetricSpawnLimit, nil); got != 90 {
		t.Errorf("%s = %v after the setting changed to 90, want 90", exporter.MetricSpawnLimit, got)
	}
}

func TestExporter_SpawnLimitGauge_DefaultsTo120(t *testing.T) {
	h := spawnLimitHarness(t, nil)
	got, ok := h.scrape(h.exp).Value(exporter.MetricSpawnLimit, nil)
	if !ok || got != float64(config.DefaultSpawnBudgetLimit) {
		t.Errorf("%s = %v (present %v), want %d", exporter.MetricSpawnLimit, got, ok, config.DefaultSpawnBudgetLimit)
	}
}
