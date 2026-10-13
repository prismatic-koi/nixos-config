package db

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// stubInstalledPrismProbe replaces probeInstalledPrism for the test, so that
// Open never runs the prism on the PATH of the test host.
func stubInstalledPrismProbe(t *testing.T, bins prismBinaries, ok bool) {
	t.Helper()
	prev := probeInstalledPrism
	probeInstalledPrism = func() (prismBinaries, bool) { return bins, ok }
	t.Cleanup(func() { probeInstalledPrism = prev })
}

// captureMigrationCaution sends the live-session caution to a buffer for the
// test.
func captureMigrationCaution(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := migrationCautionOut
	migrationCautionOut = &buf
	t.Cleanup(func() { migrationCautionOut = prev })
	return &buf
}

// openTooNew opens a database one schema version ahead of this binary and
// returns the *SchemaTooNewError from Open.
func openTooNew(t *testing.T) *SchemaTooNewError {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "too-new.db")
	seedRawDBWithVersion(t, dbPath, currentSchemaVersion+1)
	_, err := Open(dbPath)
	if err == nil {
		t.Fatal("Open: want an error for a newer schema version, got nil")
	}
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("Open error is %T, want *SchemaTooNewError: %v", err, err)
	}
	return tooNew
}

func assertContainsAll(t *testing.T, msg string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not contain %q:\n%s", want, msg)
		}
	}
}

func assertContainsNone(t *testing.T, msg string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(msg, u) {
			t.Errorf("message contains %q, which names a cause the process did not detect:\n%s", u, msg)
		}
	}
}

const (
	updatedCauseText   = "rism was updated while this session ran"
	olderInstalledText = "older prism"
	restartFixText     = "run `prism restart`"
)

// TestOpen_SchemaTooNew_UpdatedWhileRunning: the prism on PATH supports the
// database version, so the error names the update and the restart.
func TestOpen_SchemaTooNew_UpdatedWhileRunning(t *testing.T) {
	dbVersion := currentSchemaVersion + 1
	stubInstalledPrismProbe(t, prismBinaries{
		running:      "/nix/store/old-prism/bin/prism",
		installed:    "/nix/store/new-prism/bin/prism",
		installedMax: dbVersion,
	}, true)

	tooNew := openTooNew(t)
	if tooNew.Cause != SkewCauseUpdatedWhileRunning {
		t.Fatalf("Cause = %d, want SkewCauseUpdatedWhileRunning", tooNew.Cause)
	}
	msg := tooNew.Error()
	assertContainsAll(t, msg,
		fmt.Sprintf("db schema version %d", dbVersion),
		fmt.Sprintf("(max %d)", currentSchemaVersion),
		"Cause: prism was updated while this session ran",
		"restart the session",
		restartFixText,
		"/nix/store/old-prism/bin/prism",
		"/nix/store/new-prism/bin/prism",
	)
	assertContainsNone(t, msg, olderInstalledText, "newer database was restored", "cannot tell")
}

// TestOpen_SchemaTooNew_CauseUnknown: the process cannot detect the cause,
// so the error names both causes and the fix for each.
func TestOpen_SchemaTooNew_CauseUnknown(t *testing.T) {
	dbVersion := currentSchemaVersion + 1
	stubInstalledPrismProbe(t, prismBinaries{}, false)

	tooNew := openTooNew(t)
	if tooNew.Cause != SkewCauseUnknown {
		t.Fatalf("Cause = %d, want SkewCauseUnknown", tooNew.Cause)
	}
	msg := tooNew.Error()
	assertContainsAll(t, msg,
		fmt.Sprintf("db schema version %d", dbVersion),
		fmt.Sprintf("(max %d)", currentSchemaVersion),
		"cannot tell which of these two causes applies",
		"1. Prism was updated while this session ran.",
		restartFixText,
		"2. An older prism is installed, or a newer database was restored.",
		fmt.Sprintf("install a prism that supports schema version %d", dbVersion),
		"restore a database copy",
	)
	if strings.Contains(msg, "Cause:") {
		t.Errorf("message names one cause, but the process detected none:\n%s", msg)
	}
}

