package exporter

// AC: "The exporter opens prism.db read-only, and no code path issues a
// write."
//
// This file lives inside package exporter so it can reach the daemon's own
// *sql.DB handle. An external test could only prove that a handle it opened
// itself is read-only, which proves nothing about the one the daemon uses.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

func newInternalTestExporter(t *testing.T) (*Exporter, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "prism.db")
	writeDB := sidecartest.OpenDB(t, dbPath)
	t.Cleanup(func() { _ = writeDB.Close() })

	e, err := New(Config{
		DBPath:     dbPath,
		StatePath:  filepath.Join(dir, "exporter-state.json"),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, writeDB
}

// Every write shape SQLite offers must be refused by the engine itself, on
// the exporter's own connection.
func TestExporterConnection_RefusesEveryWrite(t *testing.T) {
	e, writeDB := newInternalTestExporter(t)

	// A row must exist so an UPDATE or DELETE has something to target;
	// otherwise SQLite could return success without ever attempting a
	// write and the test would pass vacuously.
	if err := writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "prism-test@exporter",
		Repo:        "nixos-config",
		Worktree:    "/tmp/prism-test",
		Type:        "tool_call",
		Payload:     "{}",
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"insert", `INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at) VALUES ('x','s','r','w','t','{}',1)`},
		{"update", `UPDATE agent_events SET type = 'tampered'`},
		{"delete", `DELETE FROM agent_events`},
		{"create table", `CREATE TABLE exporter_scratch (a INTEGER)`},
		{"drop table", `DROP TABLE agent_events`},
		{"alter table", `ALTER TABLE agent_events ADD COLUMN injected TEXT`},
		{"create index", `CREATE INDEX idx_exporter_scratch ON agent_events(type)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.conn.ExecContext(context.Background(), tc.stmt)
			if err == nil {
				t.Fatalf("the exporter connection accepted a write: %s", tc.stmt)
			}
			if !db.IsReadOnlyError(err) {
				t.Fatalf("write was rejected, but not as a read-only violation: %v", err)
			}
		})
	}

	// The seed row is untouched.
	var count int64
	if err := writeDB.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("agent_events has %d rows after the write attempts, want 1", count)
	}
}

// The read half must still work, or the negative test above would pass for
// the wrong reason (a broken connection rather than a read-only one).
func TestExporterConnection_CanStillRead(t *testing.T) {
	e, writeDB := newInternalTestExporter(t)
	if err := writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "prism-test@exporter",
		Repo:        "nixos-config",
		Worktree:    "/tmp/prism-test",
		Type:        "tool_call",
		Payload:     "{}",
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	src := agentEventSource{conn: e.conn}
	maxID, err := src.MaxID(context.Background())
	if err != nil {
		t.Fatalf("MaxID: %v", err)
	}
	if maxID == 0 {
		t.Fatal("MaxID = 0 with a row present; the read path is broken")
	}
	records, err := src.Records(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 || records[0].Value != "tool_call" {
		t.Fatalf("Records = %+v, want one tool_call record", records)
	}
}

// A read-only connection must not be able to start a write transaction
// either, which is the shape a future contributor would most plausibly reach
// for.
func TestExporterConnection_RefusesAWriteInsideATransaction(t *testing.T) {
	e, _ := newInternalTestExporter(t)

	tx, err := e.conn.BeginTx(context.Background(), nil)
	if err != nil {
		// Refusing to begin at all is an acceptable outcome.
		return
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM agent_events`); err == nil {
		t.Fatal("a write inside a transaction on the exporter connection succeeded")
	} else if !db.IsReadOnlyError(err) {
		t.Fatalf("write in a transaction was rejected, but not as a read-only violation: %v", err)
	}
}
