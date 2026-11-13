// Package unifi decides whether a session gets the UniFi credential (issues
// #3472 and #3476).
//
// The credential is the password of a local UniFi OS user with read access
// only. A session of a repo in unifi.json (nx.programs.prism.unifi.repos)
// gets three environment variables:
//
//   - UNIFI_PASSWORD_FILE: the path of the decrypted unifi_password secret.
//     No variable holds the password itself. bwrap binds the file read-only,
//     and the sandbox-exec profile admits a read of the file and no write.
//   - UNIFI_USERNAME: the name of the user (nx.programs.prism.unifi.username).
//   - UNIFI_HOST: the value of the unifi_host secret.
//
// The repo comes from projecttrust.Policy.SessionProject: the session name,
// the start directory, and the project roots in config.json. The repo list,
// the username, and the secret paths come from unifi.json. No sandbox can
// read or write config.json or unifi.json.
//
// A review agent does not get the credential: it reviews a diff on a
// read-only worktree, and it has no use for the UniFi API. An investigator
// gets the credential, as a coordinator and a worker do: it answers questions
// about the live system, and the user has read access only.
//
// The password file must hold a value that the redactor can mask
// (payload.UsableSecretValue). If not, or if a secret file is missing, the
// session gets no variable, and ForSession gives a warning. The session
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
	// PasswordFileEnvName holds the path of the password file.
	PasswordFileEnvName = payload.UniFiPasswordFileEnvName
	// UsernameEnvName holds the name of the UniFi OS user.
	UsernameEnvName = "UNIFI_USERNAME"
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
	// PasswordPath is the host path of the password file. It is also the
	// path in a bwrap or sandbox-exec sandbox.
	PasswordPath string
	// Username is the name of the UniFi OS user.
	Username string
	// Host is the value of the unifi_host secret.
	Host string
}

// Enabled reports whether the session gets the credential.
func (d Delivery) Enabled() bool { return d.PasswordPath != "" }

// EnvVars returns the variables of the delivery, or nil when it gives
// nothing.
func (d Delivery) EnvVars() map[string]string {
	if !d.Enabled() {
		return nil
	}
	return map[string]string{
		PasswordFileEnvName: d.PasswordPath,
		UsernameEnvName:     d.Username,
		HostEnvName:         d.Host,
	}
}

// noVariables ends each warning. It names the variables that the session
// does not get.
var noVariables = fmt.Sprintf("the session gets no %s, no %s, and no %s", PasswordFileEnvName, UsernameEnvName, HostEnvName)

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
		warn(fmt.Sprintf("%v — %s", err, noVariables))
		return Delivery{}
	}
	if !slices.Contains(u.Repos, filepath.Base(root)) {
		return Delivery{}
	}
	d, err := resolve(u)
	if err != nil {
		warn(fmt.Sprintf("unifi: %v — %s", err, noVariables))
		return Delivery{}
	}
	return d
}

// resolve checks the username and the two secret files, and reads the host.
// The errors name a path and a reason, never a secret value.
func resolve(u config.UniFi) (Delivery, error) {
	if u.PasswordPath == "" || u.HostPath == "" {
		return Delivery{}, fmt.Errorf("%s names no path for the password or the host", config.UniFiFileName)
	}
	if u.Username == "" || strings.ContainsFunc(u.Username, isSpaceOrControl) {
		return Delivery{}, fmt.Errorf("%s holds no username, or a username with a space or a control character", config.UniFiFileName)
	}
	password, err := payload.ReadSecretFile(u.PasswordPath)
	if err != nil {
		return Delivery{}, fmt.Errorf("password: %w", err)
	}
	if !payload.UsableSecretValue(password) {
		return Delivery{}, fmt.Errorf("the redactor cannot mask the value in the password file %s: it is empty, holds fewer than %d characters, or holds a $( literal", u.PasswordPath, payload.MinCredentialValueLen)
	}
	host, err := payload.ReadSecretFile(u.HostPath)
	if err != nil {
		return Delivery{}, fmt.Errorf("host: %w", err)
	}
	if host == "" || strings.ContainsFunc(host, isSpaceOrControl) {
		return Delivery{}, fmt.Errorf("host file %s is empty or holds a space or a control character", u.HostPath)
	}
	return Delivery{PasswordPath: u.PasswordPath, Username: u.Username, Host: host}, nil
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }
