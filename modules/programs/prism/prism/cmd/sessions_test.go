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

// TestRunSessionsList_JSONOutput verifies that --json emits valid JSONL.
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

	// Every line must be independently parseable as a JSON object.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 JSONL lines, got %d\n%s", len(lines), out)
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line %d is not valid JSON: %v\nline: %s", i, err, line)
		}
		// Required fields must be present.
		for _, field := range []string{"instanceId", "sessionName", "repo", "harness", "startedAt"} {
			if _, ok := obj[field]; !ok {
				t.Errorf("line %d missing required JSON field %q\n%s", i, field, line)
			}
		}
	}
}

// TestRunSessionsList_JSONOutput_Empty verifies that --json with no sessions
// produces no output (empty JSONL is valid).
func TestRunSessionsList_JSONOutput_Empty(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderSessionsListJSON(nil); err != nil {
			t.Errorf("renderSessionsListJSON: %v", err)
		}
	})

	// Should produce no output (valid empty JSONL).
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty output for empty sessions list, got: %q", out)
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
