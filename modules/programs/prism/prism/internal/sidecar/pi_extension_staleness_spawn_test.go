package sidecar

// Handler-level tests for the pi_extension_dir staleness diagnostic wired into
// the host-API /spawn handler.
//
// These prove three things the pure-function test in
// pi_extension_staleness_test.go cannot:
//
//   - the /spawn handler actually reads the sidecar's startup-cached value
//     (s.cfg.PIExtensionDir), re-reads config.json fresh, and logs the
//     diagnostic on a genuine mismatch;
//   - a switch that leaves pi_extension_dir unchanged logs nothing;
//   - a missing or unreadable config.json neither blocks nor crashes the
//     spawn, and logs nothing (fail open).
//
// The spawn itself is stubbed (PrismBinaryPath → a shell script that echoes a
// "created" line) so the handler returns 200 without launching real prism.

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSpawnSidecarForStaleness builds a coordinator Sidecar whose spawn is
// stubbed and whose log output is captured in the returned buffer. cachedDir
// is the value the sidecar "read at startup".
func newSpawnSidecarForStaleness(t *testing.T, cachedDir string) (*Sidecar, *bytes.Buffer) {
	t.Helper()
	d := openTestDB(t)

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stub := "#!/bin/sh\necho 'session \"test-repo@feature\" created'\n"
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("write spawn stub: %v", err)
	}

	buf := &bytes.Buffer{}
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		PIExtensionDir:  cachedDir,
		Logger:          log.New(buf, "", 0),
		Harness:         newSSEHarness(),
	}
	return New(cfg), buf
}

// writeConfigWithExtDir writes a minimal config.json carrying pi_extension_dir
// and points PRISM_CONFIG_FILE at it for the duration of the test, so
// config.LoadFresh() inside the handler reads this file.
func writeConfigWithExtDir(t *testing.T, extDir string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"pi_extension_dir":"` + extDir + `"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

const (
	stalenessOldExt = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-prism-pi-extension"
	stalenessNewExt = "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-prism-pi-extension"
)

// TestHostAPI_Spawn_StaleExtension_Warns: the sidecar cached the pre-switch
// path; config.json now holds the post-switch path. The spawn still succeeds
// (fail open) and the diagnostic naming both paths is logged.
func TestHostAPI_Spawn_StaleExtension_Warns(t *testing.T) {
	sc, buf := newSpawnSidecarForStaleness(t, stalenessOldExt)
	writeConfigWithExtDir(t, stalenessNewExt)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (spawn must fail open); body = %s", rr.Code, rr.Body.String())
	}
	logged := buf.String()
	for _, want := range []string{"STALE PI EXTENSION", stalenessOldExt, stalenessNewExt, "prism restart"} {
		if !strings.Contains(logged, want) {
			t.Errorf("sidecar log does not contain %q; log = %s", want, logged)
		}
	}
}

// TestHostAPI_Spawn_UnchangedExtension_Silent: a switch that does not move
// pi_extension_dir logs nothing (AC edge-case).
func TestHostAPI_Spawn_UnchangedExtension_Silent(t *testing.T) {
	sc, buf := newSpawnSidecarForStaleness(t, stalenessOldExt)
	writeConfigWithExtDir(t, stalenessOldExt)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(buf.String(), "STALE PI EXTENSION") {
		t.Errorf("unchanged extension must not warn; log = %s", buf.String())
	}
}

// TestHostAPI_Spawn_MissingConfig_NoCrashNoBlock: a config.json that does not
// exist must neither crash the sidecar nor block the spawn, and must not warn
// (AC edge-case; fail open).
func TestHostAPI_Spawn_MissingConfig_NoCrashNoBlock(t *testing.T) {
	sc, buf := newSpawnSidecarForStaleness(t, stalenessOldExt)
	// Point PRISM_CONFIG_FILE at a path that does not exist.
	t.Setenv("PRISM_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist.json"))

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (missing config must not block spawn); body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(buf.String(), "STALE PI EXTENSION") {
		t.Errorf("missing config must not warn; log = %s", buf.String())
	}
}
