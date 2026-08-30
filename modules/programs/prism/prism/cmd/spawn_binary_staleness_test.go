package cmd

// TestProxySpawn_WarningPrintedToStderr is the CLI-side regression test: the
// sidecar's /spawn response can carry a "warning" field (the prism-binary
// staleness diagnostic) alongside session_name, and proxySpawn must decode and
// print it, or the diagnostic never reaches a real `prism spawn` caller and
// stays log-only in practice.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestProxySpawn_WarningPrintedToStderr(t *testing.T) {
	const warning = "STALE PRISM BINARY: this sidecar launched from \"/old\" but the " +
		"currently-installed prism binary resolves to \"/new\". Run `prism restart` " +
		"to pick up the new binary. (issue #2742)"

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@test-branch","warning":` + jsonQuote(warning) + `}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	t.Setenv("PRISM_BARE_ROOT", "/prism-git")

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("branch", "test-branch")
	_ = cmd.Flags().Set("prompt", "hello world")

	_, stderr := captureStdoutStderr(t, func() {
		if err := proxySpawn(srv.apiURL(), cmd); err != nil {
			t.Fatalf("proxySpawn: %v", err)
		}
	})

	if !strings.Contains(stderr, "STALE PRISM BINARY") {
		t.Errorf("stderr does not contain the sidecar's warning; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "#2742") {
		t.Errorf("stderr does not contain the issue reference; stderr = %q", stderr)
	}
}

// TestProxySpawn_NoWarningField_SilentStderr is the sibling case: a response
// with no "warning" field (the common case, sidecar current) must not print
// anything to stderr.
func TestProxySpawn_NoWarningField_SilentStderr(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@test-branch"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	t.Setenv("PRISM_BARE_ROOT", "/prism-git")

	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("variant", "", "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("branch", "test-branch")
	_ = cmd.Flags().Set("prompt", "hello world")

	_, stderr := captureStdoutStderr(t, func() {
		if err := proxySpawn(srv.apiURL(), cmd); err != nil {
			t.Fatalf("proxySpawn: %v", err)
		}
	})

	if strings.TrimSpace(stderr) != "" {
		t.Errorf("stderr should be empty with no warning field; stderr = %q", stderr)
	}
}

// jsonQuote produces a Go-source JSON string literal for embedding a
// diagnostic (which itself contains double quotes) inside a hand-written
// JSON response body.
func jsonQuote(s string) string {
	quoted := strings.ReplaceAll(s, `\`, `\\`)
	quoted = strings.ReplaceAll(quoted, `"`, `\"`)
	return `"` + quoted + `"`
}
