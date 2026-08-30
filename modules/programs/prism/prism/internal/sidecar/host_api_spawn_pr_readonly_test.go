package sidecar

// Tests for the sandboxed empty-prompt PR-spawn path: a sandboxed
// `prism spawn --pr <number>`
// (routed through the host-API /spawn handler) must reach the host-side
// `prism spawn --pr` subprocess even when the caller supplied no prompt —
// the read-only guidance injected there (withPRReadOnlyGuidance in
// cmd/spawn.go) is what fills the prompt in, not this layer. This is the
// layer-3 carve-out counterpart to TestHostAPI_Spawn_EmptyPromptReturns400.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostAPI_Spawn_PREmptyPromptSucceeds verifies that a /spawn request
// carrying a non-empty "pr" field and an empty (or absent) "prompt" field is
// accepted (200), not rejected the way a plain branch-based empty-prompt
// request is (see TestHostAPI_Spawn_EmptyPromptReturns400). It also asserts
// the shelled-out host-side command actually receives --pr, proving the
// request reaches the same `prism spawn --pr` subprocess that
// `prism pr <number>` container-routes through.
func TestHostAPI_Spawn_PREmptyPromptSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "explicit empty string", body: `{"pr":"386","prompt":""}`},
		{name: "prompt field omitted", body: `{"pr":"386"}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)

			// Stub that records its argv and echoes a success line, mirroring
			// TestHostAPI_Spawn_EmptyRepoFieldSucceeds's stub pattern.
			argvPath := filepath.Join(t.TempDir(), "argv.txt")
			stubPath := filepath.Join(t.TempDir(), "prism-stub")
			stubScript := "#!/bin/sh\n" +
				"echo \"$@\" > " + argvPath + "\n" +
				"last=\"\"\n" +
				"for arg; do last=\"$arg\"; done\n" +
				"echo \"session \\\"${last}@pr-386\\\" created\"\n"
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

			rr := doHostAPI(t, sc, http.MethodPost, "/spawn", tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for empty prompt with pr set; body = %s", rr.Code, rr.Body.String())
			}

			argvBytes, readErr := os.ReadFile(argvPath)
			if readErr != nil {
				t.Fatalf("stub was not invoked (argv file missing): %v", readErr)
			}
			argv := string(argvBytes)
			if !strings.Contains(argv, "--pr 386") {
				t.Errorf("host-side invocation args = %q, want to contain %q", argv, "--pr 386")
			}
		})
	}
}
