package config_test

// unifi_test.go — LoadUniFi is the only source of the repo list, the
// username, and the secret paths of the UniFi credential delivery (issues
// #3472 and #3476).

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func withUniFiFile(t *testing.T, body []byte) string {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if err := os.WriteFile(filepath.Join(prismDir, config.UniFiFileName), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configHome
}

func TestUniFiPath_UsesXDGConfigHome(t *testing.T) {
	configHome := withUniFiFile(t, nil)
	if got, want := config.UniFiPath(), filepath.Join(configHome, "prism", "unifi.json"); got != want {
		t.Errorf("UniFiPath() = %q, want %q", got, want)
	}
}

func TestLoadUniFi(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		want    config.UniFi
		wantErr bool
	}{
		{"absent file gives nothing", nil, config.UniFi{}, false},
		{"malformed JSON is an error", []byte(`{"repos":`), config.UniFi{}, true},
		{"repos that are not a list is an error", []byte(`{"repos":"home-ops"}`), config.UniFi{}, true},
		{"relative password path is an error", []byte(`{"repos":["a"],"password_path":"k","host_path":"/h"}`), config.UniFi{}, true},
		{"relative host path is an error", []byte(`{"repos":["a"],"password_path":"/k","host_path":"h"}`), config.UniFi{}, true},
		{
			"rendered shape",
			[]byte(`{"repos":["home-ops","obsidian"],"username":"agent-readonly","password_path":"/run/secrets/unifi_password","host_path":"/run/secrets/unifi_host"}`),
			config.UniFi{Repos: []string{"home-ops", "obsidian"}, Username: "agent-readonly", PasswordPath: "/run/secrets/unifi_password", HostPath: "/run/secrets/unifi_host"},
			false,
		},
		{
			"blank repos dropped and values trimmed",
			[]byte(`{"repos":[" home-ops ",""," "],"username":" u ","password_path":" /k ","host_path":"/h"}`),
			config.UniFi{Repos: []string{"home-ops"}, Username: "u", PasswordPath: "/k", HostPath: "/h"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withUniFiFile(t, tc.body)
			got, err := config.LoadUniFi()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("LoadUniFi() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
