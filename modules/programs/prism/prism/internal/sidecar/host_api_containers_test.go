package sidecar

// Tests for the host-API /spawn handler accepting the "containers" field
// (#2317 / #2323). The handler is the bridge between the proxy CLI and the
// host-side `prism spawn` invocation; the regression we want to prevent is
// the same as #1059 for --isolation: silently dropping the field on the
// host-API boundary so a sandboxed `prism spawn --containers` lands as a
// containerless session on the host.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostAPI_Spawn_ContainersForwardedWhenSet verifies that when the
// /spawn request body carries {"containers": true}, the sidecar appends
// "--containers" to the args passed to the prism binary.
//
// Mirror of TestHostAPI_Spawn_IsolationForwarded but for the boolean
// --containers flag.
func TestHostAPI_Spawn_ContainersForwardedWhenSet(t *testing.T) {
	d := openTestDB(t)

	argsFile := filepath.Join(t.TempDir(), "captured-args")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := `#!/bin/sh
echo "$*" > ` + argsFile + `
last=""
for arg; do last="$arg"; done
echo "session \"${last}@containers-branch\" created"
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	body := `{"branch":"containers-branch","prompt":"hi","containers":true}`
	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	capturedArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(capturedArgs), "--containers") {
		t.Errorf("captured args %q do not contain --containers; the field was silently dropped at the host-API boundary (regression class #1059 for --isolation, now #2323 for --containers)",
			string(capturedArgs))
	}
}

// TestHostAPI_Spawn_ContainersOmittedWhenAbsent verifies that when the
// request body does not include a "containers" field, no --containers
// flag is appended to the spawned subprocess args. Symmetry with the
// --isolation absence test (TestHostAPI_Spawn_IsolationOmittedWhenAbsent):
// absence must mean "default false", not "explicit false forwarded".
//
// This matters because the explicit-vs-default distinction is the only
// thing standing between a containerised parent's `prism spawn` (no
// --containers) silently inheriting the parent's enabled state if a
// future refactor of the proxy-body construction defaulted the field on.
func TestHostAPI_Spawn_ContainersOmittedWhenAbsent(t *testing.T) {
	d := openTestDB(t)

	argsFile := filepath.Join(t.TempDir(), "captured-args")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := `#!/bin/sh
echo "$*" > ` + argsFile + `
last=""
for arg; do last="$arg"; done
echo "session \"${last}@no-containers-branch\" created"
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn",
		`{"branch":"no-containers-branch","prompt":"hi"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	capturedArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if strings.Contains(string(capturedArgs), "--containers") {
		t.Errorf("captured args %q contain --containers; absence in body must omit the flag, not pass it (issue #2323: unset child must not inherit enabled state)",
			string(capturedArgs))
	}
}

// TestHostAPI_Spawn_ContainersFalseExplicitOmitted verifies that an
// explicit "containers": false in the body also omits --containers from
// the subprocess args. This is by design: --containers is a boolean
// presence flag (the absence/presence distinction is what carries
// meaning on the CLI), so an explicit-false request is functionally
// indistinguishable from the absent case at the spawn-binary layer.
//
// If a future revision wants to support a --no-containers form (e.g. to
// override a config-driven default), this is the test that will need to
// flip to assert the inverse.
func TestHostAPI_Spawn_ContainersFalseExplicitOmitted(t *testing.T) {
	d := openTestDB(t)

	argsFile := filepath.Join(t.TempDir(), "captured-args")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := `#!/bin/sh
echo "$*" > ` + argsFile + `
last=""
for arg; do last="$arg"; done
echo "session \"${last}@explicit-false-branch\" created"
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn",
		`{"branch":"explicit-false-branch","prompt":"hi","containers":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	capturedArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if strings.Contains(string(capturedArgs), "--containers") {
		t.Errorf("captured args %q contain --containers; explicit false must omit the flag (--containers is a presence flag, not a tri-state)",
			string(capturedArgs))
	}
}
