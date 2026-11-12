package db_test

// redact_unifi_test.go — control 2 masks the UniFi API key in the rows of a
// tool call (issue #3472). The redactor is the one that ProcessRedactor
// builds, from the credential files of the host config.
//
// SECURITY: the key value is synthetic.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/unifi/unifitest"
)

func TestHostCredentialFiles(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if got := db.HostCredentialFiles(); got != nil {
		t.Errorf("with no unifi.json HostCredentialFiles = %v, want nil", got)
	}
	unifitest.WriteRaw(t, configDir, []byte(`{"repos":`))
	if got := db.HostCredentialFiles(); got != nil {
		t.Errorf("with a malformed unifi.json HostCredentialFiles = %v, want nil", got)
	}
	keyPath, hostPath := unifitest.Secrets(t, unifitest.Key, unifitest.Host)
	unifitest.WriteFile(t, configDir, nil, keyPath, hostPath)
	got := db.HostCredentialFiles()
	if len(got) != 1 || got[payload.UniFiAPIKeyName] != keyPath {
		t.Errorf("HostCredentialFiles = %v, want {%s: %s}", got, payload.UniFiAPIKeyName, keyPath)
	}
}

func TestWrite_UniFiKeyIsMaskedInEventsAndHarnessFrames(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	keyPath, hostPath := unifitest.Secrets(t, unifitest.Key, unifitest.Host)
	unifitest.WriteFile(t, configDir, []string{"home-ops"}, keyPath, hostPath)

	d := openTestDB(t)
	d.SetRedactor(payload.NewRedactorFromEnvironAndFiles(nil, db.HostCredentialFiles()))

	body, err := json.Marshal(payload.ToolResult{ID: "call_1", Success: true, Output: unifitest.Key + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	writeEventWithPayload(t, d, "tool_result", string(body))
	if err := d.WriteHarnessFrame(db.HarnessFrame{
		ID:          uuid.New().String(),
		SessionName: fakeSessionName,
		Direction:   db.HarnessFrameDirectionIn,
		Type:        "tool_result",
		Payload:     string(body),
	}); err != nil {
		t.Fatalf("WriteHarnessFrame: %v", err)
	}

	const marker = "[redacted:UNIFI_API_KEY]"
	stored := storedPayload(t, d, fakeSessionName)
	if strings.Contains(stored, unifitest.Key) || !strings.Contains(stored, marker) {
		t.Errorf("agent_events payload = %s, want the marker in place of the key", stored)
	}
	frames, err := d.QueryHarnessFrames(fakeSessionName, "", nil, "")
	if err != nil || len(frames) != 1 {
		t.Fatalf("QueryHarnessFrames = %d frames, %v", len(frames), err)
	}
	if strings.Contains(frames[0].Payload, unifitest.Key) || !strings.Contains(frames[0].Payload, marker) {
		t.Errorf("harness_frames payload = %s, want the marker in place of the key", frames[0].Payload)
	}
}
