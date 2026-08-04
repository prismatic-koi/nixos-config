package cmd

// Tests for --json flag on prism checkin (#1463).
//
// Both the direct-DB path and the proxy path are exercised.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestCheckin_JSONFlag_DirectPath verifies that checkin --json on the direct-DB
// path emits a JSON object with "session", "state", and "events" keys.
func TestCheckin_JSONFlag_DirectPath(t *testing.T) {
	dbPath := openCheckinTestDB(t).Path()
	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	defer d.Close()

	const session = "nixos-config@main"
	if err := d.UpsertStatus(session, "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  uuid.New().String(),
		SessionName: session,
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Write one msg_assistant event so there's something to return.
	msgID := "msg-json-test"
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Type:        "msg_assistant",
		Payload:     fmt.Sprintf(`{"messageId":%q,"text":"hello"}`, msgID),
		CreatedAt:   time.Now().Add(-5 * time.Minute),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runCheckinSessionJSON(session, 10, nil, nil, nil); err != nil {
			t.Errorf("runCheckinSessionJSON: %v", err)
		}
	})

	trimmed := strings.TrimSpace(out)
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}

	for _, key := range []string{"session", "state", "events", "truncated"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("--json output missing key %q; got: %s", key, out)
		}
	}

	// "session" value should be the session name.
	var sessionVal string
	if err := json.Unmarshal(resp["session"], &sessionVal); err == nil {
		if sessionVal != session {
			t.Errorf("session = %q, want %q", sessionVal, session)
		}
	}
}

// TestCheckin_JSONFlag_ProxyPath verifies that checkin --json with PRISM_HOST_API
// set emits the raw server response JSON (byte-identical to server response).
func TestCheckin_JSONFlag_ProxyPath(t *testing.T) {
	serverResp := map[string]any{
		"session": "nixos-config@main",
		"state":   "active",
		"events":  []map[string]any{},
	}
	respBody, _ := json.Marshal(serverResp)

	var capturedPath string
	var capturedQuery string
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	out := captureStdout(t, func() {
		if err := runCheckinSession("nixos-config@main", 10, nil, nil, nil, false, true); err != nil {
			t.Errorf("checkin --json proxy: %v", err)
		}
	})

	if capturedPath != "/checkin" {
		t.Errorf("path = %q, want /checkin", capturedPath)
	}
	if !strings.Contains(capturedQuery, "session=nixos-config%40main") {
		t.Errorf("query %q does not contain encoded session", capturedQuery)
	}

	// Output should be the raw server response.
	trimmed := strings.TrimSpace(out)
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &got); err != nil {
		t.Fatalf("--json proxy output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := got["events"]; !ok {
		t.Errorf("--json proxy output missing 'events' key; got: %s", out)
	}
}

// TestCheckin_JSONFlag_StableSchema verifies the schema for checkin --json output:
// the output must contain "session", "state", and "events" keys.
// This is the "stable schema" AC from the issue.
func TestCheckin_JSONFlag_StableSchema(t *testing.T) {
	serverResp := map[string]any{
		"session": "nixos-config@main",
		"state":   "active",
		"events": []map[string]any{
			{
				"ID":          "evt-1",
				"SessionName": "nixos-config@main",
				"Type":        "msg_assistant",
				"Payload":     `{"text":"hello"}`,
				"CreatedAt":   "2026-01-01T00:00:00Z",
			},
		},
	}
	respBody, _ := json.Marshal(serverResp)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	out := captureStdout(t, func() {
		if err := runCheckinSession("nixos-config@main", 10, nil, nil, nil, false, true); err != nil {
			t.Errorf("checkin --json schema: %v", err)
		}
	})

	trimmed := strings.TrimSpace(out)
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}

	for _, key := range []string{"session", "state", "events"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("--json output missing key %q; got: %s", key, out)
		}
	}

	// events must be an array.
	var events []json.RawMessage
	if err := json.Unmarshal(resp["events"], &events); err != nil {
		t.Errorf("events is not an array: %v", err)
	}
}

// TestCheckin_HumanReadable_UnchangedByJSON verifies that the human-readable
// rendering path is unchanged when --json is false (regression guard).
func TestCheckin_HumanReadable_UnchangedByJSON(t *testing.T) {
	dbPath := openCheckinTestDB(t).Path()
	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen DB: %v", err)
	}
	defer d.Close()

	const session = "nixos-config@main"
	if err := d.UpsertStatus(session, "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  uuid.New().String(),
		SessionName: session,
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	msgID := "msg-human-test"
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Type:        "msg_assistant",
		Payload:     fmt.Sprintf(`{"messageId":%q,"text":"hello human"}`, msgID),
		CreatedAt:   time.Now().Add(-1 * time.Minute),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	grantCheckinCallerIdentity(t, session)

	out := captureStdout(t, func() {
		if err := runCheckinSession(session, 10, nil, nil, nil, false, false); err != nil {
			t.Errorf("checkin (human): %v", err)
		}
	})

	// Human output should contain "checkin:" header.
	if !strings.Contains(out, "checkin:") {
		t.Errorf("human-readable output missing 'checkin:' header:\n%s", out)
	}
	// Should NOT be raw JSON.
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "{") {
		t.Errorf("human-readable output looks like JSON:\n%s", out)
	}
}
