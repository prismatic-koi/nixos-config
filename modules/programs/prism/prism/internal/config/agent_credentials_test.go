package config_test

// agent_credentials_test.go — LoadAgentCredentials is the only source of the
// repo lists, the variables, and the secret paths of the agent credentials.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func withAgentCredentialsFile(t *testing.T, body []byte) string {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if err := os.WriteFile(filepath.Join(prismDir, config.AgentCredentialsFileName), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configHome
}

func TestAgentCredentialsPath_UsesXDGConfigHome(t *testing.T) {
	configHome := withAgentCredentialsFile(t, nil)
	if got, want := config.AgentCredentialsPath(), filepath.Join(configHome, "prism", "agent-credentials.json"); got != want {
		t.Errorf("AgentCredentialsPath() = %q, want %q", got, want)
	}
}

func TestLoadAgentCredentials(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		want    map[string]config.AgentCredential
		wantErr bool
	}{
		{"absent file gives nothing", nil, nil, false},
		{"malformed JSON is an error", []byte(`{"a":`), nil, true},
		{"repos that are not a list is an error", []byte(`{"a":{"repos":"home-ops"}}`), nil, true},
		{"a literal that is not a string is an error", []byte(`{"a":{"literals":{"A_USER":1}}}`), nil, true},
		{"empty object gives no credential", []byte(`{}`), map[string]config.AgentCredential{}, false},
		{
			"rendered shape",
			[]byte(`{"example":{"repos":["home-ops","obsidian"],` +
				`"files":{"EXAMPLE_PASSWORD_FILE":{"path":"/run/secrets/example_password","marker":"EXAMPLE_PASSWORD"}},` +
				`"values":{"EXAMPLE_HOST":{"path":"/run/secrets/example_host"}},` +
				`"literals":{"EXAMPLE_USERNAME":"agent"}}}`),
			map[string]config.AgentCredential{"example": {
				Repos:    []string{"home-ops", "obsidian"},
				Files:    map[string]config.AgentCredentialFile{"EXAMPLE_PASSWORD_FILE": {Path: "/run/secrets/example_password", Marker: "EXAMPLE_PASSWORD"}},
				Values:   map[string]config.AgentCredentialValue{"EXAMPLE_HOST": {Path: "/run/secrets/example_host"}},
				Literals: map[string]string{"EXAMPLE_USERNAME": "agent"},
			}},
			false,
		},
		{
			"blank repos dropped and repo names trimmed",
			[]byte(`{"a":{"repos":[" home-ops ",""," "]},"b":{"repos":[]}}`),
			map[string]config.AgentCredential{"a": {Repos: []string{"home-ops"}}, "b": {}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAgentCredentialsFile(t, tc.body)
			got, err := config.LoadAgentCredentials()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("LoadAgentCredentials() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
