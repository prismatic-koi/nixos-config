package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestDBMaxSchemaVersion_AnswersTheProbeArgs pins the contract between
// db.MaxSchemaVersionArgs and this verb. db.Open runs the prism on PATH with
// those arguments, and parses stdout as one integer. If the verb moves or
// prints more, every new binary reports "cause unknown" to an older one.
//
// The test also sets PRISM_HOST_API and points the database path at a
// directory that does not exist. The verb must answer without the database
// and without a proxy, because the probe runs it when the database refuses
// the caller.
func TestDBMaxSchemaVersion_AnswersTheProbeArgs(t *testing.T) {
	resetRootCmdFlags(t)
	missing := filepath.Join(t.TempDir(), "absent", "prism.db")
	SetTestDBPath(missing)
	t.Cleanup(func() { SetTestDBPath("") })
	t.Setenv("PRISM_HOST_API", "unix:///nonexistent/hostapi.sock")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetArgs(db.MaxSchemaVersionArgs())
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetArgs(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("prism %s: %v", strings.Join(db.MaxSchemaVersionArgs(), " "), err)
	}

	got, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("stdout %q is not one integer: %v", out.String(), err)
	}
	if got != db.MaxSchemaVersion() {
		t.Errorf("printed %d, want db.MaxSchemaVersion() = %d", got, db.MaxSchemaVersion())
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Errorf("the verb created %s: it must not open the database", filepath.Dir(missing))
	}
}
