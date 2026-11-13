// Package agentcredtest writes an agent-credentials.json and synthetic secret
// files for tests of the agent credential delivery (internal/agentcred).
//
// Every value here is synthetic. The names describe no real credential.
package agentcredtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

const (
	// Name is the name of the synthetic credential that Example returns.
	Name = "example"
	// Password is a synthetic secret. It is not a credential and matches no
	// credential shape.
	Password = "synthetic-agent-credential-password-0123456789abcdef"
	// Username is a synthetic user name.
	Username = "synthetic-agent-user"
	// Host is a synthetic host name (RFC 2606).
	Host = "host.example.invalid"

	// PasswordFileEnv, HostEnv, and UsernameEnv are the variables of the
	// credential that Example returns.
	PasswordFileEnv = "EXAMPLE_PASSWORD_FILE"
	HostEnv         = "EXAMPLE_HOST"
	UsernameEnv     = "EXAMPLE_USERNAME"
	// PasswordMarker is the marker name of the password file.
	PasswordMarker = "EXAMPLE_PASSWORD"
)

// Secret writes value and a newline to a file named name in a new temp dir,
// with mode 0400, and returns its path.
func Secret(t *testing.T, name, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(value+"\n"), 0o400); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// Example returns a credential with one file variable, one value variable,
// and one literal variable, the shape of a password login. passwordPath and
// hostPath name the secret files.
func Example(repos []string, passwordPath, hostPath string) config.AgentCredential {
	return config.AgentCredential{
		Repos:    repos,
		Files:    map[string]config.AgentCredentialFile{PasswordFileEnv: {Path: passwordPath, Marker: PasswordMarker}},
		Values:   map[string]config.AgentCredentialValue{HostEnv: {Path: hostPath}},
		Literals: map[string]string{UsernameEnv: Username},
	}
}

// ExampleSecrets writes Password and Host to two secret files, and returns
// their paths.
func ExampleSecrets(t *testing.T) (passwordPath, hostPath string) {
	t.Helper()
	return Secret(t, "example_password", Password), Secret(t, "example_host", Host)
}

// WriteFile writes creds as agent-credentials.json under configDir/prism.
func WriteFile(t *testing.T, configDir string, creds map[string]config.AgentCredential) {
	t.Helper()
	body, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	WriteRaw(t, configDir, body)
}

// WriteRaw writes body as agent-credentials.json under configDir/prism.
func WriteRaw(t *testing.T, configDir string, body []byte) {
	t.Helper()
	dir := filepath.Join(configDir, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.AgentCredentialsFileName), body, 0o600); err != nil {
		t.Fatalf("write %s: %v", config.AgentCredentialsFileName, err)
	}
}
