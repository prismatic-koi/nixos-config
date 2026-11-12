// Package unifi decides whether a session gets the UniFi API key (issue
// #3472).
//
// A session of a repo in unifi.json (nx.programs.prism.unifi.repos) gets two
// environment variables:
//
//   - UNIFI_API_KEY_FILE: the path of the decrypted unifi_api_key secret. No
//     variable holds the key itself. bwrap binds the file read-only, and the
//     sandbox-exec profile admits a read of the file and no write.
//   - UNIFI_HOST: the value of the unifi_host secret.
//
// The repo comes from projecttrust.Policy.SessionProject: the session name,
// the start directory, and the project roots in config.json. The repo list
// and the secret paths come from unifi.json. No sandbox can read or write
// config.json or unifi.json.
//
// A review agent does not get the key: it reviews a diff on a read-only
// worktree, and it has no use for the UniFi API. An investigator gets the
// key, as a coordinator and a worker do: it answers questions about the live
// system, and the key gives read access only.
//
// The key file must hold a value that the redactor can mask
// (payload.UsableSecretValue). If not, or if a secret file is missing, the
// session gets neither variable, and ForSession gives a warning. The session
// starts in every case.
//
// Host mode has no filesystem isolation. There the variables are the only
// control: a host-mode session of any repo can read the sops path.
package unifi

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/projecttrust"
)

const (
	// APIKeyFileEnvName holds the path of the key file.
	APIKeyFileEnvName = payload.UniFiAPIKeyFileEnvName
	// HostEnvName holds the UniFi host.
	HostEnvName = "UNIFI_HOST"
)

// reviewRoles are the five roles of the canonical review set (internal/review
// Agents()). They are listed here because internal/review imports a package
// that imports this one. A fallback reviewer runs with the role of the agent
// that it replaces.
var reviewRoles = map[string]bool{
	"review-goal":     true,
	"review-code":     true,
	"review-context":  true,
	"review-qa":       true,
	"review-security": true,
}

// Delivery is what a session gets. The zero value gives nothing.
type Delivery struct {
	// APIKeyPath is the host path of the key file. It is also the path in
	// a bwrap or sandbox-exec sandbox.
	APIKeyPath string
	// Host is the value of the unifi_host secret.
	Host string
}

// Enabled reports whether the session gets the key.
func (d Delivery) Enabled() bool { return d.APIKeyPath != "" }

// EnvVars returns the variables of the delivery, or nil when it gives
// nothing.
func (d Delivery) EnvVars() map[string]string {
	if !d.Enabled() {
		return nil
	}
	return map[string]string{APIKeyFileEnvName: d.APIKeyPath, HostEnvName: d.Host}
}

// ForSession returns the delivery for the session sessionName with role that
// starts in dir. cfg gives the project roots. It calls warn once for each
// problem, and never with the value of a secret. It reads unifi.json each
// time, so a change takes effect at the next start of a session.
func ForSession(sessionName, role, dir string, cfg config.Config, warn func(string)) Delivery {
	if reviewRoles[role] {
		return Delivery{}
	}
	policy := projecttrust.Policy{ProjectLocations: cfg.ProjectLocations, ProjectSpecific: cfg.ProjectSpecific}
	root, ok := policy.SessionProject(sessionName, dir)
	if !ok {
		return Delivery{}
	}
	u, err := config.LoadUniFi()
	if err != nil {
		warn(fmt.Sprintf("%v — the session gets no %s and no %s", err, APIKeyFileEnvName, HostEnvName))
		return Delivery{}
	}
	if !slices.Contains(u.Repos, filepath.Base(root)) {
		return Delivery{}
	}
	d, err := resolve(u)
	if err != nil {
		warn(fmt.Sprintf("unifi: %v — the session gets no %s and no %s", err, APIKeyFileEnvName, HostEnvName))
		return Delivery{}
	}
	return d
}

// resolve checks the two secret files and reads the host. The errors name a
// path and a reason, never a value.
func resolve(u config.UniFi) (Delivery, error) {
	if u.APIKeyPath == "" || u.HostPath == "" {
		return Delivery{}, fmt.Errorf("%s names no path for the API key or the host", config.UniFiFileName)
	}
	key, err := payload.ReadSecretFile(u.APIKeyPath)
	if err != nil {
		return Delivery{}, fmt.Errorf("API key: %w", err)
	}
	if !payload.UsableSecretValue(key) {
		return Delivery{}, fmt.Errorf("the redactor cannot mask the value in the API key file %s: it is empty, holds fewer than %d characters, or holds a $( literal", u.APIKeyPath, payload.MinCredentialValueLen)
	}
	host, err := payload.ReadSecretFile(u.HostPath)
	if err != nil {
		return Delivery{}, fmt.Errorf("host: %w", err)
	}
	if host == "" || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return Delivery{}, fmt.Errorf("host file %s is empty or holds a space or a control character", u.HostPath)
	}
	return Delivery{APIKeyPath: u.APIKeyPath, Host: host}, nil
}
