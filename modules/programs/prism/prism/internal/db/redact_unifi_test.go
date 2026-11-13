package db_test

// redact_unifi_test.go — control 2 masks the UniFi password (issue #3472) and
// the UniFi OS session values (issue #3476) in the rows of a tool call. The
// redactor is the one that ProcessRedactor builds, from the credential files
// of the host config.
//
// SECURITY: every value in this file is synthetic.

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
	passwordPath, hostPath := unifitest.Secrets(t, unifitest.Password, unifitest.Host)
	unifitest.WriteFile(t, configDir, nil, passwordPath, hostPath)
	got := db.HostCredentialFiles()
	if len(got) != 1 || got[payload.UniFiPasswordName] != passwordPath {
		t.Errorf("HostCredentialFiles = %v, want {%s: %s}", got, payload.UniFiPasswordName, passwordPath)
	}
}

func TestWrite_UniFiSecretsAreMaskedInEventsAndHarnessFrames(t *testing.T) {
	const (
		sessionJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzeW50aGV0aWMiOnRydWV9.c3ludGhldGljLTM0NzY"
		csrfToken  = "00000000-0000-4000-8000-000000003476"
	)
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	passwordPath, hostPath := unifitest.Secrets(t, unifitest.Password, unifitest.Host)
	unifitest.WriteFile(t, configDir, []string{"home-ops"}, passwordPath, hostPath)

	d := openTestDB(t)
	d.SetRedactor(payload.NewRedactorFromEnvironAndFiles(nil, db.HostCredentialFiles()))

	body, err := json.Marshal(payload.ToolResult{ID: "call_1", Success: true, Output: unifitest.Password + "\n" +
		"Set-Cookie: TOKEN=" + sessionJWT + "; path=/\n" +
		"Cookie: TOKEN=" + sessionJWT + "\n" +
		"#HttpOnly_10.0.0.1\tFALSE\t/\tTRUE\t0\tTOKEN\t" + sessionJWT + "\n" +
		"X-CSRF-Token: " + csrfToken + "\n" +
		"x-updated-csrf-token: " + csrfToken + "\n"})
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

	wantOutput := "[redacted:UNIFI_PASSWORD]\n" +
		"Set-Cookie: TOKEN=[redacted:unifi-token-cookie]; path=/\n" +
		"Cookie: TOKEN=[redacted:unifi-token-cookie]\n" +
		"#HttpOnly_10.0.0.1\tFALSE\t/\tTRUE\t0\tTOKEN\t[redacted:unifi-token-cookie-jar]\n" +
		"X-CSRF-Token: [redacted:csrf-token-header]\n" +
		"x-updated-csrf-token: [redacted:csrf-token-header]\n"
	check := func(table, stored string) {
		t.Helper()
		for _, secret := range []string{unifitest.Password, sessionJWT, csrfToken} {
			if strings.Contains(stored, secret) {
				t.Errorf("%s payload holds a secret value", table)
			}
		}
		var got payload.ToolResult
		if err := json.Unmarshal([]byte(stored), &got); err != nil {
			t.Fatalf("%s payload is not a tool result: %v", table, err)
		}
		if got.Output != wantOutput {
			t.Errorf("%s output = %q, want %q", table, got.Output, wantOutput)
		}
	}
	check("agent_events", storedPayload(t, d, fakeSessionName))
	frames, err := d.QueryHarnessFrames(fakeSessionName, "", nil, "")
	if err != nil || len(frames) != 1 {
		t.Fatalf("QueryHarnessFrames = %d frames, %v", len(frames), err)
	}
	check("harness_frames", frames[0].Payload)
}
