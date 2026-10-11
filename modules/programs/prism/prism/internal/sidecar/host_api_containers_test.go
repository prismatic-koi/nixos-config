package sidecar

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestHostAPI_Spawn_RejectsContainersField: /spawn has no "containers"
// field. If the handler accepts the field again, this test fails.
func TestHostAPI_Spawn_RejectsContainersField(t *testing.T) {
	d := openTestDB(t)

	argsFile := filepath.Join(t.TempDir(), "captured-args")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := `#!/bin/sh
echo "$*" > ` + argsFile + `
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	sc := New(Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	})

	body := `{"branch":"containers-branch","prompt":"hi","containers":true}`
	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Errorf("the handler ran prism spawn for a request with a containers field (stat err: %v)", err)
	}
}
