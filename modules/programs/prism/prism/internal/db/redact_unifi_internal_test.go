package db

// redact_unifi_internal_test.go — the process redactor of a host process
// knows the UniFi password from unifi.json, with no path variable in its
// environment (issue #3472). The sidecar of a session is such a process.
//
// SECURITY: the password value is synthetic.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/unifi/unifitest"
)

func TestNewProcessRedactor_KnowsTheUniFiPasswordFromTheHostConfig(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv(payload.UniFiPasswordFileEnvName, "")
	passwordPath, hostPath := unifitest.Secrets(t, unifitest.Password, unifitest.Host)

	if got := newProcessRedactor().Redact(unifitest.Password); got != unifitest.Password {
		t.Fatalf("with no unifi.json the redactor masked the password: %q", got)
	}

	unifitest.WriteFile(t, configDir, []string{"home-ops"}, passwordPath, hostPath)
	got := newProcessRedactor().Redact("password=" + unifitest.Password)
	if strings.Contains(got, unifitest.Password) || got != "password=[redacted:UNIFI_PASSWORD]" {
		t.Errorf("Redact = %q, want the UNIFI_PASSWORD marker", got)
	}
}