// TestOpen_SchemaTooNew_InstalledTooOld: this process runs the installed
// prism, so a restart cannot correct the error, and the error must not say
// that prism was updated while the session ran.
func TestOpen_SchemaTooNew_InstalledTooOld(t *testing.T) {
	dbVersion := currentSchemaVersion + 1
	stubInstalledPrismProbe(t, prismBinaries{
		running:      "/nix/store/same-prism/bin/prism",
		installed:    "/nix/store/same-prism/bin/prism",
		installedMax: currentSchemaVersion,
	}, true)

	tooNew := openTooNew(t)
	if tooNew.Cause != SkewCauseInstalledTooOld {
		t.Fatalf("Cause = %d, want SkewCauseInstalledTooOld", tooNew.Cause)
	}
	msg := tooNew.Error()
	assertContainsAll(t, msg,
		"Cause: the installed prism is older than the database",
		"This process runs the installed prism, /nix/store/same-prism/bin/prism.",
		fmt.Sprintf("install a prism that supports schema version %d", dbVersion),
		"A session restart does not correct this.",
	)
	assertContainsNone(t, msg, updatedCauseText, restartFixText, "cannot tell")
}

// TestSchemaTooNewError_InstalledNewerButStillTooOld: the prism on PATH is
// newer than this binary but older than the database. A restart loads a
// binary that also refuses, so the cause is the installed prism.
func TestSchemaTooNewError_InstalledNewerButStillTooOld(t *testing.T) {
	e := &SchemaTooNewError{DBVersion: currentSchemaVersion + 2, BinaryMax: currentSchemaVersion}
	e.diagnose(prismBinaries{
		running:      "/nix/store/old/bin/prism",
		installed:    "/nix/store/mid/bin/prism",
		installedMax: currentSchemaVersion + 1,
	}, true)
	if e.Cause != SkewCauseInstalledTooOld {
		t.Fatalf("Cause = %d, want SkewCauseInstalledTooOld", e.Cause)
	}
	msg := e.Error()
	assertContainsAll(t, msg,
		"/nix/store/old/bin/prism",
		"/nix/store/mid/bin/prism",
		fmt.Sprintf("(max schema version %d)", currentSchemaVersion+1),
	)
	assertContainsNone(t, msg, updatedCauseText, restartFixText)
}

// writeFakePrism writes an executable named prism into a new directory, puts
// only that directory on PATH, and returns the resolved path of the file.
func writeFakePrism(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prism")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake prism: %v", err)
	}
	t.Setenv("PATH", dir)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve fake prism: %v", err)
	}
	return resolved
}

