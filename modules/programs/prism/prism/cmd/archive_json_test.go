package cmd

// Tests for --json flag on prism archive --all.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestArchive_AllJSON_HappyPath verifies the JSON array shape with snake_case
// keys, RFC3339 timestamps, and null archive_path for unarchived rows.
func TestArchive_AllJSON_HappyPath(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second).UTC()

	iidArchived := uuid.New().String()
	iidUnarchived := uuid.New().String()
	insertTestSession(t, d, iidArchived, "testrepo@main", "testrepo", "/code", "pi",
		base.Add(-2*time.Hour), base.Add(-1*time.Hour), "finished", "/archive/testrepo/1")
	insertTestSession(t, d, iidUnarchived, "testrepo@main", "testrepo", "/code", "pi",
		base.Add(-1*time.Hour), base, "finished", "")

	out := captureStdout(t, func() {
		if err := printArchivePathAllJSON(d, "testrepo@main"); err != nil {
			t.Fatalf("printArchivePathAllJSON: %v", err)
		}
	})

	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("output is not a valid JSON array: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	// Required snake_case keys on every row.
	for i, row := range rows {
		for _, k := range []string{"instance_id", "session_name", "started_at", "ended_at", "archive_path"} {
			if _, ok := row[k]; !ok {
				t.Errorf("row %d missing required snake_case key %q\n%s", i, k, out)
			}
		}
	}

	// SessionsByName orders newest-first; row 0 is the unarchived
	// incarnation with archive_path: null.
	if rows[0]["archive_path"] != nil {
		t.Errorf("row 0 archive_path should be null (unarchived), got %v", rows[0]["archive_path"])
	}
	if rows[1]["archive_path"] != "/archive/testrepo/1" {
		t.Errorf("row 1 archive_path: want '/archive/testrepo/1', got %v", rows[1]["archive_path"])
	}

	// started_at is RFC3339 (ends in Z).
	if s, ok := rows[0]["started_at"].(string); !ok || !strings.HasSuffix(s, "Z") {
		t.Errorf("started_at must be RFC3339 in UTC, got %v", rows[0]["started_at"])
	}
}

// TestArchive_AllJSON_UnknownSession verifies that an unknown session name
// returns a non-nil error (caller maps this to a non-zero exit + stderr msg).
func TestArchive_AllJSON_UnknownSession(t *testing.T) {
	d := openIncarnationTestDB(t)
	err := printArchivePathAllJSON(d, "no-such-session@nope")
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

// TestArchive_JSONWithoutAll_Errors verifies that --json without --all is
// rejected (the AC scopes --json to the --all path).
func TestArchive_JSONWithoutAll_Errors(t *testing.T) {
	_ = openIncarnationTestDB(t)

	archiveCmd.Flags().Set("json", "true")        //nolint:errcheck
	defer archiveCmd.Flags().Set("json", "false") //nolint:errcheck
	// --all defaults to false.

	err := runArchive(archiveCmd, []string{"some-session"})
	if err == nil {
		t.Fatal("expected error for --json without --all, got nil")
	}
	if !strings.Contains(err.Error(), "--json is only supported together with --all") {
		t.Errorf("expected explanatory error, got: %v", err)
	}
}
