package cmd

// Tests for prism sessions list (issue #999 ACs):
//   - prism sessions list → tabular listing of all rows
//   - prism sessions list --repo <name> → filter by repo
//   - prism sessions list --since <date> → filter by started_at
//   - prism sessions list --json → JSONL output (every line independently parseable)

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRunSessionsList_Empty verifies graceful output for an empty sessions table.
func TestRunSessionsList_Empty(t *testing.T) {
	_ = openIncarnationTestDB(t)

	out := captureStdout(t, func() {
		if err := renderSessionsListTable(nil); err != nil {
			t.Errorf("renderSessionsListTable: %v", err)
		}
	})

	if !strings.Contains(out, "no sessions yet") {
		t.Errorf("expected 'no sessions yet' for empty list\ngot:\n%s", out)
	}
}

// TestRunSessionsList_TabularOutput verifies that the tabular listing contains
// the expected column headers and rows.
func TestRunSessionsList_TabularOutput(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	sessions, err := d.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}

	out := captureStdout(t, func() {
		if err := renderSessionsListTable(sessions); err != nil {
			t.Errorf("renderSessionsListTable: %v", err)
		}
	})

	// Check headers.
	for _, hdr := range []string{"INSTANCE", "SESSION_NAME", "REPO", "AGENT", "STATE", "STARTED", "DURATION"} {
		if !strings.Contains(out, hdr) {
			t.Errorf("output missing column header %q\ngot:\n%s", hdr, out)
		}
	}
	// Check data.
	if !strings.Contains(out, iid[:8]) {
		t.Errorf("output missing short instance_id %q\ngot:\n%s", iid[:8], out)
	}
	if !strings.Contains(out, "testrepo@main") {
		t.Errorf("output missing session name 'testrepo@main'\ngot:\n%s", out)
	}
}

// TestRunSessionsList_RepoFilter verifies --repo filtering.
func TestRunSessionsList_RepoFilter(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid1 := uuid.New().String()
	iid2 := uuid.New().String()
	insertTestSession(t, d, iid1, "repo-a@main", "repo-a", "/code/repo-a", "opencode",
		base.Add(-2*time.Hour), base, "finished", "")
	insertTestSession(t, d, iid2, "repo-b@main", "repo-b", "/code/repo-b", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	sessions, err := d.SessionsForRepo("repo-a")
	if err != nil {
		t.Fatalf("SessionsForRepo: %v", err)
	}

	out := captureStdout(t, func() {
		if err := renderSessionsListTable(sessions); err != nil {
			t.Errorf("renderSessionsListTable: %v", err)
		}
	})

	if !strings.Contains(out, "repo-a@main") {
		t.Errorf("expected 'repo-a@main' in filtered output\ngot:\n%s", out)
	}
	if strings.Contains(out, "repo-b@main") {
		t.Errorf("'repo-b@main' should be filtered out\ngot:\n%s", out)
	}
}

// TestRunSessionsList_SinceFilter verifies --since filtering.
func TestRunSessionsList_SinceFilter(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iidOld := uuid.New().String()
	iidNew := uuid.New().String()
	insertTestSession(t, d, iidOld, "repo@old", "testrepo", "/code", "opencode",
		base.Add(-30*24*time.Hour), base.Add(-29*24*time.Hour), "finished", "")
	insertTestSession(t, d, iidNew, "repo@new", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	sinceMs := base.Add(-7 * 24 * time.Hour).UnixMilli()
	sessions, err := d.SessionsSince(sinceMs)
	if err != nil {
		t.Fatalf("SessionsSince: %v", err)
	}

	out := captureStdout(t, func() {
		if err := renderSessionsListTable(sessions); err != nil {
			t.Errorf("renderSessionsListTable: %v", err)
		}
	})

	if strings.Contains(out, "repo@old") {
		t.Errorf("old session should be filtered out\ngot:\n%s", out)
	}
	if !strings.Contains(out, "repo@new") {
		t.Errorf("expected 'repo@new' in output\ngot:\n%s", out)
	}
}

// TestRunSessionsList_JSONOutput verifies that --json emits a JSON object
// with a sessions array and truncated bool (issue #1502).
func TestRunSessionsList_JSONOutput(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid1 := uuid.New().String()
	iid2 := uuid.New().String()
	insertTestSession(t, d, iid1, "repo@main", "testrepo", "/code", "opencode",
		base.Add(-2*time.Hour), base.Add(-1*time.Hour), "finished", "/archive/1")
	insertTestSession(t, d, iid2, "repo@feature", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	sessions, err := d.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}

	out := captureStdout(t, func() {
		if err := renderSessionsListJSON(sessions); err != nil {
			t.Errorf("renderSessionsListJSON: %v", err)
		}
	})

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := obj["truncated"]; !ok {
		t.Errorf("expected 'truncated' key in output")
	}
	rowsVal, ok := obj["sessions"]
	if !ok {
		t.Fatalf("expected 'sessions' key in output")
	}
	rowsArr, ok := rowsVal.([]interface{})
	if !ok {
		t.Fatalf("sessions is not an array")
	}
	if len(rowsArr) != 2 {
		t.Errorf("expected 2 rows in JSON array, got %d\n%s", len(rowsArr), out)
	}
	// Required snake_case fields must be present on every row (null is OK
	// for optional fields, but the keys themselves must be there).
	for i, r := range rowsArr {
		row, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("row %d is not an object", i)
		}
		for _, field := range []string{"instance_id", "session_name", "repo", "harness", "started_at", "ended_at", "end_state", "archive_path", "agent_role", "root_agent_name", "harness_session_id", "group_id", "prism_version"} {
			if _, ok := row[field]; !ok {
				t.Errorf("row %d missing required snake_case JSON field %q\n%s", i, field, out)
			}
		}
		// Reject any leftover camelCase keys.
		for _, badField := range []string{"instanceId", "sessionName", "startedAt", "endedAt", "endState", "archivePath", "agentRole", "rootAgentName", "harnessSessionId", "groupId", "prismVersion"} {
			if _, ok := row[badField]; ok {
				t.Errorf("row %d has unexpected camelCase JSON field %q (must be snake_case)\n%s", i, badField, out)
			}
		}
	}
}

// TestRunSessionsList_JSONOutput_Empty verifies that --json with no sessions
// emits a JSON object with an empty sessions array and truncated:false.
func TestRunSessionsList_JSONOutput_Empty(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderSessionsListJSON(nil); err != nil {
			t.Errorf("renderSessionsListJSON: %v", err)
		}
	})

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}
	rowsVal, ok := obj["sessions"]
	if !ok {
		t.Fatalf("expected 'sessions' key in output")
	}
	arr, ok := rowsVal.([]interface{})
	if !ok || len(arr) != 0 {
		t.Errorf("expected empty sessions array, got %v", rowsVal)
	}
	if truncated, _ := obj["truncated"].(bool); truncated {
		t.Errorf("expected truncated:false for empty list")
	}
}

// TestRunSessionsList_JSONOutput_ArchivePath verifies that archive_path is
// included in JSON output when non-nil.
func TestRunSessionsList_JSONOutput_ArchivePath(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "/some/archive/path")

	sessions, err := d.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}

	out := captureStdout(t, func() {
		if err := renderSessionsListJSON(sessions); err != nil {
			t.Errorf("renderSessionsListJSON: %v", err)
		}
	})

	if !strings.Contains(out, "/some/archive/path") {
		t.Errorf("expected archive path in JSON output\ngot:\n%s", out)
	}
}