func TestProbeInstalledPrismOnPath_ReadsTheInstalledVersion(t *testing.T) {
	installed := writeFakePrism(t, `[ "$1 $2" = "db max-schema-version" ] && echo 48`)

	bins, ok := probeInstalledPrismOnPath()
	if !ok {
		t.Fatal("probe: ok = false, want true")
	}
	if bins.installed != installed {
		t.Errorf("installed = %q, want %q", bins.installed, installed)
	}
	if bins.installedMax != 48 {
		t.Errorf("installedMax = %d, want 48", bins.installedMax)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if want, _ := filepath.EvalSymlinks(self); bins.running != want {
		t.Errorf("running = %q, want %q", bins.running, want)
	}
}

func TestProbeInstalledPrismOnPath_SameBinaryNeedsNoProcess(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(self, filepath.Join(dir, "prism")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("PATH", dir)

	bins, ok := probeInstalledPrismOnPath()
	if !ok {
		t.Fatal("probe: ok = false, want true")
	}
	if bins.running != bins.installed {
		t.Errorf("running %q != installed %q, want the same path", bins.running, bins.installed)
	}
	if bins.installedMax != currentSchemaVersion {
		t.Errorf("installedMax = %d, want %d", bins.installedMax, currentSchemaVersion)
	}
}

// TestProbeInstalledPrismOnPath_UnknownCases: each case leaves the process
// unable to learn the installed version, so the probe must report ok=false.
func TestProbeInstalledPrismOnPath_UnknownCases(t *testing.T) {
	prevTimeout := maxSchemaVersionTimeout
	maxSchemaVersionTimeout = 500 * time.Millisecond
	t.Cleanup(func() { maxSchemaVersionTimeout = prevTimeout })

	cases := []struct {
		name   string
		script string
	}{
		{name: "verb unknown, prints help", script: `echo "Usage: prism db [command]"`},
		{name: "fails", script: `exit 1`},
		{name: "prints nothing", script: `true`},
		{name: "prints zero", script: `echo 0`},
		// PATH holds only the fake prism, so the loop uses shell built-ins.
		{name: "does not exit", script: `while :; do :; done`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFakePrism(t, tc.script)
			start := time.Now()
			if _, ok := probeInstalledPrismOnPath(); ok {
				t.Error("probe: ok = true, want false")
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Errorf("probe took %v, want it bounded by the timeout", elapsed)
			}
		})
	}

	t.Run("no prism on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, ok := probeInstalledPrismOnPath(); ok {
			t.Error("probe: ok = true, want false")
		}
	})
}

// seedOlderDBWithSessions creates a database at currentSchemaVersion-1 that
// holds the given agent_status rows. live is the number of rows with no
// ended_at. ended is the number of rows with an ended_at.
func seedOlderDBWithSessions(t *testing.T, live, ended int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "older.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (seed): %v", err)
	}
	now := time.Now().UnixMilli()
	for i := 0; i < live+ended; i++ {
		var endedAt any
		if i >= live {
			endedAt = now
		}
		if _, err := d.conn.Exec(
			`INSERT INTO agent_status (session_name, repo, worktree, state, last_seen, ended_at)
			 VALUES (?, 'repo', '/tmp/wt', 'idle', ?, ?)`,
			fmt.Sprintf("prism-test@s%d", i), now, endedAt,
		); err != nil {
			t.Fatalf("insert agent_status: %v", err)
		}
	}
	if _, err := d.conn.Exec(`UPDATE schema_version SET version = ?`, currentSchemaVersion-1); err != nil {
		t.Fatalf("set schema_version: %v", err)
	}
	d.Close()
	return dbPath
}

func readSchemaVersion(t *testing.T, dbPath string) int {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer conn.Close()
	var v int
	if err := conn.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	return v
}

// TestOpen_MigrationWithLiveSessions_WritesCaution: the caution names both
// versions, the live count, the verbs that fail, and the fix. The migration
// still completes.
func TestOpen_MigrationWithLiveSessions_WritesCaution(t *testing.T) {
	dbPath := seedOlderDBWithSessions(t, 3, 2)
	out := captureMigrationCaution(t)

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.Close()

	assertContainsAll(t, out.String(),
		"CAUTION",
		fmt.Sprintf("from schema version %d to %d", currentSchemaVersion-1, currentSchemaVersion),
		"lists 3 live sessions",
		"prism review",
		"prism escalate",
		"prism prompt",
		"cleanup post-hook",
		restartFixText,
	)
	if got := readSchemaVersion(t, dbPath); got != currentSchemaVersion {
		t.Errorf("schema_version after Open = %d, want %d: the caution must not stop the migration", got, currentSchemaVersion)
	}
}

