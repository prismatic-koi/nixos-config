package cmd

// Tests for the muted-aware surface of `prism sessions list` (#2013):
//
//   - The (muted) marker appears on muted rows in the human-readable table.
//   - The `muted` field appears in the --json output of every session object.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestListSessions_MutedRowHasMarker asserts the AC: every muted session
// shows a `(muted)` marker in the human-readable table; unmuted rows do not.
func TestListSessions_MutedRowHasMarker(t *testing.T) {
	d := openStatsTestDB(t)

	const mutedSession = "nixos-config@muted-row"
	const plainSession = "nixos-config@plain-row"

	for _, s := range []string{mutedSession, plainSession} {
		if err := d.UpsertStatus(s, "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", s, err)
		}
	}
	if _, err := d.SetMuted(mutedSession, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	listSessionsCmd.Flags().Set("all", "true")        //nolint:errcheck
	defer listSessionsCmd.Flags().Set("all", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := listSessionsCmd.RunE(listSessionsCmd, nil); err != nil {
			t.Errorf("list-sessions: %v", err)
		}
	})

	// Find the line for the muted session. Match by session name to avoid
	// false-negatives from terminal escape sequences wrapped around the name.
	var mutedLine, plainLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, mutedSession) {
			mutedLine = line
		}
		if strings.Contains(line, plainSession) {
			plainLine = line
		}
	}
	if mutedLine == "" {
		t.Fatalf("muted session %q not found in output:\n%s", mutedSession, out)
	}
	if plainLine == "" {
		t.Fatalf("plain session %q not found in output:\n%s", plainSession, out)
	}
	if !strings.Contains(mutedLine, "(muted)") {
		t.Errorf("muted row missing (muted) marker:\n%s", mutedLine)
	}
	if strings.Contains(plainLine, "(muted)") {
		t.Errorf("unmuted row has (muted) marker:\n%s", plainLine)
	}
}

// TestListSessions_JSON_IncludesMutedField asserts the AC: every session
// object in the --json output exposes a `muted` field. We don't assert on
// the exact casing of other fields (the broader Status struct uses
// PascalCase field names \u2014 see types.go) because the only field with an
// explicit json tag is `muted`, and that's the field this AC requires.
func TestListSessions_JSON_IncludesMutedField(t *testing.T) {
	d := openStatsTestDB(t)

	const mutedSession = "nixos-config@json-muted"
	const plainSession = "nixos-config@json-plain"

	for _, s := range []string{mutedSession, plainSession} {
		if err := d.UpsertStatus(s, "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", s, err)
		}
	}
	if _, err := d.SetMuted(mutedSession, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	listSessionsCmd.Flags().Set("all", "true")         //nolint:errcheck
	listSessionsCmd.Flags().Set("json", "true")        //nolint:errcheck
	defer func() {
		listSessionsCmd.Flags().Set("all", "false")  //nolint:errcheck
		listSessionsCmd.Flags().Set("json", "false") //nolint:errcheck
	}()

	out := captureStdout(t, func() {
		if err := listSessionsCmd.RunE(listSessionsCmd, nil); err != nil {
			t.Errorf("list-sessions --json: %v", err)
		}
	})

	var doc struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\nout: %s", err, out)
	}
	if len(doc.Sessions) == 0 {
		t.Fatalf("expected at least one session in JSON output:\n%s", out)
	}

	var (
		foundMuted bool
		foundPlain bool
	)
	for _, s := range doc.Sessions {
		nameRaw, ok := s["SessionName"]
		if !ok {
			t.Fatalf("session object missing SessionName key: %v", s)
		}
		var name string
		if err := json.Unmarshal(nameRaw, &name); err != nil {
			t.Fatalf("unmarshal SessionName: %v", err)
		}
		mutedRaw, ok := s["muted"]
		if !ok {
			t.Errorf("session %q missing `muted` field; keys=%v", name, mapKeys(s))
			continue
		}
		var muted bool
		if err := json.Unmarshal(mutedRaw, &muted); err != nil {
			t.Errorf("session %q has non-bool muted: %v", name, err)
			continue
		}
		switch name {
		case mutedSession:
			foundMuted = true
			if !muted {
				t.Errorf("session %q: muted=false in JSON, want true", name)
			}
		case plainSession:
			foundPlain = true
			if muted {
				t.Errorf("session %q: muted=true in JSON, want false", name)
			}
		}
	}
	if !foundMuted {
		t.Errorf("muted session %q not found in JSON output", mutedSession)
	}
	if !foundPlain {
		t.Errorf("plain session %q not found in JSON output", plainSession)
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
