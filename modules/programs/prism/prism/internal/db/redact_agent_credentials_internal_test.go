package db

// redact_agent_credentials_internal_test.go — the process redactor of a host
// process knows the content of each agent credential file from
// agent-credentials.json, with no path variable in its environment. The
// sidecar of a session is such a process. It knows every credential, whatever
// the repo of the session.
//
// SECURITY: every value is synthetic.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/agentcred/agentcredtest"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/payload"
)

func TestNewProcessRedactor_KnowsTheCredentialFilesFromTheHostConfig(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv(payload.CredentialFilesEnvName, "")
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	const second = "synthetic-second-credential-token-0123456789"
	tokenPath := agentcredtest.Secret(t, "second_token", second)

	if got := newProcessRedactor().Redact(agentcredtest.Password); got != agentcredtest.Password {
		t.Fatalf("with no agent-credentials.json the redactor masked the value: %q", got)
	}

	agentcredtest.WriteFile(t, configDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example([]string{"home-ops"}, passwordPath, hostPath),
		"second": {
			Repos: []string{"hass-config"},
			Files: map[string]config.AgentCredentialFile{"SECOND_TOKEN_FILE": {Path: tokenPath, Marker: "SECOND_TOKEN"}},
		},
	})
	got := newProcessRedactor().Redact("password=" + agentcredtest.Password + " token=" + second)
	if want := "password=[redacted:EXAMPLE_PASSWORD] token=[redacted:SECOND_TOKEN]"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}
