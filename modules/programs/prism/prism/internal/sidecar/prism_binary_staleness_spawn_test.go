package sidecar

// Handler-level tests for the prism-binary staleness diagnostic wired into
// the host-API /spawn handler (issue #2742).
//
// These prove what the pure-function test in prism_binary_staleness_test.go
// cannot:
//
//   - the /spawn handler resolves the sidecar's launch-time binary
//     (s.cfg.PrismBinaryPath, standing in for os.Executable() in prod),
//     resolves the currently-installed prism on PATH, and both logs the
//     diagnostic AND returns it to the caller on a genuine mismatch;
//   - an unchanged binary logs and returns nothing;
//   - an unresolvable currently-installed binary (nothing on PATH) neither
//     blocks nor crashes the spawn, and warns nothing (fail open);
//   - the sidecar log line is emitted at most once per process even across
//     multiple /spawn calls.
//
// The spawn itself is stubbed (PrismBinaryPath -> a real file standing in for
// the launch-time binary) so the handler returns 200 without launching real
// prism.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSpawnSidecarForBinaryStaleness builds a coordinator Sidecar whose spawn
// is stubbed at launchPath (standing in for the sidecar's os.Executable()
// launch-time resolution) and whose log output is captured in the returned
// buffer.
func newSpawnSidecarForBinaryStaleness(t *testing.T, launchPath string) (*Sidecar, *bytes.Buffer) {
	t.Helper()
	d := openTestDB(t)

	buf := &bytes.Buffer{}
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       "coordinator",
		PrismBinaryPath: launchPath,
		Logger:          log.New(buf, "", 0),
		Harness:         newSSEHarness(),
	}
	return New(cfg), buf
}

// writeStubBinary writes an executable shell script at dir/prism that echoes
// a spawn-shaped "created" line, and returns its path.
func writeStubBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "prism")
	stub := "#!/bin/sh\necho 'session \"test-repo@feature\" created'\n"
	if err := os.WriteFile(path, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return path
}

// putOnPath points PATH at a directory containing an executable "prism" at
// binPath, for the duration of the test, so currentInstalledPrismPath()
// inside the handler resolves to binPath.
func putOnPath(t *testing.T, binPath string) {
	t.Helper()
	t.Setenv("PATH", filepath.Dir(binPath))
}

// TestHostAPI_Spawn_StaleBinary_WarnsAndReturnsWarning: the sidecar launched
// from one binary; a different binary is now installed on PATH. The spawn
// still succeeds (fail open), the diagnostic naming both paths is logged,
// and the response body carries a "warning" field for the caller.
func TestHostAPI_Spawn_StaleBinary_WarnsAndReturnsWarning(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldBin := writeStubBinary(t, oldDir)
	newBin := writeStubBinary(t, newDir)

	sc, buf := newSpawnSidecarForBinaryStaleness(t, oldBin)
	putOnPath(t, newBin)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (spawn must fail open); body = %s", rr.Code, rr.Body.String())
	}

	logged := buf.String()
	for _, want := range []string{"STALE PRISM BINARY", oldBin, newBin, "prism restart", "#2742"} {
		if !strings.Contains(logged, want) {
			t.Errorf("sidecar log does not contain %q; log = %s", want, logged)
		}
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body = %s", err, rr.Body.String())
	}
	warning, ok := resp["warning"]
	if !ok || warning == "" {
		t.Fatalf("response missing non-empty \"warning\" field; body = %s", rr.Body.String())
	}
	if !strings.Contains(warning, "STALE PRISM BINARY") {
		t.Errorf("response warning = %q, want it to name the staleness", warning)
	}
}

// TestHostAPI_Spawn_UnchangedBinary_Silent: PATH resolves to the same binary
// the sidecar launched from — no warning, logged or returned.
func TestHostAPI_Spawn_UnchangedBinary_Silent(t *testing.T) {
	dir := t.TempDir()
	bin := writeStubBinary(t, dir)

	sc, buf := newSpawnSidecarForBinaryStaleness(t, bin)
	putOnPath(t, bin)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(buf.String(), "STALE PRISM BINARY") {
		t.Errorf("unchanged binary must not warn; log = %s", buf.String())
	}
	if strings.Contains(rr.Body.String(), "warning") {
		t.Errorf("unchanged binary must not return a warning field; body = %s", rr.Body.String())
	}
}

