package db_test

// redact_agent_credentials_test.go — control 2 masks the content of each
// agent credential file and the UniFi OS session values (issue #3476) in the
// rows of a tool call. The redactor is the one that ProcessRedactor builds,
// from the credential files of the host config.
//
// SECURITY: every name and value in this file is synthetic.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agentcred/agentcredtest"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

const secondToken = "synthetic-second-credential-token-db-0123456789"

// writeTwoCredentials writes an agent-credentials.json with the example
// credential and a second credential of another repo, and returns the paths
// of the two credential files.
func writeTwoCredentials(t *testing.T, configDir string) (passwordPath, tokenPath string) {
	t.Helper()
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	tokenPath = agentcredtest.Secret(t, "second_token", secondToken)
	agentcredtest.WriteFile(t, configDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example([]string{"home-ops"}, passwordPath, hostPath),
		"second": {
			Repos: []string{"hass-config"},
			Files: map[string]config.AgentCredentialFile{"SECOND_TOKEN_FILE": {Path: tokenPath, Marker: "SECOND_TOKEN"}},
		},
	})
	return passwordPath, tokenPath
}

func TestHostCredentialFiles(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	if got := db.HostCredentialFiles(); got != nil {
		t.Errorf("with no agent-credentials.json HostCredentialFiles = %v, want nil", got)
	}
	agentcredtest.WriteRaw(t, configDir, []byte(`{"example":`))
	if got := db.HostCredentialFiles(); got != nil {
		t.Errorf("with a malformed agent-credentials.json HostCredentialFiles = %v, want nil", got)
	}
	passwordPath, tokenPath := writeTwoCredentials(t, configDir)
	want := []payload.CredentialFile{
		{Marker: agentcredtest.PasswordMarker, Path: passwordPath},
		{Marker: "SECOND_TOKEN", Path: tokenPath},
	}
	if want[0].Path > want[1].Path {
		want[0], want[1] = want[1], want[0]
	}
	got := db.HostCredentialFiles()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("HostCredentialFiles = %v, want %v", got, want)
	}
}

// Two credentials that name one file give one entry.
func TestHostCredentialFiles_OneEntryForASharedFile(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	agentcredtest.WriteFile(t, configDir, map[string]config.AgentCredential{
		"a": agentcredtest.Example([]string{"home-ops"}, passwordPath, hostPath),
		"b": agentcredtest.Example([]string{"hass-config"}, passwordPath, hostPath),
	})
	if got := db.HostCredentialFiles(); len(got) != 1 {
		t.Errorf("HostCredentialFiles = %v, want one entry", got)
	}
}

func TestWrite_AgentCredentialsAndSessionValuesAreMaskedInEventsAndHarnessFrames(t *testing.T) {
	const (
		sessionJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzeW50aGV0aWMiOnRydWV9.c3ludGhldGljLTM0NzY"
		csrfToken  = "00000000-0000-4000-8000-000000003476"
	)
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	writeTwoCredentials(t, configDir)

	d := openTestDB(t)
	d.SetRedactor(payload.NewRedactorFromEnvironAndFiles(nil, db.HostCredentialFiles()))

	body, err := json.Marshal(payload.ToolResult{ID: "call_1", Success: true, Output: agentcredtest.Password + "\n" +
		secondToken + "\n" +
		"Set-Cookie: TOKEN=" + sessionJWT + "; path=/\n" +
		"Cookie: TOKEN=" + sessionJWT + "\n" +
		"#HttpOnly_host.example.invalid\tFALSE\t/\tTRUE\t0\tTOKEN\t" + sessionJWT + "\n" +
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

	wantOutput := "[redacted:EXAMPLE_PASSWORD]\n" +
		"[redacted:SECOND_TOKEN]\n" +
		"Set-Cookie: TOKEN=[redacted:unifi-token-cookie]; path=/\n" +
		"Cookie: TOKEN=[redacted:unifi-token-cookie]\n" +
		"#HttpOnly_host.example.invalid\tFALSE\t/\tTRUE\t0\tTOKEN\t[redacted:unifi-token-cookie-jar]\n" +
		"X-CSRF-Token: [redacted:csrf-token-header]\n" +
		"x-updated-csrf-token: [redacted:csrf-token-header]\n"
	check := func(table, stored string) {
		t.Helper()
		for _, secret := range []string{agentcredtest.Password, secondToken, sessionJWT, csrfToken} {
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