// TestOpen_MigrationWithLiveSessions_DoesNotReadStdin: Open completes while
// stdin is a pipe that never delivers data or EOF. A caution that waits for
// input blocks here.
func TestOpen_MigrationWithLiveSessions_DoesNotReadStdin(t *testing.T) {
	dbPath := seedOlderDBWithSessions(t, 1, 0)
	out := captureMigrationCaution(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prevStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = prevStdin
		w.Close()
		r.Close()
	})

	done := make(chan error, 1)
	go func() {
		d, err := Open(dbPath)
		if err == nil {
			d.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Open did not return within 30s while stdin was open: the caution waits for input")
	}

	assertContainsAll(t, out.String(), "lists 1 live session.")
	if got := readSchemaVersion(t, dbPath); got != currentSchemaVersion {
		t.Errorf("schema_version after Open = %d, want %d", got, currentSchemaVersion)
	}
}

// TestOpen_MigrationWithNoLiveSessions_NoCaution: only ended sessions, so
// the migration writes nothing.
func TestOpen_MigrationWithNoLiveSessions_NoCaution(t *testing.T) {
	dbPath := seedOlderDBWithSessions(t, 0, 2)
	out := captureMigrationCaution(t)

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.Close()

	if out.Len() != 0 {
		t.Errorf("caution written with no live session:\n%s", out.String())
	}
	if got := readSchemaVersion(t, dbPath); got != currentSchemaVersion {
		t.Errorf("schema_version after Open = %d, want %d", got, currentSchemaVersion)
	}
}

// TestOpen_NoMigrationWithLiveSessions_NoCaution: the database is already at
// currentSchemaVersion, so no migration runs and no caution is written.
func TestOpen_NoMigrationWithLiveSessions_NoCaution(t *testing.T) {
	dbPath := seedOlderDBWithSessions(t, 2, 0)
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	d.Close()

	out := captureMigrationCaution(t)
	d, err = Open(dbPath)
	if err != nil {
		t.Fatalf("Open (at current version): %v", err)
	}
	d.Close()

	if out.Len() != 0 {
		t.Errorf("caution written with no migration:\n%s", out.String())
	}
}

// TestOpen_FreshDatabase_NoCaution: a new file migrates from the seed version
// but has no sessions, so no caution is written.
func TestOpen_FreshDatabase_NoCaution(t *testing.T) {
	out := captureMigrationCaution(t)
	d, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.Close()

	if out.Len() != 0 {
		t.Errorf("caution written for a fresh database:\n%s", out.String())
	}
}

// TestOpenReadOnly_OlderDBWithLiveSessions_NoCautionNoMigration: a read-only
// open neither writes the caution nor changes the schema version.
func TestOpenReadOnly_OlderDBWithLiveSessions_NoCautionNoMigration(t *testing.T) {
	dbPath := seedOlderDBWithSessions(t, 3, 0)
	out := captureMigrationCaution(t)

	conn, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	var live int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM agent_status WHERE ended_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("read-only query: %v", err)
	}
	conn.Close()

	if live != 3 {
		t.Errorf("live sessions through the read-only handle = %d, want 3", live)
	}
	if out.Len() != 0 {
		t.Errorf("OpenReadOnly wrote a caution:\n%s", out.String())
	}
	if got := readSchemaVersion(t, dbPath); got != currentSchemaVersion-1 {
		t.Errorf("schema_version after OpenReadOnly = %d, want %d", got, currentSchemaVersion-1)
	}
}

// TestCautionLiveSessions_DefaultsToStderr: with no override, the caution
// goes to os.Stderr.
func TestCautionLiveSessions_DefaultsToStderr(t *testing.T) {
	dbPath := seedOlderDBWithSessions(t, 1, 0)

	prevOut := migrationCautionOut
	migrationCautionOut = nil
	t.Cleanup(func() { migrationCautionOut = prevOut })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prevStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = prevStderr })

	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		captured <- buf.String()
	}()

	d, openErr := Open(dbPath)
	os.Stderr = prevStderr
	w.Close()
	stderr := <-captured
	r.Close()
	if openErr != nil {
		t.Fatalf("Open: %v", openErr)
	}
	d.Close()

	assertContainsAll(t, stderr, "CAUTION", "lists 1 live session.")
}
