package db

// redact_unifi_internal_test.go — the process redactor of a host process
// knows the UniFi API key from unifi.json, with no path variable in its
// environment (issue #3472). The sidecar of a session is such a process.
//
// SECURITY: the key value is synthetic.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/unifi/unifitest"
)

func TestNewProcessRedactor_KnowsTheUniFiKeyFromTheHostConfig(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv(payload.UniFiAPIKeyFileEnvName, "")
	keyPath, hostPath := unifitest.Secrets(t, unifitest.Key, unifitest.Host)

	if got := newProcessRedactor().Redact(unifitest.Key); got != unifitest.Key {
		t.Fatalf("with no unifi.json the redactor masked the key: %q", got)
	}

	unifitest.WriteFile(t, configDir, []string{"home-ops"}, keyPath, hostPath)
	got := newProcessRedactor().Redact("X-API-KEY: " + unifitest.Key)
	if strings.Contains(got, unifitest.Key) || got != "X-API-KEY: [redacted:UNIFI_API_KEY]" {
		t.Errorf("Redact = %q, want the UNIFI_API_KEY marker", got)
	}
}
