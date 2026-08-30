package cmd

// Tests for `prism pr <number>` sessions receiving read-only guidance
// (Case 1 review-only), whether or not the caller supplies a prompt.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWithPRReadOnlyGuidance_NoCallerPrompt verifies the guidance is present
// on its own when the caller supplied no --prompt / --prompt-file.
func TestWithPRReadOnlyGuidance_NoCallerPrompt(t *testing.T) {
	got := withPRReadOnlyGuidance("")
	if got != prReadOnlyGuidance {
		t.Errorf("withPRReadOnlyGuidance(\"\") = %q, want exactly prReadOnlyGuidance", got)
	}
	if !strings.Contains(got, "read-only") {
		t.Errorf("guidance does not mention read-only: %q", got)
	}
}

// TestWithPRReadOnlyGuidance_PreservesCallerPrompt verifies a caller-supplied
// prompt is preserved alongside the guidance, not replaced by it (AC2).
func TestWithPRReadOnlyGuidance_PreservesCallerPrompt(t *testing.T) {
	callerPrompt := "please summarise the diff for me"
	got := withPRReadOnlyGuidance(callerPrompt)
	if !strings.Contains(got, callerPrompt) {
		t.Errorf("caller prompt not preserved in combined prompt: %q", got)
	}
	if !strings.Contains(got, prReadOnlyGuidance) {
		t.Errorf("guidance not present in combined prompt: %q", got)
	}
}

// TestWithPRReadOnlyGuidance_LiftOnlyByOperator checks the guidance text
// itself names the only escape hatch: an explicit operator instruction.
func TestWithPRReadOnlyGuidance_LiftOnlyByOperator(t *testing.T) {
	if !strings.Contains(prReadOnlyGuidance, "explicit instruction from") {
		t.Errorf("guidance does not state the operator-only escape hatch: %q", prReadOnlyGuidance)
	}
	if !strings.Contains(prReadOnlyGuidance, "Do NOT commit, push") {
		t.Errorf("guidance does not prohibit commit/push: %q", prReadOnlyGuidance)
	}
}

// TestPrCmd_ContainerMode_InjectsReadOnlyGuidance verifies that prCmd.RunE,
// in container mode (PRISM_HOST_API set), forwards a prompt containing the
// read-only guidance to the host-API /spawn endpoint, whether or not the
// caller supplied --prompt (AC: present regardless of --prompt/--prompt-file).
func TestPrCmd_ContainerMode_InjectsReadOnlyGuidance(t *testing.T) {
	type spawnReq struct {
		Prompt string `json:"prompt"`
	}

	for _, tc := range []struct {
		name         string
		callerPrompt string
	}{
		{"no caller prompt", ""},
		{"with caller prompt", "review this PR and summarise the changes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqCh := make(chan spawnReq, 1)

			srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
				var raw map[string]any
				_ = json.NewDecoder(r.Body).Decode(&raw)
				b, _ := json.Marshal(raw)
				var req spawnReq
				_ = json.Unmarshal(b, &req)
				reqCh <- req
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"session_name":"nixos-config@feat-386"}`))
			})

			t.Setenv("PRISM_HOST_API", srv.apiURL())

			bareDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(bareDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
				t.Fatalf("write HEAD: %v", err)
			}
			if err := os.Mkdir(filepath.Join(bareDir, "objects"), 0o755); err != nil {
				t.Fatalf("mkdir objects: %v", err)
			}
			t.Setenv("PRISM_BARE_ROOT", bareDir)

			_ = prCmd.Flags().Set("prompt", tc.callerPrompt)
			t.Cleanup(func() { _ = prCmd.Flags().Set("prompt", "") })

			if err := prCmd.RunE(prCmd, []string{"386"}); err != nil {
				t.Fatalf("prCmd.RunE: %v", err)
			}

			select {
			case req := <-reqCh:
				if !strings.Contains(req.Prompt, prReadOnlyGuidance) {
					t.Errorf("prompt sent to host API does not contain read-only guidance: %q", req.Prompt)
				}
				if tc.callerPrompt != "" && !strings.Contains(req.Prompt, tc.callerPrompt) {
					t.Errorf("prompt sent to host API dropped the caller-supplied prompt: %q", req.Prompt)
				}
				// Simulate the host-side second application: the host-API
				// /spawn handler shells `prism spawn --pr <n> --prompt
				// <req.Prompt>`, and that subprocess's runSpawn calls
				// withPRReadOnlyGuidance again (see the call site in
				// spawn.go). This is the common case for every sandboxed
				// coordinator running `prism pr`, not an edge case — assert
				// on an exact count, not mere presence, so a future change
				// that breaks the Contains-based idempotency guard (e.g. the
				// constant becomes a template, or is reworded on one call
				// site but not the other) fails loudly here instead of
				// silently doubling the guidance in a live worker's prompt.
				//
				// Marker choice: prReadOnlyGuidance itself, not a substring
				// of it. The production idempotency guard is
				// `strings.Contains(callerPrompt, prReadOnlyGuidance)` —
				// counting the exact same string this test exercises the
				// exact predicate the guard evaluates, so the test and the
				// guard can never drift out of sync with each other.
				hostSideResult := withPRReadOnlyGuidance(req.Prompt)
				if got := strings.Count(hostSideResult, prReadOnlyGuidance); got != 1 {
					t.Errorf("guidance appears %d times after simulated host-side re-injection, want exactly 1: %q", got, hostSideResult)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for request")
			}
		})
	}
}
