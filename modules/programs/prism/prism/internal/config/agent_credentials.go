// Package config — agent_credentials.go
//
// Loads the agent credentials from ~/.config/prism/agent-credentials.json.
//
// The prism NixOS module renders the file from
// nx.programs.prism.agentCredentials. Each key is the name of a credential:
//
//	{
//	  "example": {
//	    "repos": ["home-ops"],
//	    "files": {"EXAMPLE_PASSWORD_FILE": {"path": "/run/secrets/example_password", "marker": "EXAMPLE_PASSWORD"}},
//	    "values": {"EXAMPLE_HOST": {"path": "/run/secrets/example_host"}},
//	    "literals": {"EXAMPLE_USERNAME": "agent"}
//	  }
//	}
//
// Deployment through Nix is a security property. Neither
// internal/container/mounts.go (bwrap) nor the sandbox-exec profile gives a
// sandbox access to this file, so no sandbox can add its repo to a list.

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

// AgentCredentialsFileName is the basename of the rendered file under
// ~/.config/prism/.
const AgentCredentialsFileName = "agent-credentials.json"

// AgentCredential is one credential of agent-credentials.json. Each map is
// keyed by the name of the environment variable that the session gets.
type AgentCredential struct {
	// Repos are the repo directory names whose sessions get the credential.
	Repos []string `json:"repos"`
	// Files are the variables that hold the path of a secret file.
	Files map[string]AgentCredentialFile `json:"files"`
	// Values are the variables that hold the content of a secret file.
	Values map[string]AgentCredentialValue `json:"values"`
	// Literals are the variables that hold a fixed string.
	Literals map[string]string `json:"literals"`
}

// AgentCredentialFile is a file variable: the session gets the path, and the
// redactor masks the content as [redacted:<Marker>].
type AgentCredentialFile struct {
	Path   string `json:"path"`
	Marker string `json:"marker"`
}

// AgentCredentialValue is a value variable: the session gets the content of
// the file at Path.
type AgentCredentialValue struct {
	Path string `json:"path"`
}

// AgentCredentialsPath returns the path to agent-credentials.json. Returns ""
// when no home directory is discoverable.
func AgentCredentialsPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "prism", AgentCredentialsFileName)
}

// LoadAgentCredentials reads agent-credentials.json, keyed by credential
// name.
//
// A missing file returns nil and no error: the host gives no credential to
// any session. An unreadable or malformed file returns an error. Repo names
// are trimmed, and empty repo names are dropped. The other fields are
// returned as the file holds them: internal/agentcred checks each credential.
func LoadAgentCredentials() (map[string]AgentCredential, error) {
	path := AgentCredentialsPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent credentials: read %s: %w", path, err)
	}
	var raw map[string]AgentCredential
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("agent credentials: parse %s: %w", path, err)
	}
	for name, c := range raw {
		var repos []string
		for _, r := range c.Repos {
			if r = strings.TrimSpace(r); r != "" {
				repos = append(repos, r)
			}
		}
		c.Repos = repos
		raw[name] = c
	}
	return raw, nil
}
