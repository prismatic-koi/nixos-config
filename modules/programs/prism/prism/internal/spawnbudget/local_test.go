package spawnbudget_test

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/metrics/metricstest"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

// spawnFixture writes one sessions row with role and repo, and one event of
// eventType for it, age before now. It returns the instance ID.
func spawnFixture(t *testing.T, d *db.DB, role, repo, eventType string, age time.Duration) string {
	t.Helper()
	id := uuid.New().String()
	name := "prism-test@" + id[:8]
	r := role
	if err := d.InsertSession(db.Session{
		InstanceID:  id,
		SessionName: name,
		AgentRole:   &r,
		Repo:        repo,
		Worktree:    "/tmp/" + name,
		StartedAt:   time.Now().Add(-age),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: name,
		Repo:        repo,
		Worktree:    "/tmp/" + name,
		InstanceID:  &id,
		Type:        eventType,
		Payload:     `{}`,
		CreatedAt:   time.Now().Add(-age),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	return id
}

// The local count holds worker spawns of every repo inside 168 hours, and
// nothing else.
func TestLocalCounter_CountsWorkerSpawnsOfAllReposInsideTheWindow(t *testing.T) {
	d := sidecartest.OpenDB(t, filepath.Join(t.TempDir(), "prism.db"))
	t.Cleanup(func() { _ = d.Close() })
	spawn := session.EventSpawnIntent

	// Counted: workers in three repos, inside the window.
	spawnFixture(t, d, "worker", "nixos-config", spawn, time.Minute)
	spawnFixture(t, d, "worker", "nixos-config", spawn, 3*24*time.Hour)
	spawnFixture(t, d, "worker", "home-ops", spawn, time.Hour)
	spawnFixture(t, d, "worker", "work-repo", spawn, 167*time.Hour)

	// Not counted: other roles.
	spawnFixture(t, d, "review-code", "nixos-config", spawn, time.Minute)
	spawnFixture(t, d, "coordinator", "nixos-config", spawn, time.Minute)
	spawnFixture(t, d, "investigate", "nixos-config", spawn, time.Minute)
	// Not counted: older than 168 hours.
	spawnFixture(t, d, "worker", "nixos-config", spawn, 169*time.Hour)
	// Not counted: a worker event that is not a spawn.
	spawnFixture(t, d, "worker", "nixos-config", session.EventSpawnFailed, time.Minute)

	got, err := spawnbudget.LocalCounter(d)(context.Background(), time.Now().Add(-spawnbudget.Window))
	if err != nil {
		t.Fatalf("local count: %v", err)
	}
	if got != 4 {
		t.Errorf("local count = %d, want 4", got)
	}
}

// The local count and the exporter's prism_spawns_total{agent_role="worker"}
// count the same rows. If either side changes the event or the role join,
// this test fails.
func TestLocalCounter_AgreesWithExporterSpawnsTotal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "prism.db")
	d := sidecartest.OpenDB(t, dbPath)
	t.Cleanup(func() { _ = d.Close() })

	e, err := exporter.New(exporter.Config{
		DBPath:     dbPath,
		StatePath:  filepath.Join(dir, "exporter-state.json"),
		UsageDir:   filepath.Join(dir, "usage"),
		ListenAddr: "127.0.0.1:0",
		Logger:     log.New(&bytes.Buffer{}, "", 0),
	})
	if err != nil {
		t.Fatalf("exporter.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The exporter counts rows written after Start, so every fixture is
	// written now and inside the window.
	spawn := session.EventSpawnIntent
	spawnFixture(t, d, "worker", "nixos-config", spawn, time.Minute)
	spawnFixture(t, d, "worker", "home-ops", spawn, time.Minute)
	spawnFixture(t, d, "worker", "home-ops", spawn, time.Hour)
	spawnFixture(t, d, "review-goal", "home-ops", spawn, time.Minute)
	spawnFixture(t, d, "coordinator", "home-ops", spawn, time.Minute)
	spawnFixture(t, d, "worker", "home-ops", session.EventSpawnFailed, time.Minute)

	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + exporter.MetricsPath)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	exp := metricstest.MustParse(t, string(body))

	var exported float64
	for _, s := range exp.Family(t, exporter.MetricSpawnsTotal).Samples {
		if s.Labels["agent_role"] == spawnbudget.WorkerRole {
			exported += s.Value
		}
	}

	local, err := spawnbudget.LocalCounter(d)(context.Background(), time.Now().Add(-spawnbudget.Window))
	if err != nil {
		t.Fatalf("local count: %v", err)
	}
	if local != 3 || exported != 3 {
		t.Errorf("local count = %d, exporter worker spawns = %v, want 3 and 3", local, exported)
	}
}
