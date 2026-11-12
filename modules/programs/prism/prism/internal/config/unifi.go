// Package config — unifi.go
//
// Loads the UniFi API key delivery from ~/.config/prism/unifi.json (issue
// #3472).
//
// The prism NixOS module renders the file from nx.programs.prism.unifi.repos
// and the paths of the two sops secrets:
//
//	{
//	  "repos": ["home-ops", "nixos-config", "hass-config", "obsidian"],
//	  "api_key_path": "/run/secrets/unifi_api_key",
//	  "host_path": "/run/secrets/unifi_host"
//	}
//
// Deployment through Nix is a security property. Neither
// internal/container/mounts.go (bwrap) nor the sandbox-exec profile gives a
// sandbox access to this file, so no sandbox can add its repo to the list.

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// UniFiFileName is the basename of the rendered file under ~/.config/prism/.
const UniFiFileName = "unifi.json"

// UniFi is the content of unifi.json.
type UniFi struct {
	// Repos are the repo directory names whose sessions get the key.
	Repos []string `json:"repos"`
	// APIKeyPath is the host path of the decrypted unifi_api_key secret.
	APIKeyPath string `json:"api_key_path"`
	// HostPath is the host path of the decrypted unifi_host secret.
	HostPath string `json:"host_path"`
}

// UniFiPath returns the path to unifi.json. Returns "" when no home directory
// is discoverable.
func UniFiPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "prism", UniFiFileName)
}

// LoadUniFi reads unifi.json.
//
// A missing file returns the zero value and no error: the host gives the key
// to no session. An unreadable or malformed file, or a path that is not
// absolute, returns an error. Repo names are trimmed, and empty names are
// dropped.
func LoadUniFi() (UniFi, error) {
	path := UniFiPath()
	if path == "" {
		return UniFi{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return UniFi{}, nil
	}
	if err != nil {
		return UniFi{}, fmt.Errorf("unifi: read %s: %w", path, err)
	}
	var raw UniFi
	if err := json.Unmarshal(data, &raw); err != nil {
		return UniFi{}, fmt.Errorf("unifi: parse %s: %w", path, err)
	}
	out := UniFi{APIKeyPath: strings.TrimSpace(raw.APIKeyPath), HostPath: strings.TrimSpace(raw.HostPath)}
	for _, p := range []string{out.APIKeyPath, out.HostPath} {
		if p != "" && !filepath.IsAbs(p) {
			return UniFi{}, fmt.Errorf("unifi: %s: path %q is not absolute", path, p)
		}
	}
	for _, r := range raw.Repos {
		if r = strings.TrimSpace(r); r != "" {
			out.Repos = append(out.Repos, r)
		}
	}
	return out, nil
}
