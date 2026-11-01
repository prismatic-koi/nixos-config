package cmd

// ensureDashSession creates the dashboard session through
// session.NewSessionRetryingServerExit. During `prism restart`, tmux
// new-session can fail with "server exited unexpectedly" while the old
// server exits (#3309, #3338 item 7).

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/tmux"
)

func TestEnsureDashSession_RetriesServerExit_3338(t *testing.T) {
	dir := t.TempDir()
	calls := dir + "/new-session-calls"
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  has-session) exit 1 ;;
  new-session)
    echo x >> %[1]s
    if [ "$(wc -l < %[1]s)" -le 2 ]; then
      echo 'server exited unexpectedly' >&2
      exit 1
    fi ;;
esac
exit 0
`, calls)
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = dir + "/tmux"
	t.Cleanup(func() { tmux.TmuxBin = orig })

	if err := ensureDashSession(); err != nil {
		t.Fatalf("ensureDashSession: %v, want success after the server-exit failures", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read %s: %v", calls, err)
	}
	if got := strings.Count(string(data), "\n"); got != 3 {
		t.Errorf("new-session calls = %d, want 3 (2 failures, then a success)", got)
	}
}