// TestHostAPI_Spawn_NoInstalledPrism_NoCrashNoBlock: nothing named "prism" is
// on PATH, so the currently-installed path cannot be resolved. Must neither
// crash the sidecar nor block the spawn, and must not warn (fail open).
func TestHostAPI_Spawn_NoInstalledPrism_NoCrashNoBlock(t *testing.T) {
	oldDir := t.TempDir()
	oldBin := writeStubBinary(t, oldDir)

	sc, buf := newSpawnSidecarForBinaryStaleness(t, oldBin)
	// PATH points at an empty directory: no "prism" resolvable there.
	t.Setenv("PATH", t.TempDir())

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unresolvable PATH must not block spawn); body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(buf.String(), "STALE PRISM BINARY") {
		t.Errorf("unresolvable currently-installed binary must not warn; log = %s", buf.String())
	}
	if strings.Contains(rr.Body.String(), "warning") {
		t.Errorf("unresolvable currently-installed binary must not return a warning field; body = %s", rr.Body.String())
	}
}

// TestCheckBinaryStale_FiresFromNonSpawnCallSite: checkBinaryStale() is the
// method prismBinary() calls on every one of its 10 exec sites, not just
// /spawn's. Calling it directly (standing in for any of those other 9 call
// sites — /review, /cleanup, /prompt, /investigate, ...) must populate
// s.binaryStaleDiag and log, with no /spawn request involved at all. This
// pins the review-goal finding that the check must not be reachable only
// through /spawn.
func TestCheckBinaryStale_FiresFromNonSpawnCallSite(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldBin := writeStubBinary(t, oldDir)
	newBin := writeStubBinary(t, newDir)

	sc, buf := newSpawnSidecarForBinaryStaleness(t, oldBin)
	putOnPath(t, newBin)

	// No HTTP request at all — this is what any of the other 9
	// prismBinary()-calling handlers do before building their exec.Cmd.
	sc.checkBinaryStale()

	if sc.binaryStaleDiag == "" {
		t.Fatal("checkBinaryStale() left binaryStaleDiag empty for a genuine mismatch")
	}
	if !strings.Contains(buf.String(), "STALE PRISM BINARY") {
		t.Errorf("sidecar log does not contain STALE PRISM BINARY; log = %s", buf.String())
	}
}

// TestCheckBinaryStale_DedupesAcrossHostAPIHandlerBuilds: on Darwin,
// hostAPIHandler() is called twice per process — once for the always-on Unix
// socket server, once for the container-mode TCP server — each building an
// independent set of closures. Because binaryStaleOnce/binaryStaleDiag are
// fields on *Sidecar rather than local to hostAPIHandler(), two separate
// handler builds sharing the same Sidecar must still log exactly once. This
// pins the review-code finding that a closure-local sync.Once would double-
// log on that platform.
func TestCheckBinaryStale_DedupesAcrossHostAPIHandlerBuilds(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldBin := writeStubBinary(t, oldDir)
	newBin := writeStubBinary(t, newDir)

	sc, buf := newSpawnSidecarForBinaryStaleness(t, oldBin)
	putOnPath(t, newBin)

	// Build the host-API handler twice, as Run() does on Darwin in container
	// mode (Unix listener + TCP listener), and drive one /spawn request
	// through each.
	handlerA := sc.hostAPIHandler()
	handlerB := sc.hostAPIHandler()

	for _, h := range []http.Handler{handlerA, handlerB} {
		req := newHostAPIRequest(t, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
	}

	if got := strings.Count(buf.String(), "STALE PRISM BINARY"); got != 1 {
		t.Errorf("sidecar log contains %d STALE PRISM BINARY line(s) across 2 hostAPIHandler() builds, want exactly 1; log = %s", got, buf.String())
	}
}

// TestHostAPI_Spawn_StaleBinary_LogsOnce: the sidecar log line is emitted at
// most once per process (AC edge-case) even though every one of the 10
// prismBinary() exec sites could otherwise trigger it — two /spawn calls in
// the same process must produce exactly one log line.
func TestHostAPI_Spawn_StaleBinary_LogsOnce(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	oldBin := writeStubBinary(t, oldDir)
	newBin := writeStubBinary(t, newDir)

	sc, buf := newSpawnSidecarForBinaryStaleness(t, oldBin)
	putOnPath(t, newBin)

	// hostAPIHandler() builds the mux (and the closures it captures,
	// including binaryStaleOnce) fresh each call, matching the once-per-
	// mux-build scope of the check. In production hostAPIHandler() is
	// called exactly once per sidecar process, so reuse a single handler
	// here to exercise "once per process" rather than "once per
	// hostAPIHandler() call".
	handler := sc.hostAPIHandler()
	for i := 0; i < 2; i++ {
		req := newHostAPIRequest(t, http.MethodPost, "/spawn", `{"branch":"feature","prompt":"go"}`)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200; body = %s", i, rr.Code, rr.Body.String())
		}
	}

	if got := strings.Count(buf.String(), "STALE PRISM BINARY"); got != 1 {
		t.Errorf("sidecar log contains %d STALE PRISM BINARY line(s) across 2 spawns, want exactly 1; log = %s", got, buf.String())
	}
}
