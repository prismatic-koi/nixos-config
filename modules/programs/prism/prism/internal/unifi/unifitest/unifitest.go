// Package unifitest writes a unifi.json and synthetic secret files for tests
// of the UniFi credential delivery (internal/unifi).
package unifitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// Password is a synthetic UniFi password. It is not a credential and matches
// no credential shape.
const Password = "synthetic-unifi-password-3476-0123456789abcdef"

// Username is a synthetic UniFi OS username.
const Username = "synthetic-agent-user"

// Host is a synthetic UniFi host.
const Host = "10.0.0.1"

// Secrets writes password and host to two files in a temp dir, with mode
// 0400, and returns their paths.
func Secrets(t *testing.T, password, host string) (passwordPath, hostPath string) {
	t.Helper()
	dir := t.TempDir()
	passwordPath = filepath.Join(dir, "unifi_password")
	hostPath = filepath.Join(dir, "unifi_host")
	for p, v := range map[string]string{passwordPath: password + "\n", hostPath: host + "\n"} {
		if err := os.WriteFile(p, []byte(v), 0o400); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return passwordPath, hostPath
}

// WriteFile writes unifi.json under configDir/prism, with Username as the
// username.
func WriteFile(t *testing.T, configDir string, repos []string, passwordPath, hostPath string) {
	t.Helper()
	body, err := json.Marshal(config.UniFi{Repos: repos, Username: Username, PasswordPath: passwordPath, HostPath: hostPath})
	if err != nil {
		t.Fatal(err)
	}
	WriteRaw(t, configDir, body)
}

// WriteRaw writes body as unifi.json under configDir/prism.
func WriteRaw(t *testing.T, configDir string, body []byte) {
	t.Helper()
	dir := filepath.Join(configDir, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.UniFiFileName), body, 0o600); err != nil {
		t.Fatalf("write %s: %v", config.UniFiFileName, err)
	}
}
