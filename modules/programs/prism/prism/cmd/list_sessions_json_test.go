package cmd

// Tests for --json flag on prism list-sessions (#1463).
//
// Both the direct-DB path and the proxy path are exercised.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestListSessions_JSONFlag_DirectPath verifies that list-sessions --json
// (no PRISM_HOST_API) emits a valid JSON object with a sessions array.
func TestListSessions_JSONFlag_DirectPath(t *testing.T) {
	d := openStatsTestDB(t) // also unsets PRISM_HOST_API

	const session = "nixos-config@main"
	if err := d.UpsertStatus(session, "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	listSessionsCmd.Flags().Set("all", "true")   //nolint:errcheck
	listSessionsCmd.Flags().Set("json", "true")  //nolint:errcheck
	defer func() {
		listSessionsCmd.Flags().Set("all", "false")  //nolint:errcheck
		listSessionsCmd.Flags().Set("json", "false") //nolint:errcheck
	}()

	out := captureStdout(t, func() {
		if err := listSessionsCmd.RunE(listSessionsCmd, nil); err != nil {
			t.Errorf("list-sessions --json: %v", err)
		}
	})

	trimmed := strings.TrimSpace(out)
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	sessionsVal, ok := obj["sessions"]
	if !ok {
		t.Fatalf("expected 'sessions' key in output")
	}
	_ = sessionsVal // type check not needed here
	if _, ok := obj["truncated"]; !ok {
		t.Errorf("expected 'truncated' key in output")
	}

	// Find our session by re-marshalling.
	data, _ := json.Marshal(sessionsVal)
	var sessions []db.Status
	if err := json.Unmarshal(data, &sessions); err != nil {
		t.Fatalf("sessions is not a valid []db.Status: %v", err)
	}
	if len(sessions) == 0 {
		t.Error("expected at least 1 session")
	}

	found := false
	for _, s := range sessions {
		if s.SessionName == session {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("session %q not found in JSON output: %s", session, out)
	}
}

// TestListSessions_JSONFlag_ProxyPath verifies that list-sessions --json
// with PRISM_HOST_API set emits the raw server response JSON.
func TestListSessions_JSONFlag_ProxyPath(t *testing.T) {
	sessions := []db.Status{
		{SessionName: "nixos-config@main", State: "active"},
	}
	respBody, _ := json.Marshal(sessions)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/list-sessions" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	listSessionsCmd.Flags().Set("json", "true")       //nolint:errcheck
	defer listSessionsCmd.Flags().Set("json", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := listSessionsCmd.RunE(listSessionsCmd, nil); err != nil {
			t.Errorf("list-sessions --json proxy: %v", err)
		}
	})

	trimmed := strings.TrimSpace(out)
	var got []db.Status
	if err := json.Unmarshal([]byte(trimmed), &got); err != nil {
		t.Fatalf("--json proxy output is not valid JSON: %v\noutput: %s", err, out)
	}
	if len(got) == 0 || got[0].SessionName != "nixos-config@main" {
		t.Errorf("unexpected sessions in JSON output: %s", out)
	}
}

// TestListSessions_HumanReadable_UnchangedByJSON verifies that the
// human-readable output path is not affected when --json is false (regression).
func TestListSessions_HumanReadable_UnchangedByJSON(t *testing.T) {
	d := openStatsTestDB(t) // unsets PRISM_HOST_API

	const session = "nixos-config@main"
	if err := d.UpsertStatus(session, "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	listSessionsCmd.Flags().Set("all", "true")  //nolint:errcheck
	defer listSessionsCmd.Flags().Set("all", "false") //nolint:errcheck
	// --json is false by default.

	out := captureStdout(t, func() {
		if err := listSessionsCmd.RunE(listSessionsCmd, nil); err != nil {
			t.Errorf("list-sessions (human): %v", err)
		}
	})

	// Should contain the session name in the table format, not as raw JSON.
	if !strings.Contains(out, session) {
		t.Errorf("session %q not in human-readable output:\n%s", session, out)
	}
	// Should NOT be a JSON array.
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "[") {
		t.Errorf("human-readable output looks like JSON array:\n%s", out)
	}
}
